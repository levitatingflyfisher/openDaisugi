#!/usr/bin/env bash
# fake-asker.sh: a stand-in for a claude agent that finished a job and waits
# on the owner. It runs as a claude pane. It writes an invented Claude
# transcript under the scratch Claude config directory, names it from inside
# its own pane the way a warden hook does (rpc.mjs), ends on a question, and
# reports idle. The next line typed into it is the owner's answer: it reports
# working, adds the answer to the transcript, replies without a question and
# reports idle. No model and no network are involved, and the transcript
# holds only invented words.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${COPPICE_PANE:?fake-asker.sh runs inside a coppice pane}" "${COPPICE_SOCK:?}"
conf="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
dir="$conf/projects/e2e-asker"
mkdir -p "$dir"
transcript="$dir/${COPPICE_PANE//:/-}.jsonl"
cwd="$(pwd)"
n=0

now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
json() { node -e 'process.stdout.write(JSON.stringify(process.argv[1]))' "$1"; }

say() { # say ROLE TEXT: one transcript entry.
  n=$((n + 1))
  local role="$1" text="$2"
  if [ "$role" = user ]; then
    printf '{"type":"user","uuid":"e2e-k%d","timestamp":"%s","cwd":%s,"message":{"role":"user","content":%s}}\n' \
      "$n" "$(now)" "$(json "$cwd")" "$(json "$text")" >> "$transcript"
  else
    printf '{"type":"assistant","uuid":"e2e-l%d","timestamp":"%s","message":{"id":"msg_e2e_k%d","model":"claude-fake","role":"assistant","content":[{"type":"text","text":%s}]}}\n' \
      "$n" "$(now)" "$n" "$(json "$text")" >> "$transcript"
  fi
}

report() {
  node "$here/rpc.mjs" "$COPPICE_SOCK" report "$COPPICE_PANE" \
    "{\"transcript_path\":$(json "$transcript")}" "$1" "$2" > /dev/null 2>&1 || true
}

box() {
  printf '\033[2J\033[H'
  printf '%s\n' '────────────────────' '❯ ' '────────────────────'
}

: > "$transcript"
say user "Fix the fake clock."
say assistant $'Fixed the fake clock.\nShould I push it to the fake remote?'
box
report idle Stop

while IFS= read -r line; do
  line="${line%$'\r'}"
  [ -n "$line" ] || continue
  report working UserPromptSubmit
  say user "$line"
  sleep 1
  say assistant "Pushed to the fake remote."
  box
  report idle Stop
done
