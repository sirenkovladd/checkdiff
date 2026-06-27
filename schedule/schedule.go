// Package schedule is the seam that lets one source's interval
// be expressed as either a Go duration ("1h", "30m") or a
// 5-field cron expression ("0 */6 * * *"). Both code paths
// converge on intervalFn, a func(time.Time) time.Time that
// returns the next run time after a given timestamp. The
// daemon doesn't need to know which format a source uses —
// only Parse knows, and only at config load time.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// intervalFn returns the next run time after `now`. It's the
// interface the daemon uses to schedule a source's next check,
// regardless of whether the user's config uses a Go duration or a
// cron expression.
//
// Both code paths converge on this type so the rest of the daemon
// doesn't need to know which format a source uses. The schedule
// IntervalFn returns the next run time after `now`. It's the
// interface the daemon uses to schedule a source's next check,
// regardless of whether the user's config uses a Go duration
// or a cron expression.
//
// Both code paths converge on this type so the rest of the
// daemon doesn't need to know which format a source uses. The
// schedule parser is the only place the choice is made.
type IntervalFn func(now time.Time) time.Time

// parseInterval parses a scheduling string and returns the
// intervalFn that produces the next run time after any given
// timestamp. The string is interpreted in one of two ways:
//
//   - If it contains whitespace, treat as a standard 5-field cron
//     expression (minute hour day-of-month month day-of-week),
//     parsed with robfig/cron/v3's ParseStandard.
//   - Otherwise, treat as a Go duration string ("1h", "30m",
//     "15s", "2h30m") and return a fixed-interval scheduler.
//
// An empty string is rejected: callers should default to the
// global [check].check_interval before calling this. Whitespace-
// only strings are also rejected.
//
// The minimum supported interval is 1 minute (matching the
// existing validation in loadConfig). This rules out cron
// expressions that fire more often than once a minute (e.g.
// "* * * * * *" — 6 fields — won't parse as standard 5-field
// cron, so the user gets a clear error rather than a silent
// surprise).
func Parse(s string) (IntervalFn, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("interval is empty")
	}
	if strings.ContainsAny(s, " \t") {
		// Standard 5-field cron. ParseStandard rejects 6-field
		// "seconds" cron expressions, so a user who writes "* * *
		// * * *" gets a clear error rather than a scheduler that
		// runs every second.
		sched, err := cron.ParseStandard(s)
		if err != nil {
			return nil, fmt.Errorf("parse cron %q: %w", s, err)
		}
		return func(t time.Time) time.Time { return sched.Next(t) }, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("parse duration %q: %w", s, err)
	}
	if d < time.Minute {
		return nil, fmt.Errorf("interval must be >= 1 minute (got %s)", d)
	}
	return func(t time.Time) time.Time { return t.Add(d) }, nil
}
