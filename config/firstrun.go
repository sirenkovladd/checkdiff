package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// ensureConfigForDaemon checks that the config file exists and
// is usable for daemon mode. If the file is missing, a default
// is generated with a random token and the token is printed to
// the log so the user can paste it into the web UI's login
// form.
//
// This is deliberately separate from loadConfig (which only
// reads). A separate function makes the policy explicit:
// auto-generation happens in daemon mode only, with a clear
// log line, and is the first thing the daemon does.
func EnsureForDaemon(path string) error {
	if _, err := os.Stat(path); err == nil {
		// Config exists; nothing to do.
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat config: %w", err)
	}

	token, err := GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}

	body := defaultConfigWithToken(token)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	log.Printf("==========================================================")
	log.Printf("generated new config at %s", path)
	log.Printf("web UI token: %s", token)
	log.Printf("paste the token into the web UI's sign-in form.")
	log.Printf("==========================================================")
	return nil
}

// GenerateToken returns 32 random bytes encoded as base64url
// (no padding). 32 bytes = 256 bits of entropy, more than
// enough for a shared secret on a private network. Exported so
// the webapi package can rotate the token via /api/rotate-token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// defaultConfigWithToken is the starter config the daemon writes
// on first run. It includes a [[sources]] block commented out as
// a worked example, the generated token in [web], and the standard
// ntfy placeholders.
func defaultConfigWithToken(token string) string {
	return fmt.Sprintf(`# checkdiff config — auto-generated on first run.
#
# The web UI token below was generated for this install. To rotate
# it, edit this file and restart the daemon.

[ntfy]
server = "https://ntfy.sh"
topic  = "REPLACE_ME"

[web]
listen = "127.0.0.1:8080"
token  = %q

[check]
# Default interval when a source doesn't set its own check_interval.
check_interval = "1h"

# Optional LLM summarizer for page_llm sources. The key itself
# is never stored here — only where to find it. With no model
# set, page_llm sources send a static from/to diff instead of
# an LLM summary.
#
# [llm]
# provider     = "responses"                    # openai, anthropic, or responses (match the model's endpoint)
# server       = "https://opencode.ai/zen/go/v1" # opencode Go base URL
# model        = "muse-spark-1.3-contributor"
# api_key_env  = "OPENCODE_API_KEY"              # env var holding the key
# api_key_file = "/path/to/key"                 # or a file (raw or JSON)
# api_key_path = "opencode-go.key"              # dot path into a JSON store (e.g. ~/.pi/agent/auth.json)
# max_tokens   = 300

# Each [[sources]] block is one thing to monitor. The example below
# is commented out — uncomment and edit to add your first source.
#
# [[sources]]
# id              = "openrouter-models"
# name            = "OpenRouter Models"
# type            = "json"
# url             = "https://openrouter.ai/api/v1/models"
# enabled         = true
# check_interval  = "30m"
# topic           = "my-own-channel"   # optional; blank = global [ntfy].topic
# items_path      = "data"
# id_field        = "id"
# title_field     = "name"
`, token)
}
