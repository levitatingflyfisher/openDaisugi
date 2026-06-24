"""Tests for stakes policy (v0.1.3)."""
from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opendaisugi.defaults import DEFAULT_LOW_STAKES_ENVELOPE
from opendaisugi.models import Envelope


def test_default_low_stakes_envelope_shape():
    env = DEFAULT_LOW_STAKES_ENVELOPE
    assert isinstance(env, Envelope)
    assert env.id == "env_default_low_stakes"
    assert env.generated_by == "opendaisugi-library-default"
    assert env.permissions.network is False
    assert env.permissions.shell is False
    assert env.permissions.shell_allowlist == []
    assert env.permissions.file_read == ["**"]
    assert env.permissions.file_write == ["/tmp/**", "./out/**"]
    assert env.permissions.max_execution_time_s == 30
    assert env.permissions.max_output_size_mb == 10
    assert env.invariants == []
    assert env.postconditions == []
    assert env.summary == "Default low-stakes envelope (dev/sandbox use)"


@pytest.mark.asyncio
async def test_medium_stakes_is_default_and_uses_cache(tmp_path, sample_envelope):
    from opendaisugi import generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(task="x", cache=cache)
        await generate_envelope(task="x", cache=cache)

    assert fake_client.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_high_stakes_bypasses_cache_read(tmp_path, sample_envelope):
    from opendaisugi import generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    cache.put(
        sample_envelope,
        task="x", context=None, model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )

    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(task="x", cache=cache, stakes="high")

    assert fake_client.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_high_stakes_overwrites_cache_on_write(tmp_path, sample_envelope):
    from opendaisugi import generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache
    from opendaisugi.models import Envelope

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    old = sample_envelope.model_copy(update={"id": "env_old"})
    cache.put(
        old,
        task="x", context=None, model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )

    fresh = sample_envelope.model_copy(update={"id": "env_fresh"})
    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=fresh)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(task="x", cache=cache, stakes="high")

    got = cache.get(
        task="x", context=None, model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )
    assert got is not None
    assert got.id == "env_fresh"


@pytest.mark.asyncio
async def test_medium_stakes_explicit_same_as_default(tmp_path, sample_envelope):
    from opendaisugi import generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        await generate_envelope(task="x", cache=cache, stakes="medium")
        await generate_envelope(task="x", cache=cache, stakes="medium")

    assert fake_client.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_low_stakes_returns_configured_envelope_without_llm(sample_envelope):
    from opendaisugi import generate_envelope

    fake_client = MagicMock()
    fake_client.chat.completions.create = AsyncMock(return_value=sample_envelope)

    with patch("opendaisugi.envelope._llm.get_instructor_client", return_value=fake_client):
        env = await generate_envelope(
            task="anything",
            stakes="low",
            low_stakes_envelope=sample_envelope,
        )

    assert fake_client.chat.completions.create.await_count == 0
    assert env.id == sample_envelope.id


@pytest.mark.asyncio
async def test_low_stakes_raises_when_not_configured():
    from opendaisugi import generate_envelope
    from opendaisugi.exceptions import LowStakesNotConfigured

    with pytest.raises(LowStakesNotConfigured, match="with_default_low_stakes"):
        await generate_envelope(task="anything", stakes="low")


@pytest.mark.asyncio
async def test_low_stakes_returns_independent_copy(sample_envelope):
    from opendaisugi import generate_envelope

    env = await generate_envelope(
        task="anything", stakes="low", low_stakes_envelope=sample_envelope,
    )
    assert env is not sample_envelope  # model_copy produced an independent instance


@pytest.mark.asyncio
async def test_low_stakes_skips_cache(tmp_path, sample_envelope):
    from opendaisugi import generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="2026-04-15")
    await generate_envelope(
        task="x", stakes="low", low_stakes_envelope=sample_envelope, cache=cache,
    )
    assert cache.stats()["entries"] == 0


@pytest.mark.asyncio
async def test_low_stakes_with_parent_warns_and_ignores_parent(sample_envelope):
    import warnings
    from opendaisugi import generate_envelope
    from opendaisugi.exceptions import StakesInheritanceWarning

    parent = sample_envelope.model_copy(update={"id": "env_parent"})
    with warnings.catch_warnings(record=True) as rec:
        warnings.simplefilter("always")
        env = await generate_envelope(
            task="x", stakes="low",
            low_stakes_envelope=sample_envelope, parent=parent,
        )

    assert any(issubclass(w.category, StakesInheritanceWarning) for w in rec)
    # Parent was ignored — returned envelope matches the low-stakes one (no parent merging)
    assert env.parent_envelope is None
    assert env.id == sample_envelope.id
