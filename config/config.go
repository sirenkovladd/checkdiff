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
	LLM     LLMConfig       `toml:"llm" json:"llm"`
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
	// >= 15 seconds (the daemon's per-source goroutine rejects
	// shorter values).
	Interval string `toml:"check_interval" json:"interval"`
}

// WebConfig holds the optional web UI settings. When Token is
// nil or points to an empty string the web server does not
// start (the daemon still runs sources; only the HTTP surface
// is disabled). When Token is non-empty, the HTTP server binds
// to Listen and requires the token on every request.
//
// Token is *string (not string) so the wire format can
// distinguish "field absent in the JSON request" from "field
// present, value is empty string". The two have different
// meanings: absent means "leave the current value alone", and
// empty string means "explicitly clear the token to disable
// the web UI". With a plain string, JSON decode can't tell
// them apart, so a settings-save that includes web but not
// token (the normal case for the JS form, which only sends
// the token when the user types one) would silently wipe the
// existing token. Pointer-to-string is the standard Go fix.
type WebConfig struct {
	// Listen is the bind address for the HTTP server, e.g.
	// "127.0.0.1:8080" (localhost only) or ":8080" (all
	// interfaces). Default: "127.0.0.1:8080".
	Listen string `toml:"listen" json:"listen"`
	// Token is the shared secret required to access the web UI
	// and JSON API. nil/empty means the web server is disabled.
	// See the type comment for why this is *string.
	Token *string `toml:"token,omitempty" json:"token,omitempty"`
}

// LLMConfig holds the optional LLM summarizer settings used by
// page_llm sources. The block is optional: with no model
// configured, page_llm still works but sends a static from/to
// diff instead of an LLM summary (so the change still
// notifies).
//
// The API key is never stored here. ApiKeyEnv names the env
// var holding the key (e.g. "OPENCODE_API_KEY" for an opencode
// Go subscription from https://opencode.ai/auth); ApiKeyFile
// points at a file containing the raw key or a JSON object
// with an api_key/apiKey/key/token field. Env wins when both
// are set. The daemon logs once at startup when the key is
// missing rather than failing every check.
//
// Provider is "openai" (POST {server}/chat/completions),
// "anthropic" (POST {server}/messages), or "responses" (POST
// {server}/responses) — the three protocols the opencode Go
// endpoints expose (see https://opencode.ai/docs/go#endpoints). Server is the base
// URL without the operation path, e.g.
// "https://opencode.ai/zen/go/v1".
type LLMConfig struct {
	// Provider is "openai" or "anthropic". Empty defaults to
	// "openai" when a model is set.
	Provider string `toml:"provider" json:"provider"`
	// Server is the base URL, e.g.
	// "https://opencode.ai/zen/go/v1". Empty defaults to the
	// opencode Go base URL when a model is set.
	Server string `toml:"server" json:"server"`
	// Model is the model id, e.g. "glm-5.3-flash". Empty
	// disables the LLM (page_llm falls back to static diffs).
	Model string `toml:"model" json:"model"`
	// ApiKeyEnv names the env var holding the API key.
	ApiKeyEnv string `toml:"api_key_env" json:"api_key_env"`
	// ApiKeyFile points at a file holding the key.
	ApiKeyFile string `toml:"api_key_file" json:"api_key_file"`
	// ApiKeyPath is an optional dot-separated path into a JSON
	// key file (e.g. "opencode-go.key" for a multi-provider
	// store like ~/.pi/agent/auth.json). Empty = the file
	// itself is the key (raw text or {"api_key": "..."}).
	ApiKeyPath string `toml:"api_key_path" json:"api_key_path"`
	// MaxTokens caps the summary length. <=0 defaults to 300.
	MaxTokens int `toml:"max_tokens" json:"max_tokens"`
}
