#!/bin/bash
# scripts/boxcmp.sh <branch> <compare>...
# The golden compares for the Go and the Rust daisugi in the box's build
# checkout (~/daisugi-ci/build), one after another. It is the repo's copy of
# ~/daisugi-ci/boxcmp.sh on the GPU box; copy it there to use it.
#
# The Rust twins are frozen (owner ruling 2026-11-06): cases marked go_only
# test what only the Go side does. The Rust run gets --skip-go-only from
# every compare that knows the flag, so it is not counted as a disagreement.
#
#   boxcmp.sh round9 tree warden fence
#
# Scratch goes to $BOXCMP_SCRATCH, default /tmp/dc on the box.
set -u
b=$1
shift
R=$HOME/daisugi-ci
export HOME=$R/home
cd "$R/build" || exit 1
echo "== $b $(git log --oneline -1)"
dc=${BOXCMP_SCRATCH:-/tmp/dc}
mkdir -p "$dc"
for v in CLI F GARDEN L MODELS PACK PATHWAY RANK ROUTER TREE VOICE WARDEN WARDEN_SERVER WEAVE K1 K2 K3 K4; do
  export "DAISUGI_${v}_SCRATCH=$dc/$(echo "$v" | tr A-Z_ a-z-)"
done
B=$PWD
RB=$B/clients/rust/target/release
for c in "$@"; do
  if [ "$c" = warden ]; then
    g=$B/clients/go/daisugi-warden r=$RB/daisugi-warden s=warden_compare.py
  else
    g=$B/clients/go/daisugi r=$RB/daisugi s=${c}_compare.py
  fi
  skip=()
  if grep -q -- '--skip-go-only' "clients/$s"; then
    skip=(--skip-go-only)
  fi
  for p in go:$g rust:$r; do
    echo "== $c ${p%%:*}"
    extra=()
    [ "${p%%:*}" = rust ] && extra=("${skip[@]}")
    uv run --no-sync python "clients/$s" --binary "${p#*:}" "${extra[@]}" >"$dc/out.txt" 2>&1
    echo "exit $?"
    grep -i -E "agree|disagree|stale|refused|not ported|fail" "$dc/out.txt" | tail -6
  done
done
rm -rf "$dc"
