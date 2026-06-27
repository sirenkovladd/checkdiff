#!/bin/bash
set -e

# SSH target is a Host alias from ~/.ssh/config (which supplies the
# HostName, User, IdentityFile, etc.). Configure it once, reuse
# everywhere — see §2.2 of docs/server-setup.md.
# Override with CHECKDIFF_SERVER=foo ./deploy.sh if you have multiple.
SERVER="${CHECKDIFF_SERVER:-luiscup}"
SSH="ssh $SERVER"
SCP="scp"

# Where things live on the server. The systemd user service runs as
# the SSH user, so ~-prefixed paths expand on the remote to their
# home (luiscup's deploy.sh uses the same trick).
REMOTE_BIN='~/bin/checkdiff'
REMOTE_CONFIG='~/.config/checkdiff/config.toml'
REMOTE_TIMER='~/.config/systemd/user/checkdiff.timer'

# Local paths. The config default mirrors checkdiff's own default
# (see -config flag in main.go), so `./deploy.sh` with no args does
# the obvious thing. Pass a path to override:
#   ./deploy.sh ./staging.toml
LOCAL_BIN="bin/checkdiff"
LOCAL_CONFIG="${1:-$HOME/.config/checkdiff/config.toml}"

if [ ! -f "$LOCAL_CONFIG" ]; then
  echo "error: config not found at $LOCAL_CONFIG" >&2
  echo "  pass it as the first arg, or generate one with:" >&2
  echo "    ./bin/checkdiff -config $LOCAL_CONFIG -init" >&2
  exit 1
fi

# Generate the systemd timer unit from the existing local binary
# (a macOS build is fine here — -print-timer is OS-agnostic). Done
# BEFORE the cross-compile below, because that overwrites $LOCAL_BIN
# with a Linux ELF that macOS can't execute. The timer content
# depends on [check].check_interval in the config, so any interval
# change re-renders the file and a SHA-compare below decides whether
# it actually needs to be uploaded.
TMP_TIMER=$(mktemp)
trap 'rm -f "$TMP_TIMER"' EXIT
if [ ! -x "$LOCAL_BIN" ]; then
  echo "error: $LOCAL_BIN not found or not executable; run 'make build' first" >&2
  exit 1
fi
if ! "$LOCAL_BIN" -config "$LOCAL_CONFIG" -print-timer > "$TMP_TIMER" 2> "$TMP_TIMER.err"; then
  cat "$TMP_TIMER.err" >&2
  echo "error: failed to render timer from $LOCAL_BIN" >&2
  echo "  (does the local config have a valid [check].check_interval?)" >&2
  exit 1
fi

echo "Building binary for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -trimpath -o "$LOCAL_BIN" .

# Compute local SHA-256 (always 64 hex chars).
LOCAL_BIN_SHA=$(shasum -a 256 "$LOCAL_BIN" | awk '{print $1}')
LOCAL_CONFIG_SHA=$(shasum -a 256 "$LOCAL_CONFIG" | awk '{print $1}')
LOCAL_TIMER_SHA=$(shasum -a 256 "$TMP_TIMER" | awk '{print $1}')

# get_remote_sha <remote-path> echoes the remote SHA-256, or an empty
# string if the file doesn't exist. A genuine SSH failure still aborts
# the script (set -e) — only "no such file" is treated as a valid
# empty result, which means "first deploy, treat as different".
#
# Note: $1 is intentionally passed UNQUOTED to the remote shell so
# that '~' gets tilde-expanded to the remote user's home. (Single
# quotes would prevent expansion and '[ -f "~/bin/checkdiff" ]'
# would always fail because no file is literally named '~'.) This
# is safe for the paths this script uses — the deploy target paths
# (~/bin/checkdiff, ~/.config/checkdiff/config.toml) contain no
# spaces. If you ever point this at a path with spaces, quote it
# here and pre-expand ~ on the remote side instead.
get_remote_sha() {
  $SSH "if [ -f $1 ]; then sha256sum $1 | awk '{print \$1}'; else echo ''; fi" 2>/dev/null
}

REMOTE_BIN_SHA=$(get_remote_sha "$REMOTE_BIN")
REMOTE_CONFIG_SHA=$(get_remote_sha "$REMOTE_CONFIG")
REMOTE_TIMER_SHA=$(get_remote_sha "$REMOTE_TIMER")

# Decide which files to deploy. Anything whose local hash differs from
# the remote hash (including a missing remote file) is on the list.
bin_action="unchanged"
config_action="unchanged"
timer_action="unchanged"
[ "$LOCAL_BIN_SHA"     != "$REMOTE_BIN_SHA"     ] && bin_action="will deploy"
[ "$LOCAL_CONFIG_SHA"  != "$REMOTE_CONFIG_SHA"  ] && config_action="will deploy"
[ "$LOCAL_TIMER_SHA"   != "$REMOTE_TIMER_SHA"   ] && timer_action="will deploy"

