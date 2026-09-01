from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.install import install_harness_extension, uninstall_harness_extension


def test_installs_extension_file(tmp_path):
    modified = install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    assert (dest / "index.ts").exists()
    assert modified  # something was written


def test_two_installs_are_idempotent_same_content(tmp_path):
    install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    before = (dest / "index.ts").read_text()
    second = install_harness_extension("pi", home=tmp_path)
    after = (dest / "index.ts").read_text()
    assert before == after
    assert second == []  # nothing changed the second time


def test_install_replaces_a_planted_symlink_instead_of_writing_through_it(tmp_path):
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    dest.mkdir(parents=True)
    victim = tmp_path / "victim.txt"
    victim.write_text("keep me")
    (dest / "index.ts").symlink_to(victim)
    install_harness_extension("pi", home=tmp_path)
    assert not (dest / "index.ts").is_symlink()
    assert victim.read_text() == "keep me"


def test_uninstall_removes_the_directory(tmp_path):
    install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    assert dest.exists()
    uninstall_harness_extension("pi", home=tmp_path)
    assert not dest.exists()


def test_uninstall_of_a_never_installed_harness_is_a_silent_no_op(tmp_path):
    assert uninstall_harness_extension("pi", home=tmp_path) == []


def test_cli_install_harness_pi_prints_the_honest_restart_and_mode_lines(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--yes"])
    assert result.exit_code == 0, result.output
    assert "Restart pi" in result.output or "/reload" in result.output
    assert "gate_mode: enforce" in result.output
    assert "will block" not in result.output  # false once no --mode travels on the wire
    assert (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()


def test_cli_install_unknown_harness_is_a_teaching_error(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    result = CliRunner().invoke(app, ["install", "--harness", "nope"])
    assert result.exit_code == 2
    assert "unknown harness" in result.output
    assert "pi" in result.output


def test_cli_install_harness_pi_uninstall(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    CliRunner().invoke(app, ["install", "--harness", "pi", "--yes"])
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--uninstall"])
    assert result.exit_code == 0, result.output
    assert not (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate").exists()


def test_cli_install_harness_pi_dry_run_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--dry-run"])
    assert result.exit_code == 0, result.output
    assert "would write" in result.output
    assert not (tmp_path / ".pi").exists()


def test_gate_check_format_help_names_pi():
    result = CliRunner().invoke(app, ["gate", "check", "--help"])
    assert result.exit_code == 0, result.output
    assert "pi" in result.output.split("--format", 1)[1][:200]


def test_cli_install_harness_pi_uninstall_dry_run_removes_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    CliRunner().invoke(app, ["install", "--harness", "pi", "--yes"])
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--uninstall", "--dry-run"])
    assert result.exit_code == 0, result.output
    assert "would remove" in result.output
    assert (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()
