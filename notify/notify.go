// Package notify publishes notifications to an ntfy.sh-compatible
// server. The Client owns its server URL and topic; callers
// reach the wire only through Publish, and the runtime
// configuration is changed only through Update. There's no
// other writer to Client's fields, so the trailing-slash
// quirk lives in exactly one place (Update, mirroring New).
package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"checkdiff/source"
)

// Client publishes notifications to an ntfy.sh-compatible
// server. The Server and Topic fields are exported for tests
// that want to assert on the current values; production code
// should always go through New + Update so the trailing-slash
// normalisation is applied consistently.
type Client struct {
	server string
	topic  string
	mu     sync.RWMutex
	http   *http.Client
}

// New constructs a Client with the given server and topic. The
// server URL has any trailing slash stripped, matching the
// convention used by Publish when composing the endpoint URL.
func New(server, topic string) *Client {
	return &Client{
		server: strings.TrimRight(server, "/"),
		topic:  topic,
		http:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Update changes the server URL and topic. The server URL is
// normalised the same way as in New (trailing slash stripped),
// so callers don't have to think about the URL shape. The
// daemon's Reload path uses this to pick up settings changes
// without restarting.
func (c *Client) Update(server, topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.server = strings.TrimRight(server, "/")
	c.topic = topic
}

// Server returns the current server URL. Exported for tests
// and the web API; production code should call Publish.
func (c *Client) Server() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.server
}

// Topic returns the current topic. Exported for tests and the
// web API; production code should call Publish.
func (c *Client) Topic() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topic
}

// Publish sends one notification built from a source.Notification.
// The Notification carries the ntfy-specific wire format
// (Title, Priority, Tags, Click); Publish adds the POST
// envelope.
func (c *Client) Publish(ctx context.Context, n source.Notification) error {
	c.mu.RLock()
	server, topic := c.server, c.topic
	c.mu.RUnlock()
	endpoint := fmt.Sprintf("%s/%s", server, url.PathEscape(topic))

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(n.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if n.Title != "" {
		req.Header.Set("Title", n.Title)
	}
	if n.Priority != "" {
		req.Header.Set("Priority", n.Priority)
	}
	if n.Tags != "" {
		req.Header.Set("Tags", n.Tags)
	}
	if n.Click != "" {
		req.Header.Set("Click", n.Click)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("ntfy: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	// Drain to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
