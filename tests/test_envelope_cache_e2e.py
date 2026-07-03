"""End-to-end integration: facade + cache + parent + summarize all wired together."""

import pytest

from opendaisugi import Daisugi
from opendaisugi.models import Envelope, Permission


@pytest.mark.asyncio
async def test_v012_cache_inheritance_summarize_e2e(tmp_path, mock_llm_client):
    """One Daisugi instance: parent gen, summarized child gen, cached re-fetch.

    Walks the v0.1.2 happy path:
    1. Generate parent envelope (no parent, no summary, cache miss → LLM call).
    2. Generate summarized child (parent set, summarize=True, cache miss → LLM call).
       - Child must be tighter than parent, otherwise EnvelopeInheritanceError.
       - child.summary must be set.
       - child.parent_envelope must equal parent.id.
    3. Re-generate the same summarized child (cache hit → no LLM call).
    """
    d = Daisugi(data_dir=tmp_path)

    # Step 1: parent — broad permissions.
    parent_env = Envelope(
        generated_by="test",
        task="parent: do something general",
        permissions=Permission(
            file_read=["/tmp/**", "/var/log/**"],
            shell=True,
            shell_allowlist=["echo", "cat"],
            network=True,
            max_execution_time_s=60,
        ),
    )
    mock_llm_client.set_next_envelope(parent_env)
    parent = await d.generate_envelope("parent: do something general")
    assert parent.id == parent_env.id
    assert mock_llm_client.call_count == 1

    # Step 2: summarized child — strictly tighter.
    child_env = Envelope(
        generated_by="test",
        task="child: a focused subtask",
        permissions=Permission(
            file_read=["/tmp/**"],
            shell=True,
            shell_allowlist=["echo"],
            network=False,
            max_execution_time_s=30,
        ),
        summary="reads /tmp files only",
    )
    mock_llm_client.set_next_envelope(child_env)
    child = await d.generate_envelope(
        "child: a focused subtask", parent=parent, summarize=True,
    )
    assert child.parent_envelope == parent.id
    assert child.summary == "reads /tmp files only"
    assert mock_llm_client.call_count == 2

    # Step 3: same child query → cache hit, no LLM call.
    same = await d.generate_envelope(
        "child: a focused subtask", parent=parent, summarize=True,
    )
    assert mock_llm_client.call_count == 2  # unchanged
    assert same.id == child.id
    assert same.parent_envelope == parent.id
    assert same.summary == "reads /tmp files only"

    # Cache stats sanity.
    stats = d.cache.stats()
    assert stats["entries"] == 2  # parent + child stored
