package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigWatcherFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	called := make(chan struct{}, 1)
	cw := newConfigWatcher(path, func() { called <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give the watcher a moment to subscribe before we modify
	// the file; otherwise the Write may race the Add.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(path, []byte("updated"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Errorf("config watcher callback did not fire within 2s")
	}
}

func TestConfigWatcherDebounces(t *testing.T) {
	// Multiple rapid writes should coalesce into a single
	// callback (after the 200ms debounce).
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	count := int64(0)
	cw := newConfigWatcher(path, func() { atomic.AddInt64(&count, 1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Three rapid writes within 100ms total.
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(path, []byte("v"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatalf("writeFile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Wait for the debounce to fire.
	time.Sleep(500 * time.Millisecond)

	if got := atomic.LoadInt64(&count); got > 1 {
		t.Errorf("debounce: got %d callbacks, want <= 1", got)
	}
}

func TestConfigWatcherStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	cw := newConfigWatcher(path, func() {})

	ctx, cancel := context.WithCancel(context.Background())
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	// After cancel, the goroutine should exit promptly. We can't
	// observe the goroutine directly, but the test is a smoke
	// test that the cancel path doesn't panic or hang.
	time.Sleep(100 * time.Millisecond)
}

func TestConfigWatcherSurvivesRename(t *testing.T) {
	// Editors that save by writing a temp file and renaming it
	// over the target (vim with backupcopy=no, TextEdit, most
	// IDEs) used to break the file-watching approach: the rename
	// leaves the original inode behind and the watcher loses its
	// subscription. With directory-watching, the rename shows up
	// as a Create event for the new file and we fire the
	// callback.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	called := make(chan struct{}, 4)
	cw := newConfigWatcher(path, func() { called <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Write a new file then rename it over the target. The
	// rename produces a Create event for the directory entry.
	tmp := filepath.Join(dir, "config.toml.tmp")
	if err := os.WriteFile(tmp, []byte("updated"), 0o644); err != nil {
		t.Fatalf("writeFile tmp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Errorf("config watcher did not fire after a rename-save")
	}
}

func TestDaemonReloadSwapsConfig(t *testing.T) {
	// Reload with a new config should replace the runner for an
	// existing source and add a runner for a new one.
	enabled := true
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	st := &State{Version: currentStateVersion, Sources: map[string]*SourceState{}}
	d := newDaemon(cfg, st, NewNtfyClient("https://ntfy.sh", "test"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// Build a new config: a is still here (unchanged), b is new.
	newCfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
			{ID: "b", Name: "b", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	d.Reload(newCfg)

	d.mu.Lock()
	runners := len(d.runners)
	d.mu.Unlock()
	if runners != 2 {
		t.Errorf("after Reload: got %d runners, want 2", runners)
	}
}

func TestDaemonReloadRemovesSource(t *testing.T) {
	// Reload with a config that drops a source should cancel
	// that source's runner.
	enabled := true
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
			{ID: "b", Name: "b", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	st := &State{Version: currentStateVersion, Sources: map[string]*SourceState{}}
	d := newDaemon(cfg, st, NewNtfyClient("https://ntfy.sh", "test"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// New config: only a remains.
	newCfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{
			{ID: "a", Name: "a", Type: "json", URL: "https://127.0.0.1:1", Enabled: &enabled},
		},
	}
	d.Reload(newCfg)

	d.mu.Lock()
	runners := len(d.runners)
	_, hasB := d.runners["b"]
	d.mu.Unlock()
	if runners != 1 {
		t.Errorf("after Reload: got %d runners, want 1", runners)
	}
	if hasB {
		t.Errorf("source b should have been removed by Reload")
	}
}

func TestDaemonReloadBeforeStartIsNoOp(t *testing.T) {
	// Reload before Start should be a safe no-op (parentCtx is nil).
	cfg := &Config{
		Check: CheckConfig{Interval: "1h"},
		Sources: []Source{},
	}
	d := newDaemon(cfg, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}, NewNtfyClient("https://ntfy.sh", "test"))
	// Must not panic.
	d.Reload(cfg)
}
