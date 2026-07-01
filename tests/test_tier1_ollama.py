"""Tests for OllamaTier1Provider (v0.23)."""

import pytest

from opendaisugi.tier1 import LiteLLMTier1Provider, OllamaTier1Provider


def test_ollama_defaults_to_localhost_no_api_key():
    p = OllamaTier1Provider()
    assert isinstance(p, LiteLLMTier1Provider)
    assert p.model == "ollama/llama3.2:3b"
    assert p.base_url == "http://localhost:11434"
    assert p.api_key is None


def test_ollama_accepts_bare_model_name():
    p = OllamaTier1Provider(model="qwen2.5:7b")
    assert p.model == "ollama/qwen2.5:7b"


def test_ollama_accepts_already_prefixed_model_name():
    p = OllamaTier1Provider(model="ollama/llama3.1:8b")
    assert p.model == "ollama/llama3.1:8b"
    # No double-prefix.
    assert not p.model.startswith("ollama/ollama/")


def test_ollama_default_name_includes_model():
    p = OllamaTier1Provider(model="phi4:14b")
    assert "phi4" in p.name


def test_ollama_overrides_base_url_and_timeout():
    p = OllamaTier1Provider(
        model="llama3.2:3b",
        base_url="http://my-llama-box.lan:11434",
        timeout_s=120.0,
        name="lan-llama",
    )
    assert p.base_url == "http://my-llama-box.lan:11434"
    assert p.timeout_s == 120.0
    assert p.name == "lan-llama"


@pytest.mark.asyncio
async def test_ollama_provider_is_a_valid_tier1_protocol():
    """Quack-test: implements the Tier1Provider protocol."""
    from opendaisugi.tier1 import Tier1Provider
    p = OllamaTier1Provider()
    assert isinstance(p, Tier1Provider)
