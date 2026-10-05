package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"checkdiff/template"
	"golang.org/x/net/html"
)

// pageLLMfetcher implements the Fetcher interface for the
// "page_llm" source type: a web page whose MAIN content is
// extracted via a selector, hashed by its normalized text,
// and — when the text changes between runs — summarized by
// an LLM into a short notification message.
//
// Where html is the right choice: a page with discrete
// entries (changelog headings, list items) where each entry
// is tracked individually and the notification lists
// added/removed lines.
//
// Where page_llm is the right choice: a narrative page
// (campaign, product, status page) whose whole body text is
// the signal — e.g. a Crowd Supply campaign flipping from
// "This project is launching soon" to live, or spec text
// being edited. Tracking entry-level diffs there is noise;
// one LLM sentence ("Campaign went live…") is the message.
//
// Diff semantics: a single Item whose ID is the normalized
// page text (whitespace-collapsed, capped at maxPageLLMChars
// so state.json stays small). A change produces one added +
// one removed Item; Format hands both to the configured
// LLMSummarizer and sends only the summary. When no
// summarizer is configured (or the LLM call fails), Format
// falls back to a static from/to diff so the change still
// notifies instead of being swallowed.
type pageLLMfetcher struct{}

// Type returns the registry key for this fetcher.
func (pageLLMfetcher) Type() string { return "page_llm" }

// maxPageLLMChars caps the normalized text stored as the
// Item ID. 12k chars is enough for a full campaign page
// body; anything longer is boilerplate. The cap also bounds
// state.json growth and the LLM prompt (Summarize clamps
// again to 6k per side).
const maxPageLLMChars = 12000

// LLMSummarizer generates the notification text for a
// page_llm diff. The signature uses only stdlib types so the
// llm package (which config imports, and which would
// otherwise create an import cycle) can implement it without
// importing source. Set by the daemon at startup/reload via
// SetLLMSummarizer; nil means "no LLM configured". The model
// argument is the per-source llm_model override ("" = use the
// client's default).
type LLMSummarizer interface {
	Summarize(ctx context.Context, model, sourceName, pageURL, oldText, newText, customPrompt string) (string, error)
}

var llmSummarizer LLMSummarizer

// SetLLMSummarizer installs the process-wide summarizer used
// by page_llm Format. Called by the daemon (and tests); safe
// for concurrent use only during setup, before checks run.
func SetLLMSummarizer(s LLMSummarizer) { llmSummarizer = s }

// SetLLMForTest swaps the summarizer and returns the previous
// value so tests can restore it with defer.
func SetLLMForTest(s LLMSummarizer) LLMSummarizer {
	old := llmSummarizer
	llmSummarizer = s
	return old
}

// Fetch retrieves the page, extracts the text under the
// source's selector (all matching elements concatenated),
// normalizes whitespace, caps length, and returns it as a
// single Item. Any change in the text between runs is the
// diff signal.
func (pageLLMfetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := template.Render(s.URL, now)
	if fetchVerbose {
		log.Printf("[%s] fetch: GET %s (page_llm selector=%q)", s.ID, url, s.Selector)
	}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap, same as html
	if err != nil {
		return nil, err
	}

	text, err := extractPageText(body, s.Selector)
	if err != nil {
		return nil, err
	}
	if fetchVerbose {
		log.Printf("[%s] fetch ok: %d chars (selector=%q)", s.ID, len(text), s.Selector)
	}
	return []Item{{ID: text, Title: text}}, nil
}

// Validate requires URL; Selector defaults to "main" (the
// whole main content — the case this type exists for).
// Prompt and LLMModel are optional overrides.
func (pageLLMfetcher) Validate(s *Source) error {
	if s.URL == "" {
		return fmt.Errorf("page_llm requires url")
	}
	if s.Selector == "" {
		s.Selector = "main"
	}
	if _, err := parseHTMLSelector(s.Selector); err != nil {
		return fmt.Errorf("page_llm: unsupported selector %q (use a tag name like main, or 'tag.class' such as div.content)", s.Selector)
	}
	return nil
}

// Format builds the notification. With a summarizer
// configured, the body is the LLM summary only (plus the
// source line). On LLM failure — or with no summarizer — it
// falls back to a truncated from/to diff so the change still
// notifies.
func (pageLLMfetcher) Format(ctx context.Context, s *Source, added, removed []Item) Notification {
	var oldVal, newVal string
	if len(removed) > 0 {
		oldVal = removed[0].Title
	}
	if len(added) > 0 {
		newVal = added[0].Title
	}

	click := s.URL
	if s.Link != "" {
		click = s.Link
	}

	if llmSummarizer != nil {
		summary, err := llmSummarizer.Summarize(ctx, s.LLMModel, s.Name, click, oldVal, newVal, s.Prompt)
		if err != nil {
			log.Printf("[%s] llm summarize failed, using fallback: %v", s.ID, err)
		} else if strings.TrimSpace(summary) != "" {
			// Summary only — no "Source:" trailer. The URL
			// already travels in the ntfy Click header, so
			// repeating it in the body is noise.
			return Notification{
				Title:    fmt.Sprintf("🔔 %s: update", s.Name),
				Body:     strings.TrimSpace(summary),
				Priority: "default",
				Tags:     "loudspeaker",
				Click:    click,
			}
		}
	}

	// Fallback: static from/to (also the no-LLM path).
	return formatPageLLMFallback(s, oldVal, newVal, click)
}

// formatPageLLMFallback renders a truncated from/to body.
// Cap each side at 1500 chars so a full-page rewrite still
// fits comfortably in an ntfy message. No "Source:" trailer
// — the URL travels in the Click header (see Format).
func formatPageLLMFallback(s *Source, oldVal, newVal, click string) Notification {
	const maxSide = 1500
	var b strings.Builder
	fmt.Fprintf(&b, "Content changed for %s\n\n", s.Name)
	if oldVal != "" {
		fmt.Fprintf(&b, "From: %s\n", truncateSide(oldVal, maxSide))
	}
	if newVal != "" {
		fmt.Fprintf(&b, "To:   %s\n", truncateSide(newVal, maxSide))
	}
	return Notification{
		Title:    fmt.Sprintf("🔔 %s: update", s.Name),
		Body:     b.String(),
		Priority: "default",
		Tags:     "loudspeaker",
		Click:    click,
	}
}

func truncateSide(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// extractPageText parses body, concatenates the normalized
// text of every element matching selector, and caps the
// result at maxPageLLMChars. Whitespace runs collapse to a
// single space (via extractText); blocks join with newlines
// so "status" vs "updates" sections stay distinguishable in
// the LLM prompt.
func extractPageText(body []byte, selector string) (string, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	sel, err := parseHTMLSelector(selector)
	if err != nil {
		return "", err
	}
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if sel.matches(n) {
			t := strings.TrimSpace(extractText(n))
			if t != "" {
				parts = append(parts, t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(parts) == 0 {
		return "", errors.New("no matching elements found")
	}
	// A selector like "main" usually matches once; a broader
	// one (e.g. "div") can match nested elements whose texts
	// repeat. Deduplicate consecutive repeats to keep the
	// stored text (and the LLM prompt) compact.
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	text := strings.Join(out, "\n")
	if len(text) > maxPageLLMChars {
		text = text[:maxPageLLMChars] + "…(truncated)"
	}
	return text, nil
}
