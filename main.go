package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var (
	flagConfig  = flag.String("config", defaultConfigPath(), "path to config TOML")
	flagState   = flag.String("state", defaultStatePath(), "path to state JSON")
	flagVerbose = flag.Bool("v", false, "verbose logging")
	flagGhPath  = flag.String("gh", "", "explicit path to gh binary (default: auto-discover)")
)

func defaultConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "checkdiff", "config.toml")
	}
	return "config.toml"
}

func defaultStatePath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "checkdiff", "state.json")
	}
	return "state.json"
}

func main() {
	// Go's log package defaults to stderr. Send everything to stdout
	// so the supervisor (launchd, systemd, or a terminal) can route
	// both streams to a single log destination.
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	flag.Parse()

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		// First-run experience: if the config file simply doesn't
		// exist, generate a default with a fresh token before giving
		// up. The user pastes the printed token into the web UI's
		// login form, and localStorage keeps them signed in for
		// subsequent visits.
		//
		// Use errors.Is (not os.IsNotExist) so the wrapped error from
		// loadConfig is recognized. os.IsNotExist returns false for
		// errors wrapped with fmt.Errorf("...: %w", err) — see the
		// os.IsNotExist doc comment, which explicitly redirects
		// wrapped-error callers to errors.Is(err, fs.ErrNotExist).
		if errors.Is(err, fs.ErrNotExist) {
			if genErr := ensureConfigForDaemon(*flagConfig); genErr != nil {
				log.Fatalf("generate config: %v", genErr)
			}
			cfg, err = loadConfig(*flagConfig)
		}
		if err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	st, err := loadState(*flagState)
	if err != nil {
		log.Fatalf("state: %v", err)
	}

	// Prune state entries for sources that no longer exist in the
	// config. Without this, deleting a source via the UI (or
	// editing the TOML) leaves a stale entry in state.json that
	// grows without bound. Called once at startup with the
	// current source IDs; daemon.Reload handles the runtime
	// case.
	validIDs := make(map[string]bool, len(cfg.Sources))
	for i := range cfg.Sources {
		validIDs[cfg.Sources[i].ID] = true
	}
	st.Prune(validIDs)

	if *flagVerbose {
		enabled := 0
		for i := range cfg.Sources {
			if cfg.Sources[i].IsEnabled() {
				enabled++
			}
		}
		log.Printf("loaded %d sources (%d enabled), topic=%s server=%s",
			len(cfg.Sources), enabled, cfg.Ntfy.Topic, cfg.Ntfy.Server)
	}

	runDaemon(cfg, st)
}

// runDaemon starts the long-running supervisor and blocks until
// SIGINT/SIGTERM. On signal it stops the supervisor, saves state,
// and exits.
//
// The daemon is the binary's only mode. The web UI rewrites the
// TOML config; the fsnotify watcher picks up the change and
// triggers a Reload. Per-source goroutines start, stop, and
// restart as the config evolves.
func runDaemon(cfg *Config, st *State) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Translate OS signals into context cancellation.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %s, shutting down", sig)
		cancel()
	}()

	ntfy := NewNtfyClient(cfg.Ntfy.Server, cfg.Ntfy.Topic)

	d := newDaemon(cfg, st, ntfy)
	d.Start(ctx)

	// Start the web server (HTTP API + UI). If [web] token is
	// empty, this is a no-op and the daemon runs headless.
	ws := newWebServer(cfg, d, st, *flagConfig, *flagState)
	if err := ws.Start(); err != nil {
		log.Printf("web server: %v", err)
	}

	// Watch the config file for changes. On debounced change,
	// re-parse the config and ask the daemon to reconcile its
	// per-source runners. The daemon preserves the in-memory
	// state (items_seen, items_hash) so editing a URL doesn't
	// flood the user with "new" notifications. The web server
	// also re-reads the new config so the token and listen
	// address changes take effect.
	cw := newConfigWatcher(*flagConfig, func() {
		newCfg, err := loadConfig(*flagConfig)
		if err != nil {
			log.Printf("config reload: %v", err)
			return
		}
		log.Printf("config reloaded: %d sources", len(newCfg.Sources))
		d.Reload(newCfg)
		ws.Reload(newCfg)
	})
	if err := cw.Start(ctx); err != nil {
		log.Printf("config watcher: %v", err)
	}

	if *flagVerbose {
		enabled := 0
		for _, s := range cfg.Sources {
			if s.IsEnabled() {
				enabled++
			}
		}
		log.Printf("daemon running: %d enabled sources", enabled)
	}

	// Block until the context is cancelled (by the signal handler).
	<-ctx.Done()

	ws.Stop()
	d.Stop()
	st.LastRun = time.Now().UTC()
	if err := saveState(*flagState, st); err != nil {
		log.Printf("save state: %v", err)
	}
}

