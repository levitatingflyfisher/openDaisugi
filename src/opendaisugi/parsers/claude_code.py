"""ClaudeCodeParser — parse Claude Code .jsonl transcripts into episodes."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

from opendaisugi.models import ActionStep, FileReadStep, FileWriteStep, NetworkStep, ShellStep
from opendaisugi.parsers import Episode, ParseResult


def __getattr__(name: str):
    """Lazily expose ``litellm`` as a module attribute (PEP 562).

    This module is imported at package-init time (parser registration), but
    litellm is a ~2s import only needed when LLM episode-splitting actually
    runs. Keeping it behind ``__getattr__`` preserves ``claude_code.litellm``
    as a patch target for tests while deferring the real import.
    """
    if name == "litellm":
        import litellm
        return litellm
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")

_TOOL_TYPE_MAP: dict[str, str] = {
    "Edit": "file_write",
    "Write": "file_write",
    "Read": "file_read",
    "Bash": "shell",
    "Glob": "file_read",
    "Grep": "file_read",
    "WebFetch": "network",
    "WebSearch": "network",
}

_SPLIT_SYSTEM_PROMPT = """\
You split a sequence of agent tool calls into logical sub-tasks.

Given a user message and a numbered list of tool calls, identify coherent sub-tasks.
Each sub-task should represent a focused unit of work.

Return a JSON object with a "subtasks" array. Each subtask has:
- "start_index": first tool call index (inclusive)
- "end_index": last tool call index (inclusive)
- "task": short description of this sub-task (imperative form)