short_sha() { echo "${1:0:8}"; }

echo
echo "Comparing:"
if [ -n "$REMOTE_BIN_SHA" ]; then
  printf "  %-7s  local %s  remote %s  → %s\n" \
    "bin"    "$(short_sha "$LOCAL_BIN_SHA")"    "$(short_sha "$REMOTE_BIN_SHA")"    "$bin_action"
else
  printf "  %-7s  local %s  remote (absent)     → %s\n" \
    "bin"    "$(short_sha "$LOCAL_BIN_SHA")"    "$bin_action"
fi
if [ -n "$REMOTE_CONFIG_SHA" ]; then
  printf "  %-7s  local %s  remote %s  → %s\n" \
    "config" "$(short_sha "$LOCAL_CONFIG_SHA")" "$(short_sha "$REMOTE_CONFIG_SHA")" "$config_action"
else
  printf "  %-7s  local %s  remote (absent)     → %s\n" \
    "config" "$(short_sha "$LOCAL_CONFIG_SHA")" "$config_action"
fi
if [ -n "$REMOTE_TIMER_SHA" ]; then
  printf "  %-7s  local %s  remote %s  → %s\n" \
    "timer"  "$(short_sha "$LOCAL_TIMER_SHA")"  "$(short_sha "$REMOTE_TIMER_SHA")"  "$timer_action"
else
  printf "  %-7s  local %s  remote (absent)     → %s\n" \
    "timer"  "$(short_sha "$LOCAL_TIMER_SHA")"  "$timer_action"
fi
echo

# Short-circuit: nothing to do, so don't restart the service either.
if [ "$bin_action" = "unchanged" ] && [ "$config_action" = "unchanged" ] && [ "$timer_action" = "unchanged" ]; then
  echo "Nothing to deploy: bin, config, and timer are identical on the server."
  exit 0
fi

# Upload only the files that changed.
[ "$bin_action" = "will deploy" ] && {
  echo "Uploading binary..."
  $SCP "$LOCAL_BIN" "$SERVER:/tmp/checkdiff"
}
[ "$config_action" = "will deploy" ] && {
  echo "Uploading config..."
  $SCP "$LOCAL_CONFIG" "$SERVER:/tmp/checkdiff-config.toml"
}
[ "$timer_action" = "will deploy" ] && {
  echo "Uploading timer..."
  $SCP "$TMP_TIMER" "$SERVER:/tmp/checkdiff.timer"
}

# Compose the remote install command. The prelude is always run; the
# per-file install steps are only included for changed files. The
# postlude is always run (we restarted the service, so the new
# binary/config is picked up). When the timer file changed, we also
# create ~/.config/systemd/user/ on the remote before installing.
PRELUDE="systemctl --user stop checkdiff.timer 2>/dev/null || true; \
  systemctl --user stop checkdiff.service 2>/dev/null || true; \
  mkdir -p ~/bin ~/.config/checkdiff ~/.local/share/checkdiff ~/.config/systemd/user"

INSTALL=""
[ "$bin_action"    = "will deploy" ] && INSTALL="${INSTALL:+${INSTALL} && }install -m 0755 /tmp/checkdiff $REMOTE_BIN"
[ "$config_action" = "will deploy" ] && INSTALL="${INSTALL:+${INSTALL} && }install -m 0600 /tmp/checkdiff-config.toml $REMOTE_CONFIG"
[ "$timer_action"  = "will deploy" ] && INSTALL="${INSTALL:+${INSTALL} && }install -m 0644 /tmp/checkdiff.timer $REMOTE_TIMER"

POSTLUDE="systemctl --user daemon-reload; \
  systemctl --user enable --now checkdiff.timer; \
  systemctl --user start checkdiff.service"

echo "Deploying..."
# Join with ';' when one side is empty (e.g. only one file changed)
# so we don't pass an empty `&& ` to the shell.
if [ -z "$INSTALL" ]; then
  $SSH "$PRELUDE; $POSTLUDE"
else
  $SSH "$PRELUDE; $INSTALL; $POSTLUDE"
fi

echo "Done! Status:"
$SSH "systemctl --user status checkdiff.timer --no-pager | head -10 && \
  echo '--- next scheduled run ---' && \
  systemctl --user list-timers --no-pager | grep checkdiff || true && \
  echo '--- last 5 log lines ---' && \
  journalctl --user -u checkdiff.service -n 5 --no-pager || true"
