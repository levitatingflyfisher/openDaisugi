"""pi session parser: pi sessions become onboardable episodes.

pi stores each session as JSONL, version 3 (``~/.pi/agent/sessions/<cwd-slug>/
<timestamp>_<uuid>.jsonl``). Source: https://pi.dev/docs/latest/session-format
and ``packages/ai/src/types.ts`` in ``badlogic/pi-mono`` (fetched 2026-10-30).
The first line is the header ``{"type": "session", "version": 3, "id", ...}``.
Every other line has ``id``, ``parentId``, ``type`` and ``timestamp``. A
``message`` entry holds ``message: {role, content}``. An assistant message
lists ``text``, ``thinking`` and ``toolCall`` blocks (``id``, ``name``,
``arguments``). A tool result is its own message with ``role: "toolResult"``.

This parser turns those entries into the flat ``{role, content}`` messages the
shared episode pipeline (``ClaudeCodeParser``) already reads, as
``CodexParser`` does:

- a ``toolCall`` block becomes a ``tool_use`` block; the lowercase pi name maps
  to the Claude Code name (``bash`` to ``Bash`` and so on), and any other name
  becomes ``mcp__pi__<name>``;
- user and assistant text keep their roles;
- thinking blocks, tool results and every other entry type are dropped;
- ``uuid``, ``parentUuid`` and ``sessionId`` carry the entry's own tree ids.

Malformed lines are skipped.
"""

from __future__ import annotations

import json

from opendaisugi.parsers.claude_code import ClaudeCodeParser

_PI_TO_CLAUDE = {
    "bash": "Bash",
    "read": "Read",
    "write": "Write",
    "edit": "Edit",
    "grep": "Grep",
    "find": "Glob",
    "ls": "Glob",
}


class PiParser(ClaudeCodeParser):
    """Parses pi session .jsonl files into episodes."""

    SOURCE = "pi"

    def _read_messages(self, path) -> list[dict]:
        messages: list[dict] = []
        session_id = None
        with open(path, encoding="utf-8") as f:
            for raw in f:
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    d = json.loads(raw)
                except json.JSONDecodeError:
                    continue
                if not isinstance(d, dict):
                    continue
                kind = d.get("type")
                if kind == "session":
                    if isinstance(d.get("id"), str):
                        session_id = d["id"]
                    continue
                if kind != "message":
                    continue
                msg = d.get("message")
                if not isinstance(msg, dict):
                    continue
                role = msg.get("role")
                if role == "user":
                    content = msg.get("content", "")
                elif role == "assistant":
                    content = self._assistant_blocks(msg.get("content"))
                    if not content:
                        continue
                else:
                    continue
                messages.append(
                    {
                        "role": role,
                        "content": content,
                        "uuid": d.get("id"),
                        "parentUuid": d.get("parentId"),
                        "sessionId": session_id,
                    }
                )
        return messages

    @staticmethod
    def _assistant_blocks(content: object) -> list[dict]:
        blocks: list[dict] = []
        if not isinstance(content, list):
            return blocks
        for block in content:
            if not isinstance(block, dict):
                continue
            btype = block.get("type")
            if btype == "text" and isinstance(block.get("text"), str):
                blocks.append({"type": "text", "text": block["text"]})
            elif btype == "toolCall" and isinstance(block.get("name"), str):
                name = block["name"]
                args = block.get("arguments")
                blocks.append(
                    {
                        "type": "tool_use",
                        "id": block.get("id"),
                        "name": _PI_TO_CLAUDE.get(name, f"mcp__pi__{name}"),
                        "input": args if isinstance(args, dict) else {},
                    }
                )
        return blocks
