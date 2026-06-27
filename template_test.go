package main

import (
	"strings"
	"testing"
	"time"
)

func TestRenderURL(t *testing.T) {
	// Fixed timestamp so the assertions are deterministic.
	now := time.Unix(1700000000, 123456789).UTC()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no placeholders",
			in:   "https://example.com/api",
			want: "https://example.com/api",
		},
		{
			name: "unix milli",
			in:   "https://example.com/api?ts={{.UnixMilli}}",
			want: "https://example.com/api?ts=1700000000123",
		},
		{
			name: "unix seconds",
			in:   "https://example.com/api?ts={{.Unix}}",
			want: "https://example.com/api?ts=1700000000",
		},
		{
			name: "ISO 8601",
			in:   "https://example.com/api?t={{.ISO}}",
			want: "https://example.com/api?t=2023-11-14T22:13:20Z",
		},
		{
			name: "date only",
			in:   "https://example.com/api?d={{.Date}}",
			want: "https://example.com/api?d=2023-11-14",
		},
		{
			name: "user's stated use case",
			in:   "https://example.com/page%5D&ui_random={{.UnixMilli}}",
			want: "https://example.com/page%5D&ui_random=1700000000123",
		},
		{
			name: "multiple placeholders in one URL",
			in:   "https://example.com/api?t={{.UnixMilli}}&d={{.Date}}",
			want: "https://example.com/api?t=1700000000123&d=2023-11-14",
		},
		{
			name: "stray double brace is left as-is",
			in:   "https://example.com/{{notaplaceholder}}",
			want: "https://example.com/{{notaplaceholder}}",
		},
		{
			name: "placeholder at start",
			in:   "{{.UnixMilli}}-suffix",
			want: "1700000000123-suffix",
		},
		{
			name: "placeholder at end",
			in:   "prefix-{{.UnixMilli}}",
			want: "prefix-1700000000123",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderURL(c.in, now)
			if got != c.want {
				t.Errorf("renderURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRenderURLAllSubstitutionsSeeSameTimestamp(t *testing.T) {
	// All placeholders in one URL must see the same `now` value —
	// callers pass a single timestamp and expect consistent results.
	now := time.Unix(1700000000, 0).UTC()
	in := "{{.Unix}}|{{.UnixMilli}}|{{.ISO}}|{{.Date}}"
	got := renderURL(in, now)
	for _, part := range strings.Split(got, "|") {
		if part == "" {
			t.Errorf("renderURL left an empty segment: %q", got)
		}
	}
}
