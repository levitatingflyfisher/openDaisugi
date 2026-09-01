#!/usr/bin/env bash
# harness/coppice/scripts/preflight.sh
# Reports whether this machine can build coppice. Exit 0 when it can, 1 when it
# cannot. Every failure line names the command that fixes it.
set -uo pipefail

prefix="${COPPICE_GHOSTTY_PREFIX:-$HOME/.local/ghostty-vt}"
missing=0

say_missing() {
  printf 'missing: %s\n  fix: %s\n' "$1" "$2" >&2
  missing=1
}

command -v go >/dev/null 2>&1 || say_missing "go" "install Go 1.26 or newer"
if command -v go >/dev/null 2>&1; then
  if [ "$(go env GOTOOLCHAIN)" = "local" ]; then
    say_missing "a switchable Go toolchain" "run harness/coppice/scripts/toolchain.sh"
  fi
fi
command -v zig >/dev/null 2>&1 || say_missing "zig 0.16" "run harness/coppice/scripts/toolchain.sh"
command -v cmake >/dev/null 2>&1 || say_missing "cmake" "run harness/coppice/scripts/toolchain.sh"
command -v pkg-config >/dev/null 2>&1 || say_missing "pkg-config" "install pkgconf from your distribution"
[ -f "$prefix/share/pkgconfig/libghostty-vt-static.pc" ] ||
  say_missing "libghostty-vt at $prefix" "run harness/coppice/scripts/toolchain.sh"

if [ "$missing" -ne 0 ]; then
  echo "coppice cannot build here yet. Fix the lines above, then run this script again." >&2
  exit 1
fi
echo "coppice can build here."
