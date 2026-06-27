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

Each source can set its own `check_interval` (a Go duration like
`"30m"` or `"1h"`) or fall back to the global `[check]
check_interval`. Per-source intervals are useful when a fast-
moving changelog should be checked more often than a slow-moving
package tracker.

Source URLs can include `{{...}}` placeholders for cache-busting
or timestamp injection. Supported placeholders:

| Placeholder       | Replaced with                                  |
| ----------------- | ---------------------------------------------- |
| `{{.UnixMilli}}`  | `time.Now().UnixMilli()`                        |
| `{{.Unix}}`       | `time.Now().Unix()`                             |
| `{{.ISO}}`        | `time.Now().UTC().Format(time.RFC3339)`         |
| `{{.Date}}`       | `time.Now().UTC().Format("2006-01-02")`         |

For example: `https://example.com/api?cb={{.UnixMilli}}` will
include the current millisecond timestamp on every request.

## Web UI

The daemon exposes a web UI and JSON API on the address
configured in `[web] listen` (default `127.0.0.1:8080`). The UI
requires the token from `[web] token` in the config:

- On first visit, paste the token into the sign-in form. The
  browser stores it in `localStorage` so subsequent visits don't
  re-prompt.
- To rotate the token, edit `config.toml` and restart the
  daemon. The browser's stored token will stop working and the
  UI will re-prompt for the new one.

The UI shows, per source:

- ID, name, type, URL, interval, enabled/disabled
- Last run / next run timestamps
- Current item count and a hash fingerprint of the current set
- The last diff (added / removed counts)
- **Run now** to trigger an immediate check
- **Edit** / **Delete** / **Add** buttons

## Installation

```sh
# Build
make build

# First run: auto-generates config.toml with a random token,
# prints the token to stdout. Paste it into the web UI.
make run

# Or, on Linux, install as a systemd service:
make install
make service
systemctl --user status checkdiff.service
```

`make service` installs `contrib/checkdiff.service` into
`~/.config/systemd/user/`, patches the binary and config paths,
and enables the daemon. Logs go to `journalctl --user -u
checkdiff.service`.

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
items_path     = "data"
id_field       = "id"
title_field    = "name"
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
  own their own tickers. No `launchd` plist, no `systemd` timer,
  no `cron` job.
- **Token auth, always.** The web UI and JSON API require the
  `[web] token` on every request. There is no localhost bypass.
  If the token is empty, the HTTP server does not start (the
  daemon still runs sources; only the HTTP surface is disabled).
- **Hot-reload.** The config file is watched via `fsnotify`.
  Edits are debounced 200ms and trigger a reconcile: new
  sources start, removed sources are cancelled, changed
  sources restart. The in-memory state is preserved so editing
  a URL doesn't fire a flood of "new" notifications.
- **Token storage in the browser.** The web UI stores the token
  in `localStorage` after the first successful sign-in. A 401
  response clears it and re-prompts. This means the user signs
  in once per browser; clearing the browser's storage signs
  them out.
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
