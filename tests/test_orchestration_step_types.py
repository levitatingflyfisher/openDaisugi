"""The orchestration step types: task / skill / mcp (v0.32)."""

from __future__ import annotations

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    MCPStep,
    Permission,
    SkillStep,
    StepBase,
    TaskStep,
    get_step_type_registry,
)


def test_task_step_is_a_pure_reasoning_leaf():
    s = TaskStep(id="t1", prompt="summarize the findings", preferred_model="haiku")
    assert s.type == "task"
    assert s.prompt == "summarize the findings"
    assert s.preferred_model == "haiku"
    # A TaskStep must NOT carry a capability field — that structural absence is
    # what contains it (no command/path/url to splice a privileged action into).
    assert not hasattr(s, "command")
    assert not hasattr(s, "path")
    assert not hasattr(s, "url")


def test_skill_step_carries_optional_contract_envelope():
    env = Envelope(generated_by="t", task="x", permissions=Permission(shell=True, shell_allowlist=["ls"]))
    s = SkillStep(id="k1", skill_id="tidy-inbox", skill_input={"limit": 10}, contract_envelope=env)
    assert s.type == "skill"
    assert s.skill_id == "tidy-inbox"
    assert s.skill_input == {"limit": 10}
    assert s.contract_envelope is env
    # contract_envelope is optional (opaque skill)
    assert SkillStep(id="k2", skill_id="x").contract_envelope is None


def test_mcp_step_names_server_tool_arguments():
    s = MCPStep(id="m1", server="github", tool="create_issue", arguments={"title": "bug"})
    assert s.type == "mcp"
    assert s.server == "github"
    assert s.tool == "create_issue"
    assert s.arguments == {"title": "bug"}


def test_all_three_are_registered_step_types():
    reg = get_step_type_registry()
    assert reg["task"] is TaskStep
    assert reg["skill"] is SkillStep
    assert reg["mcp"] is MCPStep
    for cls in (TaskStep, SkillStep, MCPStep):
        assert issubclass(cls, StepBase)


def test_round_trip_through_actionplan_from_dicts():
    plan = ActionPlan(
        source="decomposer",
        task="demo",
        steps=[
            {"type": "task", "id": "t1", "prompt": "analyze"},
            {"type": "skill", "id": "k1", "skill_id": "tidy", "depends_on": ["t1"]},
            {"type": "mcp", "id": "m1", "server": "gh", "tool": "list", "depends_on": ["k1"]},
        ],
    )
    kinds = [s.type for s in plan.steps]
    assert kinds == ["task", "skill", "mcp"]
    assert isinstance(plan.steps[0], TaskStep)
    assert isinstance(plan.steps[1], SkillStep)
    assert isinstance(plan.steps[2], MCPStep)


def test_permission_has_mcp_allowlist_defaulting_empty():
    p = Permission()
    assert p.mcp_allowlist == []
    p2 = Permission(mcp_allowlist=["github/*", "fs/read_file"])
    assert p2.mcp_allowlist == ["github/*", "fs/read_file"]
