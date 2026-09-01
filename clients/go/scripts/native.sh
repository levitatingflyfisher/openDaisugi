#!/usr/bin/env bash
# clients/go/scripts/native.sh
# Builds the C and C++ libraries openDaisugi's Go binaries link statically:
# Z3, the tree-sitter runtime and the tree-sitter-bash grammar for the gate,
# and libghostty-vt for coppice. No sudo, no /tmp.
#
#   clients/go/scripts/native.sh                 # build (once) and link this checkout
#   clients/go/scripts/native.sh --print-prefix  # print where the build goes, and stop
#   clients/go/scripts/native.sh --print-key     # print the built prefix's content key
#
# The content key is the first 16 hex digits of the SHA-256 of the
# prefix's manifest. The build scripts put it in CGO_CFLAGS, which Go
# hashes into its build cache key, so a prefix with other libraries forces
# a rebuild and relink. Go does not hash a static library a cgo LDFLAGS
# line names, so without the key it links the old one from its cache.
#
# Everything is fetched at a pinned version and checked against a pinned
# digest (the SHA-256 the publishers list: the Z3 GitHub release and PyPI;
# the git commit for ghostty, whose zig build checks its own dependencies'
# hashes). The versions are the ones the Python oracle runs: z3-solver
# 5.1.0.0, tree-sitter 0.26.0 and tree-sitter-bash 0.25.1. The tree-sitter
# sources come from the same PyPI source archives the oracle's wheels are
# built from. libghostty-vt is built at the commit go-libghostty fetches.
#
# The box needs gcc, make, cmake, git, curl, python3 (Z3's build runs it)
# and zig 0.16.0 exactly (harness/coppice/scripts/toolchain.sh installs it
# and checks its release tarball against the published SHA-256). There is
# no need for g++: Z3 is compiled with `zig c++`, and zig's own libc++,
# libc++abi and libunwind are copied beside libz3.a, so the Go link needs
# no libstdc++. Those three archives have no published digest to check
# them against: zig compiles them on this box from the sources in its own
# tarball, so the zig version check is what pins them. Their digests go in
# the prefix's manifest, as every other file's do.
#
# Z3, zig's C++ runtime and libghostty-vt are compiled for the baseline
# CPU, so the binaries run on any box of their architecture. Every C and
# C++ file of Z3 and tree-sitter is compiled with the build directory
# mapped to /daisugi-build in __FILE__ and in debug info. libghostty-vt is
# a ReleaseFast build; its debug info names build paths, and the Go link
# (-s -w) drops it. So no binary carries this box's paths.
#
# Output: one prefix per set of inputs,
# $XDG_CACHE_HOME/opendaisugi/native/<stamp> (~/.cache when
# XDG_CACHE_HOME is unset), where <stamp> is a hash of the pinned versions
# and digests, the flags, the C compiler, zig and the CPU architecture.
# DAISUGI_NATIVE_PREFIX names another prefix. The prefix holds include/,
# lib/ and share/pkgconfig/, the stamp file that says what was built and
# how, and manifest.sha256, the digest of every file in those three
# directories. A prefix that holds a build is used again only when its
# stamp is this script's and every file matches the manifest; otherwise the
# script stops and says how to rebuild. The script then points
# clients/go/.native at the prefix (a git-ignored symlink), which is where
# the gate's cgo flags look; coppice finds libghostty-vt through
# share/pkgconfig (scripts/install.sh sets PKG_CONFIG_PATH).
set -euo pipefail

Z3_VERSION="5.1.0"
Z3_SDIST="z3_solver-5.1.0.0.tar.gz"
Z3_URL="https://github.com/Z3Prover/z3/releases/download/z3-${Z3_VERSION}/${Z3_SDIST}"
Z3_SHA256="269a0bf62949d227a16ab42afee6750f18477e173a14de6aeaf8789f33f813b5"

TS_SDIST="tree_sitter-0.26.0.tar.gz"
TS_URL="https://files.pythonhosted.org/packages/f7/03/5600b84aff2e6c4fe80cfebb4063fe2f50299521befe5f6092ab8c082f4a/${TS_SDIST}"
TS_SHA256="b40c219edccc4564530c96f8f1556f6202b37cda964d1cbd7bd2b7e68b40a245"

