#!/usr/bin/env bash
# harness/coppice/scripts/toolchain.sh
# Installs the build-time toolchain for coppice under
# ${XDG_DATA_HOME:-$HOME/.local/share}/opendaisugi. No sudo. It changes no
# global Go setting and puts nothing on PATH: it prints the lines a build needs.
set -euo pipefail

# Map this host to one of the platform names Zig's own release tarballs use
# (https://ziglang.org/download/#release-0.16.0), and refuse cleanly on
# anything else instead of silently downloading a binary for the wrong
# platform and failing later with a confusing exec error.
case "$(uname -s)" in
  Linux)  zig_os="linux" ;;
  Darwin) zig_os="macos" ;;
  *)
    echo "toolchain.sh: unsupported OS '$(uname -s)'. Zig 0.16.0 is only" >&2
    echo "fetched here for Linux and macOS. Install zig 0.16.0 yourself and" >&2
    echo "put it on PATH, then re-run this script." >&2
    exit 1
    ;;
esac
case "$(uname -m)" in
  x86_64|amd64)   zig_arch="x86_64" ;;
  aarch64|arm64)  zig_arch="aarch64" ;;
  *)
    echo "toolchain.sh: unsupported CPU architecture '$(uname -m)'. Zig" >&2
    echo "0.16.0 is only fetched here for x86_64 and aarch64. Install zig" >&2
    echo "0.16.0 yourself and put it on PATH, then re-run this script." >&2
    exit 1
    ;;
esac

ZIG_VERSION="0.16.0"
ZIG_PLATFORM="${zig_arch}-${zig_os}"
ZIG_URL="https://ziglang.org/download/${ZIG_VERSION}/zig-${ZIG_PLATFORM}-${ZIG_VERSION}.tar.xz"
# Checksums copied from the shasum field https://ziglang.org/download/index.json
# publishes for each platform. Only x86_64-linux has actually been downloaded
# and run through this script; the other three are taken as published, not
# independently re-verified here.
case "$ZIG_PLATFORM" in
  x86_64-linux)
    ZIG_SHA256="70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00" ;;
  aarch64-linux)
    ZIG_SHA256="ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17" ;;
  x86_64-macos)
    ZIG_SHA256="0387557ed1877bc6a2e1802c8391953baddba76081876301c522f52977b52ba7" ;;
  aarch64-macos)
    ZIG_SHA256="b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489" ;;
  *)
    echo "toolchain.sh: no pinned checksum for platform '$ZIG_PLATFORM'." >&2
    exit 1
    ;;
esac
GHOSTTY_COMMIT="b0c421fcd2e290629d4285c181b52fe2f2095f06"

# sha256sum is not on macOS by default; shasum -a 256 is the built-in there.
if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
else
  sha256_check() { shasum -a 256 -c -; }
fi

# Everything goes under the XDG data directory. A libghostty-vt an older
# version of this script built at ~/.local/ghostty-vt is kept and used
# (internal/toolchain.GhosttyPrefix applies the same rule).
data="${XDG_DATA_HOME:-$HOME/.local/share}/opendaisugi"
if [ -n "${COPPICE_GHOSTTY_PREFIX:-}" ]; then
  prefix="$COPPICE_GHOSTTY_PREFIX"
elif [ -f "$HOME/.local/ghostty-vt/share/pkgconfig/libghostty-vt-static.pc" ]; then
  prefix="$HOME/.local/ghostty-vt"
else
  prefix="$data/ghostty-vt"
fi
work="${COPPICE_BUILD_DIR:-$data/coppice-build}"   # real disk; /tmp may be RAM
mkdir -p "$work"

# 1. Zig 0.16, checksum-verified, under $data. Nothing goes on the user's
#    PATH; this script puts it first on its own PATH, so the pinned zig is
#    the one used even when another zig is installed.
zig_dir="$data/zig-$ZIG_VERSION"
if [ ! -x "$zig_dir/zig" ]; then
  echo "installing zig $ZIG_VERSION ($ZIG_PLATFORM) into $zig_dir"
  curl -fsSL "$ZIG_URL" -o "$work/zig.tar.xz"
  echo "$ZIG_SHA256  $work/zig.tar.xz" | sha256_check
  tar -xJf "$work/zig.tar.xz" -C "$work"
  rm -rf "$zig_dir"
  mv "$work/zig-$ZIG_PLATFORM-$ZIG_VERSION" "$zig_dir"
  rm -f "$work/zig.tar.xz"
