package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is what we remember between runs.
//
//   - Version lets us migrate the file format in the future.
//   - Sources maps source ID -> set of item IDs we last saw.
//   - LastRun is informational (so the user can see in the file when
//     we last successfully checked).
type State struct {
	Version int                        `json:"version"`
	LastRun time.Time                  `json:"last_run"`
	Sources map[string]map[string]bool `json:"sources"`
}

const stateVersion = 1

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				Version: stateVersion,
				Sources: map[string]map[string]bool{},
			}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.Version == 0 {
		s.Version = stateVersion
	}
	if s.Sources == nil {
		s.Sources = map[string]map[string]bool{}
	}

	// Migrate legacy html state. Earlier versions stored item IDs as
	// "txt:<16 hex>" (sha256 of element text). The current code tracks
	// html items by their text content directly so additions and
	// removals are detectable. Silently drop any source whose state
	// still uses the old hash format — the next run will treat it as
	// a first-time baseline (no "new" notification flood).
	for id, m := range s.Sources {
		for k := range m {
			if isLegacyHTMLID(k) {
				delete(s.Sources, id)
				break
			}
		}
	}

	return &s, nil
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

// remember updates the in-memory state with the current items for a
// source. Callers should call saveState after the run to persist.
func (s *State) remember(sourceID string, items []Item) {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		seen[it.ID] = true
	}
	s.Sources[sourceID] = seen
}
