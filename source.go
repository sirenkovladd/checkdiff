package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Item is one change-worthy entry from a Source. The state file stores
// the IDs of all items last seen for a given source; any item whose ID
// is not in that set is treated as new.
type Item struct {
	ID    string `json:"id"`              // stable per item, e.g. git blob SHA, changelog-entry text
	Title string `json:"title,omitempty"` // short, used as notification line
	Body  string `json:"body,omitempty"`  // optional longer snippet, capped
	// Link is an optional URL associated with the item. When set on
	// an added item, the notification's ntfy Click header uses it
	// (instead of the source's URL) and the item is rendered as a
	// markdown link in the body so the entry itself is tappable.
	Link string `json:"link,omitempty"`
}

// fetchSource runs one source's fetch logic and returns the current
// set of items. The returned error is for transient failures (network,
// gh CLI not found, etc.) and is reported to the user as a separate
// error notification when set.
//
// now is passed through to the underlying fetchers so that any
// {{...}} placeholders in the source's URL fields are substituted
// with a consistent timestamp. A single timestamp per run avoids
// the case where two substitutions in one URL see different values
// (which would be a subtle and confusing bug).
func fetchSource(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	switch s.Type {
	case "github_file":
		return fetchGitHubFile(ctx, s, now)
	case "html":
		return fetchHTML(ctx, s, now)
	case "json":
		return fetchJSON(ctx, s, now)
	default:
		return nil, fmt.Errorf("unsupported type %q", s.Type)
	}
}

// githubContents is the subset of the response from
// `gh api repos/{owner}/{repo}/contents/{path}?ref={ref}` we care about.
type githubContents struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func fetchGitHubFile(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	gh, err := resolveGhBinary()
	if err != nil {
		return nil, err
	}
	// Apply URL templates to the GitHub fields before composing the
	// API path. This lets a source use {{.UnixMilli}} in owner/repo/
	// ref/path to bust caches on APIs that key by those values.
	owner := renderURL(s.Owner, now)
	repo := renderURL(s.Repo, now)
	ref := renderURL(s.Ref, now)
	path := renderURL(s.Path, now)
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		owner, repo, path, ref)

	// Use --jq to keep gh's output tight and to fail fast on errors.
	// We pull the whole JSON object back; it's small even for big files
	// because the content is just base64.
	cmd := exec.CommandContext(ctx, gh, "api", apiPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %v: %s", apiPath, err, strings.TrimSpace(stderr.String()))
	}

	var gc githubContents
	if err := json.Unmarshal(stdout.Bytes(), &gc); err != nil {
		return nil, fmt.Errorf("gh api %s: decode: %w", apiPath, err)
	}
	if gc.Type != "file" {
		return nil, fmt.Errorf("gh api %s: not a file (type=%q)", apiPath, gc.Type)
	}

	var raw []byte
	switch gc.Encoding {
	case "base64":
		var derr error
		raw, derr = base64.StdEncoding.DecodeString(strings.ReplaceAll(gc.Content, "\n", ""))
		if derr != nil {
			return nil, fmt.Errorf("gh api %s: base64 decode: %w", apiPath, derr)
		}
	case "":
		raw = []byte(gc.Content)
	default:
		return nil, fmt.Errorf("gh api %s: unsupported encoding %q", apiPath, gc.Encoding)
	}

	// Prefer the git blob SHA from GitHub (changes only when content
	// changes). Fall back to sha256 of the decoded content.
	id := gc.SHA
	if id == "" {
		sum := sha256.Sum256(raw)
		id = "sha256:" + hex.EncodeToString(sum[:])
	}

	return []Item{{
		ID:    id,
		Title: fmt.Sprintf("%s @ %s (%d bytes)", s.Path, shortRef(s.Ref), gc.Size),
		Body:  truncate(string(raw), 4000),
	}}, nil
}

