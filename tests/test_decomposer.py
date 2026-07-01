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
