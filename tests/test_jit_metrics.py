"""Tests for the jit_metrics ruler (roadmap: the JIT-compiler measurement)."""

from __future__ import annotations

import pytest

from opendaisugi.jit_metrics import (
    CompilableReport,
    GuardCostReport,
    _percentile,
    breakeven_verifies,
    compilable_fraction,
    envelope_is_symbolic,
    measure_guard_cost,
)
from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep

# --- pure helpers -----------------------------------------------------------


def test_percentile_nearest_rank():
    xs = [10.0, 20.0, 30.0, 40.0]
    assert _percentile(xs, 50) == 20.0
    assert _percentile(xs, 100) == 40.0
    assert _percentile(xs, 0) == 10.0


def test_percentile_empty_is_zero():
    assert _percentile([], 50) == 0.0


def test_breakeven_verifies_recoups_compile_cost():
    # A 2000-token envelope compile that saves 4500 tokens per reuse pays for
    # itself in well under one reuse.
    assert breakeven_verifies(2000, 4500) == pytest.approx(2000 / 4500)


def test_breakeven_verifies_no_saving_is_infinite():
    assert breakeven_verifies(2000, 0) == float("inf")
    assert breakeven_verifies(2000, -1) == float("inf")


# --- guard-cost measurement (injected timer, no Z3 needed) ------------------


def _pair():
    plan = ActionPlan(source="t", task="x", steps=[ShellStep(id="s1", command="ls")])
    env = Envelope(generated_by="t", task="x", permissions=Permission(shell=True))
    return plan, env


def test_measure_guard_cost_percentiles_and_rates():
    pairs = [_pair() for _ in range(4)]
    times = iter([10.0, 20.0, 30.0, 40.0])
    symb = iter([True, True, False, True])

    def timer(plan, env):
        return next(times), False, True  # (elapsed_ms, timed_out, ok)

    def symbolic(plan, env):
        return next(symb)

    report = measure_guard_cost(pairs, timer=timer, symbolic=symbolic)
    assert isinstance(report, GuardCostReport)
    assert report.n == 4
    assert report.verify_ms_p50 == 20.0
    assert report.verify_ms_max == 40.0
    assert report.timeout_rate == 0.0
    assert report.symbolic_only_fraction == pytest.approx(0.75)


def test_measure_guard_cost_counts_timeouts():
    pairs = [_pair() for _ in range(2)]
    outcomes = iter([(10.0, False, True), (500.0, True, False)])

    def timer(plan, env):
        return next(outcomes)

    report = measure_guard_cost(pairs, timer=timer, symbolic=lambda p, e: True)
    assert report.timeout_rate == 0.5
    assert report.ok_rate == 0.5


# --- symbolic-vs-LLM classification (real compile) --------------------------


def test_envelope_with_no_exprs_is_symbolic():
    plan, _ = _pair()
    env = Envelope(generated_by="t", task="x", permissions=Permission(shell=True))
    assert envelope_is_symbolic(plan, env) is True


def test_envelope_with_llm_check_invariant_is_not_symbolic():
    plan, _ = _pair()
    env = Envelope(
        generated_by="t",
        task="x",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(
                type="llm_guard",
                description="the command is not destructive",
                expr={"op": "llm_check", "rule": "the command is not destructive"},
            )
        ],
    )
    assert envelope_is_symbolic(plan, env) is False


# --- compilable fraction ----------------------------------------------------


def test_compilable_fraction_pessimistic_denominator():
    # 6 interactions: 5 successes clustering as A,A,A,B,B + 1 failure.
    items = [
        {"ok": True, "key": "A"},
        {"ok": True, "key": "A"},
        {"ok": True, "key": "A"},
        {"ok": True, "key": "B"},
        {"ok": True, "key": "B"},
        {"ok": False, "key": "A"},
    ]
    rep = compilable_fraction(
        items,
        is_success=lambda i: i["ok"],
        cluster_key=lambda i: i["key"],
        min_traces=3,
    )
    assert isinstance(rep, CompilableReport)
    assert rep.total_interactions == 6
    # Only cluster A (3 successes) clears min_traces; B has 2. Upper bound = 3.
    assert rep.compilable_upper_bound == 3
    assert rep.compilable_fraction_of_all_interactions == pytest.approx(3 / 6)
    # Of the 3 traces in distillable clusters, all pass the (default) guard.
    assert rep.hot_task_guard_pass_rate == pytest.approx(1.0)


def test_compilable_fraction_guard_upper_bound_trims():
    items = [{"ok": True, "key": "A"} for _ in range(3)]
    # One of the three distillable traces fails the guard on reuse.
    guard = iter([True, True, False])
    rep = compilable_fraction(
        items,
        is_success=lambda i: i["ok"],
        cluster_key=lambda i: i["key"],
        guard_ok=lambda i: next(guard),
        min_traces=3,
    )
    assert rep.compilable_upper_bound == 2
    assert rep.hot_task_guard_pass_rate == pytest.approx(2 / 3)
