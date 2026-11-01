#!/usr/bin/env bash
# scripts/box-gotest.sh
# Runs the three Go suites one after another, each as go test -p 1 ./...,
# and exits 1 when any of them failed. The exit status of a suite is read
# from PIPESTATUS in this shell, not in a subshell, so a failing suite cannot
# be hidden by the tee behind it.
#
#   box-gotest.sh [SUITE...]   suites default to clients/go harness/coppice
#                              harness/sprig
#
# TMPDIR defaults to /var/tmp/od (short, and outside HOME). Logs go to
# ${BOX_GOTEST_LOG:-$TMPDIR/box-gotest}/<suite with / as ->.log. GO names the
# go tool (a test replaces it).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export TMPDIR="${TMPDIR:-/var/tmp/od}"
mkdir -p "$TMPDIR"
export GOFLAGS="${GOFLAGS:--p=2}"
log="${BOX_GOTEST_LOG:-$TMPDIR/box-gotest}"
mkdir -p "$log"
go_tool="${GO:-go}"

suites=("$@")
if [ "${#suites[@]}" -eq 0 ]; then
  suites=(clients/go harness/coppice harness/sprig)
fi

fail=0
for s in "${suites[@]}"; do
  name="${s//\//-}"
  cd "$root/$s"
  set +e
  "$go_tool" test -p 1 ./... 2>&1 | tee "$log/$name.log"
  rc=${PIPESTATUS[0]}
  set -e
  if [ "$rc" -eq 0 ]; then
    printf '%s: ok\n' "$s"
  else
    printf '%s: FAIL\n' "$s"
    fail=1
  fi
done
exit "$fail"
