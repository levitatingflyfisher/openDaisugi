"""Intelligent token-saving routing — recommend the cheapest viable tier per task.

The differentiator vs Anthropic's advisor tool (which makes a fixed cheap executor
smarter via a mid-generation Opus consult, re-derived every request): openDaisugi
routes a *repeat* task to a verified, reusable pathway (Tier-0, ~free, provably
safe), an easy novel task to a cheap model (Tier-1), and a hard novel task to the
frontier — and flags where the advisor-tool pairing is the better spend.
"""

import time

import numpy as np

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore
from opendaisugi.routing import (
    RouteAdvice,
    RouteAdvisor,
    advisor_tool_available_for_harness,
    estimate_difficulty,
)


def _store_with_hit(tmp_path):
    from opendaisugi._search import _MODEL_NAME
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    store = PathwayStore(tmp_path / "p.db")
    store.put(
        CompiledPathway(
            id="pathway_known01",
            task_description="known task",
            task_embedding=[1.0, 0.0, 0.0],
            embedding_model=_MODEL_NAME,
            embedding_model_version=_EMBEDDING_MODEL_VERSION,
            envelope=Envelope(generated_by="t", task="T", permissions=Permission(shell=True)),
            plan_template=ActionPlan(source="t", task="T", steps=[ShellStep(id="s", command="echo")]),
            source_trace_ids=[],
            distilled_at=time.time(),
        )
    )
    store._embed_query = lambda _: np.array([1.0, 0.0, 0.0])  # exact hit
    return store


def test_difficulty_low_for_trivial_task():
    assert estimate_difficulty("list the files in this directory") < 0.5


def test_difficulty_high_for_complex_task():
    assert estimate_difficulty(
        "design and refactor the concurrent scheduler to fix the deadlock and race condition"
    ) >= 0.5


def test_pathway_hit_routes_to_tier0_reuse(tmp_path):
    adv = RouteAdvisor(pathway_store=_store_with_hit(tmp_path))
    advice = adv.advise("known task")
    assert isinstance(advice, RouteAdvice)
    assert advice.tier == "tier0-pathway"
    assert advice.pathway_id == "pathway_known01"
    assert advice.advisor_pairing is False  # no need to spend — we have a verified plan


def test_easy_novel_task_routes_to_cheap_tier1(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store._embed_query = lambda _: np.array([0.0, 1.0, 0.0])  # empty store -> no hit
    adv = RouteAdvisor(
        pathway_store=store, cheap_model="claude-haiku-4-5", frontier_model="claude-opus-4-8"
    )
    advice = adv.advise("print the current date")
    assert advice.tier == "tier1-cheap"
    assert advice.model == "claude-haiku-4-5"
    assert advice.advisor_pairing is False


def test_hard_novel_task_routes_to_frontier_and_suggests_advisor_pairing(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    adv = RouteAdvisor(
        pathway_store=store, cheap_model="claude-haiku-4-5", frontier_model="claude-opus-4-8"
    )
    advice = adv.advise(
        "architect a distributed consensus algorithm and prove its safety under partition"
    )
    assert advice.tier == "tier2-frontier"
    assert advice.model == "claude-opus-4-8"
    # For a hard, novel task, the Anthropic advisor-tool pairing is the better spend.
    assert advice.advisor_pairing is True
    assert "advisor" in advice.reason.lower()


def test_advise_without_pathway_store_still_works():
    adv = RouteAdvisor(pathway_store=None)
    advice = adv.advise("print the current date")
    assert advice.tier in {"tier1-cheap", "tier2-frontier"}
    assert advice.pathway_id is None


def test_hard_novel_task_without_advisor_tool_omits_advisor(tmp_path):
    # On a harness where the Anthropic advisor tool does not exist (Codex,
    # Ollama/local, Hermes, OpenClaw), routing must not dangle an advisor-tool
    # pairing it can't deliver: frontier model, no advisor mention, no pairing.
    store = PathwayStore(tmp_path / "p.db")
    adv = RouteAdvisor(
        pathway_store=store,
        cheap_model="claude-haiku-4-5",
        frontier_model="claude-opus-4-8",
        advisor_tool_available=False,
    )
    advice = adv.advise(
        "architect a distributed consensus algorithm and prove its safety under partition"
    )
    assert advice.tier == "tier2-frontier"
    assert advice.model == "claude-opus-4-8"
    assert advice.advisor_pairing is False
    assert "advisor" not in advice.reason.lower()


def test_advisor_tool_available_for_harness_anthropic_only():
    # Only Claude/Anthropic harnesses get the advisor tool.
    assert advisor_tool_available_for_harness("claude-code") is True
    assert advisor_tool_available_for_harness("claude") is True
    assert advisor_tool_available_for_harness("anthropic") is True
    assert advisor_tool_available_for_harness("Claude-Code") is True  # case-insensitive
    assert advisor_tool_available_for_harness("  claude-code  ") is True  # trimmed
    # Everything non-Anthropic does not have the advisor tool.
    assert advisor_tool_available_for_harness("codex") is False
    assert advisor_tool_available_for_harness("ollama") is False
    assert advisor_tool_available_for_harness("local") is False
    assert advisor_tool_available_for_harness("llamafile") is False
    assert advisor_tool_available_for_harness("hermes") is False
    assert advisor_tool_available_for_harness("openclaw") is False
    assert advisor_tool_available_for_harness("unknown-thing") is False
