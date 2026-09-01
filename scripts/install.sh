#!/usr/bin/env bash
# scripts/install.sh [--link] [--replace-daisugi]
# Builds coppice (the floor), sprig (the loop) and the Go daisugi (the gate
# and its install) from this checkout, and puts all three on PATH in
# $XDG_BIN_HOME or ~/.local/bin. No sudo.
#
#   --link             link the build instead of copying it, so a rebuild
#                      is live at once
#   --replace-daisugi  replace a daisugi on PATH that is a script (the
#                      Python CLI's entry point); without it that one is
#                      kept, and coppice and sprig are still installed
#
# Steps: scripts/preflight.sh names every missing tool and stops before
# anything is built. clients/go/scripts/native.sh then builds the pinned
# native prefix (Z3, tree-sitter, libghostty-vt) once, or checks the one it
# built before. Then the three go builds, and the install.
#
# After this, a floor of agents is two commands:
#   daisugi install --gate      # shadow by default; --enforce to enforce
#   coppice                     # or: coppice web
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="${XDG_BIN_HOME:-$HOME/.local/bin}"
link=0
replace_daisugi=0
for a in "$@"; do
  case "$a" in
    --link) link=1 ;;
    --replace-daisugi) replace_daisugi=1 ;;
    -h|--help) awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; exit 0 ;;
    *) echo "install.sh: unknown option $a (see --help)" >&2; exit 2 ;;
  esac
done

"$root/scripts/preflight.sh"

# A daisugi on PATH that is not a compiled binary (the Python CLI installs
# a script) is kept: the Go daisugi carries only the gate commands.
keep_daisugi=0
if [ -e "$bin/daisugi" ] && [ ! -L "$bin/daisugi" ] && [ "$replace_daisugi" -eq 0 ] &&
   [ "$(head -c 4 "$bin/daisugi" 2>/dev/null)" != "$(printf '\177ELF')" ]; then
  keep_daisugi=1
fi

native="$root/clients/go/scripts/native.sh"
prefix="$("$native" --print-prefix)"
"$native"

# Every build reads the prefix's pkg-config file for libghostty-vt, and no
# global go env setting. The prefix's content key goes into CGO_CFLAGS,
# which Go hashes, so a changed native library forces a rebuild and relink.
export PKG_CONFIG=pkg-config
export PKG_CONFIG_PATH="$prefix/share/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -DOPENDAISUGI_NATIVE=$("$native" --print-key)"
# A source build reports the checkout it came from.
version="$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)"
build() { # dir out package version-var
  (cd "$root/$1" && go build -trimpath -tags netgo -ldflags="-s -w -X $4=$version" -o "$2" "$3")
}
mkdir -p "$root/harness/coppice/build" "$root/harness/sprig/build" "$root/clients/go/build"
build harness/coppice build/coppice ./cmd/coppice github.com/opendaisugi/coppice/internal/cli.version
build harness/sprig build/sprig ./cmd/sprig main.version
build clients/go build/daisugi ./cmd/daisugi daisugi-verify/internal/cli.Version

mkdir -p "$bin"
for pair in "coppice:$root/harness/coppice/build/coppice" "sprig:$root/harness/sprig/build/sprig" \
            "daisugi:$root/clients/go/build/daisugi"; do
  name="${pair%%:*}" src="${pair#*:}"
  if [ "$name" = daisugi ] && [ "$keep_daisugi" -eq 1 ]; then
    echo "kept $bin/daisugi: it is a script, most likely the Python CLI, which also has install --gate."
    echo "  The Go daisugi is at $src. To put it there instead, run again with --replace-daisugi."
    continue
  fi
  rm -f "$bin/$name.new"
  if [ "$link" -eq 1 ]; then
    ln -s "$src" "$bin/$name.new"
  else
    install -m 0755 "$src" "$bin/$name.new"
  fi
  mv -f "$bin/$name.new" "$bin/$name"
  echo "installed $bin/$name"
done

case ":$PATH:" in
  *":$bin:"*) ;;
  *) echo "$bin is not on PATH. Add it to PATH first." >&2 ;;
esac
cat <<'NEXT'

Next:
  daisugi install --gate    # watch every agent tool call (add --enforce to deny)
  coppice                   # the floor in this terminal, or: coppice web
NEXT
