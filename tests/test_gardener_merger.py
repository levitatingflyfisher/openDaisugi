"""Tests for the v0.4.0 Gardener merger."""

from __future__ import annotations

import time

from opendaisugi.gardener import MergeConfig, MergeReport, merge
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(
    id_: str,
    embedding: list[float],
    *,
    hit_count: int = 0,
    source_trace_ids: list[str] | None = None,
    distilled_at: float | None = None,
    permissions: Permission | None = None,
) -> CompiledPathway:
    env = Envelope(
        generated_by="test", task="T",
        permissions=permissions or Permission(shell=True),
    )
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id=id_,
        task_description="T",
        task_embedding=embedding,
        envelope=env,
        plan_template=plan,
        source_trace_ids=source_trace_ids or [],
        distilled_at=distilled_at if distilled_at is not None else time.time(),
        hit_count=hit_count,
    )


def test_near_duplicates_merged(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    a = _pathway("a", [1.0, 0.0, 0.0], hit_count=5, source_trace_ids=["t1"])
    b = _pathway("b", [0.99, 0.01, 0.0], hit_count=2, source_trace_ids=["t2"])
    store.put(a)
    store.put(b)

    report = merge(store, MergeConfig(similarity_threshold=0.9))
    assert report.merged_pairs == [("a", "b")]  # a wins, hit_count=5 > 2
    remaining = store.list_all()
    assert len(remaining) == 1
    winner = remaining[0]
    assert winner.id == "a"
    assert sorted(winner.source_trace_ids) == ["t1", "t2"]
    assert winner.hit_count == 7  # combined


def test_dissimilar_pathways_kept_separate(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", [1.0, 0.0, 0.0]))
    store.put(_pathway("b", [0.0, 1.0, 0.0]))  # orthogonal

    report = merge(store, MergeConfig(similarity_threshold=0.9))
    assert report.merged_pairs == []
    assert len(store.list_all()) == 2


def test_incompatible_permissions_prevents_merge(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    a = _pathway("a", [1.0, 0.0, 0.0], permissions=Permission(shell=True, network=False))
    b = _pathway("b", [0.99, 0.01, 0.0], permissions=Permission(shell=True, network=True))
    store.put(a)
    store.put(b)

    report = merge(store, MergeConfig(
        similarity_threshold=0.9, require_compatible_permissions=True,
    ))
    assert report.merged_pairs == []
    assert len(store.list_all()) == 2


def test_dry_run_does_not_mutate(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", [1.0, 0.0, 0.0], hit_count=5))
    store.put(_pathway("b", [0.99, 0.01, 0.0], hit_count=2))

    report = merge(store, MergeConfig(similarity_threshold=0.9), dry_run=True)
    assert report.merged_pairs == [("a", "b")]
    # Store untouched.
    assert len(store.list_all()) == 2


def test_tiebreak_by_distilled_at(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    now = time.time()
    older = _pathway("older", [1.0, 0.0, 0.0], hit_count=3, distilled_at=now - 86_400)
    newer = _pathway("newer", [0.99, 0.01, 0.0], hit_count=3, distilled_at=now)
    store.put(older)
    store.put(newer)

    report = merge(store, MergeConfig(similarity_threshold=0.9))
    # Equal hit_count — newer wins.
    assert report.merged_pairs == [("newer", "older")]
    remaining = store.list_all()
    assert len(remaining) == 1
    assert remaining[0].id == "newer"


def test_three_way_chain_merges_once(tmp_path):
    """Greedy pass: a absorbs b; c then compares against the remaining set."""
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", [1.0, 0.0, 0.0], hit_count=10))
    store.put(_pathway("b", [0.99, 0.01, 0.0], hit_count=5))
    store.put(_pathway("c", [0.98, 0.02, 0.0], hit_count=2))

    report = merge(store, MergeConfig(similarity_threshold=0.9))
    # Three all near-duplicates — a wins both pairings.
    assert {pair[0] for pair in report.merged_pairs} == {"a"}
    assert {pair[1] for pair in report.merged_pairs} == {"b", "c"}
    remaining = store.list_all()
    assert len(remaining) == 1
    assert remaining[0].id == "a"
    assert remaining[0].hit_count == 17


def test_default_config(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    report = merge(store)
    assert isinstance(report, MergeReport)
    assert report.merge_count == 0


def test_cross_embedding_version_not_merged(tmp_path):
    # M8: identical embeddings from DIFFERENT embedding-model versions must NOT
    # merge — a cross-space similarity is meaningless and could delete an
    # unrelated pathway.
    store = PathwayStore(tmp_path / "p.db")
    a = _pathway("a", [1.0, 0.0, 0.0]).model_copy(update={"embedding_model": "m", "embedding_model_version": "3"})
    b = _pathway("b", [1.0, 0.0, 0.0]).model_copy(update={"embedding_model": "m", "embedding_model_version": "4"})
    store.put(a); store.put(b)
    report = merge(store, MergeConfig(similarity_threshold=0.5, require_compatible_permissions=False))
    assert report.merged_pairs == []
    assert len(store.list_all()) == 2


def test_mismatched_embedding_dimension_does_not_crash_merge(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    a = _pathway("a", [1.0, 0.0, 0.0]).model_copy(update={"embedding_model": "m", "embedding_model_version": "3"})
    b = _pathway("b", [1.0, 0.0]).model_copy(update={"embedding_model": "m", "embedding_model_version": "3"})
    store.put(a); store.put(b)
    report = merge(store, MergeConfig(similarity_threshold=0.5, require_compatible_permissions=False))
    assert report.merged_pairs == []  # skipped, no ValueError
