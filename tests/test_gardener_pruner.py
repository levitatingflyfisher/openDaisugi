"""Tests for the v0.4.0 Gardener pruner."""

from __future__ import annotations

import time

from opendaisugi.gardener import PruneConfig, PruneReport, prune
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(
    id_: str,
    *,
    hit_count: int = 0,
    failure_count: int = 0,
    last_activation_at: float = 0.0,
) -> CompiledPathway:
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id=id_,
        task_description="T",
        task_embedding=[0.1, 0.2, 0.3],
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
        hit_count=hit_count,
        failure_count=failure_count,
        last_activation_at=last_activation_at,
    )


def test_stale_pathway_removed(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    stale = _pathway(
        "stale",
        hit_count=10,
        last_activation_at=time.time() - 60 * 86_400,  # 60 days ago
    )
    store.put(stale)

    report = prune(store, PruneConfig(max_idle_days=30))
    assert report.removed_ids == ["stale"]
    assert "stale" in report.reasons["stale"]
    assert store.list_all() == []


def test_active_pathway_kept(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    fresh = _pathway("fresh", hit_count=10, last_activation_at=time.time() - 86_400)
    store.put(fresh)

    report = prune(store, PruneConfig(max_idle_days=30))
    assert report.removed_ids == []
    assert report.kept_count == 1
    assert [p.id for p in store.list_all()] == ["fresh"]


def test_failure_dominated_pathway_removed(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    bad = _pathway(
        "bad",
        hit_count=2,
        failure_count=8,
        last_activation_at=time.time(),
    )
    store.put(bad)

    report = prune(store, PruneConfig(max_failure_ratio=0.5))
    assert report.removed_ids == ["bad"]
    assert "failure_dominated" in report.reasons["bad"]


def test_min_activations_grace_period(tmp_path):
    """A pathway with few total activations is always kept, even if stale."""
    store = PathwayStore(tmp_path / "p.db")
    new = _pathway(
        "new",
        hit_count=2,
        last_activation_at=time.time() - 90 * 86_400,
    )
    store.put(new)

    report = prune(store, PruneConfig(
        max_idle_days=30,
        min_activations_before_prune=5,
    ))
    assert report.removed_ids == []
    assert report.kept_count == 1


def test_dry_run_does_not_mutate(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    stale = _pathway(
        "stale",
        hit_count=10,
        last_activation_at=time.time() - 60 * 86_400,
    )
    store.put(stale)

    report = prune(store, PruneConfig(max_idle_days=30), dry_run=True)
    assert report.removed_ids == ["stale"]
    # Store is untouched.
    assert [p.id for p in store.list_all()] == ["stale"]


def test_mixed_store(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    now = time.time()
    store.put(_pathway("keep", hit_count=10, last_activation_at=now - 86_400))
    store.put(_pathway("stale", hit_count=10, last_activation_at=now - 60 * 86_400))
    store.put(_pathway("failing", hit_count=2, failure_count=8, last_activation_at=now))

    report = prune(store)
    assert set(report.removed_ids) == {"stale", "failing"}
    assert report.kept_count == 1
    remaining = {p.id for p in store.list_all()}
    assert remaining == {"keep"}


def test_default_config_used_when_none(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    report = prune(store)
    assert isinstance(report, PruneReport)
    assert report.removed_count == 0


def test_never_activated_pathway_kept_under_grace(tmp_path):
    """last_activation_at=0.0 (never touched) should not count as stale."""
    store = PathwayStore(tmp_path / "p.db")
    # 0 hits, 0 failures, 0 activation timestamp — fresh from distillation.
    store.put(_pathway("brand_new", hit_count=0, last_activation_at=0.0))
    report = prune(store)
    assert report.removed_ids == []
    assert report.kept_count == 1


def test_v028_4_mark_failure_stamps_last_activation_at(tmp_path):
    """v0.28.4 — mark_failure now stamps last_activation_at. Pre-fix it
    only bumped failure_count, so a failure-only pathway sat at 0.0
    forever, dodging the stale-check short-circuit."""
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("p1"))
    before = time.time()
    store.mark_failure("p1")
    after = time.time()
    [p] = store.list_all()
    assert p.failure_count == 1
    assert before <= p.last_activation_at <= after


def test_v028_4_failure_only_pathway_falls_through_to_distilled_at(tmp_path):
    """v0.28.4 — a pathway old enough by distilled_at but never activated
    is now pruned. Pre-fix, last_activation_at=0.0 and the
    ``pathway.last_activation_at and …`` short-circuit kept it forever."""
    store = PathwayStore(tmp_path / "p.db")
    # Old distill, never activated (no mark_failure call so last_activation_at=0.0),
    # but enough total_activations via direct hits so it clears the grace gate.
    ancient = time.time() - 1000 * 86_400
    p = _pathway("old", hit_count=5, last_activation_at=0.0)
    p.distilled_at = ancient
    store.put(p)
    report = prune(store, config=PruneConfig(max_idle_days=30))
    assert "old" in report.removed_ids
    assert "stale" in report.reasons["old"]
