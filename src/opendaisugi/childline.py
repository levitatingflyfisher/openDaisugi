"""The line protocol both resident children share: the voice engine
(``voice.resident``) and a pack's worker (``pack.client``). Each child
answers on stdout one JSON object per line, and a parent that sees a
child end names how it ended. This module is below both, so the pack
client does not import the voice package.
"""

from __future__ import annotations

import json


def parse_line(raw: bytes) -> dict | None:
    """A reply or ready line as a JSON object, or None when it is not one."""
    try:
        obj = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        return None
    return obj if isinstance(obj, dict) else None


def exit_reason(returncode: int | None, stderr_tail: list[str]) -> str:
    """How a child ended, in a few words, with its last stderr line."""
    if returncode is None:
        what = "stopped"
    elif returncode < 0:
        what = f"killed by signal {-returncode}"
    else:
        what = f"exited {returncode}"
    last = stderr_tail[-1].strip()[:200] if stderr_tail else ""
    return f"{what}: {last}" if last else what
