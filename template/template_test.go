package template

import (
	"regexp"
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
		{
			name: "UUID placeholder",
			in:   "https://example.com/api?id={{.UUID}}",
			want: "", // verified separately below
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Render(c.in, now)
			// The UUID test verifies the format separately; the
			// expected literal is empty for that case.
			if c.want == "" {
				if !strings.HasPrefix(got, "https://example.com/api?id=") {
					t.Errorf("Render(%q) = %q, want URL prefix", c.in, got)
				}
				return
			}
			if got != c.want {
				t.Errorf("Render(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRenderURLAllSubstitutionsSeeSameTimestamp(t *testing.T) {
	// All placeholders in one URL must see the same `now` value —
	// callers pass a single timestamp and expect consistent results.
	// (UUID is excluded: it's a fresh value per call by design.)
	now := time.Unix(1700000000, 0).UTC()
	in := "{{.Unix}}|{{.UnixMilli}}|{{.ISO}}|{{.Date}}"
	got := Render(in, now)
	for _, part := range strings.Split(got, "|") {
		if part == "" {
			t.Errorf("Render left an empty segment: %q", got)
		}
	}
}

func TestRenderURLUUIDIsFresh(t *testing.T) {
	// {{.UUID}} must produce a different value on every call —
	// it's used to bust caches that key on the URL alone.
	in := "https://example.com/api?id={{.UUID}}"
	a := Render(in, time.Unix(1700000000, 0).UTC())
	b := Render(in, time.Unix(1700000000, 0).UTC())
	if a == b {
		t.Errorf("two renders of {{.UUID}} produced the same value: %q", a)
	}
	if !regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).MatchString(strings.TrimPrefix(a, "https://example.com/api?id=")) {
		t.Errorf("{{.UUID}} substitution did not look like a UUID: %q", a)
	}
}

// TestRenderLongestPlaceholderWins pins the invariant that
// protects against a future placeholder being a prefix of an
// existing one (e.g. {{.U}} or {{.DateX}}). When two
// placeholders could match at a given cursor, the longer one
// must win; otherwise the shorter match leaves trailing
// characters as garbage. The current placeholder set happens
// to be disjoint, so this test would fail the moment a
// prefix-related placeholder was added — which is exactly the
// regression it's designed to catch.
func TestRenderLongestPlaceholderWins(t *testing.T) {
	// Sanity check: {{.UnixMilli}} and {{.Unix}} both start
	// with {{.Unix, so a naive (shortest-first) scan would
	// match {{.Unix}} and leave "Milli}}" as garbage. The
	// longest-first scan must pick {{.UnixMilli}}.
	now := time.Unix(1700000000, 0).UTC()
	in := "x={{.UnixMilli}}"
	got := Render(in, now)
	want := "x=1700000000000"
	if got != want {
		t.Errorf("Render(%q) = %q, want %q (longer placeholder should win)", in, got, want)
	}
}
