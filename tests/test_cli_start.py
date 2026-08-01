"""CLI: `daisugi start` — the one way in.

The 3rd of the plan's subprocess-spawning CLI tests (see the plan's Method
note): `test_start_runs_end_to_end_and_spawns_the_resident_gate` really
spawns `daisugi gate serve` in a subprocess. It needs an editable install
(`sys.executable -m opendaisugi.cli` resolving the package) and only works
in this working tree, not an isolated git-archive scratch — that's expected.
"""

from __future__ import annotations

import os
import signal
import subprocess

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gate_server import SOCK_NAME

runner = CliRunner()


def _params(command_name: str) -> set[str]:
    from typer.main import get_command

    cmd = get_command(app).commands[command_name]
    return {p.name for p in cmd.params}


def test_start_has_the_documented_flags():
    # Click param introspection, not substring-in---help — FORCE_COLOR=3 on
    # this box wraps rendered help text with ANSI codes that can split words.
    params = _params("start")
    assert {"enforce", "ask", "no_ui", "dry_run", "data_dir"} <= params


def test_start_dry_run_prints_the_steps_and_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    monkeypatch.setattr("shutil.which", lambda n: "/usr/bin/claude")
    res = runner.invoke(app, ["start", "--dry-run", "--no-ui", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    for key in ("harness", "hook", "envelope", "gate-server", "view"):
        assert key in res.output
    assert not (tmp_path / ".claude" / "settings.json").exists()
    assert not (tmp_path / "data").exists()


def test_start_dry_run_step_table_survives_quiet(tmp_path, monkeypatch):
    """The step table is the command's *result* (console.say), not progress
    chatter (console.note) — it must not be silenced by -q."""
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    monkeypatch.setattr("shutil.which", lambda n: "/usr/bin/claude")
    res = runner.invoke(app, ["-q", "start", "--dry-run", "--no-ui", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    assert "harness" in res.output


def test_ask_without_enforce_is_an_explicit_error(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    res = runner.invoke(app, ["start", "--ask", "--dry-run", "--no-ui", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 2
    assert "--enforce" in res.output


def test_start_runs_end_to_end_and_spawns_the_resident_gate(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    monkeypatch.setattr("shutil.which", lambda n: "/usr/bin/claude")
    data_dir = tmp_path / "data"
    root = data_dir / "gate"
    res = runner.invoke(app, ["start", "--no-ui", "--data-dir", str(data_dir)])
    try:
        assert res.exit_code == 0, res.output
        assert (tmp_path / ".claude" / "settings.json").exists()
        assert (root / SOCK_NAME).exists()
        assert "done" in res.output
    finally:
        out = subprocess.run(["pgrep", "-f", str(root)], capture_output=True, text=True).stdout
        for pid in out.split():
            try:
                os.kill(int(pid), signal.SIGTERM)
            except (ProcessLookupError, ValueError):
                pass


def test_quickstart_is_gone_and_suggests_start():
    res = runner.invoke(app, ["quickstart"])
    assert res.exit_code != 0
    assert "start" in res.output
