#!/usr/bin/env bash
# harness/coppice/scripts/record-vt.sh
# Records a real harness session as a raw VT byte stream for the grid goldens.
# Usage: scripts/record-vt.sh recorded-claude-idle -- claude
set -euo pipefail
name="${1:?usage: record-vt.sh <name> -- <command> [args...]}"
shift
[ "${1:-}" = "--" ] && shift
out="$(dirname "$0")/../testdata/vt/${name}.bin"
echo "recording to $out. Quit the harness when the screen shows what you want."
script -q -c "$*" "$out.typescript"
# script writes a typescript with its own header line; drop it.
tail -c +$(( $(head -1 "$out.typescript" | wc -c) + 1 )) "$out.typescript" > "$out"
rm -f "$out.typescript"
echo "wrote $out. Now run: COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/"
