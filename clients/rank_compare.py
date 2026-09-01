"""Run the rank cases through a daisugi binary (Go or Rust) and
compare them to the oracle.

    uv run --no-sync python clients/rank_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--verbose]

For every case in clients/fixtures/rank (clients/rank_cases.py writes them from
the oracle) it checks the exit code, stdout, stderr, the tree after (every
database dumped whole) and the model requests, byte for byte, as
clients/garden_compare.py checks its own. A command the binary names as not
in it yet must leave the tree as it was.

The guard: on every tree the binary wrote, Python must still read the
journal and the pathway store it left.

--oracle also reruns the Python side, so a stale fixture shows up.
--max-refused N fails the run when more than N cases are refused or not
ported, so a binary that carries rank cannot fall back to refusing it.
"""

from __future__ import annotations

import argparse
import collections
import json
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import rank_cases  # noqa: E402, F401 - sibling module, run as a script (it patches garden_cases)
from cli_cases import as_daisugi  # noqa: E402
from fixture_paths import leaks  # noqa: E402
from garden_cases import run_case  # noqa: E402
from garden_compare import before_tree, compare, guard  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402
from rank_cases import FIXTURE_DIR, SCRATCH, cmd_for  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--max-refused", type=int, default=None)
    args = ap.parse_args()
    SCRATCH.mkdir(parents=True, exist_ok=True)
    binary = as_daisugi(args.binary, SCRATCH)
    cases = [
        json.loads(ln)
        for ln in (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
        if ln.strip()
    ]
    classes: collections.Counter[str] = collections.Counter()
    stale = guarded = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, SCRATCH / "cmp" / f"{i:04d}b")
        got = run_case(case, cmd_for(case, binary), work)
        cls, problems = compare(case, got, before)
        if cls == "agree" and got["tree"] != before:
            guarded += 1
            problems = guard(binary, work)
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run_case(case, cmd_for(case, None), SCRATCH / "cmp" / f"{i:04d}py")
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}")
        classes[cls] += 1
        if cls == "disagree":
            print(f"DISAGREE: {case['name']}")
            for p in problems[:12]:
                print(f"    {p}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    print(
        f"\nrank: {sum(classes.values())} cases: "
        + ", ".join(f"{k}={v}" for k, v in sorted(classes.items()))
    )
    print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    print(f"guard: Python read back the trees of {guarded} cases the binary wrote")
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    refused = classes.get("refused", 0) + classes.get("not-ported", 0)
    over = args.max_refused is not None and refused > args.max_refused
    if over:
        print(f"{refused} cases refused or not ported; at most {args.max_refused} are ruled")
    return 1 if classes.get("disagree") or stale or leaked or over else 0


if __name__ == "__main__":
    raise SystemExit(main())
