"""Tests for opendaisugi.journal — SQLite + YAML two-layer trace store."""

import json
import sqlite3
from pathlib import Path
from unittest.mock import patch as mock_patch

import pytest
import yaml

from opendaisugi import journal as journal_module
from opendaisugi.dag import topological_order
from opendaisugi.journal import Journal, JournalStats, ReplayResult, TraceRecord
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    Trace,
    VerificationResult,
    Violation,
)
from opendaisugi.run_session import RunSession, RunStatus, StepOutcome


def test_journal_creates_data_dir_structure(tmp_path):
    j = Journal(data_dir=tmp_path)
    assert (tmp_path / "journal").is_dir()
    assert (tmp_path / "journal" / "traces").is_dir()
    assert (tmp_path / "journal" / "index.db").exists()


def test_journal_creates_sqlite_schema(tmp_path):
    Journal(data_dir=tmp_path)
    with sqlite3.connect(tmp_path / "journal" / "index.db") as con:
        cur = con.execute("SELECT name FROM sqlite_master WHERE type='table'")
        tables = {row[0] for row in cur.fetchall()}
    assert "traces" in tables


def test_journal_schema_has_spec_columns(tmp_path):
    Journal(data_dir=tmp_path)
    with sqlite3.connect(tmp_path / "journal" / "index.db") as con:
        cur = con.execute("PRAGMA table_info(traces)")
        columns = {row[1] for row in cur.fetchall()}
    # Per spec §"SQLite Schema"
    expected = {
        "id", "created_at", "task", "plan_id", "envelope_id",
        "ok", "duration_ms", "violations_json",
    }
    assert expected.issubset(columns)


def test_journal_init_is_idempotent(tmp_path):
    # Calling Journal() twice on the same data_dir must not fail.
    Journal(data_dir=tmp_path)
    Journal(data_dir=tmp_path)  # second init should be a no-op
    assert (tmp_path / "journal" / "index.db").exists()


