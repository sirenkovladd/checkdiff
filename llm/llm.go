// Package llm generates short human-readable summaries of page
// changes for the page_llm source type. It talks to an
// OpenAI-compatible /chat/completions endpoint or an Anthropic
// /messages endpoint — both of which the opencode Go
// subscription exposes under https://opencode.ai/zen/go/v1
// (see https://opencode.ai/docs/go#endpoints).
//
// The API key is never stored in config.toml. The [llm] block
// only names WHERE the key lives: an env var (api_key_env,
// e.g. "OPENCODE_API_KEY") or a file (api_key_file, either
// the raw key or a JSON object with an api_key/apiKey/key/
// token field). ResolveAPIKey reads it at startup; the daemon
// logs once when it's missing instead of failing every check
// (Format falls back to a static from/to diff so the change
// notification still fires).
package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client summarizes old-vs-new page text. Provider selects the
// protocol: "openai" (POST {server}/chat/completions),
// "anthropic" (POST {server}/messages), or "responses" (POST
// {server}/responses, the OpenAI Responses API). Server is the
// base URL without a trailing operation path, e.g.
// "https://opencode.ai/zen/go/v1" — which exposes all three,
// with different models on each (see
// https://opencode.ai/docs/go#endpoints; e.g. Muse Spark and
// Grok live on /responses, GLM/Kimi on /chat/completions).
type Client struct {
	Provider  string
	Server    string
	Model     string
	APIKey    string
	MaxTokens int
	http      *http.Client
}

// New builds a Client. An empty maxTokens defaults to 300
// (a 2-3 sentence summary never needs more). The http timeout
// is 60s — LLM calls are slower than page fetches.
func New(provider, server, model, apiKey string, maxTokens int) *Client {
	if maxTokens <= 0 {
		maxTokens = 300
	}
	return &Client{
		Provider:  provider,
		Server:    strings.TrimRight(server, "/"),
		Model:     model,
		APIKey:    apiKey,
		MaxTokens: maxTokens,
		http:      &http.Client{Timeout: 60 * time.Second},
	}
}

// ResolveAPIKey returns the API key from an env var and/or a
// file. The env var wins when both are set and non-empty.
// apiKeyPath is an optional dot-separated path into a JSON
// key file (e.g. "opencode-go.key" for a multi-provider store
// like ~/.pi/agent/auth.json). With a path set, the file must
// be JSON and the path must resolve to a non-empty string.
// Without a path, a file ending in .json (or containing JSON)
// is parsed for an api_key / apiKey / key / token field; any
// other file is treated as the raw key with surrounding
// whitespace trimmed. Empty env name / file path are skipped,
// so callers can pass the config values straight through.
func ResolveAPIKey(apiKeyEnv, apiKeyFile, apiKeyPath string) string {
	if apiKeyEnv != "" {
		if v := strings.TrimSpace(os.Getenv(apiKeyEnv)); v != "" {
			return v
		}
	}
	if apiKeyFile != "" {
		b, err := os.ReadFile(apiKeyFile)
		if err != nil {
			log.Printf("llm: read api_key_file: %v", err)
			return ""
		}
		if apiKeyPath != "" {
			k, err := keyAtPath(b, apiKeyPath)
			if err != nil {
				log.Printf("llm: api_key_path %q: %v", apiKeyPath, err)
				return ""
			}
			return k
		}
		if k := parseKeyFile(b); k != "" {
			return k
		}
	}
	return ""
}

// keyAtPath extracts a string at a dot-separated path (e.g.
// "opencode-go.key") from JSON file bytes. Each segment is a
// literal object key (no escaping, no array indices — key
// stores don't need them). Returns an error when the file
// isn't JSON, a segment is missing, or the landing value
// isn't a non-empty string.
func keyAtPath(b []byte, path string) (string, error) {
	var root any
	if err := json.Unmarshal(bytes.TrimSpace(b), &root); err != nil {
		return "", fmt.Errorf("not JSON: %w", err)
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("segment %q: not an object", seg)
		}
		cur, ok = obj[seg]
		if !ok {
			return "", fmt.Errorf("segment %q: missing", seg)
		}
	}
	s, ok := cur.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("path %q: not a non-empty string", path)
	}
	return strings.TrimSpace(s), nil
}

