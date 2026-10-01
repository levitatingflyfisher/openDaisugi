"""Run the ML pack's golden cases through a daisugi binary (Go or Rust)
and compare them to the oracle.

    uv run --no-sync python clients/pack_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--verbose]

For every case in clients/fixtures/pack/cases.jsonl (clients/pack_cases.py
writes them from the oracle) it checks each step's exit code, stdout and
stderr, the tree after the last step and the paths asked of the fake
server, byte for byte. --oracle also reruns the Python side, so a stale
fixture shows up. The exit code is the number of disagreements (at most
100).
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from pack_cases import FIXTURE_DIR, PY_CLI, SCRATCH, run_case  # noqa: E402


def diff(want, got, where: str = "") -> list[str]:
    if isinstance(want, dict) and isinstance(got, dict):
        out = []
        for k in sorted(set(want) | set(got)):
            out += diff(want.get(k), got.get(k), f"{where}.{k}")
        return out
    if isinstance(want, list) and isinstance(got, list) and len(want) == len(got):
        out = []
        for i, (a, b) in enumerate(zip(want, got, strict=True)):
            out += diff(a, b, f"{where}[{i}]")
        return out
    return [] if want == got else [f"{where}: want {want!r}, got {got!r}"]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary")
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    if not args.binary and not args.oracle:
        ap.error("give --binary, --oracle or both")
    cases = [
        json.loads(ln)
        for ln in (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
        if ln.strip()
    ]
    runs = []
    if args.oracle:
        runs.append(("oracle", PY_CLI))
    if args.binary:
        runs.append((Path(args.binary).name, [str(Path(args.binary).resolve())]))
    bad = 0
    total = 0
    for label, cmd in runs:
        for i, case in enumerate(cases):
            if args.only and args.only not in case["name"]:
                continue
            total += 1
            work = SCRATCH / f"cmp{i:03d}"
            got = run_case(case, cmd, work)
            shutil.rmtree(work, ignore_errors=True)
            problems = diff(case["expect"], got)
            if problems:
                bad += 1
                print(f"DISAGREE {label} {case['name']}")
                for p in problems[: 40 if args.verbose else 5]:
                    print(f"  {p[:300]}")
            elif args.verbose:
                print(f"agree {label} {case['name']}")
    print(f"pack_compare: {total - bad} agree, {bad} disagree")
    return min(bad, 100)


if __name__ == "__main__":
    sys.exit(main())
