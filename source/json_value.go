package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"checkdiff/template"
)

// jsonValueFetcher implements the Fetcher interface for the
// "json_value" source type: a JSON endpoint that exposes a
// single scalar field whose value the user wants to watch for
// changes. The endpoint may return a flat object, a nested
// object, or even an object at the end of an array index —
// what matters is that the path resolves to one string/number/
// bool that is the "current state" being tracked.
//
// Where the json fetcher is the right choice: a list of
// items where each item has a stable ID and you want to
// know when items are added or removed.
//
// Where json_value is the right choice: a single field whose
// value is the entire signal (e.g. an API status string, a
// feature flag, a notification text). When the value changes,
// notify.
//
// Diff semantics: the fetched value is wrapped in a single
// Item whose ID is the value text. If the value differs from
// the previous run, check.One sees one added Item and one
// removed Item. The format helper produces a "changed from
// X to Y" notification, which is the right shape for this
// kind of signal.
type jsonValueFetcher struct{}

// Type returns the registry key for this fetcher.
func (jsonValueFetcher) Type() string { return "json_value" }

// Fetch retrieves the current value at s.Path and returns it
// as a single Item. The Item's ID is the value text, so
// check.One's diff logic treats any change as a one-add /
// one-remove pair.
func (jsonValueFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := template.Render(s.URL, now)
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
	// 5 MiB is more than enough for a single-field JSON; the
	// value itself is at most a few hundred chars.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}

	value, err := ExtractJSONValue(body, s.Path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return []Item{{ID: value, Title: value}}, nil
}

// Validate requires URL and Path. Path is a dot-separated
// sequence of object keys with optional [N] array indices
// (see source.ExtractJSONValue for the grammar).
func (jsonValueFetcher) Validate(s *Source) error {
	if s.URL == "" {
		return fmt.Errorf("json_value requires url")
	}
	if s.Path == "" {
		return fmt.Errorf("json_value requires path (dot-separated JSON path to the value, e.g. body.button_status.notification)")
	}
	return nil
}

// Format builds a "changed from X to Y" notification. The
// check.One diff for a single-value source produces one
// removed (the old value) and one added (the new value),
// and the body surfaces both so the user can see exactly
// what changed.
func (jsonValueFetcher) Format(s *Source, added, removed []Item) Notification {
	var oldVal, newVal string
	if len(removed) > 0 {
		oldVal = removed[0].Title
	}
	if len(added) > 0 {
		newVal = added[0].Title
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Value at %s changed:\n\n", s.Path)
	if oldVal != "" {
		fmt.Fprintf(&b, "From: %s\n", oldVal)
	}
	if newVal != "" {
		fmt.Fprintf(&b, "To:   %s\n", newVal)
	}
	fmt.Fprintf(&b, "\nSource: %s\n", s.URL)

	return Notification{
		Title:    fmt.Sprintf("🔔 %s: changed", s.Name),
		Body:     b.String(),
		Priority: "default",
		Tags:     "loudspeaker",
		Click:    s.URL,
	}
}
