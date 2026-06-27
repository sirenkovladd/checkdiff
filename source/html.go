package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"checkdiff/template"
	"golang.org/x/net/html"
)

// htmlFetcher implements the Fetcher interface for the "html"
// source type: a web page whose elements are matched by a
// CSS-ish selector. Items are tracked by their text content
// directly (not a hash) so additions and removals of individual
// entries are detectable.
type htmlFetcher struct{}

// Type returns the registry key for this fetcher.
func (htmlFetcher) Type() string { return "html" }

// Fetch retrieves the current set of items from a web page by
// walking the parsed DOM and collecting the text content of
// every element that matches the source's selector. The text
// content is the stable identifier.
func (htmlFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := template.Render(s.URL, now)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	sel, err := parseHTMLSelector(s.Selector)
	if err != nil {
		return nil, err
	}

	var titles []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if sel.matches(n) {
			t := extractText(n)
			t = strings.TrimSpace(t)
			if t != "" {
				titles = append(titles, t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(titles) == 0 {
		return nil, errors.New("no matching elements found")
	}

	items := make([]Item, 0, len(titles))
	seen := make(map[string]bool, len(titles))
	for _, t := range titles {
		// Track by the element's text content (not a hash) so
		// additions and removals of individual entries are
		// detectable across runs. Deduplicate so the same text
		// on the page (e.g. a repeated heading) doesn't bloat
		// the state set.
		if seen[t] {
			continue
		}
		seen[t] = true
		items = append(items, Item{
			ID:    t,
			Title: t,
		})
	}
	return items, nil
}

// Validate applies the html-specific defaults: URL is required,
// Selector defaults to "h3" (the most common changelog heading
// level), and the selector must parse.
func (htmlFetcher) Validate(s *Source) error {
	if s.URL == "" {
		return fmt.Errorf("html requires url")
	}
	if s.Selector == "" {
		s.Selector = "h3"
	}
	if _, err := parseHTMLSelector(s.Selector); err != nil {
		return fmt.Errorf("html: unsupported selector %q (use a tag name like h3, or 'tag.class' such as li.attachedfile)", s.Selector)
	}
	return nil
}

// Format builds an "added/removed" notification for the diff.
// The title and body follow the same layout the json fetcher
// uses, so the user sees a consistent format for list-style
// sources.
func (htmlFetcher) Format(s *Source, added, removed []Item) Notification {
	return formatListDiff(s, added, removed, s.URL)
}
