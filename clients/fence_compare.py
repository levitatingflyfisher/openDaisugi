"""Compare a port's `daisugi fence` rule list with the oracle's golden cases.

    uv run --no-sync python clients/fence_compare.py --bin PATH_TO_DAISUGI

Each case in ``clients/fixtures/fence/cases.jsonl`` is fed to the binary's
hidden ``fence --plan-from -`` on stdin; its answer must equal the
oracle's, key for key. Exit 1 on any disagreement.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

CASES = Path(__file__).resolve().parent / "fixtures" / "fence" / "cases.jsonl"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--bin", required=True, type=Path)
    args = ap.parse_args()
    bad = 0
    n = 0
    for line in CASES.read_text().splitlines():
        case = json.loads(line)
        n += 1
        run = subprocess.run(
            [str(args.bin), "fence", "--plan-from", "-"],
            input=json.dumps(case["input"]),
            capture_output=True,
            text=True,
            timeout=20,
        )
        try:
            got = json.loads(run.stdout)
        except json.JSONDecodeError:
            got = {"error": run.stdout + run.stderr}
        if got != case["expect"]:
            bad += 1
            print(f"DISAGREE {case['name']} ({case['id']})\n  got  {got}\n  want {case['expect']}")
    print(f"{n} cases, {bad} disagreements")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
