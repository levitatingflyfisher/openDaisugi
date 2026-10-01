#!/usr/bin/env bash
# scripts/release.sh VERSION [--rust]
# Builds the release tarball of coppice, sprig and daisugi (Go, unless
# COPPICE_PORT=rust or SPRIG_PORT=rust, below) for this box's architecture
# (Linux only):
#
#   dist/opendaisugi-VERSION-linux-<arch>.tar.gz
#     opendaisugi-VERSION-linux-<arch>/
#       coppice sprig daisugi  README  LICENSE  NOTICE-*
#   dist/SHA256SUMS
#
# COPPICE_PORT=rust puts the Rust coppice (harness/coppice-rs) in the
# tarball in place of the Go one, under the same name, coppice. The two
# speak one protocol and are checked against each other case by case
# (clients/coppice_compare.py); Go stays the default (COPPICE_PORT=go or
# unset). The Rust build needs cargo and links libgcc_s as well.
#
# SPRIG_PORT=rust likewise puts the Rust sprig (harness/sprig-rs) in the
# tarball as sprig, checked against the Go one case by case
# (clients/sprig_compare.py); Go stays the default (SPRIG_PORT=go or
# unset).
#
# With --rust it also builds the Rust daisugi and daisugi-gate (about half
# an hour, most of it Z3) into a second tarball,
# dist/opendaisugi-rust-VERSION-linux-<arch>.tar.gz. Its name is longer,
# so mise's github backend, which picks the shortest name among equal
# matches, still takes the Go tarball.
#
# PACK_BUNDLES="train vla-ref" also writes one ML pack bundle per name
# (scripts/pack-bundle.sh, with the Go daisugi just built), a separate
# artifact beside the tarballs and in SHA256SUMS:
# dist/opendaisugi-pack-NAME-VERSION-linux-<arch>.tar. It fetches the
# pinned CPython and the pack's wheels, about 350 MB for train.
#
# The binaries sit at the top of the one directory, so
# `tar -xzf ... --strip-components=1 -C ~/.local/bin` installs them, and
# mise's github backend (which strips a single top directory) finds them.
# Every binary reports VERSION from --version.
#
# Checks before it writes a tarball, and stops on any failure:
#   - each binary has its NOTICE file;
#   - no binary names a path of this box (both check-paths scripts);
#   - ldd shows nothing but libc, libm, libpthread, libdl and the loader,
#     and libgcc_s for the Rust binaries, whose unwinder needs it on glibc;
#   - each binary's --version says VERSION.
#
# The native prefix's content key goes into CGO_CFLAGS, which Go hashes
# into its cache key, so a changed native library forces a rebuild.
#
# Reproducible where it is cheap: -trimpath, an empty build id, no VCS
# stamp, and a tar with sorted names, one owner and every mtime set to
# SOURCE_DATE_EPOCH (default: the commit time of HEAD), gzip with no name
# or time. The same commit, native prefix and Go toolchain give the same
# bytes. What is not pinned: the Go toolchain (go.mod pins the version,
# not the build of it), the system gcc that compiles tree-sitter and cgo's
# glue, the Rust toolchain, and the system glibc the binaries link against.
set -euo pipefail

rust=0
[ "${2:-}" = --rust ] && rust=1
if [ "$#" -ne 1 ] && { [ "$#" -ne 2 ] || [ "$rust" -ne 1 ]; }; then
  echo "usage: release.sh VERSION [--rust]   (for example 0.44.0)" >&2
  exit 2
fi
version="${1#v}"
case "$version" in
  *[!0-9A-Za-z.+-]*|"") echo "release.sh: VERSION must look like 0.44.0" >&2; exit 2 ;;
esac
[ "$(uname -s)" = Linux ] || { echo "release.sh: this builds Linux releases only" >&2; exit 1; }
port="${COPPICE_PORT:-go}"
case "$port" in
  go|rust) ;;
  *) echo "release.sh: COPPICE_PORT must be go or rust, not $port" >&2; exit 2 ;;
esac
sprig_port="${SPRIG_PORT:-go}"
case "$sprig_port" in
  go|rust) ;;
  *) echo "release.sh: SPRIG_PORT must be go or rust, not $sprig_port" >&2; exit 2 ;;
esac
arch="$(uname -m)"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$root/scripts/preflight.sh"
native="$root/clients/go/scripts/native.sh"
prefix="$("$native" --print-prefix)"
"$native"

SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct)}"
export SOURCE_DATE_EPOCH
commit="$(git -C "$root" rev-parse HEAD)"
dirty=""
git -C "$root" diff --quiet HEAD -- || dirty=" (with uncommitted changes)"

