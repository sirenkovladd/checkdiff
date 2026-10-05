package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAPIKeyEnv(t *testing.T) {
	t.Setenv("CHECKDIFF_TEST_LLM_KEY", "  secret-123  ")
	if got := ResolveAPIKey("CHECKDIFF_TEST_LLM_KEY", "", ""); got != "secret-123" {
		t.Errorf("ResolveAPIKey(env) = %q, want %q", got, "secret-123")
	}
}

func TestResolveAPIKeyMissing(t *testing.T) {
	if got := ResolveAPIKey("CHECKDIFF_TEST_LLM_KEY_DEFINITELY_UNSET", "", ""); got != "" {
		t.Errorf("ResolveAPIKey(missing) = %q, want empty", got)
	}
	if got := ResolveAPIKey("", "", ""); got != "" {
		t.Errorf("ResolveAPIKey(empty) = %q, want empty", got)
	}
}

func TestResolveAPIKeyRawFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("  file-key-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAPIKey("", path, ""); got != "file-key-456" {
		t.Errorf("ResolveAPIKey(file) = %q, want %q", got, "file-key-456")
	}
}

func TestResolveAPIKeyJSONFile(t *testing.T) {
	for _, body := range []string{
		`{"api_key": "json-key-1"}`,
		`{"apiKey": "json-key-2"}`,
		`{"key": "json-key-3"}`,
		`{"token": "json-key-4"}`,
	} {
		path := filepath.Join(t.TempDir(), "key.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ResolveAPIKey("", path, ""); got == "" {
			t.Errorf("ResolveAPIKey(%s) = empty, want non-empty", body)
		}
	}
}

func TestResolveAPIKeyEnvWinsOverFile(t *testing.T) {
	t.Setenv("CHECKDIFF_TEST_LLM_KEY2", "env-wins")
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("file-loses"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAPIKey("CHECKDIFF_TEST_LLM_KEY2", path, ""); got != "env-wins" {
		t.Errorf("ResolveAPIKey = %q, want %q", got, "env-wins")
	}
}

func TestSummarizeOpenAI(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotModel = req.Model
		if len(req.Messages) != 2 {
			t.Errorf("messages = %d, want 2 (system+user)", len(req.Messages))
		}
		_ = json.NewEncoder(rw).Encode(openAIResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "Campaign went live with early-bird pricing."}}},
		})
	}))
	defer srv.Close()

	c := New("openai", srv.URL, "test-model", "k", 300)
	out, err := c.Summarize(context.Background(), "Openprinter", "https://example.com", "launching soon", "now live", "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "Campaign went live with early-bird pricing." {
		t.Errorf("Summarize = %q", out)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer k")
	}
	if gotModel != "test-model" {
		t.Errorf("model = %q, want test-model", gotModel)
	}
}

func TestSummarizeOpenAIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("openai", srv.URL, "m", "bad", 300)
	if _, err := c.Summarize(context.Background(), "n", "u", "old", "new", ""); err == nil {
		t.Errorf("Summarize on 401: got nil error, want error")
	}
}

