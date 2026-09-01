"""Shared Node availability check for the pi extension's TypeScript tests.

Node runs the extension source directly with --experimental-strip-types, so
the tests in this package need a real Node 22 or newer on PATH. This is test
support only. It lives under tests/, never under src/opendaisugi/.
"""

from __future__ import annotations

import shutil
import subprocess


def node_status(min_major: int = 22) -> tuple[bool, str]:
    """Return (True, "vX.Y.Z") when a usable Node is on PATH, else (False, reason)."""
    exe = shutil.which("node")
    if exe is None:
        return False, f"node not on PATH (need Node >= {min_major} for the pi extension TS tests)"
    try:
        out = subprocess.run(
            [exe, "--version"], capture_output=True, text=True, timeout=5, check=True
        ).stdout.strip()
    except Exception as exc:  # noqa: BLE001
        return False, f"node --version failed: {exc}"
    version = out.lstrip("v")
    try:
        major = int(version.split(".")[0])
    except ValueError:
        return False, f"could not parse node --version output {out!r}"
    if major < min_major:
        return False, (
            f"node {out} is older than required {min_major} "
            "(pi extension TS tests need --experimental-strip-types)"
        )
    return True, out
