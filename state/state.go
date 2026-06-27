// Package state is the in-memory + on-disk record of what
// checkdiff has seen for each configured source. The shape
// mirrors the JSON file (state.json by default): a top-level
// State with versioning and a per-source map; each per-source
// entry splits into a Baseline (the diff baseline) and a Record
// (the operational fields).
//
// Concurrency: the State.mu mutex guards the Sources map and
// every *SourceState value it points to. RWMutex is the right
// primitive — reads (web /api/state, the per-goroutine "have I
// run before" check in check.One) are much more frequent than
// writes (one per check, on the order of seconds).
//
// On-disk format: the v2 shape (a single SourceState with
// ItemsSeen + operational fields flattened) is preserved by
// the snapshot types in saveState, so the in-memory split
// between Baseline and Record is invisible to the on-disk
// format and to the v1→v2 migration.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"checkdiff/source"
)

// CurrentVersion is the schema version written by this build.
// Bump it whenever the on-disk shape of State or SourceState
// changes.
const CurrentVersion = 2

// State is what we remember between runs.
//
//   - Version lets us migrate the file format in the future.
//   - LastRun is informational (so the user can see in the file
//     when we last successfully checked).
//   - Sources maps source ID -> per-source runtime state. Each
//     source splits its state into a Baseline (the diff
//     baseline) and a Record (the operational fields).
//   - Path is the on-disk path the state was loaded from. The
//     daemon needs this to call saveState from contexts (the
//     per-source goroutines, hot-reload, etc.) that don't have
//     the path passed in. It's not serialized.
type State struct {
	mu      sync.RWMutex
	Version int                     `json:"version"`
	LastRun time.Time               `json:"last_run"`
	Sources map[string]*SourceState `json:"sources"`
	path    string                  `json:"-"`
}

// SourceState is the per-source runtime state. It composes a
// Baseline and a Record so the diff code and the operational
// code can be changed independently.
type SourceState struct {
	Baseline *Baseline
	Record   *Record
}

// Load reads state from path. Missing file is not an error —
// it returns an empty state at the current version. A file
// from an older version is migrated (see the v1→v2 path
// below).
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// Use errors.Is (not os.IsNotExist) so wrapped errors
		// are recognized. os.IsNotExist returns false for
		// errors wrapped with fmt.Errorf("...: %w", err) — see
		// the os.IsNotExist doc comment, which explicitly
		// redirects wrapped-error callers to errors.Is(err,
		// fs.ErrNotExist).
		if errors.Is(err, fs.ErrNotExist) {
			return &State{
				Version: CurrentVersion,
				Sources: map[string]*SourceState{},
				path:    path,
			}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}

	// First pass: parse into a raw shape so we can detect the
	// v1 format (Sources is map[string]map[string]bool) and
	// migrate it to v2 (Sources is map[string]*SourceState).
	// Doing this in a typed way (i.e. unmarshaling into State
	// directly) would silently lose data because json.Unmarshal
	// would happily decode v1 into a zero-value v2
	// SourceState's ItemsSeen.
	var raw struct {
		Version int                        `json:"version"`
		LastRun time.Time                  `json:"last_run"`
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if raw.Version == 0 {
		raw.Version = CurrentVersion
	}
	if raw.Sources == nil {
		raw.Sources = map[string]json.RawMessage{}
	}

	s := &State{
		Version: CurrentVersion,
		LastRun: raw.LastRun,
		Sources: make(map[string]*SourceState, len(raw.Sources)),
		path:    path,
	}
	for id, msg := range raw.Sources {
		// Try v2 shape first.
		var v2 decodedV2
		if err := json.Unmarshal(msg, &v2); err == nil && v2.ItemsSeen != nil {
			s.Sources[id] = v2.toSourceState()
			continue
		}
		// Fall back to v1 shape: Sources is map[string]map[string]bool.
		var v1Items map[string]bool
		if err := json.Unmarshal(msg, &v1Items); err == nil {
			s.Sources[id] = &SourceState{
				Baseline: &Baseline{ItemsSeen: v1Items},
				Record:   &Record{},
			}
			continue
		}
		// Neither shape decoded cleanly. Skip the source
		// rather than fail the whole load — the next check
		// will re-baseline it.
	}

	// Migrate legacy html state. Earlier versions stored item
	// IDs as "txt:<16 hex>" (sha256 of element text). The
	// current code tracks html items by their text content
	// directly so additions and removals are detectable.
	// Silently drop any source whose state still uses the old
	// hash format — the next run will treat it as a first-time
	// baseline (no "new" notification flood).
	for id, src := range s.Sources {
		if src.Baseline == nil {
			continue
		}
		for k := range src.Baseline.ItemsSeen {
			if isLegacyHTMLID(k) {
				delete(s.Sources, id)
				break
			}
		}
	}

	// Prune state entries for sources that no longer exist in
	// the config. The daemon calls Prune() once at startup
	// with the current set of source IDs; runtime removals
	// are handled by daemon.Reload (which calls Prune
	// synchronously after the reconcile). Without this,
	// state.json grows without bound as sources come and go.
	//
	// Load itself doesn't know which source IDs are valid —
	// the config is loaded separately — so the pruning step
	// lives in its own method.

	return s, nil
}

