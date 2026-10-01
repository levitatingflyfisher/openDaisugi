#!/usr/bin/env bash
# fake-foreman.sh: a stand-in for a claude foreman, for the web check. It
# runs as the claude harness in the foreman's pane. It writes an invented
# Claude transcript under the scratch Claude config directory, names it
# from inside its own pane the way a gate hook does (rpc.mjs), and draws an
# empty prompt box, which coppice reads as idle. Each line typed into it
# (the server's page line first, then each sentence from floor.talk) is
# one turn: it reports working, adds the line to the transcript as the
# owner's, waits a moment, adds a reply that names the agent "e2e shell",
# and reports idle. No model and no network are involved, and the
# transcript holds only invented words.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${COPPICE_PANE:?fake-foreman.sh runs inside a coppice pane}" "${COPPICE_SOCK:?}"
conf="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
dir="$conf/projects/e2e-foreman"
mkdir -p "$dir"
transcript="$dir/${COPPICE_PANE//:/-}.jsonl"
cwd="$(pwd)"
# REPLY_WAIT is how long a turn takes, so the page shows a sentence as
# sent before the reply marks it read.
wait_s="${FAKE_FOREMAN_WAIT:-1.5}"
n=0

now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }

# json STRING: STRING as a JSON string.
json() { node -e 'process.stdout.write(JSON.stringify(process.argv[1]))' "$1"; }

say() { # say ROLE TEXT: one transcript entry.
  n=$((n + 1))
  local role="$1" text="$2"
  if [ "$role" = user ]; then
    printf '{"type":"user","uuid":"e2e-u%d","timestamp":"%s","cwd":%s,"message":{"role":"user","content":%s}}\n' \
      "$n" "$(now)" "$(json "$cwd")" "$(json "$text")" >> "$transcript"
  else
    printf '{"type":"assistant","uuid":"e2e-a%d","timestamp":"%s","message":{"id":"msg_e2e_%d","model":"claude-fake","role":"assistant","content":[{"type":"text","text":%s}]}}\n' \
      "$n" "$(now)" "$n" "$(json "$text")" >> "$transcript"
  fi
}

report() { # report STATE DETAIL: a gate hook's report from inside the pane.
  node "$here/rpc.mjs" "$COPPICE_SOCK" report "$COPPICE_PANE" \
    "{\"transcript_path\":$(json "$transcript")}" "$1" "$2" > /dev/null 2>&1 || true
}

box() {
  printf '\033[2J\033[H'
  printf '%s\n' '────────────────────' '❯ ' '────────────────────'
}

: > "$transcript"
say user "You are the fake foreman of this e2e floor."
say assistant "Ready. I can see e2e shell on the floor."
box
report idle Stop

while IFS= read -r line; do
  line="${line%$'\r'}"
  [ -n "$line" ] || continue
  report working UserPromptSubmit
  say user "$line"
  sleep "$wait_s"
  say assistant "Got it. I asked e2e shell to look."
  box
  report idle Stop
done
