"""B4 — bind a typed pathway's holes, with two independent safety fallbacks."""

from __future__ import annotations

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway, PathwayParameter
from opendaisugi.pathway_bind import Bindings, bind_parameters


class _FakeCompletions:
    def __init__(self, result):
        self._result = result

    async def create(self, **kwargs):
        return self._result


class _FakeClient:
    def __init__(self, result):
        self.chat = type("C", (), {"completions": _FakeCompletions(result)})()


def _env(*allow: str) -> Envelope:
    return Envelope(
        generated_by="t",
        task="x",
        permissions=Permission(shell=True, shell_allowlist=list(allow) or ["grep"]),
    )


def _typed_pathway() -> CompiledPathway:
    template = ActionPlan(
        source="distilled",
        task="find X",
        steps=[ShellStep(id="s1", command="grep -rn TODO src")],
    )
    return CompiledPathway(
        id="pw",
        task_description="find X",
        task_embedding=[0.1],
        envelope=_env("grep"),
        plan_template=template,
        source_trace_ids=["t1"],
        distilled_at=0.0,
        parameters=[
            PathwayParameter(
                name="s1.command",
                step_index=0,
                step_id="s1",
                field="command",
                head="grep",
                observed=["grep -rn TODO src", "grep -rn FIXME src"],
            )
        ],
    )


async def test_frozen_pathway_returns_template_unchanged():
    pw = _typed_pathway().model_copy(update={"parameters": []})
    plan = await bind_parameters(
        pw,
        "find FIXME",
        envelope=_env("grep"),
        model="m",
        z3_timeout_ms=500,
        client=_FakeClient(None),  # bomb: frozen path must not call the LLM
    )
    assert plan.steps[0].command == "grep -rn TODO src"


async def test_valid_bind_applies_and_verifies():
    pw = _typed_pathway()
    client = _FakeClient(Bindings(values={"s1.command": "grep -rn FIXME src"}))
    plan = await bind_parameters(
        pw,
        "find FIXME",
        envelope=_env("grep"),
        model="m",
        z3_timeout_ms=500,
        client=client,
    )
    assert plan.steps[0].command == "grep -rn FIXME src"


async def test_capability_head_change_falls_back_to_template():
    # The LLM tries to swap the program (grep → git): rejected as a head change,
    # even though the caller's envelope happens to allow git too.
    pw = _typed_pathway()
    client = _FakeClient(Bindings(values={"s1.command": "git status"}))
    plan = await bind_parameters(
        pw,
        "status",
        envelope=_env("grep", "git"),
        model="m",
        z3_timeout_ms=500,
        client=client,
    )
    assert plan.steps[0].command == "grep -rn TODO src"  # frozen fallback


async def test_out_of_envelope_bind_falls_back_to_template():
    # Head preserved (grep), but the caller's envelope forbids grep → verify
    # fails on the bound plan → frozen fallback. Second, independent safety net.
    pw = _typed_pathway()
    client = _FakeClient(Bindings(values={"s1.command": "grep -rn FIXME src"}))
    plan = await bind_parameters(
        pw,
        "find FIXME",
        envelope=_env("echo"),
        model="m",
        z3_timeout_ms=500,
        client=client,
    )
    assert plan.steps[0].command == "grep -rn TODO src"


async def test_malformed_bind_response_falls_back_to_template():
    # A response whose `values` isn't a dict must fall back, never crash.
    pw = _typed_pathway()

    class _Bad:
        values = "not a dict"

    plan = await bind_parameters(
        pw, "x", envelope=_env("grep"), model="m", z3_timeout_ms=500, client=_FakeClient(_Bad())
    )
    assert plan.steps[0].command == "grep -rn TODO src"  # frozen fallback
