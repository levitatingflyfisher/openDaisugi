"""Distillation-fidelity benchmark harness (roadmap Stage 4).

The oldest honest gap in the scorecard is whether distillation — journaled runs
compiled into reusable pathways — actually *pays*. This module is the ruler for
that question: seeded, content-addressed **paired runs** of the same task with
and without a distilled pathway available, token / latency / outcome deltas
reported with confidence intervals, and the safety direction checked too
(pathway-warm runs must not attempt *more* denied or violating actions than
cold ones — a "faster but looser" win is not a win).

The harness deliberately does not know how a task is executed. Execution is an
injected **runner** — `runner(task, *, warm, seed) -> RunMetric | None` — so the
scaffold is testable offline with a fake runner and the real numbers come from a
runner backed by a local model (the piece Stage 4 waits on). A `None` return is
a dropped run (a failed/aborted execution), not a crash.

Stage 4 is *solved* only when this runs over ≥20 real tasks with ≥5 seeded
repeats each for ≥1 local model and the deltas are published whether or not they
flatter; `meets_stage4_bar` checks the run met that bar so the harness can
self-certify rather than over-claim on a thin sample.
"""

from __future__ import annotations

import hashlib
import json
import statistics
from dataclasses import dataclass, field
from typing import Any, Callable

# 95% two-sided Student-t critical values by degrees of freedom (n-1). Small
# samples (the Stage-4 minimum is 5 repeats → df 4) need the t-correction; a
# normal 1.96 would understate the interval. df ≥ 30 falls back to the normal
# approximation.
_T95: dict[int, float] = {
    1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365,
    8: 2.306, 9: 2.262, 10: 2.228, 11: 2.201, 12: 2.179, 13: 2.160, 14: 2.145,
    15: 2.131, 16: 2.120, 17: 2.110, 18: 2.101, 19: 2.093, 20: 2.086,
    21: 2.080, 22: 2.074, 23: 2.069, 24: 2.064, 25: 2.060, 26: 2.056,
    27: 2.052, 28: 2.048, 29: 2.045,
}


def _t95(df: int) -> float:
    if df <= 0:
        return 0.0
    return _T95.get(df, 1.96)


@dataclass
class RunMetric:
    """One run's measurement. ``denials`` / ``violations`` are the count of
    actions the gate/verifier refused during the run — the safety signal."""

    tokens: int
    latency_ms: float
    success: bool
    denials: int = 0
    violations: int = 0


@dataclass
class PairedResult:
    task_id: str
    cold: list[RunMetric] = field(default_factory=list)
    warm: list[RunMetric] = field(default_factory=list)


Runner = Callable[..., "RunMetric | None"]


def tasks_hash(tasks: list[dict[str, Any]]) -> str:
    """Stable content address for the task set, so a benchmark rerun is
    verifiably over the same tasks."""
    blob = json.dumps(tasks, sort_keys=True)
    return hashlib.sha256(blob.encode()).hexdigest()[:16]


def run_paired_benchmark(
    tasks: list[dict[str, Any]],
    runner: Runner,
    *,
    repeats: int = 5,
    base_seed: int = 0,
) -> list[PairedResult]:
    """Run each task ``repeats`` times cold (no pathway) and ``repeats`` times
    warm (pathway available), pairing cold/warm on the same seed so the only
    difference is pathway warmth. A runner returning ``None`` drops that run.
    """
    results: list[PairedResult] = []
    for task in tasks:
        pr = PairedResult(task_id=task.get("id", ""))
        for i in range(repeats):
            seed = base_seed + i
            cold = runner(task, warm=False, seed=seed)
            if cold is not None:
                pr.cold.append(cold)
        for i in range(repeats):
            seed = base_seed + i
            warm = runner(task, warm=True, seed=seed)
            if warm is not None:
                pr.warm.append(warm)
        results.append(pr)
    return results


def _ci95(values: list[float]) -> tuple[float, float]:
    """95% confidence interval for the mean via the t-distribution (honest for
    the small samples Stage 4 uses). Degenerate cases return the point."""
    n = len(values)
    if n == 0:
        return (0.0, 0.0)
    m = statistics.fmean(values)
    if n == 1:
        return (m, m)
    sd = statistics.stdev(values)
    se = sd / (n ** 0.5)
    h = _t95(n - 1) * se
    return (m - h, m + h)


