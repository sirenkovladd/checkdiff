# AGENTS.md

Guidance for AI coding agents working on `checkdiff`. Read this
file end-to-end before making changes — it covers the project
layout, the conventions the codebase follows, and how to
operate the deployed instance (the public daemon the user
monitors) over its JSON API.

## What checkdiff is

A long-running Go daemon that polls URLs/files on a schedule
and publishes a notification to [ntfy.sh](https://ntfy.sh)
when something has changed. One binary, per-source
goroutines, hot-reloadable config, a small web UI for
managing sources.

Full user-facing docs: [`README.md`](README.md).

## Layout

```
main.go                  daemon entry point (flag.Parse, signal handling, wiring)
config/                  TOML config loader, first-run generator, fsnotify watcher
source/                  Fetcher interface + one file per type (github_file, html,
                         json, json_value, amazon)
state/                   on-disk + in-memory diff baseline per source
check/                   fetch → diff → notify decision
notify/                  ntfy.sh publisher
daemon/                  per-source goroutine supervisor
webapi/                  HTTP server: JSON API + embedded web UI
schedule/                Go duration + 5-field cron parser
template/                URL {{...}} placeholder substitution
webui/                   embedded static UI assets (index.html, app.js, style.css)
contrib/                 systemd user unit template
deploy.sh                build + scp + restart the service on the remote host
```

## Adding a new source type

Each source type is one file in `source/` implementing the
`Fetcher` interface (`Type`, `Fetch`, `Validate`, `Format`).
Adding a new type is then:

1. Create `source/<name>.go` with the fetcher.
2. Add a one-line entry to the `registry` map in
   `source/source.go`.
3. Add type-specific fields to the `Source` struct in
   `source/source.go` (with `omitempty` tags so existing
   configs keep working).
4. Add the type to the `renderTypeFields` map in
   `webui/web/app.js` and the `<select>` in
   `webui/web/index.html` so the web UI knows about it.
5. Add a `case` for the new type in `handleSourceContent`
   in `webapi/webapi.go` so the "View" dialog renders
   something sensible.
6. Add a row to the source-type table in `README.md` and
   a worked TOML example.
7. Write tests in `source/<name>_test.go`. The existing
   `source/format_test.go` and `source/source_test.go`
   show the conventions (httptest server, t.Run subtests,
   table-driven cases).

The `amazon` fetcher (`source/amazon.go`) is the most
recent precedent — read it first for the canonical example
of how a single-value HTML source with cookies/referer
plumbing looks end-to-end.

## Building, testing, linting

```sh
make build           # compile ./bin/checkdiff
go test ./...        # 217 tests, ~1s
go vet ./...         # must be clean before commit
gofmt -l .           # no output = nothing to reformat
```

CI: there's no CI file in the repo. The user runs the
three commands above locally before merging.

## Commit / PR conventions

- Imperative-mood subject line ("Add X", not "Added X").
- Body explains the why, not the what — the diff shows the
  what.
- New source type PRs must include a `README.md` update in
  the same PR (the table + worked example).

## Deploying

```sh
./deploy.sh
```

The script builds for `linux/amd64`, scp's the binary to
the `luiscup` SSH alias (see `~/.ssh/config`), and
restarts the systemd user service. The service file is
patched on the remote to use the user's actual paths. The
config on the server is **not** touched by `deploy.sh` —
manage it via the web API (below) or by hand.

## Operating the deployed instance

The daemon runs on the user's server and is reachable over
the public web:

- **URL**: `https://checkdiff.sirenko.ca/`
- **Reverse proxy**: Traefik (see
  `/etc/traefik/dynamic/checkdiff.yml` on the server);
  forwards to `127.0.0.1:8765` (the daemon's `[web]
  listen`).
- **Access restriction**: Tailscale CGNAT range +
  `216.180.67.170/32` (the user's home/office). You can't
  reach the URL from arbitrary networks.

### Authentication

Every request must carry the bearer token from the
server-side config:

```sh
TOKEN=$(ssh luiscup 'grep -E "^\s*token" ~/.config/checkdiff/config.toml | head -1 | sed -E "s/.*token\s*=\s*\"([^\"]+)\".*/\1/"')
```

The token is rotated via `POST /api/rotate-token` (returns
the new token in the response, then updates the config and
the in-memory state on the server). After a rotation,
update the browser's `localStorage` key `checkdiff.token`
to match.

### Listing sources

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/sources | jq .
```

Each source is a JSON object with at least `{id, name, type,
url, enabled, check_interval}`. The list is the source of
truth — the on-disk TOML on the server is just its
serialised form.

### Inspecting a single source's current content (e.g. the latest status)

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/sources/<id>/content | jq .
```

The response shape depends on the source type:
- `json_value` / `amazon`: `{"type": "...", "value": "current status string"}`
- `html` / `json`: `{"type": "...", "items": [{"id": "...", "title": "..."}]}`
- `github_file`: same as `html`/`json` plus a `commit` object (latest commit that touched the file)

To see the source's *configuration* (not its current state), list all sources with `GET /api/sources` and filter — there is no per-source GET endpoint, by design.

### Inspecting runtime state (last run, error, items)

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/state | jq .
```

The response is a flat object keyed by source id, e.g.
`{"<id>": {"items_seen": {...}, "items_count": N, "last_run": "...", "last_error": "", ...}}`.
The `last_error` field is the most useful thing to look at
first when a source is misbehaving.

### One-shot snapshot of everything the web UI shows

The web UI polls a single endpoint every 5s to refresh the
main page. The same endpoint is also handy from `curl` when
you want all three pieces (sources + state + config) in one
round-trip without the 3-call fan-out:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/overview | jq .
```

The response shape is `{"sources": [...], "state": {...},
"config": {...}}`. The `config.web.token` field is masked to
`"****"` (same as `/api/config`). The three individual
endpoints above are unchanged for external consumers; this
is purely a convenience.

### Adding a source

`POST /api/sources` with the source JSON in the body. The
daemon validates the source first (so a malformed source
returns a 400 and the config is not touched), then writes
the new TOML to disk; the fsnotify watcher picks up the
change and starts a new goroutine.

```sh
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "my-new-source",
    "name": "My new source",
    "type": "json",
    "url": "https://example.com/api",
    "check_interval": "30m",
    "enabled": true
  }' \
  https://checkdiff.sirenko.ca/api/sources
```

The `id` is the lookup key — choose a stable, URL-safe
slug. The daemon enforces uniqueness and returns 400 on
collision.

### Updating a source

`PUT /api/sources/<id>` with the **full** new source JSON
in the body. PUT is a replace, not a partial update — any
field you omit will be reset to its default (or zero
value). Always re-send every field you want to preserve.

```sh
curl -sS -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "my-new-source",
    "name": "My renamed source",
    "type": "json",
    "url": "https://example.com/api",
    "check_interval": "1h",
    "enabled": true
  }' \
  https://checkdiff.sirenko.ca/api/sources/my-new-source
