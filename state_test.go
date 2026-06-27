package main

import (
	"testing"
	"time"
)

func TestStateRecordErrorCreatesEntry(t *testing.T) {
	// recordError on a source with no prior state should create
	// a minimal SourceState with the operational fields set.
	s := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	s.recordError("newsrc", now, next, errSentinel)

	got, ok := s.Sources["newsrc"]
	if !ok {
		t.Fatalf("recordError did not create entry for %q", "newsrc")
	}
	if !got.LastRun.Equal(now) {
		t.Errorf("LastRun = %v, want %v", got.LastRun, now)
	}
	if !got.NextRun.Equal(next) {
		t.Errorf("NextRun = %v, want %v", got.NextRun, next)
	}
	if got.LastError != errSentinel.Error() {
		t.Errorf("LastError = %q, want %q", got.LastError, errSentinel.Error())
	}
	// ItemsSeen is initialized to an empty map (so callers don't
	// have to nil-check) but the diff baseline is not established.
	if got.ItemsSeen == nil {
		t.Errorf("ItemsSeen is nil; should be empty map")
	}
}

func TestStateRecordErrorPreservesBaseline(t *testing.T) {
	// recordError on a source that already has a baseline must
	// NOT clobber ItemsSeen / ItemsHash / LastAdded / LastRemoved.
	// A failed fetch shouldn't reset the diff baseline.
	items := map[string]bool{"x": true, "y": true}
	s := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{
			"a": {
				ItemsSeen:   items,
				ItemsHash:   "sha256:abc",
				ItemsCount:  2,
				LastAdded:   5,
				LastRemoved: 3,
			},
		},
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	s.recordError("a", now, next, errSentinel)

	got := s.Sources["a"]
	if len(got.ItemsSeen) != 2 {
		t.Errorf("ItemsSeen was clobbered: %v", got.ItemsSeen)
	}
	if got.ItemsHash != "sha256:abc" {
		t.Errorf("ItemsHash was clobbered: %q", got.ItemsHash)
	}
	if got.ItemsCount != 2 {
		t.Errorf("ItemsCount was clobbered: %d", got.ItemsCount)
	}
	if got.LastAdded != 5 {
		t.Errorf("LastAdded was clobbered: %d", got.LastAdded)
	}
	if got.LastRemoved != 3 {
		t.Errorf("LastRemoved was clobbered: %d", got.LastRemoved)
	}
	if got.LastError != errSentinel.Error() {
		t.Errorf("LastError = %q, want %q", got.LastError, errSentinel.Error())
	}
}

func TestStateRememberHashPrefix(t *testing.T) {
	// ItemsHash must be self-identifying with the "sha256:"
	// prefix. Otherwise a bare 64-char hex string is ambiguous.
	s := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	s.remember("a", []Item{{ID: "x"}}, time.Now().UTC(), time.Now().Add(time.Hour), 0, 0, "")
	if got := s.Sources["a"].ItemsHash; len(got) < 7 || got[:7] != "sha256:" {
		t.Errorf("ItemsHash = %q, want sha256: prefix", got)
	}
}

// errSentinel is a small fixed error for the recordError tests.
// Using a sentinel keeps the test output deterministic.
var errSentinel = &sentinelErr{}

type sentinelErr struct{}

func (sentinelErr) Error() string { return "sentinel error" }
