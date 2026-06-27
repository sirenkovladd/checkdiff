package state

// Baseline is the diff baseline for one source: the set of item
// IDs last observed, the count, and a stable fingerprint hash.
// This is the only state a fetcher reads when computing the
// added/removed diff on the next run; the rest of the per-source
// state (timestamps, error message, last-diff counts) is in
// Record.
//
// Separating Baseline from Record lets the on-disk format
// evolve (e.g. add a per-source retry counter) without touching
// the diff code, and lets the web UI read just the baseline
// (for "what's currently in the set") without paying for the
// run-time fields.
type Baseline struct {
	// ItemsSeen is the set of item IDs last observed for this
	// source. The diff is computed by comparing this against
	// the current set on each check.
	ItemsSeen map[string]bool
	// ItemsHash is a sha256 fingerprint of the sorted item IDs.
	// The web UI shows this so the user can see at a glance
	// whether the item set has changed since the last run.
	ItemsHash string
	// ItemsCount is the number of items in the most recent set.
	ItemsCount int
}
