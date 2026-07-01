"""Orchestrator — the forward-looking composition root (v0.32)."""

from __future__ import annotations

from unittest.mock import patch

from opendaisugi.budget import BudgetTracker
from opendaisugi.decomposer import DecomposedPlan, DecomposedStep
from opendaisugi.model_sizer import DEFAULT_LADDER
from opendaisugi.models import Envelope, Permission, TaskStep
from opendaisugi.orchestrator import (
    BudgetAwareDelegatingExecutor,
    OrchestrationResult,
    Orchestrator,
)
from opendaisugi.pathway import CompiledPathway, PathwayMatch
from opendaisugi.models import ActionPlan, ShellStep


# --- fakes -----------------------------------------------------------------

class _FakeCompletions:
    def __init__(self, result):
        self._result = result

    async def create(self, **kwargs):
        return self._result


class _FakeClient:
    def __init__(self, result):
        self.chat = type("C", (), {"completions": _FakeCompletions(result)})()


def _decompose_client(*steps):
    return _FakeClient(DecomposedPlan(steps=list(steps)))


def _synth_client(answer):
    return _FakeClient(type("A", (), {"answer": answer})())


def _echo_envelope():
    return Envelope(generated_by="t", task="demo",
                    permissions=Permission(shell=True, shell_allowlist=["echo"]))


# --- BudgetAwareDelegatingExecutor -----------------------------------------

def test_budget_aware_executor_downgrades_and_records():
    tracker = BudgetTracker(total_tokens=2500)  # < frontier est (4500)
    exe = BudgetAwareDelegatingExecutor(tracker=tracker, ladder=DEFAULT_LADDER)
    hard = TaskStep(id="t1", prompt="prove this concurrency algorithm is deadlock-free and race-free")
    with patch.object(exe, "_call", return_value='{"ok": true}'):
        exe.run(hard, timeout_s=5, max_output_bytes=1024)
    # frontier was too expensive → routed to a cheaper rung...
    assert exe.last.model != DEFAULT_LADDER.rungs[-1].model
    # ...and the spend was recorded live (estimate, since the fake gave no usage).
    assert tracker.spent() > 0


def test_budget_aware_executor_records_actual_tokens_when_available():
    from types import SimpleNamespace
    tracker = BudgetTracker(total_tokens=100_000)
    exe = BudgetAwareDelegatingExecutor(tracker=tracker, ladder=DEFAULT_LADDER)
    fake_result = SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content='{"ok": true}'))],
        usage=SimpleNamespace(total_tokens=321),
    )
    with patch("litellm.completion", return_value=fake_result):
        exe.run(TaskStep(id="t1", prompt="easy"), timeout_s=5, max_output_bytes=1024)
    assert tracker.spent() == 321


# --- Orchestrator end to end ------------------------------------------------

async def test_orchestrate_shell_plan_end_to_end():
    orch = Orchestrator()
    result = await orch.orchestrate(
        "print two words",
        envelope=_echo_envelope(),
        decompose_client=_decompose_client(
            DecomposedStep(id="s1", type="shell", command="echo alpha"),
            DecomposedStep(id="s2", type="shell", command="echo beta", depends_on=["s1"]),
        ),
        synth_client=_synth_client("COMBINED ANSWER"),
    )
    assert isinstance(result, OrchestrationResult)
    assert result.status == "succeeded"
    assert [s.type for s in result.plan.steps] == ["shell", "shell"]
    assert result.final_answer == "COMBINED ANSWER"
    assert len(result.sizings) == 2
    assert result.reused_pathway is False
    assert result.budget.spent == 0  # no LLM steps → no token spend


async def test_orchestrate_deterministic_synth_when_budget_zero():
    orch = Orchestrator()
    result = await orch.orchestrate(
        "print a word",
        envelope=_echo_envelope(),
        budget_tokens=0,  # exhausted from the start → synth must not call an LLM
        decompose_client=_decompose_client(
            DecomposedStep(id="s1", type="shell", command="echo GAMMA"),
        ),
        synth_client=_synth_client("SHOULD NOT BE USED"),
    )
    assert result.used_llm_synthesis is False
    assert "GAMMA" in result.final_answer
    assert result.final_answer != "SHOULD NOT BE USED"


async def test_orchestrate_reuses_pathway_when_matched():
    template = ActionPlan(source="distilled", task="cached", steps=[ShellStep(id="s1", command="echo cached")])
    pathway = CompiledPathway(
        id="pw_1", task_description="print two words", task_embedding=[0.1],
        envelope=_echo_envelope(), plan_template=template, source_trace_ids=["tr1"],
        distilled_at=0.0,
    )

    class _Store:
        def find(self, task, *, threshold):
            return PathwayMatch(pathway=pathway, similarity=0.99)

    orch = Orchestrator(pathway_store=_Store())
    # decompose_client is intentionally a bomb: reuse must skip decomposition.
    result = await orch.orchestrate(
        "print two words",
        envelope=_echo_envelope(),
        decompose_client=_FakeClient(None),
        synth_client=_synth_client("REUSED"),
    )
    assert result.reused_pathway is True
    assert result.plan.steps[0].command == "echo cached"
    assert result.final_answer == "REUSED"


async def test_orchestrate_rejects_out_of_policy_decomposition():
    from opendaisugi.decomposer import DecompositionError
    orch = Orchestrator()
    try:
        await orch.orchestrate(
            "delete everything",
            envelope=_echo_envelope(),  # only 'echo' allowed
            decompose_client=_decompose_client(
                DecomposedStep(id="s1", type="shell", command="rm -rf /"),
            ),
            synth_client=_synth_client("x"),
        )
        assert False, "expected DecompositionError for out-of-policy plan"
    except DecompositionError:
        pass
