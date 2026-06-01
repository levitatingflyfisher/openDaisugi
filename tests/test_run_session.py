from dataclasses import FrozenInstanceError

import pytest

from opendaisugi.exceptions import (
    ApprovalDeniedError,
    NotTerminalError,
    OpenDaisugiError,
    StepExecutionError,
    SupervisorAborted,
)
from opendaisugi.models import VerificationResult
from opendaisugi.run_session import RunSession, RunStatus, StepOutcome


def test_supervisor_exceptions_inherit_from_base():
    for exc_cls in (
        StepExecutionError,
        ApprovalDeniedError,
        SupervisorAborted,
        NotTerminalError,
    ):
        assert issubclass(exc_cls, OpenDaisugiError)
        # Each exception is constructible with a message
        instance = exc_cls("test message")
        assert str(instance) == "test message"


def test_run_status_values():
    assert RunStatus.PENDING.value == "pending"
    assert RunStatus.REJECTED.value == "rejected"
    assert RunStatus.RUNNING.value == "running"
    assert RunStatus.SUCCEEDED.value == "succeeded"
    assert RunStatus.FAILED.value == "failed"
    assert RunStatus.ABORTED.value == "aborted"


def test_step_outcome_is_frozen():
    outcome = StepOutcome(
        step_id="s1", status="succeeded", approved_by="allowlist",
        rc=0, stdout="hi\n", duration_ms=12.5,
        started_at="2026-04-13T10:00:00Z", error=None,
    )
    with pytest.raises(FrozenInstanceError):
        outcome.rc = 1
    assert outcome.step_id == "s1"
    assert outcome.status == "succeeded"


def test_run_session_mutable_status_and_steps():
    verification = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id="env_x", plan_id="plan_x", duration_ms=1.0,
    )
    session = RunSession(
        id="run_abcd1234",
        envelope_id="env_x",
        plan_id="plan_x",
        status=RunStatus.PENDING,
        verification=verification,
        steps=[],
        started_at="2026-04-13T10:00:00Z",
        ended_at=None,
        trace_id=None,
    )
    session.status = RunStatus.RUNNING
    session.steps.append(StepOutcome(
        step_id="s1", status="succeeded", approved_by="allowlist",
        rc=0, stdout="", duration_ms=1.0,
        started_at="2026-04-13T10:00:00Z", error=None,
    ))
    assert session.status == RunStatus.RUNNING
    assert len(session.steps) == 1
