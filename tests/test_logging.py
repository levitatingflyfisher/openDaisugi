"""Structured-logging contract tests (v0.16.0).

Asserts the library logs at named decision points under ``opendaisugi.*``
loggers, and that importing the package does not emit records on its own
(silent-by-default NullHandler idiom).
"""

from __future__ import annotations

import logging

import pytest

from opendaisugi.approval import AllowlistBypassStrategy, DenyStrategy
from opendaisugi.executor import ExecutorResult, FakeExecutor
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify


def _env(allowlist):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=allowlist),
    )


def _plan(*steps):
    return ActionPlan(source="t", task="t", steps=list(steps))


def test_top_level_logger_has_null_handler():
    root = logging.getLogger("opendaisugi")
    assert any(isinstance(h, logging.NullHandler) for h in root.handlers), (
        "opendaisugi must attach a NullHandler so hosts without logging "
        "config still silently import the library"
    )


def test_verify_emits_pass_record(caplog):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    with caplog.at_level(logging.INFO, logger="opendaisugi.verify"):
        result = verify(plan, env)
    assert result.ok
    messages = [r.message for r in caplog.records if r.name == "opendaisugi.verify"]
    assert "verify.pass" in messages


def test_verify_emits_fail_record_with_stages(caplog):
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=False, shell_allowlist=[]),
    )
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verify"):
        result = verify(plan, env)
    assert not result.ok
    fail_records = [
        r for r in caplog.records
        if r.name == "opendaisugi.verify" and r.message == "verify.fail"
    ]
    assert len(fail_records) == 1
    assert fail_records[0].violation_count >= 1
    assert "permissions" in fail_records[0].violation_stages


@pytest.mark.asyncio
async def test_supervisor_emits_run_lifecycle(tmp_path, caplog):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor({"echo hi": ExecutorResult(0, "hi\n", 1.0, False)})
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
    )
    with caplog.at_level(logging.INFO, logger="opendaisugi.supervisor"):
        await sup.run(plan, env)
    messages = [r.message for r in caplog.records if r.name == "opendaisugi.supervisor"]
    assert "run.start" in messages
    assert "run.end" in messages
    end = next(r for r in caplog.records if r.message == "run.end")
    assert end.status == "succeeded"


@pytest.mark.asyncio
async def test_supervisor_logs_verify_rejection(tmp_path, caplog):
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=False, shell_allowlist=[]),
    )
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
    )
    with caplog.at_level(logging.WARNING, logger="opendaisugi.supervisor"):
        await sup.run(plan, env)
    records = [
        r for r in caplog.records
        if r.name == "opendaisugi.supervisor" and r.message == "run.rejected_by_verify"
    ]
    assert len(records) == 1
    assert records[0].violation_count >= 1


@pytest.mark.asyncio
async def test_supervisor_logs_approval_denial(tmp_path, caplog):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=DenyStrategy(),
        journal=Journal(data_dir=tmp_path),
    )
    with caplog.at_level(logging.WARNING, logger="opendaisugi.supervisor"):
        await sup.run(plan, env)
    records = [
        r for r in caplog.records
        if r.name == "opendaisugi.supervisor" and r.message == "run.approval_denied"
    ]
    assert len(records) == 1
    assert records[0].step_id == "s1"


def test_contracts_logger_emits_on_delegation(caplog):
    from opendaisugi.contracts import Contract, verify_delegation

    env = _env(["echo"])
    contract = Contract(
        contract_id="c1", skill_id="s1", version="0.1.0",
        envelope=env,
    )
    with caplog.at_level(logging.INFO, logger="opendaisugi.contracts"):
        decision = verify_delegation(env, contract)
    assert decision.allowed
    messages = [r.message for r in caplog.records if r.name == "opendaisugi.contracts"]
    assert "delegation.allow" in messages
