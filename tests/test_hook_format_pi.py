from __future__ import annotations

import json

from opendaisugi.gate import GateDecision, evaluate_call, gate_and_contract, register_envelope
from opendaisugi.hook import _classify_tool, _payload_to_record
from opendaisugi.models import Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(generated_by="test", task="pi format test", permissions=Permission(**perms))


def _gate_root(tmp_path, envelope=None):
    root = tmp_path / "gate"
    register_envelope(envelope or _envelope(), session_id="s1", root=root)
    return root


def _payload(tool_name: str, tool_input: dict) -> bytes:
    return json.dumps(
        {"tool_name": tool_name, "tool_input": tool_input, "session_id": "s1"}
    ).encode()


# --- bash/read/write/edit classify the same way Claude's Bash/Read/Write/Edit do -----------


def test_pi_bash_classifies_as_shell():
    assert _classify_tool("bash", fmt="pi") == "shell"


def test_pi_read_classifies_as_file_read():
    assert _classify_tool("read", fmt="pi") == "file_read"


def test_pi_write_classifies_as_file_write():
    assert _classify_tool("write", fmt="pi") == "file_write"


def test_pi_edit_classifies_as_file_write():
    assert _classify_tool("edit", fmt="pi") == "file_write"


def test_claude_format_is_unaffected_by_the_pi_additions():
    assert _classify_tool("Bash") == "shell"
    assert _classify_tool("Read") == "file_read"
    # Lowercase pi names are not recognized under the claude default.
    assert _classify_tool("bash") is None


def test_payload_to_record_maps_pi_path_field_for_read():
    rec = _payload_to_record({"tool_name": "read", "tool_input": {"path": "/allowed/x"}}, fmt="pi")
    assert rec["step_type"] == "file_read"
    assert rec["path"] == "/allowed/x"


# --- "anything else" becomes MCP-style, not an unconditional deny --------------------------


def test_unknown_pi_tool_becomes_mcp_style_not_dropped():
    rec = _payload_to_record(
        {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}, fmt="pi"
    )
    assert rec is not None, "pi's fallback must not silently drop an unrecognized tool"
    assert rec["step_type"] == "mcp"
    assert rec["mcp_server"] == "pi"
    assert rec["mcp_tool"] == "grep_files"
    assert rec["arguments"] == {"pattern": "TODO"}


def test_same_unknown_tool_under_claude_fmt_is_still_dropped():
    assert _payload_to_record({"tool_name": "grep_files", "tool_input": {}}) is None


def test_unknown_pi_tool_is_denied_when_not_in_mcp_allowlist():
    payload = {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}
    d = evaluate_call(payload, _envelope(), mode="enforce", fmt="pi")
    assert isinstance(d, GateDecision)
    assert d.allow is False
    assert "pi/grep_files" in d.reason or "mcp_allowlist" in d.reason


def test_unknown_pi_tool_is_allowed_once_named_in_mcp_allowlist():
    payload = {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}
    env = _envelope(mcp_allowlist=["pi/grep_files"])
    d = evaluate_call(payload, env, mode="enforce", fmt="pi")
    assert d.allow is True


# --- the real socket-facing exit-code contract ----------------------------------------------
# Each test is named for the failure it proves. A test that would let a bad
# exit-code format resolve to "allow" is the regression this file exists to
# catch.


def test_pi_is_an_exit_code_format():
    from opendaisugi.hook import EXIT_CODE_FORMATS

    assert {"claude", "pi", "opencode"} <= EXIT_CODE_FORMATS


def test_stdout_block_formats_are_exactly_hermes_and_openclaw():
    from opendaisugi.hook import EXIT_CODE_FORMATS, STDOUT_BLOCK_FORMATS

    assert STDOUT_BLOCK_FORMATS == {"hermes", "openclaw"}
    assert not (STDOUT_BLOCK_FORMATS & EXIT_CODE_FORMATS)


def test_pi_format_denies_via_exit_code_not_json_stdout(tmp_path):
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("bash", {"command": "rm -rf /"}), root=root, fmt="pi", mode="enforce"
    )
    assert out.exit_code == 2
    assert "DENIED" in out.stderr
    assert out.stdout == ""


def test_pi_format_shadow_mode_never_denies_the_host(tmp_path):
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("bash", {"command": "rm -rf /"}), root=root, fmt="pi", mode="shadow"
    )
    assert out.exit_code == 0
    assert out.decision.would_deny is True  # observed and logged, just not blocked