def _sample_envelope() -> Envelope:
    return Envelope(
        id="env_test01",
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


def _sample_plan() -> ActionPlan:
    return ActionPlan(
        id="plan_test01",
        source="test",
        task="t",
        steps=[ShellStep(id="s1", command="echo hi")],
    )


def _sample_result(ok: bool = True) -> VerificationResult:
    return VerificationResult(
        ok=ok,
        violations=[] if ok else [Violation(stage="permissions", message="x")],
        warnings=[],
        envelope_id="env_test01",
        plan_id="plan_test01",
        duration_ms=1.23,
    )


def test_log_writes_yaml_trace_file(tmp_path):
    j = Journal(data_dir=tmp_path)
    trace_id = j.log(
        task="demo task",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
        trace_id="2026-04-09-cafebabe",
        created_at="2026-04-09T14:30:00Z",
    )
    assert trace_id == "2026-04-09-cafebabe"
    yaml_path = tmp_path / "journal" / "traces" / "2026-04-09-cafebabe.yaml"
    assert yaml_path.exists()


def test_logged_yaml_contains_full_envelope_and_plan(tmp_path):
    j = Journal(data_dir=tmp_path)
    j.log(
        task="demo task",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
        trace_id="2026-04-09-cafebabe",
        created_at="2026-04-09T14:30:00Z",
    )
    yaml_path = tmp_path / "journal" / "traces" / "2026-04-09-cafebabe.yaml"
    loaded = yaml.safe_load(yaml_path.read_text())
    # Top-level keys per spec §Journal YAML schema
    assert loaded["id"] == "2026-04-09-cafebabe"
    assert loaded["created_at"] == "2026-04-09T14:30:00Z"
    assert loaded["task"] == "demo task"
    # Replay discipline: full envelope body, not just ID
    assert loaded["envelope"]["id"] == "env_test01"
    assert loaded["envelope"]["permissions"]["shell"] is True
    assert loaded["envelope"]["permissions"]["shell_allowlist"] == ["echo"]
    # Full plan body too
    assert loaded["plan"]["id"] == "plan_test01"
    assert loaded["plan"]["steps"][0]["command"] == "echo hi"
    # Full result
    assert loaded["result"]["ok"] is True


def test_log_writes_sqlite_index_row(tmp_path):
    j = Journal(data_dir=tmp_path)
    j.log(
        task="demo task",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(ok=False),
        trace_id="2026-04-09-deadbeef",
        created_at="2026-04-09T14:30:00Z",
    )
    with sqlite3.connect(tmp_path / "journal" / "index.db") as con:
        cur = con.execute(
            "SELECT id, task, plan_id, envelope_id, ok, duration_ms, violations_json "
            "FROM traces WHERE id = ?",
            ("2026-04-09-deadbeef",),
        )
        row = cur.fetchone()
    assert row is not None
    assert row[0] == "2026-04-09-deadbeef"
    assert row[1] == "demo task"
    assert row[2] == "plan_test01"
    assert row[3] == "env_test01"
    assert row[4] == 0  # ok=False serialized as 0
    assert row[5] == 1.23
    violations = json.loads(row[6])
    assert violations[0]["stage"] == "permissions"


def test_log_generates_trace_id_when_not_provided(tmp_path):
    j = Journal(data_dir=tmp_path)
    trace_id = j.log(
        task="auto id task",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
    )
    # Spec format: YYYY-MM-DD-<8 hex chars>
    assert len(trace_id) == len("2026-04-09-") + 8
    assert trace_id[4] == "-" and trace_id[7] == "-" and trace_id[10] == "-"
    yaml_path = tmp_path / "journal" / "traces" / f"{trace_id}.yaml"
    assert yaml_path.exists()


def test_load_trace_returns_trace_record(tmp_path):
    j = Journal(data_dir=tmp_path)
    trace_id = j.log(
        task="demo task",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
        trace_id="2026-04-09-loadtest",
        created_at="2026-04-09T14:30:00Z",
    )
    record = j.load_trace(trace_id)
    assert isinstance(record, TraceRecord)
    assert record.id == "2026-04-09-loadtest"
    assert record.created_at == "2026-04-09T14:30:00Z"
    assert record.task == "demo task"
    assert isinstance(record.envelope, Envelope)
    assert isinstance(record.plan, ActionPlan)
    assert isinstance(record.result, VerificationResult)
    assert record.envelope.id == "env_test01"
    assert record.plan.steps[0].command == "echo hi"
    assert record.result.ok is True


def test_load_trace_missing_id_raises_file_not_found(tmp_path):
    j = Journal(data_dir=tmp_path)
    with pytest.raises(FileNotFoundError, match="2026-04-09-nosuch"):
        j.load_trace("2026-04-09-nosuch")


def test_list_recent_returns_empty_on_fresh_journal(tmp_path):
    j = Journal(data_dir=tmp_path)
    assert j.list_recent() == []


def test_list_recent_returns_trace_metadata(tmp_path):
    j = Journal(data_dir=tmp_path)
    j.log(
        task="first",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
        trace_id="2026-04-09-00000001",
        created_at="2026-04-09T10:00:00Z",
    )
    j.log(
        task="second",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(ok=False),
        trace_id="2026-04-09-00000002",
        created_at="2026-04-09T11:00:00Z",
    )
    recent = j.list_recent()
    assert len(recent) == 2
    # Ordered newest-first
    assert isinstance(recent[0], Trace)
    assert recent[0].id == "2026-04-09-00000002"
    assert recent[0].task == "second"
    assert recent[0].ok is False
    assert recent[1].id == "2026-04-09-00000001"
    assert recent[1].ok is True


def test_list_recent_honors_limit(tmp_path):
    j = Journal(data_dir=tmp_path)
    for i in range(5):
        j.log(
            task=f"task{i}",
            envelope=_sample_envelope(),
            plan=_sample_plan(),
            result=_sample_result(),
            trace_id=f"2026-04-09-{i:08d}",
            created_at=f"2026-04-09T10:0{i}:00Z",
        )
    recent = j.list_recent(limit=2)
    assert len(recent) == 2
    assert recent[0].id == "2026-04-09-00000004"
    assert recent[1].id == "2026-04-09-00000003"


def test_stats_empty_journal(tmp_path):
    j = Journal(data_dir=tmp_path)
    stats = j.stats()
    assert isinstance(stats, JournalStats)
    assert stats.total == 0
    assert stats.passed == 0
    assert stats.failed == 0
    assert stats.avg_duration_ms == 0.0


def test_stats_mixed_results(tmp_path):
    j = Journal(data_dir=tmp_path)
    # Two passing traces with duration_ms=1.23, one failing with duration_ms=1.23
    for i, ok in enumerate([True, True, False]):
        j.log(
            task=f"task{i}",
            envelope=_sample_envelope(),
            plan=_sample_plan(),
            result=_sample_result(ok=ok),
            trace_id=f"2026-04-09-{i:08d}",
            created_at=f"2026-04-09T10:0{i}:00Z",
        )
    stats = j.stats()
    assert stats.total == 3
    assert stats.passed == 2
    assert stats.failed == 1
    assert stats.avg_duration_ms == pytest.approx(1.23)


def test_replay_no_drift_when_verify_agrees(tmp_path):
    j = Journal(data_dir=tmp_path)
    j.log(
        task="shell echo",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(ok=True),
        trace_id="2026-04-09-nodrift0",
        created_at="2026-04-09T10:00:00Z",
    )
    replay = j.replay("2026-04-09-nodrift0")
    assert isinstance(replay, ReplayResult)
    assert replay.trace_id == "2026-04-09-nodrift0"
    assert replay.original_ok is True
    assert replay.replayed_ok is True
    assert replay.drift is False


def test_replay_detects_drift_when_verify_disagrees(tmp_path, monkeypatch):
    j = Journal(data_dir=tmp_path)
    j.log(
        task="shell echo",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(ok=True),
        trace_id="2026-04-09-drift000",
        created_at="2026-04-09T10:00:00Z",
    )

    # Simulate verification-code drift by making verify() return ok=False.
    from opendaisugi.models import VerificationResult
    def fake_verify(plan, envelope, *, z3_timeout_ms=500):
        return VerificationResult(
            ok=False,
            violations=[],
            warnings=["drift simulation"],
            envelope_id=envelope.id,
            plan_id=plan.id,
            duration_ms=0.1,
        )

    monkeypatch.setattr(journal_module, "verify", fake_verify)

    replay = j.replay("2026-04-09-drift000")
    assert replay.original_ok is True
    assert replay.replayed_ok is False
    assert replay.drift is True


def test_replay_raises_on_missing_trace(tmp_path):
    j = Journal(data_dir=tmp_path)
    with pytest.raises(FileNotFoundError):
        j.replay("2026-04-09-nosuch0")


def test_replay_passes_z3_timeout_through(tmp_path, monkeypatch):
    # Confirm the Journal forwards its z3_timeout_ms config to verify().
    j = Journal(data_dir=tmp_path, z3_timeout_ms=777)
    j.log(
        task="t",
        envelope=_sample_envelope(),
        plan=_sample_plan(),
        result=_sample_result(),
        trace_id="2026-04-09-timeout0",
        created_at="2026-04-09T10:00:00Z",
    )
    captured = {}
    def spy_verify(plan, envelope, *, z3_timeout_ms=500):
        captured["z3_timeout_ms"] = z3_timeout_ms
        from opendaisugi.models import VerificationResult
        return VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=0.1,
        )
    monkeypatch.setattr(journal_module, "verify", spy_verify)

    j.replay("2026-04-09-timeout0")
    assert captured["z3_timeout_ms"] == 777


