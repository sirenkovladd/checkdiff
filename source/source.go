// Package source is the domain root for the things checkdiff
// monitors. A Source is one configured entry in config.toml; each
// type of source (github_file, html, json) is implemented by a
// Fetcher registered in this package.
//
// The Fetcher interface bundles three responsibilities — Fetch
// (turn a Source into []Item), Validate (apply defaults to a
// freshly-decoded Source), and Format (turn a diff into a
// notification). The bundle keeps "what it means to be a Source"
// in one place: adding a new source type is one file
// (implementing the interface) and one line in the registry.
//
// Notification is a value type decoupled from the ntfy wire
// format. The notify package consumes a Notification, so the
// per-type format knowledge (e.g. github_file's "file changed"
// layout) lives with the fetcher that knows the source, and the
// HTTP/publishing concern lives in notify/.
package source

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Item is one change-worthy entry from a Source. The state file
// stores the IDs of all items last seen for a given source; any
// item whose ID is not in that set is treated as new.
type Item struct {
	ID    string `json:"id"`              // stable per item, e.g. git blob SHA, changelog-entry text
	Title string `json:"title,omitempty"` // short, used as notification line
	Body  string `json:"body,omitempty"`  // optional longer snippet, capped
	// Link is an optional URL associated with the item. When set
	// on an added item, the notification's ntfy Click header uses
	// it (instead of the source's URL) and the item is rendered
	// as a markdown link in the body so the entry itself is
	// tappable.
	Link string `json:"link,omitempty"`
}

// Source describes one thing to monitor. The Type field selects
// which subset of the remaining fields is meaningful; each
// registered Fetcher knows which fields it owns and applies its
// own defaults in Validate.
//
// To pause a source temporarily, comment out its [[sources]]
// block in the TOML file. The decoder will skip the entry
// entirely.
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
	// LinkField names a JSON field whose string value is attached
	// to each item as its Link — the notification's Click header
	// then opens that URL instead of the source's URL, and the
	// item is rendered as a markdown link in the body.
	LinkField string `toml:"link_field,omitempty" json:"link_field,omitempty"`
	// Link is a static URL attached to the source as a whole.
	// The notification's Click header uses it (after per-item
	// Link, before the bare URL) so the user is taken to a fixed
	// destination — e.g. a package tracking page for a
	// single-package source.
	Link string `toml:"link,omitempty" json:"link,omitempty"`

	// CheckInterval overrides [check].check_interval for this
	// source. Accepts either a Go duration string ("1h", "30m",
	// "10m") or a 5-field cron expression ("0 */6 * * *"). The
	// format is auto-detected by the presence of whitespace. If
	// empty, the source uses the global [check].check_interval.
	CheckInterval string `toml:"check_interval,omitempty" json:"check_interval,omitempty"`

	// Enabled controls whether the source is active. The pointer
	// is used so a missing field (the common case for existing
	// configs) is distinct from an explicit false: a nil pointer
	// means "default to enabled", preserving backward
	// compatibility with sources written before this field
	// existed. IsEnabled hides the pointer from the rest of the
	// codebase.
	Enabled *bool `toml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether the source should be active. A nil
// Enabled pointer is treated as true (the historical default).
func (s *Source) IsEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// SetEnabled is a convenience for code paths (web API, tests)
// that want to flip the enabled flag without dealing with the
// pointer.
func (s *Source) SetEnabled(v bool) {
	s.Enabled = &v
}

// ResolvedInterval returns the effective polling interval for
// this source: the per-source CheckInterval if set, otherwise
// the supplied global default. The return value is a Go duration
// string ("1h", "30m", "10m", etc.) — callers parse it with
// time.ParseDuration.
//
// Whitespace-only per-source values are treated as empty so the
// global default is used. Defensive: the loader rejects
// whitespace-only values as invalid durations, but the helper
// stays self-consistent regardless.
func (s *Source) ResolvedInterval(globalDefault string) string {
	if v := strings.TrimSpace(s.CheckInterval); v != "" {
		return v
	}
	return globalDefault
}

// Notification is the value a Fetcher.Format returns. The notify
// package consumes it. Keeping this in the source package means
// the per-type format knowledge lives next to the per-type
// fetch knowledge; the notify package only knows how to ship
// what's in front of it.
type Notification struct {
	Title    string // ntfy Title header
	Body     string // ntfy message body
	Priority string // "default" | "high" | etc.
	Tags     string // comma-separated ntfy tags
	Click    string // ntfy Click header
}

// Fetcher is one source type's full contract: fetch the current
// set of items, validate a freshly-decoded Source (apply
// defaults), and format a diff into a Notification. Implement
// this interface, register the value in the registry, and the
// dispatcher (Fetch/Validate/Format) routes to it.
//
// The interface is intentionally narrow: every method is one
// thing the rest of the binary needs to ask of a source type.
type Fetcher interface {
	// Type returns the string that identifies this fetcher in
	// the registry and in Source.Type. The convention is to
	// return a literal (e.g. "github_file"); the registry key
	// must match.
	Type() string
	// Fetch retrieves the current set of items for s. now is
	// passed through so that any {{...}} placeholders in s's URL
	// fields are substituted with a consistent timestamp per
	// run. A single timestamp avoids the case where two
	// substitutions in one URL see different values.
	Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error)
	// Validate applies defaults and checks that the source has
	// the fields this fetcher needs. It mutates s in place
	// (e.g. setting Ref to "HEAD" for github_file sources with
	// no ref).
	Validate(s *Source) error
	// Format builds a Notification for the diff (added and/or
	// removed items). Callers pass only items that actually
	// changed; the format helper decides the body layout.
	Format(s *Source, added, removed []Item) Notification
}

// registry maps a Source.Type string to its Fetcher. Adding a
// new source type is one line here.
var registry = map[string]Fetcher{
	"github_file": githubFileFetcher{},
	"html":        htmlFetcher{},
	"json":        jsonFetcher{},
	"json_value":  jsonValueFetcher{},
}

// SupportedTypes returns the registered type names, useful for
// validating user input and for error messages.
func SupportedTypes() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	return out
}

// Fetch dispatches to the Fetcher registered for s.Type. The
// returned error is for transient failures (network, gh CLI not
// found, etc.) and is reported to the user as a separate error
// notification when set.
func Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	f, ok := registry[s.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported type %q (want one of %v)", s.Type, SupportedTypes())
	}
	return f.Fetch(ctx, s, now)
}

// Validate dispatches to the Fetcher registered for s.Type. It
// returns an error for an unknown type or for a source that
// doesn't have the fields its fetcher needs.
func Validate(s *Source) error {
	f, ok := registry[s.Type]
	if !ok {
		return fmt.Errorf("unknown type %q (want one of %v)", s.Type, SupportedTypes())
	}
	return f.Validate(s)
}

// Format dispatches to the Fetcher registered for s.Type. For an
// unknown type it returns a default notification (so a
// misconfigured source still surfaces a clear error to the
// user) rather than panicking.
func Format(s *Source, added, removed []Item) Notification {
	f, ok := registry[s.Type]
	if !ok {
		return Notification{
			Title:    s.Name,
			Body:     fmt.Sprintf("(unknown source type %q)", s.Type),
			Priority: "default",
			Tags:     "loudspeaker",
		}
	}
	return f.Format(s, added, removed)
}
