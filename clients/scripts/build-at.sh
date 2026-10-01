#!/usr/bin/env bash
# clients/scripts/build-at.sh
# Prints the path of a coppice binary built from a worktree's HEAD, and
# builds it only if no one has built that commit with this toolchain before.
#
#   clients/scripts/build-at.sh go|rust WORKTREE   # print the binary's path
#   clients/scripts/build-at.sh --prune DAYS       # delete cache entries unused for more than DAYS
#
# A clean tree (no change or untracked file under harness/coppice*) is keyed
# by the HEAD sha and a toolchain key: the binary lives at
# $BUILD_AT_CACHE/<sha>/<go|rust>-<toolkey>/coppice and a second call for the
# same key prints it at once. The toolkey is a hash of `native.sh --print-key`
# (the native prefix: Z3, ghostty), and of `go version` (Go) or `rustc -V`
# (Rust), so a rebuilt prefix or a new compiler never gives a stale hit. A
# dirty tree is built every time, not cached, and the script says so on stderr.
#
# Two callers at the same key never build twice: the build runs under a flock
# on the key directory, and the caller that waits finds the binary there. The
# binary is copied in as a temp file and renamed, so no one sees half of it.
#
# The Rust coppice carries its plugins and the phone page with include_bytes!
# (harness/coppice-rs/build.rs), so a built binary does not read the build
# path at run time and is safe to keep. All Rust builds share ONE target dir
# ($BUILD_AT_RS_TARGET), under a flock that runs them one at a time, so the
# disk holds one set of dep crates and only the ~12 MB binary is kept per sha.
# Cargo rebuilds the coppice crate when the source or COPPICE_VERSION (the
# git describe, which differs by commit) changes.
#
# --prune removes only sha dirs (40 hex) and dirty/<name>/<kind> entries whose
# mtime is more than DAYS old (a hit touches its entry), and only in a cache
# this script made: the root must hold the marker file .build-at-cache, which
# the script writes when it creates the cache root. It takes no lock.
#
# Environment: BUILD_AT_CACHE (default /mnt/tera/.tt/bin), BUILD_AT_RS_TARGET
# (default /mnt/tera/.tt/bin-rs-target), BUILD_AT_ENV (a file to source before
# a build, default ~/opendaisugi-scratch/buildenv.sh), BUILD_AT_TOOLKEY (a
# fixed toolkey, for tests) and BUILD_AT_BUILDER (a command called as
# `BUILDER go|rust WORKTREE OUTPUT TARGET_DIR`, instead of the real build: for
# tests).
set -euo pipefail

cache="${BUILD_AT_CACHE:-/mnt/tera/.tt/bin}"
rs_target="${BUILD_AT_RS_TARGET:-/mnt/tera/.tt/bin-rs-target}"
marker=".build-at-cache"

usage() {
  echo "usage: build-at.sh go|rust WORKTREE | --prune DAYS" >&2
  exit 2
}

if [ "${1:-}" = "--prune" ]; then
  days="${2:-}"
  case "$days" in '' | *[!0-9]*) usage ;; esac
  if [ ! -f "$cache/$marker" ]; then
    echo "build-at.sh: $cache has no $marker, so this script did not make it; not pruning" >&2
    echo "  (if it is the cache, run: touch '$cache/$marker')" >&2
    exit 1
  fi
  # find -mtime +N means more than N+1 days; -mmin is exact.
  mins=$((days * 1440))
  prune() { # DIR: remove it and say so
    rm -rf -- "$1"
    echo "pruned $1" >&2
  }
  while IFS= read -r -d '' d; do
    prune "$d"
  done < <(find "$cache" -mindepth 1 -maxdepth 1 -type d -regextype posix-extended -regex '.*/[0-9a-f]{40}' -mmin "+$mins" -print0)
  # A dirty entry is dirty/<name>/<kind>; a rebuild changes that directory's mtime.
  if [ -d "$cache/dirty" ]; then
    while IFS= read -r -d '' d; do
      prune "$d"
    done < <(find "$cache/dirty" -mindepth 2 -maxdepth 2 -type d -mmin "+$mins" -print0)
    find "$cache/dirty" -mindepth 1 -maxdepth 1 -type d -empty -delete
  fi
  exit 0
fi

