"""Oracle probe harness for the Go client's decompose port.

Reads shell command strings on stdin (one per line), runs each through the
REAL Python oracle's decompose_command(), and prints the Decomposition as
JSON on stdout. Used to build Go test tables and to minimize corpus
mismatches down to the exact tree-sitter-bash behavior, without guessing.

Usage:
    CUDA_VISIBLE_DEVICES="" uv run python clients/go/probe.py <<'EOF'
    FOO=1
    a=(1 2)
    $(which ls) -la
    EOF

Run from the repo root (uv project root), NOT from clients/go/.
"""

import json
import sys

from opendaisugi.shell_decompose import decompose_command


def main() -> None:
    for line in sys.stdin:
        # Preserve the raw line minus only the trailing newline, so a command
        # that is itself whitespace-only round-trips faithfully.
        if line.endswith("\n"):
            line = line[:-1]
        d = decompose_command(line)
        out = {
            "command": line,
            "ok": d.ok,
            "heads": list(d.heads),
            "commands": list(d.commands),
            "reads": list(d.reads),
            "writes": list(d.writes),
            "reason": d.reason,
        }
        print(json.dumps(out))


if __name__ == "__main__":
    main()
