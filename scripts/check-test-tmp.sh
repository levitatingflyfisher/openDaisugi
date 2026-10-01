#!/usr/bin/env bash
# Run the coppice test suites with TMPDIR set to a new empty directory, then
# fail if anything is left in it. A test that makes a temp dir must remove it
# again, on success and on failure. /tmp is RAM on this box, so a leak costs
# memory until the next reboot.
#
# Usage:
#   scripts/check-test-tmp.sh [go] [rust]     default: both
#
# The Rust half runs `cargo test --release`: a debug build does not link on
# the build box (zig as the C++ compiler leaves ubsan symbols undefined).
#
# The build environment is yours to set up first (the pinned zig and cmake,
# PKG_CONFIG_PATH for libghostty-vt, CARGO_TARGET_DIR). This script sets
# TMPDIR and COPPICE_TEST_KEEP_TMPDIR (which tells the Rust test home to leave
# TMPDIR alone, so a leak shows up here and is not swept at exit). It makes
# the empty directory under $CHECK_TMP_BASE (default /tmp) and removes it
# when the run is clean. A dirty run keeps it so
# you can read it. Keep the base short: some tests bind unix sockets, and a
# socket path cannot pass 107 bytes.
#
# Exit codes: 0 clean, 1 a suite failed, 2 something was left behind.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
base="${CHECK_TMP_BASE:-/tmp}"
want=("$@")
[ ${#want[@]} -eq 0 ] && want=(go rust)

status=0
for suite in "${want[@]}"; do
  case "$suite" in
    go) dir="$root/harness/coppice"; cmd=(go test ./...) ;;
    rust) dir="$root/harness/coppice-rs"; cmd=(cargo test --release --offline) ;;
    *) echo "check-test-tmp: unknown suite '$suite' (go or rust)" >&2; exit 1 ;;
  esac
  tmp="$(mktemp -d "$base/cXXXX")" || exit 1
  echo "check-test-tmp: $suite with TMPDIR=$tmp"
  if ! (cd "$dir" && TMPDIR="$tmp" COPPICE_TEST_KEEP_TMPDIR=1 "${cmd[@]}"); then
    echo "check-test-tmp: the $suite suite failed" >&2
    status=1
  fi
  left="$(find "$tmp" -mindepth 1 -maxdepth 1 | sort)"
  if [ -n "$left" ]; then
    echo "check-test-tmp: the $suite suite left $(printf '%s\n' "$left" | wc -l) entries in $tmp:" >&2
    printf '%s\n' "$left" | head -20 >&2
    [ "$status" -eq 0 ] && status=2
  else
    rmdir "$tmp"
    echo "check-test-tmp: $suite clean"
  fi
done
exit "$status"
