package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk config file. TOML is the only supported
// format. To disable a source, comment out its [[sources]] block —
// the TOML parser drops the lines and the run loop never sees the
// entry.
type Config struct {
	Ntfy    NtfyConfig  `toml:"ntfy"`
	Check   CheckConfig `toml:"check"`
	Web     WebConfig   `toml:"web"`
	Sources []Source    `toml:"sources"`
}

type NtfyConfig struct {
	// Server is the ntfy base URL. Defaults to https://ntfy.sh.
	Server string `toml:"server"`
	// Topic is the ntfy topic. Required.
	Topic string `toml:"topic"`
}

// CheckConfig holds runtime options that aren't tied to a specific
// source. Today there's only one knob: the polling interval. On
// Linux this drives the systemd timer's OnCalendar (see timer.go);
// on macOS it drives the launchd plist's StartInterval (see the
// Makefile). Defaults to 1h to preserve historical behavior.
type CheckConfig struct {
	// Interval is a Go duration string: "1h", "30m", "10m", "15s",
	// "2h30m", etc. Must be >= 1 minute and must evenly divide an
	// hour (for minute-level) or a day (for hour-level) — see
	// onCalendarFor in timer.go for the full set of accepted values.
	Interval string `toml:"check_interval"`
}

// WebConfig holds the optional web UI settings. When Token is
// empty the web server does not start (the daemon still runs
// sources; only the HTTP surface is disabled). When Token is
// non-empty, the HTTP server binds to Listen and requires the
// token on every request.
type WebConfig struct {
	// Listen is the bind address for the HTTP server, e.g.
	// "127.0.0.1:8080" (localhost only) or ":8080" (all
	// interfaces). Default: "127.0.0.1:8080".
	Listen string `toml:"listen"`
	// Token is the shared secret required to access the web UI
	// and JSON API. Empty means the web server is disabled.
	Token string `toml:"token,omitempty"`
}

// Source describes one thing to monitor. The Type field selects which
// subset of the remaining fields is meaningful:
//
//	"github_file": Owner, Repo, Ref, Path are required.
//	"html":        Selector selects elements from the page at URL.
//	               Supported selectors: a tag name ("h1".."h4", "title",
//	               "li", etc.) or "tag.class" / "tag.class1.class2" for
//	               elements with the given class(es), e.g. "li.attachedfile".
//	               Items are tracked by their text content, so additions
//	               and removals of individual entries are detected.
//	"json":        URL returns a JSON document. ItemsPath locates the
//	               array of items (dot-separated keys, default "data"),
//	               IDField is the field used as the stable item ID
//	               (default "id"), and TitleField is the field used as
//	               the display name (default "name"). Optional
//	               LinkField names a JSON field whose string value is
//	               attached to each item as its Link — the
//	               notification's Click header then opens that URL
//	               instead of the source's URL, and the item is
//	               rendered as a markdown link in the body. Items are
//	               tracked by ID, so additions and removals are
//	               detected.
//
// To pause a source temporarily, comment out its [[sources]] block
// in the TOML file. The decoder will skip the entry entirely.
type Source struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
	Type string `toml:"type"`
	URL  string `toml:"url,omitempty"`

	// GitHub-specific
	Owner string `toml:"owner,omitempty"`
	Repo  string `toml:"repo,omitempty"`
	Ref   string `toml:"ref,omitempty"`
	Path  string `toml:"path,omitempty"`

	// HTML-specific
	Selector string `toml:"selector,omitempty"`

	// JSON-specific
	ItemsPath  string `toml:"items_path,omitempty"`
	IDField    string `toml:"id_field,omitempty"`
	TitleField string `toml:"title_field,omitempty"`
	// LinkField is the JSON field whose string value is attached to
	// each item as its Link. When set, items carry their own URL —
	// the notification's ntfy Click header uses the first added
	// item's Link (falling back to s.URL) and that item is rendered
	// as a markdown link in the body. Useful for sources like
	// package tracking where each entry has its own detail page.
	LinkField string `toml:"link_field,omitempty"`
	// Link is a static URL attached to the source as a whole. The
	// notification's Click header uses it (after per-item Link, before
	// the bare URL) so the user is taken to a fixed destination —
	// e.g. a package tracking page for a single-package source.
	Link string `toml:"link,omitempty"`

	// CheckInterval overrides [check].check_interval for this source.
	// Accepts a Go duration string ("1h", "30m", "10m", "15s", "2h30m").
	// If empty, the source uses the global [check].check_interval.
	// Today this is loaded and validated but not yet used for
	// scheduling — the daemon-mode scheduler is the next step.
	CheckInterval string `toml:"check_interval,omitempty"`

	// Enabled controls whether the source is active. The pointer
	// is used so that a missing field in the TOML (the common case
	// for existing configs) is distinct from an explicit false: a
	// nil pointer means "no preference, default to enabled", which
	// preserves backward compatibility with sources written before
	// this field existed. The IsEnabled method hides the pointer
	// from the rest of the codebase.
	Enabled *bool `toml:"enabled,omitempty"`
}

