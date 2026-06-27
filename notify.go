package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NtfyClient publishes notifications to an ntfy.sh-compatible server.
type NtfyClient struct {
	Server string
	Topic  string
	HTTP   *http.Client
}

func NewNtfyClient(server, topic string) *NtfyClient {
	return &NtfyClient{
		Server: strings.TrimRight(server, "/"),
		Topic:  topic,
		HTTP:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Publish sends one notification. body may be multi-line. headers
// sets ntfy headers (Title, Priority, Tags, Click, etc.).
func (c *NtfyClient) Publish(ctx context.Context, body string, headers map[string]string) error {
	endpoint := fmt.Sprintf("%s/%s", c.Server, url.PathEscape(c.Topic))

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
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

// formatNotification builds a human-friendly ntfy body for the diff
// of a source. Caller is responsible for filtering down to the items
// that actually changed (added and/or removed).
//
// For github_file sources, `removed` holds the old git blob SHA which
// isn't meaningful — it's ignored and the notification reads as a
// simple "file changed" with the new content excerpt.
//
// For html and json sources, both lists are shown so the user can
// see exactly what appeared and what disappeared.
func formatNotification(s *Source, added, removed []Item) (title, body string, priority string, tags string) {
	if len(added) == 0 && len(removed) == 0 {
		// Defensive: callers shouldn't call with no changes.
		return s.Name, "(no changes)", "low", "repeat"
	}

	// github_file: single item, old SHA is noise. Show only the new
	// file's title and a content excerpt.
	if s.Type == "github_file" {
		priority = "default"
		var b strings.Builder
		fmt.Fprintf(&b, "File changed: %s\n", s.Name)
		fmt.Fprintf(&b, "Source: %s\n\n", s.URL)
		const maxBody = 3500
		for i, it := range added {
			if i >= 10 {
				fmt.Fprintf(&b, "\n… and %d more\n", len(added)-i)
				break
			}
			fmt.Fprintf(&b, "• %s\n", it.Title)
			if it.Body != "" {
				excerpt := it.Body
				if len(excerpt) > 400 {
					excerpt = excerpt[:400] + "…"
				}
				fmt.Fprintf(&b, "  %s\n", excerpt)
			}
			if b.Len() > maxBody {
				b.WriteString("\n…(body truncated)\n")
				break
			}
		}
		title = fmt.Sprintf("🔔 %s: changed", s.Name)
		tags = "loudspeaker"
		return title, b.String(), priority, tags
	}

	// html / json: show added and removed lists.
	total := len(added) + len(removed)
	priority = "default"
	if total > 5 {
		priority = "high"
	}

	var b strings.Builder
	switch {
	case len(added) > 0 && len(removed) > 0:
		fmt.Fprintf(&b, "%d added, %d removed for %s\n", len(added), len(removed), s.Name)
	case len(added) > 0:
		fmt.Fprintf(&b, "%d added for %s\n", len(added), s.Name)
	default:
		fmt.Fprintf(&b, "%d removed for %s\n", len(removed), s.Name)
	}
	fmt.Fprintf(&b, "Source: %s\n\n", s.URL)

	// Cap body so we don't blow past ntfy's 4 KiB default.
	const maxBody = 3500
	const maxListed = 10

	if len(added) > 0 {
		b.WriteString("Added:\n")
		for i, it := range added {
			if i >= maxListed {
				fmt.Fprintf(&b, "  … and %d more\n", len(added)-i)
				break
			}
			fmt.Fprintf(&b, "  • %s\n", formatItemLine(it))
			if b.Len() > maxBody {
				b.WriteString("  …(truncated)\n")
				break
			}
		}
	}
	if len(removed) > 0 {
		if len(added) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Removed:\n")
		for i, it := range removed {
			if i >= maxListed {
				fmt.Fprintf(&b, "  … and %d more\n", len(removed)-i)
				break
			}
			fmt.Fprintf(&b, "  • %s\n", formatItemLine(it))
			if b.Len() > maxBody {
				b.WriteString("  …(truncated)\n")
				break
			}
		}
	}

	title = fmt.Sprintf("🔔 %s: %d added, %d removed", s.Name, len(added), len(removed))
	tags = "loudspeaker"
	return title, b.String(), priority, tags
}

// formatItemLine renders one Item as a single body line. When the
// item carries a Link, the title is wrapped in a ntfy-rendered
// markdown link so tapping it in the ntfy app/web opens that URL
// directly. Items without a Link fall back to plain text — that's
// the common case (html items, github_file items, json items from
// sources that don't set link_field) and matches the previous
// formatting exactly.
func formatItemLine(it Item) string {
	if it.Link == "" {
		return it.Title
	}
	return fmt.Sprintf("[%s](%s)", it.Title, it.Link)
}