// decodedV2 is the on-disk v2 shape: a single struct with the
// baseline fields (items_seen) and the record fields
// (timestamps, error, counts) flattened. The in-memory
// SourceState splits these into Baseline and Record, but the
// on-disk shape is unchanged.
type decodedV2 struct {
	ItemsSeen   map[string]bool `json:"items_seen"`
	LastRun     time.Time       `json:"last_run,omitempty"`
	NextRun     time.Time       `json:"next_run,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	LastAdded   int             `json:"last_added,omitempty"`
	LastRemoved int             `json:"last_removed,omitempty"`
	ItemsHash   string          `json:"items_hash,omitempty"`
	ItemsCount  int             `json:"items_count,omitempty"`
}

func (d decodedV2) toSourceState() *SourceState {
	return &SourceState{
		Baseline: &Baseline{
			ItemsSeen:  d.ItemsSeen,
			ItemsHash:  d.ItemsHash,
			ItemsCount: d.ItemsCount,
		},
		Record: &Record{
			LastRun:     d.LastRun,
			NextRun:     d.NextRun,
			LastError:   d.LastError,
			LastAdded:   d.LastAdded,
			LastRemoved: d.LastRemoved,
		},
	}
}

// Path returns the on-disk path the state was loaded from
// (and to which Save writes). Returns the empty string for a
// state that was constructed in memory without a load.
func (s *State) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetPath records the on-disk path. Typically called by Load
// right after reading; tests that build a State by hand for
// Save to a custom path call this before calling Save.
func (s *State) SetPath(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = p
}

// SetLastRun records the timestamp at which the daemon last
// stopped. Called from main on shutdown so the on-disk state
// reflects when the daemon last ran, even if no source check
// has happened since.
func (s *State) SetLastRun(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastRun = t
}

// Snapshot returns a deep copy of the per-source state for
// id, or nil if there is no state for that source. Used by
// check.One to read the baseline and run-record without
// holding the state lock for the whole diff/notify
// computation. The returned *SourceState is independent of
// the in-memory state: callers may mutate it freely.
func (s *State) Snapshot(id string) *SourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.Sources[id]
	if !ok {
		return nil
	}
	return cloneSourceState(src)
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

// isLegacyHTMLID reports whether k matches the old html
// item-ID format ("txt:" + 16 lowercase hex chars = sha256
// prefix). The pattern is specific enough that real page text
// is extremely unlikely to collide.
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

// saveSnapshot is a deep-copy representation of State used
// only as the source of truth for the JSON encoder. We pass
// it to json.MarshalIndent so the encoder iterates over a
// stable copy rather than the live, concurrently-mutated map.
type saveSnapshot struct {
	Version int                                 `json:"version"`
	LastRun time.Time                           `json:"last_run"`
	Sources map[string]*saveSourceSnapshot      `json:"sources"`
}

type saveSourceSnapshot struct {
	ItemsSeen   map[string]bool `json:"items_seen"`
	LastRun     time.Time       `json:"last_run,omitempty"`
	NextRun     time.Time       `json:"next_run,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	LastAdded   int             `json:"last_added,omitempty"`
	LastRemoved int             `json:"last_removed,omitempty"`
	ItemsHash   string          `json:"items_hash,omitempty"`
	ItemsCount  int             `json:"items_count,omitempty"`
}

func snapshotSource(src *SourceState) *saveSourceSnapshot {
	if src == nil {
		return &saveSourceSnapshot{ItemsSeen: map[string]bool{}}
	}
	cp := &saveSourceSnapshot{}
	if src.Baseline != nil {
		cp.ItemsHash = src.Baseline.ItemsHash
		cp.ItemsCount = src.Baseline.ItemsCount
		if src.Baseline.ItemsSeen != nil {
			cp.ItemsSeen = make(map[string]bool, len(src.Baseline.ItemsSeen))
			for k, v := range src.Baseline.ItemsSeen {
				cp.ItemsSeen[k] = v
			}
		} else {
			cp.ItemsSeen = map[string]bool{}
		}
	} else {
		cp.ItemsSeen = map[string]bool{}
	}
	if src.Record != nil {
		cp.LastRun = src.Record.LastRun
		cp.NextRun = src.Record.NextRun
		cp.LastError = src.Record.LastError
		cp.LastAdded = src.Record.LastAdded
		cp.LastRemoved = src.Record.LastRemoved
	}
	return cp
}

