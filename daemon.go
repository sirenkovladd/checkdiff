package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// daemon is the long-running supervisor that manages one goroutine
// per enabled source. Each goroutine runs checkOne on a ticker
// using the source's per-source interval (falling back to the
// global [check].check_interval).
//
// The supervisor is intentionally minimal: it owns the goroutines
// and their cancellation, and nothing more. Config hot-reload and
// the HTTP server are separate concerns that will be layered on
// top in later steps.
type daemon struct {
	cfg *Config
	st  *State
	ntfy *NtfyClient

	mu      sync.Mutex
	runners map[string]*sourceRunner
}

// sourceRunner is one source's per-goroutine state.
type sourceRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
	// interval is cached at start time so a config reload that
	// changes the interval only takes effect after the runner
	// is restarted (cancel + start).
	interval time.Duration
}

func newDaemon(cfg *Config, st *State, ntfy *NtfyClient) *daemon {
	return &daemon{
		cfg:     cfg,
		st:      st,
		ntfy:    ntfy,
		runners: make(map[string]*sourceRunner),
	}
}

// Start launches one goroutine per enabled source. Sources that are
// disabled (or that don't validate) are skipped. Start is
// idempotent: calling it twice cancels the first set of runners
// before starting the second.
func (d *daemon) Start(ctx context.Context) {
	d.mu.Lock()
	for id, r := range d.runners {
		r.cancel()
		delete(d.runners, id)
	}
	d.mu.Unlock()

	for i := range d.cfg.Sources {
		s := &d.cfg.Sources[i]
		if !s.IsEnabled() {
			continue
		}
		interval, err := time.ParseDuration(s.ResolvedInterval(d.cfg.Check.Interval))
		if err != nil {
			log.Printf("[%s] invalid interval %q, skipping: %v", s.ID, s.ResolvedInterval(d.cfg.Check.Interval), err)
			continue
		}
		d.startOne(ctx, s, interval)
	}
}

// startOne spawns a single source's goroutine.
func (d *daemon) startOne(parent context.Context, s *Source, interval time.Duration) {
	ctx, cancel := context.WithCancel(parent)
	r := &sourceRunner{
		cancel:   cancel,
		done:     make(chan struct{}),
		interval: interval,
	}
	d.mu.Lock()
	// If a runner for this source already exists, cancel it first.
	if existing, ok := d.runners[s.ID]; ok {
		existing.cancel()
	}
	d.runners[s.ID] = r
	d.mu.Unlock()

	go d.runSource(ctx, s, r, interval)
}

func (d *daemon) runSource(ctx context.Context, s *Source, r *sourceRunner, interval time.Duration) {
	defer close(r.done)
	// Run immediately on start so a freshly-restarted daemon
	// doesn't wait a full interval before its first check.
	if err := checkOne(ctx, d.ntfy, d.st, s, interval); err != nil {
		log.Printf("[%s] check failed: %v", s.ID, err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := checkOne(ctx, d.ntfy, d.st, s, interval); err != nil {
				log.Printf("[%s] check failed: %v", s.ID, err)
			}
		}
	}
}

// Stop cancels all runners and waits for their goroutines to exit.
func (d *daemon) Stop() {
	d.mu.Lock()
	for _, r := range d.runners {
		r.cancel()
	}
	d.mu.Unlock()
	for _, r := range d.runners {
		<-r.done
	}
	d.mu.Lock()
	d.runners = make(map[string]*sourceRunner)
	d.mu.Unlock()
}
