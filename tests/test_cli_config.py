# tests/test_cli_config.py
"""The `config` command prints resolved settings; --json gives the same as an object."""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_config_help():
    assert runner.invoke(app, ["config", "--help"]).exit_code == 0


def test_config_prints_sources(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    (tmp_path / ".opendaisugi" / "config.yaml").write_text("gate_mode: enforce\nbanana: 1\n")
    res = runner.invoke(app, ["config"])
    assert res.exit_code == 0, res.output
    assert "gate_mode" in res.output and "(file)" in res.output
    assert "(default)" in res.output
    assert "banana" in res.output and "ignored" in res.output


def test_config_json(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    res = runner.invoke(app, ["config", "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["gate_mode"]["source"] == "default"
    assert "path" in body["_meta"]


def test_config_reports_a_cwd_scoped_start_hook_not_default(tmp_path, monkeypatch):
    """A `daisugi start --enforce` hook has no counterpart under home, so a
    home-only read would under-report an active enforce gate as `default`."""
    home = tmp_path / "home"
    proj = tmp_path / "proj"
    monkeypatch.setattr("pathlib.Path.home", lambda: home)
    proj.mkdir()
    monkeypatch.chdir(proj)
    settings = proj / ".claude" / "settings.json"
    settings.parent.mkdir(parents=True)
    settings.write_text(json.dumps({"hooks": {"PreToolUse": [{"hooks": [
        {"type": "command", "command": "py -m opendaisugi.gate_client --mode enforce"}]}]}}))
    res = runner.invoke(app, ["config", "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["gate_mode (resolved)"]["value"] == "enforce"
    assert body["gate_mode (resolved)"]["source"] == "project"
