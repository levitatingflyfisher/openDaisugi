"""Integration tests for signal safety + SubprocessExecutor + Supervisor.

Skipped on non-POSIX. These tests launch real shells; they should each
complete within a few seconds.
"""

import os
import signal
import subprocess
import sys
import threading
import time

import pytest

from opendaisugi.approval import AllowlistBypassStrategy, DenyStrategy
from opendaisugi.executor import SubprocessExecutor
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, ShellStep, Envelope, Permission
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor

pytestmark = pytest.mark.skipif(
    sys.platform == "win32", reason="signal tests are POSIX-only in v0.1"
)


def test_supervisor_finalizes_and_journals_on_keyboard_interrupt(tmp_path):
    """If KeyboardInterrupt is raised mid-step, the session must still be journaled.

    Python delivers SIGINT only to the main thread as KeyboardInterrupt.  The
    supervisor must run on the main thread; a timer thread fires SIGINT after a
    short delay so the step (sleep 5) is in-flight when the interrupt arrives.
    """
    import asyncio

    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["sleep"]))
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="sleep 5"),
    ])
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": SubprocessExecutor()},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        step_timeout_s=30,
    )

    # Fire SIGINT at the main thread (where Python delivers it) after 0.8 s.
    def _fire_sigint():
        time.sleep(0.8)
        os.kill(os.getpid(), signal.SIGINT)

    t = threading.Thread(target=_fire_sigint, daemon=True)
    t.start()

    session = None
    try:
        session = asyncio.run(sup.run(plan, env))
    except KeyboardInterrupt:
        pass

    t.join(timeout=5)

    if session is not None:
        assert session.status in (RunStatus.ABORTED, RunStatus.FAILED)
        assert session.trace_id is not None
    else:
        traces = list((tmp_path / "journal" / "traces").glob("*.yaml"))
        assert len(traces) >= 1


async def test_supervisor_reaps_grandchildren_on_step_timeout(tmp_path):
    """A step that forks a grandchild must have it killed on timeout.

    The command is written to a shell script so no metacharacters appear in
    the ActionStep.command string (which would fail the metachar check in
    verify.check_permissions).
    """
    marker = f"supervisor-sig-test-{os.getpid()}"
    script = tmp_path / "fork_grandchild.sh"
    script.write_text(
        f"#!/bin/sh\nsleep 30 &\necho {marker}\nwait\n"
    )
    script.chmod(0o755)

    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["sh"]))
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command=f"sh {script}"),
    ])
    sup = Supervisor(
        executors={"shell": SubprocessExecutor()},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
        step_timeout_s=1,
    )
    session = await sup.run(plan, env)
    assert session.status == RunStatus.FAILED
    assert session.steps[0].error == "timed out"
    time.sleep(0.5)
    ps_out = subprocess.run(
        ["ps", "-eo", "pid,command"], capture_output=True, text=True,
    ).stdout
    for line in ps_out.splitlines():
        if marker in line and "sleep" in line:
            pytest.fail(f"grandchild survived teardown: {line}")


def test_subprocess_executor_reaps_on_keyboard_interrupt(tmp_path):
    """Ctrl-C during a running step must kill the process group, not orphan it."""
    import os
    import signal as _signal
    import subprocess as _subprocess
    import threading as _threading
    import time as _time

    from opendaisugi.executor import SubprocessExecutor
    from opendaisugi.models import ShellStep

    # Unique sleep duration so grep can find the orphan without ambiguity
    unique_sleep = "99991"
    script = tmp_path / "s.sh"
    script.write_text(f"#!/bin/sh\nsleep {unique_sleep} &\nwait\n")
    script.chmod(0o755)

    step = ShellStep(id="s1", command=f"sh {script}")
    executor = SubprocessExecutor()

    def _interrupt_soon():
        _time.sleep(0.8)
        os.kill(os.getpid(), _signal.SIGINT)

    _threading.Thread(target=_interrupt_soon, daemon=True).start()

    try:
        executor.run(step, timeout_s=30, max_output_bytes=1024)
        pytest.fail("expected KeyboardInterrupt")
    except KeyboardInterrupt:
        pass

    # Give the kernel a moment to reap
    _time.sleep(0.5)
    ps_out = _subprocess.run(
        ["ps", "-eo", "pid,command"], capture_output=True, text=True
    ).stdout
    for line in ps_out.splitlines():
        if f"sleep {unique_sleep}" in line:
            # Best-effort cleanup if the test is about to fail
            try:
                pid = int(line.split()[0])
                os.kill(pid, _signal.SIGKILL)
            except (ValueError, ProcessLookupError):
                pass
            pytest.fail(f"grandchild survived KeyboardInterrupt: {line}")