func TestSummarizeAnthropic(t *testing.T) {
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Errorf("path = %q, want /messages", r.URL.Path)
		}
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		var req anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.System == "" {
			t.Errorf("system prompt empty")
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "Price dropped to $99."}},
		})
	}))
	defer srv.Close()

	c := New("anthropic", srv.URL, "qwen3.8-flash", "ak", 300)
	out, err := c.Summarize(context.Background(), "n", "u", "old", "new", "focus on pricing")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "Price dropped to $99." {
		t.Errorf("Summarize = %q", out)
	}
	if gotKey != "ak" {
		t.Errorf("x-api-key = %q, want ak", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
}

func TestSummarizeNoModel(t *testing.T) {
	c := New("openai", "https://example.com", "", "", 300)
	if _, err := c.Summarize(context.Background(), "n", "u", "old", "new", ""); err == nil {
		t.Errorf("Summarize with no model: got nil error, want error")
	}
}

func TestResolveAPIKeyDottedPath(t *testing.T) {
	body := `{"opencode-go": {"type": "api_key", "key": "nested-key-789"}, "other": {"key": "x"}}`
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAPIKey("", path, "opencode-go.key"); got != "nested-key-789" {
		t.Errorf("ResolveAPIKey(path) = %q, want %q", got, "nested-key-789")
	}
	// Missing segment → empty.
	if got := ResolveAPIKey("", path, "opencode-go.missing"); got != "" {
		t.Errorf("ResolveAPIKey(bad path) = %q, want empty", got)
	}
	// Non-JSON file with a path → empty.
	raw := filepath.Join(t.TempDir(), "raw")
	if err := os.WriteFile(raw, []byte("rawkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAPIKey("", raw, "opencode-go.key"); got != "" {
		t.Errorf("ResolveAPIKey(raw+path) = %q, want empty", got)
	}
	// Env still wins over file+path.
	t.Setenv("CHECKDIFF_TEST_LLM_KEY3", "env-wins")
	if got := ResolveAPIKey("CHECKDIFF_TEST_LLM_KEY3", path, "opencode-go.key"); got != "env-wins" {
		t.Errorf("ResolveAPIKey = %q, want %q", got, "env-wins")
	}
}

func TestSummarizeResponses(t *testing.T) {
	var gotAuth, gotModel, gotInstructions string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %q, want /responses", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotModel = req.Model
		gotInstructions = req.Instructions
		if req.Input == "" {
			t.Errorf("input empty")
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"output": []any{
				map[string]any{"type": "reasoning", "content": []any{}},
				map[string]any{"type": "message", "content": []any{
					map[string]string{"type": "output_text", "text": "Campaign went live."},
				}},
			},
		})
	}))
	defer srv.Close()

	c := New("responses", srv.URL, "muse-spark-1.3-contributor", "k", 300)
	out, err := c.Summarize(context.Background(), "n", "u", "old", "new", "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "Campaign went live." {
		t.Errorf("Summarize = %q", out)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer k")
	}
	if gotModel != "muse-spark-1.3-contributor" {
		t.Errorf("model = %q", gotModel)
	}
	if gotInstructions == "" {
		t.Errorf("instructions empty, want system prompt")
	}
}

func TestSummarizeResponsesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New("responses", srv.URL, "m", "k", 300)
	if _, err := c.Summarize(context.Background(), "n", "u", "old", "new", ""); err == nil {
		t.Errorf("Summarize on 404: got nil error, want error")
	}
}

func TestSessionHeaderSent(t *testing.T) {
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		_ = json.NewEncoder(rw).Encode(openAIResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := New("openai", srv.URL, "m", "k", 300)
	if _, err := c.Summarize(context.Background(), "src", "https://example.com/p", "old", "new", ""); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if gotSession == "" {
		t.Fatalf("x-opencode-session missing")
	}
	// Stable across calls for the same source.
	c2 := New("openai", srv.URL, "m", "k", 300)
	var gotSession2 string
	srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotSession2 = r.Header.Get("x-opencode-session")
		_ = json.NewEncoder(rw).Encode(openAIResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "ok"}}},
		})
	})
	if _, err := c2.Summarize(context.Background(), "src", "https://example.com/p", "old", "new", ""); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if gotSession != gotSession2 {
		t.Errorf("session not stable: %q vs %q", gotSession, gotSession2)
	}
}

func TestResponsesBudgetFloor(t *testing.T) {
	if got := responsesBudget(0); got != 4096 {
		t.Errorf("responsesBudget(0) = %d, want 4096", got)
	}
	if got := responsesBudget(300); got != 4096 {
		t.Errorf("responsesBudget(300) = %d, want 4096", got)
	}
	if got := responsesBudget(8192); got != 8192 {
		t.Errorf("responsesBudget(8192) = %d, want 8192", got)
	}
}
