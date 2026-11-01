"""OpenCode storage parser: OpenCode sessions become onboardable episodes.

OpenCode keeps one JSON file per session, message and part (types:
https://unpkg.com/@opencode-ai/sdk/dist/gen/types.gen.d.ts; layout as a
third-party reader documents it, fetched 2026-10-30:
https://docs.rs/lore-cli/0.1.12/src/lore_cli/capture/watchers/opencode.rs.html):

    <storage>/session/<project>/<sessionID>.json
    <storage>/message/<sessionID>/<messageID>.json
    <storage>/part/<messageID>/<partID>.json

Whether a current release still writes this tree is UNVERIFIED. The layout has
shifted across releases. This parser takes the session file's path, finds
``<storage>`` three levels up, and reads the messages and parts of that one
session.

Messages are read in ``time.created`` order (file name breaks a tie). A user
message becomes a user turn with the text of its ``text`` parts. A tool part
of an assistant message becomes a ``tool_use`` block, unless its
``state.status`` is ``pending`` or ``error``. Names map through the OpenCode
plugin's table; any other name becomes ``mcp__opencode__<tool>``.
"""

from __future__ import annotations

import json
import math
from pathlib import Path

from opendaisugi.parsers.claude_code import ClaudeCodeParser

_OPENCODE_TO_CLAUDE = {
    "bash": "Bash",
    "read": "Read",
    "write": "Write",
    "edit": "Edit",
    "glob": "Glob",
    "grep": "Grep",
    "webfetch": "WebFetch",
    "websearch": "WebSearch",
}


def _load_object(path: Path) -> dict | None:
    try:
        d = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return d if isinstance(d, dict) else None


def _json_names(directory: Path) -> list[str]:
    if not directory.is_dir():
        return []
    return sorted(p.name for p in directory.iterdir() if p.name.endswith(".json") and p.is_file())


def _created(msg: dict) -> float:
    """The message's ``time.created`` as a sort key; 0 when absent or unusable.

    A bool, a non-number, a NaN, an infinity and an integer past 2**53 all
    count as absent, so every port sorts the same way.
    """
    t = msg.get("time")
    c = t.get("created") if isinstance(t, dict) else None
    if isinstance(c, bool):
        return 0.0
    if isinstance(c, int) and abs(c) <= 2**53:
        return float(c)
    if isinstance(c, float) and math.isfinite(c):
        return c
    return 0.0


class OpenCodeParser(ClaudeCodeParser):
    """Parses one OpenCode session (its storage tree) into episodes."""

    SOURCE = "opencode"

    def _read_messages(self, path) -> list[dict]:
        session_file = Path(path)
        storage = session_file.parent.parent.parent
        sid = (
            session_file.name[: -len(".json")]
            if session_file.name.endswith(".json")
            else session_file.name
        )
        msg_dir = storage / "message" / sid
        names = _json_names(msg_dir)
        if not names:
            raise ValueError(f"no OpenCode messages for session {sid}")
        loaded: list[tuple[float, str, dict]] = []
        for name in names:
            d = _load_object(msg_dir / name)
            if d is not None:
                loaded.append((_created(d), name, d))
        loaded.sort(key=lambda t: (t[0], t[1]))
        messages: list[dict] = []
        for _, name, msg in loaded:
            # The layout names the message file by its id, so the file stem is
            # the id. The id inside the file is third-party data and never a path.
            mid = name[: -len(".json")]
            parts = self._parts(storage / "part" / mid) if mid not in ("", ".", "..") else []
            role = msg.get("role")
            if role == "user":
                texts = [
                    p["text"]
                    for p in parts
                    if p.get("type") == "text" and isinstance(p.get("text"), str)
                ]
                text = "\n".join(texts).strip()
                if text:
                    messages.append(
                        {
                            "role": "user",
                            "content": text,
                            "uuid": mid,
                            "parentUuid": None,
                            "sessionId": sid,
                        }
                    )
            elif role == "assistant":
                blocks = [b for b in (self._tool_block(p) for p in parts) if b is not None]
                if blocks:
                    messages.append(
                        {
                            "role": "assistant",
                            "content": blocks,
                            "uuid": mid,
                            "parentUuid": None,
                            "sessionId": sid,
                        }
                    )
        return messages

    @staticmethod
    def _parts(directory: Path) -> list[dict]:
        out: list[dict] = []
        for name in _json_names(directory):
            d = _load_object(directory / name)
            if d is not None:
                out.append(d)
        return out

    @staticmethod
    def _tool_block(part: dict) -> dict | None:
        if part.get("type") != "tool" or not isinstance(part.get("tool"), str):
            return None
        state = part.get("state")
        if not isinstance(state, dict) or state.get("status") in ("pending", "error"):
            return None
        tool = part["tool"]
        raw = state.get("input")
        inp = dict(raw) if isinstance(raw, dict) else {}
        if isinstance(inp.get("filePath"), str) and "file_path" not in inp:
            inp["file_path"] = inp["filePath"]
        return {
            "type": "tool_use",
            "id": part.get("callID"),
            "name": _OPENCODE_TO_CLAUDE.get(tool, f"mcp__opencode__{tool}"),
            "input": inp,
        }
