package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// daemon is the long-running supervisor that manages one goroutine
// per enabled source. Each goroutine runs checkOne on its own
// schedule, computed by the source's intervalFn (which handles
// both fixed Go durations and cron expressions uniformly).
//
// The supervisor owns the goroutines and their cancellation, and
// nothing more. Config hot-reload is layered on top via Reload;
// the HTTP server is a separate concern.
type daemon struct {
	cfg  *Config
	st   *State
	ntfy *NtfyClient

	// parentCtx is the context passed to Start. It's stored so
	// that Reload (triggered by the config watcher) can derive
	// new goroutine contexts from the same parent, ensuring
	// they all share the same cancellation signal.
	parentCtx context.Context

	mu      sync.Mutex
	runners map[string]*sourceRunner
}

// sourceRunner is one source's per-goroutine state.
type sourceRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
	// runNowCh is a buffered channel (size 1) that the web API
	// handler sends to in order to trigger an immediate check
	// for this source, bypassing the schedule. The non-blocking
	// send means a second "run now" while a check is in flight
	// is silently dropped (the running check already covers it).
	runNowCh chan struct{}
	// next computes the next run time after a given timestamp.
	// Both Go-duration sources and cron-expression sources use
	// the same interface so the run loop doesn't need to know
	// which format the source uses.
	next intervalFn
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
// disabled (or that don't parse) are skipped. Start is idempotent:
// calling it twice cancels the first set of runners before
// starting the second.
func (d *daemon) Start(ctx context.Context) {
	d.mu.Lock()
	d.parentCtx = ctx
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
		next, err := parseInterval(s.ResolvedInterval(d.cfg.Check.Interval))
		if err != nil {
			log.Printf("[%s] invalid interval %q, skipping: %v", s.ID, s.ResolvedInterval(d.cfg.Check.Interval), err)
			continue
		}
		d.startOne(ctx, s, next)
	}
}

// Reload swaps in a new Config and reconciles the per-source
// runners: new sources get a fresh runner, removed sources are
// cancelled, and changed sources (any field that affects the
// runner — enabled, interval, URL, type) are cancelled and
// restarted. The state map is preserved so the user doesn't
// see a flood of "new" notifications after editing the config.
//
// Reload uses the parent context stored by the most recent
// Start call, so the new goroutines share the same cancellation
// signal as the original ones. If Start has never been called,
// Reload is a no-op.
func (d *daemon) Reload(newCfg *Config) {
	d.mu.Lock()
	parent := d.parentCtx
	d.cfg = newCfg
	// Pick up the latest ntfy settings so changes via PUT
	// /api/settings (or by editing the TOML and waiting for
	// the fsnotify watcher to fire) take effect without a
	// daemon restart.
	if d.ntfy != nil {
		// TrimRight matches NewNtfyClient so changing the
		// server URL via the UI doesn't leave a stray
		// trailing slash that produces "https://ntfy.sh//topic"
		// at publish time.
		d.ntfy.Server = strings.TrimRight(newCfg.Ntfy.Server, "/")
		d.ntfy.Topic = newCfg.Ntfy.Topic
	}
	d.mu.Unlock()
	if parent == nil {
		return
	}
	// Drop state entries for sources that no longer exist. Keeps
	// state.json from accumulating orphans across reloads.
	validIDs := make(map[string]bool, len(newCfg.Sources))
	for i := range newCfg.Sources {
		validIDs[newCfg.Sources[i].ID] = true
	}
	d.st.Prune(validIDs)
	d.Start(parent)
}

// startOne spawns a single source's goroutine.
func (d *daemon) startOne(parent context.Context, s *Source, next intervalFn) {
	ctx, cancel := context.WithCancel(parent)
	r := &sourceRunner{
		cancel:   cancel,
		done:     make(chan struct{}),
		runNowCh: make(chan struct{}, 1),
		next:     next,
	}
	d.mu.Lock()
	// If a runner for this source already exists, cancel it first.
	if existing, ok := d.runners[s.ID]; ok {
		existing.cancel()
	}
	d.runners[s.ID] = r
	d.mu.Unlock()

	go d.runSource(ctx, s, r, next)
}

// runSource is the per-source loop. It uses a time.Timer (not a
// time.Ticker) so cron expressions with variable intervals are
// handled correctly: each iteration computes the next run time
// from the current time.
func (d *daemon) runSource(ctx context.Context, s *Source, r *sourceRunner, next intervalFn) {
	defer close(r.done)
	// Run immediately on start so a freshly-restarted daemon
	// doesn't wait a full interval before its first check.
	d.runOnce(ctx, s, r, next)
	for {
		now := time.Now()
		nextRun := next(now)
		wait := time.Until(nextRun)
		if wait < 0 {
			// We're already past the next scheduled time (e.g.
			// the system was suspended). Fire immediately rather
			// than burning CPU in a tight loop.
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			d.runOnce(ctx, s, r, next)
		case <-r.runNowCh:
			// Non-blocking trigger from the web API. The channel
			// is buffered (size 1) so a second "run now" while
			// this one is in flight is dropped silently.
			timer.Stop()
			d.runOnce(ctx, s, r, next)
		}
	}
}

// runOnce performs a single check, capturing the timestamp at the
// start of the run and the next scheduled time at the end (so
// the state file records a meaningful NextRun even for cron
// sources whose next interval is variable).
func (d *daemon) runOnce(ctx context.Context, s *Source, r *sourceRunner, next intervalFn) {
	now := time.Now()
	items, err := fetchSource(ctx, s, now)
	if err != nil {
		log.Printf("[%s] check failed: %v", s.ID, err)
		// Record the error in state so the web UI can surface
		// it, but don't update items_seen (the source didn't
		// successfully fetch).
		d.st.recordError(s.ID, now, next(now), err)
		d.saveState()
		return
	}
	if err := checkOne(ctx, d.ntfy, d.st, s, items, now, next(now)); err != nil {
		log.Printf("[%s] check failed: %v", s.ID, err)
	}
	d.saveState()
}

// saveState writes the in-memory state to disk. Errors are
// logged but not returned — the next successful check will
// produce another save attempt, and we don't want a transient
// disk error to crash the daemon.
func (d *daemon) saveState() {
	if err := saveState(d.st.path, d.st); err != nil {
		log.Printf("save state: %v", err)
	}
}

// TriggerNow signals the given source's goroutine to run an
// immediate check. Returns an error if the source is not known
// to the daemon (e.g. disabled or never started).
func (d *daemon) TriggerNow(id string) error {
	d.mu.Lock()
	r, ok := d.runners[id]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("source %q is not running", id)
	}
	select {
	case r.runNowCh <- struct{}{}:
		return nil
	default:
		// Channel is full: a "run now" is already pending or a
		// check is in flight. Treat as success — the user got
		// their wish, just via the previous trigger.
		return nil
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
