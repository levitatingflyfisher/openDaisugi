"""Behave under a pipe: no escapes, no box drawing, quiet means quiet."""

from __future__ import annotations

import os
import subprocess
import sys

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()
_BOX = "─│┌┐└┘├┤┬┴┼╭╮╰╯━┃▼"


def _run(*args: str, env_extra: dict | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ, **(env_extra or {}))
    return subprocess.run(
        [sys.executable, "-m", "opendaisugi.cli", *args],
        capture_output=True, text=True, env=env, timeout=60,
    )


def _run_no_force_color(*args: str) -> subprocess.CompletedProcess:
    """Like _run, but strips FORCE_COLOR and sets NO_COLOR=1.

    This box has FORCE_COLOR=3 in the ambient shell env, which Rich (Typer's
    help renderer) honors over NO_COLOR — so a plain env-inheriting subprocess
    call would see ANSI escapes in --help regardless of pipe/tty detection.
    Only help-rendering tests need this; our own console.py output already
    respects a real isatty() check independent of FORCE_COLOR.
    """
    env = {k: v for k, v in os.environ.items() if k != "FORCE_COLOR"}
    env["NO_COLOR"] = "1"
    return subprocess.run(
        [sys.executable, "-m", "opendaisugi.cli", *args],
        capture_output=True, text=True, env=env, timeout=60,
    )


def test_help_piped_has_no_escapes():
    proc = _run_no_force_color("--help")
    assert proc.returncode == 0
    assert "\x1b[" not in proc.stdout


def test_modules_piped_has_no_box_drawing(tmp_path):
    proc = _run("modules", "--data-dir", str(tmp_path))
    assert proc.returncode == 0, proc.stderr
    assert not any(ch in proc.stdout for ch in _BOX), proc.stdout[:200]


def test_no_color_env_has_no_escapes(tmp_path):
    proc = _run("status", "--data-dir", str(tmp_path), env_extra={"NO_COLOR": "1"})
    assert "\x1b[" not in proc.stdout + proc.stderr


def test_plain_flag_before_command(tmp_path):
    res = runner.invoke(app, ["--plain", "modules", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert not any(ch in res.output for ch in _BOX)


def test_dashboard_once_piped_never_clears_the_screen(tmp_path):
    proc = _run("dashboard", "--data-dir", str(tmp_path))  # not a TTY: one frame
    assert proc.returncode == 0, proc.stderr
    assert "\x1b[H\x1b[2J" not in proc.stdout


def test_quiet_silences_the_state_echo(tmp_path, monkeypatch):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _ok(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _ok)
    res = runner.invoke(app, ["-q", "orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "backend:" not in res.output
