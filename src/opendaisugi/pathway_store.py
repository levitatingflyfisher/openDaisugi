"""SQLite-backed storage for compiled pathways (v0.3.0).

Mirrors the envelope_cache pattern: a single table, Pydantic ser/deser
via model_dump_json/model_validate_json. Embeddings are serialized as
JSON-encoded float lists; similarity search loads all rows into memory
(cluster count is expected to be small, dozens to low hundreds).
"""

from __future__ import annotations

import json
import logging
import sqlite3
import time
import warnings
from pathlib import Path
from typing import TYPE_CHECKING

from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.pathway import CompiledPathway, PathwayMatch

if TYPE_CHECKING:
    pass

_log = logging.getLogger("opendaisugi.pathway_store")

# Calibrated retrieval/clustering threshold for the shipped all-MiniLM-L6-v2
# embedder. On that model, same-task paraphrases score cosine ~0.5 (mean) while
# *different* tasks max out ~0.29 — so the historical 0.85 default retrieved
# nothing but near-verbatim restatements (the documented token-savings
# "value-killer"). 0.55 sits in the safe band: it catches the real paraphrase
# range while staying well above the different-task ceiling, so unrelated tasks
# never false-merge. Overridable per call and per ``Daisugi(pathway_threshold=)``.
DEFAULT_PATHWAY_THRESHOLD = 0.55

_search_extra_warned = False
_stale_embeddings_warned = False


def _warn_stale_embeddings_once(stale_ratio: float, stale_count: int) -> None:
    """Emit the stale-embeddings warning at most once per process. v0.28.4."""
    global _stale_embeddings_warned
    if _stale_embeddings_warned:
        return
    _stale_embeddings_warned = True
    msg = (
        f"{stale_count} pathway(s) ({stale_ratio:.0%} of store) were embedded "
        f"under a different model/version than the current distiller. They are "
        f"excluded from find() — semantic comparison across embedding spaces "
        f"would return wrong matches. Run `daisugi tend` to re-embed."
    )
    warnings.warn(msg, UserWarning, stacklevel=4)


def _warn_search_extra_missing_once() -> None:
    """Emit the [search]-extra missing warning at most once per process.

    Uses warnings.warn (UserWarning) so it surfaces without any logging
    configuration — the install hint reaches users who haven't set up
    logging handlers.
    """
    global _search_extra_warned
    if _search_extra_warned:
        return
    _search_extra_warned = True
    msg = (
        "opendaisugi[search] is not installed; pathway lookup is disabled "
        "and token savings via the pathway store will not work. "
        "Install with: uv add 'opendaisugi[search]'  (or: pip install 'opendaisugi[search]')"
    )
    warnings.warn(msg, UserWarning, stacklevel=4)
    _log.warning(msg)

_SCHEMA = """
CREATE TABLE IF NOT EXISTS pathways (
    id TEXT PRIMARY KEY,
    task_description TEXT NOT NULL,
    task_embedding_json TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    plan_template_json TEXT NOT NULL,
    source_trace_ids_json TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    hit_count INTEGER NOT NULL DEFAULT 0,
    distilled_at REAL NOT NULL,
    embedding_model TEXT NOT NULL DEFAULT '',
    embedding_model_version TEXT NOT NULL DEFAULT '',
    last_activation_at REAL NOT NULL DEFAULT 0.0,
    failure_count INTEGER NOT NULL DEFAULT 0
);
"""

# Columns added post-v0.3.0. Each tuple: (column_name, full ALTER clause).
# Kept here rather than wiring a full migration framework — the table is
# single-writer and low-row, so one-liner additive migrations are enough.
_ADDITIVE_COLUMNS = (
    ("embedding_model", "ALTER TABLE pathways ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''"),
    ("embedding_model_version", "ALTER TABLE pathways ADD COLUMN embedding_model_version TEXT NOT NULL DEFAULT ''"),
    ("last_activation_at", "ALTER TABLE pathways ADD COLUMN last_activation_at REAL NOT NULL DEFAULT 0.0"),
    ("failure_count", "ALTER TABLE pathways ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0"),
)

# Columns removed in v0.5.1 — dropped best-effort on open so legacy DBs
# shed the stale NOT NULL columns rather than fail INSERT. Unknown-column
# names are silently ignored; SQLite 3.35+ supports DROP COLUMN.
_DROPPED_COLUMNS = ("pitfalls_json", "validation_score")


