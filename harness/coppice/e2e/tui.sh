#!/usr/bin/env bash
# tui.sh: the TUI check. run.sh starts the server, makes the panes and puts
# a gate ask on "e2e ask"; this script runs the coppice TUI inside a coppice
# pane. The server's own virtual terminal is then the screen, and
# `coppice pane read` returns its text. Each screen read is saved to
# $E2E_DIR/shots/tui-*.txt. Three runs: 80x36, where no window fits and Space
# opens a peek, 100x36, where the rail is 6 columns wide and one window
# takes keys, and 120x36, where the foreman's chat is the window.
set -euo pipefail
: "${COPPICE_BIN:?run tui.sh through run.sh}" "${sock:?}" "${E2E_DIR:?}"
cop() { "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" "$@"; }
id_of() { sed -n 's/.*"pane": *"\([^"]*\)".*/\1/p' | head -n1; }
shots="$E2E_DIR/shots"
failed=0

# start_tui COLS: a TUI in a new coppice pane of COLS x 36, its id in $tui.
# env -u COPPICE_PANE: inside a pane, coppice would speak as that pane, which
# may not answer; the TUI is the operator here.
tui=""
# hcop runs coppice against the server that hosts the TUI pane. It is the
# floor's own server until the chat run, which needs another one.
hcop() { cop "$@"; }
start_tui() {
  tui=$(hcop pane create --json --label "e2e tui $1" --kind pty --cols "$1" --rows 36 --cwd "$E2E_DIR/proj/alpha" \
    -- env -u COPPICE_PANE "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" | id_of)
  [ -n "$tui" ] || { echo "tui.sh: the TUI pane was not made" >&2; exit 1; }
}
trap '[ -z "$tui" ] || hcop pane close "$tui" >/dev/null 2>&1 || true' EXIT

# 80 columns leaves no room for a window beside the rail, so Space opens a
# peek, which is where the TUI shows an ask's answers and the trust screen's.
start_tui 80

