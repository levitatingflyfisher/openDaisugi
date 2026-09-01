"""Every read command accepts --json. Drop one and this fails.

FORCE_COLOR=3 is set in this box's ambient env, and Rich honors FORCE_COLOR
over NO_COLOR — so a substring check like ``"--json" in res.output`` is not
reliable here (Rich's own help-panel rendering can wrap/style the text in
ways that still contain the substring today, but the check is fragile and
the corrections call it out explicitly). Assert flag presence via Click
param introspection instead: walk the real Click command tree and check
``"--json" in param.opts``.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
import typer.main
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()

READ_COMMANDS = [
    ["status"],
    ["config"],
    ["modules"],
    ["dashboard"],
    ["models"],
    ["gate", "status"],
    ["gate", "report"],
    ["gate", "audit"],
    ["hook", "list"],
    ["pathways", "list"],
    ["pathways", "show"],
    ["pathways", "stats"],
    ["journal", "stats"],
    ["journal", "search"],
    ["tiers", "stats"],
    ["gardener", "status"],
    ["registry", "status"],
    ["coppice", "backends"],
]


def _click_params(cmd_path: list[str]) -> set[str]:
    """The set of option strings (e.g. {'--json', '--help'}) on a subcommand."""
    cmd = typer.main.get_command(app)
    for name in cmd_path:
        cmd = cmd.commands[name]
    opts: set[str] = set()
    for p in cmd.params:
        opts |= set(p.opts)
    return opts


@pytest.mark.parametrize("cmd", READ_COMMANDS, ids=[" ".join(c) for c in READ_COMMANDS])
def test_read_command_accepts_json(cmd):
    assert "--json" in _click_params(cmd), f"{' '.join(cmd)} has no --json"


def test_gate_status_json_has_mode(tmp_path):
    res = runner.invoke(app, ["gate", "status", "--root", str(tmp_path / "gate"), "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["armed"] is True
    assert body["mode"] in ("shadow", "enforce")
    assert body["envelopes"] == []


def test_gate_status_human_shows_mode(tmp_path, monkeypatch):
    # gate_status_cmd reads both ~/.claude/settings.json and
    # ./.claude/settings.json for an installed hook's --mode (plan 5's fix 2:
    # the effective mode is the stricter of the two). Left unpatched, this
    # test's outcome depends on whatever hook (if any) happens to be
    # installed on the box running it -- a real box with an enforce-mode
    # hook flakes this assertion. Point both Path.home() and Path.cwd() at
    # empty, hookless fixture directories so the mode is purely a function
    # of the fresh --root.
    monkeypatch.setattr(Path, "home", lambda: tmp_path / "home")
    monkeypatch.chdir(tmp_path)
    res = runner.invoke(app, ["gate", "status", "--root", str(tmp_path / "gate")])
    assert res.exit_code == 0
    assert "mode: shadow" in res.output


def test_gate_status_reports_a_cwd_only_enforce_hook(tmp_path, monkeypatch):
    """`gate status` is THE command for "is the gate armed and in what mode" —
    it must not show shadow/config while a daisugi start --enforce hook (no
    global hook installed) is actually denying calls in this directory."""
    home = tmp_path / "home"
    proj = tmp_path / "proj"
    proj.mkdir()
    (proj / ".claude").mkdir()
    (proj / ".claude" / "settings.json").write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "py -m opendaisugi.gate_client --mode enforce --root /r",
                                }
                            ]
                        }
                    ]
                }
            }
        )
    )
    monkeypatch.setattr(Path, "home", lambda: home)
    monkeypatch.chdir(proj)
    res = runner.invoke(app, ["gate", "status", "--root", str(tmp_path / "gate"), "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["mode"] == "enforce"
    assert body["mode_source"] == "project"


def test_hook_list_json_empty(tmp_path):
    res = runner.invoke(app, ["hook", "list", "--captures-root", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    assert json.loads(res.output) == []


def test_pathways_list_json_empty(tmp_path):
    res = runner.invoke(app, ["pathways", "list", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    assert json.loads(res.output) == []


def test_gate_check_rejects_unknown_mode(tmp_path):
    res = runner.invoke(
        app, ["gate", "check", "--mode", "bogus", "--root", str(tmp_path)], input="{}"
    )
    assert res.exit_code != 0
