package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSummarizer records its inputs and returns a canned
// summary (or error). Implements LLMSummarizer.
type fakeSummarizer struct {
	model        string
	sourceName   string
	customPrompt string
	summary      string
	err          error
}

func (f *fakeSummarizer) Summarize(ctx context.Context, model, sourceName, pageURL, oldText, newText, customPrompt string) (string, error) {
	f.model = model
	f.sourceName = sourceName
	f.customPrompt = customPrompt
	return f.summary, f.err
}

func TestPageLLMValidate(t *testing.T) {
	s := &Source{ID: "x", Type: "page_llm", URL: "https://example.com"}
	if err := Validate(s); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.Selector != "main" {
		t.Errorf("Selector = %q, want default %q", s.Selector, "main")
	}

	bad := &Source{ID: "x", Type: "page_llm"}
	if err := Validate(bad); err == nil {
		t.Errorf("Validate without URL: got nil error, want error")
	}
}

func TestExtractPageText(t *testing.T) {
	body := []byte(`<html><body><main><h1>Openprinter</h1><p>This project is launching soon.</p></main><footer>footer noise</footer></body></html>`)
	got, err := extractPageText(body, "main")
	if err != nil {
		t.Fatalf("extractPageText: %v", err)
	}
	if !strings.Contains(got, "Openprinter") || !strings.Contains(got, "launching soon") {
		t.Errorf("extractPageText = %q, want title + status", got)
	}
	if strings.Contains(got, "footer noise") {
		t.Errorf("extractPageText = %q, want footer excluded", got)
	}
}

func TestExtractPageTextNoMatch(t *testing.T) {
	body := []byte(`<html><body><p>hi</p></body></html>`)
	if _, err := extractPageText(body, "main"); err == nil {
		t.Errorf("extractPageText with no main: got nil error, want error")
	}
}

func TestPageLLMFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(`<html><body><main><p>Campaign is live!</p></main></body></html>`))
	}))
	defer srv.Close()

	s := &Source{ID: "x", Type: "page_llm", URL: srv.URL, Selector: "main"}
	items, err := Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Fetch: got %d items, want 1", len(items))
	}
	if !strings.Contains(items[0].ID, "Campaign is live!") {
		t.Errorf("Fetch item = %q, want page text", items[0].ID)
	}
}

func TestPageLLMFormatSummary(t *testing.T) {
	old := SetLLMForTest(&fakeSummarizer{summary: "Campaign went live with early-bird pricing."})
	defer SetLLMForTest(old)

	s := &Source{ID: "x", Name: "Openprinter", Type: "page_llm", URL: "https://example.com/campaign"}
	added := []Item{{ID: "new", Title: "Campaign is live!"}}
	removed := []Item{{ID: "old", Title: "This project is launching soon."}}
	n := Format(context.Background(), s, added, removed)
	if !strings.Contains(n.Body, "Campaign went live with early-bird pricing.") {
		t.Errorf("Format body = %q, want LLM summary", n.Body)
	}
	if strings.Contains(n.Body, "launching soon") {
		t.Errorf("Format body = %q, want summary ONLY (no raw diff)", n.Body)
	}
	if n.Click != s.URL {
		t.Errorf("Click = %q, want %q", n.Click, s.URL)
	}
}

func TestPageLLMFormatPassesModelAndPrompt(t *testing.T) {
	fake := &fakeSummarizer{summary: "s"}
	old := SetLLMForTest(fake)
	defer SetLLMForTest(old)

	s := &Source{ID: "x", Name: "n", Type: "page_llm", URL: "https://example.com",
		Prompt: "focus on pricing", LLMModel: "strong-model"}
	n := Format(context.Background(), s,
		[]Item{{ID: "n", Title: "new"}}, []Item{{ID: "o", Title: "old"}})
	if fake.model != "strong-model" {
		t.Errorf("model passed = %q, want %q", fake.model, "strong-model")
	}
	if fake.customPrompt != "focus on pricing" {
		t.Errorf("prompt passed = %q", fake.customPrompt)
	}
	if !strings.Contains(n.Body, "s") {
		t.Errorf("body = %q, want summary", n.Body)
	}
}

func TestPageLLMFormatFallbackOnError(t *testing.T) {
	old := SetLLMForTest(&fakeSummarizer{err: errors.New("boom")})
	defer SetLLMForTest(old)

	s := &Source{ID: "x", Name: "n", Type: "page_llm", URL: "https://example.com"}
	n := Format(context.Background(), s,
		[]Item{{ID: "n", Title: "Campaign is live!"}}, []Item{{ID: "o", Title: "launching soon"}})
	if !strings.Contains(n.Body, "Campaign is live!") || !strings.Contains(n.Body, "launching soon") {
		t.Errorf("fallback body = %q, want from/to text", n.Body)
	}
}

func TestPageLLMFormatFallbackNoLLM(t *testing.T) {
	old := SetLLMForTest(nil)
	defer SetLLMForTest(old)

	s := &Source{ID: "x", Name: "n", Type: "page_llm", URL: "https://example.com"}
	n := Format(context.Background(), s,
		[]Item{{ID: "n", Title: "new text"}}, []Item{{ID: "o", Title: "old text"}})
	if !strings.Contains(n.Body, "new text") {
		t.Errorf("no-LLM body = %q, want static diff", n.Body)
	}
}

func TestPageLLMFormatSuppressesNoMeaningfulChange(t *testing.T) {
	// The exact user-reported shape: raw text churned, the LLM
	// judged it boilerplate. Format must return an empty
	// Notification so check.One publishes nothing.
	summaries := []string{
		"NO_MEANINGFUL_CHANGE",
		"  NO_MEANINGFUL_CHANGE.  ",
		"no_meaningful_change",
		"No meaningful change — the Openprinter campaign is still Coming Soon with the same two updates and no new pricing or launch date.",
		"Nothing changed; the page shows the same status as before.",
	}
	for _, summary := range summaries {
		old := SetLLMForTest(&fakeSummarizer{summary: summary})
		s := &Source{ID: "x", Name: "Openprinter", Type: "page_llm", URL: "https://example.com/campaign"}
		n := Format(context.Background(), s,
			[]Item{{ID: "new", Title: "new text"}}, []Item{{ID: "old", Title: "old text"}})
		if n.Title != "" || n.Body != "" {
			t.Errorf("Format(%q) = %+v, want empty Notification (suppressed)", summary, n)
		}
		SetLLMForTest(old)
	}
}

func TestIsNoMeaningfulChange(t *testing.T) {
	cases := []struct {
		summary string
		want    bool
	}{
		{"NO_MEANINGFUL_CHANGE", true},
		{"NO_MEANINGFUL_CHANGE.", true},
		{"No meaningful change — still Coming Soon.", true},
		{"Nothing changed; same status as before.", true},
		{"Only boilerplate differs (timestamps).", true},
		{"Campaign went live with early-bird pricing.", false},
		// Contrastive follow-up: opens with "no X" but
		// reports a real change after it — must notify.
		{"No meaningful change in pricing, but a launch date was announced.", false},
		{"No change in status, however a new update was posted.", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isNoMeaningfulChange(c.summary); got != c.want {
			t.Errorf("isNoMeaningfulChange(%q) = %v, want %v", c.summary, got, c.want)
		}
	}
}
