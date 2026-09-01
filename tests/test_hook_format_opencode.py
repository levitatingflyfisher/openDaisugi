"""The gate's view of an OpenCode tool call.

The OpenCode plugin maps each built-in tool id to the gate's own tool name
and every other id to mcp__opencode__<id>, and sends --format opencode. So
under this format a record looks like a Claude record, except that
OpenCode's file tools name their path filePath.
"""

from __future__ import annotations

import json

from opendaisugi.gate import GateDecision, evaluate_call, gate_and_contract, register_envelope
from opendaisugi.hook import EXIT_CODE_FORMATS, _payload_to_record
from opendaisugi.models import Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(
        generated_by="test", task="opencode format test", permissions=Permission(**perms)
    )


def _gate_root(tmp_path, envelope=None):
    root = tmp_path / "gate"
    register_envelope(envelope or _envelope(), root=root)
    return root


def _payload(tool_name: str, tool_input: dict) -> bytes:
    return json.dumps(
        {"tool_name": tool_name, "tool_input": tool_input, "session_id": "ses_abc"}
    ).encode()


def test_filepath_is_the_path_of_an_opencode_read():
    record = _payload_to_record(
        {"tool_name": "Read", "tool_input": {"filePath": "/proj/a.txt"}}, fmt="opencode"
    )
    assert record is not None
    assert record["path"] == "/proj/a.txt"


def test_filepath_is_the_path_of_an_opencode_edit_and_write():
    for tool in ("Edit", "Write"):
        record = _payload_to_record(
            {"tool_name": tool, "tool_input": {"filePath": "/proj/b.txt", "content": "x"}},
            fmt="opencode",
        )
        assert record is not None
        assert record["step_type"] == "file_write"
        assert record["path"] == "/proj/b.txt"


def test_filepath_does_not_shadow_the_existing_path_keys():
    record = _payload_to_record({"tool_name": "Read", "tool_input": {"file_path": "/x/y.txt"}})
    assert record["path"] == "/x/y.txt"
    record = _payload_to_record({"tool_name": "Read", "tool_input": {"path": "/x/z.txt"}})
    assert record["path"] == "/x/z.txt"


def test_an_opencode_extra_tool_is_mcp_style_under_the_opencode_server():
    record = _payload_to_record(
        {"tool_name": "mcp__opencode__todowrite", "tool_input": {"todos": []}},
        fmt="opencode",
    )
    assert record["step_type"] == "mcp"
    assert (record["mcp_server"], record["mcp_tool"]) == ("opencode", "todowrite")


def test_an_opencode_extra_tool_is_denied_until_the_envelope_names_it():
    payload = {"tool_name": "mcp__opencode__todowrite", "tool_input": {"todos": []}}
    d = evaluate_call(payload, _envelope(), mode="enforce", fmt="opencode")
    assert isinstance(d, GateDecision)
    assert d.allow is False
    env = _envelope(mcp_allowlist=["opencode/todowrite"])
    assert evaluate_call(payload, env, mode="enforce", fmt="opencode").allow is True


def test_opencode_is_an_exit_code_format():
    assert "opencode" in EXIT_CODE_FORMATS


def test_opencode_format_denies_with_exit_code_2(tmp_path):
    root = _gate_root(tmp_path, _envelope(shell=False))
    out = gate_and_contract(
        _payload("Bash", {"command": "rm -rf /"}), root=root, fmt="opencode", mode="enforce"
    )
    assert out.exit_code == 2
    assert "DENIED" in out.stderr


def test_opencode_format_allows_a_filepath_read_with_exit_code_0(tmp_path):
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("Read", {"filePath": "/allowed/a.txt"}), root=root, fmt="opencode", mode="enforce"
    )
    assert out.exit_code == 0, out.stderr


def test_opencode_format_denies_a_filepath_read_outside_the_envelope(tmp_path):
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("Read", {"filePath": "/etc/shadow"}), root=root, fmt="opencode", mode="enforce"
    )
    assert out.exit_code == 2
