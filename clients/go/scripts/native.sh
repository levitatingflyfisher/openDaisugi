#!/usr/bin/env bash
# clients/go/scripts/native.sh
# Builds the C and C++ libraries openDaisugi's Go binaries link statically:
# Z3, the tree-sitter runtime and the tree-sitter-bash grammar for the gate,
# and libghostty-vt for coppice. No sudo, no /tmp.
#
#   clients/go/scripts/native.sh                 # build (once) and link this checkout
#   clients/go/scripts/native.sh --print-prefix  # print where the build goes, and stop
#   clients/go/scripts/native.sh --print-key     # print the built prefix's content key
#   clients/go/scripts/native.sh --mujoco        # install the pinned MuJoCo (robotics)
#   clients/go/scripts/native.sh --onnxruntime   # install the pinned ONNX Runtime (VLA)
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

# Moonshine, the speech engine of the voice bridge, at the v0.1.5 tag, and
# ONNX Runtime, Microsoft's prebuilt CPU release for this architecture
# (the SHA-256 GitHub publishes for the release asset). Moonshine vendors
# that same release; native.sh copies Microsoft's own files over it.
MOONSHINE_REPO="https://github.com/moonshine-ai/moonshine.git"
MOONSHINE_COMMIT="234f60faa0eb388b01cdf7e60aca232af37aefda"
ORT_VERSION="1.23.2"
case "$(uname -m)" in
  x86_64) ORT_ARCH=x64 ORT_DIR=x86_64 ORT_SHA256="1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b" ;;
  aarch64) ORT_ARCH=aarch64 ORT_DIR=aarch64 ORT_SHA256="7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c" ;;
  *) ORT_ARCH="" ORT_DIR="" ORT_SHA256="" ;;
esac
ORT_TGZ="onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}.tgz"
ORT_URL="https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/${ORT_TGZ}"
# Change this when a moonshine-cli build flag changes.
MOONSHINE_FLAGS_REV="1"

# Parakeet, the desktop speech engine (ruling VO-16): CrispASR's Parakeet
# code at a pinned commit, with only the files of
# clients/native/parakeet-cli/crispasr-files.sha256 checked out and each
# checked against its SHA-256, and the ggml fork CrispASR builds on, at the
# commit its submodule pins, with only the CPU backend checked out and the
# whole tree checked against one SHA-256 (of the sorted sha256sum listing).
CRISPASR_REPO="https://github.com/CrispStrobe/CrispASR.git"
CRISPASR_COMMIT="7e2b030780df5c9ad7ede6ac1935bc783b037d18"
GGML_REPO="https://github.com/CrispStrobe/ggml.git"
GGML_COMMIT="2f5a80d258c46e6ac8eee95f1328c0f58376d7ee"
GGML_TREE_SHA256="aa6369fd8fa3d6bc6d025acc2c87dd7a3bef529f2db1ca34ffad4a9d9bf4783e"
# Change this when a parakeet-cli build flag changes.
PARAKEET_FLAGS_REV="1"

# MuJoCo, the official prebuilt release (the SHA-256 GitHub publishes
# for the release asset). The version is the mujoco wheel's in uv.lock.
MUJOCO_VERSION="3.12.0"
case "$(uname -m)" in
  x86_64) MUJOCO_ARCH=x86_64 MUJOCO_SHA256="a9367911e6d5eaeade17c2197304687421c1fc932cdf7bcd4cb8cfaf0374dcb2" ;;
  aarch64) MUJOCO_ARCH=aarch64 MUJOCO_SHA256="08fd5627a2ef7d5a42580c40e014ab2c1a644f082010c584ca361a3ed8cad838" ;;
  *) MUJOCO_ARCH="" MUJOCO_SHA256="" ;;
esac
MUJOCO_TGZ="mujoco-${MUJOCO_VERSION}-linux-${MUJOCO_ARCH}.tar.gz"
MUJOCO_URL="https://github.com/google-deepmind/mujoco/releases/download/${MUJOCO_VERSION}/${MUJOCO_TGZ}"

