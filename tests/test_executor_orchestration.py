"""Executor plumbing for orchestration step types (v0.32)."""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import patch

import pytest

from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.executor import DryRunExecutor, ExecutorResult, FakeExecutor
from opendaisugi.models import MCPStep, ShellStep, SkillStep, TaskStep


def test_dry_run_handles_task_skill_mcp():
    dry = DryRunExecutor()
    for step, needle in [
        (TaskStep(id="t1", prompt="do a thing"), "task"),
        (SkillStep(id="k1", skill_id="tidy"), "skill"),
        (MCPStep(id="m1", server="gh", tool="list"), "mcp"),
    ]:
        r = dry.run(step, timeout_s=1, max_output_bytes=1024)
        assert r.rc == 0
        assert needle in r.stdout.lower()


def test_fake_executor_keys_new_step_types():
    fake = FakeExecutor({
        "summarize": ExecutorResult(rc=0, stdout="TASK", duration_ms=0.0, timed_out=False),
        "tidy": ExecutorResult(rc=0, stdout="SKILL", duration_ms=0.0, timed_out=False),
        "gh/list": ExecutorResult(rc=0, stdout="MCP", duration_ms=0.0, timed_out=False),
    })
    assert fake.run(TaskStep(id="t1", prompt="summarize"), timeout_s=1, max_output_bytes=64).stdout == "TASK"
    assert fake.run(SkillStep(id="k1", skill_id="tidy"), timeout_s=1, max_output_bytes=64).stdout == "SKILL"
    assert fake.run(MCPStep(id="m1", server="gh", tool="list"), timeout_s=1, max_output_bytes=64).stdout == "MCP"


def test_delegating_executor_captures_usage_tokens():
    exe = DelegatingExecutor(default_model="haiku")
    fake_result = SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content='{"ok": true}'))],
        usage=SimpleNamespace(total_tokens=137),
    )
    with patch("litellm.completion", return_value=fake_result):
        r = exe.run(TaskStep(id="t1", prompt="x"), timeout_s=5, max_output_bytes=1024)
    assert r.rc == 0
    assert exe.last.tokens == 137
    assert exe.last.model == "haiku"


def test_delegating_executor_tokens_none_when_backend_gives_no_usage():
    exe = DelegatingExecutor(default_model="haiku")
    # _call patched to a bare string (as most tests do) → no usage available.
    with patch.object(exe, "_call", return_value='{"ok": true}'):
        exe.run(ShellStep(id="s1", command="echo hi"), timeout_s=5, max_output_bytes=1024)
    assert exe.last.tokens is None
