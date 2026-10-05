# checkdiff

A long-running Go daemon that polls a handful of URLs/files and
pushes a notification to [ntfy.sh](https://ntfy.sh) whenever
something has changed. The daemon manages its own per-source
timers internally (no external cronjob required) and exposes a
web UI for managing everything.

## What it watches

Each entry in `~/.config/checkdiff/config.toml` is a "source":

| type          | what it tracks                                                                 |
| ------------- | ------------------------------------------------------------------------------ |
| `github_file` | A file in a GitHub repo, fetched with `gh api`. The git blob SHA is the diff key — a notification fires when the file content changes. |
| `html`        | A web page; elements matching a CSS-ish selector (`h1`..`h4`, `title`, or `tag.class` like `li.attachedfile`) are diffed. Items are tracked by their text content, so **additions and removals of individual entries are detected** (not just "the page changed"). |
| `json`        | A JSON API. A configurable path (`items_path`, default `data`) locates the array of items; `id_field` (default `id`) and `title_field` (default `name`) pick the stable identifier and display name. Optional `link_field` attaches a per-item URL (e.g. a package tracking link) — the notification's Click header opens that URL instead of the source's URL, and the item is rendered as a markdown link in the body. Optional source-level `link` is a static URL for sources where every entry points at the same page (e.g. a single-package tracking source); it wins over the source's `url` when no per-item `Link` is available. Items are tracked by ID, so additions and removals are detected. Useful for sites that are client-rendered (React/Next.js) where the HTML is empty — the public API is the canonical source. |
| `json_value`  | A JSON API whose current state is a single field. The `path` is a dot-separated JSON path (e.g. `body.button_status.notification`) to the scalar value to watch. The value is treated as the entire signal; a change fires a notification ("from X to Y"). |
| `amazon`      | An Amazon shipment tracking page (e.g. `https://www.amazon.ca/gp/your-account/ship-track?itemId=...&orderId=...&shipmentId=...`). The fetcher parses the `pt-status-milestones` div, extracts the current step (the `data-last-reached="true"` milestone's label), the in-progress percent (the next milestone's `data-percent-complete`), and the "Step X of Y" segment from the aria-label. The formatted status string (`"Out for delivery; Step 3 of 4 (51%)"`) is the Item's ID; a change fires a notification. Amazon requires session cookies, so the source config must include the `cookies` field (paste the `Cookie` header value from your browser's dev tools). |
| `page_llm`    | A web page whose main content is summarized by an LLM. The `selector` (default `main`) extracts the page body text; any change fires a notification whose body is a 2-3 sentence LLM summary of old-vs-new (not a raw diff). Needs the global `[llm]` block (`provider` `openai`/`anthropic`/`responses` — match the model's endpoint table, e.g. `responses` for Muse Spark/Grok/GPT Luna; `server`, `model`, key via `api_key_env`/`api_key_file` + optional `api_key_path`) — e.g. an opencode Go subscription at `https://opencode.ai/zen/go/v1` with the key in `OPENCODE_API_KEY`. The client sends `x-opencode-session` (required) derived per-source. Optional per-source `prompt` (extra instruction) and `llm_model` (model override). With no model configured (or when the LLM call fails), the notification falls back to a truncated from/to diff so the change still notifies. |

Each source can set its own `check_interval` — either a Go
duration string (`"30m"`, `"1h"`) **or** a standard 5-field cron
expression (`"0 */6 * * *"`). The format is auto-detected by
the presence of whitespace. Per-source intervals are useful
when a fast-moving changelog should be checked more often than
a slow-moving package tracker. Durations must be at least 15
seconds (fixed-interval polling of the same URL more often
than that would hammer the target).

Each source can also override the ntfy topic with a per-source
`topic` field (blank or missing = the global `[ntfy] topic`).
This routes a single source's notifications to its own ntfy
channel — e.g. a fast-polling registration watch gets a
channel of its own so its messages don't drown out the rest.
The ntfy server is always the global one; only the channel
differs.

Source URLs can include `{{...}}` placeholders for cache-busting
or timestamp injection. Supported placeholders:

| Placeholder       | Replaced with                                  |
| ----------------- | ---------------------------------------------- |
| `{{.UnixMilli}}`  | `time.Now().UnixMilli()`                        |
| `{{.Unix}}`       | `time.Now().Unix()`                             |
| `{{.ISO}}`        | `time.Now().UTC().Format(time.RFC3339)`         |
| `{{.Date}}`       | `time.Now().UTC().Format("2006-01-02")`         |
| `{{.UUID}}`       | a fresh UUID v4 (different on every request)    |

For example: `https://example.com/api?cb={{.UnixMilli}}` will
include the current millisecond timestamp on every request.

## Web UI

The daemon exposes a web UI and JSON API on the address
configured in `[web] listen` (default `127.0.0.1:8080`). The UI
requires the token from `[web] token` in the config:

- On first visit, paste the token into the sign-in form. The
  browser stores it in `localStorage` so subsequent visits don't
  re-prompt.
- To rotate the token, open the **Settings** dialog and enter
  a new value (or call `PUT /api/settings` directly with
  `{"web": {"token": "..."}}`).

The UI shows, per source:

- ID, name, type, URL, interval, enabled/disabled
- Last run / next run timestamps
- Current item count and a `sha256:` hash fingerprint of the
  current set
- The last diff (added / removed counts)
- **Run now** to trigger an immediate check
- **Edit** / **Delete** / **Add** buttons

The Settings dialog exposes the ntfy server/topic, the global
interval, the web listen address, and the web token. A **Rotate
token** button generates a new random token, writes it to
`config.toml`, updates the browser's `localStorage`, and shows
the new value once so the user can copy it elsewhere.

All UI changes are persisted to `config.toml` and the daemon
hot-reloads — no restart required.

## First-run experience

If `config.toml` doesn't exist when the daemon starts, the
daemon generates a default with a random token, prints the
token to the log, and starts normally. The user pastes the
token into the sign-in form and is signed in for subsequent
visits (the token is stored in `localStorage`).

## Installation

```sh
# Build
make build

# First run: auto-generates config.toml with a random token,
# prints the token to stdout. Paste it into the web UI.
make run

# Or, on Linux, install as a systemd service:
make service
systemctl --user status checkdiff.service
```

`make service` installs `contrib/checkdiff.service` into
`~/.config/systemd/user/`, patches the binary and config paths,
and enables the daemon. Logs go to
`journalctl --user -u checkdiff.service`.

To uninstall: `make uninstall`.

## Configuration

`~/.config/checkdiff/config.toml`:

```toml
[ntfy]
server = "https://ntfy.sh"
topic  = "my-topic"

[web]
listen = "127.0.0.1:8080"
token  = "..."           # empty = web UI disabled; non-empty = required for all access

[check]
check_interval = "1h"    # global default; sources can override

[[sources]]
id             = "openrouter-models"
name           = "OpenRouter Models"
type           = "json"
url            = "https://openrouter.ai/api/v1/models"
enabled        = true
check_interval = "30m"   # overrides [check].check_interval
topic          = "my-own-channel"  # optional; blank = global [ntfy] topic
items_path     = "data"
id_field       = "id"
title_field    = "name"

# Amazon package tracking. The `cookies` field is the
# Cookie header value from your browser's dev tools
# (Network tab → request → Request Headers). Without it
# the tracking page redirects to sign-in. The fetcher
# defaults the Referer to the order history page on the
# same host; override `referer` if your region uses a
# different landing page.
[[sources]]
id             = "amazon-pkg-1"
name           = "Amazon package 1"
type           = "amazon"
url            = "https://www.amazon.ca/gp/your-account/ship-track?itemId=...&orderId=...&shipmentId=..."
enabled        = true
check_interval = "30m"
# cookies = "session-id=...; ubid-acbca=...; at-acbca=..."
# referer = "https://www.amazon.ca/gp/css/order-history/"

# LLM summarizer for page_llm sources. The key itself is never
# stored here — only where to find it. An opencode Go
# subscription (https://opencode.ai/auth) exposes both protocols:
# provider "openai" hits {server}/chat/completions, "anthropic"
# hits {server}/messages. With no model set, page_llm still
# works but sends a static from/to diff.
# [llm]
# provider     = "openai"
# server       = "https://opencode.ai/zen/go/v1"
# model        = "glm-5.3-flash"
# api_key_env  = "OPENCODE_API_KEY"
# api_key_file = "/path/to/key"   # raw key or {"api_key": "..."} JSON
# api_key_path = "opencode-go.key" # dot path into a JSON store (e.g. ~/.pi/agent/auth.json)
# max_tokens   = 300

# Campaign page watched by main content + LLM summary. The
# selector extracts the body text; any change triggers the LLM
# to summarize old-vs-new into a 2-3 sentence notification.
# [[sources]]
# id             = "openprinter"
# name           = "Openprinter campaign"
# type           = "page_llm"
# url            = "https://www.crowdsupply.com/open-tools/openprinter"
# enabled        = true
# check_interval = "30m"
# selector       = "main"   # default; whole main content
# prompt         = "Focus on launch status, pricing, dates, and new updates; ignore press links."
# llm_model      = ""       # blank = global [llm].model
```

To disable a source temporarily, set `enabled = false`. To
remove it entirely, delete the `[[sources]]` block. To add a
new source, use the web UI's **Add** button (which writes the
TOML for you and the daemon hot-reloads).

