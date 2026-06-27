package config

import (
	"os"
	"path/filepath"
	"testing"

	"checkdiff/source"
)

// writeConfig drops a TOML string to a temp file and returns
// the path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigPerSourceInterval(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "1h"

[[sources]]
id              = "fast"
name            = "fast source"
type            = "json"
url             = "https://example.com/api"
check_interval  = "30m"

[[sources]]
id   = "default"
name = "uses default"
type = "json"
url  = "https://example.com/other"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Sources[0].CheckInterval; got != "30m" {
		t.Errorf("Sources[0].CheckInterval = %q, want %q", got, "30m")
	}
	if got := cfg.Sources[1].CheckInterval; got != "" {
		t.Errorf("Sources[1].CheckInterval = %q, want empty (fall back to global)", got)
	}
}

func TestLoadConfigPerSourceIntervalInvalid(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "1h"

[[sources]]
id             = "bad"
name           = "bad interval"
type           = "json"
url            = "https://example.com/api"
check_interval = "not-a-duration-or-cron"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Errorf("Load with invalid per-source interval: got nil error, want error")
	}
}

func TestLoadConfigPerSourceIntervalCron(t *testing.T) {
	// Per-source interval accepts cron expressions. A standard
	// 5-field cron string should parse without error.
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "1h"

[[sources]]
id             = "cron-src"
name           = "cron source"
type           = "json"
url            = "https://example.com/api"
check_interval = "0 */6 * * *"
`
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Errorf("Load with cron per-source interval: got %v, want nil", err)
	}
}

func TestLoadConfigPerSourceIntervalTooShort(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "1h"

[[sources]]
id             = "too-fast"
name           = "too fast"
type           = "json"
url            = "https://example.com/api"
check_interval = "30s"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Errorf("Load with sub-minute per-source interval: got nil error, want error")
	}
}

func TestLoadConfigGlobalIntervalStillValidates(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "not-a-duration"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Errorf("Load with invalid global interval: got nil error, want error")
	}
}

func TestSourceResolvedInterval(t *testing.T) {
	// The helper applies the per-source value when set and
	// falls back to the global default otherwise.
	cases := []struct {
		name       string
		perSource  string
		globalDef  string
		want       string
	}{
		{"per-source wins", "30m", "1h", "30m"},
		{"falls back to global", "", "1h", "1h"},
		{"whitespace treated as empty", "  ", "1h", "1h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &source.Source{CheckInterval: c.perSource}
			if got := s.ResolvedInterval(c.globalDef); got != c.want {
				t.Errorf("ResolvedInterval(%q) = %q, want %q", c.perSource, got, c.want)
			}
		})
	}
}

func TestLoadConfigWebBlockDefaults(t *testing.T) {
	body := `
[ntfy]
topic = "test"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.Listen != "127.0.0.1:8080" {
		t.Errorf("Web.Listen = %q, want default 127.0.0.1:8080", cfg.Web.Listen)
	}
	if cfg.Web.Token != "" {
		t.Errorf("Web.Token = %q, want empty (web disabled)", cfg.Web.Token)
	}
}

func TestLoadConfigWebBlockExplicit(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[web]
listen = "0.0.0.0:9000"
token  = "secret"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.Listen != "0.0.0.0:9000" {
		t.Errorf("Web.Listen = %q, want 0.0.0.0:9000", cfg.Web.Listen)
	}
	if cfg.Web.Token != "secret" {
		t.Errorf("Web.Token = %q, want secret", cfg.Web.Token)
	}
}

func TestWriteConfigFileRoundTrip(t *testing.T) {
	// Write a config, read it back, write it again, read it
	// back. The two reads should match: round-trip stability
	// is the property the web UI depends on when it
	// rewrites the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	src := &Config{
		Ntfy:  NtfyConfig{Server: "https://ntfy.sh", Topic: "topic"},
		Check: CheckConfig{Interval: "1h"},
		Web:   WebConfig{Listen: "127.0.0.1:8080", Token: "tok"},
		Sources: []source.Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://example.com/a"},
		},
	}
	if err := WriteFile(path, src); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := WriteFile(path, loaded); err != nil {
		t.Fatalf("WriteFile (re-encoding): %v", err)
	}
	loaded2, err := Load(path)
	if err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	if loaded2.Ntfy.Topic != src.Ntfy.Topic {
		t.Errorf("round-trip: Ntfy.Topic = %q, want %q", loaded2.Ntfy.Topic, src.Ntfy.Topic)
	}
	if len(loaded2.Sources) != 1 || loaded2.Sources[0].ID != "a" {
		t.Errorf("round-trip: sources lost or reordered: %+v", loaded2.Sources)
	}
}
