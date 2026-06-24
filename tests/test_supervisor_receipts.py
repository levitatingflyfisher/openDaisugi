"""Supervisor writes a Receipt for every executed step (v0.18 L3)."""
from pathlib import Path

import pytest

from opendaisugi.executor import DryRunExecutor
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
)
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


def _env(allowlist: list[str] | None = None) -> Envelope:
    return Envelope(
        generated_by="test", task="test",
        permissions=Permission(shell=True, shell_allowlist=allowlist or ["echo"]),
    )


@pytest.mark.asyncio
async def test_supervisor_writes_receipt_for_each_successful_step(tmp_path: Path):
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": DryRunExecutor()},
        journal=journal,
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo a"),
        ShellStep(id="s2", command="echo b", depends_on=["s1"]),
    ])
    session = await sup.run(plan, _env())
    assert session.status == RunStatus.SUCCEEDED
    receipts = journal.receipts_for_run(session.id)
    assert {r.step_id for r in receipts} == {"s1", "s2"}
    for r in receipts:
        assert r.verify_result is True
        assert r.run_id == session.id
        assert len(r.evidence_hash) == 64  # sha256


@pytest.mark.asyncio
async def test_receipt_evidence_carries_execution_output(tmp_path: Path):
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": DryRunExecutor()},
        journal=journal,
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hello"),
    ])
    session = await sup.run(plan, _env())
    receipts = journal.receipts_for_run(session.id)
    assert len(receipts) == 1
    r = receipts[0]
    # Evidence carries rc, stdout (at minimum)
    assert "rc" in r.evidence
    assert r.evidence["rc"] == 0
