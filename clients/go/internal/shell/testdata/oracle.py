"""Write the oracle's decomposition of each command, for decompose_test.go.

    PYTHONPATH=src python oracle.py commands.jsonl out.jsonl

Each input line is a JSON string. Each output line is
{"src", "ok", "heads", "commands", "reads", "writes", "reason"}, or
{"src", "error"} when decompose_command raises.
"""

import json
import sys

from opendaisugi.shell_decompose import decompose_command


def main() -> None:
    with open(sys.argv[1], encoding="utf-8") as f, open(sys.argv[2], "w", encoding="utf-8") as o:
        for line in f:
            if not line.strip():
                continue
            src = json.loads(line)
            try:
                d = decompose_command(src)
            except Exception as exc:  # noqa: BLE001 - recorded as the oracle's answer
                o.write(json.dumps({"src": src, "error": f"{type(exc).__name__}: {exc}"}) + "\n")
                continue
            o.write(json.dumps({
                "src": src, "ok": d.ok, "heads": list(d.heads), "commands": list(d.commands),
                "reads": list(d.reads), "writes": list(d.writes), "reason": d.reason,
            }) + "\n")


if __name__ == "__main__":
    main()
