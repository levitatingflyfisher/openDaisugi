"""CLI: `daisugi setup` (hardware → recommendation → qualify/wire) + status local-model line."""

import json

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_setup_help():
    res = runner.invoke(app, ["setup", "--help"])
    assert res.exit_code == 0
    assert "setup" in res.output.lower()


def test_setup_detects_and_recommends(tmp_path):
    res = runner.invoke(app, ["setup", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    low = res.output.lower()
    assert "llamafile" in low                       # recommends the onefile runtime
    assert "qualif" in low                           # provisional-until-qualified guidance
    assert ("ram" in low or "vram" in low or "budget" in low)  # reported the hardware budget


def test_setup_json(tmp_path):
    res = runner.invoke(app, ["setup", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    data = json.loads(res.output)
    assert "hardware" in data and "recommendation" in data
    assert data["recommendation"]["provisional"] is True


def test_setup_endpoint_requires_model(tmp_path):
    res = runner.invoke(
        app, ["setup", "--data-dir", str(tmp_path), "--endpoint", "http://localhost:8080/v1"]
    )
    assert res.exit_code != 0
    assert "model" in res.output.lower()


def test_status_shows_local_model_section(tmp_path):
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    low = res.output.lower()
    assert "local model" in low or "hardware" in low
