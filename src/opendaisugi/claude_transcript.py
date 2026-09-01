"""Read Claude Code's session JSONL for what daisugi cannot see from a hook.

On path D Claude owns the prefix and the tree. Its transcript carries
``uuid``/``parentUuid`` on every row and ``message.usage`` on every assistant
turn. daisugi reads it read-only, joined to its own verdicts by ``tool_use_id``.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

_EMPTY_USAGE = {"fresh": 0, "cacheRead": 0, "cacheWrite": 0, "out": 0}
_TEXT_LIMIT = 400


@dataclass(frozen=True)
class Turn:
    uuid: str
    parent_uuid: str | None
    kind: str  # "user" | "assistant"
    ts: str
    model: str | None
    usage: dict[str, int]
    text: str
    tool_uses: list[dict[str, Any]] = field(default_factory=list)
    tool_result_ids: list[str] = field(default_factory=list)


def _usage(msg: dict[str, Any]) -> dict[str, int]:
    u = msg.get("usage") or {}
    return {
        "fresh": int(u.get("input_tokens") or 0),
        "cacheRead": int(u.get("cache_read_input_tokens") or 0),
        "cacheWrite": int(u.get("cache_creation_input_tokens") or 0),
        "out": int(u.get("output_tokens") or 0),
    }


def _content(msg: dict[str, Any]) -> tuple[str, list[dict[str, Any]], list[str]]:
    content = msg.get("content")
    if isinstance(content, str):
        return content[:_TEXT_LIMIT], [], []
    texts: list[str] = []
    uses: list[dict[str, Any]] = []
    results: list[str] = []
    for block in content or []:
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "text":
            texts.append(str(block.get("text", "")))
        elif kind == "tool_use":
            uses.append({"id": block.get("id"), "name": block.get("name")})
        elif kind == "tool_result" and block.get("tool_use_id"):
            results.append(str(block["tool_use_id"]))
    return " ".join(texts)[:_TEXT_LIMIT], uses, results


def read_turns(path: Path) -> list[Turn]:
    out: list[Turn] = []
    try:
        text = Path(path).read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return out
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(row, dict) or row.get("type") not in ("user", "assistant"):
            continue
        msg = row.get("message")
        if not isinstance(msg, dict) or not row.get("uuid"):
            continue
        body, uses, results = _content(msg)
        out.append(
            Turn(
                uuid=str(row["uuid"]),
                parent_uuid=row.get("parentUuid"),
                kind=str(row["type"]),
                ts=str(row.get("timestamp", "")),
                model=msg.get("model"),
                usage=_usage(msg) if row["type"] == "assistant" else dict(_EMPTY_USAGE),
                text=body,
                tool_uses=uses,
                tool_result_ids=results,
            )
        )
    return out


def usage_totals(turns: list[Turn]) -> dict[str, int]:
    total = dict(_EMPTY_USAGE)
    for t in turns:
        if t.kind == "assistant":
            for k in total:
                total[k] += t.usage.get(k, 0)
    return total


def last_prompt_uuid(turns: list[Turn]) -> str | None:
    """The last user turn that is a real prompt (text, not a tool result)."""
    for t in reversed(turns):
        if t.kind == "user" and t.text and not t.tool_result_ids:
            return t.uuid
    return None


def leaf_uuids(turns: list[Turn]) -> list[str]:
    parents = {t.parent_uuid for t in turns if t.parent_uuid}
    return [t.uuid for t in turns if t.uuid not in parents]


def last_model(turns: list[Turn]) -> str | None:
    for t in reversed(turns):
        if t.kind == "assistant" and t.model:
            return t.model
    return None
