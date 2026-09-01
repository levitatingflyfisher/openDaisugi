"""Integration test for Distiller.tend() full pipeline."""

import sqlite3

import numpy as np
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
    """Seed a successful trace.

    ``Journal.log()`` writes the YAML + SQLite row but leaves run_id /
    run_status NULL, so ``list_successful_traces`` (which filters on
    run_status='succeeded') would not find it. We log first so
    ``load_trace()`` works (the Distiller needs the YAML body), then
    update the run columns via raw SQL — same pattern used in
    ``tests/test_journal_distillable.py``.
    """
    env = Envelope(
        generated_by="test",
        task=task,
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan = ActionPlan(
        source="t",
        task=task,
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )
    result = VerificationResult(
        ok=True,
        violations=[],
        warnings=[],
        envelope_id=env.id,
        plan_id=plan.id,
        duration_ms=0.1,
    )
    trace_id = journal.log(task=task, envelope=env, plan=plan, result=result)
    run_id = f"run_{trace_id}"
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (run_id, trace_id),
        )
    return trace_id


@pytest.mark.asyncio
async def test_tend_creates_pathway_from_cluster(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    # Write 3 near-identical successful traces.
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")

    distiller = Distiller(
        journal=journal,
        pathway_store=store,
        model="test-model",
        min_traces=3,
    )

    # Stub embedder: same vector for all tasks so they cluster together.
    monkeypatch.setattr(distiller, "_embed_tasks", lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures", lambda sigs: np.ones((len(sigs), 4)))

    # Stub LLM generalization + improvement with deterministic returns.
    from opendaisugi import distiller as dist_mod
    from opendaisugi.distiller import GeneralizedTemplate

    env_tmpl = Envelope(
        generated_by="distilled",
        task="T",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan_tmpl = ActionPlan(
        source="template",
        task="T",
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files",
            plan_template=plan_tmpl,
        )

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)

    report = await distiller.tend()
    assert report.created == 1
    assert report.skipped == 0
    assert len(store.list_all()) == 1

    pathway = store.list_all()[0]
    assert pathway.task_description == "find stale temp files"
    assert len(pathway.source_trace_ids) >= 1


@pytest.mark.asyncio
async def test_tend_skips_small_clusters(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    # Only 2 traces — below default min_traces=3.
    _write_success_trace(journal, "task one")
    _write_success_trace(journal, "task two")

    distiller = Distiller(journal=journal, pathway_store=store, model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks", lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures", lambda sigs: np.ones((len(sigs), 4)))

    report = await distiller.tend()
    assert report.created == 0
    assert report.skipped >= 1
    assert store.list_all() == []


@pytest.mark.asyncio
async def test_generalization_failure_warns_in_one_line(tmp_path, monkeypatch):
    """An instructor retry exception renders its failed attempts as a
    multi-line dump with fresh response ids; the warning keeps the first
    cause and the count only."""
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")
    distiller = Distiller(journal=journal, pathway_store=store, model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks", lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures", lambda sigs: np.ones((len(sigs), 4)))

    from opendaisugi import distiller as dist_mod

    class _Attempt:
        exception = ValueError("bad reply\nsecond line")

    class _RetryError(Exception):
        failed_attempts = [_Attempt(), _Attempt(), _Attempt()]

        def __str__(self):
            return "<failed_attempts>\n<completion>chatcmpl-random</completion>"

    async def _fail(**kwargs):
        raise _RetryError()

    monkeypatch.setattr(dist_mod, "_generalize_template", _fail)
    report = await distiller.tend()
    assert report.warnings == ["cluster generalization failed: ValueError: bad reply (3 attempts)"]


@pytest.mark.asyncio
async def test_template_whose_z3_check_timed_out_is_not_stored(tmp_path, monkeypatch):
    """verify() keeps a Z3 timeout as a warning in lenient mode; nothing
    proved the template inside its envelope, so it is not stored."""
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")
    distiller = Distiller(journal=journal, pathway_store=store, model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks", lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures", lambda sigs: np.ones((len(sigs), 4)))

    from opendaisugi import distiller as dist_mod
    import importlib

    verify_mod = importlib.import_module("opendaisugi.verify")
    from opendaisugi.distiller import GeneralizedTemplate

    plan_tmpl = ActionPlan(source="template", task="T", steps=[ShellStep(id="s1", command="find /tmp")])

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(task_description="find stale temp files", plan_template=plan_tmpl)

    def _timed_out(plan, envelope, **kw):
        return VerificationResult(
            ok=True, warnings=["Z3 plan-vs-envelope check exceeded 500ms"],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=1.0,
        )

    async def _same(*, envelope, failing_plans, model):
        return envelope

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)
    monkeypatch.setattr(dist_mod, "_improve_envelope", _same)
    monkeypatch.setattr(dist_mod, "_verify", _timed_out)
    monkeypatch.setattr(verify_mod, "verify", _timed_out)
    report = await distiller.tend()
    assert report.created == 0
    assert store.list_all() == []
    assert report.warnings[-1:] == [
        "distilled plan_template does not verify against its envelope "
        "(verifier timed out; raise the Z3 timeout); dropping cluster"
    ]


def test_validate_envelope_counts_a_timed_out_plan_as_failing(monkeypatch):
    from opendaisugi import distiller as dist_mod
    from opendaisugi.models import Violation

    env = Envelope(generated_by="t", task="T", permissions=Permission(shell=True, shell_allowlist=["ls"]))
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="ls")])

    def _violation(p, e, **kw):
        v = Violation(stage="z3", message="verifier timed out (plan-vs-envelope check); raise the Z3 timeout",
                      detail={"reason": "z3_timeout"})
        return VerificationResult(ok=False, violations=[v], envelope_id=e.id, plan_id=p.id, duration_ms=1.0)

    def _warning(p, e, **kw):
        return VerificationResult(ok=True, warnings=["Z3 envelope self-consistency check exceeded 500ms"],
                                  envelope_id=e.id, plan_id=p.id, duration_ms=1.0)

    for fake in (_violation, _warning):
        monkeypatch.setattr(dist_mod, "_verify", fake)
        score, failing = dist_mod._validate_envelope(env, [plan])
        assert (score, failing) == (0.0, [plan])
