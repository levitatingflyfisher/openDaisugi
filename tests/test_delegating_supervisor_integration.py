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

