package source

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mkItem(id, title string) Item {
	return Item{ID: id, Title: title}
}

func TestFormatNotificationGitHubFile(t *testing.T) {
	s := &Source{ID: "opencode", Name: "opencode go route", Type: "github_file", URL: "https://example.com"}
	added := []Item{mkItem("newsha123", "packages/.../index.tsx @ dev (1234 bytes)")}
	added[0].Body = "export const foo = 1;"

	n := githubFileFetcher{}.Format(context.Background(), s, added, nil)

	if !strings.Contains(n.Title, "changed") {
		t.Errorf("github_file title should say 'changed', got %q", n.Title)
	}
	if !strings.Contains(n.Body, "File changed") {
		t.Errorf("github_file body should start with 'File changed', got %q", n.Body)
	}
	if !strings.Contains(n.Body, "packages/.../index.tsx") {
		t.Errorf("github_file body should show file path, got %q", n.Body)
	}
	if n.Priority != "default" {
		t.Errorf("expected default priority, got %q", n.Priority)
	}
	if n.Tags != "loudspeaker" {
		t.Errorf("expected loudspeaker tag, got %q", n.Tags)
	}
}

func TestFormatNotificationHTMLAddedAndRemoved(t *testing.T) {
	s := &Source{ID: "aa", Name: "Artificial Analysis", Type: "html", URL: "https://example.com/changelog"}
	added := []Item{
		mkItem("New Model A", "New Model A"),
		mkItem("New Model B", "New Model B"),
	}
	removed := []Item{mkItem("Old Model X", "Old Model X")}

	n := htmlFetcher{}.Format(context.Background(), s, added, removed)

	if !strings.Contains(n.Title, "2 added, 1 removed") {
		t.Errorf("title should show '2 added, 1 removed', got %q", n.Title)
	}
	if !strings.Contains(n.Body, "Added:") {
		t.Errorf("body should have 'Added:' section, got %q", n.Body)
	}
	if !strings.Contains(n.Body, "Removed:") {
		t.Errorf("body should have 'Removed:' section, got %q", n.Body)
	}
	if !strings.Contains(n.Body, "New Model A") {
		t.Errorf("body should list New Model A, got %q", n.Body)
	}
	if !strings.Contains(n.Body, "Old Model X") {
		t.Errorf("body should list Old Model X, got %q", n.Body)
	}
	if n.Priority != "default" {
		t.Errorf("expected default priority for small diff, got %q", n.Priority)
	}
	if n.Tags != "loudspeaker" {
		t.Errorf("expected loudspeaker tag, got %q", n.Tags)
	}
}

func TestFormatNotificationHTMLHighPriority(t *testing.T) {
	s := &Source{ID: "aa", Name: "AA", Type: "html", URL: "https://example.com"}
	added := []Item{
		mkItem("a", "a"), mkItem("b", "b"), mkItem("c", "c"),
		mkItem("d", "d"), mkItem("e", "e"), mkItem("f", "f"),
	}
	n := htmlFetcher{}.Format(context.Background(), s, added, nil)
	if n.Priority != "high" {
		t.Errorf("expected high priority for 6+ changes, got %q", n.Priority)
	}
}

func TestFormatNotificationOnlyRemoved(t *testing.T) {
	s := &Source{ID: "or", Name: "OpenRouter", Type: "json", URL: "https://example.com"}
	removed := []Item{mkItem("old/model", "old/model")}

	n := jsonFetcher{}.Format(context.Background(), s, nil, removed)

	if !strings.Contains(n.Title, "0 added, 1 removed") {
		t.Errorf("title should show '0 added, 1 removed', got %q", n.Title)
	}
	if !strings.Contains(n.Body, "1 removed") {
		t.Errorf("body should mention '1 removed', got %q", n.Body)
	}
	if strings.Contains(n.Body, "Added:") {
		t.Errorf("body should NOT have 'Added:' section when nothing was added, got %q", n.Body)
	}
}

func TestFormatNotificationNoChanges(t *testing.T) {
	// In the new design, check.One never calls Format with
	// (nil, nil) — it returns early when there are no changes.
	// The formatListDiff helper still produces a coherent
	// body for completeness, so we just assert it doesn't
	// panic and produces a non-empty notification.
	s := &Source{ID: "x", Name: "X", Type: "html", URL: "https://example.com"}
	n := htmlFetcher{}.Format(context.Background(), s, nil, nil)
	if n.Title == "" {
		t.Errorf("Format with no changes: Title should be non-empty, got %q", n.Title)
	}
	if n.Body == "" {
		t.Errorf("Format with no changes: Body should be non-empty, got %q", n.Body)
	}
}

