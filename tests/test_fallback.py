"""Unit tests for fallback handlers."""

import pytest
from unittest.mock import AsyncMock, patch

from opendaisugi.fallback import FallbackOutcome, HaltHandler, RecomputeHandler
from opendaisugi.models import (
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
    Violation,
)


def _env():
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


def _step():
    return ShellStep(id="s1", command="rm -rf /")


def _failed_result(envelope):
    return VerificationResult(
        ok=False,
        violations=[Violation(stage="permissions", message="not allowed", detail={})],
        warnings=[],
        envelope_id=envelope.id,
        plan_id="plan_x",
        duration_ms=1.0,
    )


def test_fallback_outcome_halted():
    outcome = FallbackOutcome(action="halted")
    assert outcome.action == "halted"
    assert outcome.replacement_step is None
    assert outcome.replacement_result is None


def test_fallback_outcome_recomputed():
    replacement = ShellStep(id="s1_v2", command="echo safe")
    vr = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id="e", plan_id="p", duration_ms=0.5,
    )
    outcome = FallbackOutcome(
        action="recomputed",
        replacement_step=replacement,
        replacement_result=vr,
    )
    assert outcome.action == "recomputed"
    assert outcome.replacement_step.id == "s1_v2"


@pytest.mark.asyncio
async def test_halt_handler_always_halts():
    env = _env()
    step = _step()
    result = _failed_result(env)
    handler = HaltHandler()
    outcome = await handler.handle(step, result, env)
    assert outcome.action == "halted"
    assert outcome.replacement_step is None
    assert outcome.replacement_result is None


def _passing_result(envelope):
    return VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id=envelope.id, plan_id="plan_x", duration_ms=0.5,
    )


@pytest.mark.asyncio
async def test_recompute_handler_success():
    """RecomputeHandler returns 'recomputed' when replacement passes verify."""
    env = _env()
    step = _step()
    result = _failed_result(env)
    replacement = ShellStep(id="s1_v2", command="echo safe")

    fake_completions = AsyncMock(return_value=replacement)
    fake_client = type("C", (), {"chat": type("Ch", (), {"completions": type("Co", (), {"create": fake_completions})()})()})()

    with patch("opendaisugi.fallback._get_recompute_client", return_value=fake_client):
        with patch("opendaisugi.fallback.verify") as mock_verify:
            mock_verify.return_value = _passing_result(env)
            handler = RecomputeHandler(model="anthropic/claude-sonnet-4-20250514")
            outcome = await handler.handle(step, result, env)

    assert outcome.action == "recomputed"
    assert outcome.replacement_step.id == "s1_v2"
    assert outcome.replacement_result.ok is True


@pytest.mark.asyncio
async def test_recompute_handler_replacement_fails_verification():
    """RecomputeHandler returns 'halted' when replacement also fails verify."""
    env = _env()
    step = _step()
    result = _failed_result(env)
    replacement = ShellStep(id="s1_v2", command="rm -rf /home")

    fake_completions = AsyncMock(return_value=replacement)
    fake_client = type("C", (), {"chat": type("Ch", (), {"completions": type("Co", (), {"create": fake_completions})()})()})()

    with patch("opendaisugi.fallback._get_recompute_client", return_value=fake_client):
        with patch("opendaisugi.fallback.verify") as mock_verify:
            mock_verify.return_value = _failed_result(env)
            handler = RecomputeHandler(model="anthropic/claude-sonnet-4-20250514")
            outcome = await handler.handle(step, result, env)

    assert outcome.action == "halted"
    assert outcome.replacement_step is None
    assert outcome.replacement_result is None


@pytest.mark.asyncio
async def test_recompute_handler_llm_error_returns_halted():
    """RecomputeHandler returns 'halted' if the LLM call itself fails."""
    env = _env()
    step = _step()
    result = _failed_result(env)

    fake_completions = AsyncMock(side_effect=Exception("LLM down"))
    fake_client = type("C", (), {"chat": type("Ch", (), {"completions": type("Co", (), {"create": fake_completions})()})()})()

    with patch("opendaisugi.fallback._get_recompute_client", return_value=fake_client):
        handler = RecomputeHandler(model="anthropic/claude-sonnet-4-20250514")
        outcome = await handler.handle(step, result, env)

    assert outcome.action == "halted"


@pytest.mark.asyncio
async def test_recompute_handler_include_refinement_false():
    """When include_refinement=False, violations are not in the LLM prompt."""
    env = _env()
    env.fallback.include_refinement = False
    step = _step()
    result = _failed_result(env)
    replacement = ShellStep(id="s1_v2", command="echo ok")

    captured_messages = []

    async def capturing_create(_self=None, **kwargs):
        captured_messages.extend(kwargs.get("messages", []))
        return replacement

    fake_client = type("C", (), {"chat": type("Ch", (), {"completions": type("Co", (), {"create": capturing_create})()})()})()

    with patch("opendaisugi.fallback._get_recompute_client", return_value=fake_client):
        with patch("opendaisugi.fallback.verify") as mock_verify:
            mock_verify.return_value = _passing_result(env)
            handler = RecomputeHandler(model="anthropic/claude-sonnet-4-20250514")
            outcome = await handler.handle(step, result, env)

    user_msg = next((m["content"] for m in captured_messages if m["role"] == "user"), "")
    assert "not allowed" not in user_msg
    assert outcome.action == "recomputed"
