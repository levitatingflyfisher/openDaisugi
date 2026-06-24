"""Journal: two-layer trace store.

YAML files under ``<data_dir>/journal/traces/`` are the source of truth —
each holds the FULL serialized envelope+plan+result for a single run, so
that ``Journal.replay()`` can re-verify the trace against current code.
SQLite (``<data_dir>/journal/index.db``) is a queryable metadata index,
rebuildable from the YAML files.

The journal is append-only. Nothing in v0.0.1 deletes or mutates traces.
"""

from __future__ import annotations

import json
import re
import sqlite3
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import TYPE_CHECKING
from uuid import uuid4

import yaml
from pydantic import BaseModel

from opendaisugi.models import ActionPlan, Envelope, VerificationResult
from opendaisugi.verify import verify

if TYPE_CHECKING:
    from opendaisugi.refinement import RefinementLog, RefinementRecord  # noqa: F401
    from opendaisugi.run_session import RunSession  # noqa: F401


@dataclass(frozen=True)
class TraceRecord:
    """In-memory view of a loaded YAML trace — the full replayable body.

    This is distinct from ``opendaisugi.models.Trace``, which is the
    lightweight metadata row that lives in the SQLite index.
    """

    id: str
    created_at: str
    task: str
    envelope: Envelope
    plan: ActionPlan
    result: VerificationResult


class DistillableTrace(BaseModel):
    """Lightweight trace summary for v0.3.0 distillation.

    Avoids loading YAML bodies — the Distiller only needs task text and
    IDs for clustering. Full TraceRecord comes from load_trace() per-cluster.
    """
    trace_id: str
    task: str
    envelope_id: str
    plan_id: str
    run_id: str | None
    run_status: str | None
    created_at: str
    # v0.24+: canonical step-type sequence; None on traces from v0.23 stores
    # that haven't been re-stamped. Distiller falls back to task-only
    # clustering for traces missing this.
    structure_signature: str | None = None


@dataclass(frozen=True)
class JournalStats:
    total: int
    passed: int
    failed: int
    avg_duration_ms: float


@dataclass(frozen=True)
class ReplayResult:
    """Result of re-running verify() on a stored trace.

    ``drift`` is True iff the current verify() disagrees with the
    originally stored result on ``ok`` — a signal that verification
    code has changed behavior relative to when the trace was logged.
    """

    trace_id: str
    original_ok: bool
    replayed_ok: bool
    drift: bool
    original_result: VerificationResult
    replayed_result: VerificationResult


_TRACE_ID_RE = re.compile(r"^[a-zA-Z0-9._-]+$")


@dataclass(frozen=True)
class ProvenanceRecord:
    """Lightweight provenance record for alias registration and vacuity events (v0.27.0)."""

    detail: dict


_SCHEMA = """
CREATE TABLE IF NOT EXISTS traces (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    task TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    envelope_id TEXT NOT NULL,
    ok INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    violations_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS refinement_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    record_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refinement_session ON refinement_log(session_id);
CREATE TABLE IF NOT EXISTS receipts (
    run_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    timestamp REAL NOT NULL,
    evidence_hash TEXT NOT NULL,
    verify_result INTEGER NOT NULL,
    verify_details TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '{}',
    model_id TEXT,
    PRIMARY KEY (run_id, step_id)
);
CREATE INDEX IF NOT EXISTS idx_receipts_run ON receipts(run_id);
CREATE TABLE IF NOT EXISTS hook_conversions (
    session_id TEXT PRIMARY KEY,
    trace_id TEXT NOT NULL,
    converted_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS provenance_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    detail_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
"""

_V2_MIGRATION_COLUMNS = (
    ("run_id", "TEXT"),
    ("run_status", "TEXT"),
    ("failed_step_id", "TEXT"),
    ("total_duration_ms", "REAL"),
)

_V3_REFINEMENT_COLUMNS = (
    ("cache_key", "TEXT"),
)


