"""Decomposer: prompt → verified typed-step DAG (v0.32)."""

from __future__ import annotations

import pytest

from opendaisugi.decomposer import (
    DecomposedPlan,
    DecomposedStep,
    DecompositionError,
    decompose,
)
from opendaisugi.models import Envelope, MCPStep, Permission, ShellStep, TaskStep


class _FakeCompletions:
    def __init__(self, result):
        self._result = result

    async def create(self, **kwargs):
        return self._result


class _FakeClient:
    """Mimics the instructor async client surface used by envelope.py."""

    def __init__(self, result):
        self.chat = type("C", (), {"completions": _FakeCompletions(result)})()


def _dp(*steps):
    return DecomposedPlan(steps=list(steps))


async def test_decomposes_into_typed_dag():
    result = _dp(
        DecomposedStep(id="t1", type="task", prompt="analyze the data"),
        DecomposedStep(id="s1", type="shell", command="ls", depends_on=["t1"]),
    )
    plan = await decompose("do the thing", client=_FakeClient(result))
    assert plan.source == "decomposer"
    assert plan.task == "do the thing"
    assert isinstance(plan.steps[0], TaskStep)
    assert isinstance(plan.steps[1], ShellStep)
    assert plan.steps[1].depends_on == ["t1"]


async def test_maps_all_orchestration_step_types():
    result = _dp(
        DecomposedStep(id="t1", type="task", prompt="reason"),
        DecomposedStep(id="k1", type="skill", skill_id="tidy", depends_on=["t1"]),
        DecomposedStep(id="m1", type="mcp", server="gh", tool="list", depends_on=["k1"]),
    )
    plan = await decompose("x", client=_FakeClient(result))
    assert [s.type for s in plan.steps] == ["task", "skill", "mcp"]
    assert isinstance(plan.steps[2], MCPStep)


async def test_rejects_a_cyclic_decomposition():
    result = _dp(
        DecomposedStep(id="a", type="task", prompt="p", depends_on=["b"]),
        DecomposedStep(id="b", type="task", prompt="q", depends_on=["a"]),
    )
    with pytest.raises(DecompositionError):
        await decompose("x", client=_FakeClient(result))


async def test_rejects_missing_required_field():
    result = _dp(DecomposedStep(id="t1", type="task"))  # task with no prompt
    with pytest.raises(DecompositionError):
        await decompose("x", client=_FakeClient(result))


async def test_envelope_gate_passes_in_policy_plan():
    result = _dp(DecomposedStep(id="s1", type="shell", command="ls"))
    env = Envelope(generated_by="t", task="x",
                   permissions=Permission(shell=True, shell_allowlist=["ls"]))
    plan = await decompose("x", client=_FakeClient(result), envelope=env)
    assert plan.steps[0].command == "ls"


async def test_envelope_gate_rejects_out_of_policy_plan():
    result = _dp(DecomposedStep(id="s1", type="shell", command="rm -rf /"))
    env = Envelope(generated_by="t", task="x",
                   permissions=Permission(shell=True, shell_allowlist=["ls"]))
    with pytest.raises(DecompositionError) as ei:
        await decompose("x", client=_FakeClient(result), envelope=env)
    assert "verify" in str(ei.value).lower() or "policy" in str(ei.value).lower()


class _CapturingClient:
    """Captures the messages sent to the decomposer and returns a canned plan."""
    def __init__(self, result):
        self.result = result
        self.messages = None
        outer = self

        class _Completions:
            async def create(self, **kwargs):
                outer.messages = kwargs["messages"]
                return outer.result

        self.chat = type("C", (), {"completions": _Completions()})()


async def test_empty_inventory_instructs_task_only():
    client = _CapturingClient(_dp(DecomposedStep(id="t1", type="task", prompt="x")))
    await decompose("do a thing", client=client, available_skills=[], available_mcp_tools=[])
    user_msg = client.messages[-1]["content"]
    assert "ONLY 'task' steps" in user_msg
    assert "do a thing" in user_msg


async def test_inventory_lists_available_skills_and_tools():
    client = _CapturingClient(_dp(DecomposedStep(id="t1", type="task", prompt="x")))
    await decompose("do a thing", client=client,
                    available_skills=["tidy-inbox"], available_mcp_tools=["github/create_issue"])
    user_msg = client.messages[-1]["content"]
    assert "tidy-inbox" in user_msg
    assert "github/create_issue" in user_msg
    assert "Do NOT invent" in user_msg


async def test_no_inventory_is_unconstrained():
    client = _CapturingClient(_dp(DecomposedStep(id="t1", type="task", prompt="x")))
    await decompose("do a thing", client=client)  # neither list passed
    user_msg = client.messages[-1]["content"]
    assert user_msg == "do a thing"  # no inventory block prepended
