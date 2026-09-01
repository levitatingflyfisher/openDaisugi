"""gate_states_reader: the newest gate-sourced state entry per pane, read
straight from the session store, for a pane backend to merge in.

Spec-03's tmux section says gate events for a pane arrive from the session
tree's own `state` entries. This module is the reader that makes those
entries reachable from a pane backend; gate.py is what writes them.
"""

from __future__ import annotations

import json
import time
from pathlib import Path

from opendaisugi.floor import Ask, PaneStateEvent
from opendaisugi.floor.gate_states import gate_states_reader
from opendaisugi.session_tree import SessionTree


def _write_state_entry(sessions_dir: Path, session_id: str, ev: PaneStateEvent) -> None:
    # SessionTree.append() always stamps its own entry with the write-time
    # clock, the same as gate.py's real writes: the session tree owns entry
    # order, not the caller's payload. clock=lambda: ev.ts pins that stamp
    # to the event's own ts, so a test can control ordering deterministically
    # instead of racing real wall-clock time between two fast appends.
    tree = SessionTree.open_or_create(
        sessions_dir,
        session_id=session_id,
        harness=ev.harness,
        cwd="/repo",
    )
    tree.append("state", json.loads(ev.to_json()), clock=lambda: ev.ts)


def test_returns_the_newest_state_entry_per_pane(tmp_path):
    sessions_dir = tmp_path / "sessions"
    older = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="working",
        source="gate",
        ts=1.0,
        pane="p1",
    )
    newer = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=2.0,
        pane="p1",
        ask=Ask(id="a1", tool="Bash", summary="rm -rf", deadline=time.time() + 3600),
    )
    _write_state_entry(sessions_dir, "s1", older)
    _write_state_entry(sessions_dir, "s1", newer)

    read = gate_states_reader(tmp_path)
    result = read()

    assert result["p1"].state == "blocked" and result["p1"].ts == 2.0


def test_reads_across_more_than_one_session_file(tmp_path):
    sessions_dir = tmp_path / "sessions"
    ev1 = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    ev2 = PaneStateEvent(
        session_id="s2", harness="codex", state="idle", source="gate", ts=1.0, pane="p2"
    )
    _write_state_entry(sessions_dir, "s1", ev1)
    _write_state_entry(sessions_dir, "s2", ev2)

    read = gate_states_reader(tmp_path)
    result = read()

    assert set(result) == {"p1", "p2"}
    assert result["p1"].state == "working" and result["p2"].state == "idle"


def test_skips_a_state_entry_with_no_pane(tmp_path):
    sessions_dir = tmp_path / "sessions"
    no_pane = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0
    )
    _write_state_entry(sessions_dir, "s1", no_pane)

    read = gate_states_reader(tmp_path)
    assert read() == {}


def test_skips_a_malformed_state_entry_without_raising(tmp_path):
    sessions_dir = tmp_path / "sessions"
    sessions_dir.mkdir(parents=True)
    tree = SessionTree.open_or_create(
        sessions_dir,
        session_id="s1",
        harness="claude-code",
        cwd="/repo",
    )
    # A state row that is not even shaped like a PaneStateEvent: from_json
    # requires state/source/session_id/harness/ts, all missing here.
    tree.append("state", {"pane": "p1"})

    read = gate_states_reader(tmp_path)
    assert read() == {}


def test_never_raises_when_the_sessions_directory_is_missing(tmp_path):
    read = gate_states_reader(tmp_path / "does-not-exist")
    assert read() == {}


def test_never_raises_when_one_session_file_has_a_non_numeric_ts(tmp_path):
    """SessionTree.entries() calls float(row.get("ts") or 0.0) with no guard
    of its own around that one line, so a row whose ts is not a number
    raises ValueError straight out of entries(). A prior write torn by a
    killed gate process is exactly how a line like this happens. The
    garbage line sits in its own session file, beside a second, unrelated
    file holding one good entry: that file's own read must not be caught
    up in the first file's failure."""
    sessions_dir = tmp_path / "sessions"
    good = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    _write_state_entry(sessions_dir, "s1", good)
    bad_row = {**json.loads(good.to_json()), "ts": "not-a-number", "pane": "p2"}
    (sessions_dir / "s2.jsonl").write_text(
        json.dumps({"type": "state", **bad_row}) + "\n", encoding="utf-8"
    )

    read = gate_states_reader(tmp_path)
    result = read()

    assert result["p1"].state == "working"
    assert "p2" not in result


def test_never_raises_when_a_session_file_is_not_valid_utf8(tmp_path):
    """path.read_text(encoding="utf-8") raises UnicodeDecodeError, a
    ValueError, on a write torn mid multibyte character. A file this
    broken has nothing readable left in it, so the whole file is skipped
    rather than any one entry."""
    sessions_dir = tmp_path / "sessions"
    sessions_dir.mkdir(parents=True)
    (sessions_dir / "s1.jsonl").write_bytes(b'{"type": "state", "pane": "p1"\xff\xfe')

    read = gate_states_reader(tmp_path)
    assert read() == {}


def test_never_raises_when_a_session_file_disappears_between_glob_and_read(tmp_path, monkeypatch):
    sessions_dir = tmp_path / "sessions"
    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    _write_state_entry(sessions_dir, "s1", ev)

    real_glob = Path.glob

    def _glob_then_delete(self, pattern):
        paths = list(real_glob(self, pattern))
        for p in paths:
            p.unlink(missing_ok=True)
        return iter(paths)

    monkeypatch.setattr(Path, "glob", _glob_then_delete)
    read = gate_states_reader(tmp_path)
    assert read() == {}