## Notification behavior

- First run for a source → record baseline, no notification.
- Subsequent run, no diff → no notification.
- `github_file`: content changed → one notification with the
  new file path, size, and a content excerpt.
- `html` / `json`: items added and/or removed → one
  notification with separate **Added:** and **Removed:**
  sections listing the affected entries by their stable
  identifier. High-priority when there are 6+ changes.
- `page_llm`: main content changed → one notification whose
  body is the LLM's 2-3 sentence summary (or a truncated
  from/to diff when no model is configured / the LLM call
  fails).
- Source fetch fails → one **high-priority warning**
  notification, no state change.

## Layout

```
checkdiff/
├── main.go                       # daemon entry point
├── config.go                     # TOML config types + loader
├── source.go                     # Source interface, github_file, html, json
├── notify.go                     # ntfy.sh publisher
├── state.go                      # persistent state (item IDs per source)
├── template.go                   # URL {{...}} substitution
├── schedule.go                   # interval parsing (Go duration + cron)
├── daemon.go                     # per-source goroutine supervisor
├── web.go                        # HTTP server + JSON API
├── firstrun.go                   # auto-generate config on first run
├── configwatch.go                # fsnotify hot-reload
├── web/                          # embedded web UI assets
│   ├── index.html
│   ├── app.js
│   └── style.css
├── contrib/checkdiff.service     # systemd user unit
├── Makefile
└── README.md
```

