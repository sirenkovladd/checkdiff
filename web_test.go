package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestWebServer builds a webServer wired to a minimal in-memory
// daemon and state. The returned server is not started; tests use
// the underlying handler directly via httptest.
func newTestWebServer(t *testing.T, token string) *webServer {
	t.Helper()
	cfg := &Config{
		Ntfy:  NtfyConfig{Topic: "test"},
		Check: CheckConfig{Interval: "1h"},
		Web:   WebConfig{Listen: "127.0.0.1:0", Token: token},
		Sources: []Source{
			{ID: "alpha", Name: "alpha", Type: "json", URL: "https://example.com/a"},
			{ID: "beta", Name: "beta", Type: "html", URL: "https://example.com/b"},
		},
	}
	d := newDaemon(cfg, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{},
	}, NewNtfyClient("https://ntfy.sh", "test"))
	return newWebServer(cfg, d, &State{
		Version: currentStateVersion,
		Sources: map[string]*SourceState{
			"alpha": {ItemsSeen: map[string]bool{"x": true}, ItemsCount: 1},
		},
	})
}

// callAuth is a test helper that hits a path on the auth-wrapped
// mux with the given token (empty string means no auth).
func callAuth(ws *webServer, method, path, token, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	ws.registerRoutes(mux)
	handler := ws.authMiddleware(mux)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	return rw
}

func TestWebAuthRequiresToken(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/state", "", "")
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rw.Code)
	}
}

func TestWebAuthRejectsWrongToken(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/state", "wrong", "")
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rw.Code)
	}
}

func TestWebAuthAcceptsHeaderToken(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/state", "secret123", "")
	if rw.Code != http.StatusOK {
		t.Errorf("right token via header: got %d, want 200", rw.Code)
	}
}

func TestWebAuthAcceptsQueryToken(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	mux := http.NewServeMux()
	ws.registerRoutes(mux)
	handler := ws.authMiddleware(mux)
	req := httptest.NewRequest("GET", "/api/state?token=secret123", nil)
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("right token via query: got %d, want 200", rw.Code)
	}
}

func TestWebAuthMissingBearerPrefix(t *testing.T) {
	// A bare token in the Authorization header (no "Bearer " prefix)
	// must be rejected. Otherwise an attacker who can set a header
	// to a known prefix could bypass the bearer semantics.
	ws := newTestWebServer(t, "secret123")
	mux := http.NewServeMux()
	ws.registerRoutes(mux)
	handler := ws.authMiddleware(mux)
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("Authorization", "secret123")
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("missing Bearer prefix: got %d, want 401", rw.Code)
	}
}

func TestWebStateEndpoint(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/state", "secret123", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rw.Code)
	}
	var got map[string]*SourceState
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["alpha"]; !ok {
		t.Errorf("state response missing 'alpha' source")
	}
}

func TestWebConfigEndpointMasksToken(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/config", "secret123", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rw.Code)
	}
	var got Config
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Web.Token != "****" {
		t.Errorf("Web.Token = %q, want %q (must be masked in /api/config response)", got.Web.Token, "****")
	}
}

func TestWebSourcesGET(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "GET", "/api/sources", "secret123", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rw.Code)
	}
	var got []Source
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d sources, want 2", len(got))
	}
}

func TestWebSourcesPOST(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	body := `{"id":"gamma","name":"gamma","type":"json","url":"https://example.com/g"}`
	rw := callAuth(ws, "POST", "/api/sources", "secret123", body)
	if rw.Code != http.StatusCreated {
		t.Errorf("POST: got %d, want 201", rw.Code)
	}
	if got := len(ws.cfg.Sources); got != 3 {
		t.Errorf("after POST: got %d sources, want 3", got)
	}
	if ws.cfg.Sources[2].ID != "gamma" {
		t.Errorf("appended source ID = %q, want gamma", ws.cfg.Sources[2].ID)
	}
}

func TestWebSourcePUT(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	body := `{"id":"alpha","name":"renamed","type":"json","url":"https://example.com/a2"}`
	rw := callAuth(ws, "PUT", "/api/sources/alpha", "secret123", body)
	if rw.Code != http.StatusOK {
		t.Errorf("PUT: got %d, want 200", rw.Code)
	}
	for _, s := range ws.cfg.Sources {
		if s.ID == "alpha" && s.Name != "renamed" {
			t.Errorf("PUT did not update: alpha.Name = %q, want renamed", s.Name)
		}
	}
}

func TestWebSourceDELETE(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	rw := callAuth(ws, "DELETE", "/api/sources/alpha", "secret123", "")
	if rw.Code != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", rw.Code)
	}
	if got := len(ws.cfg.Sources); got != 1 {
		t.Errorf("after DELETE: got %d sources, want 1", got)
	}
	for _, s := range ws.cfg.Sources {
		if s.ID == "alpha" {
			t.Errorf("alpha should have been removed")
		}
	}
}

func TestWebStartNoOpsWhenTokenEmpty(t *testing.T) {
	// With an empty token, Start() must not bind a port. The
	// daemon still runs; only the HTTP surface is disabled.
	ws := newTestWebServer(t, "")
	if err := ws.Start(); err != nil {
		t.Errorf("Start with empty token: got %v, want nil", err)
	}
	ws.Stop() // should also be safe
}

func TestTokenFromRequest(t *testing.T) {
	cases := []struct {
		name   string
		header string
		query  string
		want   string
	}{
		{"none", "", "", ""},
		{"bearer header", "Bearer abc123", "", "abc123"},
		{"bearer header with trailing space", "Bearer abc123 ", "", "abc123"},
		{"query takes over when no header", "", "xyz", "xyz"},
		{"header takes precedence over query", "Bearer header1", "query1", "header1"},
		{"header without bearer prefix is ignored", "abc123", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/state", nil)
			if c.query != "" {
				q := r.URL.Query()
				q.Set("token", c.query)
				r.URL.RawQuery = q.Encode()
			}
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			if got := tokenFromRequest(r); got != c.want {
				t.Errorf("tokenFromRequest = %q, want %q", got, c.want)
			}
		})
	}
}

// keep bytes/json imports used so go vet doesn't complain even
// if a future refactor drops the last caller.
var _ = bytes.NewReader
var _ = json.Marshal
