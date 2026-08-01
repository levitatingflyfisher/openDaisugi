"""`daisugi hook auto-tend` honors the background-distillation consent gate."""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import Config, save_config

runner = CliRunner()


def test_auto_tend_skips_without_consent(tmp_path):
    # No config → unasked → the background tend must not run.
    res = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "background distillation is off" in res.output.lower()
    assert "converted" not in res.output  # never reached the capture→trace loop


def test_auto_tend_skips_when_declined(tmp_path):
    save_config(Config(auto_tend=False), tmp_path / "config.yaml")
    res = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "background distillation is off" in res.output.lower()


def test_auto_tend_runs_when_consented(tmp_path):
    # Consented → the gate lets it through; with no captured sessions it reports
    # a clean zero-conversion run rather than the "off" skip message.
    save_config(Config(auto_tend=True), tmp_path / "config.yaml")
    (tmp_path / "captures").mkdir()
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(tmp_path),
            "--captures-root",
            str(tmp_path / "captures"),
        ],
    )
    assert res.exit_code == 0, res.output
    assert "background distillation is off" not in res.output.lower()
    assert "converted 0 sessions" in res.output.lower()


def test_auto_tend_force_bypasses_the_gate(tmp_path):
    # --force tends once even with no consent (a manual, deliberate run).
    (tmp_path / "captures").mkdir()
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(tmp_path),
            "--captures-root",
            str(tmp_path / "captures"),
            "--force",
        ],
    )
    assert res.exit_code == 0, res.output
    assert "background distillation is off" not in res.output.lower()
