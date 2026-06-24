"""Gardener orchestrator and report (v0.4.0).

Composes the prune + merge passes (and optional A/B sweep) into a
single ``run_gardener(store, config)`` entry point. The resulting
``GardenerReport`` is the shape the CLI renders for users.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from opendaisugi.gardener.ab_test import ABResult
from opendaisugi.gardener.merger import MergeConfig, MergeReport, merge
from opendaisugi.gardener.pruner import PruneConfig, PruneReport, prune
from opendaisugi.gardener.regression import RegressionAlert
from opendaisugi.pathway_store import PathwayStore


@dataclass
class GardenerConfig:
    """Composite config for a full gardener pass."""

    prune: PruneConfig = field(default_factory=PruneConfig)
    merge: MergeConfig = field(default_factory=MergeConfig)
    run_prune: bool = True
    run_merge: bool = True


@dataclass
class GardenerReport:
    """Summary of one full gardener pass."""

    prune: PruneReport = field(default_factory=PruneReport)
    merge: MergeReport = field(default_factory=MergeReport)
    ab_results: list[ABResult] = field(default_factory=list)
    alerts: list[RegressionAlert] = field(default_factory=list)

    @property
    def store_size_after(self) -> int:
        return len(self.merge.kept_ids)


def run_gardener(
    store: PathwayStore,
    config: GardenerConfig | None = None,
    *,
    dry_run: bool = False,
) -> GardenerReport:
    """Prune then merge the store. A/B + regression are opt-in elsewhere.

    Execution order matters: pruning first keeps the merger from wasting
    work on pathways that are about to die.
    """
    cfg = config or GardenerConfig()
    report = GardenerReport()

    if cfg.run_prune:
        report.prune = prune(store, cfg.prune, dry_run=dry_run)
    if cfg.run_merge:
        report.merge = merge(store, cfg.merge, dry_run=dry_run)

    return report
