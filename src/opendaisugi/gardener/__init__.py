"""Gardener: lifecycle management for compiled pathways (v0.4.0).

The Distiller grows the pathway store; the Gardener keeps it healthy.
Three passes run over the existing store:

* pruner — evict stale or failure-dominated pathways
* merger — collapse near-duplicates
* ab_test + regression — compare Tier-0 output against fresh Tier-2,
  alert on quality regressions

Nothing in this package talks to the journal directly — it operates on
``CompiledPathway`` records and the ``PathwayStore``'s public API.
"""

from __future__ import annotations

from opendaisugi.gardener.ab_test import ABResult, ab_test
from opendaisugi.gardener.merger import (
    MergeConfig,
    MergeReport,
    merge,
)
from opendaisugi.gardener.outcomes import RunOutcome, record_run_outcome
from opendaisugi.gardener.pruner import (
    PruneConfig,
    PruneReport,
    prune,
)
from opendaisugi.gardener.regression import RegressionAlert, regression_check
from opendaisugi.gardener.report import (
    GardenerConfig,
    GardenerReport,
    run_gardener,
)

__all__ = [
    "ABResult",
    "ab_test",
    "GardenerConfig",
    "GardenerReport",
    "MergeConfig",
    "MergeReport",
    "merge",
    "PruneConfig",
    "PruneReport",
    "prune",
    "RegressionAlert",
    "regression_check",
    "RunOutcome",
    "record_run_outcome",
    "run_gardener",
]