class PathwayStore:
    """SQLite-backed store for CompiledPathway objects."""

    def __init__(self, db_path: str | Path) -> None:
        # ``:memory:`` is a sentinel for an in-process, non-persistent store
        # (used by ``daisugi tend --dry-run``). It requires a persistent
        # connection because a fresh ``sqlite3.connect(":memory:")`` opens
        # a brand-new empty database every time.
        if str(db_path) == ":memory:":
            self._db_path = Path(":memory:")
            # check_same_thread=False because ``Daisugi.find_pathway`` offloads
            # the sync find() call via ``asyncio.to_thread``. The connection
            # is never shared across instances — each PathwayStore gets its
            # own — so cross-thread use is safe.
            self._shared_con: sqlite3.Connection | None = sqlite3.connect(
                ":memory:", check_same_thread=False,
            )
        else:
            self._db_path = Path(db_path)
            self._db_path.parent.mkdir(parents=True, exist_ok=True)
            self._shared_con = None

        with self._connect() as con:
            con.executescript(_SCHEMA)
            existing = {r[1] for r in con.execute("PRAGMA table_info(pathways)")}
            for col, ddl in _ADDITIVE_COLUMNS:
                if col not in existing:
                    con.execute(ddl)
            for col in _DROPPED_COLUMNS:
                if col in existing:
                    try:
                        con.execute(f"ALTER TABLE pathways DROP COLUMN {col}")
                    except sqlite3.OperationalError:
                        pass

    def _connect(self) -> sqlite3.Connection:
        """Return a connection. Shared for :memory:, fresh otherwise.

        ``sqlite3.Connection.__exit__`` only commits/rolls back — it does
        not close — so the shared in-memory connection survives repeated
        ``with self._connect() as con:`` blocks.
        """
        if self._shared_con is not None:
            return self._shared_con
        return sqlite3.connect(self._db_path)

    def put(self, pathway: CompiledPathway) -> None:
        """Insert or replace a pathway."""
        with self._connect() as con:
            con.execute(
                "INSERT OR REPLACE INTO pathways "
                "(id, task_description, task_embedding_json, envelope_json, "
                "plan_template_json, source_trace_ids_json, "
                "version, hit_count, distilled_at, "
                "embedding_model, embedding_model_version, "
                "last_activation_at, failure_count) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    pathway.id,
                    pathway.task_description,
                    json.dumps(pathway.task_embedding),
                    pathway.envelope.model_dump_json(),
                    pathway.plan_template.model_dump_json(),
                    json.dumps(pathway.source_trace_ids),
                    pathway.version,
                    pathway.hit_count,
                    pathway.distilled_at,
                    pathway.embedding_model,
                    pathway.embedding_model_version,
                    pathway.last_activation_at,
                    pathway.failure_count,
                ),
            )

    def find(
        self, task: str, *, threshold: float = DEFAULT_PATHWAY_THRESHOLD
    ) -> PathwayMatch | None:
        """Embed ``task`` and return the best matching pathway above threshold.

        Returns None when the store is empty, the embedder is unavailable
        (the ``[search]`` extra is not installed), or the best match is
        below threshold. Short-circuits before importing sentence-transformers
        when the pathway table is empty.

        v0.28.4: rows whose ``(embedding_model, embedding_model_version)``
        does not match the current distiller are filtered out before
        cosine similarity — pre-v0.28.4, vectors from older embedder
        runs were compared against current-model query vectors in
        incompatible spaces, silently surfacing semantically wrong
        matches. When ≥ 10% of the store is filtered out, a one-shot
        warning instructs the operator to run ``daisugi tend`` to
        re-embed.
        """
        rows = self._load_all_rows()
        if not rows:
            return None

        try:
            query_vec = self._embed_query(task)
        except ImportError:
            _warn_search_extra_missing_once()
            return None

        from opendaisugi._search import _MODEL_NAME
        from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

        current_model = _MODEL_NAME
        current_version = _EMBEDDING_MODEL_VERSION
        # Rows with empty embedding_model are pre-provenance-tracking
        # legacy rows. Admit them as wildcards rather than orphaning every
        # pre-v0.5 store. Rows with a SET but non-matching model/version
        # are concretely stale and excluded — that's the (a)
        # cross-model contamination the M3 fix targets.
        def _is_compatible(r) -> bool:
            row_model = r["embedding_model"]
            row_version = r["embedding_model_version"]
            if not row_model and not row_version:
                return True  # legacy wildcard
            return row_model == current_model and row_version == current_version

        compatible = [r for r in rows if _is_compatible(r)]
        if not compatible:
            return None
        stale_ratio = 1.0 - (len(compatible) / len(rows))
        if stale_ratio >= 0.10:
            _warn_stale_embeddings_once(stale_ratio, len(rows) - len(compatible))

        import numpy as np

        from opendaisugi._similarity import cosine_similarity_batch

        task_vecs = np.array([json.loads(r["task_embedding_json"]) for r in compatible])
        scores = cosine_similarity_batch(query_vec, task_vecs)

        best_idx = int(np.argmax(scores))
        best_score = float(scores[best_idx])
        if best_score < threshold:
            return None

        pathway = self._row_to_pathway(compatible[best_idx])
        return PathwayMatch(pathway=pathway, similarity=best_score)

    def _embed_query(self, task: str):
        """Embed a task description. Overridable in tests.

        Raises ``ImportError`` if the ``[search]`` extra is not installed —
        ``find()`` catches this and returns None so callers see graceful
        degradation rather than a crash.
        """
        from opendaisugi._search import _get_model
        return _get_model().encode([task], convert_to_numpy=True)[0]

    def _load_all_rows(self) -> list[dict]:
        with self._connect() as con:
            con.row_factory = sqlite3.Row
            return [dict(r) for r in con.execute("SELECT * FROM pathways")]

    def _row_to_pathway(self, row: dict) -> CompiledPathway:
        return CompiledPathway(
            id=row["id"],
            task_description=row["task_description"],
            task_embedding=json.loads(row["task_embedding_json"]),
            embedding_model=row["embedding_model"],
            embedding_model_version=row["embedding_model_version"],
            envelope=Envelope.model_validate_json(row["envelope_json"]),
            plan_template=ActionPlan.model_validate_json(row["plan_template_json"]),
            source_trace_ids=json.loads(row["source_trace_ids_json"]),
            version=row["version"],
            hit_count=row["hit_count"],
            distilled_at=row["distilled_at"],
            last_activation_at=row["last_activation_at"] if "last_activation_at" in row.keys() else 0.0,
            failure_count=row["failure_count"] if "failure_count" in row.keys() else 0,
        )

    def increment_hit(self, pathway_id: str) -> None:
        """Bump hit_count and stamp last_activation_at. No-op if pathway doesn't exist."""
        with self._connect() as con:
            con.execute(
                "UPDATE pathways SET hit_count = hit_count + 1, "
                "last_activation_at = ? WHERE id = ?",
                (time.time(), pathway_id),
            )

    def mark_failure(self, pathway_id: str) -> None:
        """Bump failure_count and stamp last_activation_at.

        Called by the A/B harness or regression pass. No-op if pathway
        doesn't exist. v0.28.4: stamps ``last_activation_at`` to match
        ``increment_hit`` — pre-v0.28.4 a failure-only pathway had
        ``last_activation_at=0.0`` and the gardener's stale-check
        short-circuited on the falsy value, so failure-only pathways
        were never pruned for idleness.
        """
        with self._connect() as con:
            con.execute(
                "UPDATE pathways SET failure_count = failure_count + 1, "
                "last_activation_at = ? WHERE id = ?",
                (time.time(), pathway_id),
            )

    def list_all(self) -> list[CompiledPathway]:
        """Return all pathways. Used by CLI reporting."""
        return [self._row_to_pathway(r) for r in self._load_all_rows()]

    def delete(self, pathway_id: str) -> bool:
        """Remove a pathway. Returns True if it existed."""
        with self._connect() as con:
            cur = con.execute("DELETE FROM pathways WHERE id = ?", (pathway_id,))
            return cur.rowcount > 0

    def stats(self) -> dict:
        """Return counts and total hits."""
        with self._connect() as con:
            row = con.execute(
                "SELECT COUNT(*), SUM(hit_count) FROM pathways"
            ).fetchone()
        return {
            "count": row[0] or 0,
            "total_hits": row[1] or 0,
        }
