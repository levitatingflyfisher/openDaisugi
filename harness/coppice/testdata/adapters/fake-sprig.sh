#!/usr/bin/env bash
# A stand-in for one invocation of the real sprig, matching
# harness/sprig/cli.go's own flag handling and session-file refusals well
# enough to exercise the adapter's per-turn argv construction and its
# error-surfacing, without a model backend:
#
#   --session-dir DIR (required)
#   --session ID      fresh: refuses if DIR/ID.jsonl already exists, same
#                      message shape NewSessionWriter's own refusal has
#                      ("session file exists: PATH")
#   --resume ID        resume: refuses if DIR/ID.jsonl does not exist, same
#                      shape OpenSessionWriter's own os.Stat failure has
#   --                 stops flag parsing, same as sprig's own flag.Parse;
#                      everything after is the task
#
# On success this writes the session header (fresh only), then a "prompt"
# row carrying the task text verbatim - the same row OnPrompt writes first
# thing in a real turn (harness/sprig/loop.go's own Agent.Run, session_tree.go's
# SessionWriter.OnPrompt) - then appends COPPICE_SPRIG_FIXTURE's rows to
# DIR/ID.jsonl, standing in for whatever OnAssistant/OnToolCall/OnVerdict/
# OnToolResult calls the rest of a real turn would make, prints one "answer"
# line to stdout, and exits 0.
#
# COPPICE_SPRIG_ECHO_ARGV, appended to (not overwritten) on every
# invocation, so a test can see every turn's own argv in order - same
# convention as COPPICE_CODEX_ECHO_ARGV.
#
# COPPICE_SPRIG_SLEEP, when set, sleeps that many seconds after the file
# checks above but before writing anything - a slow turn, so a test can
# reliably observe "a turn is still running" instead of racing a near-
# instant script.
#
# COPPICE_SPRIG_EXIT, when set, exits with that code right after writing
# the fixture's rows and the answer line, instead of the default 0.
#
# COPPICE_SPRIG_FAIL_BEFORE_SESSION, when set, exits 2 immediately after
# logging argv, before --session-dir is even checked - standing in for a
# bad flag in the caller's own extra argv making the real flag.FlagSet
# reject the whole invocation before openOrNewSessionWriter ever runs, so
# the session file this fake would otherwise write never gets created at
# all even though the process itself started fine.
set -euo pipefail

orig_argv="$*"

session_dir=""
session_id=""
resume_id=""
task_parts=()
in_task=false
while [ $# -gt 0 ]; do
  if $in_task; then
    task_parts+=("$1"); shift; continue
  fi
  case "$1" in
    --session-dir) session_dir="$2"; shift 2 ;;
    --session) session_id="$2"; shift 2 ;;
    --resume) resume_id="$2"; shift 2 ;;
    --) in_task=true; shift ;;
    *) shift ;; # an option this fake does not model (--gate, --max-turns, ...): ignore
  esac
done
task="${task_parts[*]:-}"

if [ -n "${COPPICE_SPRIG_ECHO_ARGV:-}" ]; then
  printf '%s\n' "$orig_argv" >> "$COPPICE_SPRIG_ECHO_ARGV"
fi

if [ -n "${COPPICE_SPRIG_FAIL_BEFORE_SESSION:-}" ]; then
  echo "flag provided but not defined: -bogus" >&2
  exit 2
fi

if [ -n "$resume_id" ] && [ -z "$session_dir" ]; then
  # cli.go:59's own refusal, quoted verbatim: real sprig does not require
  # --session-dir in general (it just runs with no session tree), only when
  # --resume is given without it.
  echo "sprig: --resume needs --session-dir too (which session store to resume from)" >&2
  exit 1
fi
if [ -z "$session_dir" ]; then
  echo "sprig: --session-dir is required" >&2
  exit 1
fi
mkdir -p "$session_dir"

fresh=false
if [ -n "$resume_id" ]; then
  file="$session_dir/$resume_id.jsonl"
  if [ ! -f "$file" ]; then
    echo "sprig: session tree: stat $file: no such file or directory" >&2
    exit 1
  fi
elif [ -n "$session_id" ]; then
  file="$session_dir/$session_id.jsonl"
  if [ -f "$file" ]; then
    echo "sprig: session tree: session file exists: $file" >&2
    exit 1
  fi
  fresh=true
else
  echo "sprig: no --session or --resume given" >&2
  exit 1
fi

if [ -n "${COPPICE_SPRIG_SLEEP:-}" ]; then
  sleep "$COPPICE_SPRIG_SLEEP"
fi

if $fresh; then
  printf '{"type":"session","v":1,"id":"%s","harness":"sprig","cwd":"%s","ts":0}\n' \
    "$session_id" "$PWD" > "$file"
fi

# json_escape backslash-escapes backslashes and double-quotes so an
# arbitrary task (this fake is exercised with one beginning "-", among
# others) can never break the row's own JSON.
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

printf '{"type":"prompt","text":"%s"}\n' "$(json_escape "$task")" >> "$file"

if [ -n "${COPPICE_SPRIG_FIXTURE:-}" ]; then
  cat "$COPPICE_SPRIG_FIXTURE" >> "$file"
fi

echo "sprig: turn done: $task"

if [ -n "${COPPICE_SPRIG_EXIT:-}" ]; then
  exit "$COPPICE_SPRIG_EXIT"
fi
