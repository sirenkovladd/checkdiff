package check

import (
	"context"
	"net/http"
	"net/http/httptest"
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
