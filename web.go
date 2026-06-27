package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web/*.html web/*.css web/*.js
var webAssets embed.FS

// webServer is the HTTP surface for the daemon. It serves the JSON
// API and the static web UI. Auth is enforced by a middleware that
// checks a token supplied via the Authorization header or a
// ?token= query parameter. When the token is empty, the server
// does not start (the daemon still runs sources; only the HTTP
// surface is disabled).
//
// The server has access to the on-disk paths of the config and
// state files so mutating API endpoints can persist their changes
// to TOML. The fsnotify watcher (in main.go) picks up the
// rewritten TOML and triggers a daemon Reload, so the new
// configuration takes effect automatically.
//
// Concurrent writes to the config (e.g. two POSTs racing) are
// serialized by writeMu. Without it, two handlers can both
// append, both write the file, and one will lose its source on
// the next load — or, worse, an interleaved slice operation
// can panic with an out-of-range index.
type webServer struct {
	cfg        *Config
	daemon     *daemon
	state      *State
	configPath string
	statePath  string
	token      string
	listen     string
	srv        *http.Server
	mu         sync.Mutex
	writeMu    sync.Mutex
	running    bool
}

func newWebServer(cfg *Config, d *daemon, st *State, configPath, statePath string) *webServer {
	return &webServer{
		cfg:        cfg,
		daemon:     d,
		state:      st,
		configPath: configPath,
		statePath:  statePath,
		token:      cfg.Web.Token,
		listen:     cfg.Web.Listen,
	}
}

// authMiddleware checks that the request carries the configured
// token, either as an Authorization: Bearer header or as a
// ?token= query parameter. On mismatch it returns 401. The static
// UI assets are also gated so the login form can't be bypassed
// by navigating directly to /.
func (w *webServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		token := tokenFromRequest(r)
		w.mu.Lock()
		currentToken := w.token
		w.mu.Unlock()
		if token == "" || token != currentToken {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	return r.URL.Query().Get("token")
}

// Start binds the HTTP server and begins serving. It returns an
// error if the bind fails. If the token is empty, Start is a
// no-op (the web server is disabled by config).
func (w *webServer) Start() error {
	w.mu.Lock()
	if w.token == "" {
		w.mu.Unlock()
		log.Printf("web UI disabled: set [web] token in config to enable")
		return nil
	}
	if w.running {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()
	return w.bind()
}

// bind is the actual server-binding logic, shared by Start and
// Reload (when the listen address changes).
func (w *webServer) bind() error {
	w.mu.Lock()
	listen := w.listen
	w.mu.Unlock()

	mux := http.NewServeMux()
	w.registerRoutes(mux)

	srv := &http.Server{
		Addr:              listen,
		Handler:           w.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	w.mu.Lock()
	w.srv = srv
	w.running = true
	w.mu.Unlock()

	log.Printf("web server listening on %s", listen)
	// ListenAndServe blocks; run it in a goroutine so Start
	// returns immediately. The caller is expected to call Stop
	// on shutdown.
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("web server: %v", err)
		}
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()
	return nil
}

// Reload picks up the new config's token and listen address. The
// token change takes effect immediately (the next request is
// checked against the new token); the listen address change
// requires a graceful HTTP server restart.
//
// If the listen address hasn't changed, this is a no-op for the
// HTTP server itself; we just update the in-memory token.
func (w *webServer) Reload(newCfg *Config) {
	w.mu.Lock()
	oldListen := w.listen
	w.token = newCfg.Web.Token
	w.listen = newCfg.Web.Listen
	w.cfg = newCfg
	w.mu.Unlock()

	if w.token == "" {
		// Token was just cleared; stop the server entirely.
		w.Stop()
		return
	}
	if newCfg.Web.Listen != oldListen {
		// Listen address changed; restart the server.
		log.Printf("web listen address changed from %s to %s; restarting", oldListen, newCfg.Web.Listen)
		w.Stop()
		if err := w.bind(); err != nil {
			log.Printf("web server restart: %v", err)
		}
	}
}

// Stop gracefully shuts down the HTTP server. It is a no-op if
// the server was never started.
func (w *webServer) Stop() {
	w.mu.Lock()
	srv := w.srv
	w.srv = nil
	w.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

// registerRoutes wires the API endpoints and the static UI
// assets into the mux. The static assets are served from the
// embedded web/ directory so the binary is self-contained
// (no separate files to deploy).
func (w *webServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", w.handleLogin)
	mux.HandleFunc("/api/state", w.handleState)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/sources", w.handleSources)
	mux.HandleFunc("/api/sources/", w.handleSourceByID)
	mux.HandleFunc("/api/settings", w.handleSettings)

	// Static UI assets. We expose them at / so the user only
	// needs to remember the listen address. /api/... takes
	// precedence because it's registered first.
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Printf("web: embed sub: %v", err)
		return
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// handleLogin is a token-only endpoint. It exists so the web UI
// can verify a token before storing it in localStorage without
// pulling down the (potentially large) state response. Returns
// 200 on match, 401 on mismatch.
func (w *webServer) handleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// authMiddleware has already verified the token matches (we
	// wouldn't be here otherwise). Return 200 with an empty body.
	rw.WriteHeader(http.StatusOK)
}

// handleState returns the per-source runtime state. Read-only.
func (w *webServer) handleState(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(rw, w.state.All())
}

// handleConfig returns the current config with the web token
// masked (so a leaked state response can't be used to extract
// the secret). Read-only; use PUT /api/settings to modify.
func (w *webServer) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	masked := *w.cfg
	masked.Web.Token = "****"
	writeJSON(rw, masked)
}

// handleSources lists or creates sources. GET returns the current
// source list; POST appends a new source. Both paths write the
// config to disk; the fsnotify watcher in main.go picks up the
// change and calls daemon.Reload, which starts a runner for the
// new source.
func (w *webServer) handleSources(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(rw, w.cfg.Sources)
	case http.MethodPost:
		var src Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		// Validate before committing: a malformed source would
		// otherwise be written to TOML and trigger a load error
		// on the next reload, breaking all other sources.
		if err := validateSource(&src); err != nil {
			http.Error(rw, fmt.Sprintf("validate: %v", err), http.StatusBadRequest)
			return
		}
		// ID must be unique. The TOML parser doesn't enforce this
		// in the runtime API; we check here so the user gets a
		// clear 400 instead of a confusing reload error.
		for _, existing := range w.cfg.Sources {
			if existing.ID == src.ID {
				http.Error(rw, fmt.Sprintf("source %q already exists", src.ID), http.StatusBadRequest)
				return
			}
		}
		w.writeMu.Lock()
		defer w.writeMu.Unlock()
		w.cfg.Sources = append(w.cfg.Sources, src)
		if err := writeConfigFile(w.configPath, w.cfg); err != nil {
			// Roll back in-memory state so the UI doesn't show
			// a source that wasn't persisted.
			w.cfg.Sources = w.cfg.Sources[:len(w.cfg.Sources)-1]
			http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(src)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSourceByID routes the various /api/sources/{id}[/*] paths.
// It supports:
//   POST   /api/sources/{id}/run  - trigger an immediate check
//   PUT    /api/sources/{id}      - replace this source's config
//   DELETE /api/sources/{id}      - remove this source
//
// Run-now is non-blocking: it sends to the source's runNowCh and
// returns 202 immediately. If a run is already pending, the
// request is silently coalesced (the running check covers it).
//
// PUT and DELETE write the new config to disk; the fsnotify
// watcher triggers a daemon.Reload, which cancels the old
// runner (preserving state) and starts a new one with the new
// config.
func (w *webServer) handleSourceByID(rw http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sources/"):]
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	// /api/sources/{id}/run
	if strings.HasSuffix(id, "/run") {
		sourceID := strings.TrimSuffix(id, "/run")
		if r.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := w.daemon.TriggerNow(sourceID); err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)
			return
		}
		rw.WriteHeader(http.StatusAccepted)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var src Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		if err := validateSource(&src); err != nil {
			http.Error(rw, fmt.Sprintf("validate: %v", err), http.StatusBadRequest)
			return
		}
		// Find and replace. The ID in the URL is authoritative
		// (the body's id field is ignored if it differs, to avoid
		// the URL/Body desync footgun).
		src.ID = id
		w.writeMu.Lock()
		defer w.writeMu.Unlock()
		found := false
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				old := w.cfg.Sources[i]
				w.cfg.Sources[i] = src
				if err := writeConfigFile(w.configPath, w.cfg); err != nil {
					// Roll back.
					w.cfg.Sources[i] = old
					http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
					return
				}
				found = true
				break
			}
		}
		if !found {
			http.NotFound(rw, r)
			return
		}
		writeJSON(rw, src)
	case http.MethodDelete:
		w.writeMu.Lock()
		defer w.writeMu.Unlock()
		found := -1
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				found = i
				break
			}
		}
		if found < 0 {
			http.NotFound(rw, r)
			return
		}
		old := w.cfg.Sources[found]
		w.cfg.Sources = append(w.cfg.Sources[:found], w.cfg.Sources[found+1:]...)
		if err := writeConfigFile(w.configPath, w.cfg); err != nil {
			// Roll back.
			w.cfg.Sources = append(w.cfg.Sources[:found], append([]Source{old}, w.cfg.Sources[found:]...)...)
			http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// settingsBody is the request body for PUT /api/settings. Each
// field is optional; only the supplied blocks are updated.
type settingsBody struct {
	Ntfy  *NtfyConfig  `json:"ntfy,omitempty"`
	Check *CheckConfig `json:"check,omitempty"`
	Web   *WebConfig   `json:"web,omitempty"`
}

// handleSettings updates the ntfy / check / web blocks in the
// config. Each block is optional; only supplied fields override
// the current values. The new config is written to disk; the
// fsnotify watcher triggers a daemon.Reload and a webServer.Reload.
//
// The web token and listen address are picked up immediately by
// the web server's Reload. ntfy settings are picked up by the
// daemon's Reload (which updates the shared ntfy client).
// Changes to [check].check_interval only affect sources that
// don't set their own per-source interval, and only on the next
// reload (existing runners keep their cached intervalFn).
func (w *webServer) handleSettings(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body settingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	if body.Ntfy != nil {
		if body.Ntfy.Server != "" {
			w.cfg.Ntfy.Server = body.Ntfy.Server
		}
		if body.Ntfy.Topic != "" {
			w.cfg.Ntfy.Topic = body.Ntfy.Topic
		}
	}
	if body.Check != nil {
		if body.Check.Interval != "" {
			if _, err := parseInterval(body.Check.Interval); err != nil {
				http.Error(rw, fmt.Sprintf("check.check_interval: %v", err), http.StatusBadRequest)
				return
			}
			w.cfg.Check.Interval = body.Check.Interval
		}
	}
	if body.Web != nil {
		if body.Web.Listen != "" {
			w.cfg.Web.Listen = body.Web.Listen
		}
		// Token may legitimately be the empty string (the user
		// wants to disable the web UI). Update unconditionally
		// when the field is present in the request.
		w.cfg.Web.Token = body.Web.Token
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := writeConfigFile(w.configPath, w.cfg); err != nil {
		http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
		return
	}
	// Return the (now-updated) config with the token masked, so
	// the UI can refresh its local copy in one round-trip.
	masked := *w.cfg
	masked.Web.Token = "****"
	writeJSON(rw, masked)
}

// writeConfigFile persists the in-memory config to disk atomically
// (write to .tmp, fsync, rename). The fsnotify watcher in main.go
// will pick up the change and call daemon.Reload + webServer.Reload.
//
// All mutating API endpoints (handleSources, handleSourceByID,
// handleSettings) route through this helper so the on-disk
// config is the single source of truth.
func writeConfigFile(path string, cfg *Config) error {
	b, err := marshalConfig(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := writeFileAtomic(tmp, path, b, 0o644); err != nil {
		return err
	}
	return nil
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}
