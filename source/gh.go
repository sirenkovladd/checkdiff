package source

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ghPathOverride is the explicit --gh CLI flag value. The
// daemon sets it once at startup via SetGhPath; the github_file
// fetcher reads it via GhPath at fetch time. Reading is
// concurrent-safe because the value is set before the fetcher
// goroutines start; writes happen during startup, before
// concurrent reads.
//
// Kept as a plain string (not atomic) because the write/read
// ordering is guaranteed by the daemon's startup sequence. If a
// future change makes the path dynamic (e.g. mid-run reload),
// wrap in atomic.Pointer[string] or a sync.RWMutex.
var ghPathOverride string

// SetGhPath records the explicit gh binary path. Typically
// called from main after flag.Parse. Empty string clears the
// override and falls back to PATH + tool-manager lookup.
func SetGhPath(p string) { ghPathOverride = p }

// GhPath returns the currently-configured explicit gh path.
// Tests can read this to assert what was configured, and the
// github_file fetcher passes it to ResolveGhBinary.
func GhPath() string { return ghPathOverride }

// ResolveGhBinary returns an absolute path to the gh executable,
// or an error if it can't be found. ghPath is the explicit
// override (typically the value of the --gh CLI flag); pass ""
// to skip the override and use the auto-discovered PATH and
// tool-manager locations.
//
// Resolution order:
//  1. ghPath, if non-empty. LookPath first (so a bare "gh"
//     works), then a direct stat (so an absolute path works
//     even when PATH is empty — e.g. when checkdiff runs under
//     launchd).
//  2. exec.LookPath("gh") against the current PATH.
//  3. Common tool-manager locations (mise, asdf, rtx,
//     Homebrew). This matters when checkdiff is run by launchd,
//     whose default PATH doesn't include ~/.local/bin or
//     similar.
func ResolveGhBinary(ghPath string) (string, error) {
	if ghPath != "" {
		if _, err := exec.LookPath(ghPath); err == nil {
			return ghPath, nil
		}
		// Allow a direct absolute path even if LookPath would
		// refuse.
		if isExecutableFile(ghPath) {
			return ghPath, nil
		}
		return "", fmt.Errorf("--gh %q is not an executable file", ghPath)
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		// Each entry may be a literal path or contain a single
		// "*" glob (handled in the loop below).
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
				// Pick the lexically last match (mise/rtx use
				// version directories where the highest version
				// sorts last).
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
