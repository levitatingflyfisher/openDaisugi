"""An installed warden hook bakes its --root. A relative root would resolve
against the hook's cwd, the agent's workspace, so install refuses one."""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.install import _patch_claude_warden, _patch_codex_warden


def test_claude_refuses_a_relative_root(tmp_path):
    settings = tmp_path / "settings.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_claude_warden(settings, root=Path("od/warden"))
    assert not settings.exists()


def test_claude_refuses_a_relative_default_root(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.warden.DEFAULT_WARDEN_ROOT", Path("rel/warden"))
    settings = tmp_path / "settings.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_claude_warden(settings)
    assert not settings.exists()


def test_codex_refuses_a_relative_default_root(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.warden.DEFAULT_WARDEN_ROOT", Path("rel/warden"))
    hooks = tmp_path / "hooks.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_codex_warden(hooks)
    assert not hooks.exists()


def test_an_absolute_root_installs(tmp_path):
    settings = tmp_path / "settings.json"
    assert _patch_claude_warden(settings, root=tmp_path / "warden") == [settings]
    assert str(tmp_path / "warden") in settings.read_text()
