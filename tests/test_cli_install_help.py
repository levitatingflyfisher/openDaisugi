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
