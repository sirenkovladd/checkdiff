package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const trendingFixtureTwoRepos = `<!DOCTYPE html><html><body>` +
	`<article class="Box-row">` +
	`<h2 class="h3 lh-condensed"><a href="/owner1/repo1"> owner1 / repo1 </a></h2>` +
	`<p>First repo description</p>` +
	`</article>` +
	`<article class="Box-row">` +
	`<h2 class="h3 lh-condensed"><a href="/Owner2/Repo2"> Owner2 / Repo2 </a></h2>` +
	`</article>` +
	`<article class="Box-row">` +
	`<h2 class="h3 lh-condensed"><span>No link here</span></h2>` +
	`</article>` +
	`</body></html>`

func TestParseTrendingRepos(t *testing.T) {
	repos, err := parseTrendingRepos([]byte(trendingFixtureTwoRepos))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	if repos[0].FullName != "owner1/repo1" {
		t.Errorf("repos[0].FullName = %q, want owner1/repo1", repos[0].FullName)
	}
	if repos[0].URL != "https://github.com/owner1/repo1" {
		t.Errorf("repos[0].URL = %q, want https://github.com/owner1/repo1", repos[0].URL)
	}
	if repos[1].FullName != "Owner2/Repo2" {
		t.Errorf("repos[1].FullName = %q, want Owner2/Repo2 (href casing preserved)", repos[1].FullName)
	}
}

func TestParseTrendingReposEmpty(t *testing.T) {
	_, err := parseTrendingRepos([]byte(`<html><body><p>redesign</p></body></html>`))
	if err == nil {
		t.Fatal("expected error for page with no trending repos, got nil")
	}
	if !strings.Contains(err.Error(), "GitHub HTML may have changed") {
		t.Errorf("error should hint at HTML change, got %q", err.Error())
	}
}