fi

# 2. CMake 4.4.3, the binary inside the official PyPI wheel, checked
#    against the sha256 PyPI publishes, under $data. No pip, no uv tool, no
#    shim on PATH; used even when another cmake is installed. Only the
#    x86_64-linux wheel has been downloaded and checked through this
#    script; the other two hashes are taken as PyPI publishes them.
CMAKE_VERSION="4.4.3"
cmake_pypi="https://files.pythonhosted.org/packages"
case "$ZIG_PLATFORM" in
  x86_64-linux)
    CMAKE_URL="$cmake_pypi/0b/d0/18fe62f0bddc8b9bb71ba55762ee48a224f8344101c34350d969f6e0d327/cmake-4.4.3-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl"
    CMAKE_SHA256="bae3c4954623ec4d62e62c70443f0da7988b733111c2871fcc6a31ead5137e20"
    cmake_in_wheel="cmake/data/bin" ;;
  aarch64-linux)
    CMAKE_URL="$cmake_pypi/fc/64/c227d8a26f17c82b0864faafb9f4a2db0eb425f613367eaad1e9d6260eef/cmake-4.4.3-py3-none-manylinux2014_aarch64.manylinux_2_17_aarch64.whl"
    CMAKE_SHA256="520ff2ba3afb7a1e34a0ab222c0f4e89ad334dc7462814886e099bdf6ba8cbb3"
    cmake_in_wheel="cmake/data/bin" ;;
  x86_64-macos|aarch64-macos)
    CMAKE_URL="$cmake_pypi/da/2e/78cc0dab93ad407e4b126ab6a3c8a6fc3df89011b49e910c5a5e9bda78a6/cmake-4.4.3-py3-none-macosx_10_10_universal2.whl"
    CMAKE_SHA256="6c95b37116bb5c714656e4f76931ebdcb739209a1aee91cf51408ccfe137694e"
    cmake_in_wheel="cmake/data/CMake.app/Contents/bin" ;;
esac
cmake_dir="$data/cmake-$CMAKE_VERSION"
cmake_bin="$cmake_dir/$cmake_in_wheel"
if [ ! -x "$cmake_bin/cmake" ]; then
  echo "installing cmake $CMAKE_VERSION into $cmake_dir"
  curl -fsSL "$CMAKE_URL" -o "$work/cmake.whl"
  echo "$CMAKE_SHA256  $work/cmake.whl" | sha256_check
  rm -rf "$cmake_dir"
  python3 -m zipfile -e "$work/cmake.whl" "$cmake_dir"
  chmod +x "$cmake_bin"/*
  rm -f "$work/cmake.whl"
fi

# The steps below use the pinned zig and cmake, whatever else is on PATH.
export PATH="$zig_dir:$cmake_bin:$PATH"

# 3. libghostty-vt at the commit go-libghostty fetches.
if [ ! -f "$prefix/share/pkgconfig/libghostty-vt-static.pc" ]; then
  echo "building libghostty-vt at $GHOSTTY_COMMIT"
  if [ ! -d "$work/ghostty/.git" ]; then
    git clone --filter=blob:none https://github.com/ghostty-org/ghostty.git "$work/ghostty"
  fi
  git -C "$work/ghostty" fetch --depth 1 origin "$GHOSTTY_COMMIT"
  git -C "$work/ghostty" checkout --detach "$GHOSTTY_COMMIT"
  ( cd "$work/ghostty" && zig build -Demit-lib-vt --prefix "$prefix" )
fi

# 4. No global setting and nothing on the user's PATH. A build needs these
#    lines in its own environment (a shell, a CI step); nothing else on
#    this box changes.
echo "done. A coppice build needs these in its environment:"
echo "  export PATH=\"$zig_dir:$cmake_bin:\$PATH\""
echo "  export GOTOOLCHAIN=auto"
echo "  export PKG_CONFIG_PATH=\"$prefix/share/pkgconfig\${PKG_CONFIG_PATH:+:\$PKG_CONFIG_PATH}\""
echo "Then run harness/coppice/scripts/preflight.sh to confirm."