def test_log_rolls_back_sqlite_on_yaml_write_failure(tmp_path):
    """YAML write failure inside the transaction causes SQLite auto-rollback."""
    j = Journal(data_dir=tmp_path)

    original_write = Path.write_text

    def failing_write(self_path, *args, **kwargs):
        if str(self_path).endswith(".yaml") and "traces" in str(self_path):
            raise OSError("disk full")
        return original_write(self_path, *args, **kwargs)

    with mock_patch.object(Path, "write_text", failing_write):
        with pytest.raises(OSError, match="disk full"):
            j.log(
                task="crash test",
                envelope=_sample_envelope(),
                plan=_sample_plan(),
                result=_sample_result(),
                trace_id="2026-04-09-crashme0",
                created_at="2026-04-09T14:30:00Z",
            )

    # After rollback: neither store should contain the trace
    assert j.stats().total == 0
    with pytest.raises(FileNotFoundError):
        j.load_trace("2026-04-09-crashme0")


def test_log_rejects_trace_id_with_path_traversal(tmp_path):
    j = Journal(data_dir=tmp_path)
    with pytest.raises(ValueError, match="Invalid trace_id"):
        j.log(
            task="evil",
            envelope=_sample_envelope(),
            plan=_sample_plan(),
            result=_sample_result(),
            trace_id="../../etc/evil",
            created_at="2026-04-09T14:30:00Z",
        )


