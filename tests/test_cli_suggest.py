"""Hidden commands still work and are still suggested on a typo.

Click's "Did you mean" suggestion is built from `list_commands`, which
includes hidden ones — this is what makes hiding safe: nothing that used to
work stops working, and a typo of a hidden name still finds it.
"""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_hidden_group_still_runs():
    res = runner.invoke(app, ["lora", "--help"])
    assert res.exit_code == 0, res.output


def test_hidden_command_still_runs():
    res = runner.invoke(app, ["viz", "--help"])
    assert res.exit_code == 0, res.output


def test_typo_of_a_hidden_command_is_suggested():
    res = runner.invoke(app, ["relaese"])
    assert res.exit_code != 0
    assert "release" in res.output


def test_typo_of_a_hidden_group_is_suggested():
    res = runner.invoke(app, ["registri"])
    assert res.exit_code != 0
    assert "registry" in res.output
