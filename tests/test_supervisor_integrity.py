"""Run-end integrity check: no silent step-skipping (v0.18 L4)."""
from pathlib import Path

import pytest

from opendaisugi.executor import DryRunExecutor, ExecutorResult
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


def _env() -> Envelope:
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=["echo"]))


@pytest.mark.asyncio
async def test_integrity_passes_when_all_steps_receipted(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    sup = Supervisor(executors={"shell": DryRunExecutor()}, journal=j)
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a"),
        ShellStep(id="s2", command="echo b", depends_on=["s1"]),
    ])
    session = await sup.run(plan, _env())
    assert session.status == RunStatus.SUCCEEDED
    assert session.integrity_passed is True


@pytest.mark.asyncio
async def test_integrity_fails_on_silent_skip(tmp_path: Path):
    """If a receipt for an expected step is missing, integrity must fail."""
    j = Journal(data_dir=tmp_path)
    # Monkey-patch append_receipt so s2's receipt is silently dropped —
    # simulates a misbehaving cheap sub-agent that claims success.
    original_append = j.append_receipt

    def filtering_append(receipt):
        if receipt.step_id == "s2":
            return
        original_append(receipt)

    j.append_receipt = filtering_append

    sup = Supervisor(executors={"shell": DryRunExecutor()}, journal=j)
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a"),
        ShellStep(id="s2", command="echo b", depends_on=["s1"]),
    ])
    session = await sup.run(plan, _env())
    assert session.status == RunStatus.SUCCEEDED   # supervisor thinks it worked
    assert session.integrity_passed is False        # but the check catches it


@pytest.mark.asyncio
async def test_approval_strategy_exception_aborts_cleanly(tmp_path: Path):
    """A custom approval strategy that raises must not crash the supervisor.
    The run is marked ABORTED with a clean error message and the integrity
    check finishes."""
    from opendaisugi.approval import ApprovalDecision

    class RaisingApproval:
        def decide(self, step, env):
            raise RuntimeError("approval backend exploded")

    j = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": DryRunExecutor()},
        journal=j,
        approval=RaisingApproval(),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a"),
    ])
    session = await sup.run(plan, _env())
    assert session.status == RunStatus.ABORTED
    # Should not have crashed; integrity check ran without journal contamination.
    assert session.integrity_passed is True


@pytest.mark.asyncio
async def test_integrity_accepts_halt_on_failure_partial(tmp_path: Path):
    """Halt-on-failure: receipts only for 1..failing-step are expected.
    Steps after the failure point legitimately unrun — not a violation.
    """
    j = Journal(data_dir=tmp_path)

    class FailingOnS2:
        def run(self, step, *, timeout_s, max_output_bytes):
            if step.id == "s2":
                return ExecutorResult(rc=1, stdout="boom", duration_ms=0.0, timed_out=False)
            return ExecutorResult(rc=0, stdout="", duration_ms=0.0, timed_out=False)

    sup = Supervisor(executors={"shell": FailingOnS2()}, journal=j)
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a"),
        ShellStep(id="s2", command="echo b", depends_on=["s1"]),
        ShellStep(id="s3", command="echo c", depends_on=["s2"]),
    ])
    session = await sup.run(plan, _env())
    assert session.status == RunStatus.FAILED
    assert session.failed_step_id == "s2"
    # s3 legitimately unreached; integrity should NOT flag this as violation
    assert session.integrity_passed is True
