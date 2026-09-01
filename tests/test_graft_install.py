"""The graft installer: install, status and remove a rule file, and the
refusal beside an unknown PreToolUse hook (GR-3)."""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi import delegate, graft_install
from opendaisugi.cli import app

GATE = "python -m opendaisugi.gate --mode audit --format claude"


def _settings(path: Path, *commands: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    hooks = [{"type": "command", "command": c} for c in commands]
    path.write_text(json.dumps({"hooks": {"PreToolUse": [{"matcher": "*", "hooks": hooks}]}}))


def test_install_writes_an_audit_rule_the_gate_reads(tmp_path):
    root = tmp_path / "data" / "gate"
    got = graft_install.install(root, home=tmp_path / "h", cwd=tmp_path / "c")
    assert isinstance(got, graft_install.Installed)
    rules, bad = delegate.load_rules(root)
    assert bad == [] and len(rules) == 1
    r = rules[0]
    assert (r.id, r.version, r.state, r.min_lines, r.allow_remote) == (
        "big-read",
        1,
        "audit",
        350,
        False,
    )


def test_install_again_bumps_the_version(tmp_path):
    root = tmp_path / "gate"
    graft_install.install(root, home=tmp_path, cwd=tmp_path)
    got = graft_install.install(root, home=tmp_path, cwd=tmp_path, state="active", lines=40)
    assert got.version == 2
    r = delegate.acting_rule(root)
    assert (r.version, r.state, r.min_lines) == (2, "active", 40)


def test_known_hooks_do_not_block(tmp_path):
    home = tmp_path / "h"
    _settings(home / ".claude" / "settings.json", GATE, "daisugi hook record --event pre")
    got = graft_install.install(tmp_path / "gate", home=home, cwd=tmp_path)
    assert isinstance(got, graft_install.Installed)


def test_unknown_hook_refuses_and_writes_nothing(tmp_path):
    home = tmp_path / "h"
    _settings(home / ".codex" / "hooks.json", GATE, "rtk rewrite")
    root = tmp_path / "gate"
    got = graft_install.install(root, home=home, cwd=tmp_path)
    assert isinstance(got, graft_install.Refused)
    assert '"rtk rewrite"' in got.line and "hooks.json" in got.line
    assert not (root / "grafts").exists()


def test_project_settings_are_checked(tmp_path):
    _settings(tmp_path / ".claude" / "settings.local.json", "shunt")
    got = graft_install.install(tmp_path / "gate", home=tmp_path / "h", cwd=tmp_path)
    assert isinstance(got, graft_install.Refused)


def test_unreadable_settings_refuse(tmp_path):
    p = tmp_path / "h" / ".claude" / "settings.json"
    p.parent.mkdir(parents=True)
    p.write_text("{")
    got = graft_install.install(tmp_path / "gate", home=tmp_path / "h", cwd=tmp_path)
    assert isinstance(got, graft_install.Refused) and "not readable JSON" in got.line


def test_cli_install_status_remove(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path / "h"))
    monkeypatch.chdir(tmp_path)
    runner = CliRunner()
    dd = str(tmp_path / "d")
    out = runner.invoke(app, ["graft", "install", "--data-dir", dd, "--state", "active"])
    assert out.exit_code == 0, out.output
    out = runner.invoke(app, ["graft", "status", "--data-dir", dd, "--json"])
    doc = json.loads(out.output)
    assert doc["rules"][0]["in_force"] is True and doc["rival_hooks"] == []
    out = runner.invoke(app, ["graft", "remove", "--data-dir", dd])
    assert (
        out.exit_code == 0 and not (tmp_path / "d" / "gate" / "grafts" / "big-read.json").exists()
    )
    out = runner.invoke(app, ["graft", "install", "--data-dir", dd, "--state", "on"])
    assert out.exit_code == 2
