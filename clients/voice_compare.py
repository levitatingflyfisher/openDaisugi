"""Run the voice bridge cases through a port and compare it to the oracle.

    uv run --no-sync python clients/voice_compare.py --binary PATH/daisugi \\
        --probe PATH/voice-probe [--oracle] [--only TEXT] [--max-refused N] [--json out.json]

Cases come from clients/fixtures/voice/cases.jsonl (clients/voice_cases.py
writes them from the oracle). A probe case runs the port's voice-probe on the
same query; a cli case runs ``daisugi voice ...``; a server case starts the
port's ``daisugi voice serve`` against the same fake whisper-cli, fake coppice
and fake upstream. Each observation is compared with the oracle's, with a
case's ``port_expect`` fields (the rulings, VO-n in clients/ADJUDICATIONS.md)
put over it first.

A disagreement is classed by what it would do:

- fail-open: the port answers 2xx where the oracle refuses, or reaches the
  pane (a coppice send) where the oracle does not. A blocker.
- disagree: any other difference.
- refused: the port says the command is not in it.

With --oracle the Python side runs the cases live as well, so a stale
fixture shows as an oracle-vs-fixture difference.
"""

from __future__ import annotations

import argparse
import copy
import json
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixture_paths import leaks  # noqa: E402
from voice_cases import FIXTURE_DIR, SCRATCH, run_case  # noqa: E402


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
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


def expected(case: dict[str, Any]) -> dict[str, Any]:
    want = copy.deepcopy(case["expect"])
    for key, value in (case.get("port_expect") or {}).items():
        node = want
        parts = key.split(".")
        for p in parts[:-1]:
            node = node.setdefault(p, {})
        node[parts[-1]] = value
    return want


def _sent(obs: dict[str, Any]) -> int:
    return sum(
        1
        for ln in obs.get("coppice", [])
        if isinstance(ln, dict) and ln.get("cmd") in ("pane.send_text", "agent.prompt")
    )


def classify(case: dict[str, Any], want: dict[str, Any], got: dict[str, Any]) -> str:
    if want == got:
        return "agree"
    if case["kind"] == "cli" and "not in this binary" in got.get("stderr", ""):
        return "refused"
    if case["kind"] == "server":
        if _sent(got) > _sent(want):
            return "fail-open"
        for w, g in zip(want.get("replies", []), got.get("replies", []), strict=False):
            ws, gs = w.get("status"), g.get("status")
            if isinstance(ws, int) and isinstance(gs, int) and ws >= 400 and 200 <= gs < 300:
                return "fail-open"
    if case["kind"] == "probe":
        wa, ga = want.get("answer") or {}, got.get("answer") or {}
        if wa.get("armed") is False and ga.get("armed") is True:
            return "fail-open"
        if wa.get("delivered") in ("refused", "preview") and ga.get("delivered") == "sent":
            return "fail-open"
    return "disagree"


def load_cases(path: Path) -> list[dict[str, Any]]:
    return [json.loads(ln) for ln in path.read_text(encoding="utf-8").splitlines() if ln.strip()]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True, help="a daisugi binary with voice")
    ap.add_argument("--probe", required=True, help="the port's voice-probe binary")
    ap.add_argument("--cases", type=Path, default=FIXTURE_DIR / "cases.jsonl")
    ap.add_argument("--oracle", action="store_true", help="also run the oracle live")
    ap.add_argument("--only", default="", help="run only cases whose name holds this text")
    ap.add_argument("--max-refused", type=int, default=None)
    ap.add_argument("--json", type=Path, default=None)
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    probe = str(Path(args.probe).resolve())
    cases = load_cases(args.cases)
    if args.only:
        cases = [c for c in cases if args.only in c["name"]]
    counts: dict[str, int] = {}
    rows = []
    stale: list[str] = []
    for i, c in enumerate(cases):
        work = SCRATCH / "cmp" / f"{i:04d}"
        got = run_case(c, binary, probe, work)
        want = expected(c)
        cls = classify(c, want, got)
        counts[cls] = counts.get(cls, 0) + 1
        row: dict[str, Any] = {"name": c["name"], "id": c["id"], "class": cls}
        if cls != "agree":
            row["diff"] = diff(want, got)
            print(f"{cls:9s} {c['name']}")
            for d in row["diff"]:
                print(f"    {d}")
        rows.append(row)
        if args.oracle:
            live = run_case(c, None, None, SCRATCH / "cmp" / f"{i:04d}py")
            if live != c["expect"]:
                stale.append(c["name"])
                print(f"stale     {c['name']}: {diff(c['expect'], live)[:3]}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    summary = ", ".join(f"{k} {v}" for k, v in sorted(counts.items()))
    print(f"{len(cases)} voice cases: {summary}")
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    refused = counts.get("refused", 0)
    over = args.max_refused is not None and refused > args.max_refused
    if over:
        print(f"{refused} cases refused; at most {args.max_refused} are ruled")
    if args.json:
        args.json.write_text(
            json.dumps(
                {"cases": len(cases), "counts": counts, "rows": rows, "stale": stale}, indent=1
            )
            + "\n",
            encoding="utf-8",
        )
    bad = sum(v for k, v in counts.items() if k not in ("agree", "refused"))
    return 1 if bad or stale or leaked or over else 0


if __name__ == "__main__":
    raise SystemExit(main())
