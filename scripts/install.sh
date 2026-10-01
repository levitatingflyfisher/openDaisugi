#!/usr/bin/env bash
# scripts/install.sh [--link] [--replace-daisugi] [--voice]
# Builds coppice (the floor), sprig (the loop) and the Go daisugi (the gate
# and its install) from this checkout, and puts all three on PATH in
# $XDG_BIN_HOME or ~/.local/bin. No sudo.
#
#   --link             link the build instead of copying it, so a rebuild
#                      is live at once
#   --replace-daisugi  replace a daisugi on PATH that is a script (the
#                      Python CLI's entry point); without it that one is
#                      kept, and coppice and sprig are still installed
#   --voice            also build the voice engines, moonshine-cli and
#                      parakeet-cli (a long build; most people skip it)
#
# Steps: scripts/preflight.sh --fetchable names every missing tool that
# only your distribution can supply (git, a C compiler, pkg-config, go) and
# stops before anything is built. This script fetches the rest itself, with
# no sudo: zig 0.16.0 and cmake from harness/coppice/scripts/toolchain.sh
# (pinned, checksummed, under ~/.local/share/opendaisugi), and a go 1.26
# toolchain through GOTOOLCHAIN=auto when your go is older. Then
# clients/go/scripts/native.sh builds the pinned native prefix (Z3,
# tree-sitter, libghostty-vt) once, or checks the one it built before.
# With --voice it does the same for moonshine-cli and parakeet-cli.
# Then the three go builds, and the install.
#
# Compiler output goes to a log file (INSTALL_LOG, default
# ~/.local/state/opendaisugi/install.log). A failed stage prints the last
# 30 lines of it and the path.
#
# INSTALL_DRY=1 checks the tools, prints the plan and builds nothing.
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
# the pinned Z3 built by zig; the first build is slow, see FIRST_BUILD_HINT)
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
voice=0
# The whole first build, as a person sees it. This is the one place for the
# figure in this script: docs/start-here.md must say the same. It is a
# placeholder; measure it again on a clean box (the 37 min on record
# included the voice builds).
FIRST_BUILD_HINT="about 45 min on 4 cores"
ZIG_VERSION="0.16.0"
GO_MIN_MINOR=26
for a in "$@"; do
  case "$a" in
    --link) link=1 ;;
    --replace-daisugi) replace_daisugi=1 ;;
    --voice) voice=1 ;;
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

"$root/scripts/preflight.sh" --fetchable

# What this script fetches itself, so a stock box needs only the
# distribution packages preflight names.
data="${XDG_DATA_HOME:-$HOME/.local/share}/opendaisugi"
want_tools=0
if ! command -v zig >/dev/null 2>&1 || [ "$(zig version 2>/dev/null)" != "$ZIG_VERSION" ] ||
   ! command -v cmake >/dev/null 2>&1; then
  want_tools=1
fi
want_auto=0
go_minor="$(go env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')"
if [ -n "$go_minor" ] && [ "$go_minor" -lt "$GO_MIN_MINOR" ]; then
  want_auto=1
fi
log="${INSTALL_LOG:-${XDG_STATE_HOME:-$HOME/.local/state}/opendaisugi/install.log}"
yesno() { if [ "$1" -eq 1 ]; then echo yes; else echo no; fi; }
if [ "${INSTALL_DRY:-}" = 1 ]; then
  echo "plan: fetch zig $ZIG_VERSION and cmake into $data: $(yesno "$want_tools")"
  echo "plan: go toolchain 1.$GO_MIN_MINOR through GOTOOLCHAIN=auto: $(yesno "$want_auto")"
  echo "plan: voice engines (moonshine, parakeet): $(yesno "$voice")"
  echo "plan: ports: coppice=$port sprig=$sprig_port daisugi=$daisugi_port"
  echo "plan: install into $bin, build log $log"
  exit 0
fi

mkdir -p "$(dirname "$log")"
: > "$log"
echo "Build output goes to $log"
# stage LABEL CMD...: print the label, run the command with its output in
# the log, and on failure show the log's last 30 lines.
stage() {
  local label="$1"; shift
  echo "==> $label"
  if ! "$@" >>"$log" 2>&1; then
    echo "install.sh: failed: $label" >&2
    tail -n 30 "$log" >&2
    echo "install.sh: the full log is $log" >&2
    return 1
  fi
}

