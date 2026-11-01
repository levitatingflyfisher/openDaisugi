"""Codex payload pins: the documented fields, `apply_patch` by Codex's name.

Codex sends the claude-shaped payload plus `model`, `turn_id` and
`permission_mode`. Its `apply_patch` tool names no `mcp__` prefix and puts the
patch text in `tool_input.command`. The warden reads it as one file write per
path, as it reads OpenCode's. Codex's `update_plan` stays unrecognized.
"""

from __future__ import annotations

import pytest

from opendaisugi.hook import JOIN_KEYS, _payload_to_record
from opendaisugi.models import Envelope, Permission
from opendaisugi.warden import evaluate_call


def codex(tool, inp):
    return {
        "session_id": "thr_1",
        "transcript_path": "/home/user/.codex/sessions/thr_1.jsonl",
        "cwd": "/work",
        "hook_event_name": "PreToolUse",
        "model": "gpt-5-codex",
        "permission_mode": "default",
        "turn_id": "turn_1",
        "tool_name": tool,
        "tool_use_id": "call_1",
        "tool_input": inp,
    }


PATCH_IN = "*** Begin Patch\n*** Update File: /work/a.py\n@@\n-x\n+y\n*** End Patch\n"
PATCH_OUT = "*** Begin Patch\n*** Update File: /etc/hosts\n@@\n-x\n+y\n*** End Patch\n"


@pytest.fixture
def homes(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    return tmp_path


def _env(**kw) -> Envelope:
    perms = {"file_read": ["/**"], "file_write": ["/work/**"], **kw}
    return Envelope(generated_by="test", task="codex", permissions=Permission(**perms))


def test_join_keys_keep_turn_and_model():
    assert "turn_id" in JOIN_KEYS and "model" in JOIN_KEYS
    rec = _payload_to_record(codex("Bash", {"command": "ls"}))
    assert rec["turn_id"] == "turn_1" and rec["model"] == "gpt-5-codex" and rec["command"] == "ls"


def test_absent_join_keys_stay_absent():
    p = codex("Bash", {"command": "ls"})
    del p["turn_id"], p["model"]
    rec = _payload_to_record(p)
    assert "turn_id" not in rec and "model" not in rec


def test_apply_patch_by_codex_name_is_a_patch_not_a_capture():
    assert _payload_to_record(codex("apply_patch", {"command": PATCH_IN})) is None


def test_mcp_arguments_arrive():
    rec = _payload_to_record(codex("mcp__github__get_issue", {"repo": "acme/app", "number": 3}))
    assert rec["step_type"] == "mcp" and rec["arguments"] == {"repo": "acme/app", "number": 3}


def test_apply_patch_inside_is_allowed(homes):
    d = evaluate_call(codex("apply_patch", {"command": PATCH_IN}), _env(), mode="enforce")
    assert not d.would_deny, d.reason


def test_apply_patch_outside_is_denied(homes):
    d = evaluate_call(codex("apply_patch", {"command": PATCH_OUT}), _env(), mode="enforce")
    assert d.would_deny


def test_apply_patch_heredoc_is_read(homes):
    heredoc = "cat <<'EOF'\n" + PATCH_OUT.rstrip("\n") + "\nEOF"
    d = evaluate_call(codex("apply_patch", {"command": heredoc}), _env(), mode="enforce")
    assert d.would_deny and "unrecognized" not in d.reason


def test_apply_patch_without_text_is_denied(homes):
    d = evaluate_call(codex("apply_patch", {}), _env(), mode="enforce")
    assert d.would_deny and "cannot read this patch" in d.reason


def test_update_plan_stays_unrecognized(homes):
    d = evaluate_call(codex("update_plan", {"plan": []}), _env(), mode="enforce")
    assert d.would_deny and d.reason.startswith("unrecognized tool")


def test_mcp_rule_denies_a_bad_argument_under_codex(homes):
    env = _env(
        mcp_allowlist=["github/get_issue"],
        mcp_rules=[
            {
                "tool": "github/get_issue",
                "args": [{"op": "equals", "path": "repo", "value": "acme/app"}],
            }
        ],
    )
    ok = evaluate_call(codex("mcp__github__get_issue", {"repo": "acme/app"}), env, mode="enforce")
    bad = evaluate_call(codex("mcp__github__get_issue", {"repo": "evil/x"}), env, mode="enforce")
    assert not ok.would_deny, ok.reason
    assert bad.would_deny
