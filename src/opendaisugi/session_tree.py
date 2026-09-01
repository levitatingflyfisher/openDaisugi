"""One append-only JSONL per session: the prompt tree daisugi can navigate.

pi's model, with the head persisted: every entry carries ``type``, ``id`` (8 hex
chars), ``parentId``, ``ts``. Line one is the session header. ``head`` lines
move the current leaf and carry no id. Nothing is ever deleted; a fork copies a
path into a new file that names its parent. Readers ignore a torn last line.
"""

from __future__ import annotations

import json
import os
import re
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

SCHEMA_VERSION = 1
ENTRY_TYPES = frozenset(
    {
        "session",
        "prompt",
        "assistant",
        "tool_call",
        "verdict",
        "tool_result",
        "checkpoint",
        "compaction",
        "branch_summary",
        "label",
        "note",
        "head",
        "state",
    }
)
_RESERVED = frozenset({"session", "head"})
# "state" joins _NO_MOVE in the SAME edit as ENTRY_TYPES: a state entry
# must never become the tree's head, or the next tool_call/checkpoint
# mis-parents onto it instead of onto the previous verdict (S3, spec-01).
_NO_MOVE = frozenset({"label", "head", "session", "state"})
_META_KEYS = ("type", "id", "parentId", "ts")
_UNSET = object()  # sentinel: the head cache has not been computed yet


def new_id() -> str:
    return secrets.token_hex(4)


def _safe_id(raw: object) -> str:
    safe = re.sub(r"[^A-Za-z0-9._-]", "_", str(raw or "")).strip(".")[:128]
    return safe or "no-session"


@dataclass(frozen=True)
class Entry:
    type: str
    id: str | None
    parent_id: str | None
    ts: float
    data: dict[str, Any] = field(default_factory=dict)

    def to_json(self) -> str:
        row: dict[str, Any] = {"type": self.type}
        if self.id is not None:
            row["id"] = self.id
        if self.type not in ("head", "session"):
            row["parentId"] = self.parent_id
        row["ts"] = self.ts
        # A caller's data dict must never be able to overwrite the store's
        # own invariant fields (e.g. a tool's output happening to contain a
        # literal "id" or "type" key) — the store guards its own shape.
        row.update({k: v for k, v in self.data.items() if k not in _META_KEYS})
        return json.dumps(row, ensure_ascii=False)

    @classmethod
    def from_row(cls, row: dict[str, Any]) -> "Entry":
        data = {k: v for k, v in row.items() if k not in _META_KEYS}
        return cls(
            type=str(row["type"]),
            id=row.get("id"),
            parent_id=row.get("parentId"),
            ts=float(row.get("ts") or 0.0),
            data=data,
        )


