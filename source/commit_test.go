package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGetLatestCommitSuccess drives GetLatestCommit against a
// fake `gh` binary that echoes a canned GitHub response, then
// asserts the parsed commit has the right SHA, message,
// author, date, and html_url. The fake-gh trick lets the
// test exercise the real exec/gh code path without hitting
// the network or requiring the gh CLI in CI.
func TestGetLatestCommitSuccess(t *testing.T) {
	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	// The fake gh prints a single-element array of commits
	// (the shape `gh api .../commits` returns) then exits 0.
	// The argv check ensures we only stub for the expected
	// subcommand and would catch accidental over-stubbing.
	script := `#!/bin/sh
if [ "$1" != "api" ]; then echo "unexpected arg: $1" >&2; exit 2; fi
cat <<'JSON'
[{"sha":"abc1234567890","html_url":"https://github.com/anomalyco/opencode/commit/abc1234567890","commit":{"message":"fix: type dropdown","author":{"name":"sirenko","date":"2026-06-27T08:15:00Z"}}}]
JSON
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell script")
	}
	SetGhPath(ghPath)
	defer SetGhPath("")

	s := &Source{
		Type:  "github_file",
		Owner: "anomalyco",
		Repo:  "opencode",
		Ref:   "dev",
		Path:  "packages/web/index.html",
	}
	c, err := GetLatestCommit(context.Background(), s)
	if err != nil {
		t.Fatalf("GetLatestCommit: %v", err)
	}
	if c.SHA != "abc1234567890" {
		t.Errorf("SHA = %q, want abc1234567890", c.SHA)
	}
	if c.HTMLURL != "https://github.com/anomalyco/opencode/commit/abc1234567890" {
		t.Errorf("HTMLURL = %q", c.HTMLURL)
	}
	if c.Commit.Message != "fix: type dropdown" {
		t.Errorf("Message = %q", c.Commit.Message)
	}
	if c.Commit.Author.Name != "sirenko" {
		t.Errorf("Author.Name = %q", c.Commit.Author.Name)
	}
	if c.Commit.Author.Date != "2026-06-27T08:15:00Z" {
		t.Errorf("Author.Date = %q", c.Commit.Author.Date)
	}
}

func TestGetLatestCommitRejectsNonGithubFile(t *testing.T) {
	s := &Source{Type: "json", URL: "https://example.com"}
	_, err := GetLatestCommit(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "not github_file") {
		t.Errorf("expected type-rejection error, got %v", err)
	}
}

func TestGetLatestCommitEmptyArray(t *testing.T) {
	// gh returning an empty array (e.g. wrong path) must
	// surface as an error, not an empty commit.
	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	script := `#!/bin/sh
echo '[]'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell script")
	}
	SetGhPath(ghPath)
	defer SetGhPath("")

	s := &Source{Type: "github_file", Owner: "x", Repo: "y", Ref: "main", Path: "no-such-file"}
	_, err := GetLatestCommit(context.Background(), s)
	if err == nil {
		t.Errorf("empty array: got nil error, want error")
	}
}

// TestCommitJSONShape pins the field names the web UI reads
// from /api/sources/{id}/content. The fields are nested
// (commit.message, commit.author.name) — a small mistake in
// the JSON tags would cause the UI to show "unknown" for
// every commit.
func TestCommitJSONShape(t *testing.T) {
	c := Commit{
		SHA:     "abc",
		HTMLURL: "https://example.com/commit/abc",
		Commit: struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		}{
			Message: "msg",
			Author: struct {
				Name string `json:"name"`
				Date string `json:"date"`
			}{Name: "alice", Date: "2026-01-01T00:00:00Z"},
		},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["sha"] != "abc" {
		t.Errorf("sha = %v", got["sha"])
	}
	commit, ok := got["commit"].(map[string]interface{})
	if !ok {
		t.Fatalf("commit object missing")
	}
	if commit["message"] != "msg" {
		t.Errorf("commit.message = %v", commit["message"])
	}
	author := commit["author"].(map[string]interface{})
	if author["name"] != "alice" || author["date"] != "2026-01-01T00:00:00Z" {
		t.Errorf("commit.author = %v", author)
	}
}

// keep the import live for the httptest-only test below
var _ = httptest.NewRecorder
var _ = http.StatusOK
