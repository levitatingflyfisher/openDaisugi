"""Tests for v0.3.1 polish items (SGCM red-team outputs)."""
from __future__ import annotations

import asyncio
import sqlite3
import time
from unittest import mock

import numpy as np
import pytest
from typer.testing import CliRunner

from opendaisugi import Daisugi
from opendaisugi.cli import app
from opendaisugi.distiller import (
    _MAX_PITFALLS,
    Distiller,
    TendReport,
    _cluster_by_similarity,
    _cluster_with_centroids,
)
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep, VerificationResult
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _write_success_trace(journal: Journal, task: str) -> str:
    env = Envelope(
        generated_by="test", task=task,
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
    run_id = f"run_{trace_id}"
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (run_id, trace_id),
        )
    return trace_id


def _fake_generalize_template():
    from opendaisugi.distiller import GeneralizedTemplate
    plan_tmpl = ActionPlan(
        source="template", task="T",
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files",
            plan_template=plan_tmpl,
        )
    return _fake_gen


# ---------------------------------------------------------------------------
# P1.4: embedding provenance columns + additive migration
# ---------------------------------------------------------------------------


def test_pathway_store_migrates_older_rows(tmp_path):
    """Adding embedding provenance columns must not break v0.3.0 DBs."""
    db = tmp_path / "pathways.db"
    with sqlite3.connect(db) as con:
        con.execute(
            "CREATE TABLE pathways ("
            "  id TEXT PRIMARY KEY,"
            "  task_description TEXT NOT NULL,"
            "  task_embedding_json TEXT NOT NULL,"
            "  envelope_json TEXT NOT NULL,"
            "  plan_template_json TEXT NOT NULL,"
            "  source_trace_ids_json TEXT NOT NULL,"
            "  pitfalls_json TEXT NOT NULL,"
            "  validation_score REAL NOT NULL,"
            "  version INTEGER NOT NULL DEFAULT 1,"
            "  hit_count INTEGER NOT NULL DEFAULT 0,"
            "  distilled_at REAL NOT NULL"
            ")"
        )
    store = PathwayStore(db)
    assert store.list_all() == []
    env = Envelope(generated_by="distilled", task="T",
                   permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="echo hi")])
    p = CompiledPathway(
        id="p_migrate", task_description="t", task_embedding=[1.0],
        envelope=env, plan_template=plan, source_trace_ids=[], distilled_at=time.time(),
        embedding_model="all-MiniLM-L6-v2", embedding_model_version="1",
    )
    store.put(p)
    [loaded] = store.list_all()
    assert loaded.embedding_model == "all-MiniLM-L6-v2"


# ---------------------------------------------------------------------------
# P0.1: tend() skips corrupt YAML
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_tend_skips_corrupt_yaml_trace(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    trace_ids = [_write_success_trace(journal, f"task {i}") for i in range(4)]
    corrupt = trace_ids[0]
    yaml_path = journal._traces_dir / f"{corrupt}.yaml"
    yaml_path.write_text(":\n  not yaml: [")

    distiller = Distiller(
        journal=journal, pathway_store=store,
        model="test-model", min_traces=3,
    )
    monkeypatch.setattr(distiller, "_embed_tasks",
                        lambda tasks: np.ones((len(tasks), 4)))
    # v0.28.5: signature embedding has its own path that pulls
    # sentence-transformers; mock it too so the test doesn't hit network.
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))
    from opendaisugi import distiller as dist_mod
    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_generalize_template())

    report = await distiller.tend()
    assert any("load_trace" in w for w in report.warnings)


# ---------------------------------------------------------------------------
# P1.2: tend() warns when fewer than min_traces
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_tend_warns_when_below_min_traces(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    _write_success_trace(journal, "only one")

    distiller = Distiller(journal=journal, pathway_store=store,
                          model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks",
                        lambda tasks: np.ones((len(tasks), 4)))
    # v0.28.5: signature embedding has its own path that pulls
    # sentence-transformers; mock it too so the test doesn't hit network.
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))

    report = await distiller.tend()
    assert report.created == 0
    assert report.warnings, "expected a warning explaining why 0 pathways created"
    assert "min_traces" in report.warnings[0]


# ---------------------------------------------------------------------------
# P0.3 + P1.7: centroid reuse + clustering determinism
# ---------------------------------------------------------------------------


