"""Turning repeat clusters into a ranked, actionable reuse worklist.

``gateway_cluster.cluster_repeats`` groups repeated (paraphrase-included) asks from
the gateway journal into ``RepeatCluster``s. This module is the next step: rank those
clusters by the frontier spend they represent, so a human (or a later distillation
tool) knows which repeated ask is most worth turning into a reusable pathway first.

Tokens are the headline metric project-wide (the frontier token quota, not the dollar
figure, is what rate-limits a subscription harness — see ``gateway_journal``'s own
module docstring), dollars ride alongside to show how cheap those tokens already are.

This module is deliberately LLM-free and side-effect-free: it aggregates numbers
already sitting in the journal and does one OPTIONAL read-only pathway-store lookup
per cluster to flag "this repeat is already reusable." It never mints a pathway and
never calls a model itself — the only model touch anywhere in this path is
``cluster_repeats``'s own embed call, reused as-is.

Kept importable without the ``[search]`` extra at import time, same discipline as
``gateway_cluster.py``: no top-level sentence-transformers/numpy import. The embedder
is only ever reached through ``cluster_repeats``.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any

from opendaisugi.gateway_cluster import EmbedFn, RepeatCluster, cluster_repeats
from opendaisugi.gateway_journal import GatewayTurnRecord
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD


@dataclass(frozen=True)
class ReuseCandidate:
    """One repeat cluster, scored for reuse. Tokens first, dollars alongside."""

    cluster: RepeatCluster
    total_tokens: int
    total_dollars: float
    already_reusable: bool


def _already_reusable(pathway_store: Any, task: str) -> bool:
    """True iff ``pathway_store`` already has a matching pathway for ``task``.

    Guarded: this is a saving/surfacing path, so a store error (a locked db, a
    missing [search] extra the store itself doesn't already handle, whatever) must
    degrade to "not yet reusable," never sink the whole worklist.
    """
    if pathway_store is None:
        return False
    try:
        return pathway_store.find(task) is not None
    except Exception:
        return False


def rank_reuse_candidates(
    records: Iterable[GatewayTurnRecord],
    *,
    threshold: float = DEFAULT_PATHWAY_THRESHOLD,
    embed: EmbedFn | None = None,
    pathway_store: Any = None,
) -> list[ReuseCandidate]:
    """Cluster repeated asks and rank them by the frontier spend they represent.

    For each cluster returned by ``cluster_repeats``, aggregates over every record
    whose signature belongs to that cluster: ``total_tokens`` sums all four token
    buckets (fresh input, cache read, cache creation, output) across every
    occurrence; ``total_dollars`` sums ``actual_dollars`` across every occurrence.
    ``already_reusable`` is an optional, best-effort check against
    ``pathway_store`` — omit it and every candidate is ``already_reusable=False``.

    Sorted by ``total_tokens`` descending — tokens are the headline metric this
    whole project ranks by — tie-broken by ``total_dollars`` descending.
    """
    records = list(records)
    clusters = cluster_repeats(records, threshold=threshold, embed=embed)
    if not clusters:
        return []

    records_by_sig: dict[str, list[GatewayTurnRecord]] = {}
    for r in records:
        if r.signature:
            records_by_sig.setdefault(r.signature, []).append(r)

    candidates: list[ReuseCandidate] = []
    for cluster in clusters:
        member_records = [r for sig in cluster.signatures for r in records_by_sig.get(sig, [])]
        total_tokens = sum(
            r.input_tokens + r.cache_read_tokens + r.cache_creation_tokens + r.output_tokens
            for r in member_records
        )
        total_dollars = sum(r.actual_dollars for r in member_records)
        candidates.append(
            ReuseCandidate(
                cluster=cluster,
                total_tokens=total_tokens,
                total_dollars=total_dollars,
                already_reusable=_already_reusable(pathway_store, cluster.representative_task),
            )
        )

    candidates.sort(key=lambda c: (c.total_tokens, c.total_dollars), reverse=True)
    return candidates
