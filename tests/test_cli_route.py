"""CLI wiring for `daisugi route` — per-task model-routing advice."""

import json

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_route_help():
    res = runner.invoke(app, ["route", "--help"])
    assert res.exit_code == 0
    assert "route" in res.output.lower()


def test_route_easy_task_recommends_cheap(tmp_path):
    res = runner.invoke(
        app, ["route", "print the current date", "--data-dir", str(tmp_path)]
    )
    assert res.exit_code == 0, res.output
    assert "tier1-cheap" in res.output


def test_route_hard_task_recommends_frontier_and_advisor(tmp_path):
    res = runner.invoke(
        app,
        [
            "route",
            "architect a distributed consensus algorithm and prove safety under partition",
            "--data-dir", str(tmp_path),
        ],
    )
    assert res.exit_code == 0, res.output
    assert "tier2-frontier" in res.output
    assert "advisor" in res.output.lower()


def test_route_hard_task_codex_harness_omits_advisor(tmp_path):
    # On Codex (no Anthropic advisor tool), the hard-task route must still go
    # to the frontier but must NOT dangle the advisor-tool pairing.
    res = runner.invoke(
        app,
        [
            "route",
            "architect a distributed consensus algorithm and prove safety under partition",
            "--data-dir", str(tmp_path),
            "--harness", "codex",
        ],
    )
    assert res.exit_code == 0, res.output
    assert "tier2-frontier" in res.output
    assert "advisor" not in res.output.lower()


def test_route_json(tmp_path):
    res = runner.invoke(
        app, ["route", "print the date", "--data-dir", str(tmp_path), "--json"]
    )
    assert res.exit_code == 0, res.output
    data = json.loads(res.output)
    assert data["tier"] == "tier1-cheap"
    assert "model" in data and "reason" in data