TSB_SDIST="tree_sitter_bash-0.25.1.tar.gz"
TSB_URL="https://files.pythonhosted.org/packages/8e/0e/f0108be910f1eef6499eabce517e79fe3b12057280ed398da67ce2426cba/${TSB_SDIST}"
TSB_SHA256="bfc0bdaa77bc1e86e3c6652e5a6e140c40c0a16b84185c2b63ad7cd809b88f14"

GHOSTTY_REPO="https://github.com/ghostty-org/ghostty.git"
GHOSTTY_COMMIT="b0c421fcd2e290629d4285c181b52fe2f2095f06"

ZIG_VERSION="0.16.0"
# Change this when a compiler flag below changes, so an old prefix is not
# used again.
FLAGS_REV="3"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gomod="$(dirname "$here")"
cache="${XDG_CACHE_HOME:-$HOME/.cache}/opendaisugi"
work="${DAISUGI_NATIVE_BUILD_DIR:-$cache/native-build}"   # real disk; /tmp may be RAM
jobs="${DAISUGI_NATIVE_JOBS:-2}"
cc="${CC:-gcc}"

print_prefix=0
print_key=0
case "${1:-}" in
  --print-prefix) print_prefix=1 ;;
  --print-key) print_key=1 ;;
  "") ;;
  *) echo "usage: native.sh [--print-prefix | --print-key]" >&2; exit 2 ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c - >/dev/null; }
  sha256_of() { sha256sum "$@"; }
else
  sha256_check() { shasum -a 256 -c - >/dev/null; }
  sha256_of() { shasum -a 256 "$@"; }
fi

fetch() { # url file sha256
  if [ ! -f "$work/$2" ]; then
    echo "fetching $2"
    curl -fsSL "$1" -o "$work/$2.part"
    mv "$work/$2.part" "$work/$2"
  fi
  echo "$3  $work/$2" | sha256_check || { echo "native.sh: $2 does not match its pinned sha256" >&2; exit 1; }
}

command -v zig >/dev/null 2>&1 || { echo "native.sh: zig $ZIG_VERSION is not on PATH; run harness/coppice/scripts/toolchain.sh" >&2; exit 1; }
[ "$(zig version)" = "$ZIG_VERSION" ] || { echo "native.sh: zig is $(zig version), not $ZIG_VERSION" >&2; exit 1; }
command -v "$cc" >/dev/null 2>&1 || { echo "native.sh: $cc is not on PATH (Arch: pacman -S gcc)" >&2; exit 1; }

stamp="z3 $Z3_SHA256
tree-sitter $TS_SHA256
tree-sitter-bash $TSB_SHA256
ghostty $GHOSTTY_COMMIT
zig $ZIG_VERSION
cc $("$cc" --version | head -n1)
arch $(uname -m)
flags $FLAGS_REV"
stamp_id="$(printf '%s\n' "$stamp" | sha256_of | cut -c1-16)"
prefix="${DAISUGI_NATIVE_PREFIX:-$cache/native/$stamp_id}"
if [ "$print_prefix" -eq 1 ]; then
  echo "$prefix"
  exit 0
fi
if [ "$print_key" -eq 1 ]; then
  [ -f "$prefix/manifest.sha256" ] || { echo "native.sh: no build at $prefix yet; run native.sh first" >&2; exit 1; }
  sha256_of "$prefix/manifest.sha256" | cut -c1-16
  exit 0
fi
rebuild="rm -rf '$prefix' && $0"

# 0. A prefix that already holds a build is checked before it is used.
if [ -e "$prefix/stamp" ] || [ -e "$prefix/lib" ] || [ -e "$prefix/include" ]; then
  if [ "$(cat "$prefix/stamp" 2>/dev/null)" != "$stamp" ]; then
    echo "native.sh: $prefix holds a build this script did not make, or made from other sources, flags or tools. To rebuild: $rebuild" >&2
    exit 1
  fi
  if ! (cd "$prefix" && sha256_check < manifest.sha256); then
    echo "native.sh: a file in $prefix does not match manifest.sha256. To rebuild: $rebuild" >&2
    exit 1
  fi
  listed="$(cut -c67- "$prefix/manifest.sha256" | LC_ALL=C sort)"
  present="$(cd "$prefix" && find include lib share -type f | LC_ALL=C sort)"
  if [ "$listed" != "$present" ]; then
    echo "native.sh: $prefix holds files manifest.sha256 does not list. To rebuild: $rebuild" >&2
    exit 1
  fi
  ln -sfn "$prefix" "$gomod/.native"
  echo "checked: $prefix (linked from clients/go/.native)"
  exit 0
