package main

import (
	"fmt"
	"time"
)

// systemdTimerTemplate is the user-level systemd timer unit that
// triggers checkdiff.service. The two %s placeholders are:
//
//  1. The human-readable interval (e.g. "10m0s") for the Description
//  2. The OnCalendar value, produced by onCalendarFor below
//
// OnCalendar=... schedules the timer at fixed times; Persistent=true
// catches up on missed ticks after a reboot — the systemd analogue
// of launchd's "fire as soon as it wakes" intent.
const systemdTimerTemplate = `[Unit]
Description=Run checkdiff every %s

[Timer]
OnCalendar=%s
# If we missed a tick (server was off), catch up on the next boot.
Persistent=true

[Install]
WantedBy=timers.target
`

// onCalendarFor returns a systemd OnCalendar value for the given
// duration. It supports durations that align to whole minutes,
// hours, or days and that evenly divide the next larger unit
// (60 minutes, 24 hours, 30 days). For intervals outside that set,
// an error is returned — the user is expected to pick a "round"
// value (e.g. 7m, 9m are rejected; 1m, 2m, 3m, 5m, 6m, 10m, 12m,
// 15m, 20m, 30m are accepted).
//
// The output uses anchored "top of the unit" semantics to match the
// historical "hourly" behavior: 10m fires at :00, :10, :20, … :50;
// 1h fires at the top of every hour; 1d fires at midnight.
func onCalendarFor(d time.Duration) (string, error) {
	if d < time.Minute {
		return "", fmt.Errorf("interval must be >= 1 minute (got %s)", d)
	}
	if d%time.Second != 0 {
		return "", fmt.Errorf("interval must align to a whole second (got %s)", d)
	}
	// Day-level (>= 24h). Must align to a whole day.
	if d >= 24*time.Hour {
		if d%(24*time.Hour) != 0 {
			return "", fmt.Errorf("interval must align to a whole day at 1d+ (got %s)", d)
		}
		days := int(d / (24 * time.Hour))
		switch days {
		case 1:
			return "daily", nil
		case 7:
			return "weekly", nil
		}
		// systemd's day-of-month field uses a per-month cycle, so
		// 30 is the practical limit. Accept divisors of 30 (1, 2, 3,
		// 5, 6, 10, 15, 30) and reject the rest.
		if 30%days == 0 {
			return fmt.Sprintf("*-*-*/%d 00:00:00", days), nil
		}
		return "", fmt.Errorf("interval must be 1, 2, 3, 5, 6, 7, 10, 15, or 30 days (got %s)", d)
	}
	// Hour-level (1h..24h). Must align to a whole hour and divide
	// 24h evenly (so 1h, 2h, 3h, 4h, 6h, 8h, 12h are accepted; 5h,
	// 7h are rejected because "*-*-* 0/5:00:00" wouldn't fire at
	// 5am, 10am, 15:00, 20:00 — it would fire at 0, 5, 10, 15, 20
	// of each day, missing the 24-hour cadence the user wants).
	if d >= time.Hour {
		if d%time.Hour != 0 {
			return "", fmt.Errorf("interval must align to a whole hour at 1h+ (got %s)", d)
		}
		hours := int(d / time.Hour)
		if hours == 1 {
			return "hourly", nil
		}
		if 24%hours == 0 {
			return fmt.Sprintf("*-*-* 0/%d:00:00", hours), nil
		}
		return "", fmt.Errorf("interval must evenly divide 24h (got %s)", d)
	}
	// Minute-level (1m..60m). Express as total minutes and check
	// that 60 is divisible by the result (so 1, 2, 3, 4, 5, 6, 10,
	// 12, 15, 20, 30 are accepted; 7, 8, 9, 11 are not). Compound
	// minutes like "1h30m" work because they're 90 minutes, which
	// doesn't divide 60 — so they're rejected here.
	minutes := int(d / time.Minute)
	if 60%minutes != 0 {
		return "", fmt.Errorf("interval must evenly divide an hour (got %s)", d)
	}
	if minutes == 1 {
		return "minutely", nil
	}
	return fmt.Sprintf("*:0/%d", minutes), nil
}
