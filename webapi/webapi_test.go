package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"checkdiff/config"
	"checkdiff/daemon"
	"checkdiff/notify"
	"checkdiff/source"
	"checkdiff/state"
)

// newTestWebServer builds a Server wired to a minimal
// in-memory daemon and state. The returned server is not
// started; tests use the underlying handler directly via
// httptest.
func newTestWebServer(t *testing.T, token string) *Server {
	t.Helper()
	cfg := &config.Config{
		Ntfy:  config.NtfyConfig{Topic: "test"},
		Check: config.CheckConfig{Interval: "1h"},
		Web:   config.WebConfig{Listen: "127.0.0.1:0", Token: token},
		Sources: []source.Source{
			{ID: "alpha", Name: "alpha", Type: "json", URL: "https://example.com/a"},
			{ID: "beta", Name: "beta", Type: "html", URL: "https://example.com/b"},
		},
	}
	d := daemon.NewDaemon(cfg, &state.State{
		Version: state.CurrentVersion,
		Sources: map[string]*state.SourceState{},
	}, notify.New("https://ntfy.sh", "test"))
	st := &state.State{
		Version: state.CurrentVersion,
		Sources: map[string]*state.SourceState{
			"alpha": {
				Baseline: &state.Baseline{ItemsSeen: map[string]bool{"x": true}, ItemsCount: 1},
				Record:   &state.Record{},
			},
		},
	}
	dir := t.TempDir()
	return NewServer(cfg, d, st, dir+"/config.toml", dir+"/state.json")
}

// callAuth is a test helper that hits a path on the
// auth-wrapped mux with the given token (empty string means
// no auth). It registers routes on a fresh mux each call;
// concurrent callAuth invocations therefore use independent
// muxes, but the underlying Server's mu serializes the
// in-memory config mutations.
func callAuth(ws *Server, method, path, token, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	ws.RegisterForTest(mux)
	handler := ws.AuthMiddlewareForTest(mux)

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

// The "ws_..." helpers peek into the Server's state via the
// methods it exposes. They are used here so we can share the
// same config / state across multiple auth checks without
// rebuilding from scratch.
func ws_cfg(ws *Server) *config.Config { return ws.ConfigForTest() }
func ws_daemon(ws *Server) *daemon.Daemon { return ws.DaemonForTest() }
func ws_state(ws *Server) *state.State { return ws.StateForTest() }
func ws_configPath(ws *Server) string { return ws.ConfigPathForTest() }
func ws_statePath(ws *Server) string { return ws.StatePathForTest() }

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
	probe := NewServer(ws_cfg(ws), ws_daemon(ws), ws_state(ws), ws_configPath(ws), ws_statePath(ws))
	probe.Reload(ws_cfg(ws))
	mux := http.NewServeMux()
	probe.RegisterForTest(mux)
	handler := probe.AuthMiddlewareForTest(mux)
	req := httptest.NewRequest("GET", "/api/state?token=secret123", nil)
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("right token via query: got %d, want 200", rw.Code)
	}
}

func TestWebAuthMissingBearerPrefix(t *testing.T) {
	// A bare token in the Authorization header (no "Bearer "
	// prefix) must be rejected. Otherwise an attacker who can
	// set a header to a known prefix could bypass the bearer
	// semantics.
	ws := newTestWebServer(t, "secret123")
	probe := NewServer(ws_cfg(ws), ws_daemon(ws), ws_state(ws), ws_configPath(ws), ws_statePath(ws))
	probe.Reload(ws_cfg(ws))
	mux := http.NewServeMux()
	probe.RegisterForTest(mux)
	handler := probe.AuthMiddlewareForTest(mux)
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
	var got map[string]*state.SourceState
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
	var got config.Config
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
	var got []source.Source
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
	cfg := ws_cfg(ws)
	if got := len(cfg.Sources); got != 3 {
		t.Errorf("after POST: got %d sources, want 3", got)
	}
	if cfg.Sources[2].ID != "gamma" {
		t.Errorf("appended source ID = %q, want gamma", cfg.Sources[2].ID)
	}
}

