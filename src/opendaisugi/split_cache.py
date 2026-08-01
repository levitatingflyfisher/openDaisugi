"""SQLite-backed cache for LLM episode-split boundaries.

The transcript parser asks an LLM to split an oversized episode into sub-tasks
(``ClaudeCodeParser._llm_split``). That call is the parser's only per-episode
model cost, and it was recomputed on every ``daisugi onboard`` run — so
re-onboarding the same history paid for the same splits again. This cache makes
the split content-addressable: the same (model, prompt, episode) returns the
stored boundaries with no model call.

Mirrors ``envelope_cache``: key = sha256 of (split-prompt version, model, the
exact content sent to the splitter). Prompt-version-aware — rows from an older
split prompt are evicted at construction. Advisory: ``put`` never raises;
``get`` returns ``None`` on miss.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import sqlite3
import time
from pathlib import Path

_log = logging.getLogger("opendaisugi.split_cache")

_SCHEMA = """
CREATE TABLE IF NOT EXISTS split_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    boundaries_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_split_prompt_version ON split_cache(prompt_version);
"""


def make_split_key(*, prompt_version: str, model: str, content: str) -> str:
    """sha256 of the full determinant of one split call. No normalization."""
    payload = f"version:{prompt_version}\nmodel:{model}\ncontent:{content}"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


class SplitCache:
    """SQLite-backed cache of LLM split boundaries, keyed by call signature."""

    def __init__(self, db_path: str | os.PathLike, *, prompt_version: str) -> None:
        self._db_path = Path(db_path)
        self._db_path.parent.mkdir(parents=True, exist_ok=True)
        self._prompt_version = prompt_version
        self._evicted_on_init = 0
        with sqlite3.connect(self._db_path) as con:
            con.executescript(_SCHEMA)
            cur = con.execute(
                "DELETE FROM split_cache WHERE prompt_version != ?",
                (self._prompt_version,),
            )
            self._evicted_on_init = cur.rowcount
        if self._evicted_on_init:
            _log.info("split_cache: evicted %d stale entries", self._evicted_on_init)

    def get(self, *, model: str, content: str) -> list | None:
        """Return the cached boundaries for this call, or ``None`` on miss.

        A cached *empty* list (a legitimate "no split needed" answer) is a hit,
        not a miss — the whole point is to avoid re-calling the model for it.
        """
        key = make_split_key(prompt_version=self._prompt_version, model=model, content=content)
        with sqlite3.connect(self._db_path) as con:
            row = con.execute(
                "SELECT boundaries_json FROM split_cache WHERE cache_key = ?", (key,)
            ).fetchone()
        if row is None:
            return None
        return json.loads(row[0])

    def put(self, boundaries: list, *, model: str, content: str) -> None:
        """Store ``boundaries`` for this call. Best-effort — never raises."""
        key = make_split_key(prompt_version=self._prompt_version, model=model, content=content)
        try:
            with sqlite3.connect(self._db_path) as con:
                con.execute(
                    "INSERT OR REPLACE INTO split_cache "
                    "(cache_key, prompt_version, boundaries_json, inserted_at) "
                    "VALUES (?, ?, ?, ?)",
                    (key, self._prompt_version, json.dumps(boundaries), time.time()),
                )
        except sqlite3.Error as exc:
            _log.warning("split_cache put failed: %s", exc)

    def stats(self) -> dict[str, int]:
        with sqlite3.connect(self._db_path) as con:
            row = con.execute("SELECT COUNT(*) FROM split_cache").fetchone()
        return {"entries": int(row[0]) if row else 0, "evicted_on_init": self._evicted_on_init}
