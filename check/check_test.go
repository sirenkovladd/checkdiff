package check

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"checkdiff/notify"
	"checkdiff/source"
	"checkdiff/state"
)

func TestOnePublishesToResolvedTopic(t *testing.T) {
	// A source with its own Topic must publish there; a source
	// without one falls back to the client's configured topic.
	cases := []struct {
		name     string
		srcTopic string
		wantPath string
	}{
		{"per-source topic wins", "burnaby-volleyball", "/burnaby-volleyball"},
		{"empty topic falls back to client topic", "", "/default-topic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits int32
			var lastPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				lastPath = r.URL.Path
				w.WriteHeader(200)
			}))
			defer srv.Close()

			st := &state.State{Sources: map[string]*state.SourceState{}}
			// Baseline: "old" seen. Diff: "new" appears → publish.
			st.Remember("src", []source.Item{{ID: "old"}}, time.Now(), time.Now(), 0, 0, "")

			ntfy := notify.New(srv.URL, "default-topic")
			s := &source.Source{ID: "src", Name: "src", Type: "json", URL: "https://example.com", Topic: c.srcTopic}
			err := One(context.Background(), ntfy, st, s,
				[]source.Item{{ID: "old"}, {ID: "new"}},
				time.Now(), time.Now(), true)
			if err != nil {
				t.Fatalf("One: %v", err)
			}
			if atomic.LoadInt32(&hits) != 1 {
				t.Errorf("publish hits = %d, want 1", hits)
			}
			if lastPath != c.wantPath {
				t.Errorf("published path = %q, want %q", lastPath, c.wantPath)
			}
		})
	}
}

func TestOneSuppressesEmptyNotification(t *testing.T) {
	// page_llm whose LLM reports NO_MEANINGFUL_CHANGE formats
	// to an empty Notification. One must publish nothing but
	// still advance the baseline so the same churn doesn't
	// re-fire next tick.
	old := source.SetLLMForTest(stubSummarizer{summary: "NO_MEANINGFUL_CHANGE"})
	defer source.SetLLMForTest(old)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st := &state.State{Sources: map[string]*state.SourceState{}}
	st.Remember("src", []source.Item{{ID: "old text", Title: "old text"}}, time.Now(), time.Now(), 0, 0, "")

	ntfy := notify.New(srv.URL, "default-topic")
	s := &source.Source{ID: "src", Name: "src", Type: "page_llm", URL: "https://example.com"}
	if err := One(context.Background(), ntfy, st, s,
		[]source.Item{{ID: "new text", Title: "new text"}},
		time.Now(), time.Now(), true); err != nil {
		t.Fatalf("One: %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("publish hits = %d, want 0 (suppressed)", hits)
	}
	// Baseline advanced to the new text.
	ss := st.Snapshot("src")
	if ss == nil || ss.Baseline == nil {
		t.Fatalf("no state after suppressed run")
	}
	if !ss.Baseline.ItemsSeen["new text"] {
		t.Errorf("baseline not advanced after suppress; seen=%v", ss.Baseline.ItemsSeen)
	}
	if ss.Baseline.ItemsSeen["old text"] {
		t.Errorf("stale item still in baseline after suppress")
	}
}

func TestOneIgnoreRemovals(t *testing.T) {
	// runOne executes One against a capturing ntfy stub and
	// returns the publish count plus the last Title header
	// and body sent.
	runOne := func(t *testing.T, s *source.Source, baseline, current []source.Item) (int32, string, string, *state.State) {
		t.Helper()
		var hits int32
		var lastTitle, lastBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			lastTitle = r.Header.Get("Title")
			b, _ := io.ReadAll(r.Body)
			lastBody = string(b)
			w.WriteHeader(200)
		}))
		t.Cleanup(srv.Close)

		st := &state.State{Sources: map[string]*state.SourceState{}}
		st.Remember(s.ID, baseline, time.Now(), time.Now(), 0, 0, "")

		ntfy := notify.New(srv.URL, "default-topic")
		if err := One(context.Background(), ntfy, st, s, current, time.Now(), time.Now(), false); err != nil {
			t.Fatalf("One: %v", err)
		}
		return atomic.LoadInt32(&hits), lastTitle, lastBody, st
	}

	t.Run("removal-only diff is suppressed", func(t *testing.T) {
		s := &source.Source{ID: "src", Name: "src", Type: "html", URL: "https://example.com", IgnoreRemovals: true}
		hits, _, _, st := runOne(t, s,
			[]source.Item{{ID: "a"}, {ID: "b"}},
			[]source.Item{{ID: "a"}})
		if hits != 0 {
			t.Errorf("publish hits = %d, want 0 (removal ignored)", hits)
		}
		// Baseline still advances so the removal doesn't re-fire.
		ss := st.Snapshot("src")
		if ss == nil || ss.Baseline == nil {
			t.Fatalf("no state after ignored run")
		}
		if ss.Baseline.ItemsSeen["b"] {
			t.Errorf("stale item still in baseline after ignored removal")
		}
	})

	t.Run("removal-only diff notifies without the flag", func(t *testing.T) {
		s := &source.Source{ID: "src", Name: "src", Type: "html", URL: "https://example.com"}
		hits, _, body, _ := runOne(t, s,
			[]source.Item{{ID: "a"}, {ID: "b"}},
			[]source.Item{{ID: "a"}})
		if hits != 1 {
			t.Fatalf("publish hits = %d, want 1", hits)
		}
		if !strings.Contains(body, "Removed:") {
			t.Errorf("body should list removals, got:\n%s", body)
		}
	})

	t.Run("mixed diff notifies added only", func(t *testing.T) {
		s := &source.Source{ID: "src", Name: "src", Type: "html", URL: "https://example.com", IgnoreRemovals: true}
		hits, _, body, _ := runOne(t, s,
			[]source.Item{{ID: "a"}, {ID: "b"}},
			[]source.Item{{ID: "a"}, {ID: "c"}})
		if hits != 1 {
			t.Fatalf("publish hits = %d, want 1", hits)
		}
		if !strings.Contains(body, "Added:") || !strings.Contains(body, "c") {
			t.Errorf("body should list the addition, got:\n%s", body)
		}
		if strings.Contains(body, "Removed:") || strings.Contains(body, "b") {
			t.Errorf("body should omit the removal, got:\n%s", body)
		}
	})

	t.Run("single-value change keeps old and new", func(t *testing.T) {
		s := &source.Source{ID: "src", Name: "src", Type: "json_value", URL: "https://example.com/api", IgnoreRemovals: true}
		// json_value paths are validated only on Fetch;
		// Format (the unit under test here) needs none.
		hits, _, body, _ := runOne(t, s,
			[]source.Item{{ID: "old", Title: "old"}},
			[]source.Item{{ID: "new", Title: "new"}})
		if hits != 1 {
			t.Fatalf("publish hits = %d, want 1", hits)
		}
		if !strings.Contains(body, "From: old") || !strings.Contains(body, "To:   new") {
			t.Errorf("body should keep the from/to pair, got:\n%s", body)
		}
	})
}

