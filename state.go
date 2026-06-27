package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
//   - Path is the on-disk path the state was loaded from. The
//     daemon needs this to call saveState() from contexts (the
//     per-source goroutines, hot-reload, etc.) that don't have
//     the path passed in. It's not serialized.
//
// mu guards the Sources map. Multiple per-source goroutines
// (one per enabled source) call remember/recordError concurrently
// while the web handler reads via All() — every read and write
// of the map (or any *SourceState value it points to) must
// happen under mu. RWMutex is the right primitive: reads
// (web /api/state, the per-goroutine "have I run before" check
// in checkOne) are much more frequent than writes (one per
// check, which is on the order of seconds).
type State struct {
	mu      sync.RWMutex
	Version int                     `json:"version"`
	LastRun time.Time               `json:"last_run"`
	Sources map[string]*SourceState `json:"sources"`
	path    string                  `json:"-"`
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
		// Use errors.Is (not os.IsNotExist) so wrapped errors are
		// recognized. os.IsNotExist returns false for errors wrapped
		// with fmt.Errorf("...: %w", err) — see the os.IsNotExist
		// doc comment, which explicitly redirects wrapped-error
		// callers to errors.Is(err, fs.ErrNotExist).
		if errors.Is(err, fs.ErrNotExist) {
			return &State{
				Version: currentStateVersion,
				Sources: map[string]*SourceState{},
				path:    path,
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
		path:    path,
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

	// Prune state entries for sources that no longer exist in the
	// config. The daemon calls Prune() once at startup with the
	// current set of source IDs; runtime removals are handled by
	// daemon.Reload (which calls Prune synchronously after the
	// reconcile). Without this, state.json grows without bound as
	// sources come and go.
	//
	// loadState itself doesn't know which source IDs are valid —
	// the config is loaded separately — so the pruning step lives
	// in its own method.

	return s, nil
}

// Prune removes state entries for source IDs that are not in
// the supplied set. Called at startup (with the config's source
// IDs) and after every config reload (with the new config's
// source IDs). Locked because it can be called from the
// hot-reload path while per-source goroutines are running.
func (s *State) Prune(validIDs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.Sources {
		if !validIDs[id] {
			delete(s.Sources, id)
		}
	}
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

// saveStateMu serializes concurrent calls to saveState. Without
// it, two per-source goroutines that finish checks at the same
// instant both write to the same .tmp file, and one's rename
// races the other's write — leaving the .tmp file in an
// inconsistent state on disk.
var saveStateMu sync.Mutex

// saveStateSnapshot is a deep-copy representation of State used
// only as the source of truth for the JSON encoder. We pass it
// to json.MarshalIndent so the encoder iterates over a stable
// copy rather than the live, concurrently-mutated map.
type saveStateSnapshot struct {
	Version int                                  `json:"version"`
	LastRun time.Time                            `json:"last_run"`
	Sources map[string]*saveStateSourceSnapshot  `json:"sources"`
}

type saveStateSourceSnapshot struct {
	ItemsSeen   map[string]bool `json:"items_seen"`
	LastRun     time.Time       `json:"last_run,omitempty"`
	NextRun     time.Time       `json:"next_run,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	LastAdded   int             `json:"last_added,omitempty"`
	LastRemoved int             `json:"last_removed,omitempty"`
	ItemsHash   string          `json:"items_hash,omitempty"`
	ItemsCount  int             `json:"items_count,omitempty"`
}

func saveState(path string, s *State) error {
	saveStateMu.Lock()
	defer saveStateMu.Unlock()
	// Snapshot under the state lock so concurrent per-source
	// goroutines (which call remember / recordError) can't
	// mutate the map while the JSON encoder iterates it.
	s.mu.RLock()
	snap := saveStateSnapshot{
		Version: s.Version,
		LastRun: s.LastRun,
		Sources: make(map[string]*saveStateSourceSnapshot, len(s.Sources)),
	}
	for id, src := range s.Sources {
		cp := saveStateSourceSnapshot{
			LastRun:     src.LastRun,
			NextRun:     src.NextRun,
			LastError:   src.LastError,
			LastAdded:   src.LastAdded,
			LastRemoved: src.LastRemoved,
			ItemsHash:   src.ItemsHash,
			ItemsCount:  src.ItemsCount,
		}
		if src.ItemsSeen != nil {
			cp.ItemsSeen = make(map[string]bool, len(src.ItemsSeen))
			for k, v := range src.ItemsSeen {
				cp.ItemsSeen[k] = v
			}
		} else {
			cp.ItemsSeen = map[string]bool{}
		}
		snap.Sources[id] = &cp
	}
	s.mu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// All returns a snapshot of all per-source runtime states. The
// map values are deep copies, so callers can mutate the returned
// map without affecting the in-memory state. The web UI's
// /api/state handler is the primary consumer.
//
// Called from web handlers (concurrent with per-source goroutines
// that call remember/recordError), so the read is guarded by the
// state mutex.
func (s *State) All() map[string]*SourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*SourceState, len(s.Sources))
	for id, src := range s.Sources {
		cp := *src
		cp.ItemsSeen = make(map[string]bool, len(src.ItemsSeen))
		for k, v := range src.ItemsSeen {
			cp.ItemsSeen[k] = v
		}
		out[id] = &cp
	}
	return out
}

// remember updates the in-memory state for a source with the
// current item set and the run metadata. Pass now=time.Now() and
// the next scheduled run time (computed by the caller via the
// source's intervalFn; the helper doesn't know whether the
// source is on a fixed duration or a cron expression). Pass
// added/removed counts and an optional error string from the
// most recent check.
//
// ItemsHash is computed from the sorted item IDs so the web UI
// can show a stable fingerprint of the current set.
//
// Called from per-source goroutines, which can run concurrently.
// The Sources map mutation is guarded by the state mutex.
func (s *State) remember(sourceID string, items []Item, now, nextRun time.Time, added, removed int, lastErr string) {
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
		NextRun:     nextRun,
		LastError:   lastErr,
		LastAdded:   added,
		LastRemoved: removed,
		// "sha256:" prefix makes the hash self-identifying — useful
		// in the web UI and in any future log output where the user
		// sees a bare 64-char hex string and wonders which hash
		// algorithm produced it.
		ItemsHash:  "sha256:" + hex.EncodeToString(h.Sum(nil)),
		ItemsCount: len(items),
	}
	s.mu.Lock()
	s.Sources[sourceID] = src
	s.mu.Unlock()
}

// recordError updates only the operational fields (timestamps,
// error message) of a source's state, leaving ItemsSeen and
// ItemsHash untouched. Used when a fetch fails: we don't want to
// lose the baseline, but we do want the web UI to surface the
// error.
//
// If the source has no state yet, a minimal SourceState is
// created so the web UI can show the error from the first run.
//
// Called from per-source goroutines, which can run concurrently.
// The Sources map mutation is guarded by the state mutex.
func (s *State) recordError(sourceID string, now, nextRun time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.Sources[sourceID]
	if !ok {
		src = &SourceState{
			ItemsSeen: map[string]bool{},
		}
		s.Sources[sourceID] = src
	}
	src.LastRun = now
	src.NextRun = nextRun
	src.LastError = err.Error()
	// ItemsSeen / ItemsHash / LastAdded / LastRemoved are left
	// alone — a failed fetch shouldn't reset the diff baseline.
}
