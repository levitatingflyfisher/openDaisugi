"""Tests for the Receipt Pydantic model (v0.18 L1)."""
from opendaisugi.models import Receipt, compute_evidence_hash


def test_receipt_construction_minimal():
    r = Receipt(
        step_id="s1", run_id="r1", timestamp=1000.0,
        evidence={}, evidence_hash=compute_evidence_hash({}),
        verify_result=True,
    )
    assert r.step_id == "s1"
    assert r.verify_details == ""


def test_evidence_hash_is_content_addressed():
    # same content, different key order: same hash
    assert compute_evidence_hash({"b": 2, "a": 1}) == compute_evidence_hash({"a": 1, "b": 2})
    # different content: different hash
    assert compute_evidence_hash({"a": 1}) != compute_evidence_hash({"a": 2})


def test_receipt_roundtrips_through_json():
    r = Receipt(
        step_id="s2", run_id="r1", timestamp=1000.0,
        evidence={"exit_code": 0, "stdout_hash": "abc"},
        evidence_hash=compute_evidence_hash({"exit_code": 0, "stdout_hash": "abc"}),
        verify_result=True, verify_details="exit 0",
    )
    r2 = Receipt.model_validate_json(r.model_dump_json())
    assert r2 == r


def test_receipt_model_id_default_none():
    """v0.19 L1: model_id field defaults to None for non-LLM-produced receipts."""
    r = Receipt(
        step_id="s1", run_id="r1", timestamp=1.0,
        evidence={}, evidence_hash=compute_evidence_hash({}),
        verify_result=True,
    )
    assert r.model_id is None


def test_receipt_with_model_id_roundtrips():
    """v0.19 L1: receipts produced by a delegating executor record which model
    produced the evidence; this is the per-receipt selection signal."""
    r = Receipt(
        step_id="s1", run_id="r1", timestamp=1.0,
        evidence={}, evidence_hash=compute_evidence_hash({}),
        verify_result=True, model_id="haiku",
    )
    r2 = Receipt.model_validate_json(r.model_dump_json())
    assert r2.model_id == "haiku"


def test_receipt_domain_agnostic_evidence_shapes():
    """Evidence dict holds arbitrary content — shell, email, robotic, fs alike."""
    shell_ev = {"exit_code": 0, "stdout_hash": "abc"}
    email_ev = {"message_id": "<1@example>", "recipient": "a@b.c"}
    robotic_ev = {"final_joint_angles": [0.1, 0.2, 0.3], "pose_error_mm": 0.4}
    for ev in (shell_ev, email_ev, robotic_ev):
        r = Receipt(
            step_id="x", run_id="y", timestamp=0.0,
            evidence=ev, evidence_hash=compute_evidence_hash(ev),
            verify_result=True,
        )
        assert r.evidence == ev
        assert len(r.evidence_hash) == 64  # sha256 hex
