"""The envelope's deadline holds at call time.

An envelope with a ``deadline`` starts no new work after it. The gate denies
every call made after the deadline of the envelope that checks it, with a
one-line reason. DAISUGI_GATE_NOW pins the clock for tests, and it can only
move the clock forward: the gate reads the later of it and the real time,
so the variable can add denies but never extend a deadline.
"""

from __future__ import annotations

import json

import pytest

from opendaisugi import gate as gate_mod
from opendaisugi.gate import evaluate_call, gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission

FAR = 4000000000.0


def _env(deadline: float | None) -> Envelope:
    return Envelope(
        generated_by="t",
        task="t",
        deadline=deadline,
        permissions=Permission(file_read=["/w/**"], shell=True, shell_allowlist=["ls"]),
    )


def _read(path: str = "/w/a") -> dict:
    return {"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": path}, "cwd": "/w"}


def test_a_call_before_the_deadline_is_checked_as_before(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "3999999999")
    d = evaluate_call(_read(), _env(FAR), mode="enforce")
    assert d.allow and not d.would_deny


def test_a_call_at_the_deadline_still_passes(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "4000000000")
    d = evaluate_call(_read(), _env(FAR), mode="enforce")
    assert d.allow


def test_a_call_after_the_deadline_is_denied_with_one_line(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "4000000000.5")
    d = evaluate_call(_read(), _env(FAR), mode="enforce")
    assert not d.allow and d.would_deny
    assert d.reason == gate_mod.deadline_reason(FAR)
    assert "\n" not in d.reason
    assert "4000000000.0" in d.reason


def test_audit_mode_records_it_and_allows(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "4000000001")
    d = evaluate_call(_read(), _env(FAR), mode="audit")
    assert d.allow and d.would_deny


def test_no_deadline_no_check(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "9999999999")
    d = evaluate_call(_read(), _env(None), mode="enforce")
    assert d.allow


def test_a_past_deadline_denies_with_no_pin(monkeypatch):
    monkeypatch.delenv("DAISUGI_GATE_NOW", raising=False)
    d = evaluate_call(_read(), _env(1000000000.0), mode="enforce")
    assert not d.allow and d.reason == gate_mod.deadline_reason(1000000000.0)


@pytest.mark.parametrize("pin", ["1", "0", "nan", "inf", "-5", "1e12", " 4000000001", "x", ""])
def test_a_pin_never_moves_the_clock_back_and_odd_pins_are_ignored(monkeypatch, pin):
    # A deadline already past stays past whatever the pin says.
    monkeypatch.setenv("DAISUGI_GATE_NOW", pin)
    d = evaluate_call(_read(), _env(1000000000.0), mode="enforce")
    assert not d.allow
    # An odd pin is ignored: a far deadline is not reached.
    d = evaluate_call(_read(), _env(FAR), mode="enforce")
    assert d.allow


def test_the_hard_deny_rules_come_first(monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "4000000001")
    cmd = {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": "daisugi gate disarm"},
    }
    d = evaluate_call(cmd, _env(FAR), mode="enforce")
    assert d.pane_rule and "operator" in d.reason


def test_the_whole_gate_denies_after_the_deadline(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_GATE_NOW", "4000000001")
    root = tmp_path / "gate"
    register_envelope(_env(FAR), root=root)
    out = gate_and_contract(json.dumps(_read()).encode(), root=root, fmt="claude", mode="enforce")
    assert out.exit_code == 2
    assert out.decision.reason == gate_mod.deadline_reason(FAR)
