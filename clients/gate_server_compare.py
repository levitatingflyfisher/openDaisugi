"""Run every resident-gate case through a binary's `gate serve` and compare it to the oracle.

    uv run --no-sync python clients/gate_server_compare.py \\
        --binary PATH/daisugi [--port go|rust] [--oracle] [--json out.json]

Cases come from clients/fixtures/gate_server/cases.jsonl (written by
clients/gate_server_cases.py from the Python server). The binary's server
is started the way the cases start the oracle's; every request case and
every scenario runs against it, and each observation is compared with the
oracle's, with a case's ``port_expect`` fields (the rulings) put in first.

A disagreement is classed by what it would do to a client:

- fail-open: the binary's reply is an allow where the oracle's is a deny.
  A blocker.
- stricter: the binary's reply is a deny where the oracle's is an allow.
- record: the same verdict, but the reply bytes or a side effect differ.

With --oracle the Python server runs the cases live as well, so a stale
fixture shows as an oracle-vs-fixture difference. The p50 of a gate
decision over the socket (connect, send, reply) is reported for each.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from gate_server_cases import (  # noqa: E402 - sibling module, run as a script
    FIXTURE_DIR,
    SCRATCH,
    binary_cmd,
    oracle_cmd,
    reply_verdict,
    run_all,
)


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    """Paths where two JSON values differ (at most 12)."""
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            if k not in a or k not in b:
                out.append(f"{path}.{k}: {'missing' if k not in b else 'extra'}")
            else:
                diff(a[k], b[k], f"{path}.{k}", out)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{path}: {len(a)} items vs {len(b)}")
        for i, (x, y) in enumerate(zip(a, b, strict=False)):
            diff(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append(f"{path}: {str(a)[:300]!r} vs {str(b)[:300]!r}")
    return out


def expected(case: dict[str, Any], port: str) -> dict[str, Any]:
    want = {**case["expect"], **(case.get("port_expect") or {})}
    if port == "rust":
        want.update(case.get("rust_expect") or {})
    return want


def classify(case: dict[str, Any], want: dict[str, Any], got: dict[str, Any]) -> str:
    if want == got:
        return "agree"
    if case["kind"] == "request":
        w, g = reply_verdict(want.get("reply", "")), reply_verdict(got.get("reply", ""))
        if w == "deny" and g == "allow":
            return "fail-open"
        if w == "allow" and g == "deny":
            return "stricter"
    return "record"


def load_cases(path: Path) -> list[dict[str, Any]]:
    return [
        json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()
    ]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True, help="a daisugi binary with `gate serve`")
    ap.add_argument("--port", choices=("go", "rust"), default="go", help="whose rulings apply")
    ap.add_argument("--cases", type=Path, default=FIXTURE_DIR / "cases.jsonl")
    ap.add_argument("--oracle", action="store_true", help="also run the Python server live")
    ap.add_argument("--only", default="", help="run only cases whose name holds this text")
    ap.add_argument("--json", type=Path, default=None)
    args = ap.parse_args()
    cases = load_cases(args.cases)
    if args.only:
        cases = [c for c in cases if args.only in c["name"]]
    got, times = run_all(cases, binary_cmd(args.binary), SCRATCH / "port")
    counts: dict[str, int] = {}
    rows = []
    for c, g in zip(cases, got, strict=True):
        want = expected(c, args.port)
        cls = classify(c, want, g)
        counts[cls] = counts.get(cls, 0) + 1
        row = {"name": c["name"], "id": c["id"], "class": cls}
        if cls != "agree":
            row["diff"] = diff(want, g)
            print(f"{cls:9s} {c['name']}")
            for d in row["diff"]:
                print(f"    {d}")
        rows.append(row)
    report: dict[str, Any] = {"cases": len(cases), "counts": counts, "rows": rows}
    if times:
        report["port_p50_ms"] = round(statistics.median(times) * 1000, 2)
        report["port_decisions"] = len(times)
    if args.oracle:
        live, otimes = run_all(cases, oracle_cmd(), SCRATCH / "oracle")
        stale = [c["name"] for c, o in zip(cases, live, strict=True) if o != c["expect"]]
        report["oracle_vs_fixture"] = stale
        for n in stale:
            print(f"stale     {n}")
        if otimes:
            report["oracle_p50_ms"] = round(statistics.median(otimes) * 1000, 2)
    summary = ", ".join(f"{k} {v}" for k, v in sorted(counts.items()))
    print(f"{len(cases)} cases: {summary}")
    for k in ("port_p50_ms", "oracle_p50_ms"):
        if k in report:
            print(f"{k}: {report[k]}")
    if args.json:
        args.json.write_text(json.dumps(report, indent=1) + "\n", encoding="utf-8")
    bad = sum(v for k, v in counts.items() if k != "agree")
    stale_n = len(report.get("oracle_vs_fixture", []))
    return 1 if bad or stale_n else 0


if __name__ == "__main__":
    raise SystemExit(main())
