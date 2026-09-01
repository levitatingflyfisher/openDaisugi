#!/usr/bin/env bash
# scripts/preflight.sh
# Checks that this box has every tool the Go build of coppice, sprig and
# daisugi needs, and names the Arch package for each one that is missing.
# Exit 0 when the build can run, 1 when it cannot. It builds nothing.
# scripts/install.sh and scripts/release.sh run it first.
set -uo pipefail

ZIG_VERSION="0.16.0"
GO_MIN_MINOR=26   # go.mod asks for go 1.26
missing=0

need() { # tool arch-package [note]
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing: %s (Arch: pacman -S %s)%s\n' "$1" "$2" "${3:+. $3}" >&2
    missing=1
  fi
}

need go go
need zig zig "It must be zig $ZIG_VERSION; harness/coppice/scripts/toolchain.sh installs that version into ~/.local"
need cmake cmake
need gcc gcc
need make make
need pkg-config pkgconf
need git git
need curl curl
need python3 python
need strip binutils
need strings binutils

if command -v zig >/dev/null 2>&1 && [ "$(zig version)" != "$ZIG_VERSION" ]; then
  printf 'wrong version: zig is %s, the build needs %s exactly. Run harness/coppice/scripts/toolchain.sh, or put zig %s first on PATH\n' \
    "$(zig version)" "$ZIG_VERSION" "$ZIG_VERSION" >&2
  missing=1
fi
if command -v go >/dev/null 2>&1; then
  minor="$(go env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')"
  if [ -n "$minor" ] && [ "$minor" -lt "$GO_MIN_MINOR" ] && [ "$(go env GOTOOLCHAIN)" = "local" ]; then
    printf 'wrong version: go is %s and GOTOOLCHAIN=local, the build needs go 1.%s. Update go (Arch: pacman -S go), or run: go env -w GOTOOLCHAIN=auto\n' \
      "$(go env GOVERSION)" "$GO_MIN_MINOR" >&2
    missing=1
  fi
fi

if [ "$missing" -ne 0 ]; then
  echo "The build cannot run here yet. Fix the lines above, then run it again." >&2
  exit 1
fi
echo "preflight: every build tool is here."