func TestWebSourcePUT(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	body := `{"id":"alpha","name":"renamed","type":"json","url":"https://example.com/a2"}`
	rw := callAuth(ws, "PUT", "/api/sources/alpha", "secret123", body)
	if rw.Code != http.StatusOK {
		t.Errorf("PUT: got %d, want 200", rw.Code)
	}
	for _, s := range ws_cfg(ws).Sources {
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
	if got := len(ws_cfg(ws).Sources); got != 1 {
		t.Errorf("after DELETE: got %d sources, want 1", got)
	}
	for _, s := range ws_cfg(ws).Sources {
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

func TestWebSettingsPUT(t *testing.T) {
	// PUT /api/settings updates the ntfy/check/web blocks and
	// persists the config to disk. Reload picks up the change
	// via the fsnotify watcher; in this unit test we just
	// verify the in-memory and on-disk state.
	ws := newTestWebServer(t, "oldtoken")
	body := `{"ntfy":{"server":"https://ntfy.example.com","topic":"newtopic"},"web":{"token":"newtoken","listen":"127.0.0.1:9090"},"check":{"interval":"30m"}}`
	rw := callAuth(ws, "PUT", "/api/settings", "oldtoken", body)
	if rw.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings: got %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	cfg := ws_cfg(ws)
	if cfg.Ntfy.Server != "https://ntfy.example.com" {
		t.Errorf("Ntfy.Server = %q, want example.com", cfg.Ntfy.Server)
	}
	if cfg.Ntfy.Topic != "newtopic" {
		t.Errorf("Ntfy.Topic = %q, want newtopic", cfg.Ntfy.Topic)
	}
	if cfg.Check.Interval != "30m" {
		t.Errorf("Check.Interval = %q, want 30m", cfg.Check.Interval)
	}
	if cfg.Web.Listen != "127.0.0.1:9090" {
		t.Errorf("Web.Listen = %q, want 127.0.0.1:9090", cfg.Web.Listen)
	}
	// Token in the response is masked.
	var resp config.Config
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Web.Token != "****" {
		t.Errorf("response Web.Token = %q, want masked", resp.Web.Token)
	}
	// On-disk config reflects the change too.
	diskCfg, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load after PUT: %v", err)
	}
	if diskCfg.Ntfy.Topic != "newtopic" {
		t.Errorf("on-disk Ntfy.Topic = %q, want newtopic", diskCfg.Ntfy.Topic)
	}
}

func TestWebRotateToken(t *testing.T) {
	ws := newTestWebServer(t, "oldtoken")
	rw := callAuth(ws, "POST", "/api/rotate-token", "oldtoken", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("rotate: got %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" {
		t.Errorf("rotate: response token is empty")
	}
	if resp.Token == "oldtoken" {
		t.Errorf("rotate: token did not change")
	}
	cfg := ws_cfg(ws)
	if cfg.Web.Token != resp.Token {
		t.Errorf("in-memory token = %q, want %q", cfg.Web.Token, resp.Token)
	}
	rw2 := callAuth(ws, "GET", "/api/state", "oldtoken", "")
	if rw2.Code != http.StatusUnauthorized {
		t.Errorf("old token after rotate: got %d, want 401", rw2.Code)
	}
	rw3 := callAuth(ws, "GET", "/api/state", resp.Token, "")
	if rw3.Code != http.StatusOK {
		t.Errorf("new token after rotate: got %d, want 200", rw3.Code)
	}
	disk, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if disk.Web.Token != resp.Token {
		t.Errorf("on-disk token = %q, want %q", disk.Web.Token, resp.Token)
	}
}

func TestWebSettingsRejectsBadInterval(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	body := `{"check":{"interval":"not-a-duration"}}`
	rw := callAuth(ws, "PUT", "/api/settings", "secret", body)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad interval: got %d, want 400", rw.Code)
	}
}

func TestWebSourcesPOSTPersistsToDisk(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	body := `{"id":"gamma","name":"gamma","type":"json","url":"https://example.com/g"}`
	rw := callAuth(ws, "POST", "/api/sources", "secret", body)
	if rw.Code != http.StatusCreated {
		t.Fatalf("POST: got %d, want 201; body=%s", rw.Code, rw.Body.String())
	}
	diskCfg, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load after POST: %v", err)
	}
	found := false
	for _, s := range diskCfg.Sources {
		if s.ID == "gamma" {
			found = true
		}
	}
	if !found {
		t.Errorf("POST did not persist to disk; on-disk sources: %+v", diskCfg.Sources)
	}
}

func TestWebSourcePUTPersistsToDisk(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	body := `{"id":"alpha","name":"renamed","type":"json","url":"https://example.com/a2"}`
	rw := callAuth(ws, "PUT", "/api/sources/alpha", "secret", body)
	if rw.Code != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200", rw.Code)
	}
	diskCfg, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load after PUT: %v", err)
	}
	for _, s := range diskCfg.Sources {
		if s.ID == "alpha" && s.Name != "renamed" {
			t.Errorf("PUT did not persist; on-disk alpha.Name = %q, want renamed", s.Name)
		}
	}
}