if [ "$want_tools" -eq 1 ]; then
  tc="$root/harness/coppice/scripts/toolchain.sh"
  stage "Fetching zig $ZIG_VERSION and cmake (pinned, checksummed, no sudo)" "$tc" --tools-only || exit 1
  PATH="$("$tc" --print-path):$PATH"
  export PATH
fi
if [ "$want_auto" -eq 1 ]; then
  export GOTOOLCHAIN=auto
fi
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
stage "Building Z3, tree-sitter and libghostty-vt: the slowest step (the whole first build takes $FIRST_BUILD_HINT), skipped when built before" "$native" || exit 1
moonshine=""
parakeet=""
if [ "$voice" -eq 1 ]; then
  # moonshine-cli, the voice bridge's speech engine, in a prefix of its own.
  # It finds ONNX Runtime through $ORIGIN, so it is linked onto PATH, never
  # copied. A failed build (no network, an architecture with no pinned ONNX
  # Runtime) costs voice only: the rest still installs.
  if stage "Building moonshine-cli (voice)" "$native" --moonshine; then
    moonshine="$("$native" --print-moonshine)/bin/moonshine-cli"
  else
    echo "install.sh: moonshine-cli did not build; voice needs it or a voice_engine set in config.yaml." >&2
  fi
  # parakeet-cli, the desktop speech engine, and parakeet-quantize beside
  # it, in a prefix of their own. parakeet-cli is linked onto PATH, never
  # copied: the voice server finds parakeet-quantize next to the file the
  # link names.
  if stage "Building parakeet-cli (voice)" "$native" --parakeet; then
    parakeet="$("$native" --print-parakeet)/bin/parakeet-cli"
  else
    echo "install.sh: parakeet-cli did not build; voice uses Moonshine or the voice_engine set in config.yaml." >&2
  fi
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
  stage "Building $2 (go)" bash -c \
    'cd "$1" && go build -trimpath -tags netgo -ldflags="-s -w -X $4=$5" -o "$2" "$3"' \
    _ "$root/$1" "$2" "$3" "$4" "$version" || exit 1
}
mkdir -p "$root/harness/coppice/build" "$root/harness/sprig/build" "$root/clients/go/build"
coppice_src="$root/harness/coppice/build/coppice"
if [ "$port" = rust ]; then
  rust_coppice() { (cd "$root/harness/coppice-rs" &&
    COPPICE_GHOSTTY_PREFIX="$prefix" COPPICE_VERSION="$version" cargo build --release --locked --bin coppice); }
  stage "Building coppice (rust)" rust_coppice || exit 1
  coppice_src="$root/harness/coppice-rs/target/release/coppice"
else
  build harness/coppice build/coppice ./cmd/coppice github.com/opendaisugi/coppice/internal/cli.version
fi
sprig_src="$root/harness/sprig/build/sprig"
if [ "$sprig_port" = rust ]; then
  rust_sprig() { (cd "$root/harness/sprig-rs" && SPRIG_VERSION="$version" cargo build --release --locked --bin sprig); }
  stage "Building sprig (rust)" rust_sprig || exit 1
  sprig_src="$root/harness/sprig-rs/target/release/sprig"
else
  build harness/sprig build/sprig ./cmd/sprig main.version
fi
daisugi_src="$root/clients/go/build/daisugi"
if [ "$daisugi_port" = rust ]; then
  rust_daisugi() (
    cd "$root/clients/rust"
    # shellcheck disable=SC1091
    . tools/z3-build-env.sh
    DAISUGI_VERSION="$version" cargo build --release --locked --bin daisugi
  )
  stage "Building daisugi (rust, the first build takes $FIRST_BUILD_HINT)" rust_daisugi || exit 1
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
if [ "$voice" -eq 0 ] && { [ ! -e "$bin/moonshine-cli" ] || [ ! -e "$bin/parakeet-cli" ]; }; then
  echo "The voice engines were not built. To add them: scripts/install.sh --voice"
fi
cat <<'NEXT'

Next:
  daisugi install --gate    # watch every agent tool call (add --enforce to deny)
  coppice                   # the floor in this terminal, or: coppice web
NEXT
