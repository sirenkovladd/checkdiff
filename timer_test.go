package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOnCalendarFor(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		// Minute-level (the most common case for checkdiff).
		{"1m", time.Minute, "minutely"},
		{"2m", 2 * time.Minute, "*:0/2"},
		{"5m", 5 * time.Minute, "*:0/5"},
		{"10m", 10 * time.Minute, "*:0/10"},
		{"15m", 15 * time.Minute, "*:0/15"},
		{"30m", 30 * time.Minute, "*:0/30"},
		// Hour-level.
		{"1h", time.Hour, "hourly"},
		{"2h", 2 * time.Hour, "*-*-* 0/2:00:00"},
		{"3h", 3 * time.Hour, "*-*-* 0/3:00:00"},
		{"4h", 4 * time.Hour, "*-*-* 0/4:00:00"},
		{"6h", 6 * time.Hour, "*-*-* 0/6:00:00"},
		{"8h", 8 * time.Hour, "*-*-* 0/8:00:00"},
		{"12h", 12 * time.Hour, "*-*-* 0/12:00:00"},
		// Day-level.
		{"1d", 24 * time.Hour, "daily"},
		{"2d", 48 * time.Hour, "*-*-*/2 00:00:00"},
		{"3d", 72 * time.Hour, "*-*-*/3 00:00:00"},
		{"7d", 7 * 24 * time.Hour, "weekly"},
		{"15d", 15 * 24 * time.Hour, "*-*-*/15 00:00:00"},
		{"30d", 30 * 24 * time.Hour, "*-*-*/30 00:00:00"},
		// Compound minutes that happen to divide 60.
		{"1h expressed as 60m", 60 * time.Minute, "hourly"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := onCalendarFor(c.in)
			if err != nil {
				t.Fatalf("onCalendarFor(%s) error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("onCalendarFor(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestOnCalendarForErrors(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
	}{
		{"sub-minute", 30 * time.Second},
		{"7m doesn't divide hour", 7 * time.Minute},
		{"8m doesn't divide hour", 8 * time.Minute},
		{"9m doesn't divide hour", 9 * time.Minute},
		{"11m doesn't divide hour", 11 * time.Minute},
		{"1h30m doesn't divide hour", 90 * time.Minute},
		{"5h doesn't divide day", 5 * time.Hour},
		{"7h doesn't divide day", 7 * time.Hour},
		{"1h15m not aligned to hour", 75 * time.Minute},
		{"1d12h not aligned to day", 36 * time.Hour},
		{"4d", 4 * 24 * time.Hour},
		{"zero", 0},
		{"negative", -time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := onCalendarFor(c.in)
			if err == nil {
				t.Errorf("onCalendarFor(%s) = nil error, want error", c.in)
			}
		})
	}
}

func TestPrintTimer(t *testing.T) {
	// End-to-end: build the binary, run it with -print-timer, and
	// confirm the OnCalendar line matches what we expect for a 10m
	// interval. The check is stringly-typed on purpose — we want
	// the full template to render, not just the helper function.
	cfgPath := t.TempDir() + "/config.toml"
	cfg := `[ntfy]
topic = "test"
[check]
check_interval = "10m"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	// We can't shell out from a test cleanly without a built binary,
	// so just exercise the path the binary takes: load the config,
	// resolve the OnCalendar, format the template.
	c, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	d, err := time.ParseDuration(c.Check.Interval)
	if err != nil {
		t.Fatalf("parse interval: %v", err)
	}
	onCal, err := onCalendarFor(d)
	if err != nil {
		t.Fatalf("onCalendarFor: %v", err)
	}
	out := strings.Replace(systemdTimerTemplate, "%s", "placeholder", 1)
	out = strings.Replace(out, "%s", onCal, 1)
	if !strings.Contains(out, "OnCalendar=*:0/10") {
		t.Errorf("rendered timer missing OnCalendar=*:0/10:\n%s", out)
	}
	if !strings.Contains(out, "Persistent=true") {
		t.Errorf("rendered timer missing Persistent=true:\n%s", out)
	}
}

// (no extra helpers)
