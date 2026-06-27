package state

import "time"

// Record is the per-source operational record: when did the last
// run happen, when is the next one scheduled, did the last run
// error, and how many items were added/removed in the most
// recent diff. Read by the web UI ("last run", "next run",
// "last diff", error badge) and by the daemon to update state
// on error paths without disturbing the diff baseline.
//
// A zero Record means "this source has never run" (used by the
// recordError path when a fetch fails before any run has
// succeeded; the caller can read a Record with a zero time to
// distinguish "not yet run" from "ran at the zero time").
type Record struct {
	// LastRun is the timestamp of the most recent successful
	// (or attempted) check. Zero if the source has never run.
	LastRun time.Time
	// NextRun is the timestamp at which the next check is
	// scheduled. Zero if the source has never run.
	NextRun time.Time
	// LastError is the error message from the most recent
	// failed check. Empty when the last check succeeded.
	LastError string
	// LastAdded is the number of items added in the most recent
	// check.
	LastAdded int
	// LastRemoved is the number of items removed in the most
	// recent check.
	LastRemoved int
}
