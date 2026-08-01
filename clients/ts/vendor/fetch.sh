#!/bin/sh
# Fetch the pinned tree-sitter-bash grammar wasm (no native build — the npm
# package's install script needs node-gyp, which this box cannot run; the
# published tarball already contains the wasm). Verified by sha256 before
# install so the grammar can never drift from the oracle's 0.25.1 wheel.
set -eu
cd "$(dirname "$0")"
URL="https://registry.npmjs.org/tree-sitter-bash/-/tree-sitter-bash-0.25.1.tgz"
SHA256="8292919c88a0f7d3fb31d0cd0253ca5a9531bc1ede82b0537f2c63dd8abe6a7a"
[ -f tree-sitter-bash.wasm ] && \
  echo "$SHA256  tree-sitter-bash.wasm" | sha256sum -c --quiet - 2>/dev/null && \
  { echo "tree-sitter-bash.wasm already present and verified"; exit 0; }
curl -fsSL "$URL" -o ts-bash.tgz
tar -xzf ts-bash.tgz package/tree-sitter-bash.wasm
mv package/tree-sitter-bash.wasm tree-sitter-bash.wasm
rmdir package; rm ts-bash.tgz
echo "$SHA256  tree-sitter-bash.wasm" | sha256sum -c -
