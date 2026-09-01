#!/usr/bin/env bash
# harness/coppice/scripts/install.sh
# Kept for old notes: the install now lives at the repo root and puts
# coppice, sprig and daisugi on PATH together. It takes the same flags.
exec "$(cd "$(dirname "$0")/../../.." && pwd)/scripts/install.sh" "$@"
