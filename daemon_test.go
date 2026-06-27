package main

import (
	"context"
	"testing"
	"time"
)

func TestDaemonStartStop(t *testing.T) {
	// Use a very short interval so the ticker would fire during the
	// test if we didn't stop the daemon promptly. The URL points at
	// 127.0.0.1:1 (port 1 is reserved/unused) so the initial check
	// fails fast with a connection error — which is what we want,
	// since the test is about the goroutine lifecycle, not fetch
	// success.
	enabled := true
	cfg := &Config{
		Check: CheckConfig{Interval: "100ms"},
		Sources: []Source{
			{
				ID:      "test",
				Name:    "test source",
				Type:    "json",
				URL:     "https://127.0.0.1:1",
				Enabled: &enabled,
			},
		},
	}
	st := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	ntfy := NewNtfyClient("https://ntfy.sh", "test")

	d := newDaemon(cfg, st, ntfy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)
	// Immediately stop. The initial check goroutine will fail with
	// a connection error (logged, not asserted), then the ticker
	// loop will see the cancelled context and exit.
	d.Stop()

	d.mu.Lock()
	runners := len(d.runners)
	d.mu.Unlock()
	if runners != 0 {
		t.Errorf("after Stop: got %d runners, want 0", runners)
	}
}

func TestDaemonSkipsDisabledSources(t *testing.T) {
	disabled := false
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{
				ID:      "disabled",
				Name:    "disabled source",
				Type:    "json",
				URL:     "https://127.0.0.1:1",
				Enabled: &disabled,
			},
		},
	}
	st := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	ntfy := NewNtfyClient("https://ntfy.sh", "test")

	d := newDaemon(cfg, st, ntfy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)
	defer d.Stop()

	d.mu.Lock()
	runners := len(d.runners)
	d.mu.Unlock()
	if runners != 0 {
		t.Errorf("disabled source should not start a runner, got %d runners", runners)
	}
}

func TestDaemonEnabledDefaultsTrue(t *testing.T) {
	// A source with no Enabled field (nil pointer) should be
	// treated as enabled — this preserves backward compatibility
	// with configs written before the Enabled field existed.
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{
				ID:      "no-flag",
				Name:    "no enabled flag",
				Type:    "json",
				URL:     "https://127.0.0.1:1",
				Enabled: nil,
			},
		},
	}
	st := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	ntfy := NewNtfyClient("https://ntfy.sh", "test")

	d := newDaemon(cfg, st, ntfy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)
	defer d.Stop()

	d.mu.Lock()
	runners := len(d.runners)
	d.mu.Unlock()
	if runners != 1 {
		t.Errorf("source with nil Enabled should be treated as enabled, got %d runners", runners)
	}
}

func TestSourceIsEnabled(t *testing.T) {
	yes := true
	no := false
	cases := []struct {
		name string
		ptr  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", &yes, true},
		{"explicit false", &no, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Source{Enabled: c.ptr}
			if got := s.IsEnabled(); got != c.want {
				t.Errorf("IsEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSourceSetEnabled(t *testing.T) {
	s := &Source{}
	if !s.IsEnabled() {
		t.Errorf("new Source should default to enabled")
	}
	s.SetEnabled(false)
	if s.IsEnabled() {
		t.Errorf("SetEnabled(false) did not take effect")
	}
	s.SetEnabled(true)
	if !s.IsEnabled() {
		t.Errorf("SetEnabled(true) did not take effect")
	}
}

// TestDaemonStopIsIdempotent guards against a regression where
// calling Stop twice would deadlock on the second call (because
// the runners map is cleared after the first wait).
func TestDaemonStopIsIdempotent(t *testing.T) {
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "x", Name: "x", Type: "json", URL: "https://127.0.0.1:1"},
		},
	}
	st := &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}
	ntfy := NewNtfyClient("https://ntfy.sh", "test")

	d := newDaemon(cfg, st, ntfy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)
	d.Stop()
	// Second Stop should not deadlock.
	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("second Stop did not return within 2s (likely deadlocked)")
	}
}
