#!/usr/bin/env bash
# tui.sh: the TUI check. run.sh starts the server, makes the panes and puts
# a gate ask on "e2e ask"; this script runs the coppice TUI inside a coppice
# pane of 100x36. The server's own virtual terminal is then the screen, and
# `coppice pane read` returns its text. Each screen read is saved to
# $E2E_DIR/shots/tui-*.txt.
set -euo pipefail
: "${COPPICE_BIN:?run tui.sh through run.sh}" "${sock:?}" "${E2E_DIR:?}"
cop() { "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" "$@"; }
id_of() { sed -n 's/.*"pane": *"\([^"]*\)".*/\1/p' | head -n1; }
shots="$E2E_DIR/shots"
failed=0

# 100 columns leaves no room for a window beside the rail, so Space opens
# a peek, which is where the TUI shows an ask's answers and the trust
# screen's. env -u COPPICE_PANE: inside a pane, coppice would speak as that
# pane, which may not answer; the TUI is the operator here.
tui=$(cop pane create --json --label "e2e tui" --kind pty --cols 100 --rows 36 --cwd "$E2E_DIR/proj/alpha" \
  -- env -u COPPICE_PANE "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" | id_of)
[ -n "$tui" ] || { echo "tui.sh: the TUI pane was not made" >&2; exit 1; }
trap 'cop pane close "$tui" >/dev/null 2>&1 || true' EXIT

# expect NAME REGEX: read the TUI's screen until a line matches REGEX
# (grep -E), for up to 15 seconds. The last screen read is saved as
# tui-NAME.txt either way.
expect() {
  local name="$1" re="$2" out="$shots/tui-$1.txt" i
  for i in $(seq 1 50); do
    cop pane read "$tui" > "$out" 2>&1 || true
    if grep -Eq "$re" "$out"; then
      echo "ok   tui: $name"
      return 0
    fi
    sleep 0.3
  done
  echo "FAIL tui: $name: nothing matched /$re/; the screen is in $out"
  failed=1
  return 1
}

expect roster "e2e shell" || true
for label in "e2e ask" "e2e trust"; do
  grep -q "$label" "$shots/tui-roster.txt" || { echo "FAIL tui: roster: no $label"; failed=1; }
done

# The rail names both needs once the server has read the trust screen.
expect needs "2 need you" || true

# shift-tab moves the cursor to the next pane that needs you, and Space
# peeks at it. The two that need you are the held ask and the trust
# screen, in either order, so each peek is read for either one's lines.
seen=""
# Each key waits for the screen it causes, since a key that lands while
# the TUI is still drawing the last one's answer can be read differently.
for step in 1 2; do
  cop pane send-keys "$tui" shift+tab >/dev/null
  sleep 0.5
  cop pane send-keys "$tui" space >/dev/null
  expect "peek-$step" "Deny is the default|Claude asks to trust this folder" || true
  seen="$seen
$(cat "$shots/tui-peek-$step.txt")"
  cop pane send-keys "$tui" esc >/dev/null
  for _ in $(seq 1 30); do
    cop pane read "$tui" | grep -q '^peek ' || break
    sleep 0.2
  done
done
if grep -q "y allow once  t allow for this task  n deny" <<<"$seen"; then
  echo "ok   tui: the peek on the held ask shows Deny is the default and its keys"
else
  echo "FAIL tui: no peek showed the held ask's keys"; failed=1
fi
if grep -q "y trust this folder  n not now" <<<"$seen"; then
  echo "ok   tui: the peek on the trust screen shows its keys"
else
  echo "FAIL tui: no peek showed the trust screen's keys"; failed=1
fi

exit "$failed"
