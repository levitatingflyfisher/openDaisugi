"""Tests for the passive hook module (v0.21)."""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.hook import (
    captures_to_trace,
    infer_envelope,
    list_sessions,
    record_call,
)
from opendaisugi.journal import Journal


def test_record_call_writes_jsonl(tmp_path: Path):
    """A normal Claude Code Bash payload writes one row to the session file."""
    payload = {
        "session_id": "sess1",
        "tool_name": "Bash",
        "tool_input": {"command": "echo hi"},
    }
    p = record_call(payload, root=tmp_path)
    assert p == tmp_path / "sess1.jsonl"
    rows = p.read_text().strip().splitlines()
    assert len(rows) == 1
    rec = json.loads(rows[0])
    assert rec["tool_name"] == "Bash"
    assert rec["step_type"] == "shell"
    assert rec["command"] == "echo hi"


def test_record_call_unknown_tool_returns_none(tmp_path: Path):
    """Unknown tool names produce no record; caller emits continue:true regardless."""
    p = record_call({
        "session_id": "sess1",
        "tool_name": "MysteryTool",
        "tool_input": {},
    }, root=tmp_path)
    assert p is None


def test_record_call_handles_hermes_shape(tmp_path: Path):
    """Hermes shell-hooks emit ``{tool, args, session_id}`` — also accepted."""
    p = record_call({
        "session_id": "sess1",
        "tool": "shell",
        "args": {"command": "ls"},
    }, root=tmp_path)
    assert p is not None
    rec = json.loads(p.read_text().strip())
    assert rec["step_type"] == "shell"
    assert rec["command"] == "ls"


def test_record_call_strips_file_write_content(tmp_path: Path):
    """v0.21 captures store content_len, not content — captures are not exfil."""
    p = record_call({
        "session_id": "sess1",
        "tool_name": "Write",
        "tool_input": {"file_path": "/tmp/x.txt", "content": "secret data" * 100},
    }, root=tmp_path)
    rec = json.loads(p.read_text().strip())
    assert "content" not in rec
    assert rec["content_len"] == len("secret data") * 100


def test_list_sessions_returns_summaries(tmp_path: Path):
    record_call({"session_id": "a", "tool_name": "Bash", "tool_input": {"command": "ls"}}, root=tmp_path)
    record_call({"session_id": "a", "tool_name": "Read", "tool_input": {"file_path": "f"}}, root=tmp_path)
    record_call({"session_id": "b", "tool_name": "Bash", "tool_input": {"command": "pwd"}}, root=tmp_path)
    sessions = list_sessions(root=tmp_path)
    by_id = {s["session_id"]: s for s in sessions}
    assert by_id["a"]["calls"] == 2
    assert by_id["b"]["calls"] == 1


def test_infer_envelope_synthesizes_permissions():
    records = [
        {"step_type": "shell", "command": "git status"},
        {"step_type": "shell", "command": "ls /tmp"},
        {"step_type": "file_read", "path": "/etc/hosts"},
        {"step_type": "file_write", "path": "/tmp/out.txt"},
        {"step_type": "network", "url": "https://api.example.com/v1/x"},
    ]
    env = infer_envelope(records, task="t")
    assert env.permissions.shell is True
    assert "git" in env.permissions.shell_allowlist
    assert "ls" in env.permissions.shell_allowlist
    assert any("/etc/**" in g or "/etc" in g for g in env.permissions.file_read)
    assert any("/tmp/**" in g for g in env.permissions.file_write)
    assert env.permissions.network is True
    assert "api.example.com" in env.permissions.network_hosts


def test_infer_envelope_no_shell_when_no_shell_calls():
    records = [{"step_type": "file_read", "path": "/etc/hosts"}]
    env = infer_envelope(records)
    assert env.permissions.shell is False


def test_captures_to_trace_roundtrip(tmp_path: Path):
    """Capture → infer envelope → build plan → verify → journal.log."""
    # Capture a small session
    record_call({"session_id": "demo", "tool_name": "Bash", "tool_input": {"command": "git status"}}, root=tmp_path)
    record_call({"session_id": "demo", "tool_name": "Read", "tool_input": {"file_path": "/etc/hosts"}}, root=tmp_path)
    journal = Journal(data_dir=tmp_path / "j")
    trace_id = captures_to_trace(
        tmp_path / "demo.jsonl", journal, task="capture demo",
    )
    assert trace_id
    rows = journal.list_recent(limit=5)
    assert any(r.id == trace_id for r in rows)


def test_captures_to_trace_empty_session_raises(tmp_path: Path):
    empty = tmp_path / "empty.jsonl"
    empty.write_text("")
    journal = Journal(data_dir=tmp_path / "j")
    with pytest.raises(ValueError, match="no records"):
        captures_to_trace(empty, journal)


def test_journal_tracks_session_conversion(tmp_path: Path):
    """v0.22+ auto-tend depends on the journal remembering which captured
    sessions have been converted to traces."""
    j = Journal(data_dir=tmp_path)
    assert j.is_session_converted("sess1") is False
    j.mark_session_converted("sess1", "trace_xyz", converted_at=1000.0)
    assert j.is_session_converted("sess1") is True
    # Idempotent — re-marking is fine.
    j.mark_session_converted("sess1", "trace_xyz", converted_at=2000.0)
    assert j.is_session_converted("sess1") is True
    assert j.is_session_converted("sess2") is False