// parseKeyFile extracts a key from file bytes: JSON object
// with a known field first, raw trimmed text as fallback.
func parseKeyFile(b []byte) string {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			for _, k := range []string{"api_key", "apiKey", "key", "token"} {
				if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return strings.TrimSpace(string(b))
}

// Summarize asks the model for a 2-3 sentence plain-text
// summary of what changed between oldText and newText.
// customPrompt, when non-empty, is appended as an extra
// instruction (per-source `prompt` field). An empty oldText
// means "no previous baseline text available" (shouldn't
// normally happen — check.One only formats real diffs).
//
// The signature intentionally uses only stdlib types so the
// source package can consume it through a small interface
// without importing this package's config types (which would
// be an import cycle: config imports source).
func (c *Client) Summarize(ctx context.Context, sourceName, pageURL, oldText, newText, customPrompt string) (string, error) {
	if c.Model == "" {
		return "", fmt.Errorf("llm: no model configured")
	}
	// Bound the prompt: the fetcher already caps stored text
	// at ~12k chars, but clamp again here so a hand-edited
	// state file can't blow up the request body.
	oldText = truncateForPrompt(oldText, 6000)
	newText = truncateForPrompt(newText, 6000)

	system := "You summarize web page changes for a change-notification bot. " +
		"Reply with 2-3 short sentences of plain text, no markdown headers, no bullet lists. " +
		"Say what actually changed that matters to someone tracking this page (status flips like " +
		"'launching soon' to 'live', new dates, prices, availability, new entries). " +
		"If nothing meaningful changed (only boilerplate/ads), say so in one sentence."
	user := fmt.Sprintf("Page: %s (%s)\n\nOLD TEXT:\n%s\n\nNEW TEXT:\n%s",
		sourceName, pageURL, oldText, newText)
	if strings.TrimSpace(customPrompt) != "" {
		user += "\n\nExtra instruction from the user: " + strings.TrimSpace(customPrompt)
	}
	user += "\n\nSummarize the meaningful change in 2-3 sentences:"

	session := sessionID(sourceName, pageURL)
	switch c.Provider {
	case "anthropic":
		return c.summarizeAnthropic(ctx, session, system, user)
	case "responses":
		return c.summarizeResponses(ctx, session, system, user)
	default: // "openai" and anything unknown fall back to the OpenAI shape
		return c.summarizeOpenAI(ctx, session, system, user)
	}
}

// sessionID derives a stable per-source session id for the
// x-opencode-session header the opencode Go endpoints require
// (see https://opencode.ai/docs/go#where-can-i-use-it).
// Deterministic in sourceName+pageURL, so restarts keep the
// same session per source (good for prompt caching) while
// different sources don't share one.
func sessionID(sourceName, pageURL string) string {
	h := sha256.Sum256([]byte(sourceName + "\n" + pageURL))
	return "checkdiff-" + hex.EncodeToString(h[:])[:32]
}

// responsesBudget floors the output budget for the Responses
// API. Reasoning models spend the budget on reasoning tokens
// first (a 4239-char page diff exhausted a 1024 budget with
// zero text out at high reasoning effort), so anything small
// risks an "incomplete" response with no summary. 4096 gives
// ample headroom; cost follows tokens actually generated, not
// the cap, and the prompt still holds the visible reply to 2-3
// sentences.
func responsesBudget(maxTokens int) int {
	if maxTokens < 4096 {
		return 4096
	}
	return maxTokens
}

func truncateForPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// --- OpenAI-compatible /chat/completions ---

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) summarizeOpenAI(ctx context.Context, session, system, user string) (string, error) {
	endpoint := c.Server + "/chat/completions"
	reqBody, _ := json.Marshal(openAIRequest{
		Model: c.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens:   c.MaxTokens,
		Temperature: 0.3,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
	req.Header.Set("x-opencode-session", session)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("llm (openai): %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed openAIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("llm (openai): decode: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm (openai): %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm (openai): no choices in response")
	}
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", fmt.Errorf("llm (openai): empty summary")
	}
	return out, nil
}

// --- OpenAI Responses API (/responses) ---
// Used by the models the opencode Go subscription serves on
// https://opencode.ai/zen/go/v1/responses (Grok, GPT Luna,
// Muse Spark). Request takes instructions + input; the text
// comes back in output[].content[] blocks of type
// "output_text".

type responsesRequest struct {
	Model           string `json:"model"`
	Instructions    string `json:"instructions,omitempty"`
	Input           string `json:"input"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	// No temperature: the gateway normalizes sampling itself
	// (it echoed temperature:1 back at us); sending one only
	// suggests control we don't have.
}

type responsesResponse struct {
	Status     string `json:"status"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) summarizeResponses(ctx context.Context, session, system, user string) (string, error) {
	endpoint := c.Server + "/responses"
	reqBody, _ := json.Marshal(responsesRequest{
		Model:           c.Model,
		Instructions:    system,
		Input:           user,
		MaxOutputTokens: responsesBudget(c.MaxTokens),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
	req.Header.Set("x-opencode-session", session)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("llm (responses): %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed responsesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("llm (responses): decode: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm (responses): %s", parsed.Error.Message)
	}
	var sb strings.Builder
	for _, item := range parsed.Output {
		if item.Type != "message" {
			continue // e.g. "reasoning" items carry no text
		}
		for _, block := range item.Content {
			if block.Type == "output_text" && strings.TrimSpace(block.Text) != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(strings.TrimSpace(block.Text))
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		if parsed.Status == "incomplete" && parsed.Incomplete != nil {
			return "", fmt.Errorf("llm (responses): incomplete response (%s) — raise max_tokens", parsed.Incomplete.Reason)
		}
		return "", fmt.Errorf("llm (responses): empty summary")
	}
	return out, nil
}

// --- Anthropic /messages ---

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    string `json:"system,omitempty"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) summarizeAnthropic(ctx context.Context, session, system, user string) (string, error) {
	endpoint := c.Server + "/messages"
	var reqBody anthropicRequest
	reqBody.Model = c.Model
	reqBody.MaxTokens = c.MaxTokens
	reqBody.System = system
	reqBody.Messages = []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{{Role: "user", Content: user}}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	req.Header.Set("User-Agent", "checkdiff/0.1 (+https://github.com)")
	req.Header.Set("x-opencode-session", session)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("llm (anthropic): %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("llm (anthropic): decode: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm (anthropic): %s", parsed.Error.Message)
	}
	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(strings.TrimSpace(block.Text))
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("llm (anthropic): empty summary")
	}
	return out, nil
}
