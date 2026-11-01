"""The MCP tool schemas on file for the warden (plan w3, MP-5).

``daisugi warden schemas import`` stores a ``tools/list`` result at
``<warden root>/schemas/<server>.json``. Registration reads them to check an
``mcp_rules`` entry names real arguments. The warden never reads a schema to
decide a call: the server enforces the shape at call time, the envelope says
the meaning.
"""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

from opendaisugi._state_files import probe_state_text, write_state_file

SERVER_RE = re.compile(r"^[A-Za-z0-9_.-]{1,64}$")
_DIR = "schemas"


def _validate(data: object) -> dict:
    tools = data.get("tools") if isinstance(data, dict) else None
    if not isinstance(tools, list) or not all(
        isinstance(t, dict)
        and isinstance(t.get("name"), str)
        and isinstance(t.get("inputSchema"), dict)
        for t in tools
    ):
        raise ValueError(
            'not a tools/list result: expected {"tools": [{"name": ..., "inputSchema": {...}}]}'
        )
    return data


def import_schema(root: Path, server: str, src: Path) -> Path:
    """Store the ``tools/list`` result in ``src`` as the schema of ``server``."""
    if not SERVER_RE.match(server) or server in (".", ".."):
        raise ValueError(f"not a server name: {server!r}")
    try:
        data = json.loads(Path(src).read_text())
    except (OSError, ValueError) as exc:
        raise ValueError(f"not a tools/list result: {exc}") from exc
    _validate(data)
    return write_state_file(
        Path(root),
        (_DIR,),
        f"{server}.json",
        json.dumps(data, indent=2, ensure_ascii=False) + "\n",
        replace_link=False,
    )


def load_schemas(root: Path | None) -> dict[str, dict]:
    """Every schema under ``<root>/schemas``, keyed by server. A file that
    cannot be read or parsed is skipped with a warning line on stderr."""
    out: dict[str, dict] = {}
    if root is None:
        return out
    try:
        names = sorted(os.listdir(Path(root) / _DIR))
    except OSError:
        return out
    for name in names:
        if not name.endswith(".json") or not SERVER_RE.match(name[:-5]):
            continue
        text, refusal = probe_state_text(Path(root), (_DIR,), name)
        try:
            if text is None:
                raise ValueError(refusal or "unreadable")
            out[name[:-5]] = _validate(json.loads(text))
        except ValueError as exc:
            print(f"warden: skipped the schema file {name}: {exc}", file=sys.stderr)
    return out
