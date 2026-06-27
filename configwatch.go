package main

import (
	"context"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

// configWatcher monitors a TOML config file for changes and
// calls onChange after a 200ms debounce. Editors commonly emit
// several events per save (write, rename, chmod), so debouncing
// is needed to avoid reloading the config multiple times for
// one user-visible edit.
//
// The watcher runs in a goroutine launched by Start. Stop
// cancels the context and waits for the goroutine to exit.
type configWatcher struct {
	path     string
	onChange func()
}

func newConfigWatcher(path string, onChange func()) *configWatcher {
	return &configWatcher{path: path, onChange: onChange}
}

// Start launches the watcher goroutine. It returns immediately;
// call Stop to wait for shutdown.
func (cw *configWatcher) Start(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(cw.path); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		// Debounce timer: reset on every event, fire onChange
		// only after a quiet period of 200ms.
		var debounce *time.Timer
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
				// We care about writes and creates (some editors
				// save by renaming a temp file over the target).
				if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
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
