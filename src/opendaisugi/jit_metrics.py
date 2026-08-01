"""JIT-metrics — the ruler for openDaisugi's "JIT compiler for AI inference" claim.

Two measurements a skeptic (or a professor) can rerun over any journal corpus.
Both are deliberately scoped as a *ruler*, not a platform: they emit numbers,
they do not decide anything.

**1. Guard-cost vs decision-cost — is checking orders of magnitude cheaper than
doing?** This is runtime assurance's founding assumption (and the question a
hardware-JIT designer asks first: a guard that costs as much as recompilation
buys nothing). For openDaisugi the honest answer is *bimodal* and must be
reported as an amortization curve, not a flat ratio:

  - The envelope is LLM-generated **once** per task class — the "compile".
  - Z3 ``verify`` then runs **per action** — the "guard".
  - When the envelope is *purely symbolic*, the guard is ~free (microseconds,
    zero tokens) and the crux is emphatically satisfied.
  - When the envelope carries **soft nodes** (an ``llm_check`` predicate, an
    unsupported regex), the guard itself pays an LLM at Stage 2 — and the crux
    can flip. So the deliverable is the symbolic-only latency distribution
    *plus the fraction of real envelopes that are purely symbolic*. Publishing
    the unflattering second number is the credibility move.

**2. Compilable fraction — of ALL interactions, how many could be compiled into
a reusable pathway?** "Nobody has measured this" was the professor-facing claim;
this is the ruler for it. The denominator is *all* interactions (the pessimistic,
honest denominator — not "tasks that already cleared the hotness gate", which is
near-100% and worthless). A trace counts as compilable when it lands in a cluster
of ``>= min_traces`` similar successes (the distiller's own precondition) **and**
the guard passes on reuse. This is an explicitly-labelled **upper bound**: it
proves the reused plan is *authorized* and *structurally distillable*, not that
it is *correct*. A real correctness oracle is a research project; this repo's
ethos is to ship the honest upper bound with the gap named, not to fake the
oracle.
"""

from __future__ import annotations

import math
import time
from collections import defaultdict
from dataclasses import asdict, dataclass
from typing import Any, Callable, Hashable, Iterable

from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import compile_to_z3
from opendaisugi.verify import verify

# ---------------------------------------------------------------------------
# pure helpers
# ---------------------------------------------------------------------------


def _percentile(values: list[float], pct: float) -> float:
    """Nearest-rank percentile (deterministic, no interpolation surprises).

    ``pct`` in [0, 100]. Empty input is 0.0 (undefined, reported as zero rather
    than raised — the caller reports ``n`` alongside so an empty run is visible).
    """
    if not values:
        return 0.0
    ordered = sorted(values)
    if pct <= 0:
        return ordered[0]
    rank = math.ceil(pct / 100.0 * len(ordered))
    return ordered[min(rank, len(ordered)) - 1]


def breakeven_verifies(compile_cost_tokens: float, saved_tokens_per_reuse: float) -> float:
    """Reuses needed before a one-time envelope "compile" pays for itself.

    The amortization number the flat "verify is 2000× cheaper" claim hides: the
    envelope was LLM-generated once. ``inf`` when a reuse saves nothing (the
    compile never recoups) — honest, not a divide-by-zero crash.
    """
    if saved_tokens_per_reuse <= 0:
        return float("inf")
    return compile_cost_tokens / saved_tokens_per_reuse


# ---------------------------------------------------------------------------
# 1. guard cost
# ---------------------------------------------------------------------------


def envelope_is_symbolic(plan: ActionPlan, envelope: Envelope) -> bool:
    """True iff the guard needs *no* Stage-2 LLM discharge for this plan.

    Compiles every invariant/postcondition expression against the concrete plan
    and checks that none produced ``soft_nodes`` (``llm_check`` or a regex the
    translator can't handle). An expression that fails to parse/compile is
    treated as *not* symbolic — a guard we can't compile symbolically is not a
    free symbolic guard. ``expr=None`` (opaque) invariants don't add soft nodes
    and so don't flip the classification: they are a separate (strict-mode)
    concern, not an LLM-discharge cost.
    """
    exprs = [inv.expr for inv in envelope.invariants if inv.expr is not None]
    exprs += [pc.expr for pc in envelope.postconditions if pc.expr is not None]
    for raw in exprs:
        try:
            node = parse_expression(raw)
            compiled = compile_to_z3(node, plan, envelope)
        except Exception:
            return False
        if compiled.soft_nodes:
            return False
    return True


# (elapsed_ms, timed_out, ok)
GuardTimer = Callable[[ActionPlan, Envelope], "tuple[float, bool, bool]"]
SymbolicFn = Callable[[ActionPlan, Envelope], bool]


@dataclass
class GuardCostReport:
    """The guard-cost distribution — latency percentiles, timeout + symbolic rates.

    Latency is reported as p50/p95/max (never a mean: a timed-out verify sits at
    the ``z3_timeout_ms`` ceiling and a mean would smear it across the body).
    ``symbolic_only_fraction`` is the load-bearing honesty number: the free-guard
    claim only covers that slice of the corpus.
    """

    n: int
    verify_ms_p50: float
    verify_ms_p95: float
    verify_ms_max: float
    timeout_rate: float
    ok_rate: float
    symbolic_only_fraction: float

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


