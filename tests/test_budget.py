"""Tests for the live token-budget tracker (orchestrator, v0.32)."""

from __future__ import annotations

import math

import pytest

from opendaisugi.budget import BudgetExceeded, BudgetTracker, StepCost


def test_unlimited_budget_never_exhausts():
    b = BudgetTracker(total_tokens=None)
    assert b.total is None
    assert math.isinf(b.remaining())
    assert b.can_afford(10_000_000)
    assert not b.exhausted()


def test_records_spend_and_tracks_remaining():
    b = BudgetTracker(total_tokens=1000)
    assert b.remaining() == 1000
    b.record(step_id="s1", model="haiku", tokens=300)
    assert b.spent() == 300
    assert b.remaining() == 700
    assert b.can_afford(700)
    assert not b.can_afford(701)


def test_exhaustion_clamps_at_zero():
    b = BudgetTracker(total_tokens=1000)
    b.record(step_id="s1", model="haiku", tokens=1000)
    assert b.remaining() == 0
    assert b.exhausted()
    # can never go negative even if an actual overshoots the estimate
    b.record(step_id="s2", model="opus", tokens=500)
    assert b.remaining() == 0
    assert b.spent() == 1500


def test_report_remaining_is_none_when_unlimited():
    # JSON-safe: an unlimited budget reports remaining=None, not float('inf').
    b = BudgetTracker(total_tokens=None)
    rep = b.report()
    assert rep.total is None
    assert rep.remaining is None


def test_costs_and_by_model_breakdown():
    b = BudgetTracker(total_tokens=5000)
    b.record(step_id="s1", model="haiku", tokens=200)
    b.record(step_id="s2", model="opus", tokens=900)
    b.record(step_id="s3", model="haiku", tokens=100)
    assert b.costs() == [
        StepCost(step_id="s1", model="haiku", tokens=200),
        StepCost(step_id="s2", model="opus", tokens=900),
        StepCost(step_id="s3", model="haiku", tokens=100),
    ]
    assert b.by_model() == {"haiku": 300, "opus": 900}


def test_negative_tokens_rejected():
    b = BudgetTracker(total_tokens=1000)
    with pytest.raises(ValueError):
        b.record(step_id="s1", model="haiku", tokens=-5)


def test_report_snapshot():
    b = BudgetTracker(total_tokens=1000)
    b.record(step_id="s1", model="haiku", tokens=400)
    rep = b.report()
    assert rep.total == 1000
    assert rep.spent == 400
    assert rep.remaining == 600
    assert rep.by_model == {"haiku": 400}
    assert rep.step_count == 1


def test_reserve_raises_when_over_budget_in_strict_mode():
    b = BudgetTracker(total_tokens=100, strict=True)
    b.record(step_id="s1", model="haiku", tokens=90)
    # A record that blows the strict ceiling raises rather than silently overshooting.
    with pytest.raises(BudgetExceeded):
        b.record(step_id="s2", model="opus", tokens=50)


def test_non_strict_allows_overshoot():
    b = BudgetTracker(total_tokens=100)  # strict defaults False
    b.record(step_id="s1", model="haiku", tokens=90)
    b.record(step_id="s2", model="opus", tokens=50)  # no raise
    assert b.spent() == 140


def test_approx_cost_usd_blends_model_prices():
    b = BudgetTracker(total_tokens=None)
    b.record(step_id="s1", model="claude-opus-4-8", tokens=1_000_000)   # $22/Mtok
    b.record(step_id="s2", model="claude-haiku-4-5", tokens=1_000_000)  # $1.5/Mtok
    b.record(step_id="s3", model="openai/local-model", tokens=5_000_000)  # local → $0
    assert b.approx_cost_usd() == 23.5
    assert b.report().approx_cost_usd == 23.5


def test_approx_cost_price_table_is_overridable():
    b = BudgetTracker(price_table={"tiny": 0.1})
    b.record(step_id="s1", model="tiny-local", tokens=2_000_000)
    assert b.approx_cost_usd() == 0.2


def test_measured_cost_is_exact_when_backend_reports_it():
    b = BudgetTracker()
    b.record(step_id="s1", model="claude-haiku-4-5", tokens=72, cost_usd=0.0207)
    b.record(step_id="s2", model="claude-opus-4-8", tokens=500, cost_usd=0.15)
    assert b.measured_cost_usd() == 0.1707
    assert b.report().measured_cost_usd == 0.1707


def test_measured_cost_is_none_without_backend_costs():
    b = BudgetTracker()
    b.record(step_id="s1", model="haiku", tokens=100)  # no cost_usd
    assert b.measured_cost_usd() is None
    assert b.report().measured_cost_usd is None
