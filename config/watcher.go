package config

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// configWatcher monitors a TOML config file for changes and
// calls onChange after a 200ms debounce. Editors commonly emit
// several events per save (write, rename, chmod), so debouncing
// is needed to avoid reloading the config multiple times for
// one user-visible edit.
//
// IMPORTANT: this watches the parent *directory*, not the file
// itself. Many editors (vim with backupcopy=no, TextEdit, and
// most IDEs) save by writing a temp file and renaming it over
// the target. An inotify subscription on the file inode dies
// with the rename; a subscription on the parent directory
// survives the rename and sees the new file as a Create event.
// This is the well-known fsnotify footgun.
type configWatcher struct {
	path     string
	onChange func()
}

func NewWatcher(path string, onChange func()) *configWatcher {
	return &configWatcher{path: path, onChange: onChange}
}

// Start launches the watcher goroutine. It returns immediately;
// call Stop to wait for shutdown.
func (cw *configWatcher) Start(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(cw.path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		// Debounce timer: reset on every event, fire onChange
		// only after a quiet period of 200ms.
		var debounce *time.Timer
		base := filepath.Base(cw.path)
		for {
			select {
			case <-ctx.Done():
				if debounce != nil {
					debounce.Stop()
				}
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				// Filter to events on our config file. Some
				// editors save by creating a temp file and
				// renaming it over the target — the rename
				// shows up as a Create event in the parent dir.
				if filepath.Base(event.Name) != base {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(200*time.Millisecond, cw.onChange)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("config watcher: %v", err)
			}
		}
	}()
	return nil
}
