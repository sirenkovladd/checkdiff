// Command checkdiff is a long-running daemon that polls a
// handful of URLs/files and pushes a notification to ntfy.sh
// whenever something has changed.
//
// The binary is just wiring: flag.Parse, load config, set up
// the daemon supervisor, start the web server (if a token is
// set), install the fsnotify hot-reload, and block on
// signals. All the actual logic lives in the per-package
// modules (source, state, schedule, notify, daemon, webapi).
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

	"checkdiff/config"
	"checkdiff/daemon"
	"checkdiff/notify"
	"checkdiff/source"
	"checkdiff/state"
	"checkdiff/webapi"
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
	// Go's log package defaults to stderr. Send everything to
	// stdout so the supervisor (launchd, systemd, or a
	// terminal) can route both streams to a single log
	// destination.
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	flag.Parse()

	cfg, err := config.Load(*flagConfig)
	if err != nil {
		// First-run experience: if the config file simply
		// doesn't exist, generate a default with a fresh
		// token before giving up. The user pastes the
		// printed token into the web UI's login form, and
		// localStorage keeps them signed in for subsequent
		// visits.
		//
		// Use errors.Is (not os.IsNotExist) so the wrapped
		// error from config.Load is recognized. os.IsNotExist
		// returns false for errors wrapped with
		// fmt.Errorf("...: %w", err) — see the os.IsNotExist
		// doc comment, which explicitly redirects wrapped-
		// error callers to errors.Is(err, fs.ErrNotExist).
		if errors.Is(err, fs.ErrNotExist) {
			if genErr := config.EnsureForDaemon(*flagConfig); genErr != nil {
				log.Fatalf("generate config: %v", genErr)
			}
			cfg, err = config.Load(*flagConfig)
		}
		if err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	st, err := state.Load(*flagState)
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	// Prune state entries for sources that no longer exist
	// in the config. Without this, deleting a source via the
	// UI (or editing the TOML) leaves a stale entry in
	// state.json that grows without bound. Called once at
	// startup with the current source IDs; daemon.Reload
	// handles the runtime case.
	validIDs := make(map[string]bool, len(cfg.Sources))
	for i := range cfg.Sources {
		validIDs[cfg.Sources[i].ID] = true
	}
	st.Prune(validIDs)

	// Configure the github_file fetcher with the explicit
	// gh binary path (if any) before any fetcher goroutine
	// starts. SetGhPath is safe to call before the first
	// fetch; the value is read by Fetch via GhPath.
	source.SetGhPath(*flagGhPath)

	// -v turns on per-fetch log lines across all fetchers
	// (URL, status, item count) and explicit notify
	// attempt/failure lines in check.One. Set once at
	// startup; the per-source goroutines read the package-
	// level flag.
	source.SetVerbose(*flagVerbose)

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

// runDaemon starts the long-running supervisor and blocks
// until SIGINT/SIGTERM. On signal it stops the supervisor,
// saves state, and exits.
//
// The daemon is the binary's only mode. The web UI rewrites
// the TOML config; the fsnotify watcher picks up the change
// and triggers a Reload. Per-source goroutines start, stop,
// and restart as the config evolves.
func runDaemon(cfg *config.Config, st *state.State) {
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

	ntfy := notify.New(cfg.Ntfy.Server, cfg.Ntfy.Topic)

	d := daemon.NewDaemon(cfg, st, ntfy, *flagVerbose)
	d.Start(ctx)

	// Start the web server (HTTP API + UI). If [web] token
	// is empty, this is a no-op and the daemon runs
	// headless.
	ws := webapi.NewServer(cfg, d, st, *flagConfig, *flagState)
	if err := ws.Start(); err != nil {
		log.Printf("web server: %v", err)
	}

	// Watch the config file for changes. On debounced change,
	// re-parse the config and ask the daemon to reconcile
	// its per-source runners. The daemon preserves the
	// in-memory state (items_seen, items_hash) so editing a
	// URL doesn't flood the user with "new" notifications.
	// The web server also re-reads the new config so the
	// token and listen address changes take effect.
	cw := config.NewWatcher(*flagConfig, func() {
		newCfg, err := config.Load(*flagConfig)
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

	// Block until the context is cancelled (by the signal
	// handler).
	<-ctx.Done()

	ws.Stop()
	d.Stop()
	st.SetLastRun(time.Now().UTC())
	if err := state.Save(*flagState, st); err != nil {
		log.Printf("save state: %v", err)
	}
}

// Keep these references so vet doesn't complain about
// unused imports in case a future refactor drops a caller.
var (
	_ = fmt.Sprintf
)