// IsEnabled reports whether the source should be active. A nil
// Enabled pointer is treated as true (the historical default) so
// existing configs without the field keep working.
func (s *Source) IsEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// SetEnabled is a convenience for code paths (web API, tests) that
// want to flip the enabled flag without dealing with the pointer.
func (s *Source) SetEnabled(v bool) {
	s.Enabled = &v
}

// ResolvedInterval returns the effective polling interval for this
// source: the per-source CheckInterval if set, otherwise the
// supplied global default. The return value is a Go duration
// string ("1h", "30m", "10m", etc.) — callers are expected to
// parse it with time.ParseDuration.
//
// Whitespace-only per-source values are treated as empty so that
// the global default is used. This is defensive — the loader
// rejects whitespace-only values as invalid durations, but the
// helper stays self-consistent regardless.
//
// This is a pure helper; the daemon-mode scheduler (the next
// implementation step) will call it when picking a per-source
// ticker interval.
func (s *Source) ResolvedInterval(globalDefault string) string {
	if v := strings.TrimSpace(s.CheckInterval); v != "" {
		return v
	}
	return globalDefault
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Ntfy.Topic == "" {
		return nil, fmt.Errorf("config: ntfy.topic is required")
	}
	if c.Ntfy.Server == "" {
		c.Ntfy.Server = "https://ntfy.sh"
	}
	if c.Check.Interval == "" {
		c.Check.Interval = "1h"
	}
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8080"
	}
	if _, err := time.ParseDuration(c.Check.Interval); err != nil {
		return nil, fmt.Errorf("config: check.check_interval %q: %w", c.Check.Interval, err)
	}
	seen := make(map[string]bool, len(c.Sources))
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.ID == "" {
			return nil, fmt.Errorf("config: source[%d]: id is required", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("config: duplicate source id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Name == "" {
			s.Name = s.ID
		}
		if s.CheckInterval != "" {
			// Per-source interval only needs to be a valid Go duration.
			// The systemd OnCalendar constraints from onCalendarFor
			// don't apply here — the daemon uses an internal ticker
			// and accepts any duration >= 1 minute.
			d, err := time.ParseDuration(s.CheckInterval)
			if err != nil {
				return nil, fmt.Errorf("config: source %q: check_interval %q: %w", s.ID, s.CheckInterval, err)
			}
			if d < time.Minute {
				return nil, fmt.Errorf("config: source %q: check_interval must be >= 1 minute (got %s)", s.ID, d)
			}
		}
		if err := validateSource(s); err != nil {
			return nil, fmt.Errorf("config: source %q: %w", s.ID, err)
		}
	}
	return &c, nil
}

func validateSource(s *Source) error {
	switch s.Type {
	case "github_file":
		if s.Owner == "" || s.Repo == "" || s.Path == "" {
			return fmt.Errorf("github_file requires owner, repo, path (type=%q)", s.Type)
		}
		if s.Ref == "" {
			s.Ref = "HEAD"
		}
		if s.URL == "" {
			s.URL = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", s.Owner, s.Repo, s.Ref, s.Path)
		}
	case "html":
		if s.URL == "" {
			return fmt.Errorf("html requires url")
		}
		if s.Selector == "" {
			s.Selector = "h3"
		}
		// parseHTMLSelector accepts "tag" and "tag.class[.class...]"
		// forms. The tag-name-only forms (h1..h4, title) keep working
		// as before; "li.attachedfile" (and similar) is now valid too.
		if _, err := parseHTMLSelector(s.Selector); err != nil {
			return fmt.Errorf("html: unsupported selector %q (use a tag name like h3, or 'tag.class' such as li.attachedfile)", s.Selector)
		}
	case "json":
		if s.URL == "" {
			return fmt.Errorf("json requires url")
		}
		// Sensible defaults that match the most common API shape
		// ({"data": [{"id": "...", "name": "..."}]}). Override in
		// the config for APIs that use a different layout.
		if s.ItemsPath == "" {
			s.ItemsPath = "data"
		}
		if s.IDField == "" {
			s.IDField = "id"
		}
		if s.TitleField == "" {
			s.TitleField = "name"
		}
	default:
		return fmt.Errorf("unknown type %q (want github_file|html|json)", s.Type)
	}
	return nil
}