func TestWebSourceDELETEPersistsToDisk(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	rw := callAuth(ws, "DELETE", "/api/sources/alpha", "secret", "")
	if rw.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", rw.Code)
	}
	diskCfg, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load after DELETE: %v", err)
	}
	for _, s := range diskCfg.Sources {
		if s.ID == "alpha" {
			t.Errorf("DELETE did not persist; alpha still on disk")
		}
	}
}

func TestWebSourcesPOSTRejectsDuplicate(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	body := `{"id":"alpha","name":"dup","type":"json","url":"https://example.com/x"}`
	rw := callAuth(ws, "POST", "/api/sources", "secret", body)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("duplicate id: got %d, want 400", rw.Code)
	}
}

func TestWebSourcesPOSTRejectsInvalid(t *testing.T) {
	ws := newTestWebServer(t, "secret")
	body := `{"id":"bad","name":"bad","type":"json"}`
	rw := callAuth(ws, "POST", "/api/sources", "secret", body)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("invalid source: got %d, want 400", rw.Code)
	}
}

func TestWebSourcesConcurrentWrites(t *testing.T) {
	// Multiple concurrent POSTs must not panic and must not
	// lose any source. Without the writeMu serialization, two
	// handlers appending to the same slice can race and one
	// will overwrite the other's addition; or an out-of-range
	// slice operation in a rollback can panic.
	ws := newTestWebServer(t, "secret")
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"src-%d","name":"src %d","type":"json","url":"https://example.com/%d"}`, i, i, i)
			rw := callAuth(ws, "POST", "/api/sources", "secret", body)
			if rw.Code != http.StatusCreated {
				errs <- fmt.Errorf("src-%d: got %d, want 201", i, rw.Code)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	diskCfg, err := config.Load(ws_configPath(ws))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got := len(diskCfg.Sources); got != 2+n {
		t.Errorf("on-disk sources: got %d, want %d (2 initial + %d added)", got, 2+n, n)
	}
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
			if got := TokenFromRequest(r); got != c.want {
				t.Errorf("TokenFromRequest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWebServesStaticAssets(t *testing.T) {
	ws := newTestWebServer(t, "secret123")
	probe := NewServer(ws_cfg(ws), ws_daemon(ws), ws_state(ws), ws_configPath(ws), ws_statePath(ws))
	probe.Reload(ws_cfg(ws))
	mux := http.NewServeMux()
	probe.RegisterForTest(mux)
	handler := probe.AuthMiddlewareForTest(mux)

	for _, path := range []string{"/", "/style.css", "/app.js"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer secret123")
			rw := httptest.NewRecorder()
			handler.ServeHTTP(rw, req)
			if rw.Code != http.StatusOK {
				t.Errorf("GET %s: got %d, want 200", path, rw.Code)
			}
			if rw.Body.Len() == 0 {
				t.Errorf("GET %s: empty body", path)
			}
		})
	}
}

func TestWebStaticAssetsRequireAuth(t *testing.T) {
	// Static assets are gated by the same auth middleware as
	// the API: an unauthenticated request to / must be
	// rejected. Otherwise the login form would be served to
	// anyone who could see the login form's first paint
	// (which leaks nothing today, but the rule should hold
	// for the future).
	ws := newTestWebServer(t, "secret123")
	probe := NewServer(ws_cfg(ws), ws_daemon(ws), ws_state(ws), ws_configPath(ws), ws_statePath(ws))
	probe.Reload(ws_cfg(ws))
	mux := http.NewServeMux()
	probe.RegisterForTest(mux)
	handler := probe.AuthMiddlewareForTest(mux)
	req := httptest.NewRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /: got %d, want 401", rw.Code)
	}
}
