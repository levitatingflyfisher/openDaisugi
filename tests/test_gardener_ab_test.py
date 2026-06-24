"""Tests for the v0.4.0 Gardener A/B harness."""

from __future__ import annotations

import time

import pytest

from opendaisugi.gardener import ABResult, ab_test
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    Postcondition,
    ShellStep,
)
from opendaisugi.pathway import CompiledPathway


def _mk_envelope(
    *,
    postconditions: list[Postcondition] | None = None,
    permissions: Permission | None = None,
) -> Envelope:
    return Envelope(
        generated_by="test",
        task="T",
        permissions=permissions or Permission(shell=True),
        invariants=[],
        postconditions=postconditions
        or [Postcondition(type="exit_code", expected=0)],
    )


def _mk_pathway(envelope: Envelope) -> CompiledPathway:
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id="pw_1",
        task_description="T",
        task_embedding=[0.1, 0.2, 0.3],
        envelope=envelope,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
    )


@pytest.mark.asyncio
async def test_postcondition_match_passes() -> None:
    env = _mk_envelope()
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        # Same postconditions + permissions — should pass.
        return _mk_envelope()

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    assert result.postconditions_match
    assert result.permissions_match
    assert result.passed


@pytest.mark.asyncio
async def test_divergent_postconditions_flagged() -> None:
    env = _mk_envelope(postconditions=[Postcondition(type="exit_code", expected=0)])
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        return _mk_envelope(
            postconditions=[Postcondition(type="file_exists", path="/tmp/out")]
        )

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    assert not result.postconditions_match
    assert not result.passed


@pytest.mark.asyncio
async def test_divergent_permissions_flagged() -> None:
    env = _mk_envelope(permissions=Permission(shell=True, network=False))
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        return _mk_envelope(permissions=Permission(shell=True, network=True))

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    assert result.postconditions_match
    assert not result.permissions_match
    assert not result.passed


@pytest.mark.asyncio
async def test_judge_can_override_structural_match_with_failure() -> None:
    env = _mk_envelope()
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        return _mk_envelope()

    async def judge(a: Envelope, b: Envelope) -> bool:
        return False  # Judge disagrees even though structural matches.

    result = await ab_test(pathway, "do X", tier2_generator=gen, judge=judge)
    assert result.postconditions_match
    assert result.judge_verdict is False
    assert not result.passed


@pytest.mark.asyncio
async def test_judge_exception_falls_back_to_structural() -> None:
    env = _mk_envelope()
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        return _mk_envelope()

    async def judge(a: Envelope, b: Envelope) -> bool:
        raise RuntimeError("judge crashed")

    result = await ab_test(pathway, "do X", tier2_generator=gen, judge=judge)
    assert result.judge_verdict is None
    assert any("judge_error" in n for n in result.notes)
    # Falls back to structural, which passes here.
    assert result.passed


@pytest.mark.asyncio
async def test_tier0_latency_near_zero() -> None:
    env = _mk_envelope()
    pathway = _mk_pathway(env)

    async def gen(task: str) -> Envelope:
        return _mk_envelope()

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    # Tier-0 is just an attribute lookup — should be sub-millisecond.
    assert result.tier0_latency_ms < 50.0
    # Tier-0 tokens stamped as zero by design.
    assert result.tier0_tokens == 0


@pytest.mark.asyncio
async def test_v028_4_postcondition_equivalence_detects_min_max_drift() -> None:
    """v0.28.4 — _postconditions_equivalent now compares full shape, not
    just (type, expected). Pre-fix two file_size_range postconditions
    with different min/max bounds were treated as equivalent."""
    tier0_env = _mk_envelope(postconditions=[
        Postcondition(type="file_size_range", path="out/x.png", min=100, max=1000),
    ])
    pathway = _mk_pathway(tier0_env)
    drifted_env = _mk_envelope(postconditions=[
        Postcondition(type="file_size_range", path="out/x.png", min=100, max=999_999),
    ])

    async def gen(task: str) -> Envelope:
        return drifted_env

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    assert result.postconditions_match is False, (
        "different max bounds must register as drift; pre-v0.28.4 they did not"
    )


@pytest.mark.asyncio
async def test_v028_4_postcondition_equivalence_detects_path_drift() -> None:
    """v0.28.4 — `path` is now part of the comparison shape."""
    tier0_env = _mk_envelope(postconditions=[
        Postcondition(type="file_exists", path="out/a.png"),
    ])
    pathway = _mk_pathway(tier0_env)
    drifted_env = _mk_envelope(postconditions=[
        Postcondition(type="file_exists", path="out/b.png"),
    ])

    async def gen(task: str) -> Envelope:
        return drifted_env

    result = await ab_test(pathway, "do X", tier2_generator=gen)
    assert result.postconditions_match is False
