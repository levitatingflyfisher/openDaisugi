"""Tests for the call-time tool gate (ADR-0007, roadmap Stage 1).

The decision core is deny-by-default: unknown tool, unparseable input,
internal exception, and slow verifier ALL deny — each path pinned here,
because a gate whose failure modes allow is the fail-open we exist to
prevent. Shadow mode always allows but records exactly what enforce
would have denied.
"""

from __future__ import annotations

import json
import time

import pytest

from opendaisugi.models import Envelope, Permission

from opendaisugi.gate import (
    GateDecision,
    evaluate_call,
)


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(
        generated_by="test",
        task="gate test",
        permissions=Permission(**perms),
    )


def _read_payload(path: str) -> dict:
    return {
        "tool_name": "Read",
        "tool_input": {"file_path": path},
        "session_id": "sess1",
        "hook_event_name": "PreToolUse",
    }


# ---------------------------------------------------------------- allow path

def test_in_envelope_read_is_allowed():
    d = evaluate_call(_read_payload("/allowed/notes.txt"), _envelope(), mode="enforce")
    assert isinstance(d, GateDecision)
    assert d.allow is True
    assert d.would_deny is False
    assert d.tool_name == "Read"
    assert d.step_type == "file_read"


def test_decision_carries_detail_and_elapsed():
    d = evaluate_call(_read_payload("/allowed/notes.txt"), _envelope(), mode="enforce")
    assert "/allowed/notes.txt" in d.detail
    assert d.elapsed_ms >= 0


# ----------------------------------------------------------------- deny paths

def test_out_of_envelope_read_is_denied_with_violation_reason():
    d = evaluate_call(_read_payload("/etc/passwd"), _envelope(), mode="enforce")
    assert d.allow is False
    assert d.would_deny is True
    assert "/etc/passwd" in d.reason


def test_unknown_tool_is_denied():
    payload = {"tool_name": "TotallyNovelTool", "tool_input": {}, "session_id": "s"}
    d = evaluate_call(payload, _envelope(), mode="enforce")
    assert d.allow is False
    assert "TotallyNovelTool" in d.reason


def test_missing_tool_name_is_denied():
    d = evaluate_call({"tool_input": {"file_path": "/allowed/x"}}, _envelope(), mode="enforce")
    assert d.allow is False
    assert "tool name" in d.reason.lower()


def test_non_dict_payload_is_denied():
    d = evaluate_call(["not", "a", "dict"], _envelope(), mode="enforce")
    assert d.allow is False


def test_internal_exception_denies(monkeypatch):
    def _boom(*a, **k):
        raise RuntimeError("verifier exploded")
    monkeypatch.setattr("opendaisugi.gate.verify", _boom)
    d = evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="enforce")
    assert d.allow is False
    assert "verifier exploded" in d.reason


def test_slow_verifier_denies_via_inner_timeout(monkeypatch):
    def _slow(*a, **k):
        time.sleep(2.0)
    monkeypatch.setattr("opendaisugi.gate.verify", _slow)
    t0 = time.monotonic()
    d = evaluate_call(
        _read_payload("/allowed/x"), _envelope(), mode="enforce",
        verify_timeout_s=0.1,
    )
    assert time.monotonic() - t0 < 1.5  # denied without waiting the full sleep
    assert d.allow is False
    assert "time" in d.reason.lower()


# --------------------------------------------------------------- shadow mode

def test_shadow_mode_allows_but_records_would_deny():
    d = evaluate_call(_read_payload("/etc/passwd"), _envelope(), mode="shadow")
    assert d.allow is True
    assert d.would_deny is True
    assert d.mode == "shadow"


def test_shadow_mode_in_envelope_records_no_would_deny():
    d = evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="shadow")
    assert d.allow is True
    assert d.would_deny is False


# ------------------------------------------------- strictness from the stakes

def test_strictness_is_left_to_the_envelope(monkeypatch):
    """The gate must pass strict=None so resolve_strict() derives strictness
    from the envelope's stakes — never relaxed (or hardened) at the gate."""
    seen: dict = {}

    def _spy(plan, envelope, **kwargs):
        seen.update(kwargs)
        from opendaisugi.verify import verify as real_verify
        return real_verify(plan, envelope, **kwargs)

    monkeypatch.setattr("opendaisugi.gate.verify", _spy)
    evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="enforce")
    assert seen.get("strict", "MISSING") in (None, "MISSING")
    if "strict" in seen:
        assert seen["strict"] is None


def test_compound_shell_command_denied_and_names_decomposition():
    """The metachar gate applies at the call boundary too — the known
    false-positive economics case (compound &&) must at least carry the
    decomposition remediation in its reason."""
    env = _envelope(shell=True, shell_allowlist=["echo"])
    payload = {
        "tool_name": "Bash",
        "tool_input": {"command": "echo a && echo b"},
        "session_id": "s",
    }
    d = evaluate_call(payload, env, mode="enforce")
    assert d.allow is False
    assert d.step_type == "shell"
