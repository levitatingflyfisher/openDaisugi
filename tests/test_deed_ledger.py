"""Stage 8 — the deed ledger (reversibility).

A side-effecting step records its effect class and, where it can, a reversal
handle, so a harness can roll back a wrong-but-allowed step *from the ledger
alone* — no model, no executor, no re-run. Irreversible effects are marked
honestly and never claim a handle they do not have; a receipt only claims
``reversibility == "none"`` when it positively knows nothing was mutated.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi import deeds
from opendaisugi.executor import default_executors
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    FileWriteStep,
    Permission,
    ShellStep,
)
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor

pytestmark = pytest.mark.asyncio


def _env(tmp_path: Path) -> Envelope:
    return Envelope(
        generated_by="test",
        task="deed-ledger",
        permissions=Permission(
            shell=True,
            shell_allowlist=["echo"],
            file_read=[f"{tmp_path}/**"],
            file_write=[f"{tmp_path}/**"],
        ),
    )


def _sup(tmp_path: Path) -> tuple[Supervisor, Journal]:
    journal = Journal(data_dir=tmp_path / "journal")
    return Supervisor(executors=default_executors(), journal=journal), journal


async def _run(sup: Supervisor, tmp_path: Path, steps) -> object:
    plan = ActionPlan(source="t", task="deed-ledger", steps=steps)
    return await sup.run(plan, _env(tmp_path))


def _receipt(journal: Journal, run_id: str, step_id: str):
    return {r.step_id: r for r in journal.receipts_for_run(run_id)}[step_id]


async def test_new_file_write_is_reversible_with_delete_handle(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    sup, journal = _sup(tmp_path)
    target = tmp_path / "new.txt"
    session = await _run(sup, tmp_path, [FileWriteStep(id="w", path=str(target), content="hi")])
    assert session.status == RunStatus.SUCCEEDED, session
    r = _receipt(journal, session.id, "w")
    assert r.effect_class == "file_write"
    assert r.reversibility == "reversible"
    assert r.reversal is not None
    assert r.reversal.prior_existed is False
    assert r.reversal.prior_content is None


async def test_overwrite_captures_prior_content(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    target = tmp_path / "exists.txt"
    target.write_text("ORIGINAL")
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [FileWriteStep(id="w", path=str(target), content="NEW")])
    assert session.status == RunStatus.SUCCEEDED, session
    assert target.read_text() == "NEW"
    r = _receipt(journal, session.id, "w")
    assert r.reversibility == "reversible"
    assert r.reversal.prior_existed is True
    assert r.reversal.prior_content == "ORIGINAL"


async def test_rollback_restores_overwrite_and_deletes_new_no_model(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    existing = tmp_path / "exists.txt"
    existing.write_text("ORIGINAL")
    created = tmp_path / "sub" / "created.txt"  # forces a new directory too
    sup, journal = _sup(tmp_path)
    session = await _run(
        sup,
        tmp_path,
        [
            FileWriteStep(id="w1", path=str(existing), content="CLOBBERED"),
            FileWriteStep(id="w2", path=str(created), content="junk", depends_on=["w1"]),
        ],
    )
    assert session.status == RunStatus.SUCCEEDED, session
    assert existing.read_text() == "CLOBBERED"
    assert created.exists()

    # Roll back from the ledger alone — no Supervisor, no executor, no model.
    report = deeds.rollback_run(journal, session.id)

    assert existing.read_text() == "ORIGINAL"  # prior content restored
    assert not created.exists()  # new file deleted
    assert not created.parent.exists()  # the directory it created is gone too
    assert set(report.undone) == {str(existing), str(created)}
    assert report.skipped == []


async def test_shell_step_is_irreversible_no_handle(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [ShellStep(id="s", command="echo hi")])
    assert session.status == RunStatus.SUCCEEDED, session
    r = _receipt(journal, session.id, "s")
    assert r.effect_class == "shell"
    assert r.reversibility == "irreversible"
    assert r.reversal is None


async def test_file_read_is_none_and_rollback_skips_it(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    src = tmp_path / "read.txt"
    src.write_text("data")
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [FileReadStep(id="r", path=str(src))])
    assert session.status == RunStatus.SUCCEEDED, session
    r = _receipt(journal, session.id, "r")
    assert r.effect_class == "file_read"
    assert r.reversibility == "none"
    assert r.reversal is None
    report = deeds.rollback_run(journal, session.id)
    assert report.undone == []
    assert [s["step_id"] for s in report.skipped] == []  # "none" deeds are not even skips


async def test_oversized_prior_is_irreversible(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    monkeypatch.setattr("opendaisugi.executor.MAX_REVERSAL_BYTES", 4)
    target = tmp_path / "big.txt"
    target.write_text("way more than four bytes")
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [FileWriteStep(id="w", path=str(target), content="x")])
    assert session.status == RunStatus.SUCCEEDED, session
    r = _receipt(journal, session.id, "w")
    assert r.effect_class == "file_write"
    assert r.reversibility == "irreversible"  # honest: cannot hold the prior image
    assert r.reversal is None


async def test_binary_prior_is_irreversible(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    target = tmp_path / "bin.dat"
    target.write_bytes(b"\xff\xfe\x00\x01not utf8")
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [FileWriteStep(id="w", path=str(target), content="x")])
    assert session.status == RunStatus.SUCCEEDED, session
    r = _receipt(journal, session.id, "w")
    assert r.reversibility == "irreversible"
    assert r.reversal is None


async def test_refused_write_is_none_never_false_undo(tmp_path, monkeypatch):
    """A write refused before it mutates claims ``none``, never ``reversible``."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    # Symlink at the target: the executor refuses (rc=2) before writing anything.
    real = tmp_path / "real.txt"
    real.write_text("safe")
    link = tmp_path / "link.txt"
    link.symlink_to(real)
    sup, journal = _sup(tmp_path)
    session = await _run(sup, tmp_path, [FileWriteStep(id="w", path=str(link), content="evil")])
    # The step fails (refused); the receipt must not claim a reversible handle.
    r = _receipt(journal, session.id, "w")
    assert r.effect_class == "file_write"
    assert r.reversibility == "none"  # positively knows nothing was written
    assert r.reversal is None
    assert real.read_text() == "safe"  # target untouched


