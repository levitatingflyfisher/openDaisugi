"""Gardener counts integrity-failed runs as pathway failures (v0.18 L8)."""
from dataclasses import dataclass

from opendaisugi.gardener import RunOutcome, record_run_outcome


@dataclass
class _FakePathway:
    id: str = "p1"
    failure_count: int = 0
    activation_count: int = 0


def test_succeeded_and_integrity_passed_counts_as_success():
    p = _FakePathway()
    record_run_outcome(p, status="succeeded", integrity_passed=True)
    assert p.activation_count == 1
    assert p.failure_count == 0


def test_succeeded_but_integrity_failed_counts_as_failure():
    """Run said 'succeeded' but receipts missing = silent skip = failure."""
    p = _FakePathway()
    record_run_outcome(p, status="succeeded", integrity_passed=False)
    assert p.activation_count == 1
    assert p.failure_count == 1


def test_status_failed_counts_as_failure_regardless_of_integrity():
    p = _FakePathway()
    record_run_outcome(p, status="failed", integrity_passed=True)
    assert p.failure_count == 1


def test_integrity_none_gives_benefit_of_doubt():
    """Integrity None (e.g. rejected-at-verify, never executed) should not
    mark the pathway as failing."""
    p = _FakePathway()
    record_run_outcome(p, status="succeeded", integrity_passed=None)
    assert p.failure_count == 0


def test_outcomes_observer_records_run():
    p = _FakePathway()
    out: list[RunOutcome] = []
    record_run_outcome(p, status="succeeded", integrity_passed=True, outcomes=out)
    assert len(out) == 1
    assert out[0].pathway_id == "p1"
    assert out[0].integrity_passed is True
