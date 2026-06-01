"""Tests for v0.2.0 additions to RunStatus and StepOutcome."""

from opendaisugi.run_session import RunStatus, StepOutcome


def test_halted_by_simplex_exists():
    assert RunStatus.HALTED_BY_SIMPLEX.value == "halted_by_simplex"


def test_halted_by_simplex_is_distinct():
    statuses = [s.value for s in RunStatus]
    assert "halted_by_simplex" in statuses
    assert statuses.count("halted_by_simplex") == 1


def test_step_outcome_rejected_halted():
    outcome = StepOutcome(
        step_id="s1",
        status="rejected_halted",
        approved_by="allowlist",
        rc=None,
        stdout="",
        duration_ms=0.0,
        started_at="2026-04-16T00:00:00Z",
        error="step violated envelope",
    )
    assert outcome.status == "rejected_halted"


def test_step_outcome_rejected_recomputed():
    outcome = StepOutcome(
        step_id="s1",
        status="rejected_recomputed",
        approved_by="allowlist",
        rc=None,
        stdout="",
        duration_ms=0.0,
        started_at="2026-04-16T00:00:00Z",
        error=None,
    )
    assert outcome.status == "rejected_recomputed"
