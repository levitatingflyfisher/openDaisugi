import re

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_install_help_lists_four_runtimes_no_session_start():
    res = runner.invoke(app, ["install", "--help"])
    assert res.exit_code == 0
    text = res.stdout
    assert "SessionStart" not in text
    for name in ("Claude", "Codex", "Hermes", "OpenClaw"):
        assert name in text


def test_install_warden_points_at_the_envelope_it_does_not_write(tmp_path):
    """`install` wires the hook but never registers an envelope, and a
    warden with no envelope denies everything in enforce mode. Say where to get one."""
    res = runner.invoke(app, ["install", "--dry-run", "--runtime", "claude"])
    assert res.exit_code == 0, res.output
    assert "daisugi warden init" in res.output


def test_install_help_has_no_warden_flag_and_names_enforce():
    text = runner.invoke(app, ["install", "--help"]).stdout
    assert re.search(r"--warden(?!way)", text) is None
    flat = " ".join(re.sub(r"[│╭╮╰╯─]", " ", text).split())
    assert "--enforce" in flat and "records them and allows" in flat
