"""Supervisor + DelegatingExecutor integration: Receipt.model_id flows (v0.19 L6)."""
from pathlib import Path
from unittest.mock import patch

import pytest

from opendaisugi.approval import CallbackStrategy
from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.supervisor import Supervisor


def _env() -> Envelope:
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


@pytest.mark.asyncio
async def test_supervisor_stamps_receipt_model_id_from_delegating_executor(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    exe = DelegatingExecutor(default_model="haiku")
    sup = Supervisor(
        executors={"shell": exe},
        journal=j,
        approval=CallbackStrategy(lambda s, e: True),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a", preferred_model="sonnet"),
    ])
    with patch.object(exe, "_call", return_value='{"ok": true}'):
        session = await sup.run(plan, _env())
    receipts = j.receipts_for_run(session.id)
    assert len(receipts) == 1
    assert receipts[0].model_id == "sonnet"


@pytest.mark.asyncio
async def test_supervisor_leaves_model_id_none_for_non_delegating_executor(tmp_path: Path):
    """A normal (non-LLM) executor leaves Receipt.model_id as None."""
    from opendaisugi.executor import DryRunExecutor

    j = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": DryRunExecutor()},
        journal=j,
        approval=CallbackStrategy(lambda s, e: True),
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo a")])
    session = await sup.run(plan, _env())
    receipts = j.receipts_for_run(session.id)
    assert len(receipts) == 1
    assert receipts[0].model_id is None



async def test_eb7_recomputed_step_is_reverified_and_halts_when_out_of_policy():
    # EB-7 coverage: the recompute path is hard to reach statically (whole-plan
    # verify() gates first), so patch verify_step to fail per-step while the
    # whole-plan verify still passes. A recompute fallback returns a replacement;
    # the EB-7 re-verify must reject it and HALT — never execute it.
    from opendaisugi.models import VerificationResult, Violation
    from opendaisugi.fallback import FallbackOutcome
    from opendaisugi.run_session import RunStatus
    from opendaisugi.executor import FakeExecutor, ExecutorResult

    executed = {"n": 0}

    class _RecordingExec:
        def run(self, step, *, timeout_s, max_output_bytes):
            executed["n"] += 1
            return ExecutorResult(rc=0, stdout="RAN", duration_ms=0.0, timed_out=False)

    class _Recompute:
        async def handle(self, step, result, envelope):
            return FallbackOutcome(
                action="recomputed",
                replacement_step=ShellStep(id=step.id, command="echo replacement"),
                replacement_result=VerificationResult(ok=True, envelope_id="e", plan_id="p", duration_ms=0.0),
            )

    env = Envelope(generated_by="t", task="x",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(source="t", task="x", steps=[ShellStep(id="s1", command="echo hi")])
    bad = VerificationResult(ok=False, envelope_id="e", plan_id="p", duration_ms=0.0,
                             violations=[Violation(stage="permissions", message="forced per-step failure")])
    sup = Supervisor(executors={"shell": _RecordingExec()},
                     approval=CallbackStrategy(lambda s, e: True), fallback=_Recompute())
    # whole-plan verify() runs real (plan is in-policy → loop entered); per-step
    # verify_step always fails → recompute → EB-7 re-verify also fails → halt.
    with patch("opendaisugi.supervisor.verify_step", return_value=bad):
        session = await sup.run(plan, env)
    assert session.status == RunStatus.HALTED_BY_SIMPLEX
    assert executed["n"] == 0  # the recomputed replacement never executed
