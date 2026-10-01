"""scripts/install.sh: the port switches, read before anything is built.

DAISUGI_PORT=rust builds the Rust daisugi (clients/rust) and installs it
as daisugi in place of the Go one, as COPPICE_PORT and SPRIG_PORT do for
coppice and sprig. These tests stop the script before preflight runs, so
nothing is built or installed.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "install.sh"


def _run(tmp_path: Path, **env: str) -> subprocess.CompletedProcess[str]:
    base = {"HOME": str(tmp_path), "PATH": "/usr/bin:/bin", "XDG_BIN_HOME": str(tmp_path / "bin")}
    base.update(env)
    return subprocess.run(  # noqa: S603 - the script under test
        ["/bin/bash", str(SCRIPT)],
        env=base,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )


def test_a_daisugi_port_other_than_go_or_rust_is_refused(tmp_path):
    r = _run(tmp_path, DAISUGI_PORT="zig")
    assert r.returncode == 2
    assert "install.sh: DAISUGI_PORT must be go or rust, not zig" in r.stderr
    assert not (tmp_path / "bin").exists()


def test_the_rust_daisugi_needs_cargo(tmp_path):
    # PATH has no cargo: the script says so before preflight.
    r = _run(tmp_path, DAISUGI_PORT="rust", PATH=str(tmp_path / "empty") + ":/usr/bin:/bin")
    if (Path("/usr/bin/cargo")).exists():
        return
    assert r.returncode == 1
    assert "install.sh: DAISUGI_PORT=rust needs cargo on PATH" in r.stderr
    assert not (tmp_path / "bin").exists()


def test_the_help_names_the_switch():
    text = SCRIPT.read_text(encoding="utf-8")
    head = text.split("set -euo pipefail", 1)[0]
    assert "DAISUGI_PORT=rust" in head