name="opendaisugi-$version-linux-$arch"
dist="$root/dist"
stage="$dist/stage/$name"
rm -rf "$dist/stage"
mkdir -p "$stage"

export PKG_CONFIG=pkg-config
export PKG_CONFIG_PATH="$prefix/share/pkgconfig"
export CGO_CFLAGS="-O2 -g -DOPENDAISUGI_NATIVE=$("$native" --print-key)"
build() { # dir out package extra-ldflags
  (cd "$root/$1" && go build -trimpath -buildvcs=false -tags netgo \
    -ldflags="-s -w -buildid= $4" -o "$stage/$2" "$3")
}
# rust_coppice builds the Rust coppice into the stage, with this box's
# paths mapped out of the binary as the Rust daisugi's are.
rust_coppice() {
  local registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
  (cd "$root/harness/coppice-rs" &&
    COPPICE_GHOSTTY_PREFIX="$prefix" COPPICE_VERSION="$version" \
      CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}" \
      CFLAGS="${CFLAGS:+$CFLAGS }-ffile-prefix-map=$registry=/cargo/registry -ffile-prefix-map=$root=/daisugi" \
      RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }--remap-path-prefix=$registry=/cargo/registry --remap-path-prefix=$root=/daisugi" \
      cargo build --release --locked --bin coppice)
  install -m 0755 "$root/harness/coppice-rs/target/release/coppice" "$stage/coppice"
  strip "$stage/coppice"
}
# rust_sprig builds the Rust sprig into the stage, with the same path
# maps (ring's C goes through the system compiler, hence CFLAGS).
rust_sprig() {
  local registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
  (cd "$root/harness/sprig-rs" &&
    SPRIG_VERSION="$version" CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}" \
      CFLAGS="${CFLAGS:+$CFLAGS }-ffile-prefix-map=$registry=/cargo/registry -ffile-prefix-map=$root=/daisugi" \
      RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }--remap-path-prefix=$registry=/cargo/registry --remap-path-prefix=$root=/daisugi" \
      cargo build --release --locked --bin sprig)
  install -m 0755 "$root/harness/sprig-rs/target/release/sprig" "$stage/sprig"
  strip "$stage/sprig"
}
if [ "$port" = rust ]; then
  rust_coppice
else
  build harness/coppice coppice ./cmd/coppice "-X github.com/opendaisugi/coppice/internal/cli.version=$version"
fi
if [ "$sprig_port" = rust ]; then
  rust_sprig
else
  build harness/sprig sprig ./cmd/sprig "-X main.version=$version"
fi
build clients/go daisugi ./cmd/daisugi "-X daisugi-verify/internal/cli.Version=$version"

# notices STAGE BINARY:NOTICE... copies each binary's NOTICE beside it as
# NOTICE-<binary>, and LICENSE, and stops if a NOTICE is missing or empty.
notices() {
  local st="$1" pair bin src
  shift
  for pair in "$@"; do
    bin="${pair%%:*}"
    src="$root/${pair#*:}"
    [ -s "$src" ] || { echo "release.sh: no NOTICE for $bin (${pair#*:})" >&2; exit 1; }
    cp "$src" "$st/NOTICE-$bin"
  done
  cp "$root/LICENSE" "$st/LICENSE"
}

# checked EXTRA BIN... runs both check-paths scripts, the ldd check and
# the --version check. EXTRA is a regex of more libraries a binary may
# link, or empty.
checked() {
  local allow='linux-vdso|ld-linux|libc\.so|libm\.so|libpthread\.so|libdl\.so|not a dynamic executable|statically linked'
  [ -n "$1" ] && allow="$allow|$1"
  shift
  "$root/clients/go/scripts/check-paths.sh" "$@"
  "$root/clients/rust/tools/check-paths.sh" "$@"
  local b extra said
  for b in "$@"; do
    extra="$(ldd "$b" 2>&1 | grep -v -E "$allow" || true)"
    if [ -n "$extra" ]; then
      echo "release.sh: $(basename "$b") links more than it may:" >&2
      echo "$extra" >&2
      exit 1
    fi
    said="$("$b" --version 2>/dev/null | awk '{print $NF}')"
    [ "$said" = "$version" ] ||
      { echo "release.sh: $(basename "$b") --version says '$said', not $version" >&2; exit 1; }
  done
}

# pack NAME writes dist/NAME.tar.gz from dist/stage/NAME.
pack() {
  tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
    --mode='u+rw,go=rX' -C "$dist/stage" -cf - "$1" | gzip -n -9 > "$dist/$1.tar.gz"
  echo "wrote $dist/$1.tar.gz"
}