// Save writes the in-memory state to disk. The on-disk shape
// is the v2 format: a single flat object per source. The
// in-memory Baseline/Record split is invisible to the file.
func Save(path string, s *State) error {
	saveStateMu.Lock()
	defer saveStateMu.Unlock()
	// Snapshot under the state lock so concurrent per-source
	// goroutines (which call Remember / RecordError) can't
	// mutate the map while the JSON encoder iterates it.
	s.mu.RLock()
	snap := saveSnapshot{
		Version: s.Version,
		LastRun: s.LastRun,
		Sources: make(map[string]*saveSourceSnapshot, len(s.Sources)),
	}
	for id, src := range s.Sources {
		snap.Sources[id] = snapshotSource(src)
	}
	s.mu.RUnlock()
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
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

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// All returns a snapshot of all per-source runtime states. The
// map values are deep copies, so callers can mutate the
// returned map without affecting the in-memory state. The web
// UI's /api/state handler is the primary consumer.
//
// Called from web handlers (concurrent with per-source
// goroutines that call Remember/RecordError), so the read is
// guarded by the state mutex.
func (s *State) All() map[string]*SourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*SourceState, len(s.Sources))
	for id, src := range s.Sources {
		out[id] = cloneSourceState(src)
	}
	return out
}

func cloneSourceState(src *SourceState) *SourceState {
	if src == nil {
		return nil
	}
	cp := &SourceState{}
	if src.Baseline != nil {
		b := *src.Baseline
		if src.Baseline.ItemsSeen != nil {
			b.ItemsSeen = make(map[string]bool, len(src.Baseline.ItemsSeen))
			for k, v := range src.Baseline.ItemsSeen {
				b.ItemsSeen[k] = v
			}
		}
		cp.Baseline = &b
	}
	if src.Record != nil {
		r := *src.Record
		cp.Record = &r
	}
	return cp
}

// Remember updates the in-memory state for a source with the
// current item set and the run metadata. Pass now=time.Now()
// and the next scheduled run time (computed by the caller via
// the source's intervalFn; the helper doesn't know whether the
// source is on a fixed duration or a cron expression). Pass
// added/removed counts and an optional error string from the
// most recent check.
//
// ItemsHash is computed from the sorted item IDs so the web
// UI can show a stable fingerprint of the current set.
//
// Called from per-source goroutines, which can run
// concurrently. The Sources map mutation is guarded by the
// state mutex.
func (s *State) Remember(sourceID string, items []source.Item, now, nextRun time.Time, added, removed int, lastErr string) {
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

	hash := "sha256:" + hex.EncodeToString(h.Sum(nil))
	// "sha256:" prefix makes the hash self-identifying —
	// useful in the web UI and in any future log output where
	// the user sees a bare 64-char hex string and wonders
	// which hash algorithm produced it.

	src := &SourceState{
		Baseline: &Baseline{
			ItemsSeen:  seen,
			ItemsHash:  hash,
			ItemsCount: len(items),
		},
		Record: &Record{
			LastRun:     now,
			NextRun:     nextRun,
			LastError:   lastErr,
			LastAdded:   added,
			LastRemoved: removed,
		},
	}
	s.mu.Lock()
	s.Sources[sourceID] = src
	s.mu.Unlock()
}

// RecordError updates only the Record fields (timestamps, error
// message) of a source's state, leaving Baseline untouched. Used
// when a fetch fails: we don't want to lose the baseline, but
// we do want the web UI to surface the error.
//
// If the source has no state yet, a minimal SourceState is
// created so the web UI can show the error from the first run.
//
// Called from per-source goroutines, which can run
// concurrently. The Sources map mutation is guarded by the
// state mutex.
func (s *State) RecordError(sourceID string, now, nextRun time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.Sources[sourceID]
	if !ok {
		src = &SourceState{
			Baseline: &Baseline{ItemsSeen: map[string]bool{}},
			Record:   &Record{},
		}
		s.Sources[sourceID] = src
	}
	if src.Record == nil {
		src.Record = &Record{}
	}
	src.Record.LastRun = now
	src.Record.NextRun = nextRun
	src.Record.LastError = err.Error()
	// Baseline / ItemsHash / LastAdded / LastRemoved are left
	// alone — a failed fetch shouldn't reset the diff baseline.
}

// Item is a minimal in-state representation of an observed
// item ID. We don't need the full source.Item here (no title,
// body, or link); the state's job is to remember which IDs
// were last seen, not to remember what they were called. The
// Item type is defined in the source package and re-imported
// here for Remember's signature; we keep a local alias for
// callers that don't want to import source.
type Item = source.Item
