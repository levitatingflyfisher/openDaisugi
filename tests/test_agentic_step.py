"""Tests for AgenticStep — the tool-using delegation step (roadmap Stage 2).

TaskStep stays a pure-reasoning leaf; AgenticStep is the step type that IS
allowed to touch tools, and precisely because of that it gets a real
permission-checking arm in the verifier — it must never ride TaskStep's
pure-reasoning exemption. No silent pass.
"""

from __future__ import annotations

import pytest

from opendaisugi.models import ActionPlan, AgenticStep, Envelope, Permission
from opendaisugi.verify import check_permissions, verify


def _envelope(stakes="medium", **perm_kwargs) -> Envelope:
    perms = {"file_read": ["/work/**"], **perm_kwargs}
    return Envelope(
        generated_by="test",
        task="agentic test",
        permissions=Permission(**perms),
        stakes=stakes,
    )


def _step(**kwargs) -> AgenticStep:
    defaults = dict(
        id="a1", prompt="summarize the repo", workspace="/work/repo",
        tools=["Read"],
    )
    defaults.update(kwargs)
    return AgenticStep(**defaults)


def _plan(step) -> ActionPlan:
    return ActionPlan(source="test", task="agentic test", steps=[step])


# ------------------------------------------------------------------- model

def test_agentic_step_constructs_and_roundtrips_in_plan():
    plan = _plan(_step())
    assert plan.steps[0].type == "agentic"
    rehydrated = ActionPlan.model_validate_json(plan.model_dump_json())
    assert isinstance(rehydrated.steps[0], AgenticStep)
    assert rehydrated.steps[0].tools == ["Read"]


# ---------------------------------------------- capability mapping (allow)

def test_read_tools_verify_against_file_read_globs():
    result = verify(_plan(_step(tools=["Read", "Glob", "Grep"])), _envelope())
    assert result.ok, result.violations


def test_bash_tool_verifies_when_shell_allowed():
    env = _envelope(shell=True, shell_allowlist=["git"])
    result = verify(_plan(_step(tools=["Read", "Bash"])), env)
    assert result.ok, result.violations


def test_write_and_network_tools_verify_with_capabilities():
    env = _envelope(file_write=["/work/**"], network=True)
    result = verify(_plan(_step(tools=["Write", "Edit", "WebFetch"])), env)
    assert result.ok, result.violations


# ----------------------------------------------- capability mapping (deny)

def test_bash_tool_denied_when_envelope_forbids_shell():
    vs = check_permissions(_plan(_step(tools=["Bash"])), _envelope())
    assert any("shell" in v.message for v in vs)


def test_write_tool_denied_without_file_write_globs():
    vs = check_permissions(_plan(_step(tools=["Write"])), _envelope())
    assert any("file_write" in v.message for v in vs)


def test_webfetch_denied_without_network():
    vs = check_permissions(_plan(_step(tools=["WebFetch"])), _envelope())
    assert any("network" in v.message for v in vs)


def test_unknown_requested_tool_denied_by_default():
    vs = check_permissions(_plan(_step(tools=["Read", "LaunchMissiles"])), _envelope())
    assert any("LaunchMissiles" in v.message for v in vs)


def test_empty_tools_rejected_use_a_task_step():
    vs = check_permissions(_plan(_step(tools=[])), _envelope())
    assert any("TaskStep" in v.message for v in vs)


def test_workspace_outside_file_read_globs_denied():
    vs = check_permissions(_plan(_step(workspace="/elsewhere/repo")), _envelope())
    assert any("workspace" in v.message for v in vs)


# --------------------------------------------------- stakes + strict mode

def test_physical_stakes_refuses_agentic_delegation_outright():
    env = _envelope(stakes="physical")
    result = verify(_plan(_step()), env)
    assert not result.ok
    assert any("physical" in v.message for v in result.violations)


def test_strict_mode_does_not_flag_agentic_as_unknown_type():
    """'agentic' must be in the verifier's known-type set: a high-stakes
    envelope (strict default-on) with a valid agentic step verifies without
    an 'unverifiable step type' violation."""
    env = _envelope(stakes="high")
    result = verify(_plan(_step()), env)
    assert result.ok, result.violations
