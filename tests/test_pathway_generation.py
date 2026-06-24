"""Tests for generate_envelope() pathway consumption (v0.3.0)."""

import time

import numpy as np
import pytest

from opendaisugi.envelope import generate_envelope
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _make_pathway():
    env = Envelope(
        generated_by="distilled", task="T",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="find /tmp")])
    return CompiledPathway(
        id="pathway_xyz00000",
        task_description="find stale temp files",
        task_embedding=[1.0, 0.0, 0.0],
        envelope=env,
        plan_template=plan,
        source_trace_ids=["trace_1"],
        distilled_at=time.time(),
    )


@pytest.mark.asyncio
async def test_generate_envelope_uses_pathway_when_match_above_threshold(
    tmp_path, mock_llm_client, monkeypatch,
):
    store = PathwayStore(tmp_path / "p.db")
    p = _make_pathway()
    store.put(p)
    monkeypatch.setattr(
        store, "_embed_query", lambda _: np.array([1.0, 0.0, 0.0])
    )

    env = await generate_envelope(
        task="find stale tmp files",
        pathway_store=store,
        model="test-model",
    )
    assert env.generated_by.startswith("compiled-pathway:")
    assert env.generated_by.endswith(p.id)
    # Mock LLM should not have been called (pathway short-circuits).
    assert mock_llm_client.call_count == 0


@pytest.mark.asyncio
async def test_generate_envelope_falls_through_when_no_match(
    tmp_path, mock_llm_client, monkeypatch,
):
    store = PathwayStore(tmp_path / "p.db")
    # Empty store — find returns None.
    env = await generate_envelope(
        task="unseen task",
        pathway_store=store,
        model="test-model",
    )
    # LLM called, env comes from mock.
    assert mock_llm_client.call_count == 1
    assert not env.generated_by.startswith("compiled-pathway:")


@pytest.mark.asyncio
async def test_generate_envelope_increments_hit_count(
    tmp_path, mock_llm_client, monkeypatch,
):
    store = PathwayStore(tmp_path / "p.db")
    p = _make_pathway()
    store.put(p)
    monkeypatch.setattr(store, "_embed_query", lambda _: np.array([1.0, 0.0, 0.0]))

    await generate_envelope(task="find stale tmp files", pathway_store=store, model="test-model")
    import sqlite3
    with sqlite3.connect(tmp_path / "p.db") as con:
        hits = con.execute("SELECT hit_count FROM pathways WHERE id = ?", (p.id,)).fetchone()[0]
    assert hits == 1
