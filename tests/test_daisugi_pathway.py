"""Tests for Daisugi facade pathway integration (v0.3.0)."""

import pytest

from opendaisugi import Daisugi
from opendaisugi.pathway_store import PathwayStore


def test_daisugi_default_pathway_store_is_auto_constructed(tmp_path):
    d = Daisugi(data_dir=tmp_path)
    assert d.pathway_store is not None
    assert isinstance(d.pathway_store, PathwayStore)


def test_daisugi_pathway_store_false_disables(tmp_path):
    d = Daisugi(data_dir=tmp_path, pathway_store=False)
    assert d.pathway_store is None


def test_daisugi_accepts_explicit_pathway_store(tmp_path):
    custom = PathwayStore(tmp_path / "custom.db")
    d = Daisugi(data_dir=tmp_path, pathway_store=custom)
    assert d.pathway_store is custom


def test_daisugi_pathway_store_lazy_file_creation(tmp_path):
    d = Daisugi(data_dir=tmp_path)
    # File shouldn't exist yet — not touched by __init__.
    assert not (tmp_path / "pathways.db").exists()
    _ = d.pathway_store  # access triggers creation
    assert (tmp_path / "pathways.db").exists()


import time

import numpy as np

from opendaisugi.distiller import Distiller, TendReport
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway


def _put_pathway(store):
    env = Envelope(generated_by="distilled", task="T",
                   permissions=Permission(shell=True, shell_allowlist=["find"]))
    plan = ActionPlan(source="t", task="T",
                      steps=[ShellStep(id="s1", command="find /tmp")])
    p = CompiledPathway(
        id="pathway_m00000001", task_description="find stale files",
        task_embedding=[1.0, 0.0], envelope=env, plan_template=plan,
        source_trace_ids=[],        distilled_at=time.time(),
    )
    store.put(p)
    return p


@pytest.mark.asyncio
async def test_find_pathway_returns_match(tmp_path, monkeypatch):
    d = Daisugi(data_dir=tmp_path)
    p = _put_pathway(d.pathway_store)
    monkeypatch.setattr(d.pathway_store, "_embed_query", lambda _: np.array([1.0, 0.0]))

    match = await d.find_pathway("find stale tmp files")
    assert match is not None
    assert match.pathway.id == p.id


@pytest.mark.asyncio
async def test_find_pathway_returns_none_when_disabled(tmp_path):
    d = Daisugi(data_dir=tmp_path, pathway_store=False)
    match = await d.find_pathway("anything")
    assert match is None


@pytest.mark.asyncio
async def test_tend_runs_distiller(tmp_path, monkeypatch):
    d = Daisugi(data_dir=tmp_path)
    called = {}

    async def _fake_tend(self):
        called["ran"] = True
        return TendReport(created=0, updated=0, skipped=0, pathways=[], duration_s=0.0, warnings=[])

    monkeypatch.setattr(Distiller, "tend", _fake_tend)
    report = await d.tend()
    assert called["ran"]
    assert isinstance(report, TendReport)


@pytest.mark.asyncio
async def test_adapt_plan_returns_adapted_or_falls_back(tmp_path, monkeypatch):
    from opendaisugi.pathway import PathwayMatch

    d = Daisugi(data_dir=tmp_path)
    p = _put_pathway(d.pathway_store)
    match = PathwayMatch(pathway=p, similarity=0.9)

    # Stub the instructor client to return a new plan.
    adapted_plan = ActionPlan(
        source="adapted", task="specific",
        steps=[ShellStep(id="s1", command="find /var/tmp")],
    )

    class _FakeCompletions:
        async def create(self, **kwargs):
            return adapted_plan

    class _FakeChat:
        completions = _FakeCompletions()

    class _FakeClient:
        chat = _FakeChat()

    from opendaisugi import llm
    monkeypatch.setattr(llm, "get_instructor_client", lambda _m: _FakeClient())

    result = await d.adapt_plan(match, "find stale /var/tmp files")
    # Either the adapted plan or the template (on verify failure).
    assert result.source in ("adapted", "t")
