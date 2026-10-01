"""An installed gate hook bakes its --root. A relative root would resolve
against the hook's cwd, the agent's workspace, so install refuses one."""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.install import _patch_claude_gate, _patch_codex_gate


def test_claude_refuses_a_relative_root(tmp_path):
    settings = tmp_path / "settings.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_claude_gate(settings, root=Path("od/gate"))
    assert not settings.exists()


def test_claude_refuses_a_relative_default_root(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.DEFAULT_GATE_ROOT", Path("rel/gate"))
    settings = tmp_path / "settings.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_claude_gate(settings)
    assert not settings.exists()


def test_codex_refuses_a_relative_default_root(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.gate.DEFAULT_GATE_ROOT", Path("rel/gate"))
    hooks = tmp_path / "hooks.json"
    with pytest.raises(ValueError, match="absolute"):
        _patch_codex_gate(hooks)
    assert not hooks.exists()


def test_an_absolute_root_installs(tmp_path):
    settings = tmp_path / "settings.json"
    assert _patch_claude_gate(settings, root=tmp_path / "gate") == [settings]
    assert str(tmp_path / "gate") in settings.read_text()
