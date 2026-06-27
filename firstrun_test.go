package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureConfigForDaemonCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := ensureConfigForDaemon(path); err != nil {
		t.Fatalf("ensureConfigForDaemon: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestEnsureConfigForDaemonIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := ensureConfigForDaemon(path); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call: file already exists, must be a no-op (and must
	// not overwrite the existing file).
	if err := ensureConfigForDaemon(path); err != nil {
		t.Fatalf("second call: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	// The file should still be loadable as a valid config.
	cfg, err := loadConfig(path)
	if err != nil {
		t.Errorf("after ensureConfigForDaemon, loadConfig: %v", err)
	}
	if cfg.Web.Token == "" {
		t.Errorf("generated config has empty token")
	}
	_ = body
}

func TestEnsureConfigForDaemonGeneratedConfigLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := ensureConfigForDaemon(path); err != nil {
		t.Fatalf("ensureConfigForDaemon: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig on generated file: %v", err)
	}
	if cfg.Web.Listen != "127.0.0.1:8080" {
		t.Errorf("Web.Listen = %q, want %q", cfg.Web.Listen, "127.0.0.1:8080")
	}
	if cfg.Web.Token == "" {
		t.Errorf("Web.Token is empty in generated config")
	}
	if cfg.Check.Interval != "1h" {
		t.Errorf("Check.Interval = %q, want 1h", cfg.Check.Interval)
	}
}

func TestGenerateToken(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	b, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if a == b {
		t.Errorf("two consecutive tokens are identical: %q", a)
	}
	// 32 bytes -> 43 base64url chars (no padding).
	if len(a) < 32 {
		t.Errorf("token too short: len=%d", len(a))
	}
}

func TestDefaultConfigWithToken(t *testing.T) {
	body := defaultConfigWithToken("abc123")
	if !strings.Contains(body, `token  = "abc123"`) {
		t.Errorf("generated config does not contain the token: %s", body)
	}
	if !strings.Contains(body, "[web]") {
		t.Errorf("generated config missing [web] block")
	}
	if !strings.Contains(body, "[check]") {
		t.Errorf("generated config missing [check] block")
	}
}
