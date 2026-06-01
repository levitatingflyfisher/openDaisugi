"""Tests for journal refinement persistence."""

import time

from opendaisugi.journal import Journal
from opendaisugi.models import ShellStep, VerificationResult, Violation
from opendaisugi.refinement import RefinementLog, RefinementRecord


def _make_record(**overrides):
    defaults = dict(
        step=ShellStep(id="s1", command="rm -rf /"),
        violations=[
            Violation(stage="permissions", message="shell not allowed", detail={"step": "s1"})
        ],
        z3_counterexample={"shell": False},
        envelope_id="env_abc12345",
        fallback_action="halted",
        recomputed_step=None,
        recomputed_verification=None,
        timestamp=time.time(),
    )
    defaults.update(overrides)
    return RefinementRecord(**defaults)


def test_write_refinement_and_get_back(tmp_path):
    journal = Journal(data_dir=tmp_path)
    record = _make_record()
    journal.write_refinement(record, session_id="run_test1234")
    log = journal.get_refinements("run_test1234")
    assert isinstance(log, RefinementLog)
    assert log.session_id == "run_test1234"
    assert len(log.records) == 1
    assert log.records[0].fallback_action == "halted"
    assert log.records[0].envelope_id == "env_abc12345"


def test_write_multiple_refinements_same_session(tmp_path):
    journal = Journal(data_dir=tmp_path)
    r1 = _make_record(timestamp=1.0)
    r2 = _make_record(fallback_action="recomputed", timestamp=2.0,
                       recomputed_step=ShellStep(id="s1_v2", command="echo ok"),
                       recomputed_verification=VerificationResult(
                           ok=True, violations=[], warnings=[],
                           envelope_id="env_abc12345", plan_id="p", duration_ms=0.5))
    journal.write_refinement(r1, session_id="run_multi")
    journal.write_refinement(r2, session_id="run_multi")
    log = journal.get_refinements("run_multi")
    assert len(log.records) == 2
    assert log.records[0].timestamp < log.records[1].timestamp


def test_get_refinements_empty_session(tmp_path):
    journal = Journal(data_dir=tmp_path)
    log = journal.get_refinements("run_nonexistent")
    assert log.session_id == "run_nonexistent"
    assert log.records == []


def test_get_refinements_different_sessions_isolated(tmp_path):
    journal = Journal(data_dir=tmp_path)
    r1 = _make_record(timestamp=1.0)
    r2 = _make_record(timestamp=2.0)
    journal.write_refinement(r1, session_id="run_a")
    journal.write_refinement(r2, session_id="run_b")
    log_a = journal.get_refinements("run_a")
    log_b = journal.get_refinements("run_b")
    assert len(log_a.records) == 1
    assert len(log_b.records) == 1


def test_write_refinement_is_best_effort(tmp_path, monkeypatch):
    """write_refinement logs but does not raise on SQLite errors."""
    journal = Journal(data_dir=tmp_path)
    record = _make_record()

    import sqlite3
    original_connect = sqlite3.connect

    def broken_connect(*args, **kwargs):
        raise sqlite3.OperationalError("disk full")

    monkeypatch.setattr(sqlite3, "connect", broken_connect)
    # Should not raise
    journal.write_refinement(record, session_id="run_broken")


def test_write_refinement_stores_cache_key(tmp_path):
    journal = Journal(data_dir=tmp_path)
    record = _make_record(cache_key="key_abc")
    journal.write_refinement(record, session_id="run_with_key")
    log = journal.get_refinements("run_with_key")
    assert len(log.records) == 1
    assert log.records[0].cache_key == "key_abc"


def test_get_refinements_by_key_returns_matching_records(tmp_path):
    journal = Journal(data_dir=tmp_path)
    r1 = _make_record(cache_key="key_a", timestamp=1.0)
    r2 = _make_record(cache_key="key_b", timestamp=2.0)
    r3 = _make_record(cache_key="key_a", timestamp=3.0)
    journal.write_refinement(r1, session_id="run_x")
    journal.write_refinement(r2, session_id="run_x")
    journal.write_refinement(r3, session_id="run_y")

    results = journal.get_refinements_by_key("key_a")
    assert len(results) == 2
    assert results[0].timestamp == 1.0
    assert results[1].timestamp == 3.0


def test_get_refinements_by_key_empty_for_unknown(tmp_path):
    journal = Journal(data_dir=tmp_path)
    assert journal.get_refinements_by_key("unknown_key") == []


def test_get_refinements_by_key_excludes_null_cache_key(tmp_path):
    journal = Journal(data_dir=tmp_path)
    r_with = _make_record(cache_key="key_1")
    r_without = _make_record(cache_key=None)
    journal.write_refinement(r_with, session_id="run_z")
    journal.write_refinement(r_without, session_id="run_z")

    results = journal.get_refinements_by_key("key_1")
    assert len(results) == 1
    assert results[0].cache_key == "key_1"
