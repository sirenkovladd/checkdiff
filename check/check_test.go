package check

import (
	"testing"

	"checkdiff/source"
)

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
