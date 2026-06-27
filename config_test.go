package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig drops a TOML string to a temp file and returns the path.
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
	cfg, err := loadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
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
check_interval = "not-a-duration"
`
	if _, err := loadConfig(writeConfig(t, body)); err == nil {
		t.Errorf("loadConfig with invalid per-source interval: got nil error, want error")
	}
}

func TestLoadConfigPerSourceIntervalTooShort(t *testing.T) {
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "1h"

[[sources]]
id             = "tiny"
name           = "sub-minute"
type           = "json"
url            = "https://example.com/api"
check_interval = "30s"
`
	if _, err := loadConfig(writeConfig(t, body)); err == nil {
		t.Errorf("loadConfig with sub-minute per-source interval: got nil error, want error")
	}
}

func TestLoadConfigGlobalIntervalStillValidates(t *testing.T) {
	// Make sure the new per-source field didn't accidentally loosen
	// the validation on the global [check].check_interval. The
	// global interval is just checked as a valid Go duration; the
	// systemd OnCalendar constraints (must divide an hour) are
	// applied separately by onCalendarFor when -print-timer is used.
	body := `
[ntfy]
topic = "test"

[check]
check_interval = "7m"

[[sources]]
id   = "x"
name = "x"
type = "json"
url  = "https://example.com"
`
	if _, err := loadConfig(writeConfig(t, body)); err != nil {
		t.Errorf("loadConfig with valid Go-duration global interval: got %v, want nil", err)
	}
}
