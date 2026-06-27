package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"checkdiff/template"
)

// githubFileFetcher implements the Fetcher interface for the
// "github_file" source type: one file in a GitHub repo, tracked
// by its git blob SHA. The SHA is stable across cosmetic changes
// (whitespace, line endings) and changes only when the file
// content changes, which is exactly the property we want.
type githubFileFetcher struct{}

// Type returns the registry key for this fetcher.
func (githubFileFetcher) Type() string { return "github_file" }

// githubContents is the subset of the response from
// `gh api repos/{owner}/{repo}/contents/{path}?ref={ref}` we
// care about.
type githubContents struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// Fetch retrieves the current contents of a file in a GitHub
// repo via the gh CLI. The git blob SHA returned by GitHub is
// the diff key — a notification fires when the file content
// changes.
//
// If the file is missing the SHA (older API responses, or
// non-GitHub), the fetcher falls back to a sha256 of the
// decoded content. Either way, the returned Item has exactly
// one entry.
func (githubFileFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	if fetchVerbose {
		log.Printf("[%s] fetch: gh api repos/%s/%s/contents/%s?ref=%s", s.ID, s.Owner, s.Repo, s.Path, s.Ref)
	}
	gh, err := ResolveGhBinary(GhPath())
	if err != nil {
		return nil, err
	}
	// Apply URL templates to the GitHub fields before composing
	// the API path. This lets a source use {{.UnixMilli}} in
	// owner/repo/ref/path to bust caches on APIs that key by
	// those values.
	owner := template.Render(s.Owner, now)
	repo := template.Render(s.Repo, now)
	ref := template.Render(s.Ref, now)
	path := template.Render(s.Path, now)
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		owner, repo, path, ref)

	// Use --jq to keep gh's output tight and to fail fast on
	// errors. We pull the whole JSON object back; it's small
	// even for big files because the content is just base64.
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

	// Prefer the git blob SHA from GitHub (changes only when
	// content changes). Fall back to sha256 of the decoded
	// content.
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

// Validate applies the github_file-specific defaults: defaults
// Ref to "HEAD" and synthesises URL from owner/repo/ref/path if
// the user didn't supply one.
func (githubFileFetcher) Validate(s *Source) error {
	if s.Owner == "" || s.Repo == "" || s.Path == "" {
		return fmt.Errorf("github_file requires owner, repo, path (type=%q)", s.Type)
	}
	if s.Ref == "" {
		s.Ref = "HEAD"
	}
	if s.URL == "" {
		s.URL = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", s.Owner, s.Repo, s.Ref, s.Path)
	}
	return nil
}

// Format builds a single "file changed" notification. The old
// git blob SHA isn't meaningful to the user, so the format
// helper ignores it and renders the new file's title + a
// content excerpt.
func (githubFileFetcher) Format(ctx context.Context, s *Source, added, removed []Item) Notification {
	const maxBody = 3500
	var b strings.Builder
	fmt.Fprintf(&b, "File changed: %s\n", s.Name)
	fmt.Fprintf(&b, "Source: %s\n\n", s.URL)
	for i, it := range added {
		if i >= 10 {
			fmt.Fprintf(&b, "\n… and %d more\n", len(added)-i)
			break
		}
		fmt.Fprintf(&b, "• %s\n", it.Title)
		if it.Body != "" {
			excerpt := it.Body
			if len(excerpt) > 400 {
				excerpt = excerpt[:400] + "…"
			}
			fmt.Fprintf(&b, "  %s\n", excerpt)
		}
		if b.Len() > maxBody {
			b.WriteString("\n…(body truncated)\n")
			break
		}
	}

	// Look up the latest commit so the notification can
	// include the SHA, author, date, and a link straight to
	// the commit. A lookup failure is logged but doesn't
	// fail the notification — the body is still useful with
	// just the file path and excerpt. A small budget caps
	// the time we'll spend on the commit lookup so a slow
	// gh can't stall the publish path.
	var commitSection string
	clickURL := s.URL
	if cctx, cancel := context.WithTimeout(ctx, 5*time.Second); cancel != nil {
		if commit, cerr := GetLatestCommit(cctx, s); cerr == nil && commit != nil {
			shortSHA := commit.SHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			firstLine := commit.Commit.Message
			if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
				firstLine = firstLine[:i]
			}
			author := commit.Commit.Author.Name
			date := commit.Commit.Author.Date
			if len(date) >= 10 {
				// YYYY-MM-DD only; the full timestamp is
				// noise for a notification body.
				date = date[:10]
			}
			commitSection = fmt.Sprintf(
				"\nLast commit: %s — %s\nBy %s on %s\n%s\n",
				shortSHA, firstLine, author, date, commit.HTMLURL,
			)
			clickURL = commit.HTMLURL
		} else if cerr != nil {
			log.Printf("[%s] commit lookup for notification body failed: %v", s.ID, cerr)
		}
		cancel()
	}

	return Notification{
		Title:    fmt.Sprintf("🔔 %s: changed", s.Name),
		Body:     b.String() + commitSection,
		Priority: "default",
		Tags:     "loudspeaker",
		Click:    clickURL,
	}
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