print_prefix=0
print_key=0
moonshine=0
parakeet=0
mujoco=0
onnxruntime=0
case "${1:-}" in
  --print-prefix) print_prefix=1 ;;
  --print-key) print_key=1 ;;
  --moonshine) moonshine=1 ;;
  --print-moonshine) moonshine=2 ;;
  --parakeet) parakeet=1 ;;
  --print-parakeet) parakeet=2 ;;
  --mujoco) mujoco=1 ;;
  --print-mujoco) mujoco=2 ;;
  --onnxruntime) onnxruntime=1 ;;
  --print-onnxruntime) onnxruntime=2 ;;
  "") ;;
  *) echo "usage: native.sh [--print-prefix | --print-key | --moonshine | --print-moonshine | --parakeet | --print-parakeet | --mujoco | --print-mujoco | --onnxruntime | --print-onnxruntime]" >&2; exit 2 ;;
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

# --mujoco installs MuJoCo, the physics library of the robotics executors,
# into a prefix of its own, $cache/native/mujoco-<stamp>: include/mujoco,
# lib/libmujoco.so.<version> (and the libmujoco.so link), the license and
# third-party notices under share/, the stamp and manifest.sha256. Nothing
# is compiled: it is the official prebuilt release for this
# architecture, checked against the SHA-256 GitHub publishes for the
# release asset. The version is the one the Python oracle's mujoco wheel
# pins in uv.lock, and the wheel carries the same libmujoco.so, byte for
# byte. The script then points clients/go/.mujoco at the prefix (a
# git-ignored link), which is where the robotics cgo flags and the Rust
# build look. --print-mujoco prints where it goes. Only curl and tar are
# needed.
if [ "$mujoco" -ne 0 ]; then
  [ -n "$MUJOCO_SHA256" ] || { echo "native.sh: no pinned MuJoCo release for $(uname -m)" >&2; exit 1; }
  jstamp="mujoco $MUJOCO_VERSION $MUJOCO_SHA256
arch $(uname -m)"
  jprefix="${DAISUGI_MUJOCO_PREFIX:-$cache/native/mujoco-$(printf '%s\n' "$jstamp" | sha256_of | cut -c1-16)}"
  if [ "$mujoco" -eq 2 ]; then
    echo "$jprefix"
    exit 0
  fi
  jrebuild="rm -rf '$jprefix' && $0 --mujoco"
  if [ -e "$jprefix/stamp" ] || [ -e "$jprefix/lib" ]; then
    [ "$(cat "$jprefix/stamp" 2>/dev/null)" = "$jstamp" ] ||
      { echo "native.sh: $jprefix holds another MuJoCo. To install again: $jrebuild" >&2; exit 1; }
    (cd "$jprefix" && sha256_check < manifest.sha256) ||
      { echo "native.sh: a file in $jprefix does not match manifest.sha256. To install again: $jrebuild" >&2; exit 1; }
  else
    for tool in curl tar; do
      command -v "$tool" >/dev/null 2>&1 || { echo "native.sh: $tool is not on PATH" >&2; exit 1; }
    done
    mkdir -p "$work"
    fetch "$MUJOCO_URL" "$MUJOCO_TGZ" "$MUJOCO_SHA256"
    part="$jprefix.part"
    rm -rf "$part"
    mkdir -p "$part/share/licenses/mujoco"
    tar -xzf "$work/$MUJOCO_TGZ" -C "$part" --strip-components=1 \
      "mujoco-$MUJOCO_VERSION/include" "mujoco-$MUJOCO_VERSION/lib" \
      "mujoco-$MUJOCO_VERSION/LICENSE" "mujoco-$MUJOCO_VERSION/THIRD_PARTY_NOTICES"
    mv "$part/LICENSE" "$part/THIRD_PARTY_NOTICES" "$part/share/licenses/mujoco/"
    [ -f "$part/lib/libmujoco.so.$MUJOCO_VERSION" ] ||
      { echo "native.sh: the release holds no lib/libmujoco.so.$MUJOCO_VERSION" >&2; exit 1; }
    (cd "$part" && find include lib share -type f | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done) > "$part/manifest.sha256"
    printf '%s\n' "$jstamp" > "$part/stamp"
    mv "$part" "$jprefix"
    rm -f "$work/$MUJOCO_TGZ"
  fi
  ln -sfn "$jprefix" "$gomod/.mujoco"
  echo "mujoco: $jprefix"
  exit 0
