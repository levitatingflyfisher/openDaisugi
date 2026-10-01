#!/usr/bin/env bash
# scripts/smoke-tarball.sh TARBALL [SCRATCH]
# The install smoke test: from the release tarball scripts/release.sh
# writes to the first gate decision, in a scratch HOME with PATH limited to
# the unpacked binaries and /usr/bin:/bin. Nothing reaches a model or the
# network, and the real HOME is never read.
#
# It checks, and stops on the first failure:
#   - the tarball unpacks and each binary reports a version;
#   - `daisugi gate init` and `daisugi install --gate --enforce --runtime
#     claude --yes` succeed and write the PreToolUse hook into
#     $HOME/.claude/settings.json;
#   - the hook command allows a Read inside the workspace (exit 0);
#   - the hook command denies `rm -rf /etc` (exit 2, the deny reason on
#     stderr or a deny decision on stdout);
#   - a coppice server starts, answers status and stops on a scratch socket.
#
# SCRATCH (default: ${XDG_CACHE_HOME:-$HOME/.cache}/opendaisugi-smoke) is
# removed and made again, and kept afterwards for reading.
set -euo pipefail

tarball="${1:?usage: smoke-tarball.sh TARBALL [SCRATCH]}"
tarball="$(cd "$(dirname "$tarball")" && pwd)/$(basename "$tarball")"
S="${2:-${XDG_CACHE_HOME:-$HOME/.cache}/opendaisugi-smoke}"
rm -rf "$S"
mkdir -p "$S/bin" "$S/.claude" "$S/cache" "$S/work"
run() { env -i HOME="$S" PATH="$S/bin:/usr/bin:/bin" XDG_CACHE_HOME="$S/cache" LANG=C.UTF-8 "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

if run sh -c 'command -v claude' >/dev/null 2>&1; then
  fail "a claude is on the smoke PATH; this test must never reach one"
fi

t0=$(date +%s%N)
tar -xzf "$tarball" --strip-components=1 -C "$S/bin"
for b in daisugi coppice sprig; do
  [ -x "$S/bin/$b" ] || fail "the tarball has no $b"
  v=$(run "$b" --version 2>&1) || fail "$b --version exited non-zero: $v"
  echo "version: $v"
done

run sh -c 'cd "$HOME/work" && daisugi gate init --workspace "$HOME/work"' > "$S/init.out" 2>&1 ||
  { cat "$S/init.out" >&2; fail "daisugi gate init"; }
run daisugi install --gate --enforce --runtime claude --yes > "$S/install.out" 2>&1 ||
  { cat "$S/install.out" >&2; fail "daisugi install --gate"; }
[ -f "$S/.claude/settings.json" ] || fail "install wrote no .claude/settings.json"
# install also writes the capture hook (`daisugi hook record`) beside the
# gate's, as the Python CLI does; the gate's is the one that is not it.
cmd="$(python3 -c '
import json, sys
cmds = [h["command"] for e in json.load(open(sys.argv[1]))["hooks"]["PreToolUse"] for h in e["hooks"]]
gate = [c for c in cmds if "hook record" not in c]
assert len(gate) == 1, cmds
print(gate[0])' "$S/.claude/settings.json")" ||
  fail "settings.json has not exactly one PreToolUse gate hook command"
echo "hook command: $cmd"

allow="{\"session_id\":\"s1\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"$S/work/a.txt\"},\"cwd\":\"$S/work\",\"hook_event_name\":\"PreToolUse\"}"
deny="{\"session_id\":\"s1\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"rm -rf /etc\"},\"cwd\":\"$S/work\",\"hook_event_name\":\"PreToolUse\"}"

set +e
printf '%s' "$allow" | run sh -c "$cmd" > "$S/allow.out" 2> "$S/allow.err"
ae=$?
t1=$(date +%s%N)
printf '%s' "$deny" | run sh -c "$cmd" > "$S/deny.out" 2> "$S/deny.err"
de=$?
set -e
echo "allow: exit=$ae stdout=$(head -c 300 "$S/allow.out")"
echo "deny:  exit=$de stdout=$(head -c 300 "$S/deny.out") stderr=$(head -c 300 "$S/deny.err")"
[ "$ae" -eq 0 ] || fail "the hook did not allow a Read in the workspace (exit $ae)"
grep -q '"deny"' "$S/allow.out" && fail "the hook printed a deny for a Read in the workspace"
if [ "$de" -ne 2 ] && ! grep -q '"permissionDecision": *"deny"' "$S/deny.out"; then
  fail "the hook did not deny rm -rf /etc (exit $de)"
fi
echo "unpack to first gate decision: $(( (t1 - t0) / 1000000 )) ms"

top=(--socket "$S/c.sock" --data-dir "$S/data")
run coppice "${top[@]}" server start > "$S/coppice.out" 2>&1 || { cat "$S/coppice.out" >&2; fail "coppice server start"; }
run coppice "${top[@]}" server status >> "$S/coppice.out" 2>&1 || { cat "$S/coppice.out" >&2; fail "coppice server status"; }
run coppice "${top[@]}" server stop >> "$S/coppice.out" 2>&1 || { cat "$S/coppice.out" >&2; fail "coppice server stop"; }
echo "coppice: server start, status and stop ok"
echo "smoke ok; everything it wrote is in $S"
