package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"checkdiff/template"
	"golang.org/x/net/html"
)

// defaultTrendingURL is the base page fetched when the source
// has no URL. Existing query params on the URL (e.g.
// ?language=go) are preserved and a ?since=<window> param is
// set per request, so one check performs three GETs.
const defaultTrendingURL = "https://github.com/trending"

// defaultForgetAfter is the sliding-window TTL applied when
// the source leaves forget_after empty: a repo absent from
// all three windows for 30 days is forgotten and may notify
// again if it re-trends.
const defaultForgetAfter = "720h"

// trendingWindows are fetched in this order on every check.
// The order doubles as display order and as first-window-wins
// priority when the same repo appears in several windows.
var trendingWindows = []string{"daily", "weekly", "monthly"}

// trendingRepo is one parsed row of a GitHub trending page.
// FullName is "owner/repo" as spelled in the link href;
// URL is the absolute repo URL.
type trendingRepo struct {
	FullName string
	URL      string
}

type githubTrendingFetcher struct{}

// Type returns the registry key for this fetcher.
func (githubTrendingFetcher) Type() string { return "github_trending" }

// Fetch retrieves the current trending repos for the daily,
// weekly, and monthly windows. The three pages are fetched
// sequentially from the source's base URL (default
// https://github.com/trending) with ?since=<window> set and
// any other query params (e.g. language=go) preserved.
//
// Items carry the window in Body ("daily"|"weekly"|"monthly")
// so Format can group them; the ID is the lowercased
// "owner/repo" so the seen-cache dedups globally across
// windows. A repo in several windows appears once, tagged
// with the first window (daily beats weekly beats monthly).
func (githubTrendingFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	base := template.Render(s.URL, now)
	if base == "" {
		base = defaultTrendingURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("bad trending URL %q: %w", base, err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	seen := map[string]bool{}
	var items []Item
	for _, window := range trendingWindows {
		u := *baseURL
		q := u.Query()
		q.Set("since", window)
		u.RawQuery = q.Encode()

		if fetchVerbose {
			log.Printf("[%s] fetch: GET %s (window=%s)", s.ID, u.String(), window)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
		status := resp.StatusCode
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("GET %s: %s", u.String(), resp.Status)
		}

		repos, err := parseTrendingRepos(body)
		if err != nil {
			return nil, fmt.Errorf("window %s: %w", window, err)
		}
		for _, r := range repos {
			id := strings.ToLower(r.FullName)
			if seen[id] {
				continue
			}
			seen[id] = true
			items = append(items, Item{
				ID:    id,
				Title: r.FullName,
				Body:  window,
				Link:  r.URL,
			})
		}
	}
	if fetchVerbose {
		log.Printf("[%s] fetch ok: %d unique repos across 3 windows", s.ID, len(items))
	}
	return items, nil
}

// Validate applies the github_trending defaults: URL defaults
// to https://github.com/trending and forget_after defaults to
// "720h". A non-empty forget_after must parse as a Go duration
// and be positive.
func (githubTrendingFetcher) Validate(s *Source) error {
	if s.URL == "" {
		s.URL = defaultTrendingURL
	}
	if s.ForgetAfter == "" {
		s.ForgetAfter = defaultForgetAfter
	}
	d, err := time.ParseDuration(s.ForgetAfter)
	if err != nil {
		return fmt.Errorf("github_trending: bad forget_after %q: %w", s.ForgetAfter, err)
	}
	if d <= 0 {
		return fmt.Errorf("github_trending: forget_after must be positive (got %q)", s.ForgetAfter)
	}
	return nil
}

// Format builds the single digest notification: one section per
// window that has new repos, each repo a markdown link line.
// There is intentionally no truncation — on spike days the
// ntfy server delivers the (large) body as an attachment file.
func (githubTrendingFetcher) Format(_ context.Context, s *Source, added, _ []Item) Notification {
	byWindow := map[string][]Item{}
	for _, it := range added {
		byWindow[it.Body] = append(byWindow[it.Body], it)
	}

	var b strings.Builder
	count := len(added)
	if count == 1 {
		fmt.Fprintf(&b, "1 new repo on GitHub trending:\n")
	} else {
		fmt.Fprintf(&b, "%d new repos on GitHub trending:\n", count)
	}
	for _, window := range trendingWindows {
		repos := byWindow[window]
		if len(repos) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s:\n", strings.Title(window))
		for _, it := range repos {
			title := it.Title
			if title == "" {
				title = it.ID
			}
			if it.Link != "" {
				fmt.Fprintf(&b, "- [%s](%s)\n", title, it.Link)
			} else {
				fmt.Fprintf(&b, "- %s\n", title)
			}
		}
	}

	title := fmt.Sprintf("GitHub trending: %d new repos", count)
	if count == 1 {
		title = "GitHub trending: 1 new repo"
	}
	priority := "default"
	if count >= 6 {
		priority = "high"
	}
	return Notification{
		Title:    fmt.Sprintf("📈 %s: %s", s.Name, title),
		Body:     strings.TrimRight(b.String(), "\n"),
		Priority: priority,
		Tags:     "github",
		Click:    "https://github.com/trending",
	}
}

// parseTrendingRepos extracts the repo list from one GitHub
// trending page. Each repo is an <article class="Box-row">
// whose heading link points at "/owner/repo". The FullName is
// derived from the href (not the link text, whose "owner /
// repo" spacing varies); the URL is the absolute repo URL.
//
// Returns an error when no repos are found — the page is then
// either an error/redirect or GitHub redesigned the markup,
// and the caller surfaces that loudly instead of recording an
// empty baseline.
func parseTrendingRepos(body []byte) ([]trendingRepo, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	var out []trendingRepo
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "article" && hasClass(n, "Box-row") {
			if href := trendingArticleHref(n); href != "" {
				name := trendingNameFromHref(href)
				if name != "" {
					out = append(out, trendingRepo{
						FullName: name,
						URL:      "https://github.com/" + name,
					})
				}
			}
			// Don't descend into the article: one article
			// is one repo, and nested matches would double
			// count it.
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(out) == 0 {
		return nil, errors.New("trending list not found (GitHub HTML may have changed)")
	}
	return out, nil
}

// trendingArticleHref returns the "/owner/repo" href of the
// heading link inside a trending article, or "". It prefers
// the first link inside an h1/h2/h3 (the repo title); if the
// article has no heading, it falls back to the first
// two-segment link anywhere inside the article.
func trendingArticleHref(article *html.Node) string {
	var heading *html.Node
	var findHeading func(*html.Node)
	findHeading = func(n *html.Node) {
		if heading != nil {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "h1" || n.Data == "h2" || n.Data == "h3") {
			heading = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findHeading(c)
		}
	}
	findHeading(article)

	if heading != nil {
		if href := firstRepoHref(heading); href != "" {
			return href
		}
	}
	return firstRepoHref(article)
}

// firstRepoHref walks n depth-first and returns the first link
// href that looks like "/owner/repo" (exactly two path
// segments), or "".
func firstRepoHref(n *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if found != "" {
			return
		}
		if m.Type == html.ElementNode && m.Data == "a" {
			for _, a := range m.Attr {
				if a.Key == "href" && isRepoHref(a.Val) {
					found = a.Val
					return
				}
			}
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// isRepoHref reports whether href looks like "/owner/repo":
// a root-relative path with exactly two non-empty segments.
// Query strings and fragments are ignored; deeper paths
// (e.g. "/owner/repo/stargazers") are rejected so only the
// repo title link matches.
func isRepoHref(href string) bool {
	if !strings.HasPrefix(href, "/") {
		return false
	}
	path := href
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// trendingNameFromHref derives "owner/repo" from an href that
// passed isRepoHref, preserving the href's casing.
func trendingNameFromHref(href string) string {
	path := href
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return strings.Trim(path, "/")
}
