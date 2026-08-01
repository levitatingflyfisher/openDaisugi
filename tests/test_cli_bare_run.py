"""The bare run is short, shows examples, and points at help --all.

Command presence/hidden-ness is asserted via Click introspection
(`get_command(app)`), not substring-in-`--help` output — FORCE_COLOR=3 on
this box wraps rendered `--help` text with ANSI codes and Rich word-wrap that
can split a name across lines. The bare-run text itself is plain `console.say`
output (no Rich rendering), so substring checks on it are safe.
"""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()

_VISIBLE_TOP_LEVEL = {
    "start",
    "status",
    "dashboard",
    "orchestrate",
    "install",
    "config",
    "gate",
    "pathways",
    "journal",
    "help",
}


def test_bare_run_is_short_and_actionable():
    res = runner.invoke(app, [])
    assert res.exit_code == 0, res.output
    lines = [ln for ln in res.output.splitlines() if ln.strip()]
    assert len(lines) <= 25, res.output
    assert "daisugi start" in res.output
    assert "daisugi orchestrate" in res.output
    assert "daisugi help --all" in res.output
    assert "╭" not in res.output and "│" not in res.output


def test_help_with_no_flag_matches_bare_run():
    res_bare = runner.invoke(app, [])
    res_help = runner.invoke(app, ["help"])
    assert res_help.exit_code == 0, res_help.output
    assert res_help.output == res_bare.output


def test_help_all_lists_every_command():
    res = runner.invoke(app, ["help", "--all"])
    assert res.exit_code == 0, res.output
    for name in (
        "start",
        "status",
        "orchestrate",
        "install",
        "config",
        "gate",
        "pathways",
        "journal",
        "onboard",
        "tend",
        "route",
        "viz",
        "metrics",
        "lora",
        "release",
        "batch",
        "conformance",
        "registry",
        "hook",
        "mcp",
        "tiers",
        "gardener",
        "gateway",
        "models",
    ):
        assert name in res.output, name


def test_help_all_survives_quiet():
    res = runner.invoke(app, ["-q", "help", "--all"])
    assert res.exit_code == 0, res.output
    assert "start" in res.output


def test_top_level_visible_commands_are_exactly_ten():
    """SF-5 (spec §8.2): exactly the ten named commands are visible; everything
    else (onboard, run, verify, tend, setup, and every operational group) is hidden."""
    from typer.main import get_command

    cmd = get_command(app)
    visible = {name for name, c in cmd.commands.items() if not getattr(c, "hidden", False)}
    assert visible == _VISIBLE_TOP_LEVEL
