#!/usr/bin/env bash
# harness/coppice/scripts/toolchain.sh
# Installs the build-time toolchain for coppice into $HOME/.local. No sudo.
# Undo the two global Go settings with: go env -u GOTOOLCHAIN PKG_CONFIG
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

prefix="${COPPICE_GHOSTTY_PREFIX:-$HOME/.local/ghostty-vt}"
work="${COPPICE_BUILD_DIR:-$HOME/.local/share/coppice-build}"   # real disk; /tmp may be RAM
mkdir -p "$HOME/.local/bin" "$work"

# 1. Zig 0.16, checksum-verified.
if ! command -v zig >/dev/null 2>&1; then
  echo "installing zig $ZIG_VERSION ($ZIG_PLATFORM) into $HOME/.local"
  curl -fsSL "$ZIG_URL" -o "$work/zig.tar.xz"
  echo "$ZIG_SHA256  $work/zig.tar.xz" | sha256_check
  tar -xJf "$work/zig.tar.xz" -C "$work"
  rm -rf "$HOME/.local/zig-$ZIG_VERSION"
  mv "$work/zig-$ZIG_PLATFORM-$ZIG_VERSION" "$HOME/.local/zig-$ZIG_VERSION"
  ln -sf "$HOME/.local/zig-$ZIG_VERSION/zig" "$HOME/.local/bin/zig"
  rm -f "$work/zig.tar.xz"
fi

# 2. CMake, from uv so nothing is installed system-wide.
command -v cmake >/dev/null 2>&1 || uv tool install cmake

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

# 4. A pkg-config wrapper, so a bare `go test ./...` finds the .pc file.
#    It APPENDS to PKG_CONFIG_PATH, so every other Go module on this box keeps working.
cat > "$HOME/.local/bin/coppice-pkg-config" <<WRAP
#!/usr/bin/env bash
export PKG_CONFIG_PATH="\${PKG_CONFIG_PATH:+\$PKG_CONFIG_PATH:}$prefix/share/pkgconfig"
exec pkg-config "\$@"
WRAP
chmod +x "$HOME/.local/bin/coppice-pkg-config"

# 5. Two global Go settings. Undo with: go env -u GOTOOLCHAIN PKG_CONFIG
go env -w GOTOOLCHAIN=auto
go env -w PKG_CONFIG="$HOME/.local/bin/coppice-pkg-config"

echo "done. Run harness/coppice/scripts/preflight.sh to confirm."
echo "to undo the global Go settings: go env -u GOTOOLCHAIN PKG_CONFIG"