class SessionTree:
    """An append-only session file plus an in-memory head cache.

    ``append`` is on the gate's blocking path, so it must not re-parse the
    whole file on every call (S6). We cache the head in this instance and
    update it incrementally on every write; a fresh reader (e.g. ``open()``
    from another process) rebuilds it once by scanning the file, which is
    fine because that happens off the hot path. This assumes a single writer
    per session id — concurrent writers to the same file (e.g. two processes
    racing on the same session) are not coordinated here; ``O_APPEND`` keeps
    individual small lines atomic but two writers could each cache a stale
    head and both append against it, producing two children of the same
    parent rather than a corrupted file. That is an acceptable divergence
    (a fork, not data loss) for now.
    """

    def __init__(self, path: Path) -> None:
        self.path = path
        self._head_cache: str | None | object = _UNSET

    @property
    def session_id(self) -> str:
        return self.path.stem

    # --- construction -------------------------------------------------------
    @classmethod
    def create(
        cls,
        sessions_dir: Path,
        *,
        session_id: str,
        harness: str,
        cwd: str,
        harness_session_id: str | None = None,
        transcript_path: str | None = None,
        parent_session: str | None = None,
        parent_entry: str | None = None,
        cache_key: str | None = None,
        clock: Callable[[], float] = time.time,
    ) -> "SessionTree":
        sessions_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        try:
            os.chmod(sessions_dir, 0o700)
        except OSError:
            pass
        sid = _safe_id(session_id)
        path = sessions_dir / f"{sid}.jsonl"
        if path.exists():
            raise FileExistsError(path)
        header = Entry(
            "session",
            sid,
            None,
            clock(),
            {
                "v": SCHEMA_VERSION,
                "harness": harness,
                "cwd": cwd,
                "harnessSessionId": harness_session_id,
                "transcriptPath": transcript_path,
                "parentSession": parent_session,
                "parentEntry": parent_entry,
                "cacheKey": cache_key,
            },
        )
        tree = cls(path)
        tree._write(header)
        tree._head_cache = None  # a freshly created tree has no leaf yet
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
        return tree

    @classmethod
    def open(cls, sessions_dir: Path, session_id: str) -> "SessionTree":
        path = sessions_dir / f"{_safe_id(session_id)}.jsonl"
        if not path.exists():
            raise FileNotFoundError(path)
        return cls(path)

    @classmethod
    def open_or_create(cls, sessions_dir: Path, *, session_id: str, **header: Any) -> "SessionTree":
        """Open the session, creating it first if it does not exist yet.

        Racy by construction (check-then-act across ``open``/``create``):
        two callers can both miss on ``open`` and both call ``create``. The
        loser's ``create`` raises ``FileExistsError`` (the file the winner
        just made) — that is caught here and falls back to ``open`` so the
        race produces one tree, not a lost write or an unhandled exception.
        """
        try:
            return cls.open(sessions_dir, session_id)
        except FileNotFoundError:
            pass
        try:
            return cls.create(sessions_dir, session_id=session_id, **header)
        except FileExistsError:
            return cls.open(sessions_dir, session_id)

    # --- io -----------------------------------------------------------------
    def _write(self, entry: Entry) -> None:
        with self.path.open("a", encoding="utf-8") as f:
            f.write(entry.to_json() + "\n")

    def entries(self) -> list[Entry]:
        out: list[Entry] = []
        try:
            text = self.path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return out
        for line in text.splitlines():
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue  # a torn line from a writer mid-append
            if isinstance(row, dict) and row.get("type") in ENTRY_TYPES:
                out.append(Entry.from_row(row))
        return out

    def meta(self) -> dict[str, Any]:
        for e in self.entries():
            if e.type == "session":
                return {"id": e.id, **e.data}
        return {}

    # --- tree ---------------------------------------------------------------
    def _compute_head(self) -> str | None:
        """Find the current head without decoding the whole file.

        Forward reading, "last write wins": the head is whichever comes
        *last* in the file among (a) a ``head`` entry's ``leafId`` and (b) a
        non-``_NO_MOVE`` entry's own id. That is exactly what scanning from
        the end and returning on the first such line finds — same answer,
        but the common case (the gate opens a fresh tree, asks for ``head()``
        to parent the next append, and the just-appended entry usually *is*
        the head) costs one line, not the whole file. A torn/undecodable
        line is skipped, same as the forward reader.
        """
        try:
            text = self.path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return None
        for line in reversed(text.splitlines()):
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue  # a torn line from a writer mid-append
            if not isinstance(row, dict) or row.get("type") not in ENTRY_TYPES:
                continue
            if row["type"] == "head":
                return row.get("leafId")
            if row["type"] not in _NO_MOVE and row.get("id"):
                return row["id"]
        return None

    def head(self) -> str | None:
        """The current leaf id, or ``None`` for an empty tree.

        Cached in memory and updated incrementally by ``append``/``set_head``
        so a long-lived instance never recomputes; a cold instance (the
        gate's usual pattern — ``open_or_create`` then one ``append``) still
        must not re-parse the whole file (S6), so ``_compute_head`` itself
        scans backward with an early return instead of decoding every line.
        Single-writer-per-session-id is assumed: two writers racing the same
        file could each compute a head from a stale tail and append against
        it, giving two children of the same parent (a fork, not corruption).
        """
        if self._head_cache is _UNSET:
            self._head_cache = self._compute_head()
        return self._head_cache  # type: ignore[return-value]

    def append(
        self,
        type: str,
        data: dict[str, Any],
        *,
        parent_id: str | None = "head",
        clock: Callable[[], float] = time.time,
    ) -> Entry:
        if type not in ENTRY_TYPES or type in _RESERVED:
            raise ValueError(f"cannot append entry type {type!r}")
        parent = self.head() if parent_id == "head" else parent_id
        entry = Entry(type, new_id(), parent, clock(), dict(data))
        self._write(entry)
        if type not in _NO_MOVE:
            self._head_cache = entry.id
        return entry

    def set_head(self, entry_id: str, *, clock: Callable[[], float] = time.time) -> None:
        self.path_to(entry_id)  # KeyError if unknown
        self._write(Entry("head", None, None, clock(), {"leafId": entry_id}))
        self._head_cache = entry_id

    def _by_id(self) -> dict[str, Entry]:
        return {e.id: e for e in self.entries() if e.id and e.type != "session"}

    def path_to(self, entry_id: str) -> list[Entry]:
        by_id = self._by_id()
        if entry_id not in by_id:
            raise KeyError(entry_id)
        out: list[Entry] = []
        cur: str | None = entry_id
        seen: set[str] = set()
        while cur is not None and cur not in seen:
            seen.add(cur)
            e = by_id.get(cur)
            if e is None:
                break
            out.append(e)
            cur = e.parent_id
        out.reverse()
        return out

    def children(self, entry_id: str | None) -> list[Entry]:
        return [
            e for e in self.entries() if e.id and e.type not in _NO_MOVE and e.parent_id == entry_id
        ]

    def fork(
        self,
        at_entry_id: str,
        *,
        new_session_id: str | None = None,
        clock: Callable[[], float] = time.time,
    ) -> "SessionTree":
        """Copy the path root→``at_entry_id`` into a new session that names this one.

        Ids are kept, so the copied path is byte-for-byte the same tree; the
        child's head is ``at_entry_id``; ``cacheKey`` is inherited (forks share
        the cached prefix).
        """
        path = self.path_to(at_entry_id)
        meta = self.meta()
        child = SessionTree.create(
            self.path.parent,
            session_id=new_session_id or f"{self.session_id}-{new_id()}",
            harness=str(meta.get("harness", "")),
            cwd=str(meta.get("cwd", "")),
            harness_session_id=meta.get("harnessSessionId"),
            transcript_path=meta.get("transcriptPath"),
            parent_session=self.session_id,
            parent_entry=at_entry_id,
            cache_key=meta.get("cacheKey"),
            clock=clock,
        )
        for e in path:
            child._write(e)
        child._head_cache = at_entry_id
        return child


