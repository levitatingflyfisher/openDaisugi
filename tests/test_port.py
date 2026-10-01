"""DAISUGI_PORT (ruling PK-R-15): install hands over to the named port."""

from __future__ import annotations

import os
import stat
import subprocess
import sys

import pytest

from opendaisugi.port import PortError, hand_over, install_argv, port_hop


def _exe(path, text="#!/bin/sh\n"):
    path.write_text(text)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def test_port_hop_rules(tmp_path):
    me = str(tmp_path / "daisugi-py")
    _exe(tmp_path / "daisugi-py")
    _exe(tmp_path / "daisugi")
    (tmp_path / "daisugi-rs").write_text("")  # not executable
    assert port_hop("python", me, {}) is None
    assert port_hop("python", me, {"DAISUGI_PORT": "python"}) is None
    assert port_hop("python", me, {"DAISUGI_PORT": "python", "DAISUGI_PORT_HOP": "1"}) is None
    assert port_hop("python", me, {"DAISUGI_PORT": "go"}) == str(tmp_path / "daisugi")
    with pytest.raises(PortError) as e:
        port_hop("python", me, {"DAISUGI_PORT": "rust"})
    assert str(e.value) == (
        f"DAISUGI_PORT is rust, but there is no daisugi-rs beside {me}. "
        "Install it, or unset DAISUGI_PORT. Nothing was changed."
    )
    with pytest.raises(PortError) as e:
        port_hop("python", me, {"DAISUGI_PORT": "zig"})
    assert str(e.value) == "DAISUGI_PORT must be go, rust or python, not zig. Nothing was changed."
    with pytest.raises(PortError) as e:
        port_hop("python", me, {"DAISUGI_PORT": "go", "DAISUGI_PORT_HOP": "1"})
    assert str(e.value) == (
        "DAISUGI_PORT is go, but the daisugi it ran is the python port. Nothing was changed."
    )


def test_install_argv_keeps_root_flags_and_the_rest():
    assert install_argv(["d", "-q", "--plain", "-v", "install", "--gate", "--help"]) == [
        "-q",
        "--plain",
        "install",
        "--gate",
        "--help",
    ]
    assert install_argv(["d", "status"]) is None
    assert install_argv(["d"]) is None


def test_hand_over_execs_the_sibling_with_the_hop_mark(tmp_path):
    _exe(tmp_path / "daisugi")
    calls = []
    hand_over(
        [str(tmp_path / "daisugi-py"), "install", "--gate"],
        {"DAISUGI_PORT": "go", "PATH": ""},
        execve=lambda *a: calls.append(a),
    )
    target = str(tmp_path / "daisugi")
    assert calls == [
        (
            target,
            [target, "install", "--gate"],
            {"DAISUGI_PORT": "go", "PATH": "", "DAISUGI_PORT_HOP": "1"},
        )
    ]


def test_the_cli_hands_install_over(tmp_path):
    """The real entry: `daisugi-py install` with DAISUGI_PORT=go runs the
    daisugi beside it, and `--help` goes too."""
    bindir = tmp_path / "bin"
    bindir.mkdir()
    _exe(bindir / "daisugi", '#!/bin/sh\necho "go port: $* hop=$DAISUGI_PORT_HOP"\n')
    py = bindir / "daisugi-py"
    _exe(py, f"#!{sys.executable}\nfrom opendaisugi.cli import main\nmain()\n")
    env = {**os.environ, "HOME": str(tmp_path), "DAISUGI_PORT": "go"}
    r = subprocess.run(
        [str(py), "-q", "install", "--help"], capture_output=True, text=True, env=env
    )
    assert (r.returncode, r.stdout) == (0, "go port: -q install --help hop=1\n"), r.stderr
    env["DAISUGI_PORT"] = "rust"
    r = subprocess.run([str(py), "install", "--dry-run"], capture_output=True, text=True, env=env)
    assert r.returncode == 2
    assert r.stderr == (
        f"daisugi: DAISUGI_PORT is rust, but there is no daisugi-rs beside {py}. "
        "Install it, or unset DAISUGI_PORT. Nothing was changed.\n"
    )
