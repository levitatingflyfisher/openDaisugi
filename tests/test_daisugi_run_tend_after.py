"""Daisugi.run() + tend_after:
- Daisugi.run() delegates to Supervisor and returns a RunSession
- tend_after=N triggers tend() after N successive SUCCEEDED runs
- Failed/rejected runs do not count toward the threshold
- tend() resets the counter so every N successes fires once
"""
from __future__ import annotations

import asyncio
from unittest import mock

import pytest

from opendaisugi import Daisugi
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
)
from opendaisugi.run_session import RunSession, RunStatus


def _plan():
    return ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])


def _env():
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
    )


def _session(status: RunStatus) -> RunSession:
    env = _env()
    plan = _plan()
    return RunSession(
        id="run_x",
        envelope_id=env.id,
        plan_id=plan.id,
        status=status,
        verification=mock.MagicMock(ok=True, violations=[]),
        steps=[],
        started_at="2026-01-01T00:00:00Z",
        ended_at="2026-01-01T00:00:01Z",
        trace_id=None,
    )


@pytest.mark.asyncio
async def test_daisugi_run_returns_session(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)
    succeeded = _session(RunStatus.SUCCEEDED)

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(return_value=succeeded)
        session = await d.run(_plan(), _env())

    assert session.status == RunStatus.SUCCEEDED


@pytest.mark.asyncio
async def test_tend_after_triggers_on_nth_success(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False, tend_after=3)
    succeeded = _session(RunStatus.SUCCEEDED)

    tend_calls = []

    async def fake_tend(**_):
        tend_calls.append(1)
        return mock.MagicMock()

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(return_value=succeeded)
        with mock.patch.object(d, "tend", side_effect=fake_tend):
            await d.run(_plan(), _env())
            await d.run(_plan(), _env())
            assert tend_calls == [], "should not tend before threshold"
            await d.run(_plan(), _env())
            assert tend_calls == [1], "should tend exactly once after 3 successes"


@pytest.mark.asyncio
async def test_failed_run_does_not_count(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False, tend_after=2)
    succeeded = _session(RunStatus.SUCCEEDED)
    failed = _session(RunStatus.FAILED)

    tend_calls = []

    async def fake_tend(**_):
        tend_calls.append(1)
        return mock.MagicMock()

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(side_effect=[succeeded, failed, succeeded])
        with mock.patch.object(d, "tend", side_effect=fake_tend):
            await d.run(_plan(), _env())  # success #1
            await d.run(_plan(), _env())  # fail — counter stays at 1
            await d.run(_plan(), _env())  # success #2 → triggers
            assert tend_calls == [1]


@pytest.mark.asyncio
async def test_tend_resets_counter(tmp_path):
    """After tend() fires at N, the counter resets — next N successes fire again."""
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False, tend_after=2)
    succeeded = _session(RunStatus.SUCCEEDED)

    tend_calls = []

    async def fake_tend(**_):
        tend_calls.append(1)
        return mock.MagicMock()

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(return_value=succeeded)
        with mock.patch.object(d, "tend", side_effect=fake_tend):
            for _ in range(4):
                await d.run(_plan(), _env())
    assert tend_calls == [1, 1], "should tend twice for 4 successes at threshold=2"


@pytest.mark.asyncio
async def test_no_tend_after_means_never_auto_tend(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)  # tend_after=None
    succeeded = _session(RunStatus.SUCCEEDED)

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(return_value=succeeded)
        with mock.patch.object(d, "tend") as mock_tend:
            for _ in range(10):
                await d.run(_plan(), _env())
    mock_tend.assert_not_called()


@pytest.mark.asyncio
async def test_v028_4_tend_failure_does_not_fail_run(tmp_path):
    """v0.28.4 — pre-fix, a tend() exception (LLM down, sqlite locked,
    embedder missing) would bubble out of Daisugi.run and surface as
    if the supervised run itself failed, despite the run having
    journaled successfully. Now the exception is swallowed with a log.
    """
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False, tend_after=1)
    succeeded = _session(RunStatus.SUCCEEDED)

    with mock.patch("opendaisugi.Supervisor") as MockSup:
        MockSup.return_value.run = mock.AsyncMock(return_value=succeeded)
        with mock.patch.object(d, "tend", side_effect=RuntimeError("embedder boom")):
            session = await d.run(_plan(), _env())
    assert session is succeeded, "tend() failure must not corrupt session return"
    assert session.status == RunStatus.SUCCEEDED
