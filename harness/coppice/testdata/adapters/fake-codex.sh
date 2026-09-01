#!/usr/bin/env bash
# A stand-in for `codex exec --json` and `codex exec resume <id> --json`. It
# records its argv, replays a fixture, then exits - the one-process-per-turn
# shape the real binary has.
#
# COPPICE_CODEX_SLEEP, when set, sleeps that many seconds (fractional is
# fine) BEFORE replaying the fixture - a slow turn, so a test can reliably
# observe "a turn is still running" instead of racing a near-instant cat.
#
# COPPICE_CODEX_EXIT, when set, exits with that code right after replaying
# the fixture instead of the default 0 - the shape a crash, or a turn that
# never sent turn.completed/turn.failed, takes.
#
# COPPICE_CODEX_ORPHAN, when set, backgrounds a 40s sleep right before this
# script exits - a fire-and-forget job (`x &`) that inherits this process's
# stdout pipe and keeps its write end open long after this script itself is
# gone, the shape that would hang a naive read-to-EOF-then-Wait forever.
set -euo pipefail
fixture="${COPPICE_CODEX_FIXTURE:?set COPPICE_CODEX_FIXTURE to a JSONL file}"
if [ -n "${COPPICE_CODEX_ECHO_ARGV:-}" ]; then
  printf '%s\n' "$*" >> "$COPPICE_CODEX_ECHO_ARGV"
fi
if [ -n "${COPPICE_CODEX_SLEEP:-}" ]; then
  sleep "$COPPICE_CODEX_SLEEP"
fi
cat "$fixture"
if [ -n "${COPPICE_CODEX_ORPHAN:-}" ]; then
  sleep 40.4321 &
fi
if [ -n "${COPPICE_CODEX_EXIT:-}" ]; then
  exit "$COPPICE_CODEX_EXIT"
fi
