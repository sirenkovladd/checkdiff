package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// webServer is the HTTP surface for the daemon. It serves the JSON
// API and (in a later step) the static web UI. Auth is enforced by
// a middleware that checks a token supplied via the Authorization
// header or a ?token= query parameter.
//
// The server is intentionally minimal: it doesn't hot-reload config,
// doesn't manage its own lifecycle beyond Start/Stop, and doesn't
// embed the web UI yet. Each of those is layered on in later steps.
type webServer struct {
	cfg     *Config
	daemon  *daemon
	state   *State
	token   string
	srv     *http.Server
	mu      sync.Mutex
	running bool
}

func newWebServer(cfg *Config, d *daemon, st *State) *webServer {
	return &webServer{
		cfg:    cfg,
		daemon: d,
		state:  st,
		token:  cfg.Web.Token,
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
		if token == "" || token != w.token {
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
	if w.token == "" {
		log.Printf("web UI disabled: set [web] token in config to enable")
		return nil
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	mux := http.NewServeMux()
	w.registerRoutes(mux)

	w.srv = &http.Server{
		Addr:              w.cfg.Web.Listen,
		Handler:           w.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	w.mu.Lock()
	w.running = true
	w.mu.Unlock()

	log.Printf("web server listening on %s", w.cfg.Web.Listen)
	// ListenAndServe blocks; run it in a goroutine so Start
	// returns immediately. The caller is expected to call Stop
	// on shutdown.
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("web server: %v", err)
		}
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server. It is a no-op if
// the server was never started.
func (w *webServer) Stop() {
	w.mu.Lock()
	srv := w.srv
	w.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// registerRoutes wires the API endpoints into the mux. The web
// UI assets are layered on in a later step.
func (w *webServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/state", w.handleState)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/sources", w.handleSources)
	mux.HandleFunc("/api/sources/", w.handleSourceByID)
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
// the secret). Read-only for now; PUT /api/settings is the
// write path.
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
// source list; POST appends a new source and triggers a daemon
// reload so the new source starts running immediately.
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
		w.cfg.Sources = append(w.cfg.Sources, src)
		w.daemon.Start(context.Background())
		w.writeJSONStatus(rw, http.StatusCreated, src)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSourceByID routes the various /api/sources/{id}[/*] paths.
// It supports:
//   POST   /api/sources/{id}/run  - trigger an immediate check
//   PUT    /api/sources/{id}      - replace this source's config
//   DELETE /api/sources/{id}      - remove this source
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
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				w.cfg.Sources[i] = src
				w.daemon.Start(context.Background())
				writeJSON(rw, src)
				return
			}
		}
		http.NotFound(rw, r)
	case http.MethodDelete:
		for i := range w.cfg.Sources {
			if w.cfg.Sources[i].ID == id {
				w.cfg.Sources = append(w.cfg.Sources[:i], w.cfg.Sources[i+1:]...)
				w.daemon.Start(context.Background())
				rw.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.NotFound(rw, r)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *webServer) writeJSONStatus(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}
