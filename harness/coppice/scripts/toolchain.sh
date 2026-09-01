#!/usr/bin/env bash
# harness/coppice/scripts/toolchain.sh
# Installs the build-time toolchain for coppice into $HOME/.local. No sudo.
# Undo the two global Go settings with: go env -u GOTOOLCHAIN PKG_CONFIG
set -euo pipefail

ZIG_VERSION="0.16.0"
ZIG_URL="https://ziglang.org/download/0.16.0/zig-x86_64-linux-0.16.0.tar.xz"
ZIG_SHA256="70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00"
GHOSTTY_COMMIT="b0c421fcd2e290629d4285c181b52fe2f2095f06"

prefix="${COPPICE_GHOSTTY_PREFIX:-$HOME/.local/ghostty-vt}"
work="${COPPICE_BUILD_DIR:-$HOME/.local/share/coppice-build}"   # real disk, /tmp is RAM here
mkdir -p "$HOME/.local/bin" "$work"

# 1. Zig 0.16, checksum-verified.
if ! command -v zig >/dev/null 2>&1; then
  echo "installing zig $ZIG_VERSION into $HOME/.local"
  curl -fsSL "$ZIG_URL" -o "$work/zig.tar.xz"
  echo "$ZIG_SHA256  $work/zig.tar.xz" | sha256sum -c -
  tar -xJf "$work/zig.tar.xz" -C "$work"
  rm -rf "$HOME/.local/zig-$ZIG_VERSION"
  mv "$work/zig-x86_64-linux-$ZIG_VERSION" "$HOME/.local/zig-$ZIG_VERSION"
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
