"""Integration tests for generate_envelope's cache= and parent= wiring (v0.1.2 Task 5).

Covers the cache hit short-circuit, cache miss + store, parent-id stamping,
inheritance verification, and the "violation does not cache" guarantee.
"""

import pytest

from opendaisugi.envelope import generate_envelope
from opendaisugi.envelope_cache import EnvelopeCache
from opendaisugi.inheritance import EnvelopeInheritanceError
from opendaisugi.models import Envelope, Permission


async def test_generate_with_parent_sets_parent_envelope_id(mock_llm_client):
    parent = Envelope(
        generated_by="t", task="parent task",
        permissions=Permission(file_read=["/tmp/**"], shell=True, shell_allowlist=["echo"]),
    )
    # Configure mock to return a child envelope strictly tighter than parent.
    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="child task",
        permissions=Permission(file_read=["/tmp/**"], shell=False),
    ))

    child = await generate_envelope("child task", parent=parent)
    assert child.parent_envelope == parent.id


async def test_generate_with_parent_raises_on_relaxation(mock_llm_client):
    parent = Envelope(
        generated_by="t", task="parent task",
        permissions=Permission(network=False),
    )
    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="child task",
        permissions=Permission(network=True),  # relaxes parent
    ))

    with pytest.raises(EnvelopeInheritanceError) as exc_info:
        await generate_envelope("child task", parent=parent)
    assert any("network" in v.message for v in exc_info.value.violations)


async def test_generate_without_parent_unchanged_behavior(mock_llm_client):
    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="standalone",
        permissions=Permission(),
    ))
    env = await generate_envelope("standalone")
    assert env.parent_envelope is None  # not stamped


async def test_cache_hit_short_circuits_llm_call(tmp_path, mock_llm_client):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = Envelope(generated_by="t", task="cached task", permissions=Permission())
    cache.put(
        env,
        task="cached task",
        context=None,
        model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None,
        summarize=False,
    )

    # Mock should NOT be called — cache hit returns early.
    result = await generate_envelope("cached task", cache=cache)
    assert result.id == env.id
    assert mock_llm_client.call_count == 0


async def test_cache_miss_calls_llm_then_stores(tmp_path, mock_llm_client):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="fresh task", permissions=Permission(),
    ))

    result = await generate_envelope("fresh task", cache=cache)
    assert mock_llm_client.call_count == 1
    # Second call hits cache, no second LLM call.
    result2 = await generate_envelope("fresh task", cache=cache)
    assert mock_llm_client.call_count == 1
    assert result2.id == result.id


async def test_inheritance_violation_does_not_cache(tmp_path, mock_llm_client):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    parent = Envelope(
        generated_by="t", task="p",
        permissions=Permission(network=False),
    )
    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="c",
        permissions=Permission(network=True),  # relaxation
    ))

    with pytest.raises(EnvelopeInheritanceError):
        await generate_envelope("c", parent=parent, cache=cache)
    assert cache.stats()["entries"] == 0  # nothing stored
