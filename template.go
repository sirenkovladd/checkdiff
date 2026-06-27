package main

import (
	"strconv"
	"strings"
	"time"
)

// urlPlaceholders lists the supported {{...}} substitutions and the
// functions that produce the replacement string for each. The map
// lookup is the only place that maps a placeholder name to its
// formatter — adding a new placeholder is one line.
//
// We use a simple strings.NewReplacer rather than text/template so
// that URLs containing stray {{ are a no-op rather than a parse
// error. The user's stated use case (%5D&ui_random={{.UnixMilli}})
// is the primary motivation; if a URL later needs real templating
// (conditionals, loops) we can layer text/template on top.
var urlPlaceholders = map[string]func(time.Time) string{
	"{{.UnixMilli}}": func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) },
	"{{.Unix}}":      func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) },
	"{{.ISO}}":       func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"{{.Date}}":      func(t time.Time) string { return t.UTC().Format("2006-01-02") },
}

// renderURL applies the {{...}} substitutions listed in
// urlPlaceholders to s. now is captured once and used for every
// placeholder in s, so all substitutions in a single URL see the
// same timestamp.
//
// Stray {{...}} patterns that don't match a known placeholder are
// left as-is. That's the deliberate difference from text/template:
// we never error on a URL.
func renderURL(s string, now time.Time) string {
	if !strings.ContainsAny(s, "{") {
		// Fast path: no template syntax at all. The vast majority of
		// URLs will take this branch.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Find the next "{{" that has a matching placeholder.
		// We scan by trying each known placeholder at every position.
		matched := false
		for ph, fn := range urlPlaceholders {
			if strings.HasPrefix(s[i:], ph) {
				b.WriteString(fn(now))
				i += len(ph)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
