"""Run the stage-K1 cases through the Go binary and its envelope probe, and
compare them to the oracle.

    uv run --no-sync python clients/k1_compare.py --binary clients/go/daisugi \\
        --probe PATH/envelope-probe [--oracle] [--only NAME] [--verbose]

For every case in clients/fixtures/k1 (clients/k1_cases.py writes them
from the oracle) it checks the exit code, stdout, stderr, the tree after
(every database dumped whole) and the model requests, byte for byte, as
clients/garden_compare.py checks its own. A command the binary names as
not in it yet, or a probe query that answers {"unported": ...}, must leave
the tree as it was.

The guard: on every tree the binary wrote, Python must still read what it
wrote. A pathway store is read as garden_compare reads one; an envelope
cache is read row by row with EnvelopeCache.get's validation.

--oracle also reruns the Python side, so a stale fixture shows up.
"""

from __future__ import annotations

import argparse
import collections
import json
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import k1_cases  # noqa: E402, F401 - sibling module, run as a script (it patches garden_cases.lay_out)
from fixture_paths import leaks  # noqa: E402
from garden_cases import base_env, run_case  # noqa: E402
from garden_compare import before_tree, compare, guard  # noqa: E402
from k1_cases import FIXTURE_DIR, SCRATCH, cmd_for  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402

_CACHE_GUARD = r"""
import sqlite3, sys
from opendaisugi.models import Envelope
con = sqlite3.connect(sys.argv[1])
for (text,) in con.execute("SELECT envelope_json FROM envelope_cache"):
    Envelope.model_validate_json(text)
print("ok")
"""


def cache_guard(work: Path) -> list[str]:
    home = work / "home"
    out = []
    for db in sorted(home.rglob("envelope_cache.db")) if home.exists() else []:
        g = subprocess.run(
            [sys.executable, "-c", _CACHE_GUARD, str(db)],
            env=base_env(home, work),
            capture_output=True,
            timeout=120,
            check=False,
        )
        if g.returncode != 0:
            out.append(
                f"guard {db.name}: Python cannot read the cache the binary left: {g.stderr[-300:]!r}"
            )
    return out


def unported(got: dict[str, Any]) -> bool:
    try:
        return "unported" in json.loads(got["stdout"])
    except ValueError:
        return False


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--probe", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    probe = str(Path(args.probe).resolve())
    SCRATCH.mkdir(parents=True, exist_ok=True)
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
        got = run_case(case, cmd_for(case, binary, probe), work)
        if case["kind"] == "probe" and unported(got):
            ok = got["tree"] == before
            cls, problems = (
                ("not-ported", []) if ok else ("disagree", ["unported but changed the tree"])
            )
        else:
            cls, problems = compare(case, got, before)
        if cls == "agree" and got["tree"] != before:
            guarded += 1
            problems = guard(binary, work) + cache_guard(work)
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run_case(case, cmd_for(case, None, None), SCRATCH / "cmp" / f"{i:04d}py")
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
        f"\nk1: {sum(classes.values())} cases: "
        + ", ".join(f"{k}={v}" for k, v in sorted(classes.items()))
    )
    print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    print(f"guard: Python read back the trees of {guarded} cases the binary wrote")
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    return 1 if classes.get("disagree") or stale or leaked else 0


if __name__ == "__main__":
    raise SystemExit(main())
