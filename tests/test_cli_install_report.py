"""`daisugi install --gate --report herdr|coppice` at the CLI surface.

Fix round 1, Finding 1(a): the operator-facing surface (the actual `install`
command, not just the library-level `install()`) had no durable tests, and
the coppice-persistence message was reachable only for coppice — herdr never
wrote its own preference, so the field could go stale. These tests exercise
`install_cmd` end to end through Typer's CliRunner, following
`tests/test_install_gate_baseurl.py`'s pattern: monkeypatch `Path.home` to a
tmp_path so both the harness detection (`<home>/.claude`) and the data dir
(`<home>/.opendaisugi`) land under the test's own tmp_path, never the real
`~/.claude` or `~/.opendaisugi`.
"""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import load_config
from opendaisugi.modules import ACTIVE, detect_stages

runner = CliRunner()


def _settings(home: Path) -> dict:
    path = home / ".claude" / "settings.json"
    return json.loads(path.read_text()) if path.exists() else {}


def _commands(settings: dict, event: str) -> list[str]:
    entries = settings.get("hooks", {}).get(event, [])
    return [h.get("command", "") for e in entries for h in e.get("hooks", [])]


def _install(*args: str):
    return runner.invoke(app, ["install", "--yes", "--runtime", "claude", *args])


def test_bad_report_value_exits_1_with_the_exact_error_on_stderr(tmp_path, monkeypatch):
    """Whole-branch review, minor 10 (STE100): the error now teaches the
    next command instead of just echoing back the bad value."""
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = _install("--gate", "--report", "bogus")
    assert result.exit_code == 1
    assert (
        "Error: --report must be herdr or coppice. Run: daisugi install --gate --report herdr"
        in result.stderr
    )


def test_gate_report_coppice_writes_config_and_prints_the_verbatim_line(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = _install("--gate", "--report", "coppice")
    assert result.exit_code == 0
    cfg_path = tmp_path / ".opendaisugi" / "config.yaml"
    assert load_config(cfg_path).floor_report == "coppice"
    assert (
        f"Floor report set to coppice. Saved in {cfg_path}. "
        "The coppice server is built. Nothing reports to it yet."
    ) in result.stdout


def test_report_herdr_without_gate_prints_the_note_and_installs_nothing(tmp_path, monkeypatch):
    """Whole-branch review, minor 10 (STE100): shorter, active-voice note."""
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = _install("--report", "herdr")
    assert result.exit_code == 0
    assert "Note: --report needs --gate. This run installs no floor-report hooks." in result.stdout
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_gate_report_herdr_writes_config_too(tmp_path, monkeypatch):
    """Finding 2: every --report value writes floor_report, herdr included,
    so the field can never go stale relative to what's actually installed."""
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = _install("--gate", "--report", "herdr")
    assert result.exit_code == 0
    cfg_path = tmp_path / ".opendaisugi" / "config.yaml"
    assert load_config(cfg_path).floor_report == "herdr"


def test_coppice_then_herdr_leaves_config_saying_herdr_and_one_active_floor_row(
    tmp_path, monkeypatch
):
    """Finding 2's closing example: --gate --report coppice, then --gate
    --report herdr, must leave config.yaml saying herdr (not stuck on
    coppice) and `daisugi modules` must show exactly one ACTIVE floor row
    (herdr) — coppice never counts as active."""
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    first = _install("--gate", "--report", "coppice")
    assert first.exit_code == 0
    second = _install("--gate", "--report", "herdr")
    assert second.exit_code == 0

    cfg_path = tmp_path / ".opendaisugi" / "config.yaml"
    assert load_config(cfg_path).floor_report == "herdr"

    stages = detect_stages(tmp_path / ".opendaisugi", home=tmp_path)
    floor = next(s for s in stages if s.key == "floor_report")
    active = [m for m in floor.modules if m.state == ACTIVE]
    assert [m.name for m in active] == ["herdr"]
