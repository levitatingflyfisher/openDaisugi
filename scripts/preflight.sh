#!/usr/bin/env bash
# scripts/preflight.sh
# Checks that this box has every tool the Go build of coppice, sprig and
# daisugi needs, and names each one that is missing.
# Exit 0 when the build can run, 1 when it cannot. It builds nothing.
# scripts/install.sh and scripts/release.sh run it first.
#
#   --fetchable   skip the tools install.sh fetches itself (zig, cmake, and
#                 a go older than 1.26, which GOTOOLCHAIN=auto replaces).
#                 What is left is what only your distribution can supply.
set -uo pipefail

fetchable=0
case "${1:-}" in
  "") ;;
  --fetchable) fetchable=1 ;;
  *) echo "preflight.sh: unknown option $1" >&2; exit 2 ;;
esac

ZIG_VERSION="0.16.0"
GO_MIN_MINOR=26   # go.mod asks for go 1.26
GO_FETCH_MINOR=21 # GOTOOLCHAIN=auto, which fetches go 1.26, needs go 1.21
missing=0

need() { # tool package [note]
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing: %s (install the %s package from your distribution)%s\n' "$1" "$2" "${3:+. $3}" >&2
    missing=1
  fi
}

need go go
if [ "$fetchable" -eq 0 ]; then
  need zig zig "It must be zig $ZIG_VERSION; harness/coppice/scripts/toolchain.sh installs that version"
  need cmake cmake
fi
# install.sh unpacks the zig it fetches (a .tar.xz) with xz.
if [ "$fetchable" -eq 1 ] && { ! command -v zig >/dev/null 2>&1 || [ "$(zig version 2>/dev/null)" != "$ZIG_VERSION" ]; }; then
  need xz xz-utils "install.sh unpacks the zig it fetches with it (the package is named xz on some distributions)"
fi
need gcc gcc
need make make
need pkg-config "pkgconf or pkg-config"
need git git
need curl curl
need python3 python3
need strip binutils
need strings binutils

if command -v go >/dev/null 2>&1; then
  fetch_minor="$(go env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')"
  if [ -n "$fetch_minor" ] && [ "$fetch_minor" -lt "$GO_FETCH_MINOR" ]; then
    printf 'wrong version: go is 1.%s. The build needs go 1.%s or later to fetch go 1.%s. Install a newer go from https://go.dev/dl\n' \
      "$fetch_minor" "$GO_FETCH_MINOR" "$GO_MIN_MINOR" >&2
    missing=1
  fi
fi
if [ "$fetchable" -eq 0 ] && command -v zig >/dev/null 2>&1 && [ "$(zig version)" != "$ZIG_VERSION" ]; then
  printf 'wrong version: zig is %s, the build needs %s exactly. Run harness/coppice/scripts/toolchain.sh, or put zig %s first on PATH\n' \
    "$(zig version)" "$ZIG_VERSION" "$ZIG_VERSION" >&2
  missing=1
fi
if [ "$fetchable" -eq 0 ] && command -v go >/dev/null 2>&1; then
  minor="$(go env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')"
  if [ -n "$minor" ] && [ "$minor" -lt "$GO_MIN_MINOR" ] && [ "$(go env GOTOOLCHAIN)" = "local" ]; then
    printf 'wrong version: go is %s and GOTOOLCHAIN=local, the build needs go 1.%s. Update go, or run the build with GOTOOLCHAIN=auto in its environment\n' \
      "$(go env GOVERSION)" "$GO_MIN_MINOR" >&2
    missing=1
  fi
fi

if [ "$missing" -ne 0 ]; then
  echo "The build cannot run here yet. Fix the lines above, then run it again." >&2
  exit 1
fi
echo "preflight: every build tool is here."
