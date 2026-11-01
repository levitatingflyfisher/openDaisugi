"""`daisugi install` with a relative HOME: one line, exit 2, nothing written."""

from __future__ import annotations

import os
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app


def test_install_relative_home_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("HOME", "rel")
    monkeypatch.delenv("OPENDAISUGI_HOME", raising=False)
    monkeypatch.delenv("XDG_DATA_HOME", raising=False)
    # The root is read once at import; a real run starts with HOME=rel.
    monkeypatch.setattr(
        "opendaisugi.warden.DEFAULT_WARDEN_ROOT", Path("rel/.local/share/opendaisugi/warden")
    )
    (tmp_path / "rel" / ".claude").mkdir(parents=True)
    r = CliRunner().invoke(app, ["install", "--yes"])
    assert r.exit_code == 2
    assert r.stderr.startswith("daisugi install: the warden root ")
    assert r.stderr.endswith(" Nothing was changed.\n")
    assert sorted(os.listdir(tmp_path / "rel" / ".claude")) == []
    assert not (tmp_path / "rel" / ".claude.json").exists()
    assert not (tmp_path / "rel" / ".local").exists()