def summarize(results: list[PairedResult]) -> dict[str, Any]:
    """Stratified deltas (warm − cold), two denominators — never one:

    - **Cost deltas** (token, latency) are computed over *successful* runs only.
      A run that fails cheaply at planning burns little and is cheap *because
      it failed*; pooling it with successes would understate the saving and
      conflate two effects. So cost = "what it costs to actually complete the
      task, with reuse vs without", and failed runs are dropped from it.
    - **Outcome delta** (success rate) is over *all* runs — this is where
      "warm reuses a verified plan, so it completes more often" legitimately
      shows up.
    - **Expected tokens per attempt** (including failures) is reported too, but
      labelled, never the headline.

    ``token_delta_mean`` / ``latency_delta_mean`` are ``None`` when no task has a
    paired success in both arms (undefined, not faked as 0). All raw pools are
    returned so anyone can recompute.
    """
    cold_tokens = [m.tokens for r in results for m in r.cold]
    warm_tokens = [m.tokens for r in results for m in r.warm]
    cold_ok = [m.success for r in results for m in r.cold]
    warm_ok = [m.success for r in results for m in r.warm]
    cold_denials = sum(m.denials for r in results for m in r.cold)
    warm_denials = sum(m.denials for r in results for m in r.warm)
    cold_viol = sum(m.violations for r in results for m in r.cold)
    warm_viol = sum(m.violations for r in results for m in r.warm)

    # Cost deltas: successful runs only, paired per task.
    token_deltas = _paired_deltas(results, lambda m: m.tokens, success_only=True)
    latency_deltas = _paired_deltas(results, lambda m: m.latency_ms, success_only=True)

    def _mean_or_none(xs: list[float]) -> float | None:
        return statistics.fmean(xs) if xs else None

    def _ci_or_none(xs: list[float]) -> tuple[float, float] | None:
        return _ci95(xs) if xs else None

    # A regression is warm attempting strictly MORE denied or violating actions
    # than cold — a distilled pathway must not trade safety for speed.
    safety_regression = (warm_denials > cold_denials) or (warm_viol > cold_viol)

    def _mean(xs: list[float]) -> float:
        return statistics.fmean(xs) if xs else 0.0

    return {
        "tasks": len(results),
        "cold_runs": len(cold_tokens),
        "warm_runs": len(warm_tokens),
        # Cost (successful runs only)
        "cost_delta_tasks": len(token_deltas),
        "token_delta_mean": _mean_or_none(token_deltas),
        "token_delta_ci95": _ci_or_none(token_deltas),
        "latency_delta_mean": _mean_or_none(latency_deltas),
        "latency_delta_ci95": _ci_or_none(latency_deltas),
        # Outcome (all runs)
        "cold_success_rate": (sum(cold_ok) / len(cold_ok)) if cold_ok else 0.0,
        "warm_success_rate": (sum(warm_ok) / len(warm_ok)) if warm_ok else 0.0,
        # Expected cost per attempt including failures — labelled, not headline.
        "cold_mean_tokens_per_attempt": _mean(cold_tokens),
        "warm_mean_tokens_per_attempt": _mean(warm_tokens),
        # Safety
        "cold_denials_total": cold_denials,
        "warm_denials_total": warm_denials,
        "cold_violations_total": cold_viol,
        "warm_violations_total": warm_viol,
        "safety_regression": safety_regression,
        "raw": {
            "cold_tokens": cold_tokens, "warm_tokens": warm_tokens,
            "cold_latency_ms": [m.latency_ms for r in results for m in r.cold],
            "warm_latency_ms": [m.latency_ms for r in results for m in r.warm],
        },
    }


def _paired_deltas(results: list[PairedResult],
                   metric: Callable[[RunMetric], float],
                   *, success_only: bool = False) -> list[float]:
    """Per-task warm−cold deltas of the metric means, so the CI is over
    task-level effects rather than conflating within- and between-task variance.
    With ``success_only``, each arm is restricted to its successful runs; a task
    lacking a success in *either* arm contributes no delta (the cost delta is
    undefined for it, not zero)."""
    deltas: list[float] = []
    for r in results:
        cold = [m for m in r.cold if m.success] if success_only else list(r.cold)
        warm = [m for m in r.warm if m.success] if success_only else list(r.warm)
        if not cold or not warm:
            continue
        cold_m = statistics.fmean(metric(m) for m in cold)
        warm_m = statistics.fmean(metric(m) for m in warm)
        deltas.append(warm_m - cold_m)
    return deltas


def meets_stage4_bar(results: list[PairedResult], *,
                     min_tasks: int = 20, min_repeats: int = 5) -> bool:
    """True iff the run met Stage 4's stated bar: ≥``min_tasks`` tasks, each
    with ≥``min_repeats`` cold AND warm runs. Guards against publishing a
    flattering-but-thin sample as if it settled the question."""
    if len(results) < min_tasks:
        return False
    return all(
        len(r.cold) >= min_repeats and len(r.warm) >= min_repeats
        for r in results
    )
