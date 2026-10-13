// Package check is the per-source "fetched items are in; what
// now?" decision. It owns the diff between the current set and
// the state's baseline, the first-run baseline recording, the
// notification publish, and the state update. The daemon calls
// it after a successful source.Fetch; check.One is a pure
// function over its arguments and the per-source state.
//
// Pulling this out of the main package (where it used to live
// alongside the daemon's wiring code) is what made it
// testable. The flag.*Verbose global that used to be read here
// is now an explicit argument; the test passes a literal.
package check

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"checkdiff/notify"
	"checkdiff/source"
	"checkdiff/state"
)

// ClickURLFor picks the URL ntfy should open when the user
// taps the notification. The first added item's Link wins
// (so e.g. a package tracking notification opens that
// package's detail page), otherwise the source's own Link
// (a static URL for sources where every entry points at the
// same page), otherwise the source's URL. Removed-only diffs
// and sources without any link configuration end up at s.URL.
func ClickURLFor(s *source.Source, added []source.Item) string {
	if len(added) > 0 && added[0].Link != "" {
		return added[0].Link
	}
	if s.Link != "" {
		return s.Link
	}
	return s.URL
}

// One runs the diff/notify decision for one source, after a
// successful source.Fetch. It is a pure function over its
// arguments and the in-memory state: same input, same effect.
//
// The function handles three cases:
//
//   - First run for this source: record the baseline, return
//     without publishing (no "new" notification flood).
//   - Subsequent run with no diff: just remember the new
//     items.
//   - Subsequent run with diff: publish to ntfy, then
//     remember. A fetcher may return an empty Notification
//     (no Title and no Body) to suppress the publish — the
//     raw items differed but there is nothing worth telling
//     the user about (e.g. page_llm's LLM judged the change
//     boilerplate). Suppressed runs still advance the
//     baseline so the same churn doesn't re-fire next tick.
//
// verbose controls log output. Production passes false; tests
// pass false too. The function still publishes on real diffs
// regardless of verbose, so a "verbose: true" run does not
// change behaviour, only observability.
func One(
	ctx context.Context,
	ntfy *notify.Client,
	st *state.State,
	s *source.Source,
	items []source.Item,
	now, nextRun time.Time,
	verbose bool,
) error {
	ss := st.Snapshot(s.ID)

	// First run for this source: record the baseline and stay
	// quiet. We don't want a flood of "new" notifications for
	// the 154 h3 entries that already exist on the
	// changelog.
	if ss == nil || ss.Baseline == nil {
		st.Remember(s.ID, items, now, nextRun, 0, 0, "")
		if verbose {
			log.Printf("[%s] first run, baseline set (%d items), no notification", s.ID, len(items))
		}
		return nil
	}

	// Compute the diff:
	//   added   = items in the current set that weren't seen last run
	//   removed = IDs that were seen last run but aren't present now
	// For github_file sources, the "removed" entry is the old
	// git blob SHA — not meaningful to the user — so the
	// formatter ignores it.
	currentSet := make(map[string]bool, len(items))
	var added []source.Item
	for _, it := range items {
		currentSet[it.ID] = true
		if !ss.Baseline.ItemsSeen[it.ID] {
			added = append(added, it)
		}
	}
	var removed []source.Item
	for id := range ss.Baseline.ItemsSeen {
		if !currentSet[id] {
			// The ID is the human-readable identifier for
			// html/json sources (entry text or model id). Use
			// it as both ID and Title so the notification can
			// list it directly.
			removed = append(removed, source.Item{ID: id, Title: id})
		}
	}

	// Always remember the current set, even when nothing
	// changes.
	defer st.Remember(s.ID, items, now, nextRun, len(added), len(removed), "")

	// IgnoreRemovals: removals aren't change-worthy for this
	// source (e.g. a rolling changelog window where old
	// entries dropping off the page is routine). A
	// removal-only diff publishes nothing; the baseline still
	// advances via the deferred Remember so the same removals
	// don't re-fire next tick.
	if s.IgnoreRemovals && len(added) == 0 && len(removed) > 0 {
		if verbose {
			log.Printf("[%s] %d items, 0 added, %d removed — ignored (ignore_removals)", s.ID, len(items), len(removed))
		}
		return nil
	}

	// For list-style sources with IgnoreRemovals, a mixed
	// diff notifies about the additions only. Single-value
	// sources (amazon, json_value, page_llm) keep the full
	// removed set so their Format still renders the old → new
	// pair — the old value is the "from" half of the
	// notification, not a "removal" to ignore.
	notifyRemoved := removed
	if s.IgnoreRemovals && (s.Type == "html" || s.Type == "json") {
		notifyRemoved = nil
	}

	if len(added) == 0 && len(removed) == 0 {
		if verbose {
			log.Printf("[%s] %d items, 0 added, 0 removed", s.ID, len(items))
		}
		return nil
	}

	if verbose {
		log.Printf("[%s] %d items, %d added, %d removed", s.ID, len(items), len(added), len(removed))
	}

	n := source.Format(ctx, s, added, notifyRemoved)
	if strings.TrimSpace(n.Title) == "" && strings.TrimSpace(n.Body) == "" {
		if verbose {
			log.Printf("[%s] %d items, %d added, %d removed — suppressed (no meaningful change)", s.ID, len(items), len(added), len(removed))
		}
		return nil
	}
	topic := s.ResolvedTopic(ntfy.Topic())
	if verbose {
		log.Printf("[%s] notify attempt: topic=%s title=%q body=%d bytes", s.ID, topic, n.Title, len(n.Body))
	}
	if err := ntfy.PublishTo(ctx, topic, n); err != nil {
		// Logged separately from the daemon's generic "check
		// failed" line so the user can see at a glance that
		// the failure was on the notify path, not the fetch
		// path. The fetch is in state (items remembered), so
		// the next tick will retry the publish.
		log.Printf("[%s] notify failed: %v", s.ID, err)
		return fmt.Errorf("publish: %w", err)
	}
	if verbose {
		log.Printf("[%s] notify ok: %d added, %d removed", s.ID, len(added), len(removed))
	}
	return nil
}
