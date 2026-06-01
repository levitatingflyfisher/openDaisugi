"""Gardener run-outcome recording (v0.18 L8).

Every run that activates a pathway reports its outcome here; the Gardener
uses the accumulated failure_count / activation_count for slump detection
and pruning.

A run counts as a failure for the pathway iff the run did not succeed OR
its integrity check failed (silent step-skipping). The integrity signal
is what promotes the Gardener from housekeeping to load-bearing in the
v0.18 reproduction substrate: reproduction amplifies signal only if the
selection signal is trustworthy.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass
class RunOutcome:
    pathway_id: str
    status: str
    integrity_passed: bool | None


def record_run_outcome(
    pathway: Any,
    *,
    status: str,
    integrity_passed: bool | None,
    outcomes: list[RunOutcome] | None = None,
) -> None:
    """Record a pathway-activation outcome for Gardener fitness tracking.

    Mutates ``pathway`` in place: always increments ``activation_count``,
    and increments ``failure_count`` iff ``status != 'succeeded'`` OR
    ``integrity_passed is False``. ``integrity_passed is None`` is treated
    as "not checked, give benefit of the doubt" so runs that never reached
    execution (rejected-at-verify) don't incorrectly mark the pathway as
    failing.

    ``outcomes`` is an optional observer list — if provided, a ``RunOutcome``
    record is appended for Gardener reporters.
    """
    pathway.activation_count = getattr(pathway, "activation_count", 0) + 1
    if status != "succeeded" or integrity_passed is False:
        pathway.failure_count = getattr(pathway, "failure_count", 0) + 1
    if outcomes is not None:
        outcomes.append(RunOutcome(
            pathway_id=getattr(pathway, "id", "<unknown>"),
            status=status,
            integrity_passed=integrity_passed,
        ))
