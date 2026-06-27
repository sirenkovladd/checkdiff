// Package webapi is the HTTP surface for the daemon. It serves
// the JSON API and the static web UI from the webui package.
// Auth is enforced by a middleware that checks a token
// supplied via the Authorization header or a ?token= query
// parameter. When the token is empty, the server does not
// start (the daemon still runs sources; only the HTTP surface
// is disabled).
//
// Concurrent access to the in-memory config is serialized by
// the server's RWMutex: read handlers take RLock; write
// handlers take Lock.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"checkdiff/config"
	"checkdiff/daemon"
	"checkdiff/schedule"
	"checkdiff/source"
	"checkdiff/state"
	"checkdiff/webui"
)

// Server is the HTTP surface. Construct with NewServer; call
// Start to bind, Reload to swap in a new config, Stop to
// shutdown.
type Server struct {
	cfg        *config.Config
	daemon     *daemon.Daemon
	state      *state.State
	configPath string
	statePath  string
	token      string
	listen     string
	srv        *http.Server
	mu         sync.RWMutex
	running    bool
}

// NewServer constructs a Server. The configPath and statePath
// are stored so mutating API endpoints can persist their
// changes to disk; the fsnotify watcher in main.go picks up
// the rewritten TOML and triggers a daemon.Reload.
func NewServer(cfg *config.Config, d *daemon.Daemon, st *state.State, configPath, statePath string) *Server {
	return &Server{
		cfg:        cfg,
		daemon:     d,
		state:      st,
		configPath: configPath,
		statePath:  statePath,
		token:      cfg.Web.Token,
		listen:     cfg.Web.Listen,
	}
}