// checkOne performs a single check on s and updates the in-memory
// state. It is called from the per-source goroutine after a
// successful fetchSource. The `now` and `nextRun` parameters are
// captured by the caller so the timestamp seen by the URL
// templater is the same one recorded in state.
//
// checkOne handles three cases:
//   - First run for this source: record baseline, no notification.
//   - Subsequent run with no diff: just remember the new items.
//   - Subsequent run with diff: publish to ntfy, then remember.
func checkOne(ctx context.Context, ntfy *NtfyClient, st *State, s *Source, items []Item, now, nextRun time.Time) error {
	srcState, exists := st.Sources[s.ID]

	// First run for this source: record the baseline and stay quiet.
	// We don't want a flood of "new" notifications for the 154 h3
	// entries that already exist on the changelog.
	if !exists {
		st.remember(s.ID, items, now, nextRun, 0, 0, "")
		if *flagVerbose {
			log.Printf("[%s] first run, baseline set (%d items), no notification", s.ID, len(items))
		}
		return nil
	}

	// Compute the diff:
	//   added   = items in the current set that weren't seen last run
	//   removed = IDs that were seen last run but aren't present now
	// For github_file sources, the "removed" entry is the old git blob
	// SHA — not meaningful to the user — so the formatter ignores it.
	currentSet := make(map[string]bool, len(items))
	var added []Item
	for _, it := range items {
		currentSet[it.ID] = true
		if !srcState.ItemsSeen[it.ID] {
			added = append(added, it)
		}
	}
	var removed []Item
	for id := range srcState.ItemsSeen {
		if !currentSet[id] {
			// The ID is the human-readable identifier for html/json
			// sources (entry text or model id). Use it as both ID and
			// Title so the notification can list it directly.
			removed = append(removed, Item{ID: id, Title: id})
		}
	}

	// Always remember the current set, even when nothing changes.
	defer st.remember(s.ID, items, now, nextRun, len(added), len(removed), "")

	if len(added) == 0 && len(removed) == 0 {
		if *flagVerbose {
			log.Printf("[%s] %d items, 0 added, 0 removed", s.ID, len(items))
		}
		return nil
	}

	if *flagVerbose {
		log.Printf("[%s] %d items, %d added, %d removed", s.ID, len(items), len(added), len(removed))
	}

	title, body, priority, tags := formatNotification(s, added, removed)
	headers := map[string]string{
		"Title":    title,
		"Priority": priority,
		"Tags":     tags,
		"Click":    clickURLFor(s, added),
	}
	if err := ntfy.Publish(ctx, body, headers); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if *flagVerbose {
		log.Printf("[%s] notified ntfy: %d added, %d removed", s.ID, len(added), len(removed))
	}
	return nil
}

// clickURLFor picks the URL ntfy should open when the user taps the
// notification. The first added item's Link wins (so e.g. a package
// tracking notification opens that package's detail page), otherwise
// the source's own Link (a static URL for sources where every entry
// points at the same page), otherwise the source's URL. Removed-only
// diffs and sources without any link configuration end up at s.URL.
func clickURLFor(s *Source, added []Item) string {
	if len(added) > 0 && added[0].Link != "" {
		return added[0].Link
	}
	if s.Link != "" {
		return s.Link
	}
	return s.URL
}
