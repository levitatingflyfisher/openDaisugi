"""SkillExecutor and MCPExecutor — pluggable execution of skill/mcp steps (v0.32)."""

from __future__ import annotations

import json

from opendaisugi.models import MCPStep, ShellStep, SkillStep
from opendaisugi.orchestration_executors import MCPExecutor, SkillExecutor

# --------------------------- SkillExecutor ---------------------------

def test_skill_executor_runs_matching_handler():
    exe = SkillExecutor(handlers={"greet": lambda step: f"hello {step.skill_input.get('name')}"})
    r = exe.run(SkillStep(id="k1", skill_id="greet", skill_input={"name": "sam"}),
                timeout_s=1, max_output_bytes=1024)
    assert r.rc == 0
    assert r.stdout == "hello sam"


def test_skill_executor_unknown_skill_is_rc1():
    exe = SkillExecutor(handlers={})
    r = exe.run(SkillStep(id="k1", skill_id="missing"), timeout_s=1, max_output_bytes=1024)
    assert r.rc == 1
    assert "missing" in r.stdout


def test_skill_executor_handler_error_is_rc1():
    def boom(step):
        raise RuntimeError("kaboom")
    exe = SkillExecutor(handlers={"x": boom})
    r = exe.run(SkillStep(id="k1", skill_id="x"), timeout_s=1, max_output_bytes=1024)
    assert r.rc == 1
    assert "kaboom" in r.stdout


def test_skill_executor_rejects_non_skill_step():
    exe = SkillExecutor(handlers={})
    try:
        exe.run(ShellStep(id="s1", command="ls"), timeout_s=1, max_output_bytes=64)
        assert False, "expected TypeError"
    except TypeError:
        pass


# --------------------------- MCPExecutor ---------------------------

def test_mcp_executor_invokes_transport_and_json_encodes_result():
    calls = []

    def transport(server, tool, arguments):
        calls.append((server, tool, arguments))
        return {"issue": 42}

    exe = MCPExecutor(transport=transport)
    r = exe.run(MCPStep(id="m1", server="gh", tool="create", arguments={"title": "x"}),
                timeout_s=1, max_output_bytes=1024)
    assert r.rc == 0
    assert json.loads(r.stdout) == {"issue": 42}
    assert calls == [("gh", "create", {"title": "x"})]


def test_mcp_executor_without_transport_is_rc1():
    exe = MCPExecutor()  # no transport configured
    r = exe.run(MCPStep(id="m1", server="gh", tool="create"), timeout_s=1, max_output_bytes=64)
    assert r.rc == 1
    assert "transport" in r.stdout.lower()


def test_mcp_executor_transport_error_is_rc1():
    def transport(server, tool, arguments):
        raise ConnectionError("server down")
    exe = MCPExecutor(transport=transport)
    r = exe.run(MCPStep(id="m1", server="gh", tool="create"), timeout_s=1, max_output_bytes=64)
    assert r.rc == 1
    assert "server down" in r.stdout
