"""The one-line notice when the warden is on and the fence is off (plan task 4)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.fence_notice import LINE, config_turns_fence_off, unconfined

runner = CliRunner()


def _arm(root: Path) -> None:
    (root / "envelopes").mkdir(parents=True)


def _config(home: Path, text: str) -> None:
    d = home / ".config" / "coppice"
    d.mkdir(parents=True, exist_ok=True)
    (d / "coppice.toml").write_text(text, encoding="utf-8")


@pytest.fixture
def home(tmp_path, monkeypatch):
    h = tmp_path / "home"
    h.mkdir()
    monkeypatch.setenv("HOME", str(h))
    monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
    monkeypatch.setenv("OPENDAISUGI_HOME", str(h / "data"))
    return h


@pytest.mark.parametrize(
    "text,want",
    [
        ('fence = "off"\n', True),
        ("fence = 'off'  # mine\n", True),
        ('fence = "auto"\n', False),
        ('fence = "on"\n', False),
        ("", False),
        ('[ui]\nfence = "off"\n', False),
        ('theme = "x"\nfence = "off"\n', True),
    ],
)
def test_only_a_top_level_fence_off_counts(tmp_path, text, want):
    p = tmp_path / "coppice.toml"
    p.write_text(text)
    assert config_turns_fence_off(p) is want


def test_a_missing_config_is_not_off(tmp_path):
    assert config_turns_fence_off(tmp_path / "none.toml") is False


def test_unconfined_needs_an_armed_warden(home, tmp_path):
    root = tmp_path / "warden"
    _config(home, 'fence = "off"\n')
    assert not unconfined(root)
    _arm(root)
    assert unconfined(root)
    (root / "DISARMED").write_text("")
    assert not unconfined(root)


def test_warden_status_says_files_are_not_confined(home):
    root = home / "data" / "warden"
    _arm(root)
    _config(home, 'fence = "off"\n')
    res = runner.invoke(app, ["warden", "status", "--root", str(root)])
    assert LINE in res.output
    j = runner.invoke(app, ["warden", "status", "--root", str(root), "--json"])
    assert json.loads(j.output.splitlines()[0])["unconfined"] is True


def test_warden_status_is_quiet_when_the_fence_is_not_off(home):
    root = home / "data" / "warden"
    _arm(root)
    _config(home, 'fence = "auto"\n')
    res = runner.invoke(app, ["warden", "status", "--root", str(root)])
    assert "Files are not confined" not in res.output
    j = runner.invoke(app, ["warden", "status", "--root", str(root), "--json"])
    assert "unconfined" not in json.loads(j.output.splitlines()[0])


def test_status_says_it_once(home):
    data = home / "data"
    _arm(data / "warden")
    _config(home, 'fence = "off"\n')
    res = runner.invoke(app, ["status", "--data-dir", str(data)])
    assert res.output.count("Files are not confined") == 1
