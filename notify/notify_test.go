package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"checkdiff/source"
)

func TestClientUpdateNormalisesTrailingSlash(t *testing.T) {
	// Update must apply the same trailing-slash normalisation as
	// New, so a settings change through the web UI doesn't leave
	// a stray slash that produces "https://ntfy.sh//topic" at
	// publish time.
	c := New("https://ntfy.sh", "old")
	c.Update("https://ntfy.sh/", "new")
	if got := c.Server(); got != "https://ntfy.sh" {
		t.Errorf("after Update: Server = %q, want %q", got, "https://ntfy.sh")
	}
	if got := c.Topic(); got != "new" {
		t.Errorf("after Update: Topic = %q, want %q", got, "new")
	}
}

func TestClientNewStripsTrailingSlash(t *testing.T) {
	c := New("https://ntfy.sh/", "topic")
	if got := c.Server(); got != "https://ntfy.sh" {
		t.Errorf("New: Server = %q, want %q (no trailing slash)", got, "https://ntfy.sh")
	}
}

func TestClientPublishPostsNotification(t *testing.T) {
	// Publish must POST to "{server}/{topic}" with the
	// notification's Title, Priority, Tags, and Click headers
	// set. The test server records the request so we can assert
	// on the wire shape.
	var hits int32
	var lastAuth, lastTitle, lastPriority, lastTags, lastClick, lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		lastAuth = r.Header.Get("Authorization")
		lastTitle = r.Header.Get("Title")
		lastPriority = r.Header.Get("Priority")
		lastTags = r.Header.Get("Tags")
		lastClick = r.Header.Get("Click")
		lastPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, "my-topic")
	err := c.Publish(context.Background(), source.Notification{
		Title:    "Hello",
		Body:     "world",
		Priority: "high",
		Tags:     "loudspeaker",
		Click:    "https://example.com",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("Publish: hits = %d, want 1", hits)
	}
	if lastPath != "/my-topic" {
		t.Errorf("Publish: URL path = %q, want /my-topic", lastPath)
	}
	if lastTitle != "Hello" {
		t.Errorf("Publish: Title header = %q, want Hello", lastTitle)
	}
	if lastPriority != "high" {
		t.Errorf("Publish: Priority header = %q, want high", lastPriority)
	}
	if lastTags != "loudspeaker" {
		t.Errorf("Publish: Tags header = %q, want loudspeaker", lastTags)
	}
	if lastClick != "https://example.com" {
		t.Errorf("Publish: Click header = %q, want https://example.com", lastClick)
	}
	// The Authorization header is intentionally not set by
	// Publish; ntfy.sh uses the URL itself as the auth secret.
	if lastAuth != "" {
		t.Errorf("Publish: Authorization header = %q, want empty (ntfy auth is the URL)", lastAuth)
	}
}
