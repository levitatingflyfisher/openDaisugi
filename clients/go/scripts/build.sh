#!/usr/bin/env bash
# clients/go/scripts/build.sh
# Builds daisugi-gate, daisugi and conform in clients/go, after
# scripts/native.sh, then checks that no binary names a path of this box.
# -trimpath drops the Go source paths; native.sh has already mapped the C
# and C++ ones.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(dirname "$here")"
# The prefix's content key makes Go rebuild when a native library changes.
export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -DOPENDAISUGI_NATIVE=$("$here/native.sh" --print-key)"
version="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
go build -trimpath -tags netgo -ldflags="-s -w" -o daisugi-gate ./cmd/daisugi-gate
go build -trimpath -tags netgo -ldflags="-s -w -X daisugi-verify/internal/cli.Version=$version" -o daisugi ./cmd/daisugi
go build -trimpath -ldflags="-s -w" -o conform ./cmd/conform
"$here/check-paths.sh" daisugi-gate daisugi conform