[ "$#" -eq 2 ] || usage
kind="$1"
wt="$2"
case "$kind" in go | rust) ;; *) usage ;; esac
[ -d "$wt" ] || { echo "build-at.sh: no such worktree: $wt" >&2; exit 2; }
wt="$(cd "$wt" && pwd -P)"
sha="$(git -C "$wt" rev-parse HEAD)"

# source_env: the build environment (zig, cmake, GOCACHE), if there is one.
source_env() {
  local envfile="${BUILD_AT_ENV:-$HOME/opendaisugi-scratch/buildenv.sh}"
  if [ -f "$envfile" ]; then
    set +u # the file may read variables the shell has not set
    # shellcheck disable=SC1090
    . "$envfile"
    set -u
  fi
}

# toolkey KIND WORKTREE: a short hash of what a binary depends on besides the sha.
toolkey() {
  if [ -n "${BUILD_AT_TOOLKEY:-}" ]; then
    echo "$BUILD_AT_TOOLKEY"
    return
  fi
  source_env
  {
    "$2/clients/go/scripts/native.sh" --print-key
    if [ "$1" = go ]; then go version; else rustc -V; fi
  } | sha256sum | cut -c1-12
}

# build KIND WORKTREE OUTPUT TARGET_DIR: build the binary to OUTPUT.
build() {
  local k="$1" w="$2" out="$3" target="$4"
  if [ -n "${BUILD_AT_BUILDER:-}" ]; then
    "$BUILD_AT_BUILDER" "$k" "$w" "$out" "$target"
    return
  fi
  source_env
  local native="$w/clients/go/scripts/native.sh"
  local prefix version
  prefix="$("$native" --print-prefix)"
  version="$(git -C "$w" describe --tags --always --dirty 2>/dev/null || echo unknown)"
  if [ "$k" = go ]; then
    export PKG_CONFIG=pkg-config
    export PKG_CONFIG_PATH="$prefix/share/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
    export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -DOPENDAISUGI_NATIVE=$("$native" --print-key)"
    (cd "$w/harness/coppice" &&
      go build -trimpath -tags netgo \
        -ldflags="-s -w -X github.com/opendaisugi/coppice/internal/cli.version=$version" \
        -o "$out" ./cmd/coppice)
  else
    mkdir -p "$target"
    (
      flock 8
      cd "$w/harness/coppice-rs"
      CARGO_TARGET_DIR="$target" COPPICE_GHOSTTY_PREFIX="$prefix" COPPICE_VERSION="$version" \
        cargo build --release --locked --bin coppice
      cp "$target/release/coppice" "$out"
    ) 8>"$target/.build.lock"
  fi
}

# The marker says this script made the cache root, so --prune may clean it.
# It is written only for a root that did not exist, or exists and is empty.
if [ ! -e "$cache/$marker" ]; then
  if [ ! -d "$cache" ] || [ -z "$(ls -A "$cache")" ]; then
    mkdir -p "$cache"
    : >"$cache/$marker"
  fi
fi

# Dirty: a change or an untracked file under harness/coppice*.
if [ -n "$(git -C "$wt" status --porcelain --untracked-files=all -- 'harness/coppice*')" ]; then
  # The name holds a hash of the real path, so two worktrees of one name differ.
  name="$(basename "$wt")-$(printf %s "$wt" | sha256sum | cut -c1-8)"
  echo "build-at.sh: $wt has changes under harness/coppice*; building without the cache" >&2
  dir="$cache/dirty/$name/$kind"
  mkdir -p "$dir"
  tmp="$dir/coppice.tmp.$$"
  trap 'rm -f "$tmp"' EXIT
  build "$kind" "$wt" "$tmp" "$rs_target"
  chmod +x "$tmp"
  mv -f "$tmp" "$dir/coppice"
  echo "$dir/coppice"
  exit 0
fi

dir="$cache/$sha/$kind-$(toolkey "$kind" "$wt")"
mkdir -p "$dir"
exec 9<"$dir"
flock 9
if [ ! -x "$dir/coppice" ]; then
  tmp="$dir/coppice.tmp.$$"
  trap 'rm -f "$tmp"' EXIT
  build "$kind" "$wt" "$tmp" "$rs_target"
  chmod +x "$tmp"
  mv -f "$tmp" "$dir/coppice"
fi
# A hit keeps the entry young, so --prune removes only what no one uses.
touch "$cache/$sha" "$dir" 2>/dev/null || true
echo "$dir/coppice"
