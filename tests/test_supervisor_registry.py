"""Default executor registry wiring for Supervisor.

Covers the ``default_executors()`` factory and ``Supervisor`` dispatch
via the executor registry (Task 6).
"""

from opendaisugi.executor import (
    ExecutorResult,
    FakeExecutor,
    FileReadExecutor,
    FileWriteExecutor,
    NetworkExecutor,
    StepExecutor,
    SubprocessExecutor,
    default_executors,
)
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    Permission,
    ShellStep,
)
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


def test_default_executors_wires_all_four_kinds():
    reg = default_executors()
    assert set(reg.keys()) == {"shell", "file_read", "file_write", "network"}
    assert isinstance(reg["shell"], SubprocessExecutor)
    assert isinstance(reg["file_read"], FileReadExecutor)
    assert isinstance(reg["file_write"], FileWriteExecutor)
    assert isinstance(reg["network"], NetworkExecutor)


def test_default_executors_returns_fresh_instances():
    reg_a = default_executors()
    reg_b = default_executors()
    assert reg_a["shell"] is not reg_b["shell"]


def test_default_executors_values_implement_protocol():
    reg = default_executors()
    for value in reg.values():
        assert isinstance(value, StepExecutor)


# --- Supervisor registry dispatch (Task 6) ---------------------------------


def test_supervisor_uses_default_registry_when_none_passed():
    """``Supervisor()`` with no ``executors=`` wires ``default_executors()``."""
    sup = Supervisor()
    assert set(sup._executors.keys()) == {"shell", "file_read", "file_write", "network"}
    assert isinstance(sup._executors["shell"], SubprocessExecutor)


async def test_supervisor_custom_registry_overrides(monkeypatch):
    """A per-kind registry entry overrides the default for that kind."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    fake = FakeExecutor({
        "echo hi": ExecutorResult(rc=0, stdout="mocked", duration_ms=0.0, timed_out=False),
    })
    sup = Supervisor(executors={"shell": fake})
    session = await sup.run(plan, env)
    assert session.status == RunStatus.SUCCEEDED
    assert session.steps[0].stdout == "mocked"


async def test_supervisor_unknown_step_kind_returns_error_result(monkeypatch):
    """Defensive branch: a registry missing a kind records ``no executor for kind ...``.

    Normally unreachable — verify rejects unknown kinds upstream — but this
    asserts the dispatch boundary fails closed if verify is bypassed.
    """
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_read=["/tmp/*"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        FileReadStep(id="s1", path="/tmp/whatever"),
    ])
    # Registry deliberately omits "file_read".
    shell_fake = FakeExecutor(default=ExecutorResult(rc=0, stdout="", duration_ms=0.0, timed_out=False))
    sup = Supervisor(executors={"shell": shell_fake})
    session = await sup.run(plan, env)
    assert session.status == RunStatus.FAILED
    assert session.steps[0].rc == 1
    assert "no executor for kind 'file_read'" in (session.steps[0].stdout or "")
