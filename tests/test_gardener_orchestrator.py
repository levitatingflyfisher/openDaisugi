"""Tests for the v0.4.0 Gardener orchestrator."""

from __future__ import annotations

import time

from opendaisugi.gardener import (
    GardenerConfig,
    GardenerReport,
    run_gardener,
)
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(
    id_: str,
    embedding: list[float],
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
        task_embedding=embedding,
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
        hit_count=hit_count,
        failure_count=failure_count,
        last_activation_at=last_activation_at,
    )


def test_end_to_end_prune_then_merge(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    now = time.time()

    # Stale — should be pruned.
    store.put(_pathway(
        "stale", [0.0, 0.0, 1.0],
        hit_count=10, last_activation_at=now - 60 * 86_400,
    ))
    # Two near-duplicates — should be merged.
    store.put(_pathway("a", [1.0, 0.0, 0.0], hit_count=5, last_activation_at=now))
    store.put(_pathway("b", [0.99, 0.01, 0.0], hit_count=2, last_activation_at=now))

    report = run_gardener(store)

    assert isinstance(report, GardenerReport)
    assert "stale" in report.prune.removed_ids
    assert ("a", "b") in report.merge.merged_pairs

    remaining_ids = {p.id for p in store.list_all()}
    assert remaining_ids == {"a"}


def test_run_prune_only(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", [1.0, 0.0, 0.0], hit_count=5, last_activation_at=time.time()))
    store.put(_pathway("b", [0.99, 0.01, 0.0], hit_count=2, last_activation_at=time.time()))

    cfg = GardenerConfig(run_prune=True, run_merge=False)
    report = run_gardener(store, cfg)
    assert report.merge.merge_count == 0
    assert len(store.list_all()) == 2  # nothing removed


def test_dry_run_leaves_store_intact(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    now = time.time()
    store.put(_pathway(
        "stale", [0.0, 0.0, 1.0],
        hit_count=10, last_activation_at=now - 60 * 86_400,
    ))
    store.put(_pathway("a", [1.0, 0.0, 0.0], hit_count=5, last_activation_at=now))
    store.put(_pathway("b", [0.99, 0.01, 0.0], hit_count=2, last_activation_at=now))

    report = run_gardener(store, dry_run=True)
    assert "stale" in report.prune.removed_ids
    assert ("a", "b") in report.merge.merged_pairs
    # But the store is untouched.
    assert len(store.list_all()) == 3


def test_empty_store(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    report = run_gardener(store)
    assert report.prune.removed_count == 0
    assert report.merge.merge_count == 0
