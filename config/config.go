// Package config is the on-disk shape of the user's checkdiff
// configuration. The Source type lives in the source package
// (it's the domain concept); this package owns the wrapper
// types (NtfyConfig, CheckConfig, WebConfig), the loader, the
// atomic-write helper, the fsnotify hot-reload watcher, and
// the first-run generator.
//
// The Config struct is the only thing the rest of the binary
// reads; everything in this package exists to produce and
// persist a *Config.
package config

import "checkdiff/source"

// Config is the on-disk config file. TOML is the only
// supported format. To disable a source, comment out its
// [[sources]] block — the TOML parser drops the lines and the
// run loop never sees the entry.
//
// Both toml and json tags are set so the same struct shape
// works for the on-disk TOML (lowercase keys, matching the
// committed sample) and the wire JSON returned by the web
// API (also lowercase, matching what the web UI reads). The
// toml:"token,omitempty" hides an empty token in TOML; the
// json:"token,omitempty" hides an empty token in JSON. The
// web API additionally masks a non-empty token as "****" in
// GET /api/config responses.
type Config struct {
	Ntfy    NtfyConfig      `toml:"ntfy" json:"ntfy"`
	Check   CheckConfig     `toml:"check" json:"check"`
	Web     WebConfig       `toml:"web" json:"web"`
	Sources []source.Source `toml:"sources" json:"sources"`
}

// NtfyConfig holds the ntfy server/topic pair the daemon
// publishes to. Topic is required; Server defaults to
// https://ntfy.sh.
type NtfyConfig struct {
	// Server is the ntfy base URL. Defaults to https://ntfy.sh.
	Server string `toml:"server" json:"server"`
	// Topic is the ntfy topic. Required.
	Topic string `toml:"topic" json:"topic"`
}

// CheckConfig holds runtime options that aren't tied to a
// specific source. The only knob is the global polling
// interval, used as the default for sources that don't set
// their own check_interval. Defaults to 1h to preserve
// historical behavior.
//
// Note the toml vs json tag difference: the on-disk format
// uses the snake_case "check_interval" (matching the sample
// config in the README and the daemon's first-run template),
// while the web UI's wire format uses camelCase "interval"
// (matching the existing JS field names). A single field
// needs both tags because the same Go value flows through
// both serialisations.
type CheckConfig struct {
	// Interval is a Go duration string: "1h", "30m", "10m",
	// "15s", "2h30m", etc. Used as the default for sources
	// that don't set their own Source.CheckInterval. Must be
	// >= 1 minute (the daemon's per-source goroutine rejects
	// shorter values).
	Interval string `toml:"check_interval" json:"interval"`
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
	Listen string `toml:"listen" json:"listen"`
	// Token is the shared secret required to access the web UI
	// and JSON API. Empty means the web server is disabled.
	Token string `toml:"token,omitempty" json:"token,omitempty"`
}
