#!/usr/bin/env bash
# clients/go/scripts/check-paths.sh BINARY...
# Fails when a binary carries a path of the box that built it: $HOME, this
# checkout, the Go module cache or the Go build cache. A published binary
# must not name the machine or the person it was built by.
set -euo pipefail

command -v strings >/dev/null 2>&1 || { echo "check-paths.sh: strings (binutils) is not on PATH" >&2; exit 1; }
[ "$#" -gt 0 ] || { echo "usage: check-paths.sh BINARY..." >&2; exit 2; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checkout="$(cd "$here/../../.." && pwd -P)"
needles=("$checkout")
[ -n "${HOME:-}" ] && [ "$HOME" != "/" ] && needles+=("$HOME")
if command -v go >/dev/null 2>&1; then
  for v in GOMODCACHE GOCACHE GOPATH; do
    d="$(go env "$v" 2>/dev/null || true)"
    [ -n "$d" ] && needles+=("$d")
  done
fi

args=()
for n in "${needles[@]}"; do args+=(-e "$n"); done

bad=0
for bin in "$@"; do
  [ -f "$bin" ] || { echo "check-paths.sh: $bin is not a file" >&2; exit 2; }
  hits="$(strings -a -n 4 "$bin" | grep -F "${args[@]}" || true)"
  if [ -n "$hits" ]; then
    bad=1
    echo "$bin: $(printf '%s\n' "$hits" | wc -l) strings name a build-machine path, for example:" >&2
    printf '%s\n' "$hits" | sed -n '1,5p' >&2
  else
    echo "$bin: no build-machine path"
  fi
done
exit "$bad"
