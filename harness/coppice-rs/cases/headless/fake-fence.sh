#!/bin/sh
# A stand-in for `daisugi fence`. It records the flags that matter to the
# floor (the harness and each coppice socket, one line each) in
# $FAKE_FENCE_LOG, runs $WORKSPACE/.act in the workspace if there is one (a
# fenced run that changes files), and then sleeps if $WORKSPACE/.stay is
# there, else exits.
out=${FAKE_FENCE_LOG:-/dev/null}
: > "$out"
ws=
while [ $# -gt 0 ]; do
  case "$1" in
    --harness) echo "harness $2" >> "$out"; shift ;;
    --coppice-socket) echo "socket $2" >> "$out"; shift ;;
    --workspace) ws=$2; shift ;;
    --) break ;;
  esac
  shift
done
if [ -n "$ws" ] && [ -d "$ws" ]; then
  cd "$ws" || exit 1
  [ -f .act ] && sh ./.act
  [ -f .stay ] && exec sleep 600
fi
exit 0
