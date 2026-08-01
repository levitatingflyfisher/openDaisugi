"""Embedding-based clustering of repeated gateway asks.

This is ADDITIVE to the exact-match repeat detection in ``gateway_journal``
(``turn_signature`` / ``RepeatGroup`` / ``summarize``), which only fires on a
byte-for-byte (normalized) re-ask. Real work is paraphrased — "summarize the
auth module" vs "give me a summary of the auth code" — and the exact-match
path never groups those. ``cluster_repeats`` groups by embedding similarity
instead, so paraphrases surface as one repeat cluster.

Unit tests inject a deterministic fake ``embed`` so clustering logic is
tested without loading any real model. Exactly one test exercises the real
lazy-loaded embedder (guarded by ``pytest.importorskip``), to prove the
default wiring works end to end.

All fixture text is generic/public — never real user work content.
"""

from __future__ import annotations

import os

import pytest

from opendaisugi.gateway_cluster import RepeatCluster, cluster_repeats
from opendaisugi.gateway_journal import GatewayTurnRecord, turn_signature


def _record(task: str, *, ask: str | None = None) -> GatewayTurnRecord:
    """Build a minimal GatewayTurnRecord for clustering tests.

    ``ask`` defaults to ``task`` (a plain single-turn ask); pass ``ask=""``
    to build a tool-loop continuation record (empty signature), mirroring
    ``record_turn``'s own rule.
    """
    resolved_ask = task if ask is None else ask
    signature = turn_signature(resolved_ask) if resolved_ask.strip() else ""
    return GatewayTurnRecord(
        created_at="2026-08-01T00:00:00Z",
        signature=signature,
        task=task,
        tier="fast",
        requested_model="claude-opus-4-8",
        model="claude-haiku-4-8",
        difficulty=0.1,
        downgraded=True,
        estimated=False,
        input_tokens=100,
        output_tokens=50,
        frontier_tokens_saved=100,
        actual_dollars=0.01,
        counterfactual_dollars=0.05,
    )


def _fake_embed(mapping: dict[str, list[float]]):
    """A deterministic injectable ``embed`` — looks up a fixed vector per text."""

    def embed(texts: list[str]) -> list[list[float]]:
        return [mapping[t] for t in texts]

    return embed


# ---- paraphrases cluster together ----


def test_paraphrases_cluster_into_one_group() -> None:
    task_a = "summarize the auth module"
    task_b = "give me a summary of the auth code"
    records = [
        _record(task_a),
        _record(task_a),
        _record(task_b),
    ]
    embed = _fake_embed(
        {
            task_a: [1.0, 0.0],
            task_b: [0.999, 0.001],  # near-identical unit vector -> paraphrase
        }
    )

    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    assert len(clusters) == 1
    cluster = clusters[0]
    assert isinstance(cluster, RepeatCluster)
    assert cluster.count == 3  # 2 occurrences of task_a + 1 of task_b
    assert set(cluster.member_tasks) == {task_a, task_b}
    assert cluster.representative_task == task_a  # the more common phrasing
    assert set(cluster.signatures) == {turn_signature(task_a), turn_signature(task_b)}


# ---- semantically distinct asks stay separate; singletons are dropped ----


def test_distinct_asks_form_separate_clusters_and_singletons_are_dropped() -> None:
    repeated_task = "summarize the auth module"
    singleton_task = "delete the temp files"
    records = [
        _record(repeated_task),
        _record(repeated_task),
        _record(singleton_task),
    ]
    embed = _fake_embed(
        {
            repeated_task: [1.0, 0.0],
            singleton_task: [0.0, 1.0],  # orthogonal -> not a paraphrase
        }
    )

    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    # Only the repeated group survives; the singleton never forms a returned cluster.
    assert len(clusters) == 1
    assert clusters[0].count == 2
    assert clusters[0].member_tasks == [repeated_task]


# ---- a task asked once is not a repeat ----


def test_single_occurrence_is_not_returned() -> None:
    task = "write the changelog"
    records = [_record(task)]
    embed = _fake_embed({task: [1.0, 0.0]})

    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    assert clusters == []


# ---- empty input ----


def test_empty_input_returns_empty_list() -> None:
    embed = _fake_embed({})

    assert cluster_repeats([], threshold=0.55, embed=embed) == []


# ---- tool-loop continuations (empty signature) are excluded ----


def test_continuations_are_ignored_like_summarize() -> None:
    task = "run the migration"
    records = [
        _record(task, ask=""),
        _record(task, ask=""),
        _record(task, ask=""),
    ]
    embed = _fake_embed({task: [1.0, 0.0]})

    # All three records carry an empty signature (continuations), so despite
    # sharing identical task text, none of them count as a repeated *ask*.
    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    assert clusters == []


def test_continuations_do_not_inflate_a_real_repeat_cluster() -> None:
    task = "run the migration"
    other = "clean up the branch"
    records = [
        _record(task),
        _record(task),
        _record(task, ask=""),  # continuation of the above loop; not a re-ask
        _record(other),
    ]
    embed = _fake_embed({task: [1.0, 0.0], other: [0.0, 1.0]})

    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    assert len(clusters) == 1
    assert clusters[0].count == 2  # only the two real asks, not the continuation


# ---- clusters returned sorted by count descending ----


def test_clusters_sorted_by_count_descending() -> None:
    big_task = "review the pull request"
    small_task = "rebase the branch"
    records = [
        _record(big_task),
        _record(big_task),
        _record(big_task),
        _record(small_task),
        _record(small_task),
    ]
    embed = _fake_embed({big_task: [1.0, 0.0], small_task: [0.0, 1.0]})

    clusters = cluster_repeats(records, threshold=0.55, embed=embed)

    assert [c.count for c in clusters] == [3, 2]


# ---- integration: the real lazy-loaded embedder ----


def test_real_embedder_clusters_obvious_paraphrases() -> None:
    pytest.importorskip("sentence_transformers")
    # This box's GPU crashes the embedder; force CPU before the model loads.
    os.environ["CUDA_VISIBLE_DEVICES"] = ""

    task_a = "summarize the authentication module"
    task_b = "give me a summary of the authentication code"
    task_c = "delete all temp files"
    records = [
        _record(task_a),
        _record(task_b),
        _record(task_c),
    ]

    clusters = cluster_repeats(records, threshold=0.55)

    assert len(clusters) == 1
    cluster = clusters[0]
    assert set(cluster.member_tasks) == {task_a, task_b}
    assert cluster.count == 2
