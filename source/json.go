package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"checkdiff/template"
)

// jsonFetcher implements the Fetcher interface for the "json"
// source type: a JSON API whose payload is a document
// containing an array of items at a configurable path. Each
// item's ID is taken from a configurable field (default "id"),
// the title from another (default "name"). Optional
// LinkField attaches a per-item URL.
type jsonFetcher struct{}

// Type returns the registry key for this fetcher.
func (jsonFetcher) Type() string { return "json" }

// Fetch retrieves the current set of items from a JSON API.
// Items are tracked by their configured ID field, so additions
// and removals are detectable across runs.
func (jsonFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := template.Render(s.URL, now)
	if fetchVerbose {
		log.Printf("[%s] fetch: GET %s (items_path=%q)", s.ID, url, s.ItemsPath)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// JSON APIs can be larger than the 5 MiB HTML cap. Allow
	// up to 25 MiB — still bounded so a misconfigured source
	// can't fill RAM.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
	if err != nil {
		return nil, err
	}

	rawItems, err := ExtractJSONArray(body, s.ItemsPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}

	items := make([]Item, 0, len(rawItems))
	seen := make(map[string]bool, len(rawItems))
	for _, raw := range rawItems {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			// Skip malformed entries rather than failing the
			// whole run.
			continue
		}
		id := jsonScalarAsString(obj[s.IDField])
		if id == "" {
			// Missing or non-scalar ID — skip. The state map
			// is keyed by string, so we can't track items
			// without one.
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		title := jsonScalarAsString(obj[s.TitleField])
		if title == "" {
			title = id
		}
		var link string
		if s.LinkField != "" {
			link = jsonScalarAsString(obj[s.LinkField])
		}
		items = append(items, Item{ID: id, Title: title, Link: link})
	}
	return items, nil
}

// Validate applies the json-specific defaults: URL is required,
// ItemsPath/IDField/TitleField default to the most common
// shape. The defaults match the example in the README and the
// template the daemon writes on first run.
func (jsonFetcher) Validate(s *Source) error {
	if s.URL == "" {
		return fmt.Errorf("json requires url")
	}
	if s.ItemsPath == "" {
		s.ItemsPath = "data"
	}
	if s.IDField == "" {
		s.IDField = "id"
	}
	if s.TitleField == "" {
		s.TitleField = "name"
	}
	return nil
}

// Format builds an "added/removed" notification for the diff.
// The Click header uses the first added item's Link (so a
// package tracking notification opens that package's detail
// page), falling back to the source's Link, then the source's
// URL.
func (jsonFetcher) Format(ctx context.Context, s *Source, added, removed []Item) Notification {
	click := s.URL
	if len(added) > 0 && added[0].Link != "" {
		click = added[0].Link
	} else if s.Link != "" {
		click = s.Link
	}
	return formatListDiff(s, added, removed, click)
}

// jsonScalarAsString returns a string representation of a JSON
// scalar value (string, number, or bool). Numbers are formatted
// as integers when possible (so "953389610" stays as
// "953389610", not the float64-expanded "953389610.000000").
// Returns "" for objects, arrays, nil, or unsupported types —
// callers treat that as "no id".
func jsonScalarAsString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// formatListDiff builds the "Added/Removed" notification body
// shared by html and json fetchers. Both source types produce
// item-level diffs (each Item has a stable ID), so the body
// layout is the same; only the Click URL and the priority
// threshold differ.
func formatListDiff(s *Source, added, removed []Item, click string) Notification {
	const maxBody = 3500
	const maxListed = 10

	total := len(added) + len(removed)
	priority := "default"
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

	return Notification{
		Title:    fmt.Sprintf("🔔 %s: %d added, %d removed", s.Name, len(added), len(removed)),
		Body:     b.String(),
		Priority: priority,
		Tags:     "loudspeaker",
		Click:    click,
	}
}

// formatItemLine renders one Item as a single body line. When
// the item carries a Link, the title is wrapped in a
// ntfy-rendered markdown link so tapping it in the ntfy
// app/web opens that URL directly. Items without a Link fall
// back to plain text.
func formatItemLine(it Item) string {
	if it.Link == "" {
		return it.Title
	}
	return fmt.Sprintf("[%s](%s)", it.Title, it.Link)
}