# expect NAME REGEX: read the TUI's screen until a line matches REGEX
# (grep -E), for up to 15 seconds. The last screen read is saved as
# tui-NAME.txt either way.
expect() {
  local name="$1" re="$2" out="$shots/tui-$1.txt" i
  for i in $(seq 1 50); do
    hcop pane read "$tui" > "$out" 2>&1 || true
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
  hcop pane send-keys "$tui" shift+tab >/dev/null
  sleep 0.5
  hcop pane send-keys "$tui" space >/dev/null
  expect "peek-$step" "Deny is the default|Claude asks to trust this folder" || true
  seen="$seen
$(cat "$shots/tui-peek-$step.txt")"
  hcop pane send-keys "$tui" esc >/dev/null
  for _ in $(seq 1 30); do
    hcop pane read "$tui" | grep -q '^peek ' || break
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

# 100 columns: the rail is a strip of 6 columns a row and one window of 80
# columns shows beside it. The floor opens with an agent that needs you in
# that window; the leave key, then the rail keys, move to the shell, and
# Enter puts it in the window, where what is typed reaches the shell.
hcop pane close "$tui" >/dev/null 2>&1 || true
start_tui 100
expect slim-open "typing here" || true
hcop pane send-keys "$tui" ctrl+space >/dev/null
expect slim-rail "Enter go in" || true
for _ in 1 2 3 4 5 6 7 8; do
  hcop pane send-keys "$tui" up >/dev/null
  sleep 0.2
done
hcop pane send-keys "$tui" down >/dev/null
expect slim-cursor "coppice . e2e shell" || true
hcop pane send-keys "$tui" enter >/dev/null
expect slim-shell "e2e shell .*typing here" || true
if grep -Eq '1●e1 [│┃]' "$shots/tui-slim-shell.txt"; then
  echo "ok   tui: the slim rail draws the shell as 6 columns, then the window"
else
  echo "FAIL tui: no slim row for the shell; the screen is in $shots/tui-slim-shell.txt"; failed=1
fi
hcop pane send-text "$tui" "echo slim-typed-$$" --no-enter >/dev/null
hcop pane send-keys "$tui" enter >/dev/null
expect slim-typed "[│┃]slim-typed-$$ *\$" || true

# 120 columns: the chat is a window. The floor's foreman is fake-foreman.sh
# (the claude table). Once it has replied, a floor that opens fills the
# chat window first, with the reply in it and an input line of its own. A
# line typed there goes to the foreman through floor.talk, and its next
# reply shows in the same window. The server treats a process in one of
# its own panes as a pane, and a pane may not read the chat or talk to the
# foreman, so this floor runs in a pane of a second server and speaks to
# the first as the operator. The foreman is closed after, so the web check
# starts its own.
hcop pane close "$tui" >/dev/null 2>&1 || true
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
rpc() { node "$here/rpc.mjs" "$sock" call "$1" "$2"; }
fm=$(rpc floor.foreman '{"harness":"claude"}' | id_of)
[ -n "$fm" ] || { echo "FAIL tui: the fake foreman was not started"; failed=1; }
for _ in $(seq 1 100); do
  rpc floor.chat '{}' | grep -q "I can see e2e shell" && break
  sleep 0.3
done
alt_sock="$E2E_DIR/run/alt.sock"
alt_data="$E2E_DIR/alt-data"
mkdir -p "$alt_data"
"$COPPICE_BIN" --socket "$alt_sock" --data-dir "$alt_data" server start --foreground \
  > "$E2E_DIR/alt-server.log" 2>&1 < /dev/null &
alt_pid=$!
trap 'kill "$alt_pid" 2>/dev/null || true; [ -z "$tui" ] || hcop pane close "$tui" >/dev/null 2>&1 || true' EXIT
hcop() { "$COPPICE_BIN" --socket "$alt_sock" --data-dir "$alt_data" "$@"; }
for _ in $(seq 1 100); do
  hcop server status >/dev/null 2>&1 && break
  sleep 0.1
done
start_tui 120
# The chat takes a window the way an agent does: a free one. The agents that
# need you fill first, so the one window holds one of them and the foreman's
# rail row says it holds a reply. The leave key and the down arrow move to
# that row, and Enter puts the chat in the window.
expect chat-mark "foreman +claude +reply" || true
hcop pane send-keys "$tui" ctrl+space >/dev/null
for _ in $(seq 1 12); do
  hcop pane read "$tui" > "$shots/tui-chat-cursor.txt" 2>&1 || true
  grep -Eq "^ +› .*foreman +claude" "$shots/tui-chat-cursor.txt" && break
  hcop pane send-keys "$tui" down >/dev/null
  sleep 0.3
done
hcop pane send-keys "$tui" enter >/dev/null
expect chat-reply "foreman Ready. I can see e2e shell" || true
if grep -q "Tell the foreman. Type and press Enter." "$shots/tui-chat-reply.txt" && grep -Eq "Screen +×" "$shots/tui-chat-reply.txt"; then
  echo "ok   tui: the chat window has its own line and the Screen word"
else
  echo "FAIL tui: no chat line or Screen word; the screen is in $shots/tui-chat-reply.txt"; failed=1
fi
hcop pane send-text "$tui" "tell e2e shell hi-$$" --no-enter >/dev/null
hcop pane send-keys "$tui" enter >/dev/null
expect chat-sent "you tell e2e shell hi-$$" || true
# The page line already drew one "Got it", so the answer to this line is the second.
answered=0
for _ in $(seq 1 50); do
  hcop pane read "$tui" > "$shots/tui-chat-answer.txt" 2>&1 || true
  if [ "$(grep -c "foreman Got it. I asked e2e shell to look." "$shots/tui-chat-answer.txt")" -ge 2 ]; then answered=1; break; fi
  sleep 0.3
done
if [ "$answered" = 1 ]; then
  echo "ok   tui: the foreman's answer to the typed line shows in the chat window"
else
  echo "FAIL tui: no second foreman answer; the screen is in $shots/tui-chat-answer.txt"; failed=1
fi
# Tab in the chat line shows the foreman's terminal, with the Chat word in
# its header to come back, and what is typed then reaches the terminal as
# keys, not the chat line.
hcop pane send-keys "$tui" tab >/dev/null
expect chat-screen "Chat +×" || true
if grep -q "Tell the foreman. Type and press Enter." "$shots/tui-chat-screen.txt"; then
  echo "FAIL tui: the chat line stayed over the foreman's terminal after Tab"; failed=1
fi
hcop pane send-text "$tui" "tabbed-$$" --no-enter >/dev/null
expect chat-tabbed "tabbed-$$" || true
hcop pane close "$tui" >/dev/null 2>&1 || true
tui=""
hcop server stop >/dev/null 2>&1 || true
[ -z "$fm" ] || cop pane close "$fm" >/dev/null 2>&1 || true

exit "$failed"