class Journal:
    """Append-only trace store backed by YAML files + a SQLite index.

    Instances hold a long-lived SQLite connection (``self._con``) for hot-path
    writes (append_receipt, log, log_run) so per-call connection setup
    (~0.4ms each) doesn't dominate supervised-run cost. The connection is
    opened once at __init__, reused by all methods, and closed by ``close()``
    or garbage collection. ``check_same_thread=False`` allows the journal to
    be used from async contexts; access is serialized by Python's GIL since
    all journal calls are sync. ``isolation_level=None`` puts the connection
    in autocommit mode — each ``con.execute`` that mutates commits
    immediately, matching the previous per-call ``with sqlite3.connect``
    behavior. Pre-v0.22 a fresh connection was opened per method call.
    """

    def __init__(self, *, data_dir: Path, z3_timeout_ms: int = 500) -> None:
        self.data_dir = Path(data_dir)
        self.z3_timeout_ms = z3_timeout_ms
        self._journal_dir = self.data_dir / "journal"
        self._traces_dir = self._journal_dir / "traces"
        self._db_path = self._journal_dir / "index.db"

        self._traces_dir.mkdir(parents=True, exist_ok=True)
        self._con = sqlite3.connect(
            self._db_path, check_same_thread=False, isolation_level=None,
        )
        con = self._con
        con.executescript(_SCHEMA)
        version = con.execute("PRAGMA user_version").fetchone()[0]
        if version < 2:
            for name, sql_type in _V2_MIGRATION_COLUMNS:
                try:
                    con.execute(f"ALTER TABLE traces ADD COLUMN {name} {sql_type}")
                except sqlite3.OperationalError:
                    pass  # column already exists — idempotent
            con.execute("PRAGMA user_version = 2")
        if version < 3:
            for name, sql_type in _V3_REFINEMENT_COLUMNS:
                try:
                    con.execute(f"ALTER TABLE refinement_log ADD COLUMN {name} {sql_type}")
                except sqlite3.OperationalError:
                    pass  # column already exists — idempotent
            con.execute(
                "CREATE INDEX IF NOT EXISTS idx_refinement_cache_key "
                "ON refinement_log(cache_key)"
            )
            con.execute("PRAGMA user_version = 3")
        if version < 4:
            # v0.19: receipts gain model_id column. ALTER TABLE adds it
            # if missing (idempotent across upgrades from v0.18).
            try:
                con.execute("ALTER TABLE receipts ADD COLUMN model_id TEXT")
            except sqlite3.OperationalError:
                pass  # column already exists
            con.execute("PRAGMA user_version = 4")
        if version < 5:
            # v0.24: traces gain structure_signature column. Stamped at
            # log_run / log time so the Distiller can cluster by plan
            # structure without loading YAML bodies. NULL on v0.23 rows
            # (handled by the Distiller's fallback path).
            try:
                con.execute(
                    "ALTER TABLE traces ADD COLUMN structure_signature TEXT"
                )
            except sqlite3.OperationalError:
                pass  # column already exists
            con.execute(
                "CREATE INDEX IF NOT EXISTS idx_traces_structure "
                "ON traces(structure_signature)"
            )
            con.execute("PRAGMA user_version = 5")
        if version < 6:
            # v0.27: provenance_log for alias registration + vacuity events.
            try:
                con.execute(
                    "CREATE TABLE IF NOT EXISTS provenance_log ("
                    "id INTEGER PRIMARY KEY AUTOINCREMENT, "
                    "detail_json TEXT NOT NULL, "
                    "inserted_at REAL NOT NULL"
                    ")"
                )
            except sqlite3.OperationalError:
                pass  # table already exists (created by _SCHEMA on fresh db)
            con.execute("PRAGMA user_version = 6")

    def is_session_converted(self, session_id: str) -> bool:
        """v0.22+: has this captured session already been converted to a
        journal trace via the hook-conversion path?"""
        row = self._con.execute(
            "SELECT 1 FROM hook_conversions WHERE session_id = ? LIMIT 1",
            (session_id,),
        ).fetchone()
        return row is not None

    def mark_session_converted(
        self, session_id: str, trace_id: str, *, converted_at: float | None = None,
    ) -> None:
        """v0.22+: record that a hook-captured session was converted to a
        journal trace, so subsequent auto-tend runs skip it."""
        if converted_at is None:
            converted_at = time.time()
        self._con.execute(
            "INSERT OR REPLACE INTO hook_conversions "
            "(session_id, trace_id, converted_at) VALUES (?, ?, ?)",
            (session_id, trace_id, converted_at),
        )

    def close(self) -> None:
        """Close the underlying SQLite connection. Safe to call repeatedly."""
        if getattr(self, "_con", None) is not None:
            try:
                self._con.close()
            except Exception:
                pass
            self._con = None

    def __del__(self):  # pragma: no cover - GC path
        self.close()

    def log(
        self,
        *,
        task: str,
        envelope: Envelope,
        plan: ActionPlan,
        result: VerificationResult,
        trace_id: str | None = None,
        created_at: str | None = None,
    ) -> str:
        """Append a trace. Returns the trace id.

        ``trace_id`` and ``created_at`` are optional injection points for
        deterministic testing — callers normally omit them and let the
        journal generate them.

        The YAML file is the source of truth and MUST contain the full
        serialized envelope+plan+result so the trace is replayable.
        """
        if created_at is None:
            created_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        if trace_id is None:
            date_prefix = created_at[:10]
            trace_id = f"{date_prefix}-{uuid4().hex[:8]}"

        if not _TRACE_ID_RE.match(trace_id):
            raise ValueError(
                f"Invalid trace_id {trace_id!r}: must contain only "
                f"alphanumeric characters, hyphens, underscores, and dots"
            )

        payload = {
            "id": trace_id,
            "created_at": created_at,
            "task": task,
            "envelope": envelope.model_dump(mode="json"),
            "plan": plan.model_dump(mode="json"),
            "result": result.model_dump(mode="json"),
        }

        yaml_path = self._traces_dir / f"{trace_id}.yaml"

        # Write YAML *before* the SQLite INSERT so a crash between them
        # cannot leave an orphaned index row pointing at a missing body.
        # An orphaned YAML file is harmless; an orphaned index row breaks
        # load_trace / replay permanently for that id.
        yaml_path.write_text(yaml.safe_dump(payload, sort_keys=False))
        try:
            from opendaisugi.distiller import plan_structure_signature
            structure_signature = plan_structure_signature(plan)
        except Exception:
            structure_signature = None
        try:
            con = self._con
            con.execute(
                "INSERT INTO traces "
                "(id, created_at, task, plan_id, envelope_id, ok, duration_ms, "
                " violations_json, structure_signature) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    trace_id,
                    created_at,
                    task,
                    plan.id,
                    envelope.id,
                    1 if result.ok else 0,
                    result.duration_ms,
                    json.dumps([v.model_dump(mode="json") for v in result.violations]),
                    structure_signature,
                ),
            )
        except Exception:
            # SQLite write failed after YAML landed. Remove the orphan YAML
            # so a retry can use the same trace_id without a uniqueness
            # collision.
            try:
                yaml_path.unlink()
            except OSError:
                pass
            raise

        return trace_id

    def list_recent(self, *, limit: int = 20) -> list["Trace"]:
        """Return recent trace metadata rows, newest first.

        This reads from the SQLite index only — it does not load the
        YAML bodies. Use ``load_trace(id)`` to get the full envelope+plan.
        """
        from opendaisugi.models import Trace, Violation

        con = self._con
        cur = con.execute(
            "SELECT id, created_at, task, plan_id, envelope_id, ok, "
            "duration_ms, violations_json "
            "FROM traces ORDER BY created_at DESC LIMIT ?",
            (limit,),
        )
        rows = cur.fetchall()

        traces: list[Trace] = []
        for row in rows:
            violations = [Violation(**v) for v in json.loads(row[7])]
            traces.append(
                Trace(
                    id=row[0],
                    created_at=row[1],
                    task=row[2],
                    plan_id=row[3],
                    envelope_id=row[4],
                    ok=bool(row[5]),
                    duration_ms=row[6],
                    violations=violations,
                )
            )
        return traces

    def list_successful_traces(
        self, *, since: float | None = None
    ) -> list[DistillableTrace]:
        """Return traces with run_status='succeeded', newest first.

        Reads the SQLite index only — does not load YAML bodies.
        ``since`` is a Unix timestamp; only traces with created_at >= since
        are returned. Passing None returns all successful traces.
        """
        sql = (
            "SELECT id, task, envelope_id, plan_id, run_id, run_status, "
            "created_at, structure_signature "
            "FROM traces WHERE run_status = 'succeeded'"
        )
        params: tuple = ()
        if since is not None:
            # created_at is stored as ISO-8601 text; convert since to ISO for lexicographic compare.
            iso = datetime.fromtimestamp(since, tz=timezone.utc).isoformat().replace("+00:00", "Z")
            sql += " AND created_at >= ?"
            params = (iso,)
        sql += " ORDER BY created_at DESC"

        con = self._con
        rows = con.execute(sql, params).fetchall()

        return [
            DistillableTrace(
                trace_id=r[0],
                task=r[1],
                envelope_id=r[2],
                plan_id=r[3],
                run_id=r[4],
                run_status=r[5],
                created_at=r[6],
                structure_signature=r[7],
            )
            for r in rows
        ]

    def stats(self) -> JournalStats:
        """Aggregate stats from the SQLite index."""
        con = self._con
        cur = con.execute(
            "SELECT COUNT(*), "
            "SUM(CASE WHEN ok=1 THEN 1 ELSE 0 END), "
            "SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END), "
            "AVG(duration_ms) "
            "FROM traces"
        )
        total, passed, failed, avg = cur.fetchone()
        return JournalStats(
            total=total or 0,
            passed=passed or 0,
            failed=failed or 0,
            avg_duration_ms=float(avg) if avg is not None else 0.0,
        )

    def replay(self, trace_id: str) -> ReplayResult:
        """Re-run verify() on a stored trace and report drift."""
        record = self.load_trace(trace_id)
        replayed = verify(
            record.plan, record.envelope, z3_timeout_ms=self.z3_timeout_ms
        )
        return ReplayResult(
            trace_id=record.id,
            original_ok=record.result.ok,
            replayed_ok=replayed.ok,
            drift=(record.result.ok != replayed.ok),
            original_result=record.result,
            replayed_result=replayed,
        )

    def search(self, query: str, *, limit: int = 10) -> list:
        """Semantic search across trace tasks — requires the [search] extra.

        The heavy sentence-transformers import lives in ``opendaisugi._search``
        which is lazily imported here. When the extra is not installed the
        bare import raises and we re-raise with a user-visible install hint.
        """
        try:
            from opendaisugi._search import semantic_search
        except ImportError:
            raise ImportError(
                "Semantic search requires the [search] extra: "
                "uv add 'opendaisugi[search]'"
            ) from None
        return semantic_search(self, query, limit=limit)

    def load_trace(self, trace_id: str) -> TraceRecord:
        """Load a full trace (envelope + plan + result) by id.

        Raises ``FileNotFoundError`` if the YAML file does not exist.
        """
        yaml_path = self._traces_dir / f"{trace_id}.yaml"
        if not yaml_path.exists():
            raise FileNotFoundError(
                f"No trace with id {trace_id!r} at {yaml_path}"
            )
        raw = yaml.safe_load(yaml_path.read_text())
        return TraceRecord(
            id=raw["id"],
            created_at=raw["created_at"],
            task=raw["task"],
            envelope=Envelope(**raw["envelope"]),
            plan=ActionPlan(**raw["plan"]),
            result=VerificationResult(**raw["result"]),
        )

    def write_refinement(
        self,
        record: "RefinementRecord",
        *,
        session_id: str,
    ) -> None:
        """Best-effort write. Logs on failure, never raises.

        Since v0.2.1, ``record.cache_key`` is also persisted to the
        ``cache_key`` column (indexed) for fast lookup by envelope key.
        """
        import logging
        _log = logging.getLogger("opendaisugi.journal")
        try:
            con = self._con
            con.execute(
                "INSERT INTO refinement_log "
                "(session_id, record_json, inserted_at, cache_key) "
                "VALUES (?, ?, ?, ?)",
                (
                    session_id,
                    record.model_dump_json(),
                    time.time(),
                    record.cache_key,
                ),
            )
        except sqlite3.Error as exc:
            _log.warning("journal write_refinement failed: %s", exc)

    def get_refinements(self, session_id: str) -> "RefinementLog":
        """Return all refinement records for a session."""
        from opendaisugi.refinement import RefinementLog, RefinementRecord
        con = self._con
        rows = con.execute(
            "SELECT record_json FROM refinement_log "
            "WHERE session_id = ? ORDER BY inserted_at ASC",
            (session_id,),
        ).fetchall()
        records = [RefinementRecord.model_validate_json(row[0]) for row in rows]
        return RefinementLog(session_id=session_id, records=records)

    def write_provenance(self, detail: dict) -> None:
        """Best-effort write of an alias/vacuity provenance record. Logs on failure, never raises.

        v0.27.0: called by AliasRegistry.register() to record alias registration
        and vacuity verdict in the provenance_log table.
        """
        import logging
        _log = logging.getLogger("opendaisugi.journal")
        try:
            self._con.execute(
                "INSERT INTO provenance_log (detail_json, inserted_at) VALUES (?, ?)",
                (json.dumps(detail), time.time()),
            )
        except sqlite3.Error as exc:
            _log.warning("journal write_provenance failed: %s", exc)

    def get_provenance(self) -> "list[ProvenanceRecord]":
        """Return all provenance records, ordered by insertion time ascending.

        v0.27.0: returns alias registration and vacuity provenance events.
        """
        rows = self._con.execute(
            "SELECT detail_json FROM provenance_log ORDER BY inserted_at ASC"
        ).fetchall()
        return [ProvenanceRecord(detail=json.loads(row[0])) for row in rows]

    def get_refinements_by_key(self, cache_key: str) -> "list[RefinementRecord]":
        """Return all refinement records for a given envelope cache key.

        Records without a ``cache_key`` (pre-v0.2.1 rows or hand-built
        envelopes) are never returned — they cannot be associated back to a
        generation call. Ordered by ``inserted_at`` ascending.
        """
        from opendaisugi.refinement import RefinementRecord
        con = self._con
        rows = con.execute(
            "SELECT record_json FROM refinement_log "
            "WHERE cache_key = ? ORDER BY inserted_at ASC",
            (cache_key,),
        ).fetchall()
        return [RefinementRecord.model_validate_json(row[0]) for row in rows]

    def log_run(
        self,
        session: "RunSession",
        *,
        task: str,
        envelope: Envelope,
        plan: ActionPlan,
        trace_id: str | None = None,
        created_at: str | None = None,
    ) -> str:
        """Append a RunSession trace. Returns the trace id."""
        from dataclasses import asdict

        if created_at is None:
            created_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        if trace_id is None:
            date_prefix = created_at[:10]
            trace_id = f"{date_prefix}-{uuid4().hex[:8]}"

        if not _TRACE_ID_RE.match(trace_id):
            raise ValueError(
                f"Invalid trace_id {trace_id!r}: must contain only "
                f"alphanumeric characters, hyphens, underscores, and dots"
            )

        session_dict = asdict(session)
        session_dict["status"] = session.status.value
        session_dict["verification"] = session.verification.model_dump(mode="json")

        payload = {
            "id": trace_id,
            "created_at": created_at,
            "task": task,
            "envelope": envelope.model_dump(mode="json"),
            "plan": plan.model_dump(mode="json"),
            "result": session.verification.model_dump(mode="json"),
            "run": session_dict,
        }

        failed_step_id = next(
            (s.step_id for s in session.steps if s.status == "failed"),
            None,
        )
        total_duration_ms = sum(s.duration_ms for s in session.steps)

        yaml_path = self._traces_dir / f"{trace_id}.yaml"

        # Write YAML before SQLite — see Journal.log() for the rationale.
        yaml_path.write_text(yaml.safe_dump(payload, sort_keys=False))
        try:
            from opendaisugi.distiller import plan_structure_signature
            structure_signature = plan_structure_signature(plan)
        except Exception:
            structure_signature = None
        try:
            con = self._con
            con.execute(
                "INSERT INTO traces "
                "(id, created_at, task, plan_id, envelope_id, ok, duration_ms, "
                " violations_json, run_id, run_status, failed_step_id, "
                " total_duration_ms, structure_signature) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    trace_id,
                    created_at,
                    task,
                    plan.id,
                    envelope.id,
                    1 if session.verification.ok else 0,
                    session.verification.duration_ms,
                    json.dumps([v.model_dump(mode="json") for v in session.verification.violations]),
                    session.id,
                    session.status.value,
                    failed_step_id,
                    total_duration_ms,
                    structure_signature,
                ),
            )
        except Exception:
            try:
                yaml_path.unlink()
            except OSError:
                pass
            raise

        return trace_id

    def append_receipt(self, receipt: "Receipt") -> None:
        """Append a per-step Receipt for a run. Idempotent on (run_id, step_id).

        v0.18+: evidence-of-step-execution feeding the run-end integrity check
        and the Gardener's selection signal. Content-addressed hash lets
        downstream consumers verify evidence hasn't been tampered with.
        """
        con = self._con
        con.execute(
            "INSERT OR REPLACE INTO receipts "
            "(run_id, step_id, timestamp, evidence_hash, verify_result, "
            "verify_details, evidence_json, model_id) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (
                receipt.run_id, receipt.step_id, receipt.timestamp,
                receipt.evidence_hash, int(receipt.verify_result),
                receipt.verify_details,
                json.dumps(receipt.evidence, default=str),
                receipt.model_id,
            ),
        )

    def receipts_for_run(self, run_id: str) -> "list[Receipt]":
        """Return all receipts for ``run_id``, ordered by timestamp ascending."""
        from opendaisugi.models import Receipt
        con = self._con
        rows = con.execute(
            "SELECT step_id, run_id, timestamp, evidence_json, evidence_hash, "
            "verify_result, verify_details, model_id FROM receipts WHERE run_id = ? "
            "ORDER BY timestamp ASC",
            (run_id,),
        ).fetchall()
        return [
            Receipt(
                step_id=r[0], run_id=r[1], timestamp=r[2],
                evidence=json.loads(r[3]), evidence_hash=r[4],
                verify_result=bool(r[5]), verify_details=r[6],
                model_id=r[7],
            )
            for r in rows
        ]

    def load_run(self, trace_id: str) -> "RunSession":
        """Reconstruct a RunSession from its stored YAML body."""
        from opendaisugi.run_session import RunSession, RunStatus, StepOutcome

        yaml_path = self._traces_dir / f"{trace_id}.yaml"
        if not yaml_path.exists():
            raise FileNotFoundError(
                f"No run trace with id {trace_id!r} at {yaml_path}"
            )
        raw = yaml.safe_load(yaml_path.read_text())
        run_data = raw.get("run")
        if run_data is None:
            raise ValueError(
                f"Trace {trace_id!r} was not written by log_run() "
                f"(missing 'run' section — use load_trace() for legacy entries)"
            )
        verification = VerificationResult(**run_data["verification"])
        steps = [StepOutcome(**s) for s in run_data["steps"]]
        return RunSession(
            id=run_data["id"],
            envelope_id=run_data["envelope_id"],
            plan_id=run_data["plan_id"],
            status=RunStatus(run_data["status"]),
            verification=verification,
            steps=steps,
            started_at=run_data["started_at"],
            ended_at=run_data["ended_at"],
            trace_id=run_data["trace_id"],
        )
