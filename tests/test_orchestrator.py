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


async def test_budget_gates_routing_across_steps_during_run():
    # Two hard task steps that each want the frontier (~4500 est). With a 5000
    # budget, step 1 runs at the frontier and spends it; step 2 — sized live
    # against what's left — must downgrade. This is during-run gating, not a
    # static up-front decision.
    hard = "prove this concurrency algorithm is deadlock-free and race-free under partition"
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="low")
    orch = Orchestrator()
    with patch("opendaisugi.delegating_executor.DelegatingExecutor._call", return_value="{}"):
        result = await orch.orchestrate(
            "do two hard things",
            envelope=env,
            budget_tokens=5000,
            decompose_client=_decompose_client(
                DecomposedStep(id="t1", type="task", prompt=hard),
                DecomposedStep(id="t2", type="task", prompt=hard, depends_on=["t1"]),
            ),
            synth_client=_synth_client("done"),
        )
    frontier_model = DEFAULT_LADDER.rungs[-1].model
    by_model = result.budget.by_model
    # step 1 got the frontier; step 2 was downgraded off it once the budget ran low.
    assert by_model.get(frontier_model, 0) > 0
    non_frontier_spend = sum(v for m, v in by_model.items() if m != frontier_model)
    assert non_frontier_spend > 0
    assert result.budget.step_count == 2
    # Reported sizings reflect what ACTUALLY ran (not the static estimate): step 2
    # shows the downgrade, not the frontier it was planned for.
    sizing_by_id = {s.step_id: s for s in result.sizings}
    assert sizing_by_id["t1"].model == frontier_model
    assert sizing_by_id["t2"].model != frontier_model
    assert sizing_by_id["t2"].downgraded is True


async def test_strict_budget_fails_a_step_it_cannot_afford():
    hard = "prove this concurrency algorithm is deadlock-free and race-free under partition"
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="low")
    orch = Orchestrator()
    with patch("opendaisugi.delegating_executor.DelegatingExecutor._call", return_value="{}"):
        result = await orch.orchestrate(
            "one hard thing on a starved budget",
            envelope=env,
            budget_tokens=10,          # cannot afford any tier
            strict_budget=True,        # → fail cleanly instead of overspending
            decompose_client=_decompose_client(
                DecomposedStep(id="t1", type="task", prompt=hard),
            ),
            synth_client=_synth_client("unused"),
        )
    # The step failed for budget reasons; nothing was spent (no LLM call made).
    assert result.status != "succeeded"
    assert result.budget.spent == 0


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


async def test_orchestrator_threads_generous_step_timeout_to_supervisor():
    # LLM task steps need a longer timeout than the shell-oriented 30s default.
    import opendaisugi.orchestrator as orch_mod
    captured = {}
    real_supervisor = orch_mod.Supervisor

    class _CapSupervisor(real_supervisor):
        def __init__(self, **kwargs):
            captured.update(kwargs)
            super().__init__(**kwargs)

    orch = Orchestrator(step_timeout_s=180)
    import unittest.mock as m
    with m.patch.object(orch_mod, "Supervisor", _CapSupervisor):
        await orch.orchestrate(
            "print a word",
            envelope=_echo_envelope(),
            decompose_client=_decompose_client(DecomposedStep(id="s1", type="shell", command="echo hi")),
            synth_client=_synth_client("ok"),
        )
    assert captured["step_timeout_s"] == 180


async def test_orchestrate_records_exact_cost_from_backend():
    # When the delegating executor reports a measured cost (claude-code json),
    # it flows into the budget's measured_cost_usd.
    from unittest.mock import patch as _patch
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="low")
    orch = Orchestrator()

    def fake_metered(prompt, *, timeout_s, model, binary="claude", cwd=None):
        return "answer", {"tokens": 50, "cost_usd": 0.0207}

    with _patch("opendaisugi.claude_code_llm.call_claude_p_metered", fake_metered), \
         _patch("opendaisugi.llm.resolve_backend", return_value="claude-code"):
        result = await orch.orchestrate(
            "one reasoning step",
            envelope=env,
            decompose_client=_decompose_client(DecomposedStep(id="t1", type="task", prompt="think")),
            synth_client=_synth_client("done"),
        )
    assert result.budget.measured_cost_usd == 0.0207


async def test_reused_pathway_verified_against_caller_envelope_not_pathway_envelope():
    # H2: a pathway whose OWN envelope permits shell must NOT run under a caller
    # envelope that forbids shell — the caller's envelope is the ceiling.
    from opendaisugi.models import Envelope as _Env, Permission as _Perm
    permissive = _Env(generated_by="pw", task="cached",
                      permissions=_Perm(shell=True, shell_allowlist=["rm"]))
    template = ActionPlan(source="distilled", task="cached",
                          steps=[ShellStep(id="s1", command="rm -rf /tmp/x")])
    pathway = CompiledPathway(
        id="pw_x", task_description="clean up", task_embedding=[0.1],
        envelope=permissive, plan_template=template, source_trace_ids=["t"], distilled_at=0.0)

    class _Store:
        def find(self, task, *, threshold):
            return PathwayMatch(pathway=pathway, similarity=0.99)

    orch = Orchestrator(pathway_store=_Store())
    caller_env = _Env(generated_by="caller", task="x",
                      permissions=_Perm(shell=True, shell_allowlist=["echo"]))  # no 'rm'
    result = await orch.orchestrate(
        "clean up",
        envelope=caller_env,
        decompose_client=_decompose_client(
            DecomposedStep(id="d1", type="shell", command="echo did-not-reuse")),
        synth_client=_synth_client("ok"),
    )
    # The pathway (rm) is NOT admissible under the caller (echo-only) → fall through
    # to decomposition, which ran the echo plan instead.
    assert result.reused_pathway is False
    assert result.plan.steps[0].command == "echo did-not-reuse"


async def test_strict_budget_overrun_keeps_work_and_stops_spending():
    # H3: pre-gate allows the step by estimate, but ACTUAL usage exceeds the strict
    # ceiling. The completed step's output must be kept (not discarded as an error),
    # its spend counted, and synthesis must NOT fire another LLM call.
    from types import SimpleNamespace
    env = Envelope(generated_by="t", task="demo", permissions=Permission(), stakes="low")
    orch = Orchestrator()
    fake = SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content="a real answer"))],
        usage=SimpleNamespace(total_tokens=5000),  # >> the 2500 ceiling
    )
    with patch("litellm.completion", return_value=fake):
        result = await orch.orchestrate(
            "one step that overruns",
            envelope=env,
            budget_tokens=2500,       # cheap est (2000) fits the pre-gate...
            strict_budget=True,       # ...but 5000 actual crosses the strict ceiling
            decompose_client=_decompose_client(DecomposedStep(id="t1", type="task", prompt="do it")),
            synth_client=_synth_client("SHOULD NOT SPEND"),
        )
    # the step's work is preserved, not thrown away as an executor error
    assert result.session.steps[0].status == "succeeded"
    # its spend was counted (not dropped by the raise)
    assert result.budget.spent == 5000
    # budget exhausted → deterministic synthesis, no further LLM spend
    assert result.used_llm_synthesis is False
    assert result.final_answer != "SHOULD NOT SPEND"
