"""Fail if a committed fixture holds a path from a real machine.

    uv run --no-sync python clients/fixture_paths.py

The fixtures under clients/fixtures are published. Every path in them must
be synthetic (/home/user, /work, /x ...) or a placeholder ({HOME},
{PYTHON}, {SRC}, {ROOT}). This scans every file there for the shapes a
real path takes: /mnt/, /Users/, /root/, a /home/<name> other than the
fake /home/user, a virtualenv's lib or site-packages, a pytest temp dir.
Exit 1 and one line per hit when any is found.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

FIXTURES = Path(__file__).resolve().parent / "fixtures"

# Synthetic paths such as /home/user, .venv/bin/* in an allowlist, or a
# brace pattern like /home/us{1..100} are not matched.
LEAK = re.compile(
    r"/mnt/|/Users/|/root/|/home/(?!user/)[A-Za-z0-9_.-]+/|site-packages|/\.venv/lib/|/pytest-of-"
)


def leaks(root: Path = FIXTURES) -> list[str]:
    hits: list[str] = []
    for p in sorted(root.rglob("*")):
        if not p.is_file():
            continue
        text = p.read_text(encoding="utf-8", errors="replace")
        for m in LEAK.finditer(text):
            start = max(0, m.start() - 40)
            hits.append(f"{p.relative_to(root.parent)}: ...{text[start : m.end() + 40]!r}...")
    return hits


def main() -> int:
    hits = leaks()
    for h in hits[:50]:
        print(h)
    if hits:
        print(f"{len(hits)} machine path(s) in clients/fixtures", file=sys.stderr)
        return 1
    print("no machine paths in clients/fixtures")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
