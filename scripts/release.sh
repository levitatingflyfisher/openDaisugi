#!/usr/bin/env bash
# scripts/release.sh VERSION [--rust]
# Builds the release tarball of the three Go binaries for this box's
# architecture (Linux only):
#
#   dist/opendaisugi-VERSION-linux-<arch>.tar.gz
#     opendaisugi-VERSION-linux-<arch>/
#       coppice sprig daisugi  README  LICENSE  NOTICE-*
#   dist/SHA256SUMS
#
# With --rust it also builds the Rust daisugi and daisugi-gate (about half
# an hour, most of it Z3) into a second tarball,
# dist/opendaisugi-rust-VERSION-linux-<arch>.tar.gz. Its name is longer,
# so mise's github backend, which picks the shortest name among equal
# matches, still takes the Go tarball.
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
build harness/coppice coppice ./cmd/coppice "-X github.com/opendaisugi/coppice/internal/cli.version=$version"
build harness/sprig sprig ./cmd/sprig "-X main.version=$version"
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

notices "$stage" coppice:harness/coppice/NOTICE sprig:harness/sprig/NOTICE daisugi:clients/go/NOTICE
checked "" "$stage/coppice" "$stage/sprig" "$stage/daisugi"
echo "checked: coppice, sprig and daisugi link libc and libm at most, and say $version"
cat > "$stage/README" <<EOF
openDaisugi $version for Linux $arch

  coppice   the floor: every agent in one terminal, or in the browser
  sprig     the loop: a small agent harness
  daisugi   the gate: checks each agent tool call before it runs

Install: put the three binaries on PATH, for example in ~/.local/bin.
Then a floor of agents is two commands:

  daisugi install --gate    # shadow: logs what it would deny
  coppice                   # or: coppice web

To enforce, register a policy first:

  daisugi gate init --workspace ~/Work
  daisugi install --gate --enforce

Each binary links only the C library. Built from commit $commit$dirty.
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

rm -rf "$dist/stage"
(cd "$dist" && sha256sum -- "${sums[@]}") > "$dist/SHA256SUMS"
cat "$dist/SHA256SUMS"
