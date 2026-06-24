"""Journal receipt append/read (v0.18 L2)."""
from pathlib import Path

from opendaisugi.journal import Journal
from opendaisugi.models import Receipt, compute_evidence_hash


def _make_receipt(step_id: str, run_id: str = "r1", ok: bool = True) -> Receipt:
    ev = {"step": step_id}
    return Receipt(
        step_id=step_id, run_id=run_id, timestamp=1000.0,
        evidence=ev, evidence_hash=compute_evidence_hash(ev),
        verify_result=ok, verify_details="",
    )


def test_append_and_read_receipts(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    j.append_receipt(_make_receipt("s1"))
    j.append_receipt(_make_receipt("s2"))
    receipts = j.receipts_for_run("r1")
    assert {r.step_id for r in receipts} == {"s1", "s2"}


def test_receipts_scoped_by_run(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    j.append_receipt(_make_receipt("s1", run_id="r1"))
    j.append_receipt(_make_receipt("s1", run_id="r2"))
    assert len(j.receipts_for_run("r1")) == 1
    assert len(j.receipts_for_run("r2")) == 1


def test_receipts_empty_run(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    assert j.receipts_for_run("nonexistent") == []


def test_receipt_reread_preserves_evidence(tmp_path: Path):
    j = Journal(data_dir=tmp_path)
    original = _make_receipt("s1")
    j.append_receipt(original)
    reread = j.receipts_for_run("r1")[0]
    assert reread.evidence == original.evidence
    assert reread.evidence_hash == original.evidence_hash
    assert reread.verify_result == original.verify_result


def test_append_is_idempotent(tmp_path: Path):
    """Replaying the same (run_id, step_id) receipt should not duplicate rows."""
    j = Journal(data_dir=tmp_path)
    j.append_receipt(_make_receipt("s1"))
    j.append_receipt(_make_receipt("s1"))  # same keys
    assert len(j.receipts_for_run("r1")) == 1