fi

# --onnxruntime installs ONNX Runtime, the inference library of the VLA
# executors, into a prefix of its own, $cache/native/onnxruntime-<stamp>:
# include/ (the C API headers), lib/libonnxruntime.so.<version> (and the
# libonnxruntime.so and .so.1 links), the license and third-party notices
# under share/, the stamp and manifest.sha256. Nothing is compiled: it is
# Microsoft's prebuilt CPU release, the same pinned asset the voice bridge
# uses. The script then points clients/go/.onnxruntime at the prefix (a
# git-ignored link), where the VLA cgo flags and the Rust build look.
# --print-onnxruntime prints where it goes. Only curl and tar are needed.
if [ "$onnxruntime" -ne 0 ]; then
  [ -n "$ORT_SHA256" ] || { echo "native.sh: no pinned ONNX Runtime release for $(uname -m)" >&2; exit 1; }
  ostamp="onnxruntime $ORT_VERSION $ORT_SHA256
arch $(uname -m)"
  oprefix="${DAISUGI_ONNXRUNTIME_PREFIX:-$cache/native/onnxruntime-$(printf '%s\n' "$ostamp" | sha256_of | cut -c1-16)}"
  if [ "$onnxruntime" -eq 2 ]; then
    echo "$oprefix"
    exit 0
  fi
  orebuild="rm -rf '$oprefix' && $0 --onnxruntime"
  if [ -e "$oprefix/stamp" ] || [ -e "$oprefix/lib" ]; then
    [ "$(cat "$oprefix/stamp" 2>/dev/null)" = "$ostamp" ] ||
      { echo "native.sh: $oprefix holds another ONNX Runtime. To install again: $orebuild" >&2; exit 1; }
    (cd "$oprefix" && sha256_check < manifest.sha256) ||
      { echo "native.sh: a file in $oprefix does not match manifest.sha256. To install again: $orebuild" >&2; exit 1; }
  else
    for tool in curl tar; do
      command -v "$tool" >/dev/null 2>&1 || { echo "native.sh: $tool is not on PATH" >&2; exit 1; }
    done
    mkdir -p "$work"
    fetch "$ORT_URL" "$ORT_TGZ" "$ORT_SHA256"
    part="$oprefix.part"
    top="onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}"
    rm -rf "$part"
    mkdir -p "$part/share/licenses/onnxruntime"
    tar -xzf "$work/$ORT_TGZ" -C "$part" --strip-components=1 \
      "$top/include" "$top/lib/libonnxruntime.so.$ORT_VERSION" "$top/LICENSE" "$top/ThirdPartyNotices.txt"
    mv "$part/LICENSE" "$part/ThirdPartyNotices.txt" "$part/share/licenses/onnxruntime/"
    [ -f "$part/lib/libonnxruntime.so.$ORT_VERSION" ] ||
      { echo "native.sh: the release holds no lib/libonnxruntime.so.$ORT_VERSION" >&2; exit 1; }
    ln -s "libonnxruntime.so.$ORT_VERSION" "$part/lib/libonnxruntime.so.1"
    ln -s "libonnxruntime.so.$ORT_VERSION" "$part/lib/libonnxruntime.so"
    (cd "$part" && find include lib share -type f | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done) > "$part/manifest.sha256"
    printf '%s\n' "$ostamp" > "$part/stamp"
    mv "$part" "$oprefix"
  fi
  ln -sfn "$oprefix" "$gomod/.onnxruntime"
  echo "onnxruntime: $oprefix"
  exit 0
fi

command -v zig >/dev/null 2>&1 || { echo "native.sh: zig $ZIG_VERSION is not on PATH; run harness/coppice/scripts/toolchain.sh" >&2; exit 1; }
[ "$(zig version)" = "$ZIG_VERSION" ] || { echo "native.sh: zig is $(zig version), not $ZIG_VERSION" >&2; exit 1; }
command -v "$cc" >/dev/null 2>&1 || { echo "native.sh: $cc is not on PATH; install gcc from your distribution" >&2; exit 1; }