```

The `id` in the URL is authoritative. The body's `id`
field is ignored if it differs (this prevents a
URL/body desync where PUT'ing a source changes its ID).

**Common gotcha**: when updating an `amazon` source, the
cookies can be very long. Use Python (or any tool that
handles shell escaping cleanly) to build the JSON body:

```python
import json, urllib.request

cookies = "session-id=...; ubid-acbca=...; at-acbca=..."
req = urllib.request.Request(
    "https://checkdiff.sirenko.ca/api/sources/<id>",
    data=json.dumps({
        "id": "<id>", "name": "...", "type": "amazon",
        "url": "https://www.amazon.ca/...",
        "check_interval": "15m", "enabled": True,
        "cookies": cookies,
    }).encode("utf-8"),
    method="PUT",
    headers={"Authorization": f"Bearer {TOKEN}",
             "Content-Type": "application/json"},
)
urllib.request.urlopen(req).read()
```

### Removing a source

`DELETE /api/sources/<id>`. The daemon removes the source
from the in-memory config, writes the new TOML to disk,
cancels the goroutine, and prunes its state entry.

```sh
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/sources/my-new-source
```

Returns 204 No Content on success, 404 if the id doesn't
exist. **The state file's `items_seen` for the removed
source is also pruned**, so re-adding a source with the
same id starts with a fresh baseline (no flood of "new"
notifications for everything it sees on the first run).

### Triggering an immediate check (skip the wait)

Useful right after adding/updating a source so the user
sees the result in the next 1–2 seconds instead of waiting
for the next scheduled tick.

```sh
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  https://checkdiff.sirenko.ca/api/sources/<id>/run
```

Returns 202 Accepted. Non-blocking — if a check is
already in flight, the request is silently coalesced.

### Updating the ntfy topic / default interval / web settings

`PUT /api/settings` with a partial body. Each block is
optional; only supplied fields override the current
values. The token field is special: omit it to leave it
alone, set to `""` to disable the web UI entirely.

```sh
curl -sS -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "ntfy":  {"server": "https://ntfy.sh", "topic": "new-topic"},
    "check": {"interval": "1h"},
    "web":   {"listen": "127.0.0.1:8765"}
  }' \
  https://checkdiff.sirenko.ca/api/settings
