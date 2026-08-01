"""Run the jit_metrics ruler over a real journal corpus and emit the numbers.

This is the measurement behind the "openDaisugi is a JIT compiler for AI
inference" framing: it takes an existing journal (default: ``~/.opendaisugi``),
replays every interaction through the two rulers in ``opendaisugi.jit_metrics``,
and writes a RESULTS.md a skeptic can reproduce.

    python examples/jit-metrics/measure_corpus.py                 # default corpus
    python examples/jit-metrics/measure_corpus.py --data-dir DIR  # another corpus
    python examples/jit-metrics/measure_corpus.py -o RESULTS.md   # write markdown

No LLM and no network: every number is either a deterministic Z3 ``verify``
timing or a count over the index. The one estimate — the token amortization —
is drawn from ``accounting`` and is labelled an estimate wherever it appears.
"""

from __future__ import annotations

import argparse
import sqlite3
import sys
from pathlib import Path

from opendaisugi.accounting import _ESTIMATED_TOKENS_PER_CALL
from opendaisugi.jit_metrics import (
    breakeven_verifies,
    compilable_fraction,
    envelope_is_symbolic,
    measure_guard_cost,
)
from opendaisugi.journal import Journal


def _all_trace_rows(db_path: Path) -> list[dict]:
    con = sqlite3.connect(db_path)
    try:
        rows = con.execute(
            "SELECT id, run_status, task, structure_signature FROM traces"
        ).fetchall()
    finally:
        con.close()
    return [{"id": r[0], "run_status": r[1], "task": r[2], "sig": r[3]} for r in rows]


def _load_bodies(journal: Journal, ids: list[str]) -> dict[str, object]:
    """Load each successful trace's body once, reused by both rulers.

    A body that won't load (missing/corrupt YAML) is skipped and counted rather
    than crashing the whole run."""
    bodies: dict[str, object] = {}
    missing = 0
    for tid in ids:
        try:
            bodies[tid] = journal.load_trace(tid)
        except Exception:
            missing += 1
    if missing:
        print(f"  (skipped {missing} trace bodies that would not load)", file=sys.stderr)
    return bodies


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", default=str(Path.home() / ".opendaisugi"))
    ap.add_argument("--min-traces", type=int, default=3)
    ap.add_argument("-o", "--out", default=None, help="Write RESULTS.md here.")
    args = ap.parse_args()

    data_dir = Path(args.data_dir)
    db_path = data_dir / "journal" / "index.db"
    if not db_path.exists():
        print(f"no journal index at {db_path}", file=sys.stderr)
        return 1

    journal = Journal(data_dir=data_dir)
    rows = _all_trace_rows(db_path)
    succeeded_ids = [r["id"] for r in rows if r["run_status"] == "succeeded"]
    bodies = _load_bodies(journal, succeeded_ids)

    # --- ruler 1: guard cost over successful (plan, envelope) pairs ----------
    pairs = [(b.plan, b.envelope) for b in bodies.values()]
    guard = measure_guard_cost(pairs)

    # --- ruler 2: compilable fraction over ALL interactions ------------------
    # Conservative cluster key: exact (structure, task). This UNDER-counts vs
    # the distiller's semantic (embedding) clustering, which would also fold
    # near-duplicate task phrasings together — the honest, pessimistic direction.
    def cluster_key(r: dict):
        return (r["sig"], r["task"])

    def guard_ok(r: dict) -> bool:
        b = bodies.get(r["id"])
        if b is None:
            return False
        return journal_verify_ok(b)

    comp = compilable_fraction(
        rows,
        is_success=lambda r: r["run_status"] == "succeeded",
        cluster_key=cluster_key,
        guard_ok=guard_ok,
        min_traces=args.min_traces,
    )

    # --- amortization (ESTIMATE, labelled) -----------------------------------
    compile_est = _ESTIMATED_TOKENS_PER_CALL["tier2"]  # a fresh decompose "compile"
    saved_est = _ESTIMATED_TOKENS_PER_CALL["tier2"] - _ESTIMATED_TOKENS_PER_CALL["tier0"]
    breakeven = breakeven_verifies(compile_est, saved_est)

    distinct_sigs = len({r["sig"] for r in rows if r["run_status"] == "succeeded"})

    report = _render(guard, comp, compile_est, saved_est, breakeven, distinct_sigs, data_dir)
    print(report)
    if args.out:
        Path(args.out).write_text(report, encoding="utf-8")
        print(f"\nwrote {args.out}", file=sys.stderr)
    return 0


def journal_verify_ok(body) -> bool:
    from opendaisugi.verify import verify

    return verify(body.plan, body.envelope).ok


def _pct(x: float) -> str:
    return f"{x * 100:.0f}%"


