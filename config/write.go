package config

import (
	"fmt"
	"os"
)

// WriteFile persists the in-memory config to disk atomically
// (write to .tmp, fsync, rename). The fsnotify watcher in
// main.go will pick up the change and call daemon.Reload +
// webServer.Reload.
//
// All mutating API endpoints (handleSources,
// handleSourceByID, handleSettings) route through this helper
// so the on-disk config is the single source of truth.
func WriteFile(path string, cfg *Config) error {
	b, err := Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := writeFileAtomic(tmp, path, b, 0o644); err != nil {
		return err
	}
	return nil
}

// writeFileAtomic writes data to tmp, fsyncs, then renames
// over path. The rename is atomic on POSIX filesystems, so
// concurrent readers (e.g. the fsnotify watcher, a separate
// `checkdiff` invocation) never see a half-written file.
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
