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
# built before, and does the same for moonshine-cli and parakeet-cli (the
# voice engines).
# Then the three go builds, and the install.
#
# COPPICE_PORT=rust builds the Rust coppice (harness/coppice-rs, with
# cargo) and installs it as coppice in place of the Go one. The two speak
# one protocol and are checked against each other case by case
# (clients/coppice_compare.py). Go stays the default (COPPICE_PORT=go or
# unset).
#
# SPRIG_PORT=rust likewise builds the Rust sprig (harness/sprig-rs) and
# installs it as sprig in place of the Go one; the two are checked against
# each other case by case (clients/sprig_compare.py). Go stays the default
# (SPRIG_PORT=go or unset).
#
# DAISUGI_PORT=rust builds the Rust daisugi (clients/rust, with cargo and
# the pinned Z3 built by zig; the first build takes about half an hour)
# and installs it as daisugi in place of the Go one. The two are checked
# against the Python oracle and each other case by case (clients/*_compare.py).
# Go stays the default (DAISUGI_PORT=go or unset).
#
# After this, a floor of agents is two commands:
#   daisugi install --gate      # audit by default; --enforce to enforce
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

port="${COPPICE_PORT:-go}"
case "$port" in
  go|rust) ;;
  *) echo "install.sh: COPPICE_PORT must be go or rust, not $port" >&2; exit 2 ;;
esac
sprig_port="${SPRIG_PORT:-go}"
case "$sprig_port" in
  go|rust) ;;
  *) echo "install.sh: SPRIG_PORT must be go or rust, not $sprig_port" >&2; exit 2 ;;
esac
daisugi_port="${DAISUGI_PORT:-go}"
case "$daisugi_port" in
  go|rust) ;;
  *) echo "install.sh: DAISUGI_PORT must be go or rust, not $daisugi_port" >&2; exit 2 ;;
esac
for pair in "COPPICE_PORT:$port" "SPRIG_PORT:$sprig_port" "DAISUGI_PORT:$daisugi_port"; do
  if [ "${pair#*:}" = rust ] && ! command -v cargo >/dev/null 2>&1; then
    echo "install.sh: ${pair%%:*}=rust needs cargo on PATH (https://rustup.rs)" >&2
    exit 1
  fi
done

"$root/scripts/preflight.sh"

# A daisugi on PATH that is not a compiled binary (the Python CLI installs
# a script) is kept: the Go daisugi carries many of the Python CLI's
# commands, not all of them (clients/go/README.md lists which).
keep_daisugi=0
if [ -e "$bin/daisugi" ] && [ ! -L "$bin/daisugi" ] && [ "$replace_daisugi" -eq 0 ] &&
   [ "$(head -c 4 "$bin/daisugi" 2>/dev/null)" != "$(printf '\177ELF')" ]; then
  keep_daisugi=1
fi

native="$root/clients/go/scripts/native.sh"
prefix="$("$native" --print-prefix)"
"$native"
# moonshine-cli, the voice bridge's speech engine, in a prefix of its own.
# It finds ONNX Runtime through $ORIGIN, so it is linked onto PATH, never
# copied. A failed build (no network, an architecture with no pinned ONNX
# Runtime) costs voice only: the rest still installs.
moonshine=""
if "$native" --moonshine; then
  moonshine="$("$native" --print-moonshine)/bin/moonshine-cli"
else
  echo "install.sh: moonshine-cli did not build; voice needs it or a voice_engine set in config.yaml." >&2
fi
# parakeet-cli, the desktop speech engine, and parakeet-quantize beside it,
# in a prefix of their own. parakeet-cli is linked onto PATH, never copied:
# the voice server finds parakeet-quantize next to the file the link names.
# A failed build costs Parakeet only; voice then uses Moonshine.
parakeet=""
if "$native" --parakeet; then
  parakeet="$("$native" --print-parakeet)/bin/parakeet-cli"
else
  echo "install.sh: parakeet-cli did not build; voice uses Moonshine or the voice_engine set in config.yaml." >&2
fi

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
coppice_src="$root/harness/coppice/build/coppice"
if [ "$port" = rust ]; then
  (cd "$root/harness/coppice-rs" &&
    COPPICE_GHOSTTY_PREFIX="$prefix" COPPICE_VERSION="$version" cargo build --release --locked --bin coppice)
  coppice_src="$root/harness/coppice-rs/target/release/coppice"
else
  build harness/coppice build/coppice ./cmd/coppice github.com/opendaisugi/coppice/internal/cli.version
fi
sprig_src="$root/harness/sprig/build/sprig"
if [ "$sprig_port" = rust ]; then
  (cd "$root/harness/sprig-rs" && SPRIG_VERSION="$version" cargo build --release --locked --bin sprig)
  sprig_src="$root/harness/sprig-rs/target/release/sprig"
else
  build harness/sprig build/sprig ./cmd/sprig main.version
fi
daisugi_src="$root/clients/go/build/daisugi"
if [ "$daisugi_port" = rust ]; then
  (
    cd "$root/clients/rust"
    # shellcheck disable=SC1091
    . tools/z3-build-env.sh
    DAISUGI_VERSION="$version" cargo build --release --locked --bin daisugi
  )
  daisugi_src="$root/clients/rust/target/release/daisugi"
else
  build clients/go build/daisugi ./cmd/daisugi daisugi-verify/internal/cli.Version
fi

mkdir -p "$bin"
for pair in "coppice:$coppice_src" "sprig:$sprig_src" \
            "daisugi:$daisugi_src"; do
  name="${pair%%:*}" src="${pair#*:}"
  if [ "$name" = daisugi ] && [ "$keep_daisugi" -eq 1 ]; then
    echo "kept $bin/daisugi: it is a script, most likely the Python CLI, which also has install --gate."
    echo "  The compiled daisugi is at $src. To put it there instead, run again with --replace-daisugi."
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

if [ -n "$moonshine" ]; then
  ln -sfn "$moonshine" "$bin/moonshine-cli"
  echo "linked $bin/moonshine-cli"
fi
if [ -n "$parakeet" ]; then
  ln -sfn "$parakeet" "$bin/parakeet-cli"
  echo "linked $bin/parakeet-cli"
fi

case ":$PATH:" in
  *":$bin:"*) ;;
  *) echo "$bin is not on PATH. Add it to PATH first." >&2 ;;
esac
cat <<'NEXT'

Next:
  daisugi install --gate    # watch every agent tool call (add --enforce to deny)
  coppice                   # the floor in this terminal, or: coppice web
NEXT
