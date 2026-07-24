"""Tests for the ``daisugi gate`` CLI surface (ADR-0007).

The one contract that matters most here: ``gate check --mode enforce``
exits 2 with the reason on stderr when the call is denied — that exit
code IS the deny on the Claude Code hook path, pinned empirically by
tests/test_hook_gate_contract.py.
"""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gate import gate_settings_json
from opendaisugi.gate import main as gate_main
from opendaisugi.models import Envelope, Permission

runner = CliRunner()


def _envelope_file(tmp_path, **perm_kwargs):
    env = Envelope(
        generated_by="test",
        task="cli gate test",
        permissions=Permission(file_read=["/allowed/**"], **perm_kwargs),
    )
    p = tmp_path / "env.json"
    p.write_text(env.model_dump_json())
    return p


def _payload(path="/allowed/x.txt", session="cli-sess"):
    return json.dumps({
        "tool_name": "Read",
        "tool_input": {"file_path": path},
        "session_id": session,
    })


def _register(tmp_path, root):
    env_file = _envelope_file(tmp_path)
    res = runner.invoke(app, [
        "gate", "register", str(env_file), "--root", str(root),
    ])
    assert res.exit_code == 0, res.output
    return env_file


def test_gate_register_and_status(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    res = runner.invoke(app, ["gate", "status", "--root", str(root)])
    assert res.exit_code == 0
    assert "default" in res.output
    assert "armed" in res.output.lower()


def test_gate_check_enforce_denies_with_exit_2(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    res = runner.invoke(app, [
        "gate", "check", "--mode", "enforce", "--root", str(root),
    ], input=_payload("/etc/passwd"))
    assert res.exit_code == 2


def test_gate_check_enforce_allows_in_envelope(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    res = runner.invoke(app, [
        "gate", "check", "--mode", "enforce", "--root", str(root),
    ], input=_payload("/allowed/x.txt"))
    assert res.exit_code == 0
    assert json.loads(res.stdout.strip()) == {"continue": True}


def test_gate_check_shadow_never_exits_nonzero(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    res = runner.invoke(app, [
        "gate", "check", "--root", str(root),
    ], input=_payload("/etc/passwd"))
    assert res.exit_code == 0


def test_gate_disarm_and_arm_roundtrip_via_cli(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    assert runner.invoke(app, ["gate", "disarm", "--root", str(root)]).exit_code == 0
    res = runner.invoke(app, [
        "gate", "check", "--mode", "enforce", "--root", str(root),
    ], input=_payload("/etc/passwd"))
    assert res.exit_code == 0  # disarmed gate allows
    assert runner.invoke(app, ["gate", "arm", "--root", str(root)]).exit_code == 0
    res = runner.invoke(app, [
        "gate", "check", "--mode", "enforce", "--root", str(root),
    ], input=_payload("/etc/passwd"))
    assert res.exit_code == 2


def test_gate_report_shows_would_denies(tmp_path):
    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    runner.invoke(app, ["gate", "check", "--root", str(root)],
                  input=_payload("/etc/passwd"))
    res = runner.invoke(app, ["gate", "report", "--root", str(root), "--json"])
    assert res.exit_code == 0
    rep = json.loads(res.stdout)
    assert rep["would_deny"] == 1


def test_gate_replay_reports_from_captures(tmp_path):
    env_file = _envelope_file(tmp_path)
    captures = tmp_path / "cap.jsonl"
    captures.write_text(json.dumps({
        "captured_at": 1.0, "session_id": "s", "tool_name": "Read",
        "step_type": "file_read", "path": "/nope/x",
    }) + "\n")
    res = runner.invoke(app, [
        "gate", "replay", str(captures), "--envelope", str(env_file), "--json",
    ])
    assert res.exit_code == 0
    rep = json.loads(res.stdout)
    assert rep["calls"] == 1
    assert rep["would_deny"] == 1


def test_gate_settings_prints_hooks_json(tmp_path):
    res = runner.invoke(app, [
        "gate", "settings", "--root", str(tmp_path), "--enforce",
    ])
    assert res.exit_code == 0
    settings = json.loads(res.stdout)
    entry = settings["hooks"]["PreToolUse"][0]
    assert entry["matcher"] == "*"
    cmd = entry["hooks"][0]["command"]
    assert "--mode enforce" in cmd
    assert "opendaisugi.gate" in cmd


def test_gate_settings_json_helper_defaults_to_shadow(tmp_path):
    settings = json.loads(gate_settings_json(root=tmp_path))
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--mode shadow" in cmd


def test_lean_entry_main_enforce_denies(tmp_path, monkeypatch, capsys):
    """python -m opendaisugi.gate — the lean hook entry (no typer import)."""
    import io
    import sys

    root = tmp_path / "gateroot"
    _register(tmp_path, root)
    monkeypatch.setattr(
        sys, "stdin",
        type("S", (), {"buffer": io.BytesIO(_payload("/etc/passwd").encode())})(),
    )
    code = gate_main(["--mode", "enforce", "--root", str(root)])
    assert code == 2
    assert "DENIED" in capsys.readouterr().err


def test_gate_settings_session_pin_flows_into_command(tmp_path):
    res = runner.invoke(app, [
        "gate", "settings", "--root", str(tmp_path), "--session", "job7",
    ])
    assert res.exit_code == 0
    cmd = json.loads(res.stdout)["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--session job7" in cmd


def test_gate_audit_reports_attack_and_fp_rates():
    res = runner.invoke(app, ["gate", "audit", "--json"])
    assert res.exit_code == 0
    rep = json.loads(res.stdout)
    assert rep["attack_denial_rate"] == 1.0
    assert rep["unexpected_allowed_attacks"] == []
    assert "arms" in rep


def test_gate_audit_text_summary():
    res = runner.invoke(app, ["gate", "audit"])
    assert res.exit_code == 0
    assert "attack" in res.output.lower()
    assert "13/13" in res.output or "1.00" in res.output
