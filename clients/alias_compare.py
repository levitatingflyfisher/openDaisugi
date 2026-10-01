"""Compare a port's alias registry with the oracle's on the alias cases.

    uv run --no-sync python clients/alias_compare.py --probe PATH/alias-probe [--oracle]

The probe reads clients/fixtures/alias/cases.jsonl and writes one result
line per case; each result must equal the case's recorded expectation as
canonical JSON (so 1 and 1.0 differ, as they do in a dump). With --oracle
the expectations are made again first, and a case whose expectation
changed is reported as stale.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import alias_cases  # noqa: E402 - sibling module, run as a script


def canonical(v: Any) -> str:
    return json.dumps(v, sort_keys=True, ensure_ascii=True)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--probe", required=True)
    ap.add_argument("--oracle", action="store_true", help="make the expectations again first")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    path = alias_cases.FIXTURE_DIR / "cases.jsonl"
    cases = [json.loads(ln) for ln in path.read_text(encoding="utf-8").splitlines() if ln.strip()]
    stale = 0
    if args.oracle:
        sys.path.insert(0, str(alias_cases.REPO / "src"))
        for c in cases:
            if canonical(alias_cases.run_case(c)) != canonical(c["expect"]):
                stale += 1
                print(f"STALE: {c['name']}")
    proc = subprocess.run(
        [str(Path(args.probe).resolve()), str(path)],
        capture_output=True,
        timeout=300,
        check=False,
    )
    if proc.returncode != 0:
        print(proc.stderr.decode("utf-8", "replace"))
        print("the probe failed")
        return 1
    got = {}
    for ln in proc.stdout.decode("utf-8").splitlines():
        r = json.loads(ln)
        got[r["id"]] = r["result"]
    agree = disagree = 0
    for c in cases:
        r = got.get(c["id"])
        if r is not None and canonical(r) == canonical(c["expect"]):
            agree += 1
            continue
        disagree += 1
        print(f"DISAGREE: {c['name']}")
        width = None if args.verbose else 1500
        print(f"    oracle: {canonical(c['expect'])[:width]}")
        print(f"    port:   {canonical(r)[:width]}")
    print(f"\nalias: {len(cases)} cases: agree={agree}, disagree={disagree}, stale={stale}")
    return 1 if disagree or stale else 0


if __name__ == "__main__":
    raise SystemExit(main())