```

The web `token` and `listen` changes take effect
immediately on the server (no restart). Other changes
take effect on the next config reload.

### The on-disk config

The web API and the TOML on disk are kept in sync. The
flow is: API call → in-memory config update →
`config.WriteFile` → fsnotify picks up the change →
`config.Load` → `daemon.Reload` reconciles runners.

Direct edits to `~/.config/checkdiff/config.toml` on the
server also work — the fsnotify watcher picks them up
just the same. The only difference: direct edits bypass
the per-source validation the API does, so a bad edit
makes the whole config fail to load (and the daemon logs
the error but keeps the previous in-memory config).

For an `amazon` source, the simplest direct-edit is:

```sh
ssh luiscup 'sed -i "s|^# cookies\s*=.*|cookies = \"$COOKIE_STR\"|" \
  ~/.config/checkdiff/config.toml'
```

(but escaping the long cookie string through `sed` is
fragile — the Python+API approach above is more reliable
for cookies).

### Source types reference

| type          | key fields                                                                                          |
| ------------- | --------------------------------------------------------------------------------------------------- |
| `github_file` | `owner`, `repo`, `ref` (default "HEAD"), `path` — uses `gh api` under the hood                     |
| `html`        | `selector` (default "h3") — `tag` or `tag.class1.class2`                                              |
| `json`        | `items_path` (default "data"), `id_field` (default "id"), `title_field` (default "name"), `link_field` (optional) |
| `json_value`  | `path` — dot-separated JSON path to a scalar                                                         |
| `amazon`      | `cookies` (required for live session), `referer` (default: order-history page on the same host)     |

All types accept `url`, `check_interval` (Go duration ≥ 15s
or 5-field cron), `enabled`, the static `link` (used as
the notification's Click target when no per-item link is
available), and the optional per-source `topic` (ntfy channel
override; empty = the global `[ntfy] topic`). Full schema is
in `source/source.go`.

### Notification shape

Every diff produces a `Notification` (see
`source/source.go`):

```go
type Notification struct {
    Title    string
    Body     string
    Priority string  // "default" or "high" (5+ items diff)
    Tags     string  // comma-separated, e.g. "loudspeaker", "package"
    Click    string  // URL opened when user taps the notification
}
```

The `notify` package translates this to ntfy headers
(`Title`, `Message`, `Priority`, `Tags`, `Click`). The
`body` is what shows in the ntfy app — keep it readable
as plain text AND as Markdown (so a link in `body` is
tappable in the ntfy UI).