# Each Rust binary may link libgcc_s too; each Go binary libc and libm only.
coppice_notice=harness/coppice/NOTICE
sprig_notice=harness/sprig/NOTICE
coppice_line="coppice   the floor: every agent in one terminal, or in the browser"
sprig_line="sprig     the loop: a small agent harness"
rust_bins=()
go_bins=("$stage/daisugi")
if [ "$port" = rust ]; then
  coppice_notice=harness/coppice-rs/NOTICE
  coppice_line="$coppice_line (the Rust port)"
  rust_bins+=("$stage/coppice")
else
  go_bins+=("$stage/coppice")
fi
if [ "$sprig_port" = rust ]; then
  sprig_notice=harness/sprig-rs/NOTICE
  sprig_line="$sprig_line (the Rust port)"
  rust_bins+=("$stage/sprig")
else
  go_bins+=("$stage/sprig")
fi
notices "$stage" coppice:$coppice_notice sprig:$sprig_notice daisugi:clients/go/NOTICE
[ "${#rust_bins[@]}" -eq 0 ] || checked 'libgcc_s\.so' "${rust_bins[@]}"
checked "" "${go_bins[@]}"
if [ "${#rust_bins[@]}" -eq 0 ]; then
  echo "checked: coppice, sprig and daisugi link libc and libm at most, and say $version"
  links="Each binary links only the C library."
else
  if [ "${#rust_bins[@]}" -eq 1 ]; then
    rust_names="$(basename "${rust_bins[0]}")" verb=links
  else
    rust_names="$(basename "${rust_bins[0]}") and $(basename "${rust_bins[1]}")" verb=link
  fi
  echo "checked: the Rust $rust_names $verb libc, libm and libgcc_s at most; the rest libc and libm; all say $version"
  links="The Rust $rust_names $verb the C library and libgcc_s; the Go binaries
link only the C library."
fi
cat > "$stage/README" <<EOF
openDaisugi $version for Linux $arch

  $coppice_line
  $sprig_line
  daisugi   the gate: checks each agent tool call before it runs

Install: put the three binaries on PATH, for example in ~/.local/bin.
Then a floor of agents is two commands:

  daisugi install --gate    # audit: logs what it would deny
  coppice                   # or: coppice web

To enforce, register a policy first:

  daisugi gate init --workspace ~/Work
  daisugi install --gate --enforce

$links Built from commit $commit$dirty.
Licenses: LICENSE (MIT) for openDaisugi, NOTICE-* for the code each
binary links. Source: https://github.com/levitatingflyfisher/openDaisugi
EOF
pack "$name"
sums=("$name.tar.gz")

if [ "$rust" -eq 1 ]; then
  rname="opendaisugi-rust-$version-linux-$arch"
  rstage="$dist/stage/$rname"
  mkdir -p "$rstage"
  (
    cd "$root/clients/rust"
    # shellcheck disable=SC1091
    . tools/z3-build-env.sh
    DAISUGI_VERSION="$version" CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}" \
      CMAKE_BUILD_PARALLEL_LEVEL="${CMAKE_BUILD_PARALLEL_LEVEL:-2}" NUM_JOBS="${NUM_JOBS:-2}" \
      cargo build --release --locked --bin daisugi --bin daisugi-gate
  )
  for b in daisugi daisugi-gate; do
    install -m 0755 "$root/clients/rust/target/release/$b" "$rstage/$b"
    strip "$rstage/$b"
  done
  notices "$rstage" rust:clients/rust/NOTICE
  checked 'libgcc_s\.so' "$rstage/daisugi"
  "$root/clients/go/scripts/check-paths.sh" "$rstage/daisugi-gate"
  "$root/clients/rust/tools/check-paths.sh" "$rstage/daisugi-gate"
  cat > "$rstage/README" <<EOF
openDaisugi $version for Linux $arch: the Rust port of the gate

  daisugi        the gate commands and install --gate, as the Go daisugi has
  daisugi-gate   the hook entry alone

Both link libc, libm and libgcc_s, and run on any $arch CPU.
Built from commit $commit$dirty. Licenses: LICENSE (MIT) for openDaisugi,
NOTICE-rust for the code they link. The Go tarball beside this one holds
coppice and sprig.
EOF
  pack "$rname"
  sums+=("$rname.tar.gz")
fi

if [ -n "${PACK_BUNDLES:-}" ]; then
  # shellcheck disable=SC2086 # one word per pack name
  while read -r f; do sums+=("$f"); done < <("$root/scripts/pack-bundle.sh" "$stage/daisugi" "$version" "$dist" $PACK_BUNDLES)
fi

rm -rf "$dist/stage"
(cd "$dist" && sha256sum -- "${sums[@]}") > "$dist/SHA256SUMS"
cat "$dist/SHA256SUMS"
