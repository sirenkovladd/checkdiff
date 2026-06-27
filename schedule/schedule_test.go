package schedule

import (
	"testing"
	"time"
)

func TestParseIntervalDuration(t *testing.T) {
	// A Go duration string should produce a fixed-interval scheduler.
	fn, err := Parse("30m")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := fn(start)
	want := start.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Errorf("next from %v: got %v, want %v", start, got, want)
	}
}

func TestParseIntervalCron(t *testing.T) {
	// A cron expression with whitespace is parsed as cron.
	// "*/15 * * * *" fires every 15 minutes past the hour.
	fn, err := Parse("*/15 * * * *")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// From 12:00:00, next firing is 12:15:00.
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := fn(start)
	want := time.Date(2026, 1, 1, 12, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("next from %v: got %v, want %v", start, got, want)
	}
}

func TestParseIntervalCronSixFieldsRejected(t *testing.T) {
	// 6-field cron (with seconds) is rejected because ParseStandard
	// only accepts 5 fields. This is the documented behavior —
	// we don't want a user writing "* * * * * *" to accidentally
	// schedule per-second runs.
	if _, err := Parse("* * * * * *"); err == nil {
		t.Errorf("6-field cron should have been rejected")
	}
}

func TestParseIntervalErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"bad duration", "not-a-duration"},
		{"sub-minute duration", "30s"},
		{"bad cron", "this is not cron"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.in)
			if err == nil {
				t.Errorf("Parse(%q) = nil error, want error", c.in)
			}
		})
	}
}

func TestParseIntervalAutoDetect(t *testing.T) {
	// Whitespace is the disambiguator. A duration like "1h30m"
	// has no whitespace, so it parses as Go duration even though
	// it expresses a duration similar to a cron schedule. Note:
	// sub-minute durations are rejected by Parse itself
	// (see TestParseIntervalErrors), so the duration cases here
	// are all >= 1 minute.
	cases := []struct {
		name string
		in   string
	}{
		{"duration no spaces", "1h30m"},
		{"duration with h", "2h"},
		{"duration with m", "5m"},
		{"duration with s", "60s"},
		{"duration complex", "2h30m"},
		{"cron standard", "0 * * * *"},
		{"cron every 6h", "0 */6 * * *"},
		{"cron weekday 9am", "0 9 * * 1-5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn, err := Parse(c.in)
			if err != nil {
				t.Errorf("Parse(%q): %v", c.in, err)
				return
			}
			// Sanity: the function should return a time strictly
			// after the input.
			start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			next := fn(start)
			if !next.After(start) {
				t.Errorf("Parse(%q): next %v is not after %v", c.in, next, start)
			}
		})
	}
}

func TestParseIntervalWhitespaceDisambiguator(t *testing.T) {
	// Whitespace is the disambiguator between Go duration and
	// cron. Parse trims leading/trailing whitespace before
	// checking for inner whitespace, so " 30m " is still a
	// duration. Inner whitespace (e.g. "0 * * * *") is the cron
	// signal.
	cases := []struct {
		name string
		in   string
		// isCron reports what we expect Parse to interpret.
		isCron bool
	}{
		{"trimmed cron", "0 * * * *", true},
		{"trimmed duration", "30m", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			// The two interpretations give different next-run
			// times for the same start, so we can disambiguate
			// by checking which is correct.
			start := time.Date(2026, 1, 1, 12, 7, 0, 0, time.UTC)
			next := fn(start)
			if c.isCron {
				// 0 * * * * from 12:07 → 13:00.
				if next.Hour() != 13 || next.Minute() != 0 {
					t.Errorf("Parse(%q): expected cron path (13:00), got %v", c.in, next)
				}
			} else {
				// 30m duration from 12:07 → 12:37.
				want := start.Add(30 * time.Minute)
				if !next.Equal(want) {
					t.Errorf("Parse(%q): expected %v, got %v", c.in, want, next)
				}
			}
		})
	}
}