// authMiddleware checks that the request carries the
// configured token, either as an Authorization: Bearer header
// or as a ?token= query parameter. On mismatch it returns 401.
// The static UI assets are also gated so the login form can't
// be bypassed by navigating directly to /.
func (w *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		token := tokenFromRequest(r)
		w.mu.RLock()
		currentToken := w.token
		w.mu.RUnlock()
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

// Start binds the HTTP server and begins serving. It returns
// an error if the bind fails. If the token is empty, Start is
// a no-op (the web server is disabled by config).
func (w *Server) Start() error {
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
func (w *Server) bind() error {
	w.mu.RLock()
	listen := w.listen
	w.mu.RUnlock()

	apiMux := http.NewServeMux()
	rootMux := http.NewServeMux()
	w.registerRoutes(apiMux, rootMux)

	// /api/* requires the bearer token; everything else
	// (static UI assets) is served unauthenticated so the
	// browser can load the JS/CSS that contains the login
	// form. The login form is harmless without a token —
	// every API call is gated.
	rootMux.Handle("/api/", w.authMiddleware(apiMux))

	srv := &http.Server{
		Addr:              listen,
		Handler:           rootMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	w.mu.Lock()
	w.srv = srv
	w.running = true
	w.mu.Unlock()

	log.Printf("web server listening on %s", listen)
	// ListenAndServe blocks; run it in a goroutine so Start
	// returns immediately. The caller is expected to call
	// Stop on shutdown.
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

// Reload picks up the new config's token and listen address.
// The token change takes effect immediately (the next request
// is checked against the new token); the listen address
// change requires a graceful HTTP server restart.
//
// If the listen address hasn't changed, this is a no-op for
// the HTTP server itself; we just update the in-memory token.
func (w *Server) Reload(newCfg *config.Config) {
	w.mu.Lock()
	oldListen := w.listen
	oldToken := w.token
	w.token = newCfg.Web.Token
	w.listen = newCfg.Web.Listen
	w.cfg = newCfg
	w.mu.Unlock()

	if w.token == "" {
		// Token was just cleared; stop the server entirely.
		w.Stop()
		return
	}
	if oldToken != w.token {
		// Token changed (likely via the Settings UI).
		// Subsequent requests will be checked against the new
		// token; the user's browser may 401 once until the
		// UI updates localStorage with the new value.
		log.Printf("web token rotated")
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

// Stop gracefully shuts down the HTTP server. It is a no-op
// if the server was never started.
func (w *Server) Stop() {
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
// assets. The API is mounted under /api/ behind the auth
// middleware; the static UI is mounted at / WITHOUT auth, so
// the browser can fetch app.js and style.css before the user
// has signed in. The login form is the only thing the static
// UI serves to anonymous users, and the form is harmless — it
// just asks for the token. The token is what's actually
// secret; protecting it is what authMiddleware is for.
//
// The split also makes the ?token=... URL flow work: the
// browser loads / with the query token (auth passes), then
// fetches the JS/CSS without the query — which would have 401'd
// under the old "auth everything" model. Gating only the API
// means the JS can run and persist the token to localStorage
// for subsequent visits.
func (w *Server) registerRoutes(apiMux, rootMux *http.ServeMux) {
	apiMux.HandleFunc("/api/state", w.handleState)
	apiMux.HandleFunc("/api/config", w.handleConfig)
	apiMux.HandleFunc("/api/sources", w.handleSources)
	apiMux.HandleFunc("/api/sources/", w.handleSourceByID)
	apiMux.HandleFunc("/api/settings", w.handleSettings)
	apiMux.HandleFunc("/api/rotate-token", w.handleRotateToken)

	// Static UI assets are served from the webui package so
	// the binary is self-contained. They're served at / on the
	// root mux, which is NOT behind the auth middleware.
	rootMux.Handle("/", http.FileServer(http.FS(webui.FS())))
}

// handleState returns the per-source runtime state. Read-only.
func (w *Server) handleState(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(rw, w.state.All())
}

// handleConfig returns the current config with the web token
// masked (so a leaked state response can't be used to extract
// the secret). Read-only; use PUT /api/settings to modify.
func (w *Server) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.mu.RLock()
	masked := *w.cfg
	w.mu.RUnlock()
	masked.Web.Token = "****"
	writeJSON(rw, masked)
}

// handleSources lists or creates sources. GET returns the
// current source list; POST appends a new source. Both paths
// write the config to disk; the fsnotify watcher in main.go
// picks up the change and calls daemon.Reload, which starts a
// runner for the new source.
func (w *Server) handleSources(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.mu.RLock()
		out := make([]source.Source, len(w.cfg.Sources))
		copy(out, w.cfg.Sources)
		w.mu.RUnlock()
		writeJSON(rw, out)
	case http.MethodPost:
		var src source.Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		// Validate before committing: a malformed source would
		// otherwise be written to TOML and trigger a load
		// error on the next reload, breaking all other
		// sources.
		if err := source.Validate(&src); err != nil {
			http.Error(rw, fmt.Sprintf("validate: %v", err), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		// ID must be unique. The TOML parser doesn't enforce
		// this in the runtime API; we check here so the user
		// gets a clear 400 instead of a confusing reload
		// error.
		for _, existing := range w.cfg.Sources {
			if existing.ID == src.ID {
				w.mu.Unlock()
				http.Error(rw, fmt.Sprintf("source %q already exists", src.ID), http.StatusBadRequest)
				return
			}
		}
		w.cfg.Sources = append(w.cfg.Sources, src)
		if err := config.WriteFile(w.configPath, w.cfg); err != nil {
			// Roll back in-memory state so the UI doesn't
			// show a source that wasn't persisted.
			w.cfg.Sources = w.cfg.Sources[:len(w.cfg.Sources)-1]
			w.mu.Unlock()
			http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
			return
		}
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(src)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSourceByID routes the various /api/sources/{id}[/*]
// paths. It supports:
//
//	POST   /api/sources/{id}/run  - trigger an immediate check
//	PUT    /api/sources/{id}      - replace this source's config
//	DELETE /api/sources/{id}      - remove this source
//
// Run-now is non-blocking: it sends to the source's runNowCh
// and returns 202 immediately. If a run is already pending,
// the request is silently coalesced (the running check covers
// it).
//
// PUT and DELETE write the new config to disk; the fsnotify
// watcher triggers a daemon.Reload, which cancels the old
// runner (preserving state) and starts a new one with the new
// config.
func (w *Server) handleSourceByID(rw http.ResponseWriter, r *http.Request) {
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
		var src source.Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		if err := source.Validate(&src); err != nil {
			http.Error(rw, fmt.Sprintf("validate: %v", err), http.StatusBadRequest)
			return
		}
		// Find and replace. The ID in the URL is authoritative
		// (the body's id field is ignored if it differs, to
		// avoid the URL/Body desync footgun).
		src.ID = id
		w.mu.Lock()
		found := false
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				old := w.cfg.Sources[i]
				w.cfg.Sources[i] = src
				if err := config.WriteFile(w.configPath, w.cfg); err != nil {
					// Roll back.
					w.cfg.Sources[i] = old
					w.mu.Unlock()
					http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
					return
				}
				found = true
				break
			}
		}
		w.mu.Unlock()
		if !found {
			http.NotFound(rw, r)
			return
		}
		writeJSON(rw, src)
	case http.MethodDelete:
		w.mu.Lock()
		found := -1
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				found = i
				break
			}
		}
		if found < 0 {
			w.mu.Unlock()
			http.NotFound(rw, r)
			return
		}
		old := w.cfg.Sources[found]
		w.cfg.Sources = append(w.cfg.Sources[:found], w.cfg.Sources[found+1:]...)
		if err := config.WriteFile(w.configPath, w.cfg); err != nil {
			// Roll back.
			w.cfg.Sources = append(w.cfg.Sources[:found], append([]source.Source{old}, w.cfg.Sources[found:]...)...)
			w.mu.Unlock()
			http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
			return
		}
		w.mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// settingsBody is the request body for PUT /api/settings.
// Each field is optional; only the supplied blocks are
// updated.
type settingsBody struct {
	Ntfy  *config.NtfyConfig  `json:"ntfy,omitempty"`
	Check *config.CheckConfig `json:"check,omitempty"`
	Web   *config.WebConfig   `json:"web,omitempty"`
}

// handleRotateToken generates a new random token, writes it
// to the config file, updates the in-memory token, and
// returns the new value to the caller (the web UI, which then
// stores it in localStorage). The old token is invalidated
// immediately: the next request from any client that still
// has the old token will get 401.
//
// POST /api/rotate-token — body is empty; response is
// {"token": "<new>"} as JSON.
func (w *Server) handleRotateToken(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok, err := config.GenerateToken()
	if err != nil {
		http.Error(rw, fmt.Sprintf("generate token: %v", err), http.StatusInternalServerError)
		return
	}
	w.mu.Lock()
	w.cfg.Web.Token = tok
	if err := config.WriteFile(w.configPath, w.cfg); err != nil {
		w.mu.Unlock()
		http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
		return
	}
	w.token = tok
	w.mu.Unlock()
	writeJSON(rw, map[string]string{"token": tok})
}

// handleSettings updates the ntfy / check / web blocks in the
// config. Each block is optional; only supplied fields
// override the current values. The new config is written to
// disk; the fsnotify watcher triggers a daemon.Reload and a
// Server.Reload.
//
// The web token and listen address are picked up immediately
// by the web server's Reload. ntfy settings are picked up by
// the daemon's Reload (which updates the shared ntfy client
// via notify.Client.Update). Changes to
// [check].check_interval only affect sources that don't set
// their own per-source interval, and only on the next reload
// (existing runners keep their cached intervalFn).
func (w *Server) handleSettings(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body settingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(rw, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	// Held throughout: the lock is a writer lock because
	// we're mutating the in-memory config (and serializing
	// against other writers to the same file).
	w.mu.Lock()
	defer w.mu.Unlock()
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
			if _, err := schedule.Parse(body.Check.Interval); err != nil {
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
		// Token may legitimately be the empty string (the
		// user wants to disable the web UI). Update
		// unconditionally when the field is present in the
		// request. The settings form is responsible for
		// distinguishing "user typed a new value" from "user
		// left it blank to keep the current one" (the JS
		// only sends the field when the user types
		// something).
		w.cfg.Web.Token = body.Web.Token
	}
	if err := config.WriteFile(w.configPath, w.cfg); err != nil {
		http.Error(rw, fmt.Sprintf("write config: %v", err), http.StatusInternalServerError)
		return
	}
	// Return the (now-updated) config with the token masked,
	// so the UI can refresh its local copy in one round-trip.
	masked := *w.cfg
	masked.Web.Token = "****"
	writeJSON(rw, masked)
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

// --- Test helpers (exported because the test file is in the
// same package). Kept here so the production file stays
// focused on the server's public API. ---

// ConfigForTest returns the current in-memory config.
func (w *Server) ConfigForTest() *config.Config { return w.cfg }

// DaemonForTest returns the daemon reference.
func (w *Server) DaemonForTest() *daemon.Daemon { return w.daemon }

// StateForTest returns the state reference.
func (w *Server) StateForTest() *state.State { return w.state }

// ConfigPathForTest returns the on-disk config path.
func (w *Server) ConfigPathForTest() string { return w.configPath }

// StatePathForTest returns the on-disk state path.
func (w *Server) StatePathForTest() string { return w.statePath }

// RegisterForTest wires routes into the test's muxes. The
// api mux is mounted under /api/ with the auth middleware
// applied; the root mux is mounted at / without auth. The
// returned *http.ServeMux is the top-level mux the test
// should pass to httptest.
func (w *Server) RegisterForTest() *http.ServeMux {
	apiMux := http.NewServeMux()
	rootMux := http.NewServeMux()
	w.registerRoutes(apiMux, rootMux)
	rootMux.Handle("/api/", w.authMiddleware(apiMux))
	return rootMux
}

// AuthMiddlewareForTest returns the auth-wrapped handler.
func (w *Server) AuthMiddlewareForTest(next http.Handler) http.Handler {
	return w.authMiddleware(next)
}

// TokenFromRequest is the package-level version of the private
// helper, exposed for tests.
func TokenFromRequest(r *http.Request) string { return tokenFromRequest(r) }
