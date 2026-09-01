"""Run the stage-K4 cases through a daisugi binary (Go or Rust) and
compare them to the oracle.

    uv run --no-sync python clients/k4_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--skip NAME] [--verbose] [--max-refused N]

For every case in clients/fixtures/k4 (clients/k4_cases.py writes them from
the oracle) it checks the exit code, stdout, stderr, the tree after (every
database dumped whole) and, for a serve case, every scrape, as
clients/k3_compare.py does. Where a case carries `go_expect`, those fields
replace the oracle's (K4-2).

A refusal: a command the binary names as not in it yet must leave the tree
as it was. A serve case the binary refuses never listens, so it has no
scrapes to compare.

The guard: on every tree the binary wrote, Python must still read the
journal and the pathway store it left.

--oracle also reruns the Python side, so a stale fixture shows up.
--max-refused N fails the run when more than N cases are refused or not
ported, so a binary that carries K4 cannot fall back to refusing it.
"""

from __future__ import annotations

import argparse
import collections
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import k4_cases  # noqa: E402, F401 - sibling module, run as a script (it patches garden_cases)
from cli_cases import as_daisugi  # noqa: E402
from fixture_paths import leaks  # noqa: E402
from garden_compare import before_tree, compare, guard  # noqa: E402
from k4_cases import FIXTURE_DIR, SCRATCH, cmd_for, run  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--skip")
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
        if args.skip and args.skip in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, SCRATCH / "cmp" / f"{i:04d}b")
        got = run(case, cmd_for(case, binary), work)
        want = {**case["expect"], **(case.get("go_expect") or {})}
        cls, problems = compare({**case, "expect": want}, got, before)
        if cls == "agree" and got.get("scrapes") != want.get("scrapes"):
            cls = "disagree"
            problems = ["scrapes: " + d for d in diff(want.get("scrapes"), got.get("scrapes"))]
        if cls == "agree" and got["tree"] != before:
            guarded += 1
            problems = guard(binary, work)
            if problems:
                # A store the case laid out broken is broken before the
                # binary runs too; only a new failure counts.
                base = SCRATCH / "cmp" / f"{i:04d}base"
                garden_cases.lay_out(case.get("before") or {}, base / "home", time.time())
                known = {q.split(":")[0] for q in guard(binary, base)}
                problems = [p for p in problems if p.split(":")[0] not in known]
                shutil.rmtree(base, ignore_errors=True)
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run(case, cmd_for(case, None), SCRATCH / "cmp" / f"{i:04d}py")
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}")
        classes[cls] += 1
        if cls == "disagree":
            print(f"DISAGREE: {case['name']}")
            for p in problems[:12]:
                print(f"    {str(p)[:600]}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']} {str(problems[:1])[:300]}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    print(
        f"\nk4: {sum(classes.values())} cases: "
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