# --moonshine builds moonshine-cli (clients/native/moonshine-cli) into a
# prefix of its own, $cache/native/moonshine-<stamp>: bin/moonshine-cli,
# lib/libonnxruntime.so.1 (the program finds it through $ORIGIN/../lib),
# the two licenses under share/, the stamp and manifest.sha256. Its stamp
# hashes the Moonshine commit, the ONNX Runtime digest, the program's own
# sources, zig, the architecture and the flags, so the Z3 prefix above is
# never rebuilt for it. --print-moonshine prints where it goes. The build
# needs cmake, git and curl, as the main build does.
if [ "$moonshine" -ne 0 ]; then
  [ -n "$ORT_SHA256" ] || { echo "native.sh: no pinned ONNX Runtime release for $(uname -m)" >&2; exit 1; }
  src="$(cd "$gomod/../native/moonshine-cli" && pwd)"
  mstamp="moonshine $MOONSHINE_COMMIT
onnxruntime $ORT_SHA256
sources $(cd "$src" && LC_ALL=C sha256_of CMakeLists.txt moonshine-cli.c no-diarizer.cpp no-zipvoice.cpp ../common/daisugi-native.h | sha256_of | cut -c1-64)
zig $ZIG_VERSION
arch $(uname -m)
flags $MOONSHINE_FLAGS_REV"
  mprefix="${DAISUGI_MOONSHINE_PREFIX:-$cache/native/moonshine-$(printf '%s\n' "$mstamp" | sha256_of | cut -c1-16)}"
  if [ "$moonshine" -eq 2 ]; then
    echo "$mprefix"
    exit 0
  fi
  mrebuild="rm -rf '$mprefix' && $0 --moonshine"
  if [ -e "$mprefix/stamp" ] || [ -e "$mprefix/bin" ]; then
    [ "$(cat "$mprefix/stamp" 2>/dev/null)" = "$mstamp" ] ||
      { echo "native.sh: $mprefix holds a build from other sources. To rebuild: $mrebuild" >&2; exit 1; }
    (cd "$mprefix" && sha256_check < manifest.sha256) ||
      { echo "native.sh: a file in $mprefix does not match manifest.sha256. To rebuild: $mrebuild" >&2; exit 1; }
    echo "checked: $mprefix/bin/moonshine-cli"
    exit 0
  fi
  for tool in cmake git curl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "native.sh: $tool is not on PATH" >&2; exit 1; }
  done
  mkdir -p "$work/bin"
  printf '#!/usr/bin/env bash\nexec zig c++ -mcpu=baseline "$@"\n' > "$work/bin/zig-c++"
  printf '#!/usr/bin/env bash\nexec zig cc -mcpu=baseline "$@"\n' > "$work/bin/zig-cc"
  chmod +x "$work/bin/zig-c++" "$work/bin/zig-cc"
  fetch "$ORT_URL" "$ORT_TGZ" "$ORT_SHA256"
  ort="$work/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}"
  rm -rf "$ort"
  tar -xzf "$work/$ORT_TGZ" -C "$work"
  # Only core/ and the top-level files are checked out; blobs come on demand.
  if [ ! -d "$work/moonshine/.git" ]; then
    git clone --quiet --filter=blob:none --no-checkout "$MOONSHINE_REPO" "$work/moonshine"
  fi
  git -C "$work/moonshine" cat-file -e "$MOONSHINE_COMMIT^{commit}" 2>/dev/null ||
    git -C "$work/moonshine" fetch --quiet --filter=blob:none origin "$MOONSHINE_COMMIT"
  git -C "$work/moonshine" sparse-checkout set core
  git -C "$work/moonshine" checkout --quiet --force --detach "$MOONSHINE_COMMIT"
  [ "$(git -C "$work/moonshine" rev-parse HEAD)" = "$MOONSHINE_COMMIT" ] ||
    { echo "native.sh: moonshine is not at $MOONSHINE_COMMIT" >&2; exit 1; }
  # The build links Microsoft's release, not the copy in the Moonshine tree.
  vend="$work/moonshine/core/third-party/onnxruntime"
  cp -p "$ort/lib/libonnxruntime.so.$ORT_VERSION" "$vend/lib/linux/$ORT_DIR/libonnxruntime.so.1"
  cp -p "$ort"/include/onnxruntime_*.h "$ort/include/cpu_provider_factory.h" "$vend/include/"
  mapflags="-ffile-prefix-map=$work=/daisugi-build -fmacro-prefix-map=$work=/daisugi-build -fdebug-prefix-map=$work=/daisugi-build -ffile-prefix-map=$src=/daisugi-src -fmacro-prefix-map=$src=/daisugi-src"
  # A build tree left by a stopped run is reused, so a rerun resumes.
  cmake -S "$src" -B "$work/moonshine-build" \
    -DMOONSHINE_CORE="$work/moonshine/core" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_C_COMPILER="$work/bin/zig-cc" \
    -DCMAKE_CXX_COMPILER="$work/bin/zig-c++" \
    -DCMAKE_C_FLAGS="$mapflags" \
    -DCMAKE_CXX_FLAGS="-fno-sanitize=undefined $mapflags"
  cmake --build "$work/moonshine-build" --target moonshine-cli -j "$jobs"
  part="$mprefix.part"
  rm -rf "$part"
  mkdir -p "$part/bin" "$part/lib" "$part/share/licenses/moonshine" "$part/share/licenses/onnxruntime"
  cp "$work/moonshine-build/moonshine-cli" "$part/bin/moonshine-cli"
  strip "$part/bin/moonshine-cli"
  cp "$ort/lib/libonnxruntime.so.$ORT_VERSION" "$part/lib/libonnxruntime.so.1"
  git -C "$work/moonshine" show "$MOONSHINE_COMMIT:LICENSE" > "$part/share/licenses/moonshine/LICENSE"
  cp "$ort/LICENSE" "$ort/ThirdPartyNotices.txt" "$part/share/licenses/onnxruntime/"
  (cd "$part" && find bin lib share -type f | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done) > "$part/manifest.sha256"
  printf '%s\n' "$mstamp" > "$part/stamp"
  mv "$part" "$mprefix"
  rm -rf "$work/moonshine-build" "$ort"
  echo "done: $mprefix/bin/moonshine-cli"
  exit 0
