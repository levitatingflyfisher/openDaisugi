"""Fixed git repositories for the case generators.

Commit dates are pinned, so the same files always give the same commit id.
The cases name that id as the branch base a ledger row or a card records.
"""

from __future__ import annotations

import os
import subprocess
import tempfile
from pathlib import Path

DATE = "2020-01-01T00:00:00+0000"


def env() -> dict[str, str]:
    return {
        **os.environ,
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_SYSTEM": "/dev/null",
        "GIT_AUTHOR_DATE": DATE,
        "GIT_COMMITTER_DATE": DATE,
    }


def run(cwd: Path, *args: str) -> None:
    subprocess.run(
        ["git", "-C", str(cwd), "-c", "user.name=t", "-c", "user.email=t@t", *args],
        check=True,
        capture_output=True,
        env=env(),
    )


def commit_id(files: dict[str, str]) -> str:
    """The id of the one commit ``git init``, ``add -A`` and ``commit -m base``
    make of ``files``."""
    with tempfile.TemporaryDirectory(dir=os.environ.get("GITFIX_TMP")) as d:
        repo = Path(d)
        run(repo, "init", "-q", "-b", "main")
        for name, body in files.items():
            (repo / name).parent.mkdir(parents=True, exist_ok=True)
            (repo / name).write_text(body, encoding="utf-8")
        run(repo, "add", "-A")
        run(repo, "commit", "-qm", "base")
        return subprocess.run(
            ["git", "-C", str(repo), "rev-parse", "HEAD"],
            check=True,
            capture_output=True,
            text=True,
            env=env(),
        ).stdout.strip()
