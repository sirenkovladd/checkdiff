// Package template applies {{...}} substitutions to user-supplied
// strings (primarily URL fields in source configurations). The
// supported placeholders are listed in placeholders; the
// formatter for each receives the run's timestamp so all
// substitutions in a single string see the same value.
//
// We use a hand-rolled scanner rather than text/template so
// that URLs containing stray {{ are a no-op rather than a
// parse error. The user's stated use case
// (cache-busting parameters) is the primary motivation; if a
// URL later needs real templating (conditionals, loops) we can
// layer text/template on top.
package template

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// placeholders lists the supported {{...}} substitutions and
// the functions that produce the replacement string for each.
// The map lookup is the only place that maps a placeholder
// name to its formatter — adding a new placeholder is one
// line.
//
// The formatter receives the run's `now` timestamp. For most
// placeholders this is the source of truth; {{.UUID}} ignores
// it because the UUID is freshly generated on every call (no
// time component).
var placeholders = map[string]func(time.Time) string{
	"{{.UnixMilli}}": func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) },
	"{{.Unix}}":      func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) },
	"{{.ISO}}":       func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"{{.Date}}":      func(t time.Time) string { return t.UTC().Format("2006-01-02") },
	"{{.UUID}}":      func(t time.Time) string { return uuid.New().String() },
}

// sortedPlaceholders is placeholders' keys sorted by length
// descending. We scan in this order at every cursor position
// so that the longest matching placeholder wins when two
// placeholders are prefixes of each other (e.g. {{.Unix}} and
// {{.UnixMilli}}). Without the sort, Go's randomised map
// iteration order would let the shorter key match first and
// leave the longer key's tail as garbage in the output.
//
// The sorted slice is built once at package init; the keys
// never change, so the sort cost is paid once.
var sortedPlaceholders []string

func init() {
	for ph := range placeholders {
		sortedPlaceholders = append(sortedPlaceholders, ph)
	}
	// Sort longest-first so that a placeholder like
	// {{.UnixMilli}} wins over its prefix {{.Unix}} at the
	// same cursor position.
	sort.Slice(sortedPlaceholders, func(i, j int) bool {
		return len(sortedPlaceholders[i]) > len(sortedPlaceholders[j])
	})
}

// Render applies the {{...}} substitutions listed in
// placeholders to s. now is captured once and used for every
// placeholder in s, so all substitutions in a single string
// see the same timestamp.
//
// Stray {{...}} patterns that don't match a known placeholder
// are left as-is. That's the deliberate difference from
// text/template: we never error on a string.
func Render(s string, now time.Time) string {
	if !strings.ContainsAny(s, "{") {
		// Fast path: no template syntax at all. The vast
		// majority of URLs will take this branch.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Find the next matching placeholder at position i.
		// We scan sortedPlaceholders (longest first) so that
		// the longest match wins.
		matched := false
		for _, ph := range sortedPlaceholders {
			if strings.HasPrefix(s[i:], ph) {
				b.WriteString(placeholders[ph](now))
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
