"""Tests for the LiteLLMTier1Provider adapter (v0.4.0).

These tests monkeypatch ``get_instructor_client`` so no real LLM call is
made — the adapter's job is to produce an envelope on success, decline on
any failure, and honor the configured timeout.
"""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch

import pytest

from opendaisugi.models import Envelope, Permission, Postcondition
from opendaisugi.tier1 import LiteLLMTier1Provider, Tier1Provider


def _mk_envelope() -> Envelope:
    return Envelope(
        generated_by="tier1-source",
        task="t",
        permissions=Permission(file_read=[], file_write=[], network=False, shell=False),
        invariants=[],
        postconditions=[Postcondition(type="exit_code", expected=0)],
    )


@pytest.mark.asyncio
async def test_happy_path_returns_envelope() -> None:
    provider = LiteLLMTier1Provider("ollama/llama3.2:3b")
    with patch("opendaisugi.llm.get_instructor_client") as factory:
        factory.return_value.chat.completions.create = AsyncMock(return_value=_mk_envelope())
        env = await provider.generate_envelope("do thing")
    assert env is not None
    assert env.task == "t"


@pytest.mark.asyncio
async def test_exception_returns_none() -> None:
    provider = LiteLLMTier1Provider("ollama/llama3.2:3b")
    with patch("opendaisugi.llm.get_instructor_client") as factory:
        factory.return_value.chat.completions.create = AsyncMock(
            side_effect=RuntimeError("connection refused"),
        )
        assert await provider.generate_envelope("t") is None


@pytest.mark.asyncio
async def test_timeout_returns_none() -> None:
    """A hung endpoint must not block envelope generation."""
    async def never(*args, **kwargs):
        await asyncio.sleep(10)

    provider = LiteLLMTier1Provider("ollama/llama3.2:3b", timeout_s=0.05)
    with patch("opendaisugi.llm.get_instructor_client") as factory:
        factory.return_value.chat.completions.create = never
        assert await provider.generate_envelope("t") is None


@pytest.mark.asyncio
async def test_base_url_and_api_key_forwarded() -> None:
    """llamafile/llama.cpp path: base_url + api_key must reach the client call."""
    provider = LiteLLMTier1Provider(
        "openai/qwen2-1.5b",
        base_url="http://localhost:8080/v1",
        api_key="sk-local",
    )
    with patch("opendaisugi.llm.get_instructor_client") as factory:
        create = AsyncMock(return_value=_mk_envelope())
        factory.return_value.chat.completions.create = create
        await provider.generate_envelope("t")
        call_kwargs = create.await_args.kwargs
        assert call_kwargs["base_url"] == "http://localhost:8080/v1"
        assert call_kwargs["api_key"] == "sk-local"


def test_local_endpoint_bare_model_gets_openai_prefix() -> None:
    # The headline v0.30 bug: a bare --model + local endpoint must route via
    # the openai/ provider prefix, else litellm errors and the qualification
    # gate rejects every model for the wrong reason.
    p = LiteLLMTier1Provider("qwen2.5-1.5b", base_url="http://localhost:8080/v1")
    assert p.model == "openai/qwen2.5-1.5b"


def test_local_endpoint_prefixed_model_left_alone() -> None:
    p = LiteLLMTier1Provider("openai/qwen2-1.5b", base_url="http://localhost:8080/v1")
    assert p.model == "openai/qwen2-1.5b"


def test_bare_model_without_base_url_is_untouched() -> None:
    # No local endpoint → no auto-prefix (preserves existing non-local behavior).
    p = LiteLLMTier1Provider("ollama/llama3.2:3b")
    assert p.model == "ollama/llama3.2:3b"


def test_default_name_derived_from_model() -> None:
    """Cache-key isolation relies on name being stable + unique per-config."""
    p = LiteLLMTier1Provider("ollama/llama3.2:3b")
    assert p.name == "litellm:ollama/llama3.2:3b"


def test_custom_name_override() -> None:
    p = LiteLLMTier1Provider("ollama/llama3.2:3b", name="my-box")
    assert p.name == "my-box"


def test_satisfies_protocol() -> None:
    assert isinstance(LiteLLMTier1Provider("ollama/foo"), Tier1Provider)
