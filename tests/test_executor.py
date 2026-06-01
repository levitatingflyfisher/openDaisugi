"""Unit tests for the pluggable StepExecutor interface.

Subprocess-backed integration lives in tests/test_executor_subprocess.py.
"""

from dataclasses import FrozenInstanceError

import pytest

from opendaisugi.executor import (
    DryRunExecutor,
    ExecutorResult,
    FakeExecutor,
    StepExecutor,
)
from opendaisugi.models import FileReadStep, FileWriteStep, NetworkStep, ShellStep


def test_executor_result_is_frozen():
    r = ExecutorResult(rc=0, stdout="hi", duration_ms=1.0, timed_out=False)
    with pytest.raises(FrozenInstanceError):
        r.rc = 1


def test_dry_run_executor_logs_and_returns_zero():
    executor = DryRunExecutor()
    step = ShellStep(id="s1", command="rm -rf /")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert result.rc == 0
    assert "would shell" in result.stdout.lower()
    assert "rm -rf /" in result.stdout
    assert result.timed_out is False


def test_dry_run_shell():
    executor = DryRunExecutor()
    step = ShellStep(id="s1", command="ls -la")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert "[dry-run] would shell:" in result.stdout
    assert "'ls -la'" in result.stdout  # command repr
    assert result.rc == 0


def test_dry_run_file_read():
    executor = DryRunExecutor()
    step = FileReadStep(id="s1", path="/etc/hosts")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert "[dry-run] would file_read:" in result.stdout
    assert "/etc/hosts" in result.stdout
    assert result.rc == 0


def test_dry_run_file_write():
    executor = DryRunExecutor()
    content = "hello world"
    step = FileWriteStep(id="s1", path="/tmp/out.txt", content=content)
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert "[dry-run] would file_write:" in result.stdout
    assert "/tmp/out.txt" in result.stdout
    assert f"{len(content.encode('utf-8'))} bytes" in result.stdout
    assert result.rc == 0


def test_dry_run_network():
    executor = DryRunExecutor()
    step = NetworkStep(id="s1", url="https://example.com/foo")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert "[dry-run] would network: GET" in result.stdout
    assert "https://example.com/foo" in result.stdout
    assert result.rc == 0


def test_fake_executor_returns_preloaded_result():
    preloaded = ExecutorResult(rc=0, stdout="fake output\n", duration_ms=5.0, timed_out=False)
    executor = FakeExecutor({"echo hi": preloaded})
    step = ShellStep(id="s1", command="echo hi")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert result is preloaded


def test_fake_executor_raises_on_unknown_command():
    executor = FakeExecutor({})
    step = ShellStep(id="s1", command="unexpected")
    with pytest.raises(KeyError, match="unexpected"):
        executor.run(step, timeout_s=30, max_output_bytes=1024)


def test_fake_executor_keys_by_kind():
    shell_res = ExecutorResult(rc=0, stdout="shell-out", duration_ms=0.0, timed_out=False)
    read_res = ExecutorResult(rc=0, stdout="hostfile", duration_ms=0.0, timed_out=False)
    net_res = ExecutorResult(rc=0, stdout="page", duration_ms=0.0, timed_out=False)
    executor = FakeExecutor(mapping={
        "ls -la": shell_res,
        "/etc/hosts": read_res,
        "https://example.com": net_res,
    })
    assert executor.run(
        ShellStep(id="a", command="ls -la"), timeout_s=30, max_output_bytes=1024
    ) is shell_res
    assert executor.run(
        FileReadStep(id="b", path="/etc/hosts"), timeout_s=30, max_output_bytes=1024
    ) is read_res
    assert executor.run(
        NetworkStep(id="c", url="https://example.com"), timeout_s=30, max_output_bytes=1024
    ) is net_res


def test_fake_executor_unknown_key_raises():
    executor = FakeExecutor({})
    step = FileReadStep(id="s1", path="/nope")
    with pytest.raises(KeyError, match="/nope"):
        executor.run(step, timeout_s=30, max_output_bytes=1024)


def test_fake_executor_default_success():
    """When no mapping is provided, FakeExecutor returns rc=0 by default."""
    executor = FakeExecutor(default=ExecutorResult(
        rc=0, stdout="", duration_ms=0.1, timed_out=False,
    ))
    step = ShellStep(id="s1", command="anything")
    result = executor.run(step, timeout_s=30, max_output_bytes=1024)
    assert result.rc == 0


def test_protocol_is_satisfied_by_fake_and_dryrun():
    """Structural typing: both executors should satisfy StepExecutor."""
    assert isinstance(FakeExecutor({}), StepExecutor)
    assert isinstance(DryRunExecutor(), StepExecutor)
