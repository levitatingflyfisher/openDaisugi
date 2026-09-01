"""The gate denies any call whose verify() result carries a Z3-timeout
warning, even when verify() itself said `ok` (its lenient-mode default for
low/medium-stakes, non-strict envelopes). A gate that let an unfinished
check through would be exactly the fail-open this module exists to close.

The timeout is forced by replacing ``opendaisugi.gate.verify`` outright
(mirroring ``test_internal_exception_denies`` / ``test_slow_verifier_denies_
via_inner_timeout`` in test_gate.py). Never real Z3 timing.
"""

from __future__ import annotations

import json

from opendaisugi.gate import evaluate_call, gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission, VerificationResult


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(generated_by="test", task="gate test", permissions=Permission(**perms))


def _read_payload(path: str, session: str = "sess1") -> dict:
    return {
        "tool_name": "Read",
        "tool_input": {"file_path": path},
        "session_id": session,
        "hook_event_name": "PreToolUse",
    }


def _payload_bytes(path: str, session: str = "sess1") -> bytes:
    return json.dumps(_read_payload(path, session)).encode()


def _timed_out_but_ok(*_a, **_kw) -> VerificationResult:
    """What verify() returns for a lenient envelope whose Z3 check timed
    out: ok=True (its longstanding default), with the timeout kept as a
    warning verify.is_z3_timeout_warning recognizes."""
    return VerificationResult(
        ok=True,
        violations=[],
        warnings=["Z3 self-consistency check exceeded 5ms"],
        envelope_id="env1",
        plan_id="plan1",
        duration_ms=1.0,
    )


def test_enforce_denies_a_call_verify_marked_ok_but_timed_out(monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.verify", _timed_out_but_ok)
    d = evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="enforce")
    assert d.allow is False
    assert d.would_deny is True
    assert "could not finish a check in time" in d.reason
    assert "exceeded 5ms" in d.reason


def test_shadow_allows_but_would_deny_a_call_that_timed_out(monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.verify", _timed_out_but_ok)
    d = evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="shadow")
    assert d.allow is True
    assert d.would_deny is True
    assert "could not finish a check in time" in d.reason


def test_a_real_violation_is_reported_over_a_coincidental_timeout_warning(monkeypatch):
    """A call that's ALSO a genuine violation keeps its own clause. The
    timeout deny is only for the case that would otherwise have allowed."""

    def _ok_false_with_timeout_warning(*_a, **_kw):
        from opendaisugi.models import Violation

        return VerificationResult(
            ok=False,
            violations=[Violation(stage="permissions", message="denied for real")],
            warnings=["Z3 self-consistency check exceeded 5ms"],
            envelope_id="env1",
            plan_id="plan1",
            duration_ms=1.0,
        )

    monkeypatch.setattr("opendaisugi.gate.verify", _ok_false_with_timeout_warning)
    d = evaluate_call(_read_payload("/allowed/x"), _envelope(), mode="enforce")
    assert d.allow is False
    assert "denied for real" in d.reason


def test_enforce_exit_code_2_through_gate_and_contract(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.verify", _timed_out_but_ok)
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/allowed/x"),
        root=tmp_path,
        fmt="claude",
        mode="enforce",
    )
    assert out.exit_code == 2
    assert "could not finish a check in time" in out.stderr


def test_shadow_logs_would_deny_through_gate_and_contract(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.verify", _timed_out_but_ok)
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/allowed/x", session="sessT"),
        root=tmp_path,
        fmt="claude",
        mode="shadow",
    )
    assert out.exit_code == 0
    log = tmp_path / "shadow" / "sessT.jsonl"
    assert log.exists()
    rec = json.loads(log.read_text().splitlines()[-1])
    assert rec["would_deny"] is True
    assert rec["allow"] is True
    assert rec["mode"] == "shadow"
    assert "could not finish a check in time" in rec["reason"]
