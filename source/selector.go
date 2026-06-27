package source

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// htmlSelector is a small subset of CSS selectors we support for
// the "html" source type. The grammar is intentionally tiny:
//
//	tag                 - any element with that tag name
//	tag.class           - element with that exact class
//	tag.class1.class2   - element with all of those classes (AND)
//
// The tag is matched case-insensitively against element names,
// and classes are matched exactly (case-sensitively) against
// tokens in the element's class attribute. The grammar is just
// enough to cover the concrete cases that motivated the html
// source (e.g. WordPress-style "li.attachedfile" attached-file
// lists). It is NOT a full CSS selector engine.
type htmlSelector struct {
	tag     string
	classes []string
}

func parseHTMLSelector(s string) (htmlSelector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return htmlSelector{}, fmt.Errorf("empty selector")
	}
	parts := strings.Split(s, ".")
	sel := htmlSelector{tag: strings.ToLower(strings.TrimSpace(parts[0]))}
	if sel.tag == "" {
		return htmlSelector{}, fmt.Errorf("selector %q: missing tag name", s)
	}
	for _, c := range parts[1:] {
		c = strings.TrimSpace(c)
		if c == "" {
			return htmlSelector{}, fmt.Errorf("selector %q: empty class", s)
		}
		sel.classes = append(sel.classes, c)
	}
	return sel, nil
}

// matches reports whether n is an element node that matches the
// selector. Non-element nodes never match.
func (sel htmlSelector) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if !strings.EqualFold(n.Data, sel.tag) {
		return false
	}
	if len(sel.classes) == 0 {
		return true
	}
	var classAttr string
	var haveClass bool
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, "class") {
			classAttr = a.Val
			haveClass = true
			break
		}
	}
	if !haveClass {
		return false
	}
	have := make(map[string]bool)
	for _, c := range strings.Fields(classAttr) {
		have[c] = true
	}
	for _, want := range sel.classes {
		if !have[want] {
			return false
		}
	}
	return true
}

// extractText returns the concatenation of all text nodes
// under n, with runs of whitespace collapsed to a single space.
func extractText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
