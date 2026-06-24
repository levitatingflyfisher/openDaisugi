"""Tiered model routing — escalation ladder (v0.1.3)."""
from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opendaisugi import generate_envelope
from opendaisugi.exceptions import EnvelopeGenerationError, ModelLadderExhausted
from opendaisugi.models import Envelope, FallbackStrategy, Permission, Violation


def _make_envelope(id_: str = "env_t") -> Envelope:
    return Envelope(
        id=id_, generated_by="test", task="t",
        permissions=Permission(
            file_read=[], file_write=[], network=False, network_hosts=[],
            shell=False, shell_allowlist=[],
            max_execution_time_s=30, max_output_size_mb=10,
        ),
        invariants=[], postconditions=[], fallback=FallbackStrategy(),
    )


@pytest.mark.asyncio
async def test_single_model_str_preserves_current_behavior():
    env = _make_envelope()
    fake = MagicMock()
    fake.chat.completions.create = AsyncMock(return_value=env)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake):
        out = await generate_envelope(task="t", model="anthropic/claude-sonnet-4-20250514")

    assert out.id == "env_t"
    assert fake.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_ladder_first_model_success_no_escalation():
    env = _make_envelope("env_sonnet")
    fake = MagicMock()
    fake.chat.completions.create = AsyncMock(return_value=env)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake):
        out = await generate_envelope(
            task="t",
            model=["anthropic/claude-sonnet-4-20250514", "anthropic/claude-opus-4-20250514"],
        )

    assert out.id == "env_sonnet"
    assert fake.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_ladder_escalates_on_instructor_exhaustion():
    env_opus = _make_envelope("env_opus")

    sonnet_client = MagicMock()
    sonnet_client.chat.completions.create = AsyncMock(side_effect=RuntimeError("instructor parse exhausted"))
    opus_client = MagicMock()
    opus_client.chat.completions.create = AsyncMock(return_value=env_opus)

    def client_factory(model):
        return sonnet_client if "sonnet" in model else opus_client

    with patch("opendaisugi.envelope._llm.get_instructor_client", side_effect=client_factory), \
         patch("opendaisugi.envelope._llm.translate_llm_error", side_effect=lambda e: EnvelopeGenerationError(str(e))):
        out = await generate_envelope(
            task="t",
            model=["anthropic/claude-sonnet-4-20250514", "anthropic/claude-opus-4-20250514"],
        )

    assert out.id == "env_opus"


@pytest.mark.asyncio
async def test_ladder_escalates_on_z3_self_consistency_violation():
    bad_env = _make_envelope("env_sonnet_bad")
    good_env = _make_envelope("env_opus_good")

    sonnet_client = MagicMock()
    sonnet_client.chat.completions.create = AsyncMock(return_value=bad_env)
    opus_client = MagicMock()
    opus_client.chat.completions.create = AsyncMock(return_value=good_env)

    def client_factory(model):
        return sonnet_client if "sonnet" in model else opus_client

    def fake_selfcheck(env, timeout_ms=500):
        if env.id == "env_sonnet_bad":
            return [Violation(stage="z3", message="inconsistent", detail={})]
        return []

    with patch("opendaisugi.envelope._llm.get_instructor_client", side_effect=client_factory), \
         patch("opendaisugi.envelope.check_envelope_self_consistency", side_effect=fake_selfcheck):
        out = await generate_envelope(
            task="t",
            model=["anthropic/claude-sonnet-4-20250514", "anthropic/claude-opus-4-20250514"],
        )

    assert out.id == "env_opus_good"


@pytest.mark.asyncio
async def test_ladder_exhaustion_raises_ModelLadderExhausted():
    fake = MagicMock()
    fake.chat.completions.create = AsyncMock(side_effect=RuntimeError("parse fail"))

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake), \
         patch("opendaisugi.envelope._llm.translate_llm_error", side_effect=lambda e: EnvelopeGenerationError(str(e))):
        with pytest.raises(ModelLadderExhausted) as ei:
            await generate_envelope(task="t", model=["m1", "m2"])

    assert ei.value.attempted == ["m1", "m2"]
    assert isinstance(ei.value.last_error, EnvelopeGenerationError)


@pytest.mark.asyncio
async def test_ladder_cache_key_uses_successful_model(tmp_path):
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    env_opus = _make_envelope("env_opus")

    sonnet = MagicMock()
    sonnet.chat.completions.create = AsyncMock(side_effect=RuntimeError("fail"))
    opus = MagicMock()
    opus.chat.completions.create = AsyncMock(return_value=env_opus)

    def factory(model):
        return sonnet if "sonnet" in model else opus

    with patch("opendaisugi.envelope._llm.get_instructor_client", side_effect=factory), \
         patch("opendaisugi.envelope._llm.translate_llm_error", side_effect=lambda e: EnvelopeGenerationError(str(e))):
        await generate_envelope(
            task="t", cache=cache,
            model=["anthropic/claude-sonnet-4-20250514", "anthropic/claude-opus-4-20250514"],
        )

    assert cache.get(
        task="t", context=None, model="anthropic/claude-opus-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    ) is not None
    assert cache.get(
        task="t", context=None, model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    ) is None


@pytest.mark.asyncio
async def test_ladder_cache_hit_at_first_rung_short_circuits_llm(tmp_path):
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    cached = _make_envelope("env_cached")
    cache.put(
        cached,
        task="t", context=None, model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )

    fake = MagicMock()
    fake.chat.completions.create = AsyncMock(side_effect=AssertionError("should not be called"))
    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake):
        out = await generate_envelope(
            task="t", cache=cache,
            model=["anthropic/claude-sonnet-4-20250514", "anthropic/claude-opus-4-20250514"],
        )

    assert out.id == "env_cached"
    assert fake.chat.completions.create.await_count == 0
