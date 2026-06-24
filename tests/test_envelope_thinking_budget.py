"""Tests for the per-provider thinking-budget mapping (v0.1.3)."""
from __future__ import annotations

import logging
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opendaisugi.thinking import ThinkingBudget, thinking_kwargs


class TestAnthropic:
    def test_light_returns_empty_dict(self):
        assert thinking_kwargs("anthropic/claude-sonnet-4-20250514", "light") == {}

    def test_standard_returns_empty_dict(self):
        assert thinking_kwargs("claude-opus-4-20250514", "standard") == {}

    def test_deep_enables_extended_thinking(self):
        out = thinking_kwargs("anthropic/claude-sonnet-4-20250514", "deep")
        assert out == {"thinking": {"type": "enabled", "budget_tokens": 16000}}


class TestOpenAIReasoning:
    def test_light_maps_to_low(self):
        assert thinking_kwargs("openai/o3-mini", "light") == {"reasoning_effort": "low"}

    def test_standard_maps_to_medium(self):
        assert thinking_kwargs("o3-mini", "standard") == {"reasoning_effort": "medium"}

    def test_deep_maps_to_high(self):
        assert thinking_kwargs("openai/o4-mini", "deep") == {"reasoning_effort": "high"}


class TestGeminiThinking:
    def test_light_zero_budget(self):
        out = thinking_kwargs("gemini-2.5-pro", "light")
        assert out == {"thinking_config": {"thinking_budget": 0}}

    def test_standard_mid_budget(self):
        out = thinking_kwargs("gemini-2.0-flash-thinking", "standard")
        assert out == {"thinking_config": {"thinking_budget": 4000}}

    def test_deep_with_thoughts(self):
        out = thinking_kwargs("gemini-2.5-pro", "deep")
        assert out == {"thinking_config": {"thinking_budget": 16000, "include_thoughts": True}}


class TestUnsupported:
    def test_unsupported_returns_empty(self):
        assert thinking_kwargs("gpt-4o", "deep") == {}

    def test_unsupported_logs_warning_once(self, caplog):
        import opendaisugi.thinking as tk
        tk._LOGGED_UNSUPPORTED.clear()

        with caplog.at_level(logging.WARNING, logger="opendaisugi.thinking"):
            thinking_kwargs("gpt-4o", "deep")
            thinking_kwargs("gpt-4o", "deep")
            thinking_kwargs("gpt-4o", "light")  # same model, different budget — still dedupe

        messages = [r.getMessage() for r in caplog.records if "gpt-4o" in r.getMessage()]
        assert len(messages) == 1
        assert "has no effect" in messages[0]


"""Integration with generate_envelope."""


@pytest.mark.asyncio
async def test_thinking_budget_passes_thinking_kwarg_to_anthropic(sample_envelope):
    from opendaisugi import generate_envelope

    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(
            task="do a thing",
            model="anthropic/claude-sonnet-4-20250514",
            thinking_budget="deep",
        )

    kwargs = fake_client.chat.completions.create.call_args.kwargs
    assert "thinking" in kwargs
    assert kwargs["thinking"] == {"type": "enabled", "budget_tokens": 16000}


@pytest.mark.asyncio
async def test_thinking_budget_standard_anthropic_no_kwarg(sample_envelope):
    from opendaisugi import generate_envelope

    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(
            task="do a thing",
            model="anthropic/claude-sonnet-4-20250514",
            thinking_budget="standard",
        )

    kwargs = fake_client.chat.completions.create.call_args.kwargs
    assert "thinking" not in kwargs