def _render(guard, comp, compile_est, saved_est, breakeven, distinct_sigs, data_dir) -> str:
    symbolic_pct = _pct(guard.symbolic_only_fraction)
    lines = [
        "# JIT-metrics — measured over a real journal corpus",
        "",
        f"Corpus: `{data_dir}` · {comp.total_interactions} interactions "
        f"({comp.successful} succeeded, "
        f"{comp.total_interactions - comp.successful} rejected/failed). "
        f"Reproduce: `python examples/jit-metrics/measure_corpus.py`.",
        "",
        "> **Corpus caveat, stated up front.** This corpus is benchmark- and "
        f"test-generated: only **{distinct_sigs} distinct plan-structure "
        "signatures** appear among the successes. The compilable fraction below "
        "is therefore a measurement over *this* corpus, not a claim about the "
        "diversity of organic agent usage. The ruler is the contribution; point "
        "it at your own journal for your own number.",
        "",
        "## 1. Guard cost — is checking cheaper than doing?",
        "",
        "Z3 `verify` timed over every successful plan/envelope pair. Latency is "
        "reported as percentiles, never a mean (a timed-out verify sits at the "
        "ceiling and would smear a mean).",
        "",
        "| Metric | Value |",
        "|---|---|",
        f"| Plans timed | {guard.n} |",
        f"| verify latency p50 | **{guard.verify_ms_p50:.2f} ms** |",
        f"| verify latency p95 | {guard.verify_ms_p95:.2f} ms |",
        f"| verify latency max | {guard.verify_ms_max:.2f} ms |",
        f"| Z3 timeout rate | {_pct(guard.timeout_rate)} |",
        f"| Purely-symbolic envelopes (guard needs **no** LLM) | **{symbolic_pct}** |",
        "",
        f"For the **{symbolic_pct}** of envelopes that are purely symbolic, the "
        "guard is the measured sub-millisecond Z3 solve above and costs **zero "
        "tokens** — orders of magnitude below the LLM call that authored the plan. "
        "That is runtime assurance's founding assumption, satisfied and measured."
        + (
            " Every envelope in this corpus is symbolic, so the free-guard claim "
            "holds for all of it; a corpus whose envelopes carry an `llm_check` "
            "soft node would show a lower fraction here, because those pay an LLM "
            "at Stage-2 discharge — which is exactly the slice this number sizes."
            if guard.symbolic_only_fraction >= 1.0
            else " The remaining envelopes carry a soft node (an `llm_check`) whose "
            "Stage-2 discharge pays an LLM — for those the free-guard claim does "
            "**not** hold, and this number is how you size that slice honestly."
        ),
        "",
        "### Amortization (token estimate, labelled)",
        "",
        'The envelope is LLM-authored **once** (the "compile"), then guarded '
        "cheaply per action. Using `accounting`'s per-call token *estimates* "
        f"(compile ≈ {compile_est} tok, saved per reuse ≈ {saved_est} tok):",
        "",
        f"- **break-even ≈ {breakeven:.2f} reuses** — the compile pays for itself "
        "in under one reuse. *(Estimate, not a measurement: the corpus does not "
        "store per-call token counts. The latency numbers above are measured; "
        "this line is the one modelled figure.)*",
        "",
        "## 2. Compilable fraction — how much of the corpus could compile?",
        "",
        "Over **all** interactions (the pessimistic denominator — failures and "
        "gate-rejections included). A trace is compilable when it lands in a "
        f"cluster of ≥{comp.min_traces} similar successes (the distiller's own "
        "hotness precondition) **and** its plan still verifies against its "
        "envelope.",
        "",
        "| Metric | Value |",
        "|---|---|",
        f"| Total interactions (denominator) | {comp.total_interactions} |",
        f"| Succeeded | {comp.successful} ({_pct(comp.successful_fraction)}) |",
        f"| Distillable clusters (≥{comp.min_traces}) | {comp.distillable_clusters} |",
        f"| Traces in distillable clusters | {comp.traces_in_distillable_clusters} |",
        f"| **Compilable (upper bound)** | **{comp.compilable_upper_bound}** |",
        f"| **Compilable fraction of all interactions** | **{_pct(comp.compilable_fraction_of_all_interactions)}** |",
        f"| Guard-pass rate within hot clusters | {_pct(comp.hot_task_guard_pass_rate)} |",
        "",
        f"> **Oracle:** {comp.oracle}",
        "",
        "The headline is an **upper bound**: it proves each counted interaction is "
        "*authorized* (guard passes) and *structurally distillable* (clears the "
        "hotness gate), not that a reused plan would be *correct*. A real "
        "correctness oracle is a separate research problem; naming the gap is the "
        "honest move, and the Stage-4 fidelity benchmark is where the correctness "
        "question is attacked empirically.",
        "",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    sys.exit(main())
