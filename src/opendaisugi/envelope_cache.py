"""SQLite-backed envelope cache (v0.1.2).

Content-addressable: key = sha256 of (task, context, model, parent_envelope_id,
summarize). Prompt-version-aware: rows whose ``prompt_version`` doesn't match
the current constant are dropped at construction time, keeping the read path
free of version filters.

Cache is advisory — ``put()`` failures are logged but never raise to the
caller. ``get()`` failures raise (corrupt cache is a real bug worth surfacing).
"""

from __future__ import annotations

import hashlib
import logging
import os
import sqlite3
import time
from pathlib import Path

from opendaisugi.models import Envelope

_log = logging.getLogger("opendaisugi.envelope_cache")


_SCHEMA = """
CREATE TABLE IF NOT EXISTS envelope_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_version ON envelope_cache(prompt_version);
"""


def make_cache_key(
    *,
    task: str,
    context: str | None,
    model: str,
    parent_envelope_id: str | None,
    summarize: bool,
    thinking_budget: str = "standard",
    tier1_provider_name: str | None = None,
) -> str:
    """Compute the sha256 cache key for a generator call signature.

    Public since v0.2.1 — ``generate_envelope()`` stamps the key on the
    returned envelope so Supervisor can tag refinement records.

    Since v0.4.0, ``tier1_provider_name`` participates in the key so a
    rerun through a different Tier-1 adapter doesn't silently reuse stale
    state. ``None`` means "Tier-2 generated" and preserves v0.3.x keys.

    No normalization — ``"foo "`` and ``"foo"`` get different keys. Avoiding
    false positive hits matters more than maximizing hit rate.
    """
    payload_lines = [
        f"task:{task}",
        f"context:{context or ''}",
        f"model:{model}",
        f"parent:{parent_envelope_id or ''}",
        f"summarize:{int(summarize)}",
        f"thinking:{thinking_budget}",
    ]
    # Only append when set, so v0.3.x callers get byte-identical keys.
    if tier1_provider_name is not None:
        payload_lines.append(f"tier1:{tier1_provider_name}")
    return hashlib.sha256("\n".join(payload_lines).encode("utf-8")).hexdigest()


class EnvelopeCache:
    """SQLite-backed envelope cache.

    Construct with a path and the current ``prompt_version``. Rows whose
    stored ``prompt_version`` doesn't match are evicted at construction time;
    the eviction count is exposed via ``stats()`` for one run.

    Instances are cheap: ``__init__`` creates the parent dir and schema then
    performs the one-shot eviction. Each method opens its own SQLite
    connection via ``with sqlite3.connect(...)`` (matches ``journal.py``).
    """

    def __init__(
        self,
        db_path: str | os.PathLike,
        *,
        prompt_version: str,
    ) -> None:
        self._db_path = Path(db_path)
        self._db_path.parent.mkdir(parents=True, exist_ok=True)
        self._prompt_version = prompt_version
        self._evicted_on_init = 0
        self._init_schema_and_evict()

    def _init_schema_and_evict(self) -> None:
        with sqlite3.connect(self._db_path) as con:
            con.executescript(_SCHEMA)
            cur = con.execute(
                "DELETE FROM envelope_cache WHERE prompt_version != ?",
                (self._prompt_version,),
            )
            self._evicted_on_init = cur.rowcount
        if self._evicted_on_init > 0:
            _log.info(
                "envelope_cache: evicted %d stale entries (prompt_version=%r)",
                self._evicted_on_init, self._prompt_version,
            )

    def get(
        self,
        *,
        task: str,
        context: str | None,
        model: str,
        parent_envelope_id: str | None,
        summarize: bool,
        thinking_budget: str = "standard",
        tier1_provider_name: str | None = None,
    ) -> Envelope | None:
        """Return cached envelope for these inputs, or ``None`` on miss."""
        key = make_cache_key(
            task=task, context=context, model=model,
            parent_envelope_id=parent_envelope_id, summarize=summarize,
            thinking_budget=thinking_budget,
            tier1_provider_name=tier1_provider_name,
        )
        with sqlite3.connect(self._db_path) as con:
            row = con.execute(
                "SELECT envelope_json FROM envelope_cache WHERE cache_key = ?",
                (key,),
            ).fetchone()
        if row is None:
            return None
        return Envelope.model_validate_json(row[0])

    def put(
        self,
        envelope: Envelope,
        *,
        task: str,
        context: str | None,
        model: str,
        parent_envelope_id: str | None,
        summarize: bool,
        thinking_budget: str = "standard",
        tier1_provider_name: str | None = None,
    ) -> None:
        """Store ``envelope`` under the computed key.

        Best-effort: SQLite errors are logged at WARNING but never raised.
        The cache is advisory — a failed write must not break the caller.
        """
        key = make_cache_key(
            task=task, context=context, model=model,
            parent_envelope_id=parent_envelope_id, summarize=summarize,
            thinking_budget=thinking_budget,
            tier1_provider_name=tier1_provider_name,
        )
        try:
            with sqlite3.connect(self._db_path) as con:
                con.execute(
                    "INSERT OR REPLACE INTO envelope_cache "
                    "(cache_key, prompt_version, envelope_json, inserted_at) "
                    "VALUES (?, ?, ?, ?)",
                    (
                        key,
                        self._prompt_version,
                        envelope.model_dump_json(),
                        time.time(),
                    ),
                )
        except sqlite3.Error as exc:
            _log.warning("envelope_cache put failed: %s", exc)

    def get_inserted_at(self, cache_key: str) -> float | None:
        """Return the insertion timestamp of a cached envelope, or None if missing."""
        with sqlite3.connect(self._db_path) as con:
            row = con.execute(
                "SELECT inserted_at FROM envelope_cache WHERE cache_key = ?",
                (cache_key,),
            ).fetchone()
        if row is None:
            return None
        return float(row[0])

    def invalidate(self, cache_key: str) -> bool:
        """Delete a specific cache entry. Returns True if an entry was removed."""
        try:
            with sqlite3.connect(self._db_path) as con:
                cur = con.execute(
                    "DELETE FROM envelope_cache WHERE cache_key = ?",
                    (cache_key,),
                )
                return cur.rowcount > 0
        except sqlite3.Error as exc:
            _log.warning("envelope_cache invalidate failed: %s", exc)
            return False

    def clear(self) -> int:
        """Delete all entries; return the count removed."""
        with sqlite3.connect(self._db_path) as con:
            cur = con.execute("DELETE FROM envelope_cache")
            return cur.rowcount

    def stats(self) -> dict[str, int]:
        """Return ``{'entries': N, 'evicted_on_init': K}`` for diagnostics."""
        with sqlite3.connect(self._db_path) as con:
            row = con.execute(
                "SELECT COUNT(*) FROM envelope_cache"
            ).fetchone()
        return {
            "entries": int(row[0]) if row else 0,
            "evicted_on_init": self._evicted_on_init,
        }
