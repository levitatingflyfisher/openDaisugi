"""scripts/preflight.sh names each missing tool in words that hold on any
distribution, not only on Arch."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


def test_missing_tools_are_named_without_a_distro_command(tmp_path):
    bash = shutil.which("bash")
    assert bash
    empty = tmp_path / "bin"
    empty.mkdir()
    r = subprocess.run(
        [bash, str(REPO / "scripts" / "preflight.sh")],
        env={"PATH": str(empty), "HOME": str(tmp_path)},
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert r.returncode == 1
    assert "missing: go" in r.stderr
    assert "missing: pkg-config" in r.stderr
    assert "pacman" not in r.stderr
    assert "your distribution" in r.stderr
