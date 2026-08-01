"""Cluster conform's mismatches against the corpus by shape, not by line.

Throwaway research tooling (not part of the shipped client). Feeds the whole
corpus to clients/go/conform, compares structurally via the SAME
opendaisugi.conformance.compare_verdict the CLI uses, then buckets
mismatches by kind + a coarse shape key so 700 mismatches become ~20 buckets
to actually look at.

    CUDA_VISIBLE_DEVICES="" uv run python clients/go/analyze_mismatches.py \
        [--kind decompose|verify] [--limit-per-bucket N] [--corpus PATH]
"""

import argparse
import collections
import json
import subprocess
import sys
from pathlib import Path

from opendaisugi import conformance

ROOT = Path(__file__).resolve().parents[2]


def decompose_shape(case, got):
    exp = case["expect"]
    exp_ok = exp.get("ok")
    got_ok = got.get("ok")
    if exp_ok != got_ok:
        return f"ok mismatch: expected={exp_ok} got={got_ok}"
    if not exp_ok:
        return "reject/reject (shouldn't happen — compare_verdict bug?)"
    if got.get("heads") != exp.get("heads"):
        return "heads differ"
    if sorted(got.get("reads", [])) != sorted(exp.get("reads", [])):
        return "reads differ"
    if sorted(got.get("writes", [])) != sorted(exp.get("writes", [])):
        return "writes differ"
    return "other"


def verify_shape(case, got):
    exp = case["expect"]
    exp_ok = exp.get("ok")
    got_ok = got.get("ok")
    if exp_ok != got_ok:
        return f"ok mismatch: expected={exp_ok} got={got_ok}"
    exp_stages = sorted(v["stage"] for v in exp.get("violations", []))
    got_stages = sorted(v["stage"] for v in got.get("violations", []))
    if exp_stages != got_stages:
        return f"stage-multiset differs: expected={exp_stages} got={got_stages}"
    return "same stages, different steps"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--kind", choices=["decompose", "verify"], default=None)
    ap.add_argument("--limit-per-bucket", type=int, default=3)
    ap.add_argument(
        "--corpus", default=str(ROOT / ".opendaisugi/conformance/corpus.jsonl")
    )
    ap.add_argument("--client", default=str(ROOT / "clients/go/conform"))
    args = ap.parse_args()

    lines = [
        l for l in Path(args.corpus).read_text(encoding="utf-8").splitlines() if l.strip()
    ]
    cases = [json.loads(l) for l in lines]

    proc = subprocess.run(
        [args.client],
        input="".join(l + "\n" for l in lines),
        capture_output=True,
        text=True,
        timeout=120,
    )
    if proc.returncode != 0:
        print("CLIENT EXITED", proc.returncode, file=sys.stderr)
        print(proc.stderr[-3000:], file=sys.stderr)
        sys.exit(1)

    verdicts = {}
    for line in proc.stdout.splitlines():
        if not line.strip():
            continue
        v = json.loads(line)
        verdicts[v.get("id", "")] = v

    buckets = collections.defaultdict(list)
    total = 0
    matched = 0
    for case in cases:
        if args.kind and case["kind"] != args.kind:
            continue
        total += 1
        got = verdicts.get(case["id"])
        if got is None:
            buckets[(case["kind"], "NO VERDICT")].append(case)
            continue
        m = conformance.compare_verdict(case, got)
        if m is None:
            matched += 1
            continue
        if case["kind"] == "decompose":
            shape = decompose_shape(case, got)
        else:
            shape = verify_shape(case, got)
        buckets[(case["kind"], shape)].append((case, got))

    print(f"matched {matched}/{total}")
    for (kind, shape), items in sorted(buckets.items(), key=lambda kv: -len(kv[1])):
        print(f"\n=== {kind} :: {shape} :: {len(items)} cases ===")
        for item in items[: args.limit_per_bucket]:
            if kind == "decompose":
                case, got = item
                print(f"  id={case['id']} command={case['command']!r}")
                print(f"    expect={case['expect']}")
                print(f"    got   ={got}")
            else:
                case, got = item
                print(f"  id={case['id']}")
                print(f"    expect={case['expect']}")
                print(f"    got   ={got}")


if __name__ == "__main__":
    main()
