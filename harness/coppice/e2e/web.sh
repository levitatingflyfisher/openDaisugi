#!/usr/bin/env bash
# web.sh PORT: the web floor check. run.sh starts the server, makes the
# panes and exports COPPICE_BIN, sock and E2E_DIR; this script
# serves the floor on 127.0.0.1:PORT and runs web.mjs against it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
port="${1:?usage: web.sh PORT}"
: "${COPPICE_BIN:?run web.sh through run.sh}" "${sock:?}" "${E2E_DIR:?}"
cop() { "$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" "$@"; }
token=$(cop web token --qr=false | awk '$1 == "token" {print $2; exit}')
[ -n "$token" ] || { echo "web.sh: no web token" >&2; exit 1; }

"$COPPICE_BIN" --socket "$sock" --data-dir "$E2E_DIR/data" web serve --tls off --listen "127.0.0.1:$port" --gate-root "$E2E_DIR/gate" --qr=false \
  > "$E2E_DIR/web.log" 2>&1 < /dev/null &
web=$!
# The binary is started directly, not through cop(), so $! is its own pid
# and the trap stops the server itself, not a subshell around it.
trap 'kill "$web" 2>/dev/null || true; wait "$web" 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do
  (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break
  kill -0 "$web" 2>/dev/null || { echo "web.sh: the web server exited" >&2; cat "$E2E_DIR/web.log" >&2; exit 1; }
  sleep 0.1
done

node "$here/web.mjs" "http://127.0.0.1:$port" "$token" "$E2E_DIR/shots"