fi
command -v cmake >/dev/null 2>&1 || { echo "native.sh: cmake is not on PATH (Arch: pacman -S cmake)" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "native.sh: git is not on PATH (Arch: pacman -S git)" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "native.sh: curl is not on PATH (Arch: pacman -S curl)" >&2; exit 1; }
# The build goes into a staging directory beside the prefix, which is
# renamed into place only when every step is done, so a failed build
# leaves no half prefix behind. A rerun reuses the build trees in $work.
final="$prefix"
prefix="$final.part"
rm -rf "$prefix"
mkdir -p "$work/bin" "$prefix/include" "$prefix/lib" "$prefix/share/pkgconfig"

mapflags="-ffile-prefix-map=$work=/daisugi-build -fmacro-prefix-map=$work=/daisugi-build -fdebug-prefix-map=$work=/daisugi-build"

# zig c++ as a plain compiler command, since cmake wants one word.
cat > "$work/bin/zig-c++" <<'WRAP'
#!/usr/bin/env bash
exec zig c++ -mcpu=baseline "$@"
WRAP
cat > "$work/bin/zig-cc" <<'WRAP'
#!/usr/bin/env bash
exec zig cc -mcpu=baseline "$@"
WRAP
chmod +x "$work/bin/zig-c++" "$work/bin/zig-cc"

# 1. Z3, static, Release, no executables.
fetch "$Z3_URL" "$Z3_SDIST" "$Z3_SHA256"
# tar keeps the archive's mtimes, so a kept z3-build is not rebuilt.
tar -xzf "$work/$Z3_SDIST" -C "$work"
cmake -S "$work/z3_solver-5.1.0.0/core" -B "$work/z3-build" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_C_COMPILER="$work/bin/zig-cc" \
  -DCMAKE_CXX_COMPILER="$work/bin/zig-c++" \
  -DCMAKE_CXX_FLAGS="-fno-sanitize=undefined $mapflags" \
  -DCMAKE_C_FLAGS="-fno-sanitize=undefined $mapflags" \
  -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
  -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DZ3_BUILD_LIBZ3_SHARED=OFF \
  -DZ3_BUILD_EXECUTABLE=OFF \
  -DZ3_BUILD_TEST_EXECUTABLES=OFF \
  -DZ3_ENABLE_EXAMPLE_TARGETS=OFF \
  -DZ3_INCLUDE_GIT_HASH=OFF \
  -DZ3_INCLUDE_GIT_DESCRIBE=OFF \
  -DZ3_USE_LIB_GMP=OFF
cmake --build "$work/z3-build" -j "$jobs"
cmake --install "$work/z3-build" --prefix "$prefix"
if [ -f "$prefix/lib64/libz3.a" ]; then
  cp "$prefix/lib64/libz3.a" "$prefix/lib/libz3.a"
fi
# zig c++ writes debug info even in Release; the gate does not need it.
strip -g "$prefix/lib/libz3.a"
# The cmake and pkg-config files name the prefix, and nothing links them.
rm -rf "$prefix/lib64" "$prefix/lib/cmake" "$prefix/lib/pkgconfig"

# 2. zig's C++ runtime, built at -O2 like the Z3 objects, copied beside libz3.a.
probe="$work/cxxrt-probe"
mkdir -p "$probe"
printf '#include <string>\nint main(){return (int)std::to_string(1).size()-1;}\n' > "$probe/p.cpp"
links="$(ZIG_VERBOSE_LINK=1 zig c++ -mcpu=baseline -O2 -fno-sanitize=undefined "$probe/p.cpp" -o "$probe/p" 2>&1 | tr ' ' '\n')"
for lib in libc++.a libc++abi.a libunwind.a; do
  path="$(printf '%s\n' "$links" | grep "/$lib\$" | head -n1)"
  [ -n "$path" ] || { echo "native.sh: zig did not link $lib" >&2; exit 1; }
  cp "$path" "$prefix/lib/$lib"
done

