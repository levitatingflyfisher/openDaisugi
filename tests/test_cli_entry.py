"""The console entry answers a lone --version before it imports the CLI."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


def test_python_version_does_not_import_pydantic():
    # The fast path answers --version before any heavy import.
    code = (
        "import sys; sys.argv = ['daisugi', '--version']\n"
        "from opendaisugi.cli_entry import main\n"
        "try:\n    main()\nexcept SystemExit:\n    pass\n"
        "heavy = ('pydantic', 'typer', 'click', 'yaml', 'asyncio', 'opendaisugi.cli')\n"
        "print(sorted(m for m in heavy if m in sys.modules))"
    )
    env = dict(os.environ, PYTHONPATH=str(REPO / "src"))
    out = subprocess.run(
        [sys.executable, "-c", code], env=env, capture_output=True, text=True, check=True
    ).stdout.splitlines()
    assert out[-1] == "[]", out


def test_python_version_text_is_the_clis_own():
    env = dict(os.environ, PYTHONPATH=str(REPO / "src"))

    def run(mod: str) -> bytes:
        return subprocess.run(
            [sys.executable, "-m", mod, "--version"], env=env, capture_output=True, check=True
        ).stdout

    assert run("opendaisugi.cli_entry") == run("opendaisugi.cli")


def test_other_arguments_still_reach_the_cli():
    env = dict(os.environ, PYTHONPATH=str(REPO / "src"))
    out = subprocess.run(
        [sys.executable, "-m", "opendaisugi.cli_entry", "--help"],
        env=env,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    assert "Usage" in out