async def test_journal_roundtrips_deed_fields_and_migrates_old_db(tmp_path):
    """append_receipt/receipts_for_run preserve the deed fields, and an
    old-schema DB (no deed columns) is migrated on open by the new code."""
    import sqlite3

    from opendaisugi.models import ReversalHandle

    # Build a receipts table WITHOUT the deed columns (pre-Stage-8 schema), at
    # the exact path the Journal opens: <data_dir>/journal/index.db.
    data_dir = tmp_path / "old"
    journal_dir = data_dir / "journal"
    journal_dir.mkdir(parents=True)
    con = sqlite3.connect(journal_dir / "index.db")
    con.execute(
        "CREATE TABLE receipts (run_id TEXT NOT NULL, step_id TEXT NOT NULL, "
        "timestamp REAL NOT NULL, evidence_hash TEXT NOT NULL, "
        "verify_result INTEGER NOT NULL, verify_details TEXT NOT NULL DEFAULT '', "
        "evidence_json TEXT NOT NULL DEFAULT '{}', model_id TEXT, "
        "PRIMARY KEY (run_id, step_id))"
    )
    con.execute("PRAGMA user_version = 4")  # pre-Stage-8
    con.commit()
    con.close()

    journal = Journal(data_dir=data_dir)  # opening runs the migration
    from opendaisugi.models import Receipt, compute_evidence_hash

    ev = {"rc": 0}
    handle = ReversalHandle(
        kind="file_write", path="/w/x.txt", prior_existed=True, prior_content="OLD"
    )
    journal.append_receipt(
        Receipt(
            step_id="w",
            run_id="run1",
            timestamp=1.0,
            evidence=ev,
            evidence_hash=compute_evidence_hash(ev),
            verify_result=True,
            effect_class="file_write",
            reversibility="reversible",
            reversal=handle,
        )
    )
    got = journal.receipts_for_run("run1")[0]
    assert got.effect_class == "file_write"
    assert got.reversibility == "reversible"
    assert got.reversal is not None
    assert got.reversal.path == "/w/x.txt"
    assert got.reversal.prior_content == "OLD"