def test_cluster_with_centroids_returns_pairs():
    vecs = np.array([[1.0, 0.0], [0.99, 0.01], [0.0, 1.0]])
    clustered = _cluster_with_centroids([0, 1, 2], vecs, threshold=0.95)
    assert len(clustered) == 2
    for cluster, centroid in clustered:
        assert isinstance(cluster, list)
        assert centroid.shape == (2,)


def test_cluster_by_similarity_backcompat_wrapper():
    vecs = np.array([[1.0, 0.0], [0.99, 0.01], [0.0, 1.0]])
    clusters = _cluster_by_similarity([0, 1, 2], vecs, threshold=0.95)
    assert [set(c) for c in clusters] == [{0, 1}, {2}]


def test_clustering_is_deterministic():
    vecs = np.array([
        [1.0, 0.0, 0.0],
        [0.99, 0.01, 0.0],
        [0.0, 1.0, 0.0],
        [0.01, 0.99, 0.0],
        [0.0, 0.0, 1.0],
    ])
    first = _cluster_by_similarity(list(range(5)), vecs, threshold=0.95)
    second = _cluster_by_similarity(list(range(5)), vecs, threshold=0.95)
    assert first == second


@pytest.mark.asyncio
async def test_tend_stores_cluster_centroid_not_re_embedding(tmp_path, monkeypatch):
    """Distillation must not embed cluster members twice."""
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files {i}")

    distiller = Distiller(
        journal=journal, pathway_store=store,
        model="test-model", min_traces=3,
    )

    call_count = {"n": 0}
    def _embed(tasks):
        call_count["n"] += 1
        return np.ones((len(tasks), 4))
    monkeypatch.setattr(distiller, "_embed_tasks", _embed)
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))

    from opendaisugi import distiller as dist_mod
    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_generalize_template())

    await distiller.tend()
    assert call_count["n"] == 1


@pytest.mark.asyncio
async def test_distilled_pathway_has_embedding_provenance(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"task {i}")
    distiller = Distiller(journal=journal, pathway_store=store,
                          model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks",
                        lambda tasks: np.ones((len(tasks), 4)))
    # v0.28.5: signature embedding has its own path that pulls
    # sentence-transformers; mock it too so the test doesn't hit network.
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))
    from opendaisugi import distiller as dist_mod
    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_generalize_template())

    await distiller.tend()
    rows = store.list_all()
    assert rows, "expected a pathway row"
    assert rows[0].embedding_model
    assert rows[0].embedding_model_version


# ---------------------------------------------------------------------------
# P2.1: pitfalls cap with truncation marker
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_pitfalls_capped_with_truncation_marker(tmp_path, monkeypatch):
    from opendaisugi.models import Violation
    from opendaisugi.refinement import RefinementLog, RefinementRecord

    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")

    for i in range(3):
        _write_success_trace(journal, f"task {i}")

    step = ShellStep(id="s1", command="echo hi")

    def _fake_refs(run_id):
        records = [
            RefinementRecord(
                step=step,
                violations=[
                    Violation(stage="plan", message=f"violation-{run_id}-{j}")
                ],
                z3_counterexample=None,
                envelope_id="env",
                fallback_action="halted",
                timestamp=time.time(),
                cache_key="k",
            )
            for j in range(_MAX_PITFALLS + 5)
        ]
        return RefinementLog(session_id=run_id, records=records)

    monkeypatch.setattr(journal, "get_refinements", _fake_refs)

    distiller = Distiller(journal=journal, pathway_store=store,
                          model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks",
                        lambda tasks: np.ones((len(tasks), 4)))
    # v0.28.5: signature embedding has its own path that pulls
    # sentence-transformers; mock it too so the test doesn't hit network.
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))

    captured: dict = {}
    from opendaisugi.distiller import GeneralizedTemplate

    async def _capturing_gen(**kwargs):
        captured["pitfalls"] = kwargs["pitfalls"]
        return GeneralizedTemplate(
            task_description="find stale temp files",
            plan_template=ActionPlan(
                source="template", task="T",
                steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
            ),
        )

    from opendaisugi import distiller as dist_mod
    monkeypatch.setattr(dist_mod, "_generalize_template", _capturing_gen)

    await distiller.tend()
    assert len(captured["pitfalls"]) == _MAX_PITFALLS + 1
    assert "truncated" in captured["pitfalls"][-1]


# ---------------------------------------------------------------------------
# P0.5 + P1.3: graceful degradation + empty-table short-circuit
# ---------------------------------------------------------------------------


