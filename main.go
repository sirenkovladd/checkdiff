package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	flagConfig     = flag.String("config", defaultConfigPath(), "path to config TOML")
	flagState      = flag.String("state", defaultStatePath(), "path to state JSON")
	flagDryRun     = flag.Bool("dry-run", false, "fetch and diff but don't send ntfy messages")
	flagVerbose    = flag.Bool("v", false, "verbose logging")
	flagInit       = flag.Bool("init", false, "write a default config and exit")
	flagGhPath     = flag.String("gh", "", "explicit path to gh binary (default: auto-discover)")
	flagTestNotify = flag.Bool("test-notify", false, "send a single 'test' notification (to verify ntfy wiring) and exit")
	flagPrintTimer = flag.Bool("print-timer", false, "print a systemd user timer unit (driven by [check].check_interval) to stdout and exit")
	flagDaemon     = flag.Bool("daemon", false, "run as a long-lived daemon with per-source goroutines (one-shot is the default)")
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
	// so the launchd job can route both streams to a single log file
	// without losing the verbose (-v) output.
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	flag.Parse()
	if *flagInit {
		if err := writeDefaultConfig(*flagConfig); err != nil {
			log.Fatalf("init: %v", err)
		}
		fmt.Printf("wrote default config to %s\n", *flagConfig)
		return
	}

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		// If we're entering daemon mode and the config simply
		// doesn't exist yet, generate a default with a fresh
		// token. This is the first-run experience: the user
		// runs `checkdiff -daemon` and gets a working setup
		// with a token they can paste into the web UI.
		if *flagDaemon && os.IsNotExist(err) {
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

	if *flagPrintTimer {
		interval, err := time.ParseDuration(cfg.Check.Interval)
		if err != nil {
			log.Fatalf("print-timer: parse check.check_interval %q: %v", cfg.Check.Interval, err)
		}
		onCal, err := onCalendarFor(interval)
		if err != nil {
			log.Fatalf("print-timer: %v", err)
		}
		fmt.Printf(systemdTimerTemplate, cfg.Check.Interval, onCal)
		return
	}

	if *flagTestNotify {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ntfy := NewNtfyClient(cfg.Ntfy.Server, cfg.Ntfy.Topic)
		err := ntfy.Publish(ctx, "checkdiff is wired up correctly. You'll get a real notification here the next time one of your watched sources changes.",
			map[string]string{
				"Title":    "✅ checkdiff test",
				"Priority": "low",
				"Tags":     "test_tube,white_check_mark",
			})
		if err != nil {
			log.Fatalf("test-notify: %v", err)
		}
		fmt.Println("test notification sent")
		return
	}

	if *flagVerbose {
		ids := make([]string, len(cfg.Sources))
		for i, s := range cfg.Sources {
			ids[i] = s.ID
		}
		log.Printf("loaded %d sources [%s], topic=%s server=%s",
			len(cfg.Sources), strings.Join(ids, ", "), cfg.Ntfy.Topic, cfg.Ntfy.Server)
	}

	ntfy := NewNtfyClient(cfg.Ntfy.Server, cfg.Ntfy.Topic)

	// Daemon mode: one goroutine per enabled source, blocks on
	// SIGINT/SIGTERM. The one-shot path below is the default and
	// is used for testing, dry-runs, and CI invocations.
	if *flagDaemon {
		runDaemon(cfg, st, ntfy)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	anyError := false
	for i := range cfg.Sources {
		s := &cfg.Sources[i]
		interval, err := time.ParseDuration(s.ResolvedInterval(cfg.Check.Interval))
		if err != nil {
			log.Printf("[%s] error: invalid interval %q: %v", s.ID, s.ResolvedInterval(cfg.Check.Interval), err)
			anyError = true
			continue
		}
		if err := checkOne(ctx, ntfy, st, s, interval); err != nil {
			log.Printf("[%s] error: %v", s.ID, err)
			anyError = true
		}
	}

	st.LastRun = time.Now().UTC()
	if err := saveState(*flagState, st); err != nil {
		log.Printf("save state: %v", err)
		anyError = true
	}

	if anyError {
		os.Exit(1)
	}
}

// runDaemon starts the long-running supervisor and blocks until
// SIGINT/SIGTERM. On signal it stops the supervisor, saves state,
// and exits.
func runDaemon(cfg *Config, st *State, ntfy *NtfyClient) {
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

	d := newDaemon(cfg, st, ntfy)
	d.Start(ctx)

	// Start the web server (HTTP API + UI). If [web] token is
	// empty, this is a no-op and the daemon runs headless.
	ws := newWebServer(cfg, d, st)
	if err := ws.Start(); err != nil {
		log.Printf("web server: %v", err)
	}

	// Watch the config file for changes. On debounced change,
	// re-parse the config and ask the daemon to reconcile its
	// per-source runners. The daemon preserves the in-memory
	// state (items_seen, items_hash) so editing a URL doesn't
	// flood the user with "new" notifications.
	cw := newConfigWatcher(*flagConfig, func() {
		newCfg, err := loadConfig(*flagConfig)
		if err != nil {
			log.Printf("config reload: %v", err)
			return
		}
		log.Printf("config reloaded: %d sources", len(newCfg.Sources))
		d.Reload(newCfg)
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

	// Block until the context is cancelled (by the signal handler
	// or by a future config-reload path that wants to restart us).
	<-ctx.Done()

	ws.Stop()
	d.Stop()
	st.LastRun = time.Now().UTC()
	if err := saveState(*flagState, st); err != nil {
		log.Printf("save state: %v", err)
	}
}

func checkOne(ctx context.Context, ntfy *NtfyClient, st *State, s *Source, interval time.Duration) error {
	now := time.Now().UTC()
	items, err := fetchSource(ctx, s, now)
	if err != nil {
		// Surface the error to ntfy as a separate notification so the
		// user knows the check ran but failed. Don't update state.
		if !*flagDryRun {
			_ = ntfy.Publish(ctx,
				fmt.Sprintf("check failed: %v", err),
				map[string]string{
					"Title":    fmt.Sprintf("⚠️ %s: check failed", s.Name),
					"Priority": "high",
					"Tags":     "warning,checkdiff",
					"Click":    s.URL,
				})
		}
		return err
	}

	srcState, exists := st.Sources[s.ID]

	// First run for this source: record the baseline and stay quiet.
	// We don't want a flood of "new" notifications for the 154 h3
	// entries that already exist on the changelog.
	if !exists {
		now := time.Now().UTC()
		st.remember(s.ID, items, now, interval, 0, 0, "")
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
	defer st.remember(s.ID, items, now, interval, len(added), len(removed), "")

	if len(added) == 0 && len(removed) == 0 {
		if *flagVerbose {
			log.Printf("[%s] %d items, 0 added, 0 removed", s.ID, len(items))
		}
		return nil
	}

	if *flagVerbose {
		log.Printf("[%s] %d items, %d added, %d removed", s.ID, len(items), len(added), len(removed))
	}

	if *flagDryRun {
		return nil
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

// writeDefaultConfig drops a starter config next to the configured
// config path so the user can `checkdiff -init` once and edit.
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigTOML), 0o644)
}

// defaultConfigTOML is the starter config written by `-init`. One of
// the sources is left commented out as a worked example of how to
// pause a source without losing the entry.
//
// Standard `[ntfy]` / `[[sources]]` style is used so the file looks
// like idiomatic TOML. To disable a source, comment out its entire
// `[[sources]]` block (the header line plus every key = value line
// beneath it, up to the next blank line or header). The TOML parser
// drops the lines and the source is skipped at run time.
const defaultConfigTOML = `# checkdiff config — https://github.com/...
#
# To temporarily disable a source, comment out its [[sources]] block
# entirely (the '[[sources]]' line plus every 'key = value' line
# beneath it, up to the next blank line or header). The TOML parser
# drops the lines, the source is skipped at run time, and the baseline
# in state.json is preserved so uncommenting it later won't fire a
# flood of "new" notifications.

[ntfy]
server = "https://ntfy.sh"
topic  = "REPLACE_ME"

[check]
# How often the timer fires. Accepts any Go duration string ("1h",
# "30m", "10m", "15s", "2h30m", etc.). Must be >= 1 minute and must
# evenly divide an hour (minute-level) or 24h (hour-level) or 30d
# (day-level). On Linux, drives the systemd timer's OnCalendar (see
# 'checkdiff -print-timer'). On macOS, drives the launchd plist's
# StartInterval (see the Makefile). Default: 1h.
check_interval = "1h"

# [[sources]]
# id   = "opencode-go-docs"
# name = "opencode go.mdx"
# type = "github_file"
# owner = "anomalyco"
# repo  = "opencode"
# ref  = "dev"
# path = "packages/web/src/content/docs/go.mdx"

# JSON source with a per-item URL. Set link_field to the JSON field
# whose string value is the item's link; the notification's Click
# header opens that URL (falling back to the source's url) and the
# item is rendered as a markdown link in the body. Useful for
# package tracking, ticket systems, etc., where each entry has its
# own detail page.
# [[sources]]
# id   = "uniuni-package"
# name = "uniuni package"
# type = "json"
# url  = "https://api.uniuni.example/track"
# items_path = "data.packages"
# id_field   = "tno"
# title_field = "tno"
# link_field = "tracking_url"

# JSON source with a single fixed destination. Set 'link' to a
# static URL and the notification's Click header opens it instead
# of the source's url. Useful for sources where every entry is a
# status change for the same underlying thing (e.g. scan events
# for a single package) and you want the Click to go to a single
# detail page.
# [[sources]]
# id   = "uniuni-package"
# name = "uniuni package"
# type = "json"
# url  = "https://delivery-api.uniuni.ca/track?id=..."
# items_path = "data.valid_tno[0].spath_list"
# id_field   = "id"
# title_field = "code"
# link = "https://www.uniuni.com/tracking/#tracking-detail?no=U000180542908940"

[[sources]]
id   = "opencode-go-route"
name = "opencode go route"
type = "github_file"
owner = "anomalyco"
repo  = "opencode"
ref  = "dev"
path = "packages/console/app/src/routes/go/index.tsx"

[[sources]]
id       = "artificial-analysis-changelog"
name     = "Artificial Analysis Changelog"
type     = "html"
url      = "https://artificialanalysis.ai/changelog"
selector = "h3"

# OpenRouter model list. The site is client-rendered, so we hit the
# public JSON API directly. Newest models appear first. The defaults
# (items_path="data", id_field="id", title_field="name") match the
# API response shape — no extra fields needed.
[[sources]]
id   = "openrouter-models"
name = "OpenRouter Models"
type = "json"
url  = "https://openrouter.ai/api/v1/models"

[[sources]]
id       = "tfc-volleyball-grass"
name     = "TFC Volleyball — 2026 Grass Leagues"
type     = "html"
url      = "https://tfcvolleyball.com/"
selector = "li.attachedfile"
`
