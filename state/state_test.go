package state

import (
	"os"
	"testing"
	"time"
)

func TestStateRecordErrorCreatesEntry(t *testing.T) {
	// RecordError on a source with no prior state should create
	// a minimal SourceState with the operational fields set.
	s := &State{
		Version: CurrentVersion,
		Sources: map[string]*SourceState{},
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	s.RecordError("newsrc", now, next, errSentinel)

	got, ok := s.Sources["newsrc"]
	if !ok {
		t.Fatalf("RecordError did not create entry for %q", "newsrc")
	}
	if got.Record == nil {
		t.Fatalf("Record is nil; should be set")
	}
	if !got.Record.LastRun.Equal(now) {
		t.Errorf("LastRun = %v, want %v", got.Record.LastRun, now)
	}
	if !got.Record.NextRun.Equal(next) {
		t.Errorf("NextRun = %v, want %v", got.Record.NextRun, next)
	}
	if got.Record.LastError != errSentinel.Error() {
		t.Errorf("LastError = %q, want %q", got.Record.LastError, errSentinel.Error())
	}
	// ItemsSeen is initialized to an empty map (so callers
	// don't have to nil-check) but the diff baseline is not
	// established.
	if got.Baseline == nil || got.Baseline.ItemsSeen == nil {
		t.Errorf("Baseline.ItemsSeen is nil; should be empty map")
	}
}

func TestStateRecordErrorPreservesBaseline(t *testing.T) {
	// RecordError on a source that already has a baseline must
	// NOT clobber ItemsSeen / ItemsHash / LastAdded /
	// LastRemoved. A failed fetch shouldn't reset the diff
	// baseline.
	items := map[string]bool{"x": true, "y": true}
	s := &State{
		Version: CurrentVersion,
		Sources: map[string]*SourceState{
			"a": {
				Baseline: &Baseline{ItemsSeen: items, ItemsHash: "sha256:abc", ItemsCount: 2},
				Record:   &Record{LastAdded: 5, LastRemoved: 3},
			},
		},
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	s.RecordError("a", now, next, errSentinel)

	got := s.Sources["a"]
	if got.Baseline == nil || len(got.Baseline.ItemsSeen) != 2 {
		t.Errorf("ItemsSeen was clobbered: %v", got.Baseline)
	}
	if got.Baseline.ItemsHash != "sha256:abc" {
		t.Errorf("ItemsHash was clobbered: %q", got.Baseline.ItemsHash)
	}
	if got.Baseline.ItemsCount != 2 {
		t.Errorf("ItemsCount was clobbered: %d", got.Baseline.ItemsCount)
	}
	if got.Record.LastAdded != 5 {
		t.Errorf("LastAdded was clobbered: %d", got.Record.LastAdded)
	}
	if got.Record.LastRemoved != 3 {
		t.Errorf("LastRemoved was clobbered: %d", got.Record.LastRemoved)
	}
	if got.Record.LastError != errSentinel.Error() {
		t.Errorf("LastError = %q, want %q", got.Record.LastError, errSentinel.Error())
	}
}

func TestStateRememberHashPrefix(t *testing.T) {
	// ItemsHash must be self-identifying with the "sha256:"
	// prefix. Otherwise a bare 64-char hex string is ambiguous.
	s := &State{
		Version: CurrentVersion,
		Sources: map[string]*SourceState{},
	}
	s.Remember("a", []Item{{ID: "x"}}, time.Now().UTC(), time.Now().Add(time.Hour), 0, 0, "")
	if got := s.Sources["a"].Baseline.ItemsHash; len(got) < 7 || got[:7] != "sha256:" {
		t.Errorf("ItemsHash = %q, want sha256: prefix", got)
	}
}

func TestStateLoadV1MigratesToV2(t *testing.T) {
	// Earlier versions stored per-source state as
	// map[string]map[string]bool (just the items_seen set).
	// The current code stores it as map[string]*SourceState
	// (items_seen plus per-source runtime fields). The on-disk
	// format is versioned and migrated transparently on load.
	v1 := `{
  "version": 1,
  "last_run": "2026-01-01T00:00:00Z",
  "sources": {
    "a": {"x": true, "y": true},
    "b": {"only": true}
  }
}`
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", st.Version, CurrentVersion)
	}
	if st.LastRun.IsZero() {
		t.Errorf("LastRun not preserved across migration")
	}
	if got := st.Sources["a"]; got == nil {
		t.Fatalf("source a missing after migration")
	} else if !got.Baseline.ItemsSeen["x"] || !got.Baseline.ItemsSeen["y"] {
		t.Errorf("a.ItemsSeen = %v, want {x, y}", got.Baseline.ItemsSeen)
	} else if len(got.Baseline.ItemsSeen) != 2 {
		t.Errorf("a.ItemsSeen has %d entries, want 2", len(got.Baseline.ItemsSeen))
	}
	if got := st.Sources["b"]; got == nil || !got.Baseline.ItemsSeen["only"] {
		t.Errorf("b not migrated correctly: %+v", got)
	}
}

func TestStateLoadV2RoundTrips(t *testing.T) {
	// The current v2 format should load without migration.
	v2 := `{
  "version": 2,
  "last_run": "2026-01-01T00:00:00Z",
  "sources": {
    "a": {
      "items_seen": {"x": true},
      "items_count": 1,
      "items_hash": "sha256:abc"
    }
  }
}`
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Sources["a"]; got == nil {
		t.Fatalf("source a missing")
	} else {
		if got.Baseline.ItemsHash != "sha256:abc" {
			t.Errorf("ItemsHash = %q, want sha256:abc", got.Baseline.ItemsHash)
		}
		if got.Baseline.ItemsCount != 1 {
			t.Errorf("ItemsCount = %d, want 1", got.Baseline.ItemsCount)
		}
	}
}

func TestStatePruneRemovesOrphans(t *testing.T) {
	st := &State{
		Version: CurrentVersion,
		Sources: map[string]*SourceState{
			"keep":  {Baseline: &Baseline{ItemsSeen: map[string]bool{"x": true}}, Record: &Record{}},
			"drop1": {Baseline: &Baseline{ItemsSeen: map[string]bool{"y": true}}, Record: &Record{}},
			"drop2": {Baseline: &Baseline{ItemsSeen: map[string]bool{"z": true}}, Record: &Record{}},
		},
	}
	st.Prune(map[string]bool{"keep": true})
	if _, ok := st.Sources["keep"]; !ok {
		t.Errorf("Prune removed 'keep' (it should have been preserved)")
	}
	if _, ok := st.Sources["drop1"]; ok {
		t.Errorf("Prune did not remove 'drop1'")
	}
	if _, ok := st.Sources["drop2"]; ok {
		t.Errorf("Prune did not remove 'drop2'")
	}
}

// Using a sentinel keeps the test output deterministic.
var errSentinel = &sentinelErr{}

type sentinelErr struct{}

func (sentinelErr) Error() string { return "sentinel error" }

func TestIsLegacyHTMLID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"txt:0123456789abcdef", true},
		{"txt:fedcba9876543210", true},
		{"Claude Opus 4.7", false},
		{"txt:tooshort", false},
		{"txt:ZZZZZZZZZZZZZZZZ", false}, // non-hex
		{"abc123def456", false},
		{"", false},
		{"txt:", false},
	}
	for _, c := range cases {
		if got := isLegacyHTMLID(c.in); got != c.want {
			t.Errorf("isLegacyHTMLID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
