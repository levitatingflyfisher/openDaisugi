import json

import pytest

from opendaisugi.hook import _classify_tool, _payload_to_record, record_call


def hermes(tool_name, tool_input, **extra):
    # The stdin shape of a Hermes shell hook (docs/user-guide/features/hooks).
    return {
        "hook_event_name": "pre_tool_call",
        "tool_name": tool_name,
        "tool_input": tool_input,
        "session_id": "h-1",
        "cwd": "/w",
        "profile": "default",
        "extra": {"task_id": "t", "tool_call_id": "c1"},
        **extra,
    }


@pytest.mark.parametrize(
    "name,kind",
    [
        ("terminal", "shell"),
        ("read_file", "file_read"),
        ("search_files", "file_read"),
        ("write_file", "file_write"),
        ("patch", "file_write"),
        ("web_search", "network"),
        ("web_extract", "network"),
    ],
)
def test_hermes_names_classify(name, kind):
    assert _classify_tool(name, fmt="hermes") == kind


def test_hermes_names_do_not_classify_under_claude():
    assert _classify_tool("terminal", fmt="claude") is None


def test_hermes_unknown_tool_is_dropped_not_mcp():
    assert _classify_tool("vision_analyze", fmt="hermes") is None
    assert _payload_to_record(hermes("vision_analyze", {"image": "x"}), fmt="hermes") is None


def test_hermes_terminal_record():
    rec = _payload_to_record(
        hermes("terminal", {"command": "make test", "pty": True}), fmt="hermes"
    )
    assert rec["step_type"] == "shell" and rec["command"] == "make test"
    assert rec["session_id"] == "h-1"


def test_hermes_relative_read_joins_cwd():
    rec = _payload_to_record(hermes("read_file", {"path": "a.txt"}), fmt="hermes")
    assert rec["path"] == "/w/a.txt"


def test_hermes_write_keeps_no_content():
    rec = _payload_to_record(
        hermes("write_file", {"path": "/w/o.txt", "content": "secret"}), fmt="hermes"
    )
    assert "content" not in rec and rec["content_len"] == 6


def test_hermes_web_extract_url_forms():
    got = _payload_to_record(hermes("web_extract", {"url": "https://a/b"}), fmt="hermes")
    assert got["url"] == "https://a/b"
    got = _payload_to_record(hermes("web_extract", {"urls": ["https://c/d"]}), fmt="hermes")
    assert got["url"] == "https://c/d"
    assert _payload_to_record(hermes("web_search", {"query": "q"}), fmt="hermes")["url"] == "q"


def test_record_call_hermes_writes_a_capture(tmp_path):
    out = record_call(hermes("terminal", {"command": "ls"}), root=tmp_path, fmt="hermes")
    assert out == tmp_path / "h-1.jsonl"
    assert json.loads(out.read_text())["tool_name"] == "terminal"