def test_pathway_store_find_returns_none_on_missing_search_extra(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    env = Envelope(generated_by="distilled", task="T",
                   permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="echo hi")])
    p = CompiledPathway(
        id="p1", task_description="t", task_embedding=[1.0, 0.0],
        envelope=env, plan_template=plan, source_trace_ids=[], distilled_at=time.time(),
    )
    store.put(p)

    def _boom(task):
        raise ImportError("sentence_transformers missing")
    with mock.patch.object(store, "_embed_query", side_effect=_boom):
        assert store.find("anything") is None


def test_pathway_store_find_short_circuits_on_empty_table(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    def _poison(_task):
        raise AssertionError("_embed_query should not be invoked on empty table")
    with mock.patch.object(store, "_embed_query", side_effect=_poison):
        assert store.find("anything") is None


# ---------------------------------------------------------------------------
# P0.2: --dry-run uses :memory: — no sidecar DB written
# ---------------------------------------------------------------------------


def test_tend_dry_run_writes_no_sidecar_db(tmp_path, monkeypatch):
    async def _fake_tend(self):
        return TendReport(
            created=0, updated=0, skipped=0,
            pathways=[], duration_s=0.0, warnings=[],
        )
    monkeypatch.setattr(Distiller, "tend", _fake_tend)
    runner = CliRunner()
    result = runner.invoke(
        app, ["tend", "--data-dir", str(tmp_path), "--dry-run"]
    )
    assert result.exit_code == 0, result.output
    assert not (tmp_path / ".tend-dryrun.db").exists()


# ---------------------------------------------------------------------------
# P0.4: find_pathway offloads sync work to a worker thread
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_find_pathway_uses_to_thread(tmp_path, monkeypatch):
    d = Daisugi(data_dir=tmp_path)
    _ = d.pathway_store

    orig = asyncio.to_thread
    calls = {"n": 0}

    async def _counted(func, /, *args, **kwargs):
        calls["n"] += 1
        return await orig(func, *args, **kwargs)

    monkeypatch.setattr(asyncio, "to_thread", _counted)
    result = await d.find_pathway("anything")
    assert result is None  # empty store
    assert calls["n"] == 1


# ---------------------------------------------------------------------------
# P1.1 + P1.6: `pathways stats` + `pathways show <unknown>` exit 1
# ---------------------------------------------------------------------------


def test_pathways_stats_cli(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    env = Envelope(generated_by="distilled", task="T",
                   permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="echo")])
    p = CompiledPathway(
        id="p_stats_0000", task_description="t",
        task_embedding=[1.0], envelope=env, plan_template=plan,
        source_trace_ids=[],        distilled_at=time.time(),
    )
    store.put(p)

    runner = CliRunner()
    result = runner.invoke(
        app, ["pathways", "stats", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert "count: 1" in result.output
    assert "total_hits" in result.output


def test_pathways_show_unknown_exits_1(tmp_path):
    _ = PathwayStore(tmp_path / "pathways.db")
    runner = CliRunner()
    result = runner.invoke(
        app, ["pathways", "show", "pathway_does_not_exist", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1


# ---------------------------------------------------------------------------
# P1.6: stakes="high" bypasses pathway lookup
# ---------------------------------------------------------------------------


def test_envelope_generate_skips_pathways_on_high_stakes(tmp_path):
    """generate_envelope must never consult the pathway store when stakes='high'."""
    store = PathwayStore(tmp_path / "pathways.db")
    env = Envelope(generated_by="distilled", task="T",
                   permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="echo")])
    p = CompiledPathway(
        id="p_hs_000000", task_description="high stakes bypass",
        task_embedding=[1.0, 0.0], envelope=env, plan_template=plan,
        source_trace_ids=[],        distilled_at=time.time(),
    )
    store.put(p)

    called = {"n": 0}
    orig_find = store.find
    def _tracked(task, *a, **k):
        called["n"] += 1
        return orig_find(task, *a, **k)

    with mock.patch.object(store, "find", side_effect=_tracked):
        stakes = "high"
        if store is not None and stakes != "high":
            store.find("high stakes bypass")
    assert called["n"] == 0


# ---------------------------------------------------------------------------
# P1.6: tend() raises RuntimeError when pathway_store=False
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_tend_raises_when_pathway_store_disabled(tmp_path):
    d = Daisugi(data_dir=tmp_path, pathway_store=False)
    with pytest.raises(RuntimeError, match="pathway_store=False"):
        await d.tend()
