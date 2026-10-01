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

import os
import re
import sys
from pathlib import Path

FIXTURES = Path(__file__).resolve().parent / "fixtures"

# Every case's scratch root (the directory a fixture writes as {ROOT} or
# {HOME}) is this many characters long, whatever the scratch directory.
# The gate cuts a record's text at 200 characters and YAML folds a long
# line at a space, so the text a case keeps depends on the length of the
# path in it. A fixed length makes the fixtures the same on every machine
# and under every DAISUGI_*_SCRATCH. It is short because the weave cases
# "agentic child ..." ask Z3 a containment proof (2 s budget) over a glob
# that holds the root, and the proof takes longer as the root grows: about
# 1.3 s at a 58-character HOME, 1.9 s at 69 on this box. The leaves are
# short (g/0001, c/0001, c/p0001 ...), so a scratch directory of up to 48
# characters fits.
ROOT_LEN = 56


def fixed_root(base: Path, leaf: str) -> Path:
    """base/leaf with the leaf padded with '_' to make ROOT_LEN characters.
    A scratch directory too long for that stops the run with a message."""
    path = os.path.abspath(base) + "/" + leaf
    if len(path) > ROOT_LEN:
        raise SystemExit(
            f"The scratch path {path} has {len(path)} characters. A case root must "
            f"have at most {ROOT_LEN}. Set the suite's DAISUGI_*_SCRATCH to a shorter directory."
        )
    return Path(path + "_" * (ROOT_LEN - len(path)))


# Synthetic paths such as /home/user, .venv/bin/* in an allowlist, or a
# brace pattern like /home/us{1..100} are not matched. Nor is a dot
# directory under a scratch HOME ({WORK}/home/.opendaisugi/): no user name
# starts with a dot.
LEAK = re.compile(
    r"/mnt/|/Users/|/root/|/home/(?!user/)(?!\.)[A-Za-z0-9_.-]+/|site-packages|/\.venv/lib/|/pytest-of-"
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
