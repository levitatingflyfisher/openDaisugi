#!/usr/bin/env bash
# harness/coppice/e2e/run.sh: the coppice end-to-end suite.
#
# It starts one coppice server on a scratch socket and data directory,
# with fake harnesses only (bash, and fake-trust.sh standing in for claude),
# and then:
#   1. tui: runs the coppice TUI inside a coppice pane, so the server's own
#      virtual terminal is the screen the check reads (tui.sh);
#   2. web: opens the web floor in headless chromium (web.mjs) and checks
#      the roster, a live tile, a gate ask bar and the trust screen buttons.
# Screenshots and TUI screen dumps go to $E2E_DIR/shots.
#
#   harness/coppice/e2e/run.sh [web|tui|all]
#
# Environment:
#   E2E_DIR      scratch directory, on real disk (default:
#                ${XDG_CACHE_HOME:-$HOME/.cache}/coppice-e2e). It is removed
#                and made again at the start of a run and kept afterwards.
#   COPPICE_BIN  a built coppice; default: build one into $E2E_DIR/bin.
#   E2E_PORT     the web floor's loopback port (default 18490).
#
# The suite never runs a real claude, model or upstream: it stops at once
# if the harness table it writes would reach one, and every coppice call
# names the scratch socket and data directory.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module="$(dirname "$here")"
which="${1:-all}"
case "$which" in web|tui|all) ;; *) echo "usage: run.sh [web|tui|all]" >&2; exit 2 ;; esac

E2E_DIR="${E2E_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/coppice-e2e}"
port="${E2E_PORT:-18490}"
rm -rf "$E2E_DIR"
mkdir -p "$E2E_DIR"/{bin,cfg/coppice,share,state,data,run,proj/alpha,proj/beta,shots}
chmod 700 "$E2E_DIR/run"

if [ -z "${COPPICE_BIN:-}" ]; then
  echo "building coppice"
  (cd "$module" && go build -o "$E2E_DIR/bin/coppice" ./cmd/coppice)
  COPPICE_BIN="$E2E_DIR/bin/coppice"
fi

# The socket path must stay under the 108-byte limit of a unix socket.
sock="$E2E_DIR/run/c.sock"
if [ "${#sock}" -gt 100 ]; then
  echo "run.sh: $sock is too long for a unix socket; set a shorter E2E_DIR" >&2
  exit 2
fi

# A scratch HOME and XDG tree, so nothing reads or writes the operator's
# own coppice config, layout or server.
# The browsers live under the real HOME's cache; keep that path before HOME moves.
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}"
export HOME="$E2E_DIR/home"
mkdir -p "$HOME"
export XDG_CONFIG_HOME="$E2E_DIR/cfg" XDG_DATA_HOME="$E2E_DIR/share"
export XDG_STATE_HOME="$E2E_DIR/state" XDG_RUNTIME_DIR="$E2E_DIR/run"
export COPPICE_NO_AUTOSTART=1
unset COPPICE_SOCKET ANTHROPIC_API_KEY ANTHROPIC_BASE_URL OPENAI_API_KEY 2>/dev/null || true

screen="$module/testdata/screens/claude/blocked-10.txt"
cat > "$XDG_CONFIG_HOME/coppice/coppice.toml" <<EOT
default = "sh"
projects = ["$E2E_DIR/proj/alpha", "$E2E_DIR/proj/beta"]

[harness.sh]
command = "bash"
args = ["--norc", "--noprofile"]

[harness.claude]
command = "bash"
args = ["$here/fake-trust.sh", "$screen"]

[harness.codex]
command = "bash"
args = ["--norc", "--noprofile"]
EOT
if grep -qE 'command = "(claude|codex|sprig|opencode|pi)"' "$XDG_CONFIG_HOME/coppice/coppice.toml"; then
  echo "run.sh: the harness table names a real agent; refusing to run" >&2
  exit 1
fi

cop() { "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" "$@"; }
id_of() { sed -n 's/.*"pane": *"\([^"]*\)".*/\1/p' | head -n1; }

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  cop server stop >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" server start --foreground \
  > "$E2E_DIR/server.log" 2>&1 < /dev/null &
pids+=("$!")
for _ in $(seq 1 100); do
  cop server status >/dev/null 2>&1 && break
  sleep 0.1
done
cop server status > /dev/null

# The floor both checks read: two bash panes and claude's trust screen,
# with a gate ask held on "e2e ask" the way a gate hook reports one.
shell=$(cop pane create --json --label "e2e shell" --kind pty --cwd "$E2E_DIR/proj/alpha" -- bash --norc --noprofile | id_of)
ask=$(cop pane create --json --label "e2e ask" --kind pty --cwd "$E2E_DIR/proj/beta" -- bash --norc --noprofile | id_of)
trust=$(cop pane create --json --label "e2e trust" --kind pty --harness claude --cwd "$E2E_DIR/proj/alpha" | id_of)
[ -n "$shell" ] && [ -n "$ask" ] && [ -n "$trust" ] || { echo "run.sh: a pane was not made" >&2; exit 1; }
deadline=$(( $(date +%s) + 3600 ))
node "$here/rpc.mjs" "$sock" report "$ask" \
  "{\"mode\":\"enforcing\",\"verdict\":{\"decision\":\"ask\",\"tool\":\"Bash\",\"clause\":\"shell: git push\"},\"ask\":{\"id\":\"toolu_e2e1\",\"tool\":\"Bash\",\"summary\":\"git push origin main\",\"deadline\":$deadline,\"tier\":\"undoable\"}}" \
  blocked "awaiting operator: push" > "$E2E_DIR/report.log"
export COPPICE_BIN sock E2E_DIR shell ask trust

status=0
# The TUI goes first: the web check answers the trust screen.
if [ "$which" = tui ] || [ "$which" = all ]; then
  "$here/tui.sh" || status=1
fi
if [ "$which" = web ] || [ "$which" = all ]; then
  "$here/web.sh" "$port" || status=1
fi
if [ "$status" -eq 0 ]; then
  echo "e2e ok; screenshots and screens in $E2E_DIR/shots"
else
  echo "e2e FAILED; logs, screenshots and screens in $E2E_DIR" >&2
fi
exit "$status"