# 3. The tree-sitter runtime and the bash grammar, from the oracle's own sources.
fetch "$TS_URL" "$TS_SDIST" "$TS_SHA256"
fetch "$TSB_URL" "$TSB_SDIST" "$TSB_SHA256"
rm -rf "$work/tree_sitter-0.26.0" "$work/tree_sitter_bash-0.25.1" "$work/ts-obj"
tar -xzf "$work/$TS_SDIST" -C "$work"
tar -xzf "$work/$TSB_SDIST" -C "$work"
core="$work/tree_sitter-0.26.0/tree_sitter/core/lib"
bash_src="$work/tree_sitter_bash-0.25.1/src"
mkdir -p "$work/ts-obj" "$prefix/include/tree_sitter"
# shellcheck disable=SC2086 # mapflags is three words
"$cc" -O2 -fPIC -std=c11 $mapflags -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200112L \
  -I"$core/include" -I"$core/src" -c "$core/src/lib.c" -o "$work/ts-obj/lib.o"
# shellcheck disable=SC2086
"$cc" -O2 -fPIC -std=c11 $mapflags -I"$bash_src" -c "$bash_src/parser.c" -o "$work/ts-obj/parser.o"
# shellcheck disable=SC2086
"$cc" -O2 -fPIC -std=c11 $mapflags -I"$bash_src" -c "$bash_src/scanner.c" -o "$work/ts-obj/scanner.o"
ar rcs "$prefix/lib/libtree-sitter.a" "$work/ts-obj/lib.o"
ar rcs "$prefix/lib/libtree-sitter-bash.a" "$work/ts-obj/parser.o" "$work/ts-obj/scanner.o"
cp "$core/include/tree_sitter/api.h" "$prefix/include/tree_sitter/api.h"

# 4. libghostty-vt for coppice: ReleaseFast (a Debug build carries its
#    sanitizer's source paths), baseline CPU, static. The .pc file finds
#    the prefix from its own place, so it names no path.
if [ ! -d "$work/ghostty/.git" ]; then
  git clone --quiet --filter=blob:none --no-checkout "$GHOSTTY_REPO" "$work/ghostty"
fi
# A clone that already holds the commit (a package build fetches it ahead)
# needs no network.
git -C "$work/ghostty" cat-file -e "$GHOSTTY_COMMIT^{commit}" 2>/dev/null ||
  git -C "$work/ghostty" fetch --quiet --depth 1 origin "$GHOSTTY_COMMIT"
git -C "$work/ghostty" checkout --quiet --detach "$GHOSTTY_COMMIT"
[ "$(git -C "$work/ghostty" rev-parse HEAD)" = "$GHOSTTY_COMMIT" ] || { echo "native.sh: ghostty is not at $GHOSTTY_COMMIT" >&2; exit 1; }
rm -rf "$work/ghostty-out"
(cd "$work/ghostty" && zig build -j"$jobs" -Demit-lib-vt -Doptimize=ReleaseFast -Dcpu=baseline --prefix "$work/ghostty-out")
# Its debug info still names the build and zig cache paths; the Go link
# with -s -w drops it, and check-paths.sh checks that it did.
cp "$work/ghostty-out/lib/libghostty-vt.a" "$prefix/lib/libghostty-vt.a"
cp -R "$work/ghostty-out/include/ghostty" "$prefix/include/ghostty"
cat > "$prefix/share/pkgconfig/libghostty-vt-static.pc" <<'PC'
prefix=${pcfiledir}/../..
includedir=${prefix}/include
libdir=${prefix}/lib

Name: libghostty-vt-static
URL: https://github.com/ghostty-org/ghostty
Description: Ghostty VT library (static)
Version: 0.1.0-dev
Cflags: -I${includedir}
Libs: ${libdir}/libghostty-vt.a
PC

# 5. Record what was built, then point this checkout at the prefix. The
#    build trees are removed; the fetched archives stay for a rebuild.
(cd "$prefix" && find include lib share -type f | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done) > "$prefix/manifest.sha256"
printf '%s\n' "$stamp" > "$prefix/stamp"
mv "$prefix" "$final"
prefix="$final"
ln -sfn "$prefix" "$gomod/.native"
rm -rf "$work/z3_solver-5.1.0.0" "$work/z3-build" "$work/tree_sitter-0.26.0" "$work/tree_sitter_bash-0.25.1" \
  "$work/ts-obj" "$work/cxxrt-probe" "$work/ghostty" "$work/ghostty-out"
echo "done: $prefix (linked from clients/go/.native)"
