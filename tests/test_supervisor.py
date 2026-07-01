"""Unit tests for Supervisor using FakeExecutor for determinism."""


from opendaisugi.approval import AllowlistBypassStrategy, DenyStrategy
from opendaisugi.executor import ExecutorResult, FakeExecutor
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


def _env(allowlist):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=allowlist),
    )


def _plan(*steps):
    return ActionPlan(source="t", task="t", steps=list(steps))


async def test_supervisor_rejects_unverifiable_plan(tmp_path):
    # Disable shell permission so verify() will flag the shell step
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=False, shell_allowlist=[]))
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
    )
    session = await sup.run(plan, env)
    assert session.status == RunStatus.REJECTED
    assert session.verification.ok is False
    assert len(session.steps) == 0


async def test_supervisor_runs_allowlisted_steps_to_succeeded(tmp_path):
    env = _env(["echo"])
    plan = _plan(
        ShellStep(id="s1", command="echo hi"),
        ShellStep(id="s2", command="echo bye", depends_on=["s1"]),
    )
    executor = FakeExecutor({
        "echo hi": ExecutorResult(0, "hi\n", 1.0, False),
        "echo bye": ExecutorResult(0, "bye\n", 1.0, False),
    })
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
    )
    session = await sup.run(plan, env)
    assert session.status == RunStatus.SUCCEEDED
    assert [s.step_id for s in session.steps] == ["s1", "s2"]
    assert all(s.status == "succeeded" for s in session.steps)
    assert all(s.approved_by == "allowlist" for s in session.steps)


async def test_supervisor_marks_failed_on_nonzero_rc(tmp_path):
    env = _env(["false"])
    plan = _plan(
        ShellStep(id="s1", command="false"),
        ShellStep(id="s2", command="false", depends_on=["s1"]),
    )
    executor = FakeExecutor({
        "false": ExecutorResult(1, "", 1.0, False),
    })
    sup = Supervisor(executors={"shell": executor},
                    approval=AllowlistBypassStrategy(DenyStrategy()),
                    journal=Journal(data_dir=tmp_path))
    session = await sup.run(plan, env)
    assert session.status == RunStatus.FAILED
    assert len(session.steps) == 1
    assert session.steps[0].status == "failed"
    assert session.steps[0].rc == 1


async def test_supervisor_aborts_on_approval_denied(tmp_path):
    env = _env(["echo"])  # echo is allowlisted so verify passes
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor(default=ExecutorResult(0, "", 0.1, False))
    sup = Supervisor(executors={"shell": executor},
                    approval=DenyStrategy(),  # denies every step
                    journal=Journal(data_dir=tmp_path))
    session = await sup.run(plan, env)
    assert session.status == RunStatus.ABORTED
    assert len(session.steps) == 1
    assert session.steps[0].status == "aborted"
    assert session.steps[0].approved_by == "denied"


async def test_supervisor_marks_timed_out_step_as_failed(tmp_path):
    env = _env(["sleep"])
    plan = _plan(ShellStep(id="s1", command="sleep 100"))
    executor = FakeExecutor({
        "sleep 100": ExecutorResult(-1, "", 1000.0, True),
    })
    sup = Supervisor(executors={"shell": executor},
                    approval=AllowlistBypassStrategy(DenyStrategy()),
                    journal=Journal(data_dir=tmp_path))
    session = await sup.run(plan, env)
    assert session.status == RunStatus.FAILED
    assert session.steps[0].status == "failed"
    assert session.steps[0].error == "timed out"


async def test_supervisor_logs_session_to_journal(tmp_path):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor({"echo hi": ExecutorResult(0, "hi", 1.0, False)})
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(executors={"shell": executor},
                    approval=AllowlistBypassStrategy(DenyStrategy()),
                    journal=journal)
    session = await sup.run(plan, env)
    assert session.trace_id is not None
    loaded = journal.load_run(session.trace_id)
    assert loaded.status == RunStatus.SUCCEEDED
    assert loaded.id == session.id


async def test_supervisor_without_journal_still_runs(tmp_path):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor({"echo hi": ExecutorResult(0, "hi", 1.0, False)})
    sup = Supervisor(executors={"shell": executor},
                    approval=AllowlistBypassStrategy(DenyStrategy()),
                    journal=None)
    session = await sup.run(plan, env)
    assert session.status == RunStatus.SUCCEEDED
    assert session.trace_id is None


async def test_supervisor_session_id_format(tmp_path):
    env = _env(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor({"echo hi": ExecutorResult(0, "hi", 1.0, False)})
    sup = Supervisor(executors={"shell": executor},
                    approval=AllowlistBypassStrategy(DenyStrategy()),
                    journal=None)
    session = await sup.run(plan, env)
    assert session.id.startswith("run_")
    assert len(session.id) == len("run_") + 8
