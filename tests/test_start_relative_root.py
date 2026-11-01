"""`daisugi start` with a relative data dir: one line, exit 2, nothing written.

A hook that bakes a relative warden root would resolve it against the agent's
workspace. The refusal comes before the first write, in a dry run too: a dry
run that promises a hook it could never write is a lie.
"""

from __future__ import annotations

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app

LINE = (
    "daisugi start: the warden root '../dd/warden' is not an absolute path; set HOME, "
    "OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path. Nothing was changed.\n"
)


@pytest.mark.parametrize("extra", [[], ["--dry-run"]])
def test_start_relative_data_dir_writes_nothing(tmp_path, monkeypatch, extra):
    ws = tmp_path / "ws"
    ws.mkdir()
    bin_ = tmp_path / "bin"
    bin_.mkdir()
    (bin_ / "claude").write_text("#!/bin/sh\n")
    (bin_ / "claude").chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_))
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.chdir(ws)
    r = CliRunner().invoke(app, ["start", "--no-ui", "--data-dir", "../dd", *extra])
    assert r.exit_code == 2
    assert r.stderr == LINE
    assert r.stdout == ""
    assert not (ws / ".claude").exists()
    assert not (tmp_path / "dd").exists()
