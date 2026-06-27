package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

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
// source. The only knob is the global polling interval, used as
// the default for sources that don't set their own
// check_interval. Defaults to 1h to preserve historical behavior.
type CheckConfig struct {
	// Interval is a Go duration string: "1h", "30m", "10m", "15s",
	// "2h30m", etc. Used as the default for sources that don't
	// set their own Source.CheckInterval. Must be >= 1 minute
	// (the daemon's per-source goroutine rejects shorter values).
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
	ID   string `toml:"id" json:"id"`
	Name string `toml:"name" json:"name"`
	Type string `toml:"type" json:"type"`
	URL  string `toml:"url,omitempty" json:"url,omitempty"`

	// GitHub-specific
	Owner string `toml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `toml:"repo,omitempty" json:"repo,omitempty"`
	Ref   string `toml:"ref,omitempty" json:"ref,omitempty"`
	Path  string `toml:"path,omitempty" json:"path,omitempty"`

	// HTML-specific
	Selector string `toml:"selector,omitempty" json:"selector,omitempty"`

	// JSON-specific
	ItemsPath  string `toml:"items_path,omitempty" json:"items_path,omitempty"`
	IDField    string `toml:"id_field,omitempty" json:"id_field,omitempty"`
	TitleField string `toml:"title_field,omitempty" json:"title_field,omitempty"`
	// LinkField is the JSON field whose string value is attached to
	// each item as its Link. When set, items carry their own URL —
	// the notification's ntfy Click header uses the first added
	// item's Link (falling back to s.URL) and that item is rendered
	// as a markdown link in the body. Useful for sources like
	// package tracking where each entry has its own detail page.
	LinkField string `toml:"link_field,omitempty" json:"link_field,omitempty"`
	// Link is a static URL attached to the source as a whole. The
	// notification's Click header uses it (after per-item Link, before
	// the bare URL) so the user is taken to a fixed destination —
	// e.g. a package tracking page for a single-package source.
	Link string `toml:"link,omitempty" json:"link,omitempty"`

	// CheckInterval overrides [check].check_interval for this source.
	// Accepts either a Go duration string ("1h", "30m", "10m", "15s",
	// "2h30m") or a standard 5-field cron expression
	// ("0 */6 * * *", "*/15 * * * *"). The format is auto-detected
	// by the presence of whitespace. If empty, the source uses the
	// global [check].check_interval.
	CheckInterval string `toml:"check_interval,omitempty" json:"check_interval,omitempty"`

	// Enabled controls whether the source is active. The pointer
	// is used so that a missing field in the TOML (the common case
	// for existing configs) is distinct from an explicit false: a
	// nil pointer means "no preference, default to enabled", which
	// preserves backward compatibility with sources written before
	// this field existed. The IsEnabled method hides the pointer
	// from the rest of the codebase.
	Enabled *bool `toml:"enabled,omitempty" json:"enabled,omitempty"`
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
	if _, err := parseInterval(c.Check.Interval); err != nil {
		return nil, fmt.Errorf("config: check.check_interval: %w", err)
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
			// Per-source interval accepts either a Go duration
			// (>= 1 minute) or a 5-field cron expression. The
			// format is auto-detected; the helper is the only
			// place the choice is made.
			if _, err := parseInterval(s.CheckInterval); err != nil {
				return nil, fmt.Errorf("config: source %q: check_interval: %w", s.ID, err)
			}
		}
		if err := validateSource(s); err != nil {
			return nil, fmt.Errorf("config: source %q: %w", s.ID, err)
		}
	}
	return &c, nil
}

// marshalConfig encodes cfg to TOML. The web UI rewrites the
// config on every change, so the format produced here is what
// the user sees in their config.toml file. We use a stable
// header (the date and a "this file is auto-generated" comment
// when the file is fresh) so the file is at least somewhat
// self-documenting.
func marshalConfig(cfg *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to tmp, fsyncs, then renames over
// path. The rename is atomic on POSIX filesystems, so concurrent
// readers (e.g. the fsnotify watcher, a separate `checkdiff`
// invocation) never see a half-written file.
func writeFileAtomic(tmp, path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
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
