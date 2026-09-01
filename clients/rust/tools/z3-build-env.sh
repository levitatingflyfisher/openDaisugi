#!/bin/sh
# Source this before `cargo build` on a box with a C compiler but no C++
# compiler (no g++), to build Z3 from the pinned z3-src source with
# `zig c++` and link it statically against zig's libc++:
#
#     clients/go/scripts/native.sh            # once: the native prefix
#     . clients/rust/tools/z3-build-env.sh
#     cargo build --release --manifest-path clients/rust/Cargo.toml
#     clients/rust/tools/check-paths.sh clients/rust/target/release/daisugi-gate
#
# ZIG names the zig binary (default: zig on PATH). Z3 and the C++ runtime
# are compiled for the baseline CPU, so the binaries run on any box of
# their architecture. The C++ runtime (libc++ and libc++abi) comes from
# the native prefix clients/go/scripts/native.sh builds: zig compiles it
# there for the baseline CPU, and the prefix's manifest checks it. The
# script copies the two archives into clients/rust/target/zig-cxx, writes
# a small compiler wrapper there, and exports CXX, CXXSTDLIB and
# DAISUGI_LIBCXX_DIR for cargo, and the path maps that keep this box's
# paths out of the binaries: RUSTFLAGS for Rust, CFLAGS for the C code
# (tree-sitter), and the wrapper's own flags for the C++ code (Z3).
# Nothing is installed outside target/.

_here=$(cd "$(dirname "${BASH_SOURCE:-$0}")/.." && pwd -P)
_checkout=$(cd "$_here/../.." && pwd -P)
_zig=${ZIG:-zig}
_out="$_here/target/zig-cxx"
_registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
_zigcache="${ZIG_GLOBAL_CACHE_DIR:-$HOME/.cache/zig}"
_native=$(PATH="$(dirname "$(command -v "$_zig")"):$PATH" "$_checkout/clients/go/scripts/native.sh" --print-prefix) || return 1 2>/dev/null || exit 1
if [ ! -f "$_native/stamp" ] || [ ! -f "$_native/lib/libc++.a" ] || [ ! -f "$_native/lib/libc++abi.a" ]; then
    echo "z3-build-env.sh: no native prefix at $_native. Run clients/go/scripts/native.sh first." >&2
    return 1 2>/dev/null || exit 1
fi
mkdir -p "$_out"
cp "$_native/lib/libc++.a" "$_native/lib/libc++abi.a" "$_out/"

_maps="-ffile-prefix-map=$_registry=/cargo/registry -ffile-prefix-map=$_checkout=/daisugi -ffile-prefix-map=$_zigcache=/zig-cache"

# The wrapper drops the clang-style --target=<rust triple> that cc and
# cmake pass, which zig does not parse (zig builds for the host by
# default), and adds the baseline CPU and the path maps.
cat > "$_out/zig-cxx" <<WRAP
#!/bin/sh
for a; do shift; case "\$a" in --target=*) ;; *) set -- "\$@" "\$a" ;; esac; done
exec $_zig c++ -mcpu=baseline $_maps "\$@"
WRAP
chmod +x "$_out/zig-cxx"

export CXX="$_out/zig-cxx"
export CXXSTDLIB=
export DAISUGI_LIBCXX_DIR="$_out"
export CFLAGS="${CFLAGS:+$CFLAGS }$_maps"
export RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }--remap-path-prefix=$_registry=/cargo/registry --remap-path-prefix=$_checkout=/daisugi"
