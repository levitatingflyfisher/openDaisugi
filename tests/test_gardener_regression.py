"""Tests for the v0.4.0 Gardener regression detector."""

from __future__ import annotations

import pytest

from opendaisugi.gardener import regression_check
from opendaisugi.gardener.ab_test import ABResult


def _result(*, passed: bool) -> ABResult:
    return ABResult(
        pathway_id="pw_1",
        task="T",
        postconditions_match=passed,
        permissions_match=passed,
    )


def test_regression_alerts_on_material_drop() -> None:
    # Historical: all passed; recent: all failed.
    history = [_result(passed=True)] * 20 + [_result(passed=False)] * 10
    alert = regression_check("pw_1", history, window=10, min_delta=0.2)
    assert alert is not None
    assert alert.pathway_id == "pw_1"
    assert alert.historical_pass_rate == 1.0
    assert alert.recent_pass_rate == 0.0
    assert alert.delta == 1.0


def test_stable_history_no_alert() -> None:
    history = [_result(passed=True)] * 30
    assert regression_check("pw_1", history, window=10) is None


def test_insufficient_window_returns_none() -> None:
    # Only 15 results; window=10 requires >=20.
    history = [_result(passed=True)] * 10 + [_result(passed=False)] * 5
    assert regression_check("pw_1", history, window=10) is None


def test_small_delta_below_threshold() -> None:
    # 10/10 historical pass, 8/10 recent pass — delta 0.2 exactly.
    # With min_delta=0.2, delta must exceed 0.2, so this is below threshold.
    history = [_result(passed=True)] * 10 + [_result(passed=True)] * 8 + [_result(passed=False)] * 2
    alert = regression_check("pw_1", history, window=10, min_delta=0.25)
    assert alert is None


def test_alert_delta_property() -> None:
    history = [_result(passed=True)] * 20 + [_result(passed=False)] * 10
    alert = regression_check("pw_1", history, window=10, min_delta=0.2)
    assert alert is not None
    assert alert.delta == pytest.approx(alert.historical_pass_rate - alert.recent_pass_rate)
