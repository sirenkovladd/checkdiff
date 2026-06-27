package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// currentStateVersion is the schema version written by this build.
// Bump it whenever the on-disk shape of State or SourceState changes.
const currentStateVersion = 2

// State is what we remember between runs.
//
//   - Version lets us migrate the file format in the future.
//   - LastRun is informational (so the user can see in the file when
//     we last successfully checked).
//   - Sources maps source ID -> per-source runtime state. Each
//     source tracks the set of item IDs last seen (used for the
//     add/remove diff) plus operational fields (last run time, next
//     scheduled run, error from the last attempt, diff counts, and
//     a hash fingerprint of the current item set).
type State struct {
	Version int                     `json:"version"`
	LastRun time.Time               `json:"last_run"`
	Sources map[string]*SourceState `json:"sources"`
}

// SourceState is the per-source runtime state. The ItemsSeen field
// preserves the old v1 behavior (set of item IDs last seen) so the
// add/remove diff continues to work after the v1→v2 migration.
// Everything else is new and is written by the daemon after each
// check completes.
type SourceState struct {
	// ItemsSeen is the set of item IDs last observed for this source.
	// The diff is computed by comparing this against the current set.
	ItemsSeen map[string]bool `json:"items_seen"`
	// LastRun is the timestamp of the most recent successful (or
	// attempted) check. Zero if the source has never run.
	LastRun time.Time `json:"last_run,omitempty"`
	// NextRun is the timestamp at which the next check is scheduled.
	// Zero if the source has never run.
	NextRun time.Time `json:"next_run,omitempty"`
	// LastError is the error message from the most recent failed
	// check. Empty when the last check succeeded.
	LastError string `json:"last_error,omitempty"`
	// LastAdded is the number of items added in the most recent check.
	LastAdded int `json:"last_added,omitempty"`
	// LastRemoved is the number of items removed in the most recent check.
	LastRemoved int `json:"last_removed,omitempty"`
	// ItemsHash is a sha256 fingerprint of the sorted item IDs. The
	// web UI shows this so the user can see at a glance whether the
	// item set has changed since the last run.
	ItemsHash string `json:"items_hash,omitempty"`
	// ItemsCount is the number of items in the most recent set.
	ItemsCount int `json:"items_count,omitempty"`
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				Version: currentStateVersion,
				Sources: map[string]*SourceState{},
			}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}

	// First pass: parse into a raw shape so we can detect the v1
	// format (Sources is map[string]map[string]bool) and migrate
	// it to v2 (Sources is map[string]*SourceState). Doing this in
	// a typed way (i.e. unmarshaling into State directly) would
	// silently lose data because json.Unmarshal would happily
	// decode v1 into a zero-value v2 SourceState's ItemsSeen.
	var raw struct {
		Version int                       `json:"version"`
		LastRun time.Time                 `json:"last_run"`
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if raw.Version == 0 {
		raw.Version = currentStateVersion
	}
	if raw.Sources == nil {
		raw.Sources = map[string]json.RawMessage{}
	}

	s := &State{
		Version: currentStateVersion,
		LastRun: raw.LastRun,
		Sources: make(map[string]*SourceState, len(raw.Sources)),
	}
	for id, msg := range raw.Sources {
		// Try v2 shape first.
		var v2 SourceState
		if err := json.Unmarshal(msg, &v2); err == nil && v2.ItemsSeen != nil {
			s.Sources[id] = &v2
			continue
		}
		// Fall back to v1 shape: Sources is map[string]map[string]bool.
		var v1Items map[string]bool
		if err := json.Unmarshal(msg, &v1Items); err == nil {
			s.Sources[id] = &SourceState{ItemsSeen: v1Items}
			continue
		}
		// Neither shape decoded cleanly. Skip the source rather than
		// fail the whole load — the next check will re-baseline it.
	}

	// Migrate legacy html state. Earlier versions stored item IDs as
	// "txt:<16 hex>" (sha256 of element text). The current code tracks
	// html items by their text content directly so additions and
	// removals are detectable. Silently drop any source whose state
	// still uses the old hash format — the next run will treat it as
	// a first-time baseline (no "new" notification flood).
	for id, src := range s.Sources {
		for k := range src.ItemsSeen {
			if isLegacyHTMLID(k) {
				delete(s.Sources, id)
				break
			}
		}
	}

	return s, nil
}

// isLegacyHTMLID reports whether k matches the old html item-ID
// format ("txt:" + 16 lowercase hex chars = sha256 prefix). The
// pattern is specific enough that real page text is extremely
// unlikely to collide.
func isLegacyHTMLID(k string) bool {
	if len(k) != 20 || !strings.HasPrefix(k, "txt:") {
		return false
	}
	for _, c := range k[4:] {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func saveState(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// remember updates the in-memory state for a source with the
// current item set and the run metadata. Pass now=time.Now() and
// the interval used to schedule the next run (computed by the
// caller from Source.ResolvedInterval). Pass added/removed counts
// and an optional error string from the most recent check.
//
// ItemsHash is computed from the sorted item IDs so the web UI
// can show a stable fingerprint of the current set.
func (s *State) remember(sourceID string, items []Item, now time.Time, nextInterval time.Duration, added, removed int, lastErr string) {
	seen := make(map[string]bool, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		seen[it.ID] = true
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte("\n"))
	}

	src := &SourceState{
		ItemsSeen:   seen,
		LastRun:     now,
		NextRun:     now.Add(nextInterval),
		LastError:   lastErr,
		LastAdded:   added,
		LastRemoved: removed,
		ItemsHash:   hex.EncodeToString(h.Sum(nil)),
		ItemsCount:  len(items),
	}
	s.Sources[sourceID] = src
}
