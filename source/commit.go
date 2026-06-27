package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"checkdiff/template"
)

// Commit is a single commit from a GitHub repository, the
// subset of fields the UI uses for the "latest commit that
// touched this file" display. Returned by GetLatestCommit.
//
// The fields are read via gh api, which returns the standard
// GitHub commit shape (commit.message is nested under
// "commit", not at the top level — this is the easy mistake
// to make when looking at the API docs).
type Commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

// GetLatestCommit returns the most recent commit that touched
// s.Path in the s.Owner/s.Repo repository on s.Ref. Used by
// the web UI's "View content" dialog for github_file sources:
// the user wants to see which commit last edited the file
// they're tracking, with a link to that commit on GitHub.
//
// On any failure (gh CLI not found, repo not found, network
// error, no commits matching the path) the error is returned
// and the UI surfaces it. The "no commits" case is treated
// as an error rather than an empty commit because it's
// almost always a misconfiguration (wrong path, wrong
// branch).
func GetLatestCommit(ctx context.Context, s *Source) (*Commit, error) {
	if s.Type != "github_file" {
		return nil, fmt.Errorf("GetLatestCommit: source type %q is not github_file", s.Type)
	}
	gh, err := ResolveGhBinary(GhPath())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	owner := template.Render(s.Owner, now)
	repo := template.Render(s.Repo, now)
	ref := template.Render(s.Ref, now)
	path := template.Render(s.Path, now)
	// per_page=1 because we only need the most recent commit
	// that touched this path; sha=ref constrains to the
	// branch the source is tracking. The path query param
	// is a GitHub-side filter: commits are returned only if
	// they modified that file.
	apiPath := fmt.Sprintf("repos/%s/%s/commits?path=%s&sha=%s&per_page=1", owner, repo, path, ref)
	if fetchVerbose {
		log.Printf("[%s] get latest commit: gh api %s", s.ID, apiPath)
	}
	cmd := exec.CommandContext(ctx, gh, "api", apiPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %v: %s", apiPath, err, strings.TrimSpace(stderr.String()))
	}
	var commits []Commit
	if err := json.Unmarshal(stdout.Bytes(), &commits); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits found for %s/%s path=%s on %s", owner, repo, path, ref)
	}
	return &commits[0], nil
}
