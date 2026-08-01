"""`daisugi status` is where a persisted setting becomes visible again.

The opt-in is written once — by `install`, or by hand in config.yaml — and then
silently governs every later `hook auto-tend` run from cron. A setting nobody
can see is a setting nobody can debug, and the failure it produces (every
compound command denied because the bash grammar is missing) looks exactly like
the problem the operator turned it on to solve.
"""

from __future__ import annotations

from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import Config, save_config
from opendaisugi.onboarding import gather_status

runner = CliRunner()


def test_status_reports_the_opt_in_as_off_by_default(tmp_path: Path):
    assert gather_status(tmp_path).shell_decomposition_enabled is False


def test_status_reads_the_opt_in_from_config(tmp_path: Path):
    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    rep = gather_status(tmp_path)
    assert rep.shell_decomposition_enabled is True
    assert rep.shell_decomposition_ready is rep.shell_grammar_installed


def test_status_is_not_ready_when_the_grammar_is_missing(tmp_path: Path, monkeypatch):
    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: False)
    rep = gather_status(tmp_path)
    assert rep.shell_decomposition_enabled is True
    assert rep.shell_decomposition_ready is False


def test_status_output_names_the_missing_grammar(tmp_path: Path, monkeypatch):
    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: False)
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "opendaisugi[shell]" in res.output


def test_status_json_carries_the_setting(tmp_path: Path):
    import json

    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    assert json.loads(res.output)["shell_decomposition_enabled"] is True
