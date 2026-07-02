"""Integration tests for SubprocessExecutor — runs real /bin/sh commands.

Skipped on non-POSIX platforms since v0.1 is POSIX-only.
"""

import os
import subprocess
import sys
import time

import pytest

from opendaisugi.executor import SubprocessExecutor
from opendaisugi.models import ShellStep

pytestmark = pytest.mark.skipif(
    sys.platform == "win32", reason="SubprocessExecutor is POSIX-only in v0.1"
)


def _step(cmd: str) -> ShellStep:
    return ShellStep(id="s1", command=cmd)


def test_subprocess_executor_runs_echo():
    ex = SubprocessExecutor()
    result = ex.run(_step("echo hi"), timeout_s=5, max_output_bytes=1024)
    assert result.rc == 0
    assert "hi" in result.stdout
    assert result.timed_out is False
    assert result.duration_ms > 0


def test_subprocess_executor_captures_nonzero_rc():
    ex = SubprocessExecutor()
    result = ex.run(_step("exit 7"), timeout_s=5, max_output_bytes=1024)
    assert result.rc == 7
    assert result.timed_out is False


def test_subprocess_executor_merges_stderr_into_stdout():
    ex = SubprocessExecutor()
    result = ex.run(
        _step("echo out; echo err >&2"),
        timeout_s=5, max_output_bytes=1024,
    )
    assert "out" in result.stdout
    assert "err" in result.stdout


def test_subprocess_executor_enforces_timeout():
    ex = SubprocessExecutor()
    start = time.monotonic()
    result = ex.run(_step("sleep 10"), timeout_s=1, max_output_bytes=1024)
    elapsed = time.monotonic() - start
    assert result.timed_out is True
    assert elapsed < 5.0  # teardown should be quick


def test_subprocess_executor_reaps_grandchildren_on_timeout():
    """The shell's grandchild (sleep) must not outlive the supervisor's kill.

    Start bash that spawns a subshell sleep; timeout the parent; verify the
    grandchild pid is no longer alive within a reasonable window.
    """
    ex = SubprocessExecutor()
    marker = f"opendaisugi-gc-test-{os.getpid()}"
    cmd = f'sh -c "sleep 30 & echo {marker}; wait"'
    result = ex.run(_step(cmd), timeout_s=1, max_output_bytes=1024)
    assert result.timed_out is True
    # Give the OS a moment to reap
    time.sleep(0.5)
    ps_out = subprocess.run(
        ["ps", "-eo", "pid,command"], capture_output=True, text=True,
    ).stdout
    for line in ps_out.splitlines():
        if marker in line and "sleep" in line:
            pytest.fail(f"grandchild survived teardown: {line}")


def test_subprocess_executor_truncates_large_output():
    ex = SubprocessExecutor()
    # Generate 2 KB of output, truncate to 512 bytes
    result = ex.run(
        _step("python3 -c 'print(\"x\" * 2048)'"),
        timeout_s=5, max_output_bytes=512,
    )
    assert len(result.stdout.encode()) <= 512 + len("\n... [truncated]")
    assert "[truncated]" in result.stdout


def test_subprocess_executor_satisfies_protocol():
    from opendaisugi.executor import StepExecutor
    assert isinstance(SubprocessExecutor(), StepExecutor)


def test_subprocess_output_is_bounded_against_flood():
    # EB-3: an allowlisted-but-noisy command that emits unbounded output must be
    # capped in memory (not buffered whole via communicate) and killed — this must
    # return quickly, not hang or OOM.
    import time as _t
    from opendaisugi.executor import SubprocessExecutor
    from opendaisugi.models import ShellStep
    exe = SubprocessExecutor()
    start = _t.monotonic()
    # `yes` emits "y\n" forever; cap at 4KB.
    r = exe.run(ShellStep(id="s", command="yes"), timeout_s=10, max_output_bytes=4096)
    elapsed = _t.monotonic() - start
    assert len(r.stdout.encode()) <= 4096 + len("\n... [truncated]")
    assert "[truncated]" in r.stdout
    assert elapsed < 8  # bounded + killed promptly, nowhere near the 10s timeout
