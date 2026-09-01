#!/usr/bin/env bash
# A stand-in for `claude` that replays a recorded stream-json file. It reads one
# JSON user message per line on stdin and replays the fixture for each, so the
# adapter's prompt path and its parser are both exercised without a subscription.
#
# COPPICE_CLAUDE_EXIT, when set, makes this exit with that code right after
# replaying the fixture once, instead of looping for another stdin line - the
# natural-exit shape a real claude takes when it errors out or is killed by
# something other than this adapter's own Stop().
set -euo pipefail
fixture="${COPPICE_CLAUDE_FIXTURE:?set COPPICE_CLAUDE_FIXTURE to a stream-json file}"
if [ -n "${COPPICE_CLAUDE_ECHO_ARGV:-}" ]; then
  printf '%s\n' "$*" > "$COPPICE_CLAUDE_ECHO_ARGV"
fi
while IFS= read -r line; do
  if [ -n "${COPPICE_CLAUDE_ECHO_STDIN:-}" ]; then
    printf '%s\n' "$line" >> "$COPPICE_CLAUDE_ECHO_STDIN"
  fi
  cat "$fixture"
  if [ -n "${COPPICE_CLAUDE_EXIT:-}" ]; then
    exit "$COPPICE_CLAUDE_EXIT"
  fi
done
