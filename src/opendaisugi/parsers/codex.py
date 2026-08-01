"""Codex rollout parser — Codex sessions become onboardable episodes.

Codex persists each session as rollout JSONL (``~/.codex/sessions/YYYY/MM/DD/
rollout-*.jsonl``): one wrapped item per line. This parser translates rollout
items into the flat ``{role, content}`` message shape the shared episode
pipeline (``ClaudeCodeParser``) already understands, so episode boundaries,
min-tools merging, compound-shell decomposition, and step typing are inherited
rather than reimplemented:

- ``function_call`` name ``shell``/``exec_command``/``container.exec`` (its
  ``arguments`` is a JSON *string* per the Responses API) and the older
  ``local_shell_call`` argv form both become Bash tool_use blocks, with the
  ``bash -lc <script>`` wrapper unwrapped to the script itself;
- ``apply_patch`` (function_call or custom_tool_call) becomes one Write block
  per file named in the patch envelope (``*** Add/Update/Delete File:``);
- ``message`` items become user/assistant turns; ``event_msg`` user messages
  are boundary signals too, deduplicated against the mirrored message item;
- namespaced function calls become ``mcp__<ns>__<name>`` blocks;
- reasoning, outputs, turn_context, and bookkeeping tools are dropped.

Line wrappings vary across Codex builds; ``{"type": ..., "payload": ...}``,
``{"item": {...}}``, and bare response items are all accepted. Malformed lines
are skipped — one bad line must not cost the transcript.

The inherited LLM episode-splitter works on these translated messages
unchanged, so oversized Codex episodes split exactly like Claude Code ones.
"""

from __future__ import annotations

import json
import re
import shlex
from typing import Any

from opendaisugi.parsers.claude_code import ClaudeCodeParser

_ROLLOUT_KINDS = frozenset(
    {"session_meta", "response_item", "event_msg", "turn_context", "compacted"}
)
_RESPONSE_ITEM_TYPES = frozenset(
    {
        "message",
        "function_call",
        "function_call_output",
        "local_shell_call",
        "custom_tool_call",
        "web_search_call",
        "reasoning",
    }
)
_SHELL_TOOL_NAMES = frozenset({"shell", "exec_command", "container.exec"})
_SHELLS = frozenset({"bash", "sh", "zsh", "dash"})
_PATCH_FILE_RE = re.compile(r"^\*\*\* (?:Add|Update|Delete) File: (.+)$", re.MULTILINE)


def _unwrap(line: dict) -> tuple[str | None, dict | None]:
    """Peel rollout-line wrappings down to (kind, item)."""
    d: Any = line
    for _ in range(4):  # bounded descent — wrappings observed at most 2 deep
        if not isinstance(d, dict):
            return None, None
        kind = d.get("type")
        if kind in _ROLLOUT_KINDS and isinstance(d.get("payload"), dict):
            return kind, d["payload"]
        if kind in _RESPONSE_ITEM_TYPES:
            return "response_item", d
        if isinstance(d.get("item"), dict):
            d = d["item"]
            continue
        return None, None
    return None, None


def _argv_to_command(argv: object) -> str | None:
    """A rollout argv becomes the command string the verifier should see."""
    if isinstance(argv, str):
        return argv.strip() or None
    if not isinstance(argv, list) or not argv:
        return None
    parts = [str(a) for a in argv]
    if len(parts) >= 3 and parts[0] in _SHELLS and all(p.startswith("-") for p in parts[1:-1]):
        return parts[-1].strip() or None
    return shlex.join(parts)


def _text_of(content: object) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            b.get("text", "")
            for b in content
            if isinstance(b, dict) and isinstance(b.get("text"), str)
        ).strip()
    return ""


def _patch_write_blocks(patch_text: str) -> list[dict]:
    return [
        {
            "type": "tool_use",
            "name": "Write",
            "input": {"file_path": path.strip(), "content": ""},
        }
        for path in _PATCH_FILE_RE.findall(patch_text or "")
    ]


class CodexParser(ClaudeCodeParser):
    """Parses Codex rollout .jsonl transcripts into episodes."""

    SOURCE = "codex"

    def _read_messages(self, path) -> list[dict]:  # noqa: PLR0912 — one branch per item kind
        messages: list[dict] = []
        last_user_text: str | None = None

        def add_user(text: str) -> None:
            nonlocal last_user_text
            text = text.strip()
            if not text or text == last_user_text:
                return  # event_msg and message items mirror the same turn
            last_user_text = text
            messages.append({"role": "user", "content": text})

        def add_tool_blocks(blocks: list[dict]) -> None:
            if blocks:
                messages.append({"role": "assistant", "content": blocks})

        with open(path, encoding="utf-8") as f:
            for raw in f:
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    line = json.loads(raw)
                except json.JSONDecodeError:
                    continue
                if not isinstance(line, dict):
                    continue
                kind, item = _unwrap(line)
                if item is None:
                    continue
                if kind == "event_msg":
                    if item.get("type") == "user_message":
                        msg = item.get("message")
                        add_user(msg if isinstance(msg, str) else _text_of(msg))
                    continue
                if kind != "response_item":
                    continue

                itype = item.get("type")
                if itype == "message":
                    text = _text_of(item.get("content"))
                    if item.get("role") == "user":
                        add_user(text)
                    elif text:
                        messages.append(
                            {"role": "assistant", "content": [{"type": "text", "text": text}]}
                        )
                elif itype == "function_call":
                    name = item.get("name") or ""
                    try:
                        args = json.loads(item.get("arguments") or "{}")
                    except json.JSONDecodeError:
                        continue
                    if not isinstance(args, dict):
                        continue
                    if name in _SHELL_TOOL_NAMES:
                        command = _argv_to_command(args.get("command"))
                        if command:
                            add_tool_blocks(
                                [
                                    {
                                        "type": "tool_use",
                                        "name": "Bash",
                                        "input": {"command": command},
                                    }
                                ]
                            )
                    elif name == "apply_patch":
                        add_tool_blocks(
                            _patch_write_blocks(args.get("input") or args.get("patch") or "")
                        )
                    elif item.get("namespace"):
                        add_tool_blocks(
                            [
                                {
                                    "type": "tool_use",
                                    "name": f"mcp__{item['namespace']}__{name}",
                                    "input": args,
                                }
                            ]
                        )
                    # other function tools (update_plan, view_image, …) are bookkeeping
                elif itype == "local_shell_call":
                    action = item.get("action") or {}
                    command = _argv_to_command(action.get("command"))
                    if command:
                        add_tool_blocks(
                            [{"type": "tool_use", "name": "Bash", "input": {"command": command}}]
                        )
                elif itype == "custom_tool_call":
                    if item.get("name") == "apply_patch":
                        add_tool_blocks(_patch_write_blocks(item.get("input") or ""))
                elif itype == "web_search_call":
                    query = (item.get("action") or {}).get("query")
                    if query:
                        add_tool_blocks(
                            [{"type": "tool_use", "name": "WebSearch", "input": {"query": query}}]
                        )
                # reasoning / function_call_output: nothing to onboard
        return messages