## Notes / design choices

- **Single binary, no external scheduler.** Per-source goroutines
  own their own schedules. No `launchd` plist, no `systemd`
  timer, no `cron` job. Each source can have its own interval
  (Go duration or cron expression) and they all run in parallel
  inside the daemon.
- **Token auth, always.** The web UI and JSON API require the
  `[web] token` on every request. There is no localhost bypass.
  If the token is empty, the HTTP server does not start (the
  daemon still runs sources; only the HTTP surface is disabled).
- **Hot-reload.** The config file is watched via `fsnotify`.
  Edits are debounced 200ms and trigger a reconcile: new
  sources start, removed sources are cancelled, changed
  sources restart. The in-memory state is preserved so editing
  a URL doesn't fire a flood of "new" notifications. Web UI
  changes (add / edit / delete / settings) write the TOML
  directly; the watcher picks up the change and reloads
  automatically.
- **Token storage in the browser.** The web UI stores the
  token in `localStorage` after the first successful sign-in.
  A 401 response clears it and re-prompts. This means the
  user signs in once per browser; clearing the browser's
  storage signs them out.
- **State migration on upgrade.** Earlier versions stored
  per-source state as `map[string]map[string]bool` (item IDs).
  The current code stores it as `map[string]*SourceState`
  (which includes the same item IDs plus per-source runtime
  fields like `last_run`, `next_run`, `items_hash`, etc.). The
  on-disk format is versioned (v1 → v2) and migrated
  transparently on load — no data loss.
- **No retries, no backoff.** A failure on one tick just means
  we miss changes until the next tick. Errors are sent to
  ntfy as high-priority warnings so they're visible.
- **Topic URL is the password.** Don't share it.
