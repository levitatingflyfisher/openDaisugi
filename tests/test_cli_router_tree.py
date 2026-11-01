"""The router command tree: one group, five verbs, and no old command left."""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_router_tree():
    out = runner.invoke(app, ["router", "--help"]).output
    for verb in ("serve", "report", "status", "label", "stop"):
        assert f" {verb} " in out
    assert (
        runner.invoke(app, ["gateway", "--help"]).exit_code != 0  # names-ok: old command
    )  # names-ok: the old command is gone
    assert runner.invoke(app, ["gateway-report", "--help"]).exit_code != 0  # names-ok: gone


def test_router_group_is_listed_on_the_top_level():
    assert "Router" in runner.invoke(app, ["--help"]).output


def test_install_takes_router_and_routing_mode(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / ".claude").mkdir()
    r = runner.invoke(app, ["install", "--router", "--routing-mode", "knn", "--dry-run", "--yes"])
    assert r.exit_code == 0, r.output
    assert "routing_mode" in r.output


def test_install_refuses_the_old_flags(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    for flag in ("--gateway", "--gate"):  # names-ok: the old flags are gone
        assert runner.invoke(app, ["install", flag, "--dry-run", "--yes"]).exit_code != 0
