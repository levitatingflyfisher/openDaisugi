"""2C — ``daisugi_recall``: assured reuse a harness opts into (ADR-0012).

``verify`` is sync and pure, and frozen recall needs no model, so the core
contract is fully testable with hand-built objects — no LLM, no embedder.
The one load-bearing behavior under test throughout: a reused plan is
re-verified against the CALLER's envelope, and ANY failure (no match, or
verify-fail) is a miss, never an unverified hit.
"""

from __future__ import annotations

import pytest

from opendaisugi import Daisugi
from opendaisugi.gateway_recall import RecallProvenance, RecallResult, recall
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway, PathwayMatch, PathwayParameter
from opendaisugi.pathway_store import PathwayStore

pytest.importorskip("mcp")

from opendaisugi.mcp_server import build_server  # noqa: E402


def _env(*allow: str, shell: bool = True) -> Envelope:
    return Envelope(
        generated_by="test",
        task="x",
        permissions=Permission(shell=shell, shell_allowlist=list(allow)),
    )


def _frozen_pathway(id_: str = "pw-frozen") -> CompiledPathway:
    template = ActionPlan(
        source="distilled",
        task="say hi",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    return CompiledPathway(
        id=id_,
        task_description="say hi to someone",
        task_embedding=[0.1, 0.2],
        envelope=_env("echo"),
        plan_template=template,
        source_trace_ids=["t1", "t2"],
        distilled_at=1000.0,
        hit_count=5,
    )


def _typed_pathway(id_: str = "pw-typed") -> CompiledPathway:
    template = ActionPlan(
        source="distilled",
        task="find X",
        steps=[ShellStep(id="s1", command="grep -rn TODO src")],
    )
    return CompiledPathway(
        id=id_,
        task_description="find X",
        task_embedding=[0.1],
        envelope=_env("grep"),
        plan_template=template,
        source_trace_ids=["t1"],
        distilled_at=2000.0,
        hit_count=1,
        parameters=[
            PathwayParameter(
                name="s1.command",
                step_index=0,
                step_id="s1",
                field="command",
                head="grep",
                observed=["grep -rn TODO src"],
            )
        ],
    )


class _StubStore:
    """Duck-typed ``PathwayStore``: ``.find(task)`` returns a canned match."""

    def __init__(self, match: PathwayMatch | None) -> None:
        self._match = match
        self.find_calls: list[str] = []

    def find(self, task: str, *, threshold: float | None = None) -> PathwayMatch | None:
        self.find_calls.append(task)
        return self._match


async def test_frozen_hit_permitted_returns_deep_copy_with_provenance():
    pathway = _frozen_pathway()
    store = _StubStore(PathwayMatch(pathway=pathway, similarity=0.87))

    result = await recall("say hi to Bob", _env("echo"), pathway_store=store)

    assert isinstance(result, RecallResult)
    assert result.hit is True
    assert result.reason is None
    assert result.plan is not None
    assert result.plan.steps[0].command == "echo hi"

    # Deep copy: mutating the returned plan must not touch the stored template.
    result.plan.steps[0].command = "MUTATED"
    assert pathway.plan_template.steps[0].command == "echo hi"

    assert result.provenance == RecallProvenance(
        pathway_id="pw-frozen",
        similarity=0.87,
        tier="frozen",
        source_trace_count=2,
        distilled_at=1000.0,
        hit_count=5,
    )


async def test_verify_fail_is_a_miss_not_an_unverified_hit():
    """The safety line: reuse is re-verified against the CALLER's envelope,
    not trusted just because it matched. A caller envelope that forbids the
    frozen plan's command must produce a miss, never a hit."""
    pathway = _frozen_pathway()
    store = _StubStore(PathwayMatch(pathway=pathway, similarity=0.9))

    # Caller's envelope forbids shell entirely — the pathway's own envelope
    # (which permits "echo") must NOT be what gets trusted here.
    result = await recall("say hi to Bob", _env(shell=False), pathway_store=store)

    assert result.hit is False
    assert result.reason is not None
    assert "verif" in result.reason.lower()
    assert result.plan is None
    assert result.provenance is None


async def test_no_match_is_a_miss():
    store = _StubStore(None)

    result = await recall("something never seen", _env("echo"), pathway_store=store)

    assert result == RecallResult(
        hit=False, reason="no matching pathway", plan=None, provenance=None
    )
    assert store.find_calls == ["something never seen"]


async def test_typed_routes_through_bind_parameters(monkeypatch):
    pathway = _typed_pathway()
    store = _StubStore(PathwayMatch(pathway=pathway, similarity=0.72))

    bound_plan = ActionPlan(
        source="bound",
        task="find FIXME",
        steps=[ShellStep(id="s1", command="grep -rn FIXME src")],
    )
    bind_calls = []

    async def _fake_bind_parameters(pw, task, *, envelope, model, z3_timeout_ms, client, backend):
        bind_calls.append((pw.id, task))
        return bound_plan

    import opendaisugi.gateway_recall as gateway_recall_mod

    monkeypatch.setattr(gateway_recall_mod, "bind_parameters", _fake_bind_parameters)

    caller_env = _env("grep")
    result = await recall("find FIXME", caller_env, pathway_store=store, model="m")

    assert bind_calls == [("pw-typed", "find FIXME")]
    assert result.hit is True
    assert result.plan is not None
    assert result.plan.steps[0].command == "grep -rn FIXME src"
    assert result.provenance is not None
    assert result.provenance.tier == "typed"
    assert result.provenance.pathway_id == "pw-typed"


async def test_typed_bind_result_still_gated_by_verify(monkeypatch):
    """Even a plan bind_parameters hands back must clear verify() against the
    caller's envelope — bind_parameters' own internal re-verify is not a
    substitute for recall's own gate."""
    pathway = _typed_pathway()
    store = _StubStore(PathwayMatch(pathway=pathway, similarity=0.72))

    bad_plan = ActionPlan(
        source="bound",
        task="find FIXME",
        steps=[ShellStep(id="s1", command="grep -rn FIXME src")],
    )

    async def _fake_bind_parameters(pw, task, *, envelope, model, z3_timeout_ms, client, backend):
        return bad_plan

    import opendaisugi.gateway_recall as gateway_recall_mod

    monkeypatch.setattr(gateway_recall_mod, "bind_parameters", _fake_bind_parameters)

    # Caller's envelope does not allow grep.
    result = await recall("find FIXME", _env("echo"), pathway_store=store, model="m")

    assert result.hit is False
    assert result.plan is None
    assert result.provenance is None


# ----- MCP tool shape -----


class _StubPathwayStore(PathwayStore):
    """A ``PathwayStore`` subclass so ``isinstance`` checks in ``Daisugi``
    pass, but ``find`` returns a canned match — no sqlite, no embedder."""

    def __init__(self, match: PathwayMatch | None) -> None:
        # Deliberately skips PathwayStore.__init__ — no sqlite file needed.
        self._match = match

    def find(self, task: str, *, threshold: float | None = None) -> PathwayMatch | None:
        return self._match


async def test_mcp_recall_tool_hit(tmp_path):
    pathway = _frozen_pathway()
    store = _StubPathwayStore(PathwayMatch(pathway=pathway, similarity=0.9))
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=store)

    server = build_server(d)
    _, structured = await server.call_tool(
        "recall",
        {"task": "say hi to Bob", "envelope": _env("echo").model_dump(mode="json")},
    )

    assert structured["hit"] is True
    assert structured["reason"] is None
    assert structured["plan"]["steps"][0]["command"] == "echo hi"
    assert structured["provenance"]["pathway_id"] == "pw-frozen"
    assert structured["provenance"]["tier"] == "frozen"


async def test_mcp_recall_tool_miss_no_match(tmp_path):
    store = _StubPathwayStore(None)
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=store)

    server = build_server(d)
    _, structured = await server.call_tool(
        "recall",
        {"task": "anything", "envelope": _env("echo").model_dump(mode="json")},
    )

    assert structured["hit"] is False
    assert structured["reason"] == "no matching pathway"
    assert structured["plan"] is None
    assert structured["provenance"] is None


async def test_mcp_recall_tool_no_pathway_store(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)

    server = build_server(d)
    _, structured = await server.call_tool(
        "recall",
        {"task": "anything", "envelope": _env("echo").model_dump(mode="json")},
    )

    assert structured == {
        "hit": False,
        "reason": "no pathway store",
        "plan": None,
        "provenance": None,
    }
