"""SGCM review M3: `daisugi registry init` must reject git ext::/fd:: URLs."""
import os
from typer.testing import CliRunner
from opendaisugi.cli import app

runner = CliRunner()


def test_registry_init_rejects_ext_transport(tmp_path):
    marker = tmp_path / "pwned"
    res = runner.invoke(app, ["registry", "init", f'ext::sh -c "touch {marker}"',
                              "--clone-to", str(tmp_path / "clone")])
    assert res.exit_code == 2
    assert "ext::" in res.output
    assert not marker.exists()  # no command executed


def test_registry_init_rejects_fd_transport(tmp_path):
    res = runner.invoke(app, ["registry", "init", "fd::7", "--clone-to", str(tmp_path / "c")])
    assert res.exit_code == 2
