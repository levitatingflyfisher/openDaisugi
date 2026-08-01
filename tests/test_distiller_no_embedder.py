"""Distillation must degrade gracefully when the pathway embedder is absent.

``daisugi onboard --allow-no-embedder`` (and ``tend``, and the cron/auto-tend
path) reach ``Distiller.tend()``, which embeds task text to cluster traces. When
``sentence-transformers`` is not installed that embed raises ImportError. It must
NOT crash the run — building the verified journal is the deterministic, non-LLM
half of onboarding and has to complete; distilling zero pathways is a
degradation, not a failure.
"""

import sqlite3

import pytest

from opendaisugi.distiller import Distiller
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
)
from opendaisugi.pathway_store import PathwayStore


def _write_success_trace(journal, task: str):
    env = Envelope(
        generated_by="test",
        task=task,
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan = ActionPlan(
        source="t", task=task,
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )
    result = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id=env.id, plan_id=plan.id, duration_ms=0.1,
    )
    trace_id = journal.log(task=task, envelope=env, plan=plan, result=result)
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (f"run_{trace_id}", trace_id),
        )
    return trace_id


@pytest.mark.asyncio
async def test_tend_without_embedder_degrades_gracefully(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):  # >= min_traces, so tend reaches the embed step
        _write_success_trace(journal, f"find stale tmp files run {i}")

    distiller = Distiller(
        journal=journal, pathway_store=store, model="test-model", min_traces=3
    )

    def _no_embedder(_tasks):
        raise ModuleNotFoundError("No module named 'sentence_transformers'")

    monkeypatch.setattr(distiller, "_embed_tasks", _no_embedder)

    # Must NOT raise — the deterministic journal is already built.
    report = await distiller.tend()

    assert report.created == 0
    assert report.pathways == []
    assert any("embedder" in w.lower() or "sentence-transformers" in w.lower()
               for w in report.warnings), report.warnings
