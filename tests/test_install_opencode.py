"""daisugi install --harness opencode: copy the gate plugin into OpenCode's
global plugin directory, and take it out again.

Every test runs with HOME and XDG_CONFIG_HOME under tmp_path, so no test
touches a real OpenCode config.
"""

from __future__ import annotations

import importlib.resources as ir
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.install import (
    SUPPORTED_HARNESSES,
    install_harness_extension,
    opencode_plugin_path,
    uninstall_harness_extension,
)


@pytest.fixture(autouse=True)
def private_homes(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
    return tmp_path


def _default_path(home: Path) -> Path:
    return home / ".config" / "opencode" / "plugins" / "daisugi-gate.ts"


def _shipped() -> str:
    return (
        ir.files("opendaisugi")
        .joinpath("harness_opencode", "plugin", "daisugi-gate.ts")
        .read_text(encoding="utf-8")
    )


def test_opencode_is_a_supported_harness():
    assert "opencode" in SUPPORTED_HARNESSES


def test_the_plugin_path_follows_xdg_config_home():
    home = Path("/h")
    assert opencode_plugin_path(home, {}) == _default_path(home)
    xdg = opencode_plugin_path(home, {"XDG_CONFIG_HOME": "/x/cfg"})
    assert xdg == Path("/x/cfg/opencode/plugins/daisugi-gate.ts")
    # A relative XDG_CONFIG_HOME is not a config home.
    assert opencode_plugin_path(home, {"XDG_CONFIG_HOME": "cfg"}) == _default_path(home)


def test_install_copies_the_plugin_byte_for_byte(tmp_path):
    written = install_harness_extension("opencode", home=tmp_path)
    assert written == [_default_path(tmp_path)]
    assert _default_path(tmp_path).read_text(encoding="utf-8") == _shipped()


def test_install_writes_under_xdg_config_home_when_set(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    install_harness_extension("opencode", home=tmp_path)
    assert (tmp_path / "cfg" / "opencode" / "plugins" / "daisugi-gate.ts").exists()
    assert not _default_path(tmp_path).exists()


def test_install_is_idempotent(tmp_path):
    install_harness_extension("opencode", home=tmp_path)
    assert install_harness_extension("opencode", home=tmp_path) == []
    assert _default_path(tmp_path).read_text(encoding="utf-8") == _shipped()


def test_install_replaces_a_changed_plugin(tmp_path):
    install_harness_extension("opencode", home=tmp_path)
    _default_path(tmp_path).write_text("export default async () => ({})\n")
    assert install_harness_extension("opencode", home=tmp_path) == [_default_path(tmp_path)]
    assert _default_path(tmp_path).read_text(encoding="utf-8") == _shipped()


def test_install_never_writes_through_a_planted_symlink(tmp_path):
    target = tmp_path / "evil.txt"
    dest = _default_path(tmp_path)
    dest.parent.mkdir(parents=True)
    dest.symlink_to(target)
    install_harness_extension("opencode", home=tmp_path)
    assert not target.exists()
    assert not dest.is_symlink()


def test_install_leaves_opencode_json_alone(tmp_path):
    cfg = tmp_path / ".config" / "opencode" / "opencode.json"
    cfg.parent.mkdir(parents=True)
    cfg.write_text('{"plugin": []}\n')
    install_harness_extension("opencode", home=tmp_path)
    assert cfg.read_text() == '{"plugin": []}\n'


def test_uninstall_removes_only_the_plugin_file(tmp_path):
    install_harness_extension("opencode", home=tmp_path)
    other = _default_path(tmp_path).parent / "mine.ts"
    other.write_text("export default async () => ({})\n")
    assert uninstall_harness_extension("opencode", home=tmp_path) == [_default_path(tmp_path)]
    assert not _default_path(tmp_path).exists()
    assert other.exists()


def test_uninstall_when_never_installed_is_a_no_op(tmp_path):
    assert uninstall_harness_extension("opencode", home=tmp_path) == []


def test_cli_install_prints_the_honest_restart_and_mode_lines(tmp_path):
    result = CliRunner().invoke(app, ["install", "--harness", "opencode"])
    assert result.exit_code == 0, result.output
    assert _default_path(tmp_path).exists()
    assert "Restart OpenCode" in result.output
    assert "OpenCode asks the gate in-process." in result.output
    assert "gate_mode: enforce" in result.output
    assert "daisugi start" in result.output
    assert "every tool call will be denied" not in result.output


def test_cli_dry_run_writes_nothing(tmp_path):
    result = CliRunner().invoke(app, ["install", "--harness", "opencode", "--dry-run"])
    assert result.exit_code == 0, result.output
    assert f"would write {_default_path(tmp_path)}" in result.output
    assert not _default_path(tmp_path).exists()


def test_cli_uninstall_removes_it(tmp_path):
    CliRunner().invoke(app, ["install", "--harness", "opencode"])
    result = CliRunner().invoke(app, ["install", "--harness", "opencode", "--uninstall"])
    assert result.exit_code == 0, result.output
    assert not _default_path(tmp_path).exists()


def test_cli_uninstall_dry_run_removes_nothing(tmp_path):
    CliRunner().invoke(app, ["install", "--harness", "opencode"])
    result = CliRunner().invoke(
        app, ["install", "--harness", "opencode", "--uninstall", "--dry-run"]
    )
    assert result.exit_code == 0, result.output
    assert f"would remove {_default_path(tmp_path)}" in result.output
    assert _default_path(tmp_path).exists()


def test_cli_installs_pi_and_opencode_together(tmp_path):
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--harness", "opencode"])
    assert result.exit_code == 0, result.output
    assert _default_path(tmp_path).exists()
    assert (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()


def test_cli_unknown_harness_names_both_supported_ones(tmp_path):
    result = CliRunner().invoke(app, ["install", "--harness", "nope"])
    assert result.exit_code == 2
    assert "pi, opencode" in result.output


def test_install_refuses_a_symlinked_plugins_directory(tmp_path):
    elsewhere = tmp_path / "elsewhere"
    elsewhere.mkdir()
    plugins = _default_path(tmp_path).parent
    plugins.parent.mkdir(parents=True)
    plugins.symlink_to(elsewhere)
    with pytest.raises(ValueError, match="symlink"):
        install_harness_extension("opencode", home=tmp_path)
    assert list(elsewhere.iterdir()) == []
    result = CliRunner().invoke(app, ["install", "--harness", "opencode"])
    assert result.exit_code == 1
    assert "symlink" in result.output