@dataclass(frozen=True)
class SessionSummary:
    session_id: str
    harness: str
    cwd: str
    harness_session_id: str | None
    transcript_path: str | None
    parent_session: str | None
    last_ts: float
    entry_count: int
    last_tool_call: dict[str, Any] | None
    last_verdict: dict[str, Any] | None


class SessionIndex:
    """The roster: one summary per session file, newest first. Read-only."""

    def __init__(self, sessions_dir: Path) -> None:
        self.dir = sessions_dir

    def mtime(self) -> float:
        if not self.dir.exists():
            return 0.0
        return max((p.stat().st_mtime for p in self.dir.glob("*.jsonl")), default=0.0)

    def list(self) -> list[SessionSummary]:
        out: list[SessionSummary] = []
        if not self.dir.exists():
            return out
        for path in sorted(self.dir.glob("*.jsonl")):
            tree = SessionTree(path)
            entries = tree.entries()
            if not entries or entries[0].type != "session":
                continue
            meta = {"id": entries[0].id, **entries[0].data}
            last_call = next((e for e in reversed(entries) if e.type == "tool_call"), None)
            last_verdict = next((e for e in reversed(entries) if e.type == "verdict"), None)
            # 'state' entries are bookkeeping, not activity: cockpit.py turns
            # last_ts into the WORKING/PARKED/DONE grouping, so a session that
            # reports idle/done via a hook must not look freshly WORKING just
            # because a state row landed after the real activity. `or entries`
            # is the fallback for an all-state entry list — unreachable today,
            # since the guard above already requires entries[0].type ==
            # "session", but kept explicit rather than relying on that
            # invariant never changing.
            live_entries = [e for e in entries if e.type != "state"] or entries
            out.append(
                SessionSummary(
                    session_id=str(meta["id"]),
                    harness=str(meta.get("harness", "")),
                    cwd=str(meta.get("cwd", "")),
                    harness_session_id=meta.get("harnessSessionId"),
                    transcript_path=meta.get("transcriptPath"),
                    parent_session=meta.get("parentSession"),
                    last_ts=max(e.ts for e in live_entries),
                    entry_count=len(live_entries),
                    last_tool_call=(dict(last_call.data, id=last_call.id) if last_call else None),
                    last_verdict=(
                        dict(last_verdict.data, id=last_verdict.id) if last_verdict else None
                    ),
                )
            )
        out.sort(key=lambda s: s.last_ts, reverse=True)
        return out