func TestFormatNotificationItemLink(t *testing.T) {
	// Items with a Link should be rendered as markdown links in
	// the body so tapping them in the ntfy app/web opens that
	// URL directly. This is the mechanism behind per-item URLs
	// like package tracking links (uniuni-package, etc.).
	s := &Source{ID: "uniuni", Name: "uniuni-package", Type: "json", URL: "https://api.uniuni.example"}
	trackingURL := "https://www.uniuni.com/tracking/#tracking-detail?no=U000180542908940"
	added := []Item{
		{ID: "U000180542908940", Title: "U000180542908940", Link: trackingURL},
	}
	removed := []Item{
		{ID: "old", Title: "old"},
	}

	n := jsonFetcher{}.Format(context.Background(), s, added, removed)

	want := "[U000180542908940](" + trackingURL + ")"
	if !strings.Contains(n.Body, want) {
		t.Errorf("body should render added item as markdown link %q, got:\n%s", want, n.Body)
	}
	// Removed items don't carry a Link (state only stores IDs,
	// so removed items are reconstructed with just ID+Title) —
	// they should still render as plain text.
	if strings.Contains(n.Body, "[old](http") {
		t.Errorf("removed item should not be a link, got body:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "old") {
		t.Errorf("removed item title should still appear, got body:\n%s", n.Body)
	}
}

func TestFormatNotificationItemWithoutLink(t *testing.T) {
	// The plain-text path must be preserved exactly for items
	// that don't carry a Link (html items, json items from
	// sources without link_field, etc.). Otherwise existing
	// notifications change format unexpectedly.
	s := &Source{ID: "x", Name: "X", Type: "json", URL: "https://example.com"}
	added := []Item{{ID: "a", Title: "Alpha"}}
	n := jsonFetcher{}.Format(context.Background(), s, added, nil)
	if !strings.Contains(n.Body, "  • Alpha\n") {
		t.Errorf("body should render unlinked item as plain text, got:\n%s", n.Body)
	}
}

func TestJsonValueFormatChange(t *testing.T) {
	// The json_value fetcher's format must surface the old
	// and new values so the user can see exactly what
	// changed. check.One produces a one-removed + one-added
	// diff for single-value sources, and the format helper
	// reads both to build the "from X to Y" body.
	s := &Source{ID: "active", Name: "Activity 594922", Type: "json_value", URL: "https://example.com"}
	removed := []Item{{ID: "We're sorry, but this Activity is full.", Title: "We're sorry, but this Activity is full."}}
	added := []Item{{ID: "Open", Title: "Open"}}
	n := jsonValueFetcher{}.Format(context.Background(), s, added, removed)
	if !strings.Contains(n.Title, "Activity 594922") || !strings.Contains(n.Title, "changed") {
		t.Errorf("title should name the source and say changed, got %q", n.Title)
	}
	if !strings.Contains(n.Body, "We're sorry") {
		t.Errorf("body should show old value, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "Open") {
		t.Errorf("body should show new value, got:\n%s", n.Body)
	}
	if n.Click != s.URL {
		t.Errorf("Click = %q, want %q", n.Click, s.URL)
	}
}

func TestFormatNotificationGitHubFileIncludesCommit(t *testing.T) {
	// The github_file Format must look up the latest commit
	// via gh and include a 'Last commit: <sha> — <msg>' line
	// plus the full commit URL. The Click header must point
	// at the commit URL too, so tapping the notification
	// jumps straight to the diff.
	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell script")
	}
	script := `#!/bin/sh
cat <<'JSON'
[{"sha":"10b6672be1dae17267e85bffb1c14992a60c19c8","html_url":"https://github.com/anomalyco/opencode/commit/10b6672be1dae17267e85bffb1c14992a60c19c8","commit":{"message":"go: glm 5.2\n\nmore body lines","author":{"name":"Frank","date":"2026-06-17T11:04:33Z"}}}]
JSON
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	SetGhPath(ghPath)
	defer SetGhPath("")

	s := &Source{
		ID:    "opencode-go-docs",
		Name:  "opencode go.mdx",
		Type:  "github_file",
		Owner: "anomalyco",
		Repo:  "opencode",
		Ref:   "dev",
		Path:  "packages/web/src/content/docs/go.mdx",
		URL:   "https://github.com/anomalyco/opencode/blob/dev/packages/web/src/content/docs/go.mdx",
	}
	added := []Item{{ID: "newsha", Title: "packages/web/src/content/docs/go.mdx @ dev (10 bytes)"}}

	n := githubFileFetcher{}.Format(context.Background(), s, added, nil)

	if !strings.Contains(n.Body, "Last commit: 10b6672") {
		t.Errorf("body should contain short SHA, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "go: glm 5.2") {
		t.Errorf("body should contain commit message first line, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "Frank") {
		t.Errorf("body should contain author, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "2026-06-17") {
		t.Errorf("body should contain date, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "https://github.com/anomalyco/opencode/commit/10b6672") {
		t.Errorf("body should contain full commit URL, got:\n%s", n.Body)
	}
	if n.Click != "https://github.com/anomalyco/opencode/commit/10b6672be1dae17267e85bffb1c14992a60c19c8" {
		t.Errorf("Click = %q, want commit URL", n.Click)
	}
}

func TestJsonValueValidate(t *testing.T) {
	// url and path are required; type defaults to "json" if
	// missing (an existing source config) so the user's
	// migrated entry doesn't accidentally start as
	// json_value with no type.
	v := jsonValueFetcher{}
	if err := v.Validate(&Source{Type: "json_value", URL: "https://x", Path: "a.b"}); err != nil {
		t.Errorf("validate ok case: %v", err)
	}
	if err := v.Validate(&Source{Type: "json_value", Path: "a.b"}); err == nil {
		t.Errorf("missing url: got nil error, want error")
	}
	if err := v.Validate(&Source{Type: "json_value", URL: "https://x"}); err == nil {
		t.Errorf("missing path: got nil error, want error")
	}
}
