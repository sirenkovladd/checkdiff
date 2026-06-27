# checkdiff

A small Go daemon that polls a handful of URLs/files and pushes a
notification to [ntfy.sh](https://ntfy.sh) whenever something has
changed. Runs on macOS via `launchd`, every hour.

## What it watches

Each entry in `~/.config/checkdiff/config.toml` is a "source":

| type          | what it tracks                                                                 |
| ------------- | ------------------------------------------------------------------------------ |
| `github_file` | A file in a GitHub repo, fetched with `gh api`. The git blob SHA is the diff key — a notification fires when the file content changes. |
| `html`        | A web page; elements matching a CSS-ish selector (`h1`..`h4`, `title`, or `tag.class` like `li.attachedfile`) are diffed. Items are tracked by their text content, so **additions and removals of individual entries are detected** (not just "the page changed"). |
| `json`        | A JSON API. A configurable path (`items_path`, default `data`) locates the array of items; `id_field` (default `id`) and `title_field` (default `name`) pick the stable identifier and display name. Optional `link_field` attaches a per-item URL (e.g. a package tracking link) — the notification's Click header opens that URL instead of the source's URL, and the item is rendered as a markdown link in the body. Optional source-level `link` is a static URL for sources where every entry points at the same page (e.g. a single-package tracking source); it wins over the source's `url` when no per-item `Link` is available. Items are tracked by ID, so additions and removals are detected. Useful for sites that are client-rendered (React/Next.js) where the HTML is empty — the public API is the canonical source. |

The first run establishes a baseline (no notifications are sent).
Subsequent runs notify on **additions and removals** for `html` /
`json` sources, and on **content changes** for `github_file`
sources, and remember it all forever after in
`~/.local/share/checkdiff/state.json`.

The config is TOML, so you can **disable a source by commenting out
its `[[sources]]` block** — the lines are dropped from the parse, the
entry never reaches the run loop, and the baseline in `state.json` is
preserved so uncommenting later doesn't fire a flood of "new"
notifications.

## One-time setup

```sh
cd /path/to/checkdiff
go mod download                    # fetch golang.org/x/net

# 1. Generate a default config and edit the ntfy topic.
make build
./bin/checkdiff -config ~/.config/checkdiff/config.toml -init
$EDITOR ~/.config/checkdiff/config.toml    # set ntfy.topic

# 2. Verify the ntfy wiring.
make test-notify                    # sends a single 'checkdiff test' message

# 3. Install the launchd job.
make install
launchctl list | grep checkdiff     # should show com.checkdiff running
```

`make install` drops `com.checkdiff.plist` into
`~/Library/LaunchAgents/`. The job:

- runs `checkdiff` once at load (this is the **baseline** run — silent)
- then every `3600` seconds (1 hour)
- writes `~/.checkdiff/checkdiff.log` (stdout and stderr merged)
- is launched with `-v`, so the log shows what's happening each tick
- inherits a PATH that includes Homebrew + `~/.local/bin` so `gh` resolves

To change frequency, edit `StartInterval` in
`com.checkdiff.plist.template` and `make plist install` again.

## Day-to-day

```sh
make run              # run once, normal mode, verbose
make test             # run once, dry-run, no ntfy message
make test-notify      # send a single 'checkdiff test' notification
make uninstall        # unload + remove the plist (binary + state stay)
```

To re-baseline (silence the existing diff and start tracking from
scratch — useful if you've just added a new source):

```sh
rm ~/.local/share/checkdiff/state.json
```

To pause a source without losing its entry, comment out its entire
`[[sources]]` block in `config.toml` (the `[[sources]]` header line
plus every `key = value` line beneath it, up to the next blank line
or header). The TOML parser drops the lines; the run loop never sees
the entry. Uncomment to resume.

The next run will be silent and re-establish the baseline.

## Notification behavior

checkdiff is **silent on its first run for each source**. This is so
that you don't get a flood of "154 new items" when you first point it
at the changelog. After that, **a notification is sent when the
source's set of items changes between runs**.

- First run for a source → record baseline, no notification.
- Subsequent run, no diff → no notification.
- `github_file`: content changed → one notification with the new
  file path, size, and a content excerpt.
- `html` / `json`: items added and/or removed → one notification with
  separate **Added:** and **Removed:** sections listing the affected
  entries by their stable identifier (the entry text for `html`, the
  model id for `json`). High-priority when there are 6+ changes.
- Source fetch fails → one **high-priority warning** notification, no
  state change.

## Layout

```
checkdiff/
├── main.go                       # entry: flag parsing, orchestration
├── config.go                     # config types + loader
├── source.go                     # Source interface, github_file, html
├── notify.go                     # ntfy.sh publisher
├── state.go                      # persistent state (item IDs per source)
├── go.mod / go.sum
├── com.checkdiff.plist.template  # sed-substituted by `make plist`
├── Makefile                      # build / install / uninstall
└── README.md
```

## Notes / design choices

- **`gh` CLI for GitHub.** Uses
  `gh api repos/{o}/{r}/contents/{path}?ref={ref}` and decodes the
  base64 payload. Auth comes from the existing `gh auth login`. If `gh`
  isn't on PATH, the source fails and we send a single warning
  notification.
- **`gh` discovery.** In addition to `exec.LookPath("gh")`, the binary
  tries common tool-manager locations: `~/.local/share/mise/installs/gh/*/*/bin/gh`,
  `~/.asdf/shims/gh`, `~/.local/share/rtx/installs/gh/*/bin/gh`,
  `/opt/homebrew/bin/gh`, `/usr/local/bin/gh`. Override with `--gh`.
- **HTML scraping is intentionally simple.** We support a single
  selector, which is either a bare tag name (`h1`/`h2`/`h3`/`h4`/
  `title`, or any other element such as `li`) or a `tag.class` form
  for elements with a specific class (e.g. `li.attachedfile`). Multiple
  classes are AND-ed: `li.foo.bar` matches an `<li>` that has both
  `foo` and `bar` in its `class` attribute. We track each matching
  element by its text content, so adding or removing an entry is
  detected. If the page adds non-deterministic whitespace or a rotating
  banner, the diff will false-positive. If that happens, switch the
  `selector` to something more stable or scope it to a class that
  ignores the noise.
- **JSON sources are the right choice for client-rendered sites.**
  The scraper can't execute JavaScript, so pages built with React,
  Next.js, Vue, Svelte, etc. will come back with an empty body. For
  those, use the site's public API (most expose one) and configure a
  `json` source. The defaults (`items_path = "data"`, `id_field =
  "id"`, `title_field = "name"`) match the shape used by most
  JSON:API-style services, including OpenRouter's
  `/api/v1/models`. Override the fields for APIs that use a different
  layout. For per-item URLs (e.g. a tracking page per package), set
  `link_field` to the JSON field holding the URL — the
  notification's Click header then opens that URL, and the item is
  shown as a clickable markdown link in the body.
- **State migration on upgrade.** Earlier versions stored html item
  IDs as `txt:<16 hex>` (sha256 of element text). The current code
  stores html items by their text content directly, so additions and
  removals are detectable. On load, any source whose state still uses
  the old hash format is dropped silently — the next run treats it as
  a first-time baseline (no "new" notification flood). github_file and
  json sources are unaffected.
- **First run is silent** (baseline only). You'll only get notified
  about *new* items from the second run onwards. To force a
  notification for testing, use `make test-notify` (`-test-notify`).
- **No retries, no backoff.** A failure on one tick just means we
  miss changes until the next tick. Errors are sent to ntfy as
  high-priority warnings so they're visible.
- **Topic URL is the password.** Don't share it.