def _default_timer(z3_timeout_ms: int) -> GuardTimer:
    def timer(plan: ActionPlan, envelope: Envelope) -> tuple[float, bool, bool]:
        t0 = time.perf_counter()
        result = verify(plan, envelope, z3_timeout_ms=z3_timeout_ms)
        elapsed = (time.perf_counter() - t0) * 1000.0
        # A genuine Z3 timeout can't be read back cleanly from VerifyResult, so
        # approximate it by the wall-clock ceiling. Reported as a rate, and the
        # injected-timer path (tests, precise harnesses) sets it exactly.
        timed_out = elapsed >= z3_timeout_ms * 0.95
        return elapsed, timed_out, result.ok

    return timer


def measure_guard_cost(
    pairs: Iterable[tuple[ActionPlan, Envelope]],
    *,
    timer: GuardTimer | None = None,
    symbolic: SymbolicFn | None = None,
    z3_timeout_ms: int = 500,
) -> GuardCostReport:
    """Time ``verify`` over ``(plan, envelope)`` pairs; report the distribution.

    ``timer`` and ``symbolic`` are injectable so the aggregation is unit-testable
    without Z3; the defaults wire the real verifier and the real symbolic check.
    """
    timer = timer or _default_timer(z3_timeout_ms)
    symbolic = symbolic or envelope_is_symbolic

    latencies: list[float] = []
    timeouts = 0
    oks = 0
    symbolic_ct = 0
    for plan, envelope in pairs:
        elapsed, timed_out, ok = timer(plan, envelope)
        latencies.append(elapsed)
        timeouts += int(timed_out)
        oks += int(ok)
        symbolic_ct += int(symbolic(plan, envelope))

    n = len(latencies)
    denom = n or 1
    return GuardCostReport(
        n=n,
        verify_ms_p50=_percentile(latencies, 50),
        verify_ms_p95=_percentile(latencies, 95),
        verify_ms_max=_percentile(latencies, 100),
        timeout_rate=timeouts / denom,
        ok_rate=oks / denom,
        symbolic_only_fraction=symbolic_ct / denom,
    )


# ---------------------------------------------------------------------------
# 2. compilable fraction
# ---------------------------------------------------------------------------


@dataclass
class CompilableReport:
    """What fraction of a corpus could be compiled into a reusable pathway.

    ``compilable_fraction_of_all_interactions`` is the headline and is
    deliberately pessimistic — the denominator is every interaction, failures
    included. ``oracle`` names the honesty gap: this is ``hit × guard_pass``, an
    upper bound, not a proof the reused plan was correct.
    """

    total_interactions: int
    successful: int
    successful_fraction: float
    distillable_clusters: int
    traces_in_distillable_clusters: int
    compilable_upper_bound: int
    compilable_fraction_of_all_interactions: float
    hot_task_guard_pass_rate: float
    min_traces: int
    oracle: str = (
        "hit × guard_pass — UPPER BOUND: proves authorized + structurally "
        "distillable, NOT that the reused plan is correct (no correctness oracle)"
    )

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


def compilable_fraction(
    items: Iterable[Any],
    *,
    is_success: Callable[[Any], bool],
    cluster_key: Callable[[Any], Hashable],
    guard_ok: Callable[[Any], bool] | None = None,
    min_traces: int = 3,
) -> CompilableReport:
    """Compute the compilable-fraction report over ``items`` (all interactions).

    ``cluster_key`` maps a *successful* interaction to its task-class key (the
    distiller clusters by task semantics + plan structure; a caller replays that
    signal here). ``guard_ok`` is the upper-bound reuse guard (defaults to always
    true, giving the pure structural ceiling). A cluster is *distillable* when it
    holds ``>= min_traces`` successes — the distiller's own hotness precondition.
    """
    guard_ok = guard_ok or (lambda _i: True)
    items = list(items)
    total = len(items)
    successes = [i for i in items if is_success(i)]

    clusters: dict[Hashable, list[Any]] = defaultdict(list)
    for i in successes:
        clusters[cluster_key(i)].append(i)
    distillable = [members for members in clusters.values() if len(members) >= min_traces]
    candidates = [i for members in distillable for i in members]
    compilable = [i for i in candidates if guard_ok(i)]

    denom_all = total or 1
    denom_hot = len(candidates) or 1
    return CompilableReport(
        total_interactions=total,
        successful=len(successes),
        successful_fraction=len(successes) / denom_all,
        distillable_clusters=len(distillable),
        traces_in_distillable_clusters=len(candidates),
        compilable_upper_bound=len(compilable),
        compilable_fraction_of_all_interactions=len(compilable) / denom_all,
        hot_task_guard_pass_rate=len(compilable) / denom_hot,
        min_traces=min_traces,
    )


__all__ = [
    "CompilableReport",
    "GuardCostReport",
    "breakeven_verifies",
    "compilable_fraction",
    "envelope_is_symbolic",
    "measure_guard_cost",
]
