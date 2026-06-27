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

func TestDaemonTriggerNowUnknown(t *testing.T) {
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{},
	}
	d := newDaemon(cfg, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}, NewNtfyClient("https://ntfy.sh", "test"))
	if err := d.TriggerNow("nope"); err == nil {
		t.Errorf("TriggerNow on unknown source: got nil error, want error")
	}
}

func TestDaemonTriggerNowKnown(t *testing.T) {
	// TriggerNow on a known source must return immediately (not
	// block on the check completing). The check itself runs in
	// the goroutine and will fail with a connection error since
	// 127.0.0.1:1 is unused — that's fine, the test is about
	// the non-blocking send to runNowCh.
	enabled := true
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "x", Name: "x", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	d := newDaemon(cfg, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}, NewNtfyClient("https://ntfy.sh", "test"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	done := make(chan error, 1)
	go func() { done <- d.TriggerNow("x") }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("TriggerNow on known source: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Errorf("TriggerNow did not return within 1s (likely blocked)")
	}
}

func TestDaemonTriggerNowIsIdempotent(t *testing.T) {
	// A second TriggerNow while the first is in flight must not
	// block: the runNowCh channel is buffered to size 1, so the
	// second send is silently dropped.
	enabled := true
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "x", Name: "x", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	d := newDaemon(cfg, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}, NewNtfyClient("https://ntfy.sh", "test"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// Two rapid triggers. Both must return immediately.
	done := make(chan struct{}, 2)
	go func() { _ = d.TriggerNow("x"); done <- struct{}{} }()
	go func() { _ = d.TriggerNow("x"); done <- struct{}{} }()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("TriggerNow #%d did not return within 1s (likely blocked)", i+1)
		}
	}
}

func TestDaemonReloadUpdatesNtfyClient(t *testing.T) {
	// Reload with a new config that has a different ntfy topic
	// should update the daemon's existing ntfy client in place,
	// so the next notification uses the new topic.
	//
	// st.path is left empty so the daemon's saveState calls are
	// no-ops — this test doesn't care about state persistence,
	// and avoiding the write avoids a race with t.TempDir cleanup.
	enabled := true
	cfg := &Config{
		Ntfy:  NtfyConfig{Server: "https://ntfy.sh", Topic: "old"},
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	st := &State{Version: currentStateVersion, Sources: map[string]*SourceState{}}
	ntfy := NewNtfyClient("https://ntfy.sh", "old")
	d := newDaemon(cfg, st, ntfy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	if d.ntfy.Topic != "old" {
		t.Fatalf("initial ntfy.Topic = %q, want old", d.ntfy.Topic)
	}

	newCfg := *cfg
	newCfg.Ntfy.Topic = "new"
	d.Reload(&newCfg)
	if d.ntfy.Topic != "new" {
		t.Errorf("after Reload: ntfy.Topic = %q, want new", d.ntfy.Topic)
	}
}