func fetchHTML(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := renderURL(s.URL, now)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	sel, err := parseHTMLSelector(s.Selector)
	if err != nil {
		return nil, err
	}

	var titles []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if sel.matches(n) {
			t := extractText(n)
			t = strings.TrimSpace(t)
			if t != "" {
				titles = append(titles, t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(titles) == 0 {
		return nil, errors.New("no matching elements found")
	}

	items := make([]Item, 0, len(titles))
	seen := make(map[string]bool, len(titles))
	for _, t := range titles {
		// Track by the element's text content (not a hash) so that
		// additions and removals of individual entries are detectable
		// across runs. Deduplicate so the same text on the page
		// (e.g. a repeated heading) doesn't bloat the state set.
		if seen[t] {
			continue
		}
		seen[t] = true
		items = append(items, Item{
			ID:    t,
			Title: t,
		})
	}
	return items, nil
}

// fetchJSON pulls a JSON document and extracts a set of items from an
// array located at ItemsPath. Each item's ID is taken from IDField and
// its display title from TitleField. IDs are stable, so additions and
// removals between runs are detectable.
func fetchJSON(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := renderURL(s.URL, now)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// JSON APIs can be larger than the 5 MiB HTML cap. Allow up to
	// 25 MiB — still bounded so a misconfigured source can't fill RAM.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
	if err != nil {
		return nil, err
	}

	rawItems, err := extractJSONArray(body, s.ItemsPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}

	items := make([]Item, 0, len(rawItems))
	seen := make(map[string]bool, len(rawItems))
	for _, raw := range rawItems {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			// Skip malformed entries rather than failing the whole run.
			continue
		}
		id := jsonScalarAsString(obj[s.IDField])
		if id == "" {
			// Missing or non-scalar ID — skip. The state map is keyed
			// by string, so we can't track items without one.
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		title := jsonScalarAsString(obj[s.TitleField])
		if title == "" {
			title = id
		}
		var link string
		if s.LinkField != "" {
			link = jsonScalarAsString(obj[s.LinkField])
		}
		items = append(items, Item{ID: id, Title: title, Link: link})
	}
	return items, nil
}

// jsonScalarAsString returns a string representation of a JSON scalar
// value (string, number, or bool). Numbers are formatted as integers
// when possible (so "953389610" stays as "953389610", not the
// float64-expanded "953389610.000000"). Returns "" for objects,
// arrays, nil, or unsupported types — callers treat that as "no id".
func jsonScalarAsString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// extractJSONArray navigates a path through a JSON document and
// returns the raw JSON of each element of the array found at the
// end. The path grammar is intentionally tiny:
//
//	""          - the root must be an array
//	"a"         - root must be {"a": [...]}
//	"a.b"       - root must be {"a": {"b": [...]}}
//	"a[0]"      - root must be {"a": [...]}, take index 0
//	"a[0].b"    - root must be {"a": [{"b": [...]}]}
//
// Segments separated by "." are object keys. A "[N]" suffix on a
// segment indexes into the array reached at that step. Multiple
// "[N]" suffixes on a single segment (e.g. "a[0][1]") traverse
// nested arrays. Indexing past the end of an array is an error.
// It does not support wildcards, filters, or negative indices.
func extractJSONArray(body []byte, path string) ([]json.RawMessage, error) {
	var current interface{}
	if err := json.Unmarshal(body, &current); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	steps, err := parseJSONPath(path)
	if err != nil {
		return nil, err
	}
	for i, step := range steps {
		switch step.kind {
		case pathKey:
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: expected object before key %q", path, i+1, step.key)
			}
			v, ok := m[step.key]
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: key %q not found", path, i+1, step.key)
			}
			current = v
		case pathIndex:
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: expected array before index [%d]", path, i+1, step.index)
			}
			if step.index < 0 || step.index >= len(arr) {
				return nil, fmt.Errorf("path %q: step %d: index [%d] out of range (len=%d)", path, i+1, step.index, len(arr))
			}
			current = arr[step.index]
		}
	}
	arr, ok := current.([]interface{})
	if !ok {
		return nil, fmt.Errorf("path %q: expected array at the end", path)
	}
	out := make([]json.RawMessage, 0, len(arr))
	for _, v := range arr {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

type pathStepKind int

const (
	pathKey   pathStepKind = iota // look up `key` in the current object
	pathIndex                     // take element `index` of the current array
)

type pathStep struct {
	kind  pathStepKind
	key   string
	index int
}

// parseJSONPath parses a dot-separated path with optional "[N]"
// index suffixes into a sequence of steps. See extractJSONArray for
// the grammar.
func parseJSONPath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, nil
	}
	var steps []pathStep
	for _, segment := range strings.Split(path, ".") {
		// Split "key[N1][N2]..." into key + indices.
		i := strings.IndexByte(segment, '[')
		var key string
		var indices []int
		if i < 0 {
			key = segment
		} else {
			key = segment[:i]
			rest := segment[i:]
			for len(rest) > 0 {
				if rest[0] != '[' {
					return nil, fmt.Errorf("path %q: malformed segment %q (expected '[')", path, segment)
				}
				end := strings.IndexByte(rest, ']')
				if end < 0 {
					return nil, fmt.Errorf("path %q: malformed segment %q (unmatched '[')", path, segment)
				}
				idx, err := strconv.Atoi(rest[1:end])
				if err != nil {
					return nil, fmt.Errorf("path %q: invalid index %q in segment %q", path, rest[1:end], segment)
				}
				if idx < 0 {
					return nil, fmt.Errorf("path %q: negative index %d in segment %q", path, idx, segment)
				}
				indices = append(indices, idx)
				rest = rest[end+1:]
			}
		}
		if key != "" {
			steps = append(steps, pathStep{kind: pathKey, key: key})
		}
		for _, idx := range indices {
			steps = append(steps, pathStep{kind: pathIndex, index: idx})
		}
	}
	return steps, nil
}