async def test_parallel_file_writes_keep_reversal_handles(tmp_path, monkeypatch):
    """The prefetch/parallel path must not drop the handle. A false 'none' on a
    write that actually mutated is exactly the fail-open this stage prevents, and
    it hides on the concurrent path — so exercise two independent writes at once."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    a = tmp_path / "a.txt"
    a.write_text("A0")
    b = tmp_path / "b.txt"
    b.write_text("B0")
    journal = Journal(data_dir=tmp_path / "journal")
    sup = Supervisor(executors=default_executors(), journal=journal, max_parallel=2)
    plan = ActionPlan(
        source="t",
        task="deed-ledger",
        steps=[
            FileWriteStep(id="wa", path=str(a), content="A1"),
            FileWriteStep(id="wb", path=str(b), content="B1"),  # independent: no depends_on
        ],
    )
    session = await sup.run(plan, _env(tmp_path))
    assert session.status == RunStatus.SUCCEEDED, session
    ra = _receipt(journal, session.id, "wa")
    rb = _receipt(journal, session.id, "wb")
    assert ra.reversibility == "reversible"
    assert ra.reversal is not None and ra.reversal.prior_content == "A0"
    assert rb.reversibility == "reversible"
    assert rb.reversal is not None and rb.reversal.prior_content == "B0"


async def test_rollback_repeat_writes_restores_original(tmp_path, monkeypatch):
    """Two writes to the same path in one run: newest-first rollback must land on
    the true pre-run content, not the intermediate one. This ordering is
    load-bearing — a refactor to 'apply each path once' would silently break it."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    p = tmp_path / "p.txt"
    p.write_text("ORIGINAL")
    journal = Journal(data_dir=tmp_path / "journal")
    sup = Supervisor(executors=default_executors(), journal=journal)
    plan = ActionPlan(
        source="t",
        task="deed-ledger",
        steps=[
            FileWriteStep(id="w1", path=str(p), content="MID"),
            FileWriteStep(id="w2", path=str(p), content="FINAL", depends_on=["w1"]),
        ],
    )
    session = await sup.run(plan, _env(tmp_path))
    assert session.status == RunStatus.SUCCEEDED, session
    assert p.read_text() == "FINAL"
    deeds.rollback_run(journal, session.id)
    assert p.read_text() == "ORIGINAL"  # w2(→MID) undone first, then w1(→ORIGINAL)


async def test_touched_files_reconstructs_pre_state(tmp_path, monkeypatch):
    """Criterion 3: the ledger folds into a ground-truth view of which files
    the run touched and what was there before — reconstructed, not replayed."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    existing = tmp_path / "e.txt"
    existing.write_text("BEFORE")
    fresh = tmp_path / "f.txt"
    sup, journal = _sup(tmp_path)
    session = await _run(
        sup,
        tmp_path,
        [
            FileWriteStep(id="w1", path=str(existing), content="AFTER"),
            FileWriteStep(id="w2", path=str(fresh), content="AFTER", depends_on=["w1"]),
        ],
    )
    assert session.status == RunStatus.SUCCEEDED, session
    view = deeds.touched_files(journal, session.id)
    assert view[str(existing)].pre_existed is True
    assert view[str(existing)].pre_content == "BEFORE"
    assert view[str(fresh)].pre_existed is False
