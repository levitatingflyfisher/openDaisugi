"""False-accept classifier for the Lean decompose subset parser.

Feeds the whole corpus to the built `conform` binary directly (bypassing
the daisugi CLI's capped mismatch report) and classifies every mismatch:

  - decompose false-accept: oracle ok=false, we ok=true; OR both ok=true
    but (heads, sorted reads, sorted writes) differ. This must be ZERO.
  - decompose false-reject: oracle ok=true, we ok=false. Expected/fine.
  - verify mismatch: reported separately (predicate/z3/delegation-subsumption
    stages are out of scope, so some are expected — see README).
  - error: our client emitted a bare error verdict (should be near-zero;
    task 1's gate is zero crashes, meaning every case gets SOME verdict,
    but an `error` verdict is still a mismatch worth counting separately).

Usage: CUDA_VISIBLE_DEVICES="" .venv/bin/python3 clients/lean/analysis/classify.py
       [--corpus PATH] [--client PATH] [--limit N] [--show N]
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=str(REPO_ROOT / ".opendaisugi/conformance/corpus.jsonl"))
    ap.add_argument("--client", default=str(REPO_ROOT / "clients/lean/.lake/build/bin/conform"))
    ap.add_argument("--limit", type=int, default=None)
    ap.add_argument("--show", type=int, default=15)
    args = ap.parse_args()

    lines = [
        l for l in Path(args.corpus).read_text(encoding="utf-8").splitlines() if l.strip()
    ]
    if args.limit:
        lines = lines[: args.limit]
    cases = [json.loads(l) for l in lines]

    proc = subprocess.run(
        [args.client], input="".join(l + "\n" for l in lines),
        capture_output=True, text=True, timeout=600,
    )
    if proc.returncode != 0:
        print(f"CLIENT EXITED {proc.returncode}: {proc.stderr[-2000:]}", file=sys.stderr)
        return 1

    verdicts: dict[str, dict] = {}
    bad_lines = 0
    for line in proc.stdout.splitlines():
        if not line.strip():
            continue
        try:
            v = json.loads(line)
        except json.JSONDecodeError:
            bad_lines += 1
            continue
        verdicts[v.get("id", "")] = v

    n_verify = n_decompose = 0
    verify_match = verify_mismatch = 0
    decompose_match = 0
    false_accept = []
    false_reject = 0
    errors = 0
    no_verdict = 0

    for case in cases:
        cid = case["id"]
        got = verdicts.get(cid)
        if got is None:
            no_verdict += 1
            continue
        if case["kind"] == "verify":
            n_verify += 1
            exp = case["expect"]
            exp_pairs = sorted((v["stage"], v.get("step") or "") for v in exp.get("violations", []))
            got_pairs = sorted((v["stage"], v.get("step") or "") for v in got.get("violations", []))
            if "error" in got:
                errors += 1
                verify_mismatch += 1
            elif bool(exp["ok"]) == bool(got.get("ok")) and exp_pairs == got_pairs:
                verify_match += 1
            else:
                verify_mismatch += 1
        else:
            n_decompose += 1
            exp = case["expect"]
            if "error" in got:
                errors += 1
                continue
            exp_ok = bool(exp["ok"])
            got_ok = bool(got.get("ok"))
            if not exp_ok and not got_ok:
                decompose_match += 1
            elif exp_ok and not got_ok:
                false_reject += 1
            elif exp_ok and got_ok:
                exp_tuple = (
                    tuple(exp.get("heads", [])),
                    tuple(sorted(exp.get("reads", []))),
                    tuple(sorted(exp.get("writes", []))),
                )
                got_tuple = (
                    tuple(got.get("heads", [])),
                    tuple(sorted(got.get("reads", []))),
                    tuple(sorted(got.get("writes", []))),
                )
                if exp_tuple == got_tuple:
                    decompose_match += 1
                else:
                    false_accept.append((cid, case["command"], exp, got))
            else:  # not exp_ok and got_ok
                false_accept.append((cid, case["command"], exp, got))

    print(f"total cases: {len(cases)}  (verify={n_verify} decompose={n_decompose})")
    print(f"bad JSON lines from client: {bad_lines}")
    print(f"no verdict at all: {no_verdict}")
    print(f"client-side error verdicts: {errors}")
    print()
    print(f"verify: {verify_match}/{n_verify} matched")
    print()
    print(f"decompose: {decompose_match}/{n_decompose} matched")
    print(f"decompose false-reject (safe, expected): {false_reject}")
    print(f"decompose FALSE-ACCEPT (must be zero): {len(false_accept)}")
    for cid, cmd, exp, got in false_accept[: args.show]:
        print(f"  --- {cid} ---")
        print(f"  command: {cmd!r}")
        print(f"  expected: {exp}")
        print(f"  got:      {got}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
