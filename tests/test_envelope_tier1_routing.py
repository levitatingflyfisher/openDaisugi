"""Routing tests for the Tier-1 slot in generate_envelope (v0.4.0)."""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from opendaisugi.envelope import generate_envelope
from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key
from opendaisugi.models import Envelope, Permission, Postcondition


def _mk_envelope(*, task: str, generated_by: str = "anthropic/claude-sonnet-4-20250514") -> Envelope:
    return Envelope(
        generated_by=generated_by,
        task=task,
        permissions=Permission(file_read=[], file_write=[], network=False, shell=False),
        invariants=[],
        postconditions=[Postcondition(type="exit_code", expected=0)],
    )


class _StaticTier1:
    """Tier-1 provider that returns a fixed envelope."""
    def __init__(self, name: str, env: Envelope | None) -> None:
        self.name = name
        self._env = env
        self.calls = 0

    async def generate_envelope(self, task, *, context=None):
        self.calls += 1
        if self._env is None:
            return None
        return self._env.model_copy()


class _BoomTier1:
    """Tier-1 provider that raises. Must be treated as a decline."""
    name = "boom"

    async def generate_envelope(self, task, *, context=None):
        raise RuntimeError("synthetic failure")


@pytest.mark.asyncio
async def test_declining_tier1_does_not_short_circuit_ladder() -> None:
    """Provider returning None declines — Tier-2 ladder must run."""
    decliner = _StaticTier1("decliner", None)
    produced = _mk_envelope(task="t")
    with patch("opendaisugi.envelope._llm.get_instructor_client") as mock_client_factory:
        mock_client = mock_client_factory.return_value
        mock_client.chat.completions.create = AsyncMock(return_value=produced)
        result = await generate_envelope(task="t", tier1=decliner)
    assert result.generated_by.startswith("anthropic/")
    assert mock_client.chat.completions.create.await_count == 1
    assert decliner.calls == 1


@pytest.mark.asyncio
async def test_tier1_success_short_circuits_ladder() -> None:
    """When Tier-1 returns an envelope, Tier-2 must not be called."""
    t1_env = _mk_envelope(task="t", generated_by="will-be-overwritten")
    provider = _StaticTier1("mycheapmodel", t1_env)
    with patch("opendaisugi.envelope._llm.get_instructor_client") as mock_factory:
        mock_client = mock_factory.return_value
        mock_client.chat.completions.create = AsyncMock()
        result = await generate_envelope(task="t", tier1=provider)
    assert result.generated_by == "tier1:mycheapmodel"
    assert mock_client.chat.completions.create.await_count == 0
    assert provider.calls == 1


@pytest.mark.asyncio
async def test_tier1_exception_falls_through_to_tier2() -> None:
    """An adapter exception must degrade gracefully to Tier-2."""
    produced = _mk_envelope(task="t")
    with patch("opendaisugi.envelope._llm.get_instructor_client") as mock_factory:
        mock_client = mock_factory.return_value
        mock_client.chat.completions.create = AsyncMock(return_value=produced)
        result = await generate_envelope(task="t", tier1=_BoomTier1())
    assert not result.generated_by.startswith("tier1:")
    assert mock_client.chat.completions.create.await_count == 1


@pytest.mark.asyncio
async def test_high_stakes_bypasses_tier1() -> None:
    """stakes='high' must skip Tier-1 entirely and go straight to the ladder."""
    t1_env = _mk_envelope(task="t")
    provider = _StaticTier1("shouldntrun", t1_env)
    produced = _mk_envelope(task="t")
    with patch("opendaisugi.envelope._llm.get_instructor_client") as mock_factory:
        mock_factory.return_value.chat.completions.create = AsyncMock(return_value=produced)
        result = await generate_envelope(task="t", tier1=provider, stakes="high")
    assert provider.calls == 0
    assert not result.generated_by.startswith("tier1:")


@pytest.mark.asyncio
async def test_tier1_inconsistent_envelope_falls_through(tmp_path) -> None:
    """Envelope that fails Z3 self-consistency is treated as decline."""
    # Permissions: shell=False but shell_allowlist has entries — self-inconsistent
    bad = Envelope(
        generated_by="tier1-source",
        task="t",
        permissions=Permission(
            file_read=[], file_write=[],
            network=False, shell=False,
            shell_allowlist=["grep"],
        ),
        invariants=[],
        postconditions=[Postcondition(type="exit_code", expected=0)],
    )
    provider = _StaticTier1("shady", bad)
    produced = _mk_envelope(task="t")
    with patch("opendaisugi.envelope._llm.get_instructor_client") as mock_factory:
        mock_factory.return_value.chat.completions.create = AsyncMock(return_value=produced)
        result = await generate_envelope(task="t", tier1=provider)
    assert mock_factory.return_value.chat.completions.create.await_count == 1
    assert not result.generated_by.startswith("tier1:")


@pytest.mark.asyncio
async def test_tier1_result_cached_under_provider_name_key(tmp_path) -> None:
    """Two providers with different names must not share cache entries."""
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="test")
    env_a = _mk_envelope(task="t")
    provider_a = _StaticTier1("alpha", env_a)
    result_a = await generate_envelope(task="t", tier1=provider_a, cache=cache)
    assert result_a.generated_by == "tier1:alpha"

    # Different provider, same task — must NOT hit the cache.
    env_b = _mk_envelope(task="t")
    provider_b = _StaticTier1("beta", env_b)
    result_b = await generate_envelope(task="t", tier1=provider_b, cache=cache)
    assert result_b.generated_by == "tier1:beta"
    assert provider_b.calls == 1  # called, didn't reuse alpha's cached entry


@pytest.mark.asyncio
async def test_tier1_cache_hit_reused(tmp_path) -> None:
    """Same provider name, same task, cache populated — provider not called twice."""
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="test")
    env = _mk_envelope(task="t")
    provider = _StaticTier1("alpha", env)
    await generate_envelope(task="t", tier1=provider, cache=cache)
    await generate_envelope(task="t", tier1=provider, cache=cache)
    # Second call should hit cache, bypassing the provider.
    assert provider.calls == 1


def test_make_cache_key_backward_compat() -> None:
    """Omitting tier1_provider_name must produce the pre-v0.4 key (byte-for-byte)."""
    k1 = make_cache_key(
        task="t", context=None, model="m",
        parent_envelope_id=None, summarize=False,
    )
    k2 = make_cache_key(
        task="t", context=None, model="m",
        parent_envelope_id=None, summarize=False,
        tier1_provider_name=None,
    )
    assert k1 == k2


def test_make_cache_key_tier1_name_matters() -> None:
    """Different tier1 names must produce different keys."""
    k_a = make_cache_key(
        task="t", context=None, model="m",
        parent_envelope_id=None, summarize=False,
        tier1_provider_name="alpha",
    )
    k_b = make_cache_key(
        task="t", context=None, model="m",
        parent_envelope_id=None, summarize=False,
        tier1_provider_name="beta",
    )
    assert k_a != k_b