// htmlSelector is a small subset of CSS selectors we support for the
// "html" source type. The grammar is intentionally tiny:
//
//	tag                 - any element with that tag name
//	tag.class           - element with that exact class
//	tag.class1.class2   - element with all of those classes (AND)
//
// The tag is matched case-insensitively against element names, and
// classes are matched exactly (case-sensitively) against tokens in the
// element's class attribute. The grammar is just enough to cover the
// concrete cases that motivated the html source (e.g. WordPress-style
// "li.attachedfile" attached-file lists). It is NOT a full CSS
// selector engine.
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

func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

// resolveGhBinary returns an absolute path to the gh executable, or
// an error if it can't be found.
//
// Resolution order:
//  1. If --gh was passed on the command line, use that.
//  2. exec.LookPath("gh") against the current PATH.
//  3. Common tool-manager locations (mise, asdf, rtx, Homebrew).
//     This matters when checkdiff is run by launchd, whose default
//     PATH doesn't include ~/.local/bin or similar.
func resolveGhBinary() (string, error) {
	if *flagGhPath != "" {
		if _, err := exec.LookPath(*flagGhPath); err == nil {
			return *flagGhPath, nil
		}
		// Allow a direct absolute path even if LookPath would refuse.
		if isExecutableFile(*flagGhPath) {
			return *flagGhPath, nil
		}
		return "", fmt.Errorf("--gh %q is not an executable file", *flagGhPath)
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		// Each entry may be a literal path or contain a single "*"
		// glob (handled in the loop below).
		candidates := []string{
			// mise: ~/.local/share/mise/installs/gh/<version>/*/bin/gh
			// e.g. .../gh/2.94.0/gh_2.94.0_macOS_arm64/bin/gh
			// or  .../gh/latest/gh_2.94.0_macOS_arm64/bin/gh
			filepath.Join(home, ".local/share/mise/installs/gh", "*", "*", "bin", "gh"),
			filepath.Join(home, ".asdf/shims/gh"),
			// rtx: ~/.local/share/rtx/installs/gh/<version>/bin/gh
			filepath.Join(home, ".local/share/rtx/installs/gh", "*", "bin", "gh"),
			"/opt/homebrew/bin/gh",
			"/usr/local/bin/gh",
		}
		for _, c := range candidates {
			if strings.Contains(c, "*") {
				matches, _ := filepath.Glob(c)
				// Pick the lexically last match (mise/rtx use version
				// directories where the highest version sorts last).
				sort.Strings(matches)
				for i := len(matches) - 1; i >= 0; i-- {
					if isExecutableFile(matches[i]) {
						return matches[i], nil
					}
				}
			} else if isExecutableFile(c) {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("gh CLI not found (set --gh, or install gh in PATH / Homebrew / mise / asdf)")
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}
