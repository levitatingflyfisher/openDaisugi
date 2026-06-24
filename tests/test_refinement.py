"""Unit tests for refinement data types."""

import time

from opendaisugi.models import (
    FileWriteStep,
    Permission,
    ShellStep,
    Envelope,
    VerificationResult,
    Violation,
)
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
        timestamp=1700000000.0,
    )
    defaults.update(overrides)
    return RefinementRecord(**defaults)


def test_refinement_record_round_trip_json():
    record = _make_record()
    json_str = record.model_dump_json()
    restored = RefinementRecord.model_validate_json(json_str)
    assert restored == record
    assert restored.fallback_action == "halted"
    assert restored.recomputed_step is None


def test_refinement_record_with_recomputed_step():
    replacement = ShellStep(id="s1_v2", command="echo safe")
    vr = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id="env_abc12345", plan_id="plan_x", duration_ms=1.5,
    )
    record = _make_record(
        fallback_action="recomputed",
        recomputed_step=replacement,
        recomputed_verification=vr,
    )
    json_str = record.model_dump_json()
    restored = RefinementRecord.model_validate_json(json_str)
    assert restored.fallback_action == "recomputed"
    assert restored.recomputed_step.id == "s1_v2"
    assert restored.recomputed_verification.ok is True


def test_refinement_record_none_z3_counterexample():
    record = _make_record(z3_counterexample=None)
    json_str = record.model_dump_json()
    restored = RefinementRecord.model_validate_json(json_str)
    assert restored.z3_counterexample is None


def test_refinement_log_round_trip():
    r1 = _make_record(timestamp=1.0)
    r2 = _make_record(fallback_action="recomputed", timestamp=2.0,
                       recomputed_step=ShellStep(id="s1_v2", command="echo ok"),
                       recomputed_verification=VerificationResult(
                           ok=True, violations=[], warnings=[],
                           envelope_id="env_abc12345", plan_id="p", duration_ms=0.5))
    log = RefinementLog(session_id="run_test1234", records=[r1, r2])
    json_str = log.model_dump_json()
    restored = RefinementLog.model_validate_json(json_str)
    assert restored.session_id == "run_test1234"
    assert len(restored.records) == 2
    assert restored.records[0].fallback_action == "halted"
    assert restored.records[1].fallback_action == "recomputed"


def test_refinement_log_empty_records():
    log = RefinementLog(session_id="run_empty1234")
    assert log.records == []
    json_str = log.model_dump_json()
    restored = RefinementLog.model_validate_json(json_str)
    assert restored.records == []


def test_refinement_record_file_write_step():
    """RefinementRecord works with non-shell step types."""
    step = FileWriteStep(id="fw1", path="/tmp/bad.txt", content="evil")
    record = _make_record(step=step)
    json_str = record.model_dump_json()
    restored = RefinementRecord.model_validate_json(json_str)
    assert restored.step.type == "file_write"
    assert restored.step.path == "/tmp/bad.txt"


def test_refinement_record_with_cache_key_round_trips():
    record = _make_record(cache_key="abc123def456")
    restored = RefinementRecord.model_validate_json(record.model_dump_json())
    assert restored.cache_key == "abc123def456"


def test_refinement_record_without_cache_key_is_none():
    record = _make_record()
    assert record.cache_key is None


def test_refinement_record_legacy_json_without_cache_key_deserializes():
    """Records serialized before v0.2.1 (no cache_key in JSON) still load."""
    legacy_json = (
        '{"step":{"id":"s1","type":"shell","command":"rm -rf /","depends_on":[]},'
        '"violations":[{"stage":"permissions","message":"shell not allowed",'
        '"detail":{"step":"s1"}}],'
        '"z3_counterexample":{"shell":false},'
        '"envelope_id":"env_abc12345",'
        '"fallback_action":"halted",'
        '"recomputed_step":null,'
        '"recomputed_verification":null,'
        '"timestamp":1700000000.0}'
    )
    restored = RefinementRecord.model_validate_json(legacy_json)
    assert restored.cache_key is None
    assert restored.envelope_id == "env_abc12345"
