"""Regression detector for compiled pathways (v0.4.0).

Given a history of ABResults for a pathway, compare the most recent
window's pass rate against the historical pass rate. A material drop
emits a RegressionAlert, which the Gardener report surfaces and the
CLI can escalate (for example by prompting a re-distillation).
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Sequence

from opendaisugi.gardener.ab_test import ABResult


@dataclass
class RegressionAlert:
    """Raised when a pathway's recent A/B results diverge from history."""

    pathway_id: str
    historical_pass_rate: float
    recent_pass_rate: float
    window: int

    @property
    def delta(self) -> float:
        return self.historical_pass_rate - self.recent_pass_rate


def regression_check(
    pathway_id: str,
    ab_history: Sequence[ABResult],
    *,
    window: int = 10,
    min_delta: float = 0.2,
) -> RegressionAlert | None:
    """Compare historical vs recent pass rate; return alert when drop >= min_delta.

    Requires at least ``2 * window`` results to have a meaningful split —
    below that the sample is too small to distinguish regression from
    noise, and the function returns None. History entries should be
    chronologically ordered oldest-to-newest.
    """
    if len(ab_history) < window * 2:
        return None

    history = ab_history[:-window]
    recent = ab_history[-window:]

    hist_pass = sum(1 for r in history if r.passed) / len(history)
    recent_pass = sum(1 for r in recent if r.passed) / len(recent)

    if hist_pass - recent_pass < min_delta:
        return None

    return RegressionAlert(
        pathway_id=pathway_id,
        historical_pass_rate=hist_pass,
        recent_pass_rate=recent_pass,
        window=window,
    )
