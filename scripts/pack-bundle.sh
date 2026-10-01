#!/usr/bin/env bash
# scripts/pack-bundle.sh DAISUGI VERSION DIST NAME...
# The ML pack bundles of a release, one artifact per pack, apart from the
# binaries' tarball: for each NAME, `DAISUGI pack bundle NAME` writes
#
#   DIST/opendaisugi-pack-NAME-VERSION-linux-<arch>.tar
#
# (the pinned CPython tarball and every wheel of the pack's lock, each
# checked against its pin; `daisugi pack install NAME --offline FILE`
# installs it with no network). It prints each file's name on stdout and
# the bundle's own lines on stderr. scripts/release.sh calls it when
# PACK_BUNDLES names packs. A train bundle is about 350 MB.
set -euo pipefail

if [ "$#" -lt 4 ]; then
  echo "usage: pack-bundle.sh DAISUGI VERSION DIST NAME..." >&2
  exit 2
fi
daisugi="$1" version="$2" dist="$3"
shift 3
arch="$(uname -m)"
mkdir -p "$dist"
for name in "$@"; do
  case "$name" in
    *[!a-z0-9-]*|"") echo "pack-bundle.sh: not a pack name: $name" >&2; exit 2 ;;
  esac
  out="$dist/opendaisugi-pack-$name-$version-linux-$arch.tar"
  rm -f "$out"
  "$daisugi" pack bundle "$name" "$out" >&2
  basename "$out"
done
