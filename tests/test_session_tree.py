"""The session tree: append-only, id/parentId on every entry, head persisted."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.session_tree import (
    ENTRY_TYPES,
    Entry,
    SessionIndex,
    SessionSummary,
    SessionTree,
    new_id,
)


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
    assert SessionTree.open_or_create(tmp_path / "s", session_id="s2", harness="y", cwd="/").meta()["harness"] == "x"


def test_open_or_create_falls_back_on_concurrent_create_race(tmp_path: Path, monkeypatch):
    """Two processes racing open_or_create() on the same fresh session id both
    see FileNotFoundError from open() and both call create(); the loser's
    create() raises FileExistsError, which must fall back to open() instead
    of propagating — losing a tree entry to a race is silent data loss."""
    d = tmp_path / "s"
    real_create = SessionTree.create.__func__

    def racing_create(cls, sessions_dir, *, session_id, **header):
        # Simulate another writer winning the race: it creates the file
        # first, so THIS call's own create() collides.
        real_create(cls, sessions_dir, session_id=session_id, harness="winner", cwd="/racer")
        raise FileExistsError(sessions_dir / f"{session_id}.jsonl")

    monkeypatch.setattr(SessionTree, "create", classmethod(racing_create))
    t = SessionTree.open_or_create(d, session_id="s3", harness="loser", cwd="/x")
    assert t.meta()["harness"] == "winner"


def test_torn_last_line_is_ignored(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    p = t.append("prompt", {"text": "one"})
    with t.path.open("a") as f:
        f.write('{"type":"assistant","id":"abc')  # a writer mid-line
    assert [e.id for e in t.entries() if e.id][1:] == [p.id]
    assert t.head() == p.id
    # A cold instance (no warm cache) must skip the torn line the same way
    # when the backward head scan hits it first.
    assert SessionTree.open(tmp_path / "s", "s1").head() == p.id


def test_cold_head_lookup_does_not_decode_the_whole_file(tmp_path: Path, monkeypatch):
    """S6: the gate opens a fresh SessionTree per call, then asks for head()
    to parent its first append. That must not re-decode every prior line —
    only the tail, since the just-appended entry is almost always the head."""
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="x", cwd="/")
    for i in range(200):
        t.append("tool_call", {"n": i})
    fresh = SessionTree.open(tmp_path / "s", "s1")  # a new instance: cold cache

    calls: list[str] = []
    orig_loads = json.loads

    def _counting(s, *a, **k):
        calls.append(s)
        return orig_loads(s, *a, **k)

    monkeypatch.setattr("opendaisugi.session_tree.json.loads", _counting)
    assert fresh.head() is not None
    assert len(calls) <= 3  # the head is the last line; no full-file scan


def test_entry_types_are_the_spec_set():
    assert ENTRY_TYPES == frozenset(
        {"session", "prompt", "assistant", "tool_call", "verdict", "tool_result",
         "checkpoint", "compaction", "branch_summary", "label", "note", "head"}
    )
    assert Entry("prompt", "a1b2c3d4", None, 1.0, {"text": "x"}).to_json().startswith('{"type": "prompt"')


def test_to_json_data_cannot_shadow_the_meta_keys():
    """A data payload with id/type/parentId/ts keys must never overwrite the
    store's own invariant fields — the store guards its own shape."""
    e = Entry("prompt", "a1b2c3d4", "parent99", 1.0, {
        "id": "evil", "type": "evil", "parentId": "evil", "ts": 999.0, "text": "hi",
    })
    row = json.loads(e.to_json())
    assert row["id"] == "a1b2c3d4"
    assert row["type"] == "prompt"
    assert row["parentId"] == "parent99"
    assert row["ts"] == 1.0
    assert row["text"] == "hi"


def test_fork_copies_the_path_and_names_its_parent(tmp_path: Path):
    t = SessionTree.create(tmp_path / "s", session_id="s1", harness="sprig", cwd="/w", cache_key="k")
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
    # a COLD open() of the forked child (no warm fork-time cache) must
    # compute the same head by scanning the file, not just the fork instance.
    cold = SessionTree.open(tmp_path / "s", child.session_id)
    assert cold.head() == a.id


def test_index_lists_sessions_newest_first(tmp_path: Path):
    d = tmp_path / "s"
    old = SessionTree.create(d, session_id="old", harness="claude-code", cwd="/a", clock=lambda: 10.0)
    old.append("prompt", {"text": "x"}, clock=lambda: 11.0)
    new = SessionTree.create(d, session_id="new", harness="sprig", cwd="/b", clock=lambda: 20.0)
    c = new.append("tool_call", {"name": "Bash", "detail": "ls"}, clock=lambda: 21.0)
    new.append("verdict", {"toolUseId": "t1", "decision": "deny", "clause": "shell: no"}, parent_id=c.id, clock=lambda: 22.0)
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