fi

# --parakeet builds parakeet-cli and parakeet-quantize
# (clients/native/parakeet-cli) into a prefix of its own,
# $cache/native/parakeet-<stamp>: bin/parakeet-cli, bin/parakeet-quantize,
# the CrispASR and ggml licenses, the stamp and manifest.sha256. Both are
# static apart from the C runtime. check-binary.sh must pass on both, or
# nothing is installed. On x86_64 a CPU with AVX2, FMA, F16C and BMI2 gets
# those in the ggml CPU backend (the stamp names the choice;
# DAISUGI_PARAKEET_ISA=baseline turns it off, for a package); every other
# file is compiled for the baseline CPU, with -ffp-contract=off, so the
# quantizer makes the same bytes on every box. --print-parakeet prints
# where it goes.
if [ "$parakeet" -ne 0 ]; then
  src="$(cd "$gomod/../native/parakeet-cli" && pwd)"
  common="$(cd "$gomod/../native/common" && pwd)"
  # DAISUGI_PARAKEET_ISA=baseline builds for any CPU of the architecture,
  # as a package must (its build box's CPU says nothing of the user's);
  # x86-64-v3 asks for those instructions; auto (the default) takes them
  # when this box has them.
  isa="${DAISUGI_PARAKEET_ISA:-auto}"
  case "$isa" in
    auto)
      isa=baseline
      if [ "$(uname -m)" = x86_64 ] && grep -qw avx2 /proc/cpuinfo && grep -qw fma /proc/cpuinfo &&
        grep -qw f16c /proc/cpuinfo && grep -qw bmi2 /proc/cpuinfo; then
        isa=x86-64-v3
      fi
      ;;
    baseline) ;;
    x86-64-v3)
      [ "$(uname -m)" = x86_64 ] || { echo "native.sh: DAISUGI_PARAKEET_ISA=x86-64-v3 needs an x86_64 build" >&2; exit 2; }
      ;;
    *) echo "native.sh: DAISUGI_PARAKEET_ISA must be auto, baseline or x86-64-v3, not $isa" >&2; exit 2 ;;
  esac
  pstamp="crispasr $CRISPASR_COMMIT
