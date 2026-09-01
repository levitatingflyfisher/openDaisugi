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
    p = record_call(
        {
            "session_id": "sess1",
            "tool_name": "MysteryTool",
            "tool_input": {},
        },
        root=tmp_path,
    )
    assert p is None


def test_record_call_handles_hermes_shape(tmp_path: Path):
    """Hermes shell-hooks emit ``{tool, args, session_id}`` — also accepted."""
    p = record_call(
        {
            "session_id": "sess1",
            "tool": "shell",
            "args": {"command": "ls"},
        },
        root=tmp_path,
    )
    assert p is not None
    rec = json.loads(p.read_text().strip())
    assert rec["step_type"] == "shell"
    assert rec["command"] == "ls"


def test_record_call_strips_file_write_content(tmp_path: Path):
    """v0.21 captures store content_len, not content — captures are not exfil."""
    p = record_call(
        {
            "session_id": "sess1",
            "tool_name": "Write",
            "tool_input": {"file_path": "/tmp/x.txt", "content": "secret data" * 100},
        },
        root=tmp_path,
    )
    rec = json.loads(p.read_text().strip())
    assert "content" not in rec
    assert rec["content_len"] == len("secret data") * 100


def test_list_sessions_returns_summaries(tmp_path: Path):
    record_call(
        {"session_id": "a", "tool_name": "Bash", "tool_input": {"command": "ls"}}, root=tmp_path
    )
    record_call(
        {"session_id": "a", "tool_name": "Read", "tool_input": {"file_path": "f"}}, root=tmp_path
    )
    record_call(
        {"session_id": "b", "tool_name": "Bash", "tool_input": {"command": "pwd"}}, root=tmp_path
    )
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


def test_infer_envelope_admits_observed_mcp_calls():
    """Contract: the inferred envelope admits everything observed. Now that
    MCP calls are captured, their server/tool must land in mcp_allowlist —
    otherwise captures_to_trace would produce a failing trace for a call that
    really happened."""
    records = [
        {"step_type": "mcp", "mcp_server": "github", "mcp_tool": "create_issue"},
        {"step_type": "mcp", "mcp_server": "slack", "mcp_tool": "post_message"},
    ]
    env = infer_envelope(records, task="t")
    assert "github/create_issue" in env.permissions.mcp_allowlist
    assert "slack/post_message" in env.permissions.mcp_allowlist


def test_captures_to_trace_roundtrip(tmp_path: Path):
    """Capture → infer envelope → build plan → verify → journal.log."""
    # Capture a small session
    record_call(
        {"session_id": "demo", "tool_name": "Bash", "tool_input": {"command": "git status"}},
        root=tmp_path,
    )
    record_call(
        {"session_id": "demo", "tool_name": "Read", "tool_input": {"file_path": "/etc/hosts"}},
        root=tmp_path,
    )
    journal = Journal(data_dir=tmp_path / "j")
    trace_id = captures_to_trace(
        tmp_path / "demo.jsonl",
        journal,
        task="capture demo",
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


# ------------------------------------------------ subagents as child rows


def _subagent(event: str, **extra) -> bytes:
    body = {"session_id": "s1", "hook_event_name": event, "agent_type": "Explore", **extra}
    return json.dumps(body).encode()


@pytest.mark.parametrize(
    ("event", "hook", "state"),
    [("subagent_start", "SubagentStart", "working"), ("subagent_stop", "SubagentStop", "done")],
)
def test_a_subagent_hook_reports_a_child(monkeypatch, tmp_path, event, hook, state):
    from opendaisugi.hook import record_lifecycle_event

    seen: list[tuple] = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_child",
        lambda *a, **k: seen.append(a) or "coppice",
    )
    out = record_lifecycle_event(
        _subagent(hook, agent_id="a1"), event=event, sessions_root=tmp_path / "sessions"
    )
    assert seen == [("a1", state, "Explore")]
    assert out == "" or "block" not in out.lower()


def test_a_subagent_hook_with_no_agent_id_reports_nothing(monkeypatch, tmp_path):
    from opendaisugi.hook import record_lifecycle_event

    seen: list[tuple] = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_child", lambda *a, **k: seen.append(a) or "none"
    )
    record_lifecycle_event(
        _subagent("SubagentStart"), event="subagent_start", sessions_root=tmp_path / "sessions"
    )
    assert seen == []


def test_report_child_sends_one_line_to_the_pane_host(tmp_path):
    import socket
    import threading

    from opendaisugi._state_report import report_child

    sock_path = tmp_path / "s.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)
    got: list[bytes] = []

    def serve():
        conn, _ = srv.accept()
        with conn:
            data = b""
            while not data.endswith(b"\n"):
                chunk = conn.recv(4096)
                if not chunk:
                    break
                data += chunk
            got.append(data)
            conn.sendall(b'{"id":"c","ok":true,"result":{}}\n')

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    env = {"COPPICE_SOCK": str(sock_path), "COPPICE_PANE": "w1:p1"}
    assert report_child("a1", "working", "Explore", env=env, budget_s=2.0) == "coppice"
    t.join(2)
    srv.close()
    line = json.loads(got[0])
    assert line["cmd"] == "pane.report_child"
    assert (line["pane"], line["child"], line["state"], line["label"]) == (
        "w1:p1",
        "a1",
        "working",
        "Explore",
    )


def test_report_child_outside_a_pane_goes_nowhere():
    from opendaisugi._state_report import report_child

    assert report_child("a1", "working", "Explore", env={}) == "none"
