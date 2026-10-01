"""Run the stage-K3 cases through a daisugi binary (Go or Rust) and
compare them to the oracle.

    uv run --no-sync python clients/k3_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--skip NAME] [--verbose] [--max-refused N]

For every case in clients/fixtures/k3 (clients/k3_cases.py writes them from
the oracle) it checks the exit code, stdout, stderr, the tree after (every
database dumped whole) and the model requests, byte for byte, as
clients/k2_compare.py does. For an MCP session stdout is every line the
server wrote.

A refusal: a command the binary names as not in it yet must leave the tree
as it was. An MCP request the binary does not answer the oracle's way gets
a JSON-RPC error that says "... is not in this binary yet."; the session
then agrees when every line before that reply is the oracle's, that reply
answers the oracle's id, and the binary wrote no file the oracle did not.

The guard: on every tree the binary wrote, Python must still read the
journal and the pathway store it left.

--oracle also reruns the Python side, so a stale fixture shows up.
--max-refused N fails the run when more than N cases are refused or not
ported, so a binary that carries K3 cannot fall back to refusing it.
"""

from __future__ import annotations

import argparse
import collections
import json
import re
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import k3_cases  # noqa: E402, F401 - sibling module, run as a script (it patches garden_cases)
from cli_cases import as_daisugi  # noqa: E402
from fixture_paths import leaks  # noqa: E402
from garden_cases import run_case  # noqa: E402
from garden_compare import NOT_YET, before_tree, compare, guard  # noqa: E402
from k3_cases import FIXTURE_DIR, SCRATCH, cmd_for  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402

_REPLY_ID = re.compile(r'^\{"jsonrpc":"2\.0","id":(.*?),"(?:result|error)":')


def mcp_refused(
    case: dict[str, Any], got: dict[str, Any], before: dict[str, Any]
) -> tuple[str, list[str]] | None:
    """The class of an MCP session the binary refused part of, or None when
    it refused nothing."""
    lines = got["stdout"].splitlines()
    at = next((i for i, ln in enumerate(lines) if NOT_YET in ln), None)
    if at is None:
        return None
    want = case["expect"]["stdout"].splitlines()
    problems = []
    if lines[:at] != want[:at]:
        problems.append(
            "the lines before the refusal differ: " + "; ".join(diff(want[:at], lines[:at])[:4])
        )
    # The oracle's line may hold a placeholder ({MS}), so its id is read
    # from the text, not parsed.
    rid = _REPLY_ID.match(lines[at])
    oid = _REPLY_ID.match(want[at]) if at < len(want) else None
    if rid is None or oid is None or rid.group(1) != oid.group(1):
        problems.append("the refusal is not a reply to the oracle's request")
    allowed = set(before) | set(case["expect"]["tree"])
    extra = sorted(set(got["tree"]) - allowed)
    if extra:
        problems.append(f"the binary wrote files the oracle did not: {extra}")
    for k, v in before.items():
        if got["tree"].get(k) != v:
            problems.append(f"the binary changed {k}")
    return ("disagree", problems) if problems else ("refused", [lines[at]])


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
        got = run_case(case, cmd_for(case, binary), work)
        verdict = mcp_refused(case, got, before) if case.get("kind") == "mcp" else None
        if verdict is not None:
            cls, problems = verdict
        else:
            cls, problems = compare(case, got, before)
        # A case may opt out of the guard when its own journal is broken on
        # purpose (`lora export` reads one that holds a trace with no YAML),
        # so Python cannot read it back whatever the binary does.
        if cls == "agree" and got["tree"] != before and case.get("guard", True):
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
                print(f"    {str(p)[:600]}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']} {str(problems[:1])[:300]}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    print(
        f"\nk3: {sum(classes.values())} cases: "
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