crispasr-files $(sha256_of "$src/crispasr-files.sha256" | cut -c1-64)
ggml $GGML_COMMIT $GGML_TREE_SHA256
sources $(cd "$src" && LC_ALL=C sha256_of CMakeLists.txt parakeet-cli.c check-binary.sh ../common/daisugi-native.h | sha256_of | cut -c1-64)
zig $ZIG_VERSION
arch $(uname -m)
isa $isa
flags $PARAKEET_FLAGS_REV"
  pprefix="${DAISUGI_PARAKEET_PREFIX:-$cache/native/parakeet-$(printf '%s\n' "$pstamp" | sha256_of | cut -c1-16)}"
  if [ "$parakeet" -eq 2 ]; then
    echo "$pprefix"
    exit 0
  fi
  prebuild="rm -rf '$pprefix' && $0 --parakeet"
  if [ -e "$pprefix/stamp" ] || [ -e "$pprefix/bin" ]; then
    [ "$(cat "$pprefix/stamp" 2>/dev/null)" = "$pstamp" ] ||
      { echo "native.sh: $pprefix holds a build from other sources. To rebuild: $prebuild" >&2; exit 1; }
    (cd "$pprefix" && sha256_check < manifest.sha256) ||
      { echo "native.sh: a file in $pprefix does not match manifest.sha256. To rebuild: $prebuild" >&2; exit 1; }
    echo "checked: $pprefix/bin/parakeet-cli"
    exit 0
  fi
  for tool in cmake git strings nm ldd; do
    command -v "$tool" >/dev/null 2>&1 || { echo "native.sh: $tool is not on PATH" >&2; exit 1; }
  done
  mkdir -p "$work/bin"
  printf '#!/usr/bin/env bash\nexec zig c++ -mcpu=baseline -ffp-contract=off "$@"\n' > "$work/bin/zig-c++-nofma"
  printf '#!/usr/bin/env bash\nexec zig cc -mcpu=baseline -ffp-contract=off "$@"\n' > "$work/bin/zig-cc-nofma"
  chmod +x "$work/bin/zig-c++-nofma" "$work/bin/zig-cc-nofma"
  # CrispASR: only the listed files, each checked.
  cr="$work/crispasr"
  if [ ! -d "$cr/.git" ]; then
    git clone --quiet --filter=blob:none --no-checkout "$CRISPASR_REPO" "$cr"
  fi
  git -C "$cr" cat-file -e "$CRISPASR_COMMIT^{commit}" 2>/dev/null ||
    git -C "$cr" fetch --quiet --filter=blob:none origin "$CRISPASR_COMMIT"
  git -C "$cr" sparse-checkout set --no-cone $(awk '{print "/" $2}' "$src/crispasr-files.sha256")
  git -C "$cr" -c submodule.recurse=false checkout --quiet --force --detach "$CRISPASR_COMMIT"
  [ "$(git -C "$cr" rev-parse HEAD)" = "$CRISPASR_COMMIT" ] ||
    { echo "native.sh: CrispASR is not at $CRISPASR_COMMIT" >&2; exit 1; }
  (cd "$cr" && sha256_check < "$src/crispasr-files.sha256") ||
    { echo "native.sh: a CrispASR file does not match crispasr-files.sha256" >&2; exit 1; }
  extra="$(cd "$cr" && find . -type f -not -path './.git/*' | sed 's|^\./||' | LC_ALL=C sort |
    comm -23 - <(awk '{print $2}' "$src/crispasr-files.sha256" | LC_ALL=C sort))"
  [ -z "$extra" ] || { echo "native.sh: CrispASR files outside the list are checked out: $extra" >&2; exit 1; }
  # ggml: the top files, cmake/, include/, the top of src/ and the CPU
  # backend, without its LoongArch and SpacemiT code (compiled only for
  # those CPUs, so a build for x86_64 or aarch64 never needs it, and the
  # tree to check stays small).
  gg="$work/ggml"
  if [ ! -d "$gg/.git" ]; then
    git clone --quiet --filter=blob:none --no-checkout "$GGML_REPO" "$gg"
  fi
  git -C "$gg" cat-file -e "$GGML_COMMIT^{commit}" 2>/dev/null ||
    git -C "$gg" fetch --quiet --filter=blob:none origin "$GGML_COMMIT"
  git -C "$gg" sparse-checkout set --no-cone '/*' '!/*/' '/cmake/' '/include/' '/src/*' '!/src/*/' '/src/ggml-cpu/' '!/src/ggml-cpu/spacemit/' '!/src/ggml-cpu/arch/loongarch/'
  git -C "$gg" checkout --quiet --force --detach "$GGML_COMMIT"
  [ "$(git -C "$gg" rev-parse HEAD)" = "$GGML_COMMIT" ] ||
    { echo "native.sh: ggml is not at $GGML_COMMIT" >&2; exit 1; }
  got="$(cd "$gg" && find . -type f -not -path './.git/*' | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done | sha256_of | cut -c1-64)"
  [ "$got" = "$GGML_TREE_SHA256" ] ||
    { echo "native.sh: the ggml tree hashes to $got, not the pinned $GGML_TREE_SHA256" >&2; exit 1; }
  isaflags=()
  if [ "$isa" = x86-64-v3 ]; then
    isaflags=(-DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_BMI2=ON)
  fi
  mapflags="-ffile-prefix-map=$work=/daisugi-build -fmacro-prefix-map=$work=/daisugi-build -fdebug-prefix-map=$work=/daisugi-build -ffile-prefix-map=$src=/daisugi-src -fmacro-prefix-map=$src=/daisugi-src -ffile-prefix-map=$common=/daisugi-common"
  # A build tree left by a stopped run is reused, so a rerun resumes.
  cmake -S "$src" -B "$work/parakeet-build" \
    -DCRISPASR_SRC="$cr" -DGGML_SRC="$gg" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_C_COMPILER="$work/bin/zig-cc-nofma" \
    -DCMAKE_CXX_COMPILER="$work/bin/zig-c++-nofma" \
    -DCMAKE_C_FLAGS="$mapflags" \
    -DCMAKE_CXX_FLAGS="-fno-sanitize=undefined $mapflags" \
    "${isaflags[@]}"
  cmake --build "$work/parakeet-build" --target parakeet-cli parakeet-quantize -j "$jobs"
  part="$pprefix.part"
  rm -rf "$part"
  mkdir -p "$part/bin" "$part/share/licenses/crispasr" "$part/share/licenses/ggml"
  cp "$work/parakeet-build/parakeet-cli" "$work/parakeet-build/parakeet-quantize" "$part/bin/"
  strip "$part/bin/parakeet-cli" "$part/bin/parakeet-quantize"
  "$src/check-binary.sh" "$part/bin/parakeet-cli"
  "$src/check-binary.sh" --rules "$part/bin/parakeet-quantize"
  cp "$cr/LICENSE" "$part/share/licenses/crispasr/LICENSE"
  cp "$gg/LICENSE" "$part/share/licenses/ggml/LICENSE"
  (cd "$part" && find bin share -type f | LC_ALL=C sort | while IFS= read -r f; do sha256_of "$f"; done) > "$part/manifest.sha256"
  printf '%s\n' "$pstamp" > "$part/stamp"
  mv "$part" "$pprefix"
  rm -rf "$work/parakeet-build"
  echo "done: $pprefix/bin/parakeet-cli"
  exit 0
fi

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
command -v cmake >/dev/null 2>&1 || { echo "native.sh: cmake is not on PATH; install it from your distribution" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "native.sh: git is not on PATH; install it from your distribution" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "native.sh: curl is not on PATH; install it from your distribution" >&2; exit 1; }
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