// stubSummarizer implements source.LLMSummarizer with a canned
// reply so page_llm Format can be exercised through One
// without a network call.
type stubSummarizer struct{ summary string }

func (s stubSummarizer) Summarize(ctx context.Context, model, sourceName, pageURL, oldText, newText, customPrompt string) (string, error) {
	return s.summary, nil
}

func TestClickURLFor(t *testing.T) {
	sourceURL := "https://api.uniuni.example"
	trackingURL := "https://www.uniuni.com/tracking/#tracking-detail?no=U000180542908940"
	s := &source.Source{ID: "uniuni", Name: "uniuni-package", Type: "json", URL: sourceURL}
	sWithLink := &source.Source{
		ID:   "uniuni",
		Name: "uniuni-package",
		Type: "json",
		URL:  sourceURL,
		Link: "https://www.uniuni.com/tracking/#tracking-detail?no=U000180542908940",
	}

	cases := []struct {
		name  string
		s     *source.Source
		added []source.Item
		want  string
	}{
		{
			name:  "first added item has link → use that",
			added: []source.Item{{ID: "U000180542908940", Title: "U000180542908940", Link: trackingURL}},
			want:  trackingURL,
		},
		{
			name: "first added item has no link → fall back to source URL",
			added: []source.Item{
				{ID: "a", Title: "A"},
				{ID: "b", Title: "B", Link: "https://should-not-be-used.example"},
			},
			want: sourceURL,
		},
		{
			name:  "no added items → source URL",
			added: nil,
			want:  sourceURL,
		},
		{
			name:  "all added items have empty links → source URL",
			added: []source.Item{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}},
			want:  sourceURL,
		},
		{
			name:  "no per-item links but source has Link → use source Link",
			s:     sWithLink,
			added: []source.Item{{ID: "100", Title: "scan-event"}},
			want:  sWithLink.Link,
		},
		{
			name:  "per-item link wins over source Link",
			s:     sWithLink,
			added: []source.Item{{ID: "100", Title: "scan-event", Link: "https://item.example"}},
			want:  "https://item.example",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.s
			if src == nil {
				src = s
			}
			if got := ClickURLFor(src, c.added); got != c.want {
				t.Errorf("ClickURLFor = %q, want %q", got, c.want)
			}
		})
	}
}