func trendingWindowServer(t *testing.T, byWindow map[string]string, seenQueries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if seenQueries != nil {
			*seenQueries = append(*seenQueries, r.URL.RawQuery)
		}
		body, ok := byWindow[q.Get("since")]
		if !ok {
			http.Error(w, "missing since", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
}

func trendingArticle(href, name string) string {
	return `<article class="Box-row"><h2 class="h3 lh-condensed"><a href="` + href + `">` + name + `</a></h2></article>`
}

func TestFetchTrendingThreeWindows(t *testing.T) {
	srv := trendingWindowServer(t, map[string]string{
		"daily":   `<html><body>` + trendingArticle("/ownerA/repoA", "ownerA / repoA") + trendingArticle("/ownerB/repoB", "ownerB / repoB") + `</body></html>`,
		"weekly":  `<html><body>` + trendingArticle("/ownerB/repoB", "ownerB / repoB") + trendingArticle("/ownerC/repoC", "ownerC / repoC") + `</body></html>`,
		"monthly": `<html><body>` + trendingArticle("/ownerC/repoC", "ownerC / repoC") + trendingArticle("/ownerD/repoD", "ownerD / repoD") + `</body></html>`,
	}, nil)
	defer srv.Close()

	s := &Source{ID: "t", Name: "Trending", Type: "github_trending", URL: srv.URL + "/trending"}
	items, err := (githubTrendingFetcher{}).Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// 6 rows across 3 windows, 2 dupes → 4 unique.
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4 unique", len(items))
	}
	wantWindow := map[string]string{
		"ownera/repoa": "daily",
		"ownerb/repob": "daily", // first-window-wins, not weekly
		"ownerc/repoc": "weekly",
		"ownerd/repod": "monthly",
	}
	for _, it := range items {
		w, ok := wantWindow[it.ID]
		if !ok {
			t.Errorf("unexpected item ID %q", it.ID)
			continue
		}
		if it.Body != w {
			t.Errorf("item %q window = %q, want %q", it.ID, it.Body, w)
		}
		if it.Link != "https://github.com/"+it.Title {
			t.Errorf("item %q link = %q, want repo URL", it.ID, it.Link)
		}
	}
}

func TestFetchTrendingPreservesQuery(t *testing.T) {
	var queries []string
	srv := trendingWindowServer(t, map[string]string{
		"daily":   `<html><body>` + trendingArticle("/a/b", "a / b") + `</body></html>`,
		"weekly":  `<html><body>` + trendingArticle("/a/b", "a / b") + `</body></html>`,
		"monthly": `<html><body>` + trendingArticle("/a/b", "a / b") + `</body></html>`,
	}, &queries)
	defer srv.Close()

	s := &Source{ID: "t", Name: "T", Type: "github_trending", URL: srv.URL + "/trending?language=go"}
	f := githubTrendingFetcher{}
	if _, err := f.Fetch(context.Background(), s, time.Now()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("got %d requests, want 3", len(queries))
	}
	for _, q := range queries {
		if !strings.Contains(q, "language=go") {
			t.Errorf("query %q should preserve language=go", q)
		}
		if !strings.Contains(q, "since=") {
			t.Errorf("query %q should set since=", q)
		}
	}
}

func TestValidateTrendingDefaults(t *testing.T) {
	s := &Source{ID: "t", Name: "T", Type: "github_trending"}
	if err := (githubTrendingFetcher{}).Validate(s); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.URL != "https://github.com/trending" {
		t.Errorf("URL = %q, want default trending URL", s.URL)
	}
	if s.ForgetAfter != "720h" {
		t.Errorf("ForgetAfter = %q, want 720h default", s.ForgetAfter)
	}

	bad := &Source{ID: "t", Name: "T", Type: "github_trending", ForgetAfter: "not-a-duration"}
	if err := (githubTrendingFetcher{}).Validate(bad); err == nil {
		t.Error("expected error for bad forget_after, got nil")
	}
}

func trendingItem(id, title, window string) Item {
	return Item{ID: id, Title: title, Body: window, Link: "https://github.com/" + title}
}

func TestFormatTrendingGroupsByWindow(t *testing.T) {
	s := &Source{ID: "t", Name: "GH Trends", Type: "github_trending", URL: "https://github.com/trending"}
	added := []Item{
		trendingItem("ownera/repoa", "ownerA/repoA", "weekly"),
		trendingItem("ownerb/repob", "ownerB/repoB", "daily"),
		trendingItem("ownerc/repoc", "ownerC/repoC", "daily"),
	}
	n := (githubTrendingFetcher{}).Format(context.Background(), s, added, nil)

	if !strings.Contains(n.Title, "3 new repos") {
		t.Errorf("title = %q, want '3 new repos'", n.Title)
	}
	// Daily section must come before Weekly section.
	di := strings.Index(n.Body, "Daily:")
	wi := strings.Index(n.Body, "Weekly:")
	if di < 0 || wi < 0 {
		t.Fatalf("body should have Daily: and Weekly: sections, got %q", n.Body)
	}
	if di > wi {
		t.Errorf("Daily: section should precede Weekly:, got %q", n.Body)
	}
	if strings.Contains(n.Body, "Monthly:") {
		t.Errorf("empty Monthly window should be omitted, got %q", n.Body)
	}
	for _, want := range []string{
		"[ownerA/repoA](https://github.com/ownerA/repoA)",
		"[ownerB/repoB](https://github.com/ownerB/repoB)",
	} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("body should contain %q, got %q", want, n.Body)
		}
	}
	if n.Tags != "github" {
		t.Errorf("tags = %q, want github", n.Tags)
	}
	if n.Click != "https://github.com/trending" {
		t.Errorf("click = %q, want trending page", n.Click)
	}
	if n.Priority != "default" {
		t.Errorf("priority = %q, want default for 3 items", n.Priority)
	}
}

func TestFormatTrendingHighPriority(t *testing.T) {
	s := &Source{ID: "t", Name: "T", Type: "github_trending", URL: "https://github.com/trending"}
	var added []Item
	for i := 0; i < 6; i++ {
		id := strings.Repeat("a", i+1) + "/r"
		added = append(added, trendingItem(id, id, "daily"))
	}
	n := (githubTrendingFetcher{}).Format(context.Background(), s, added, nil)
	if n.Priority != "high" {
		t.Errorf("priority = %q, want high for 6+ items", n.Priority)
	}
}
