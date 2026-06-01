"""Pathway merger (v0.4.0).

Collapses near-duplicate pathways. Two pathways are candidates when the
cosine similarity of their task embeddings exceeds the threshold *and*
their permissions are compatible (or merging permissions is allowed).

Winner selection rule:
  1. higher hit_count (more earned activations)
  2. tiebreak: newer distilled_at

The loser's ``source_trace_ids`` are unioned into the winner so provenance
is preserved, then the loser is removed from the store.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from opendaisugi._similarity import cosine_similarity
from opendaisugi.models import Permission
from opendaisugi.pathway_store import PathwayStore
from opendaisugi.permissions import intersect_permissions

if TYPE_CHECKING:
    from opendaisugi.pathway import CompiledPathway

_log = logging.getLogger("opendaisugi.gardener.merger")


@dataclass
class MergeConfig:
    """Tunables for :func:`merge`."""

    similarity_threshold: float = 0.92
    """Cosine similarity above which two pathways are merge candidates."""

    require_compatible_permissions: bool = True
    """If True, only merge when the permission intersection == both sides."""


@dataclass
class MergeReport:
    """Outcome of a merge pass."""

    merged_pairs: list[tuple[str, str]] = field(default_factory=list)
    """List of (winner_id, loser_id) pairs, in merge order."""

    kept_ids: list[str] = field(default_factory=list)
    removed_ids: list[str] = field(default_factory=list)

    @property
    def merge_count(self) -> int:
        return len(self.merged_pairs)


def _permissions_compatible(a: Permission, b: Permission) -> bool:
    """True when intersection equals both sides — i.e., both grant identical scope.

    We can't safely merge pathways whose intersection is strictly narrower
    than either input, because that would silently strip privileges the
    original pathways relied on.
    """
    merged = intersect_permissions([a, b])
    return (
        merged.model_dump() == a.model_dump()
        and merged.model_dump() == b.model_dump()
    )


def _pick_winner(a: CompiledPathway, b: CompiledPathway) -> tuple[CompiledPathway, CompiledPathway]:
    if a.hit_count != b.hit_count:
        return (a, b) if a.hit_count > b.hit_count else (b, a)
    # Tiebreak: newer distillation wins.
    return (a, b) if a.distilled_at >= b.distilled_at else (b, a)


def merge(
    store: PathwayStore,
    config: MergeConfig | None = None,
    *,
    dry_run: bool = False,
) -> MergeReport:
    """Collapse near-duplicate pathways.

    Greedy single pass: for each pair above the similarity threshold, the
    loser folds into the winner. The loser's source traces are unioned
    into the winner before deletion.
    """
    cfg = config or MergeConfig()
    pathways = store.list_all()
    report = MergeReport()

    # Track which pathways have been absorbed this pass so we don't reuse
    # a loser as a candidate later.
    removed: set[str] = set()

    for i in range(len(pathways)):
        a = pathways[i]
        if a.id in removed:
            continue
        for j in range(i + 1, len(pathways)):
            b = pathways[j]
            if b.id in removed:
                continue

            sim = cosine_similarity(a.task_embedding, b.task_embedding)
            if sim < cfg.similarity_threshold:
                continue

            if cfg.require_compatible_permissions and not _permissions_compatible(
                a.envelope.permissions, b.envelope.permissions
            ):
                continue

            winner, loser = _pick_winner(a, b)
            report.merged_pairs.append((winner.id, loser.id))
            removed.add(loser.id)

            if not dry_run:
                # Union source_trace_ids onto the winner.
                merged_sources = sorted(
                    set(winner.source_trace_ids) | set(loser.source_trace_ids)
                )
                winner.source_trace_ids = merged_sources
                winner.hit_count = winner.hit_count + loser.hit_count
                winner.failure_count = winner.failure_count + loser.failure_count
                store.put(winner)
                store.delete(loser.id)

            # If a was the loser, a is now gone — break inner loop.
            if loser.id == a.id:
                break

    report.removed_ids = sorted(removed)
    report.kept_ids = sorted(p.id for p in pathways if p.id not in removed)
    _log.info(
        "merge complete: merged=%d kept=%d dry_run=%s",
        report.merge_count, len(report.kept_ids), dry_run,
    )
    return report