def test_pi_format_allows_via_exit_code_zero(tmp_path):
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("read", {"path": "/allowed/x"}), root=root, fmt="pi", mode="enforce"
    )
    assert out.exit_code == 0
    assert out.stderr == ""
    # stdout is stdout_for_format's default body. askGate reads only
    # exit_code and never parses stdout.


def test_hermes_format_is_unaffected_by_the_pi_fix(tmp_path):
    """Regression guard: hermes keeps its JSON-carries-the-verdict contract
    (exit_code 0 always). The fix must not widen EXIT_CODE_FORMATS beyond
    claude, pi and opencode."""
    root = _gate_root(tmp_path, _envelope(shell=False))
    out = gate_and_contract(
        _payload("Bash", {"command": "rm -rf /"}), root=root, fmt="hermes", mode="enforce"
    )
    assert out.exit_code == 0
    body = json.loads(out.stdout)
    assert body.get("decision") == "block" or body.get("action") == "block"


def test_pi_harness_label_is_intentional_in_the_session_tree():
    from opendaisugi.gate import _HARNESS_BY_FMT

    assert _HARNESS_BY_FMT["pi"] == "pi"


def test_unknown_format_denies_instead_of_falling_through_to_continue_true(tmp_path):
    """--format has no argparse choices=, so a typo, or codex's still
    unwired gap, must not read as an allow."""
    root = _gate_root(tmp_path)
    out = gate_and_contract(
        _payload("read", {"path": "/allowed/x"}), root=root, fmt="codex", mode="enforce"
    )
    assert out.exit_code == 2
    assert "unknown host format" in out.stderr


def test_unknown_format_denies_even_when_the_underlying_decision_would_allow(tmp_path):
    root = _gate_root(tmp_path)
    # A call the envelope admits. The format guard must still deny: a name
    # outside both known sets has no safe body to return either way.
    out = gate_and_contract(
        _payload("Read", {"file_path": "/allowed/x"}), root=root, fmt="banana", mode="enforce"
    )
    assert out.exit_code == 2


def test_escape_outcome_shadow_body_follows_the_format():
    from opendaisugi.gate import _escape_outcome, _fmt_from_argv

    assert _fmt_from_argv(["--format", "hermes", "--root", "/x"]) == "hermes"
    assert _fmt_from_argv(["--format=openclaw"]) == "openclaw"
    assert _fmt_from_argv(["--root", "/x"]) == "claude"
    out = _escape_outcome("shadow", RuntimeError("boom"), fmt="hermes")
    assert out.exit_code == 0
    assert json.loads(out.stdout) == {}
    out = _escape_outcome("enforce", RuntimeError("boom"), fmt="hermes")
    assert out.exit_code == 2


# --- the full built-in list is pinned and covered ----------------------------------------


def test_pi_powershell_classifies_as_shell():
    assert _classify_tool("powershell", fmt="pi") == "shell"


def test_pi_powershell_command_reaches_the_shell_predicate(tmp_path):
    root = _gate_root(tmp_path, _envelope(shell=False))
    out = gate_and_contract(
        _payload("powershell", {"command": "Remove-Item -Recurse C:\\"}),
        root=root,
        fmt="pi",
        mode="enforce",
    )
    assert out.exit_code == 2
    assert out.decision.step_type == "shell"


def test_pi_tool_map_covers_every_pinned_builtin():
    """Every built-in pi ships is named in hook.py and in PINS.md, and each
    classifies to a step type under fmt="pi". A built-in with a known input
    shape maps directly; the rest are MCP-style on purpose, never dropped."""
    from pathlib import Path

    from opendaisugi.hook import _PI_BUILTIN_TOOLS, _PI_TOOL_TYPE_MAP

    pins = (
        Path(__file__).parents[1] / "src" / "opendaisugi" / "harness_pi" / "extension" / "PINS.md"
    ).read_text(encoding="utf-8")
    assert _PI_BUILTIN_TOOLS == {
        "read",
        "bash",
        "powershell",
        "edit",
        "write",
        "grep",
        "find",
        "ls",
    }
    for name in _PI_BUILTIN_TOOLS:
        assert f"`{name}`" in pins
        assert _classify_tool(name, fmt="pi") is not None
    assert set(_PI_TOOL_TYPE_MAP) <= _PI_BUILTIN_TOOLS
    assert {"bash", "powershell"} <= {k for k, v in _PI_TOOL_TYPE_MAP.items() if v == "shell"}
