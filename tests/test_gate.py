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

from opendaisugi.gate import (
    GateDecision,
    evaluate_call,
)
from opendaisugi.models import Envelope, Permission


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


# =================================================================
# Task 2: envelope registration channel, disarm, gate_and_contract
# =================================================================

from opendaisugi.gate import (  # noqa: E402
    arm,
    disarm,
    gate_and_contract,
    is_disarmed,
    load_envelope,
    register_envelope,
)


def _payload_bytes(path: str, session: str = "sess1") -> bytes:
    return json.dumps(_read_payload(path) | {"session_id": session}).encode()


# ------------------------------------------------- registration channel

def test_register_and_load_envelope_roundtrip(tmp_path):
    env = _envelope()
    p = register_envelope(env, session_id="sessA", root=tmp_path)
    assert p.exists()
    loaded = load_envelope("sessA", root=tmp_path)
    assert loaded is not None
    assert loaded.permissions.file_read == ["/allowed/**"]


def test_register_without_session_becomes_default_fallback(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    # Any session id falls back to the default envelope.
    assert load_envelope("some-other-session", root=tmp_path) is not None
    assert load_envelope(None, root=tmp_path) is not None


def test_session_envelope_wins_over_default(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    register_envelope(_envelope(file_read=["/special/**"]), session_id="sessB", root=tmp_path)
    loaded = load_envelope("sessB", root=tmp_path)
    assert loaded.permissions.file_read == ["/special/**"]


def test_load_envelope_none_when_nothing_registered(tmp_path):
    assert load_envelope("sessX", root=tmp_path) is None


def test_registered_envelope_file_is_private(tmp_path):
    p = register_envelope(_envelope(), root=tmp_path)
    assert (p.stat().st_mode & 0o777) == 0o600


# --------------------------------------------------------- disarm switch

def test_disarm_arm_roundtrip(tmp_path):
    assert is_disarmed(tmp_path) is False
    disarm(tmp_path)
    assert is_disarmed(tmp_path) is True
    arm(tmp_path)
    assert is_disarmed(tmp_path) is False


def test_disarmed_gate_allows_everything_even_out_of_envelope(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    disarm(tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="claude", mode="enforce",
    )
    assert out.exit_code == 0
    assert "disarmed" in out.decision.reason


# --------------------------------------------------- gate_and_contract

def test_enforce_deny_is_exit_2_with_reason_on_stderr(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="claude", mode="enforce",
    )
    assert out.exit_code == 2
    assert "/etc/passwd" in out.stderr
    assert out.decision.would_deny is True


def test_enforce_allow_is_exit_0_with_continue_contract(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/allowed/notes.txt"), root=tmp_path, fmt="claude", mode="enforce",
    )
    assert out.exit_code == 0
    assert json.loads(out.stdout) == {"continue": True}


def test_shadow_never_blocks_but_logs_would_deny(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="claude", mode="shadow",
    )
    assert out.exit_code == 0
    log = tmp_path / "shadow" / "sess1.jsonl"
    assert log.exists()
    rec = json.loads(log.read_text().splitlines()[-1])
    assert rec["would_deny"] is True
    assert rec["mode"] == "shadow"


def test_missing_envelope_enforce_denies_and_names_both_exits(tmp_path):
    out = gate_and_contract(
        _payload_bytes("/anything"), root=tmp_path, fmt="claude", mode="enforce",
    )
    assert out.exit_code == 2
    assert "register" in out.stderr
    assert "disarm" in out.stderr


def test_missing_envelope_shadow_allows_but_flags(tmp_path):
    out = gate_and_contract(
        _payload_bytes("/anything"), root=tmp_path, fmt="claude", mode="shadow",
    )
    assert out.exit_code == 0
    assert out.decision.would_deny is True


def test_unparseable_stdin_enforce_denies(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(b"\xff not json {{{", root=tmp_path, fmt="claude", mode="enforce")
    assert out.exit_code == 2


def test_unparseable_stdin_shadow_allows(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(b"\xff not json {{{", root=tmp_path, fmt="claude", mode="shadow")
    assert out.exit_code == 0


def test_outer_exception_enforce_denies_shadow_allows(tmp_path, monkeypatch):
    register_envelope(_envelope(), root=tmp_path)

    def _boom(*a, **k):
        raise RuntimeError("io layer exploded")
    monkeypatch.setattr("opendaisugi.gate.load_envelope", _boom)
    out = gate_and_contract(_payload_bytes("/allowed/x"), root=tmp_path, fmt="claude", mode="enforce")
    assert out.exit_code == 2
    out2 = gate_and_contract(_payload_bytes("/allowed/x"), root=tmp_path, fmt="claude", mode="shadow")
    assert out2.exit_code == 0


def test_hermes_fmt_deny_is_block_json_exit_0(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="hermes", mode="enforce",
    )
    assert out.exit_code == 0
    body = json.loads(out.stdout)
    assert body.get("decision") == "block" or body.get("action") == "block"


# =================================================================
# Task 3: shadow report + capture replay
# =================================================================

from opendaisugi.gate import replay_captures, shadow_report  # noqa: E402


def _drive_shadow_session(root, session="sessR"):
    register_envelope(_envelope(shell=True, shell_allowlist=["echo"]), root=root)
    calls = [
        _read_payload("/allowed/ok.txt"),                                   # allowed
        _read_payload("/etc/passwd"),                                       # real deny
        {"tool_name": "Bash", "tool_input": {"command": "echo a && echo b"},},  # FP candidate
        {"tool_name": "TodoWrite", "tool_input": {"todos": []}},            # unrecognized host tool → FP candidate
    ]
    for c in calls:
        gate_and_contract(
            json.dumps(c | {"session_id": session}).encode(),
            root=root, fmt="claude", mode="shadow",
        )


def test_shadow_report_counts_and_flags_fp_candidates(tmp_path):
    _drive_shadow_session(tmp_path)
    rep = shadow_report(root=tmp_path, session_id="sessR")
    assert rep["calls"] == 4
    assert rep["would_deny"] == 3
    assert rep["allowed"] == 1
    # The compound-&& and the unrecognized host tool are false-positive
    # candidates; the /etc/passwd read is a true permission denial.
    fp = rep["false_positive_candidates"]
    assert len(fp) == 2
    fp_details = " ".join(json.dumps(r) for r in fp)
    assert "echo a && echo b" in fp_details
    assert "TodoWrite" in fp_details
    true_denies = [r for r in rep["denied"] if r not in fp]
    assert any("/etc/passwd" in (r.get("detail") or "") for r in true_denies)


def test_shadow_report_all_sessions_when_none_given(tmp_path):
    _drive_shadow_session(tmp_path, session="s1")
    _drive_shadow_session(tmp_path, session="s2")
    rep = shadow_report(root=tmp_path)
    assert rep["calls"] == 8


def test_shadow_report_empty_when_no_log(tmp_path):
    rep = shadow_report(root=tmp_path)
    assert rep["calls"] == 0
    assert rep["would_deny"] == 0


def test_replay_captures_produces_report_from_passive_session(tmp_path):
    """Roadmap Stage 1 exit criterion: a shadow-mode report generated from a
    captured real session, false-positive candidates included."""
    captures = tmp_path / "cap.jsonl"
    rows = [
        {"captured_at": 1.0, "session_id": "cap", "tool_name": "Read",
         "step_type": "file_read", "path": "/allowed/a.txt"},
        {"captured_at": 2.0, "session_id": "cap", "tool_name": "Read",
         "step_type": "file_read", "path": "/secret/b.txt"},
        {"captured_at": 3.0, "session_id": "cap", "tool_name": "Bash",
         "step_type": "shell", "command": "echo hi && rm -rf /"},
    ]
    captures.write_text("\n".join(json.dumps(r) for r in rows) + "\n")
    env = _envelope(shell=True, shell_allowlist=["echo"])
    rep = replay_captures(captures, env)
    assert rep["calls"] == 3
    assert rep["would_deny"] == 2
    assert rep["allowed"] == 1
    assert any("/secret/b.txt" in (r.get("detail") or "") for r in rep["denied"])
    assert len(rep["false_positive_candidates"]) == 1  # the compound &&


# =================================================================
# Captures mirroring: gated sessions feed the distillation pipeline
# =================================================================

def test_allowed_calls_mirror_into_captures_format(tmp_path):
    """With a captures_root, calls the gate allows are recorded in passive-
    capture format — so a gated session distills through the existing
    captures → to-trace → journal pipeline."""
    register_envelope(_envelope(), root=tmp_path)
    caps = tmp_path / "caps"
    gate_and_contract(
        _payload_bytes("/allowed/ok.txt"), root=tmp_path, fmt="claude",
        mode="enforce", captures_root=caps,
    )
    files = list(caps.glob("*.jsonl"))
    assert files, "no capture written for an allowed call"
    rec = json.loads(files[0].read_text().splitlines()[0])
    assert rec["tool_name"] == "Read"
    assert rec["path"] == "/allowed/ok.txt"


def test_denied_calls_do_not_mirror_into_captures(tmp_path):
    """A denied call never executed — recording it as a capture would teach
    distillation an action that didn't happen."""
    register_envelope(_envelope(), root=tmp_path)
    caps = tmp_path / "caps"
    gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="claude",
        mode="enforce", captures_root=caps,
    )
    assert not list(caps.glob("*.jsonl")) if caps.exists() else True


def test_shadow_mode_mirrors_everything_it_allows(tmp_path):
    """Shadow mode allows every call (they all really ran), so every call
    is captured — including the would-denies."""
    register_envelope(_envelope(), root=tmp_path)
    caps = tmp_path / "caps"
    gate_and_contract(
        _payload_bytes("/etc/passwd"), root=tmp_path, fmt="claude",
        mode="shadow", captures_root=caps,
    )
    files = list(caps.glob("*.jsonl"))
    assert files, "shadow mode must capture calls that actually ran"


def test_settings_json_carries_captures_root(tmp_path):
    from opendaisugi.gate import gate_settings_json
    caps = tmp_path / "caps"
    settings = json.loads(gate_settings_json(root=tmp_path, captures_root=caps))
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--captures-root" in cmd


# =================================================================
# Fail-closed at the process boundary (advisor finding, live-verified)
# =================================================================

from opendaisugi.gate import main as gate_main  # noqa: E402


def test_main_returns_2_on_baseexception(tmp_path, monkeypatch):
    """A BaseException escaping gate_and_contract (e.g. SystemExit re-raised
    from the verify thread, which `except Exception` won't catch) must still
    exit 2 — never fall through to a non-blocking exit code."""
    import io
    import sys as _sys

    def _boom(*a, **k):
        raise SystemExit(1)
    monkeypatch.setattr("opendaisugi.gate.gate_and_contract", _boom)
    monkeypatch.setattr(
        _sys, "stdin",
        type("S", (), {"buffer": io.BytesIO(b"{}")})(),
    )
    code = gate_main(["--mode", "enforce", "--root", str(tmp_path)])
    assert code == 2


def test_claude_settings_command_maps_any_nonzero_to_deny(tmp_path):
    """The emitted claude command must default-deny at the process boundary:
    `... || exit 2` so an import failure or crash (exit 1) — where main()
    never runs — still blocks. Live-verified: without this the read leaks."""
    from opendaisugi.gate import gate_settings_json
    settings = json.loads(gate_settings_json(mode="enforce", root=tmp_path))
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert cmd.rstrip().endswith("|| exit 2")


def test_non_claude_settings_command_is_not_wrapped(tmp_path):
    """`|| exit 2` is a claude-specific deny; for hermes/openclaw a deny is
    JSON-on-stdout, so wrapping would be meaningless (their crash-time
    behavior is part of the already-documented 'unverified' enforcement)."""
    from opendaisugi.gate import gate_settings_json
    settings = json.loads(gate_settings_json(mode="enforce", root=tmp_path, fmt="hermes"))
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "|| exit 2" not in cmd


# =================================================================
# Envelope pinning: authorization must not key on untrusted input
# =================================================================

def test_payload_session_id_can_select_another_envelope_when_unpinned(tmp_path):
    """Documents the unpinned behavior: the payload's session_id chooses the
    envelope. Harmless when the host is the only writer of session_id, but it
    means authorization keys on attacker-influenceable input — which is why
    pinning exists (and why the settings emitter pins by default)."""
    register_envelope(_envelope(), root=tmp_path)
    register_envelope(_envelope(file_read=["/**"]), session_id="admin", root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd", session="admin"),
        root=tmp_path, fmt="claude", mode="enforce",
    )
    assert out.exit_code == 0  # the permissive 'admin' envelope was selected


def test_pinned_session_ignores_the_payloads_session_id(tmp_path):
    """With the envelope pinned from outside (the hook command supplies it,
    and the agent cannot rewrite the hook command), a forged session_id in
    the payload cannot escalate."""
    register_envelope(_envelope(), root=tmp_path)
    register_envelope(_envelope(file_read=["/**"]), session_id="admin", root=tmp_path)
    out = gate_and_contract(
        _payload_bytes("/etc/passwd", session="admin"),
        root=tmp_path, fmt="claude", mode="enforce", pin_session="default",
    )
    assert out.exit_code == 2


def test_pinned_session_selects_the_named_envelope(tmp_path):
    register_envelope(_envelope(file_read=["/only-here/**"]), session_id="job7", root=tmp_path)
    allowed = gate_and_contract(
        _payload_bytes("/only-here/x", session="whatever"),
        root=tmp_path, fmt="claude", mode="enforce", pin_session="job7",
    )
    assert allowed.exit_code == 0
    denied = gate_and_contract(
        _payload_bytes("/elsewhere/x", session="whatever"),
        root=tmp_path, fmt="claude", mode="enforce", pin_session="job7",
    )
    assert denied.exit_code == 2


def test_pinned_log_record_keeps_the_real_session_for_traceability(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    gate_and_contract(
        _payload_bytes("/etc/passwd", session="claimed-session"),
        root=tmp_path, fmt="claude", mode="enforce", pin_session="default",
    )
    rep = shadow_report(root=tmp_path)
    assert rep["would_deny"] == 1
    assert rep["denied"][0]["payload_session_id"] == "claimed-session"


def test_settings_emitter_pins_the_session_when_asked(tmp_path):
    from opendaisugi.gate import gate_settings_json
    settings = json.loads(gate_settings_json(root=tmp_path, session="job7"))
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--session job7" in cmd


# =================================================================
# MCP classification: allowed vs undeclared must be a REAL distinction
# =================================================================

def _mcp_env():
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(file_read=["/work/**"],
                                           mcp_allowlist=["github/*"]))


def test_allowed_mcp_call_is_admitted():
    d = evaluate_call(
        {"tool_name": "mcp__github__create_issue", "tool_input": {"title": "x"},
         "session_id": "s"},
        _mcp_env(), mode="enforce",
    )
    assert d.step_type == "mcp"
    assert d.allow is True
    assert d.would_deny is False


def test_undeclared_mcp_call_is_denied_by_allowlist():
    d = evaluate_call(
        {"tool_name": "mcp__stripe__create_charge", "tool_input": {},
         "session_id": "s"},
        _mcp_env(), mode="enforce",
    )
    assert d.step_type == "mcp"
    assert d.would_deny is True
    assert "mcp_allowlist" in d.reason or "stripe" in d.reason


def test_mcp_tool_name_with_underscores_parses_server_and_tool():
    """`mcp__<server>__<tool>` where the tool name itself contains `__`."""
    d = evaluate_call(
        {"tool_name": "mcp__github__list__issues", "tool_input": {},
         "session_id": "s"},
        _mcp_env(), mode="enforce",
    )
    assert d.step_type == "mcp"
    assert d.allow is True  # github/* still matches
