#!/usr/bin/env bash
# scripts/speed-build.sh [go|rust|all]
# Builds the binaries scripts/speed.py measures, release mode, into one
# directory: $SPEED_BIN_DIR (default .speed/bin in this checkout). Names are
# <tool>-<impl>: daisugi-go, daisugi-gate-go, coppice-go, sprig-go, and the
# same with -rs. Run it under ~/opendaisugi-scratch/heavy.sh, with
# `. ~/opendaisugi-scratch/buildenv.sh` sourced first. Rust target dirs live
# under $SPEED_TARGET_ROOT (default .speed in this checkout), one per crate, so
# no other worktree shares them (coppice-rs embeds web files by absolute
# path). The first Rust build of a crate seeds its target dir from
# $SPEED_SEED (a checkout whose crates are already built) so Z3 is not built
# again. Nothing here touches ~/.local/bin or any live state.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
what="${1:-all}"
bin="${SPEED_BIN_DIR:-$root/.speed/bin}"
troot="${SPEED_TARGET_ROOT:-$root/.speed}"
seed="${SPEED_SEED:-}"
mkdir -p "$bin" "$troot"
native="$root/clients/go/scripts/native.sh"
"$native" # checks the built prefix and links it into this checkout (Z3, tree-sitter)
prefix="$("$native" --print-prefix)"
version="$(git -C "$root" describe --tags --always 2>/dev/null || echo unknown)"

seed_target() { # crate-dir target-dir
  if [ ! -d "$2" ] && [ -n "$seed" ] && [ -d "$seed/$1/target" ]; then
    cp -a "$seed/$1/target" "$2"
  fi
}

build_go() {
  export PKG_CONFIG=pkg-config
  export PKG_CONFIG_PATH="$prefix/share/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
  export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -DOPENDAISUGI_NATIVE=$("$native" --print-key)"
  local flags="-trimpath -tags netgo"
  (cd "$root/clients/go" &&
    go build $flags -ldflags="-s -w -X daisugi-verify/internal/cli.Version=$version" -o "$bin/daisugi-go" ./cmd/daisugi &&
    go build $flags -ldflags="-s -w" -o "$bin/daisugi-gate-go" ./cmd/daisugi-gate)
  (cd "$root/harness/coppice" &&
    go build $flags -ldflags="-s -w -X github.com/opendaisugi/coppice/internal/cli.version=$version" -o "$bin/coppice-go" ./cmd/coppice)
  (cd "$root/harness/sprig" &&
    go build $flags -ldflags="-s -w -X main.version=$version" -o "$bin/sprig-go" ./cmd/sprig)
}

build_rust() {
  local t
  t="$troot/sprig-rs-target"; seed_target harness/sprig-rs "$t"
  (cd "$root/harness/sprig-rs" && CARGO_TARGET_DIR="$t" SPRIG_VERSION="$version" cargo build --release --locked --bin sprig)
  cp -f "$t/release/sprig" "$bin/sprig-rs"
  t="$troot/coppice-rs-target"; seed_target harness/coppice-rs "$t"
  (cd "$root/harness/coppice-rs" && CARGO_TARGET_DIR="$t" COPPICE_GHOSTTY_PREFIX="$prefix" COPPICE_VERSION="$version" cargo build --release --locked --bin coppice)
  cp -f "$t/release/coppice" "$bin/coppice-rs"
  # The Rust daisugi builds Z3 (about half an hour). A copied target dir does
  # not help: the Z3 cmake cache names the directory it was made in. So
  # SPEED_DAISUGI_RS_DIR=<clients/rust of a checkout already built> uses its
  # release binaries as they are; without it, this builds them here.
  if [ -n "${SPEED_DAISUGI_RS_DIR:-}" ]; then
    cp -f "$SPEED_DAISUGI_RS_DIR/target/release/daisugi" "$bin/daisugi-rs"
    cp -f "$SPEED_DAISUGI_RS_DIR/target/release/daisugi-gate" "$bin/daisugi-gate-rs"
    return
  fi
  t="$troot/daisugi-rs-target"
  (
    cd "$root/clients/rust"
    # shellcheck disable=SC1091
    . tools/z3-build-env.sh
    CARGO_TARGET_DIR="$t" DAISUGI_VERSION="$version" cargo build --release --locked --bin daisugi --bin daisugi-gate
  )
  cp -f "$t/release/daisugi" "$bin/daisugi-rs"
  cp -f "$t/release/daisugi-gate" "$bin/daisugi-gate-rs"
}

case "$what" in
  go) build_go ;;
  rust) build_rust ;;
  all) build_go; build_rust ;;
  *) echo "usage: speed-build.sh [go|rust|all]" >&2; exit 2 ;;
esac
ls -l "$bin"
