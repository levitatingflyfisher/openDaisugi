from __future__ import annotations

import json

from opendaisugi.hook import stdout_for_format


def test_claude_format_returns_continue_true():
    out = stdout_for_format("claude", block=False)
    assert json.loads(out) == {"continue": True}


def test_hermes_noop_is_empty_object():
    out = stdout_for_format("hermes", block=False)
    assert json.loads(out) == {}


def test_hermes_block_uses_decision_block():
    out = stdout_for_format("hermes", block=True, reason="policy")
    assert json.loads(out) == {"decision": "block", "reason": "policy"}


def test_openclaw_noop_is_empty_object():
    out = stdout_for_format("openclaw", block=False)
    assert json.loads(out) == {}


def test_openclaw_block_uses_block_true():
    out = stdout_for_format("openclaw", block=True, reason="policy")
    assert json.loads(out) == {"block": True, "blockReason": "policy"}


def test_unknown_format_defaults_to_claude_contract():
    out = stdout_for_format("nope", block=False)
    assert json.loads(out) == {"continue": True}


# --- SGCM fix: capture must always emit an allow contract, never crash --------

def test_record_and_contract_survives_non_utf8(tmp_path):
    from opendaisugi.hook import record_and_contract
    out = record_and_contract(b"\xff\xfe not json \x00", root=tmp_path, fmt="claude")
    assert json.loads(out) == {"continue": True}


def test_record_and_contract_survives_deeply_nested(tmp_path):
    # Genuinely deep, balanced JSON — exercises the RecursionError path, not just
    # an unterminated string. Must still fail open with {}.
    from opendaisugi.hook import record_and_contract
    out = record_and_contract(("[" * 6000 + "]" * 6000).encode(), root=tmp_path, fmt="hermes")
    assert json.loads(out) == {}


def test_record_and_contract_survives_unterminated_json(tmp_path):
    from opendaisugi.hook import record_and_contract
    out = record_and_contract(("[" * 6000).encode(), root=tmp_path, fmt="hermes")
    assert json.loads(out) == {}


def test_record_and_contract_records_valid_payload(tmp_path):
    from opendaisugi.hook import record_and_contract
    payload = b'{"tool_name":"Bash","tool_input":{"command":"ls"},"session_id":"s1"}'
    out = record_and_contract(payload, root=tmp_path, fmt="claude")
    assert json.loads(out) == {"continue": True}
    assert (tmp_path / "s1.jsonl").exists()


# --- SGCM polish: capture security (path traversal + perms) -------------------

def test_record_call_neutralizes_session_id_path_traversal(tmp_path):
    from opendaisugi.hook import record_call
    root = tmp_path / "caps"
    record_call(
        {"session_id": "../../../evil", "tool_name": "Bash", "tool_input": {"command": "ls"}},
        root=root,
    )
    assert not (tmp_path / "evil.jsonl").exists()
    assert not (tmp_path.parent / "evil.jsonl").exists()
    files = list(root.glob("*.jsonl"))
    assert len(files) == 1 and files[0].parent == root  # contained inside root


def test_capture_dir_and_file_not_world_or_group_accessible(tmp_path):
    import os

    from opendaisugi.hook import record_call
    root = tmp_path / "caps"
    root.mkdir(mode=0o777)  # pre-existing loose dir
    p = record_call(
        {"session_id": "s", "tool_name": "Bash", "tool_input": {"command": "ls"}},
        root=root,
    )
    assert (os.stat(root).st_mode & 0o077) == 0, "captures dir must be 0o700"
    assert (os.stat(p).st_mode & 0o077) == 0, "capture file must be 0o600"
