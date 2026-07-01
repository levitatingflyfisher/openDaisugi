"""verify() gives task/skill/mcp steps real checkable surfaces (v0.32).

These are the anti-vacuity tests: a new step type that passed verify() without a
dedicated permission surface would run LLM-authored actions with zero runtime
assurance — silently defeating the library. Each type must be able to FAIL.
"""

from __future__ import annotations

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    MCPStep,
    Permission,
    SkillStep,
    TaskStep,
)
from opendaisugi.verify import verify


def _plan(*steps):
    return ActionPlan(source="test", task="demo", steps=list(steps))


# --------------------------- MCPStep ---------------------------

def test_mcp_step_denied_when_allowlist_empty():
    env = Envelope(generated_by="t", task="demo", permissions=Permission())
    r = verify(_plan(MCPStep(id="m1", server="github", tool="create_issue")), env)
    assert not r.ok
    assert any(v.stage == "permissions" and "github/create_issue" in v.message for v in r.violations)


def test_mcp_step_allowed_by_exact_entry():
    env = Envelope(generated_by="t", task="demo",
                   permissions=Permission(mcp_allowlist=["github/create_issue"]))
    r = verify(_plan(MCPStep(id="m1", server="github", tool="create_issue")), env)
    assert r.ok, [v.message for v in r.violations]


def test_mcp_step_allowed_by_glob():
    env = Envelope(generated_by="t", task="demo",
                   permissions=Permission(mcp_allowlist=["github/*"]))
    r = verify(_plan(MCPStep(id="m1", server="github", tool="list_issues")), env)
    assert r.ok, [v.message for v in r.violations]


def test_mcp_step_rejected_when_server_not_matched():
    env = Envelope(generated_by="t", task="demo",
                   permissions=Permission(mcp_allowlist=["github/*"]))
    r = verify(_plan(MCPStep(id="m1", server="slack", tool="post")), env)
    assert not r.ok


# --------------------------- SkillStep ---------------------------

def _shell_env(allow):
    return Envelope(generated_by="t", task="demo",
                    permissions=Permission(shell=True, shell_allowlist=allow))


def test_skill_step_rejected_when_contract_not_subsumed():
    caller = _shell_env(["ls"])                 # caller may only run ls
    skill_contract = _shell_env(["ls", "rm"])   # skill wants ls AND rm
    step = SkillStep(id="k1", skill_id="cleanup", contract_envelope=skill_contract)
    r = verify(_plan(step), caller)
    assert not r.ok
    assert any(v.stage == "delegation" for v in r.violations)


def test_skill_step_allowed_when_contract_subsumed():
    caller = _shell_env(["ls", "cat", "rm"])
    skill_contract = _shell_env(["ls", "cat"])  # subset → subsumed
    step = SkillStep(id="k1", skill_id="viewer", contract_envelope=skill_contract)
    r = verify(_plan(step), caller)
    assert r.ok, [v.message for v in r.violations]


def test_opaque_skill_rejected_under_strict_surfaced_under_lenient():
    caller = _shell_env(["ls"])
    step = SkillStep(id="k1", skill_id="mystery")  # no contract_envelope
    strict = verify(_plan(step), caller, strict=True)
    assert not strict.ok
    assert any(v.stage == "delegation" for v in strict.violations)

    lenient = verify(_plan(step), caller, strict=False)
    assert lenient.ok
    assert any("mystery" in w for w in lenient.warnings)


# --------------------------- TaskStep ---------------------------

def test_task_step_verifies_under_minimal_software_envelope():
    # Pure-reasoning leaf: it does nothing, so a bare envelope admits it.
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="low")
    step = TaskStep(id="t1", prompt="think about it", preferred_model="haiku")
    assert verify(_plan(step), env).ok


def test_task_step_delegation_refused_under_physical_stakes():
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="physical")
    step = TaskStep(id="t1", prompt="move the arm somehow", preferred_model="haiku")
    r = verify(_plan(step), env)
    assert not r.ok
    assert any("physical" in v.message for v in r.violations)
