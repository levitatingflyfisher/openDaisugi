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


def test_install_gate_points_at_the_envelope_it_does_not_write(tmp_path):
    """`install --gate` wires the hook but never registers an envelope, and a
    gate with no envelope denies everything in enforce mode. Say where to get one."""
    res = runner.invoke(app, ["install", "--gate", "--dry-run", "--runtime", "claude"])
    assert res.exit_code == 0, res.output
    assert "daisugi gate init" in res.output
