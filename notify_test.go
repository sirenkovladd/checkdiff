package main

import (
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

	title, body, priority, tags := formatNotification(s, added, nil)

	if !strings.Contains(title, "changed") {
		t.Errorf("github_file title should say 'changed', got %q", title)
	}
	if !strings.Contains(body, "File changed") {
		t.Errorf("github_file body should start with 'File changed', got %q", body)
	}
	if !strings.Contains(body, "packages/.../index.tsx") {
		t.Errorf("github_file body should show file path, got %q", body)
	}
	if priority != "default" {
		t.Errorf("expected default priority, got %q", priority)
	}
	if tags != "loudspeaker" {
		t.Errorf("expected loudspeaker tag, got %q", tags)
	}
}

func TestFormatNotificationHTMLAddedAndRemoved(t *testing.T) {
	s := &Source{ID: "aa", Name: "Artificial Analysis", Type: "html", URL: "https://example.com/changelog"}
	added := []Item{
		mkItem("New Model A", "New Model A"),
		mkItem("New Model B", "New Model B"),
	}
	removed := []Item{mkItem("Old Model X", "Old Model X")}

	title, body, priority, tags := formatNotification(s, added, removed)

	if !strings.Contains(title, "2 added, 1 removed") {
		t.Errorf("title should show '2 added, 1 removed', got %q", title)
	}
	if !strings.Contains(body, "Added:") {
		t.Errorf("body should have 'Added:' section, got %q", body)
	}
	if !strings.Contains(body, "Removed:") {
		t.Errorf("body should have 'Removed:' section, got %q", body)
	}
	if !strings.Contains(body, "New Model A") {
		t.Errorf("body should list New Model A, got %q", body)
	}
	if !strings.Contains(body, "Old Model X") {
		t.Errorf("body should list Old Model X, got %q", body)
	}
	if priority != "default" {
		t.Errorf("expected default priority for small diff, got %q", priority)
	}
	if tags != "loudspeaker" {
		t.Errorf("expected loudspeaker tag, got %q", tags)
	}
}

func TestFormatNotificationHTMLHighPriority(t *testing.T) {
	s := &Source{ID: "aa", Name: "AA", Type: "html", URL: "https://example.com"}
	added := []Item{
		mkItem("a", "a"), mkItem("b", "b"), mkItem("c", "c"),
		mkItem("d", "d"), mkItem("e", "e"), mkItem("f", "f"),
	}
	_, _, priority, _ := formatNotification(s, added, nil)
	if priority != "high" {
		t.Errorf("expected high priority for 6+ changes, got %q", priority)
	}
}

func TestFormatNotificationOnlyRemoved(t *testing.T) {
	s := &Source{ID: "or", Name: "OpenRouter", Type: "json", URL: "https://example.com"}
	removed := []Item{mkItem("old/model", "old/model")}

	title, body, _, _ := formatNotification(s, nil, removed)

	if !strings.Contains(title, "0 added, 1 removed") {
		t.Errorf("title should show '0 added, 1 removed', got %q", title)
	}
	if !strings.Contains(body, "1 removed") {
		t.Errorf("body should mention '1 removed', got %q", body)
	}
	if strings.Contains(body, "Added:") {
		t.Errorf("body should NOT have 'Added:' section when nothing was added, got %q", body)
	}
}

func TestFormatNotificationNoChanges(t *testing.T) {
	s := &Source{ID: "x", Name: "X", Type: "html", URL: "https://example.com"}
	title, body, priority, _ := formatNotification(s, nil, nil)
	if title != "X" || body != "(no changes)" || priority != "low" {
		t.Errorf("expected no-op, got title=%q body=%q priority=%q", title, body, priority)
	}
}

func TestFormatNotificationItemLink(t *testing.T) {
	// Items with a Link should be rendered as markdown links in the
	// body so tapping them in the ntfy app/web opens that URL
	// directly. This is the mechanism behind per-item URLs like
	// package tracking links (uniuni-package, etc.).
	s := &Source{ID: "uniuni", Name: "uniuni-package", Type: "json", URL: "https://api.uniuni.example"}
	trackingURL := "https://www.uniuni.com/tracking/#tracking-detail?no=U000180542908940"
	added := []Item{
		{ID: "U000180542908940", Title: "U000180542908940", Link: trackingURL},
	}
	removed := []Item{
		{ID: "old", Title: "old"},
	}

	_, body, _, _ := formatNotification(s, added, removed)

	want := "[U000180542908940](" + trackingURL + ")"
	if !strings.Contains(body, want) {
		t.Errorf("body should render added item as markdown link %q, got:\n%s", want, body)
	}
	// Removed items don't carry a Link (state only stores IDs, so
	// removed items are reconstructed with just ID+Title) — they
	// should still render as plain text.
	if strings.Contains(body, "[old](http") {
		t.Errorf("removed item should not be a link, got body:\n%s", body)
	}
	if !strings.Contains(body, "old") {
		t.Errorf("removed item title should still appear, got body:\n%s", body)
	}
}

func TestFormatNotificationItemWithoutLink(t *testing.T) {
	// The plain-text path must be preserved exactly for items that
	// don't carry a Link (html items, json items from sources
	// without link_field, etc.). Otherwise existing notifications
	// change format unexpectedly.
	s := &Source{ID: "x", Name: "X", Type: "json", URL: "https://example.com"}
	added := []Item{{ID: "a", Title: "Alpha"}}
	_, body, _, _ := formatNotification(s, added, nil)
	if !strings.Contains(body, "  • Alpha\n") {
		t.Errorf("body should render unlinked item as plain text, got:\n%s", body)
	}
}

func TestIsLegacyHTMLID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"txt:0123456789abcdef", true},
		{"txt:fedcba9876543210", true},
		{"Claude Opus 4.7", false},
		{"txt:tooshort", false},
		{"txt:ZZZZZZZZZZZZZZZZ", false}, // non-hex
		{"abc123def456", false},
		{"", false},
		{"txt:", false},
	}
	for _, c := range cases {
		if got := isLegacyHTMLID(c.in); got != c.want {
			t.Errorf("isLegacyHTMLID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
