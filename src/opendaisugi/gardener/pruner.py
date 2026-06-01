"""Pathway pruner (v0.4.0).

Evicts pathways that have gone stale (no recent activation) or that are
dominated by failures. Intentionally conservative: a pathway has to earn
the right to be pruned — new pathways get a grace period so a freshly
distilled pathway isn't killed before it has a chance to be matched.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field

from opendaisugi.pathway_store import PathwayStore

_log = logging.getLogger("opendaisugi.gardener.pruner")


@dataclass
class PruneConfig:
    """Tunables for :func:`prune`. Defaults chosen to be conservative."""

    max_idle_days: float = 30.0
    """Evict if the pathway has not been activated in this many days."""

    max_failure_ratio: float = 0.5
    """Evict if failure_count / (hit_count + failure_count) exceeds this."""

    min_activations_before_prune: int = 5
    """Grace period — pathways with fewer total activations are kept."""


@dataclass
class PruneReport:
    """Outcome of a prune pass."""

    removed_ids: list[str] = field(default_factory=list)
    kept_count: int = 0
    reasons: dict[str, str] = field(default_factory=dict)

    @property
    def removed_count(self) -> int:
        return len(self.removed_ids)


def prune(
    store: PathwayStore,
    config: PruneConfig | None = None,
    *,
    dry_run: bool = False,
) -> PruneReport:
    """Evict stale / failure-dominated pathways from ``store``.

    Decision order (first matching reason wins, so the report is crisp):

    1. ``grace`` — total activations below ``min_activations_before_prune``;
       always kept. Prevents killing a freshly distilled pathway.
    2. ``failure_dominated`` — failure ratio exceeds threshold.
    3. ``stale`` — no activation within ``max_idle_days``.

    Set ``dry_run=True`` to see what *would* be removed without mutating
    the store — used by ``daisugi gardener prune --dry-run``.
    """
    cfg = config or PruneConfig()
    now = time.time()
    idle_cutoff = now - cfg.max_idle_days * 86_400

    report = PruneReport()

    for pathway in store.list_all():
        total_activations = pathway.hit_count + pathway.failure_count

        if total_activations < cfg.min_activations_before_prune:
            report.kept_count += 1
            continue

        denom = total_activations or 1
        failure_ratio = pathway.failure_count / denom

        if failure_ratio > cfg.max_failure_ratio:
            report.removed_ids.append(pathway.id)
            report.reasons[pathway.id] = (
                f"failure_dominated (ratio={failure_ratio:.2f})"
            )
            if not dry_run:
                store.delete(pathway.id)
            continue

        # v0.28.4: handle the never-activated case explicitly. Pre-v0.28.4
        # the `pathway.last_activation_at and ...` short-circuit treated
        # `0.0` (default at distill time, pre-v0.28.4 mark_failure didn't
        # stamp) as "skip stale check" — so an old never-hit pathway was
        # kept indefinitely. Now we treat 0.0 as "use distilled_at as the
        # freshness reference" so age-based pruning still applies.
        freshness = pathway.last_activation_at or pathway.distilled_at
        if freshness and freshness < idle_cutoff:
            report.removed_ids.append(pathway.id)
            idle_days = (now - freshness) / 86_400
            report.reasons[pathway.id] = f"stale (idle={idle_days:.1f}d)"
            if not dry_run:
                store.delete(pathway.id)
            continue

        report.kept_count += 1

    _log.info(
        "prune complete: removed=%d kept=%d dry_run=%s",
        report.removed_count, report.kept_count, dry_run,
    )
    return report
