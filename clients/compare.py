"""The tournament: run every client over the corpus, collect the comparison.

Usage (from the repo root):

    uv run python clients/compare.py .opendaisugi/conformance/corpus.jsonl \
        --client rust="clients/rust/target/release/conform" \
        --client go="clients/go/conform" \
        --out .opendaisugi/conformance/results.json

The Python oracle is always included (as "python"). Output is a JSON
document with per-client, per-kind match counts, mismatch ids (capped),
startup latency, and pipe-mode bench percentiles — the data the comparison
report renders. Never commit the output (mismatch entries embed corpus
content).
"""

from __future__ import annotations

import argparse
import collections
import json
import shlex
import subprocess
import sys
import time
from pathlib import Path

from opendaisugi.conformance import (
    ClientError,
    _normal_decompose,
    _normal_verify,
    bench_corpus,
    canonical_json,
    run_corpus,
)

MISMATCH_CAP = 40


def _measure_startup(cmd: list[str], probe_case: str, repeat: int = 5) -> float:
    """Median wall time to start the client, answer ONE case, and exit."""
    times = []
    for _ in range(repeat):
        t0 = time.monotonic()
        subprocess.run(
            cmd, input=probe_case, capture_output=True, text=True, timeout=120, check=False
        )
        times.append((time.monotonic() - t0) * 1000)
    return sorted(times)[len(times) // 2]


def _kind_of(case_ids: dict[str, str], mid: str) -> str:
    return case_ids.get(mid, "?")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("corpus", type=Path)
    ap.add_argument("--client", action="append", default=[], metavar="NAME=CMD")
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--skip-bench", action="store_true")
    args = ap.parse_args()

    clients: dict[str, list[str]] = {
        "python": [sys.executable, "-m", "opendaisugi.conformance"]
    }
    for spec in args.client:
        name, _, cmd = spec.partition("=")
        clients[name] = shlex.split(cmd)

    cases = [json.loads(x) for x in args.corpus.read_text().splitlines() if x.strip()]
    kind_by_id = {c["id"]: c["kind"] for c in cases}
    kind_totals = collections.Counter(c["kind"] for c in cases)
    probe = json.dumps(next(c for c in cases if c["kind"] == "decompose")) + "\n"

    results: dict = {
        "corpus": {"count": len(cases), "kinds": dict(kind_totals)},
        "clients": {},
    }
    for name, cmd in clients.items():
        print(f"== {name}: {' '.join(cmd)}", flush=True)
        entry: dict = {"cmd": " ".join(cmd)}
        try:
            report = run_corpus(args.corpus, cmd)
        except (ClientError, OSError) as e:
            entry["error"] = str(e)[:300]
            results["clients"][name] = entry
            print(f"   FAILED: {entry['error']}", flush=True)
            continue
        by_kind = collections.Counter(_kind_of(kind_by_id, m.case_id) for m in report.mismatches)
        entry["matched"] = report.matched
        entry["total"] = report.total
        entry["mismatches_by_kind"] = dict(by_kind)
        entry["matched_by_kind"] = {
            k: kind_totals[k] - by_kind.get(k, 0) for k in kind_totals
        }
        entry["mismatch_ids"] = [m.case_id for m in report.mismatches[:MISMATCH_CAP]]
        entry["startup_ms"] = round(_measure_startup(cmd, probe), 1)
        if not args.skip_bench:
            b = bench_corpus(args.corpus, client_cmd=cmd)
            entry["bench"] = {
                "p50_ms": b.p50_ms, "p95_ms": b.p95_ms, "p99_ms": b.p99_ms,
                "cases_per_s": round(b.cases_per_s, 1),
            }
        results["clients"][name] = entry
        print(f"   {report.matched}/{report.total} matched", flush=True)

    # Client-vs-client agreement — the N-version signal the oracle comparison
    # can't show. Two independent implementations disagreeing WITH THE ORACLE on
    # the SAME case, in the SAME direction, is the strongest bug indicator there
    # is (it is how G-4 surfaced). Recompute each client's normalized verdict per
    # case, then cross-tabulate.
    def _norm(cid, v):
        if "ok" not in v:
            return ("__error__",)
        fn = _normal_verify if kind_by_id[cid] == "verify" else _normal_decompose
        return fn(v)

    verds: dict[str, dict[str, tuple]] = {}
    for name, cmd in clients.items():
        if "error" in results["clients"].get(name, {}):
            continue
        proc = subprocess.run(  # noqa: S603 — caller's own client binaries
            cmd,
            input="".join(canonical_json(c) + "\n" for c in cases),
            capture_output=True, text=True, timeout=600, check=False,
        )
        d: dict[str, tuple] = {}
        for line in proc.stdout.splitlines():
            if line.strip():
                v = json.loads(line)
                if "id" in v:
                    d[v["id"]] = _norm(v["id"], v)
        verds[name] = d

    names = list(verds)
    pairwise = {}
    for i in range(len(names)):
        for j in range(i + 1, len(names)):
            a, b = names[i], names[j]
            agree = sum(1 for cid in kind_by_id if verds[a].get(cid) == verds[b].get(cid))
            pairwise[f"{a}|{b}"] = agree
    results["pairwise_agreement"] = pairwise

    # Cases where >=2 NON-oracle clients disagree with the oracle AND agree with
    # each other — an independent-implementation consensus against the reference.
    oracle = verds.get("python", {})
    others = [n for n in names if n != "python"]
    consensus = []
    for cid in kind_by_id:
        dissenters = [n for n in others if verds.get(n, {}).get(cid) != oracle.get(cid)]
        if len(dissenters) >= 2:
            vs = {verds[n][cid] for n in dissenters if cid in verds[n]}
            if len(vs) == 1:  # the dissenters all agree with each other
                consensus.append({"id": cid, "kind": kind_by_id[cid], "clients": dissenters})
    results["independent_consensus_against_oracle"] = {
        "count": len(consensus), "cases": consensus[:MISMATCH_CAP],
    }

    args.out.write_text(json.dumps(results, indent=1, sort_keys=True) + "\n")
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