def test_log_rejects_trace_id_with_slashes(tmp_path):
    j = Journal(data_dir=tmp_path)
    with pytest.raises(ValueError, match="Invalid trace_id"):
        j.log(
            task="evil",
            envelope=_sample_envelope(),
            plan=_sample_plan(),
            result=_sample_result(),
            trace_id="foo/bar",
            created_at="2026-04-09T14:30:00Z",
        )


# ---------------------------------------------------------------------------
# topological_order tests
# ---------------------------------------------------------------------------

def test_topological_order_returns_ordered_steps():
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="a", command="echo a"),
        ShellStep(id="b", command="echo b", depends_on=["a"]),
        ShellStep(id="c", command="echo c", depends_on=["b"]),
    ])
    ordered_ids = [s.id for s in topological_order(plan)]
    assert ordered_ids == ["a", "b", "c"]


def test_topological_order_handles_multi_root():
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="a", command="echo a"),
        ShellStep(id="b", command="echo b"),
        ShellStep(id="c", command="echo c", depends_on=["a", "b"]),
    ])
    ordered_ids = [s.id for s in topological_order(plan)]
    assert ordered_ids[-1] == "c"
    assert set(ordered_ids[:2]) == {"a", "b"}


# ---------------------------------------------------------------------------
# log_run / load_run / schema migration tests
# ---------------------------------------------------------------------------

def _sample_session(envelope_id="env_x", plan_id="plan_x", status=RunStatus.SUCCEEDED):
    verification = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id=envelope_id, plan_id=plan_id, duration_ms=1.0,
    )
    return RunSession(
        id="run_abcd1234",
        envelope_id=envelope_id,
        plan_id=plan_id,
        status=status,
        verification=verification,
        steps=[
            StepOutcome(
                step_id="s1", status="succeeded", approved_by="allowlist",
                rc=0, stdout="hi\n", duration_ms=2.5,
                started_at="2026-04-13T10:00:00Z", error=None,
            ),
        ],
        started_at="2026-04-13T10:00:00Z",
        ended_at="2026-04-13T10:00:01Z",
        trace_id=None,
    )


def _minimal_envelope():
    return Envelope(
        id="env_x", generated_by="test", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


def _minimal_plan():
    return ActionPlan(id="plan_x", source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])


def test_log_run_writes_yaml_and_sqlite(tmp_path):
    j = Journal(data_dir=tmp_path)
    session = _sample_session()
    trace_id = j.log_run(session, task="demo task", envelope=_minimal_envelope(),
                         plan=_minimal_plan())
    assert (tmp_path / "journal" / "traces" / f"{trace_id}.yaml").exists()