Rules:
- Every tool call must belong to exactly one subtask.
- Subtasks must be contiguous and ordered by index.
- Aim for 5-15 tool calls per subtask.
"""


# Cap the task length. Real Claude Code user turns can carry thousands of chars
# of injected skill/system text; an episode "task" is meant to be a short label,
# and envelope generation hard-errors past max_task_chars (4000). Keep the
# meaningful head and leave headroom for context.
_MAX_TASK_CHARS = 2000


def _clean_task(text: str) -> str:
    """Turn a raw user turn into a clean episode-task label for distillation.

    Real Claude Code user turns are polluted with injected content that is NOT
    the user's intent: skill bodies, continuation banners, system-reminders.
    Distilling on those produces garbage-labeled pathways. Collapse the common
    injection shapes to a short, clusterable label.
    """
    import re

    t = text.strip()
    # Skill injection: "Base directory for this skill: <path>\n\n# <Title>\n<body>"
    if t.startswith("Base directory for this skill:"):
        first_line = t.split("\n", 1)[0]
        path_part = first_line.split(":", 1)[1].strip() if ":" in first_line else ""
        name = path_part.rstrip("/").rsplit("/", 1)[-1] or "unknown"
        return f"skill: {name}"
    # Continuation banner (a resumed session, not a fresh task)
    if t.startswith("This session is being continued"):
        return "session continuation"
    # Strip injected wrapper blocks that can appear inline within a real turn
    t = re.sub(r"<system-reminder>.*?</system-reminder>", " ", t, flags=re.DOTALL)
    t = re.sub(r"<local-command-[^>]*>.*?</local-command-[^>]*>", " ", t, flags=re.DOTALL)
    t = re.sub(r"<command-message>.*?</command-message>", " ", t, flags=re.DOTALL)
    t = re.sub(r"</?command-(name|args)>", " ", t)  # keep the command itself
    return " ".join(t.split())


def _user_text(msg: dict) -> str | None:
    """Return user-turn text if ``msg`` is a genuine user message, else None.

    Accepts string content and single-element ``[{type:text,text:...}]``
    arrays; tool_result passthroughs and anything else return None. The result
    is length-capped so a giant user turn can't produce an un-ingestable task.
    """
    if msg.get("role") not in ("user", "human"):
        return None
    content = msg.get("content")
    text: str | None = None
    if isinstance(content, str):
        text = content
    elif isinstance(content, list) and len(content) == 1:
        block = content[0]
        if isinstance(block, dict) and block.get("type") == "text":
            inner = block.get("text")
            if isinstance(inner, str):
                text = inner
    if text is None:
        return None
    cleaned = _clean_task(text)
    # If cleaning emptied a pure-injection turn, keep a short slice so the episode
    # still has a label and boundary detection is unchanged.
    return (cleaned or text.strip()[:80])[:_MAX_TASK_CHARS]


def _is_real_user_message(msg: dict) -> bool:
    """True for genuine user turns, False for tool-result passthrough."""
    return _user_text(msg) is not None


def _extract_tool_uses(msg: dict) -> list[dict]:
    """Extract tool_use blocks from an assistant message."""
    content = msg.get("content", [])
    if isinstance(content, str):
        return []
    return [
        block for block in content
        if isinstance(block, dict) and block.get("type") == "tool_use"
    ]


def _extract_step(tool_use: dict) -> dict | None:
    """Map a tool_use block to a step dict, or None if unmapped."""
    name = tool_use.get("name", "")
    inp = tool_use.get("input", {})
    step_type = _TOOL_TYPE_MAP.get(name)
    if step_type is None:
        return None

    path = inp.get("file_path") or inp.get("path") or inp.get("pattern")
    command = inp.get("command") or inp.get("query")

    return {"type": step_type, "path": path, "command": command}


def _split_compound_shell(command: str) -> list[str]:
    """Split a shell command on top-level &&, ||, ; outside quoted strings.

    Intentionally does NOT recurse into ``$(`` or backtick substitution —
    those stay as a single part and continue to be rejected by the v0.17
    metachar gate. This is a 'structural decomposition of clearly-safe
    operators' helper, not a shell parser. v0.18.0+.
    """
    if not command:
        return []
    parts: list[str] = []
    cur: list[str] = []
    i = 0
    in_single = False
    in_double = False
    while i < len(command):
        c = command[i]
        if c == "'" and not in_double:
            in_single = not in_single
            cur.append(c); i += 1; continue
        if c == '"' and not in_single:
            in_double = not in_double
            cur.append(c); i += 1; continue
        if not in_single and not in_double:
            if c == ";":
                parts.append("".join(cur).strip()); cur = []; i += 1; continue
            if c == "&" and i + 1 < len(command) and command[i + 1] == "&":
                parts.append("".join(cur).strip()); cur = []; i += 2; continue
            if c == "|" and i + 1 < len(command) and command[i + 1] == "|":
                parts.append("".join(cur).strip()); cur = []; i += 2; continue
        cur.append(c); i += 1
    tail = "".join(cur).strip()
    if tail:
        parts.append(tail)
    return [p for p in parts if p]


def _extract_step_maybe_multiple(tool_use: dict) -> list[dict]:
    """Like ``_extract_step`` but returns a list. For Bash tool calls whose
    command joins atomic sub-commands with &&, ||, or ;, returns one shell
    step dict per sub-command with a ``_prev_hint`` marker the caller can
    resolve into sequential ``depends_on`` edges. Non-Bash tool calls and
    simple Bash calls return a single-element list. v0.18.0+.
    """
    name = tool_use.get("name", "")
    if name != "Bash":
        single = _extract_step(tool_use)
        return [single] if single is not None else []
    command = tool_use.get("input", {}).get("command", "")
    parts = _split_compound_shell(command)
    if len(parts) <= 1:
        single = _extract_step(tool_use)
        return [single] if single is not None else []
    return [
        {"type": "shell", "path": None, "command": p, "_prev_hint": True}
        for p in parts
    ]


def _validate_boundaries(boundaries: list[dict], num_steps: int) -> bool:
    """True if boundaries cover [0, num_steps) contiguously with no gaps or overlaps."""
    if not boundaries:
        return False
    try:
        sorted_subs = sorted(boundaries, key=lambda s: s["start_index"])
    except (KeyError, TypeError):
        return False
    first_start = sorted_subs[0]["start_index"]
    last_end = sorted_subs[-1].get("end_index")
    if not isinstance(first_start, int) or not isinstance(last_end, int):
        return False
    if first_start != 0 or last_end != num_steps - 1:
        return False
    for i in range(1, len(sorted_subs)):
        prev_end = sorted_subs[i - 1].get("end_index")
        curr_start = sorted_subs[i]["start_index"]
        if not isinstance(prev_end, int) or not isinstance(curr_start, int):
            return False
        if curr_start != prev_end + 1:
            return False
    return True


@dataclass
class _RawEpisode:
    task: str
    first_message: int
    last_message: int
    steps: list[dict] = field(default_factory=list)
    step_start: int | None = field(default=None)
    step_end: int | None = field(default=None)


class ClaudeCodeParser:
    """Parses Claude Code .jsonl transcripts into episodes."""

    def __init__(
        self,
        *,
        min_tools: int = 3,
        max_tools: int = 30,
        model: str = "anthropic/claude-sonnet-4-20250514",
    ) -> None:
        self.min_tools = min_tools
        self.max_tools = max_tools
        self.model = model

    def parse(self, path: Path) -> ParseResult:
        messages = self._read_messages(path)
        raw = self._identify_episodes(messages)
        merged = self._merge_small(raw)
        split = self._split_large(merged)
        episodes = self._finalize(split)
        return ParseResult(
            source="claude-code",
            source_file=str(path),
            parsed_at=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            episodes=episodes,
        )

    def _read_messages(self, path: Path) -> list[dict]:
        """Read .jsonl file into a list of flat ``{role, content}`` dicts.

        Accepts two transcript shapes:

        - Legacy/fixture: each row is already flat, ``{"role": ..., "content": ...}``.
        - Modern Claude Code: each row carries a top-level ``"type"`` discriminator
          (``user``/``assistant`` for real turns, plus many metadata row types like
          ``system``, ``custom-title``, ``file-history-snapshot``, ``attachment``,
          ``agent-name``). The actual message lives under ``"message"``.

        Metadata rows and malformed rows are dropped; user/assistant rows are
        unwrapped into the flat shape the rest of the parser expects.
        """
        messages: list[dict] = []
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    d = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(d, dict):
                    continue
                row_type = d.get("type")
                if row_type in ("user", "assistant"):
                    msg = d.get("message")
                    if isinstance(msg, dict):
                        messages.append({
                            "role": msg.get("role") or row_type,
                            "content": msg.get("content", ""),
                        })
                        continue
                if row_type and row_type not in ("user", "assistant"):
                    continue
                if "role" in d:
                    messages.append(d)
        return messages

    def _identify_episodes(self, messages: list[dict]) -> list[_RawEpisode]:
        """Split messages into raw episodes at real-user-message boundaries."""
        episodes: list[_RawEpisode] = []
        current: _RawEpisode | None = None

        for idx, msg in enumerate(messages):
            task = _user_text(msg)
            if task is not None:
                if current is not None:
                    current.last_message = idx - 1
                current = _RawEpisode(
                    task=task,
                    first_message=idx,
                    last_message=idx,
                )
                episodes.append(current)
            elif current is not None:
                current.last_message = idx
                if msg.get("role") == "assistant":
                    for tu in _extract_tool_uses(msg):
                        # v0.18: compound-shell commands (a && b; c) decompose
                        # into separate ShellSteps. The parser stamps each
                        # decomposed step's ``_prev_index`` to the previous
                        # decomposed sibling so ``_finalize`` can translate
                        # those into real ``depends_on`` step ids.
                        sub_steps = _extract_step_maybe_multiple(tu)
                        prev_index: int | None = None
                        for s in sub_steps:
                            hint = s.pop("_prev_hint", False)
                            if hint and prev_index is not None:
                                s["_prev_index"] = prev_index
                            current.steps.append(s)
                            prev_index = len(current.steps) - 1

        return episodes

    def _merge_small(self, episodes: list[_RawEpisode]) -> list[_RawEpisode]:
        """Merge episodes with fewer than min_tools into the preceding one."""
        if not episodes:
            return []
        result = [episodes[0]]
        for ep in episodes[1:]:
            if len(ep.steps) < self.min_tools:
                result[-1].steps.extend(ep.steps)
                result[-1].last_message = ep.last_message
            else:
                result.append(ep)
        return result

    def _split_large(self, episodes: list[_RawEpisode]) -> list[_RawEpisode]:
        """LLM-split episodes exceeding max_tools into sub-episodes."""
        result: list[_RawEpisode] = []
        for ep in episodes:
            if len(ep.steps) <= self.max_tools:
                result.append(ep)
                continue
            boundaries = self._llm_split(ep.task, ep.steps)
            if not _validate_boundaries(boundaries, len(ep.steps)):
                result.append(ep)
                continue
            for sub in boundaries:
                start = sub["start_index"]
                end = sub["end_index"]
                result.append(
                    _RawEpisode(
                        task=sub["task"],
                        first_message=ep.first_message,
                        last_message=ep.last_message,
                        steps=ep.steps[start : end + 1],
                        step_start=start,
                        step_end=end,
                    )
                )
        return result

    def _llm_split(self, user_message: str, steps: list[dict]) -> list[dict]:
        """Call LLM to identify sub-task boundaries.

        Routes through ``claude -p`` when ``OPENDAISUGI_LLM_BACKEND=claude-code``.
        Default stays litellm.
        """
        from opendaisugi.llm import resolve_backend
        tool_summary = "\n".join(
            f"{i}: {s['type']} {s.get('path') or s.get('command') or ''}"
            for i, s in enumerate(steps)
        )
        user_content = (
            f"User message: {user_message}\n\n"
            f"Tool calls ({len(steps)} total):\n{tool_summary}"
        )

        if resolve_backend() == "claude-code":
            from opendaisugi.claude_code_llm import call_claude_p_json_sync
            from opendaisugi.exceptions import EnvelopeGenerationError

            prompt = f"[system]\n{_SPLIT_SYSTEM_PROMPT}\n\n[user]\n{user_content}"
            try:
                body = call_claude_p_json_sync(prompt, timeout_s=60.0, model="haiku")
            except EnvelopeGenerationError:
                return []
            return body.get("subtasks", [])

        from opendaisugi.parsers import claude_code as _self  # resolves the (patchable) lazy litellm
        response = _self.litellm.completion(
            model=self.model,
            messages=[
                {"role": "system", "content": _SPLIT_SYSTEM_PROMPT},
                {"role": "user", "content": user_content},
            ],
            response_format={"type": "json_object"},
        )
        body = json.loads(response.choices[0].message.content)
        return body.get("subtasks", [])

    def _finalize(self, episodes: list[_RawEpisode]) -> list[Episode]:
        """Assign sequential IDs and convert to Episode models."""
        result: list[Episode] = []
        step_counter = 0
        for i, ep in enumerate(episodes):
            action_steps: list[ActionStep] = []
            # Map from this episode's local step-list index -> assigned step_id,
            # used to resolve v0.18 compound-shell _prev_index hints into
            # actual depends_on references.
            local_idx_to_id: dict[int, str] = {}
            for local_idx, s in enumerate(ep.steps):
                step_id = f"s{step_counter}"
                local_idx_to_id[local_idx] = step_id
                prev_index = s.get("_prev_index")
                depends_on: list[str] = []
                if prev_index is not None and prev_index in local_idx_to_id:
                    depends_on = [local_idx_to_id[prev_index]]
                step_type = s["type"]
                if step_type == "shell":
                    action_steps.append(
                        ShellStep(id=step_id, command=s.get("command") or "", depends_on=depends_on)
                    )
                elif step_type == "file_read":
                    action_steps.append(
                        FileReadStep(id=step_id, path=s.get("path") or "", depends_on=depends_on)
                    )
                elif step_type == "file_write":
                    # Transcripts don't carry the written bytes; content stays empty here.
                    # Re-execution would need the runtime executor to source it.
                    action_steps.append(
                        FileWriteStep(
                            id=step_id,
                            path=s.get("path") or "",
                            content="",
                            depends_on=depends_on,
                        )
                    )
                elif step_type == "network":
                    action_steps.append(
                        NetworkStep(
                            id=step_id,
                            url=s.get("path") or s.get("command") or "",
                            depends_on=depends_on,
                        )
                    )
                else:
                    # Unknown tool type: fall back to a shell step with an empty command.
                    action_steps.append(ShellStep(id=step_id, command="", depends_on=depends_on))
                step_counter += 1
            source_range: dict = {
                "first_message": ep.first_message,
                "last_message": ep.last_message,
            }
            if ep.step_start is not None:
                source_range["step_start"] = ep.step_start
                source_range["step_end"] = ep.step_end
            result.append(
                Episode(
                    id=f"ep_{i:02d}",
                    task=ep.task,
                    steps=action_steps,
                    source_range=source_range,
                )
            )
        return result