def test_log_run_round_trips_session(tmp_path):
    j = Journal(data_dir=tmp_path)
    original = _sample_session()
    trace_id = j.log_run(original, task="demo", envelope=_minimal_envelope(),
                         plan=_minimal_plan())
    loaded = j.load_run(trace_id)
    assert loaded.id == original.id
    assert loaded.status == original.status
    assert loaded.envelope_id == original.envelope_id
    assert len(loaded.steps) == 1
    assert loaded.steps[0].step_id == "s1"
    assert loaded.steps[0].rc == 0


def test_log_run_updates_sqlite_index(tmp_path):
    j = Journal(data_dir=tmp_path)
    session = _sample_session(status=RunStatus.FAILED)
    session.steps[0] = StepOutcome(
        step_id="s1", status="failed", approved_by="allowlist",
        rc=2, stdout="", duration_ms=3.0,
        started_at="2026-04-13T10:00:00Z", error=None,
    )
    trace_id = j.log_run(session, task="demo", envelope=_minimal_envelope(),
                         plan=_minimal_plan())
    con = sqlite3.connect(tmp_path / "journal" / "index.db")
    row = con.execute(
        "SELECT run_id, run_status, failed_step_id FROM traces WHERE id=?",
        (trace_id,),
    ).fetchone()
    con.close()
    assert row[0] == "run_abcd1234"
    assert row[1] == "failed"
    assert row[2] == "s1"


def test_schema_migration_adds_new_columns_to_existing_db(tmp_path):
    """A v0.0.4 index.db must gain the v0.1 and v0.2.1 columns on first open."""
    journal_dir = tmp_path / "journal"
    journal_dir.mkdir(parents=True)
    (journal_dir / "traces").mkdir()
    db_path = journal_dir / "index.db"
    with sqlite3.connect(db_path) as con:
        con.executescript("""
            CREATE TABLE traces (
                id TEXT PRIMARY KEY,
                created_at TEXT NOT NULL,
                task TEXT NOT NULL,
                plan_id TEXT NOT NULL,
                envelope_id TEXT NOT NULL,
                ok INTEGER NOT NULL,
                duration_ms REAL NOT NULL,
                violations_json TEXT NOT NULL
            );
            CREATE TABLE refinement_log (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                session_id TEXT NOT NULL,
                record_json TEXT NOT NULL,
                inserted_at REAL NOT NULL
            );
        """)
    Journal(data_dir=tmp_path)  # init triggers migration
    with sqlite3.connect(db_path) as con:
        trace_cols = [r[1] for r in con.execute("PRAGMA table_info(traces)").fetchall()]
        ref_cols = [r[1] for r in con.execute("PRAGMA table_info(refinement_log)").fetchall()]
        version = con.execute("PRAGMA user_version").fetchone()[0]
    assert "run_id" in trace_cols
    assert "run_status" in trace_cols
    assert "failed_step_id" in trace_cols
    assert "total_duration_ms" in trace_cols
    assert "cache_key" in ref_cols
    # v0.19: receipts table gains model_id; user_version bumps to 4.
    receipt_cols = [r[1] for r in sqlite3.connect(db_path).execute(
        "PRAGMA table_info(receipts)").fetchall()]
    assert "model_id" in receipt_cols
    # v0.24: traces gain structure_signature; user_version bumps to 5.
    trace_cols_post = [r[1] for r in sqlite3.connect(db_path).execute(
        "PRAGMA table_info(traces)").fetchall()]
    assert "structure_signature" in trace_cols_post
    # v0.27: provenance_log table added; user_version bumps to 6.
    prov_tables = [r[0] for r in sqlite3.connect(db_path).execute(
        "SELECT name FROM sqlite_master WHERE type='table'").fetchall()]
    assert "provenance_log" in prov_tables
    assert version == 6
