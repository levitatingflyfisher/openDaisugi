"""Passive hook for capturing tool calls into JSONL captures.

Claude Code, Hermes, and OpenClaw all ship tool-call hooks that can block;
none ship distillation. This hook is a passive journal-collector that
fuels the reproduction substrate with real captured runs — turn it on
alongside any agent runtime and sessions accumulate as data the Distiller
can learn from.

Design constraints:
- Never block the host runtime. Even malformed input returns
  ``{"continue": true}`` so the agent's tool call still proceeds.
- One JSONL file per session, append-only, in
  ``~/.opendaisugi/captures/<session_id>.jsonl`` by default.
- Conversion from capture to journal trace is explicit (``to-trace``);
  captures are raw observations, traces are inferred-envelope wrappers.

**Data-handling note:** captures store full shell commands and URLs as
strings. Secrets passed on a Bash command line (e.g. ``curl -H 'Authorization:
Bearer sk-...'``) or in URL query strings will land in the JSONL files. The
captures directory is created with mode 0o700 so other local users cannot
read it, but treat captures as sensitive and rotate / delete them when no
longer needed.
"""

from __future__ import annotations

import json
import os
import re
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from opendaisugi.datahome import data_home
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    FileWriteStep,
    MCPStep,
    NetworkStep,
    Permission,
    ShellStep,
)

DEFAULT_CAPTURES_ROOT = data_home() / "captures"


def stdout_for_format(fmt: str, *, block: bool, reason: str = "") -> str:
    """Return the JSON stdout a given host runtime expects from a pre-tool hook.

    Passive capture always allows the call (block=False). The block branch is
    used by the call-time warden (:mod:`opendaisugi.warden`) on hosts whose deny
    contract is JSON-on-stdout; on the Claude Code path the warden denies via
    exit code 2 instead (see ``warden.warden_and_contract``).

    - claude (and default): ``{"continue": true}`` (PreToolUse contract)
    - hermes: ``{}`` no-op, or a block object carrying BOTH ``decision`` and
      ``action`` keys — Hermes' live block contract is unverified against a
      real host and its docs disagree with the originally shipped single-key
      shape, so a fail-closed product emits both; whichever the host honors,
      it blocks. (Enforcement class for Hermes remains *unverified* until a
      per-version contract test exists — roadmap Stage 5.)
    - openclaw: ``{}`` no-op, or ``{"block": true, "blockReason": ...}``
      (same unverified caveat)
    """
    if fmt == "hermes":
        return json.dumps(
            {"decision": "block", "action": "block", "reason": reason} if block else {}
        )
    if fmt == "openclaw":
        return json.dumps({"block": True, "blockReason": reason} if block else {})
    return json.dumps({"continue": True})


# Formats whose host reads the process exit code as the allow or deny signal,
# 2 for deny, and never parses stdout for a verdict. Claude Code's PreToolUse
# hook, pi's warden extension and OpenCode's plugin all read this signal. pi
# and OpenCode read it from the resident warden's {"exit_code": ...} socket
# reply. The set names every exit-code host at once so the two plugins
# cannot land two different rewrites of warden._outcome.
EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})

# Formats whose host reads a JSON body on stdout as the verdict, exit code
# always 0. The two sets together are every format the warden speaks. A
# format in neither set must not fall through stdout_for_format's default
# body, which is an allow. warden._outcome denies it instead.
STDOUT_BLOCK_FORMATS = frozenset({"hermes", "openclaw"})


def record_and_contract(raw: bytes, *, root: Path, fmt: str) -> str:
    """Record a hook payload (best-effort) and return the host's allow contract.

    MUST NOT raise — the host runtime always receives an allow contract even on
    non-UTF-8 bytes, malformed/deeply-nested JSON, or a recording error. Decodes
    defensively (errors='replace') so a bad-bytes stdin can't crash the hook
    before the contract is emitted.
    """
    try:
        text = raw.decode("utf-8", "replace")
        payload = json.loads(text) if text.strip() else {}
        if payload:
            record_call(payload, root=root, fmt=fmt)
    except Exception:
        pass
    return stdout_for_format(fmt, block=False)


# Claude Code's Notification event (https://code.claude.com/docs/en/hooks) carries
# session_id, transcript_path, cwd, hook_event_name, message, title, and
# notification_type — one of exactly twelve documented values. Only these four
# mean a human is actually being asked something; every other value (idle_prompt,
# auth_success, elicitation_complete, elicitation_response, agent_completed,
# quota_auto_resume_fired, quota_auto_resume_stale, quota_auto_resume_disabled)
# is informational and reports idle.
_BLOCKING_NOTIFICATIONS = frozenset(
    {
        "permission_prompt",
        "elicitation_dialog",
        "elicitation_url_dialog",
        "agent_needs_input",
    }
)


# Claude Code's subagent hooks, by the --event name, and the child state
# each one reports.
_SUBAGENT_EVENTS = {"subagent_start": "working", "subagent_stop": "done"}


def _report_subagent(payload: object, *, state: str) -> None:
    """Report one subagent start or stop to coppice as a child of this pane.

    The payload carries ``agent_id`` and ``agent_type``. A payload with no
    agent id names no child and reports nothing. Never raises.
    """
    try:
        if not isinstance(payload, dict):
            return
        agent_id = payload.get("agent_id")
        if not isinstance(agent_id, str) or not agent_id:
            return
        label = payload.get("agent_type")
        from opendaisugi import _state_report

        _state_report.report_child(agent_id, state, label if isinstance(label, str) else "")
    except Exception:
        pass


def record_lifecycle_event(
    raw: bytes,
    *,
    event: str,
    fmt: str = "claude",
    sessions_root: Path | None = None,
) -> str:
    """Handle Claude Code's Stop, Notification, SubagentStart and
    SubagentStop hooks: session lifecycle, not a tool call. The two subagent
    events report a child row to coppice through ``report_child`` and
    nothing else. Reports 'idle' on Stop, and on Notification either
    'blocked' (a real request for a human — see ``_BLOCKING_NOTIFICATIONS``)
    or 'idle' (every other documented ``notification_type``) depending on
    the payload.

    MUST NOT raise, same contract as record_and_contract: a bad or missing
    payload still returns the host's allow contract.

    A payload that is not a JSON object, or that has no ``session_id``,
    reports and writes nothing (whole-branch review, minor 2) — it mirrors
    warden.py's own guard (``_maybe_report_state``/``_log_tree``). Without
    this, garbage stdin fell through to ``_safe_session_id(None) ==
    "no-session"`` and both delivered a report and created a real
    session-tree directory for a session that never existed.

    Classification: ``notification_type in _BLOCKING_NOTIFICATIONS`` when
    that field is present; when it is ABSENT, falls back to a "permission"
    substring match on ``message``. A present-but-non-blocking type (e.g.
    ``idle_prompt``) is idle regardless of what ``message`` says — the
    enum is authoritative once it exists. Fail this classification any
    OTHER way and it must fail toward idle, never toward a manufactured
    'blocked' (master spec §3.6): an over-eager blocked reading would show
    an operator a fake pending ask.
    """
    try:
        text = raw.decode("utf-8", "replace")
        payload = json.loads(text) if text.strip() else {}
    except Exception:
        payload = {}
    if event in _SUBAGENT_EVENTS:
        _report_subagent(payload, state=_SUBAGENT_EVENTS[event])
        return stdout_for_format(fmt, block=False)
    if not isinstance(payload, dict) or not payload.get("session_id"):
        return stdout_for_format(fmt, block=False)
    try:
        from opendaisugi._state_report import build_event, report_state, report_transcript_path
        from opendaisugi.warden import _HARNESS_BY_FMT

        sid = _safe_session_id(payload.get("session_id"))
        harness = _HARNESS_BY_FMT.get(fmt, fmt)
        if event == "stop":
            state, detail, ask_row = "idle", "session stop", None
        else:  # notification
            message = str(payload.get("message") or "")
            notif_type = payload.get("notification_type")
            is_permission = (
                notif_type in _BLOCKING_NOTIFICATIONS
                if notif_type
                else "permission" in message.lower()
            )
            if is_permission:
                state = "blocked"
                ask_row = {
                    "id": "harness",
                    "tool": str(notif_type or "notification"),
                    "summary": message,
                    "deadline": time.time() + 90,
                }
                detail = f"notification: {message}"[:200]
            else:
                state = "idle"
                detail = f"notification: {message}"[:200] if message else "notification"
                ask_row = None
        ev = build_event(
            session_id=sid,
            harness=harness,
            state=state,
            source="headless",
            harness_session_id=payload.get("session_id"),
            detail=detail,
            ask=ask_row,
            transcript_path=report_transcript_path(payload.get("transcript_path")),
        )
        report_state(ev)
        try:
            from opendaisugi.session_tree import SessionTree

            root = sessions_root or (DEFAULT_CAPTURES_ROOT.parent / "sessions")
            tree = SessionTree.open_or_create(
                root,
                session_id=sid,
                harness=harness,
                cwd=str(payload.get("cwd") or ""),
                harness_session_id=payload.get("session_id"),
                transcript_path=payload.get("transcript_path"),
            )
            tree.append("state", json.loads(ev))
        except Exception:
            pass
    except Exception:
        pass
    return stdout_for_format(fmt, block=False)


# Map common tool names to step types. Mirrors the Claude Code parser's
# _TOOL_TYPE_MAP but accepts variants Hermes/OpenClaw might emit too.
_TOOL_TYPE_MAP: dict[str, str] = {
    "Bash": "shell",
    "shell": "shell",
    "command": "shell",
    "Edit": "file_write",
    "Write": "file_write",
    "MultiEdit": "file_write",
    "Read": "file_read",
    "Glob": "file_read",
    "Grep": "file_read",
    "search": "file_read",
    "WebFetch": "network",
    "WebSearch": "network",
}

# Every built-in tool pi ships, from its own docs. Recorded with its source
# line in src/opendaisugi/harness_pi/extension/PINS.md.
_PI_BUILTIN_TOOLS = frozenset({"read", "bash", "powershell", "edit", "write", "grep", "find", "ls"})

# pi's built-in tool names with a known input shape, lowercase. Consulted
# only under fmt="pi", so a lowercase name stays unknown under every other
# format and no host gains a mapping it did not have. Both shells map to
# "shell" so every shell predicate sees their command. grep, find and ls
# are not here: their path parameter is not pinned, so they stay MCP-style
# and an envelope must name each one to admit it.
_PI_TOOL_TYPE_MAP: dict[str, str] = {
    "bash": "shell",
    "powershell": "shell",
    "read": "file_read",
    "write": "file_write",
    "edit": "file_write",
}

# Hermes shell-hook tool names, consulted only under fmt="hermes". Sources
# (fetched 2026-10-30):
#   https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks
#     stdin JSON: hook_event_name, tool_name, tool_input, session_id, cwd,
#     profile, extra {task_id, tool_call_id}.
#   https://hermes-agent.nousresearch.com/docs/user-guide/features/tools
#   https://raw.githubusercontent.com/NousResearch/hermes-agent/main/tools/file_tools.py
#     read_file(path, offset, limit), write_file(path, content),
#     patch(path, old_string, new_string, replace_all), search_files(pattern,
#     target, path, ...); terminal(command, background, pty).
# Argument names of web_extract and web_search are NOT documented there. The
# record takes url, then urls[0], then query. Only a real Hermes install can
# confirm them (box check 1).
_HERMES_TOOL_TYPE_MAP: dict[str, str] = {
    "terminal": "shell",
    "read_file": "file_read",
    "search_files": "file_read",
    "write_file": "file_write",
    "patch": "file_write",
    "web_search": "network",
    "web_extract": "network",
}

# OpenClaw tool names, consulted only under fmt="openclaw". Sources (fetched
# 2026-10-30):
#   https://docs.openclaw.ai/plugins/hooks/tool-policy  (before_tool_call:
#     toolName, params, toolCallId, runId; ctx.sessionId, ctx.runId)
#   https://docs.openclaw.ai/tools  (exec, read, write, edit, apply_patch,
#     web_fetch, web_search, ...)
#   https://docs.openclaw.ai/tools/exec  (command, workdir, env, background,
#     timeoutSeconds, pty, elevated)
#   https://docs.openclaw.ai/tools/web-fetch  (url, extractMode, maxChars)
# Argument names of read, write and edit are NOT on those pages. The record
# takes path, then file_path, then filePath. Only a real OpenClaw install can
# confirm them (box check 2).
_OPENCLAW_TOOL_TYPE_MAP: dict[str, str] = {
    "exec": "shell",
    "read": "file_read",
    "write": "file_write",
    "edit": "file_write",
    "apply_patch": "file_write",
    "web_fetch": "network",
    "web_search": "network",
}


def _classify_tool(name: str, *, fmt: str = "claude") -> str | None:
    """Return our step type for a host's tool name, or None for unknown.

    Unknown tools are dropped from captures rather than guessed at, which
    keeps the post-hoc inference honest. One exception: under ``fmt="pi"``
    a name this function does not recognize is treated as MCP-style instead
    of dropped. pi has no ``mcp__`` prefix convention, and a dropped record
    would become an unconditional deny that no envelope could override. As
    an MCP-style call, the warden's deny-by-default ``mcp_allowlist`` can name
    it, the same as any other MCP tool. Host MCP tools follow the
    ``mcp__<server>__<tool>`` convention under every other format and
    classify as ``mcp`` regardless of ``fmt``.
    """
    if name.startswith("mcp__"):
        return "mcp"
    mapped = _TOOL_TYPE_MAP.get(name)
    if mapped is not None:
        return mapped
    if fmt == "pi":
        return _PI_TOOL_TYPE_MAP.get(name, "mcp")
    if fmt == "hermes":
        return _HERMES_TOOL_TYPE_MAP.get(name)
    if fmt == "openclaw":
        return _OPENCLAW_TOOL_TYPE_MAP.get(name)
    return None


# OpenCode's apply_patch tool. The plugin sends it under this MCP-style
# name, but it writes files, so the warden never treats it as an MCP call.
APPLY_PATCH_TOOL = "mcp__opencode__apply_patch"
# Codex sends the same tool under its own bare name, with the patch text in
# tool_input.command. It is a patch writer under the claude-shaped format
# Codex's hook uses (OpenClaw has its own apply_patch handling).
CODEX_APPLY_PATCH_TOOL = "apply_patch"


def is_patch_call(tool_name: object, fmt: str = "claude") -> bool:
    """True for a call the warden reads as a patch: OpenCode's name, or
    Codex's bare name under the claude-shaped format."""
    return tool_name == APPLY_PATCH_TOOL or (
        tool_name == CODEX_APPLY_PATCH_TOOL and fmt == "claude"
    )


_PATCH_HEREDOC = re.compile(r"^(?:cat\s+)?<<['\"]?(\w+)['\"]?\s*\n([\s\S]*?)\n\1\s*$")
_PATCH_HEADERS = ("*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:")


def parse_apply_patch(text: object) -> list[str] | None:
    """Every path an OpenCode apply_patch names, in order, or None when the
    patch cannot be read.

    This follows OpenCode's own parser: strip the text, unwrap one heredoc,
    find the first Begin Patch and End Patch lines, and read the header
    lines between them. It takes every header line in that range, even one
    OpenCode would skip, so the warden checks at least every path OpenCode
    touches. A patch with no Begin and End pair, or with no path, is None.
    """
    if not isinstance(text, str):
        return None
    body = text.strip()
    m = _PATCH_HEREDOC.match(body)
    if m:
        body = m.group(2)
    lines = body.split("\n")
    stripped = [line.strip() for line in lines]
    try:
        begin = stripped.index("*** Begin Patch")
        end = stripped.index("*** End Patch")
    except ValueError:
        return None
    if begin >= end:
        return None
    paths: list[str] = []
    for line in lines[begin + 1 : end]:
        for head in _PATCH_HEADERS:
            if line.startswith(head):
                path = line[len(head) :].strip()
                if path:
                    paths.append(path)
                break
    return paths or None


def _parse_mcp_tool_name(name: str) -> tuple[str, str] | None:
    """Split a host ``mcp__<server>__<tool>`` name into (server, tool).

    The tool half may itself contain ``__`` (``mcp__github__list__issues`` →
    ``github`` / ``list__issues``), so only the first ``__`` after the server
    is the split point. Returns None if the name isn't a well-formed MCP tool.
    """
    if not name.startswith("mcp__"):
        return None
    rest = name[len("mcp__") :]
    parts = rest.split("__", 1)
    if len(parts) != 2 or not parts[0] or not parts[1]:
        return None
    return parts[0], parts[1]


JOIN_KEYS = (
    "tool_use_id",
    "agent_id",
    "agent_type",
    "cwd",
    "transcript_path",
    "hook_event_name",
    "permission_mode",
    "turn_id",
    "model",
)


def join_keys(payload: dict[str, Any]) -> dict[str, Any]:
    """The payload fields that link a call to its session, transcript, and agent.

    ``tool_use_id`` joins a verdict to Claude's own transcript row;
    ``transcript_path`` is how a path-D session reaches usage counters and the
    ``parentUuid`` tree; ``agent_id`` names a sub-agent. Absent keys stay absent.
    """
    return {k: payload[k] for k in JOIN_KEYS if payload.get(k)}


_INPUT_KEYS = ("tool_input", "args", "input")


def _mcp_call_arguments(payload: dict[str, Any], inp: Any) -> Any:
    """What an MCP call's arguments are, as the host sent them.

    An object stays. A host value that is no object (a list, a string, a
    number, null) stays as it is, so an argument rule can deny it; an empty
    value (``[]``, ``""``, ``0``, null) is found past the ``or`` chain that
    gave ``{}``. No key at all is a call with no arguments, ``{}``.
    """
    if not isinstance(inp, dict):
        return inp
    if inp:
        return inp
    for key in _INPUT_KEYS:
        if key in payload and not isinstance(payload[key], dict):
            return payload[key]
    return inp


def _payload_to_record(payload: dict[str, Any], *, fmt: str = "claude") -> dict[str, Any] | None:
    """Convert a raw hook payload into a normalized capture record.

    Handles the shapes different hosts emit:
    - Claude Code: ``{tool_name, tool_input, session_id, ...}``
    - Hermes shell-hook: ``{event, tool, args, session_id, ...}``
    - pi (``fmt="pi"``): ``{tool_name, tool_input, session_id, cwd}`` with
      lowercase built-in names. Anything not in ``_TOOL_TYPE_MAP`` becomes
      an MCP-style record with ``mcp_server="pi"``.

    Returns ``None`` for payloads that don't carry a recognizable tool
    call. Caller is expected to skip these (still emitting continue:true
    on stdout to keep the host runtime happy).
    """
    tool_name = payload.get("tool_name") or payload.get("tool") or payload.get("name")
    if not tool_name:
        return None
    if is_patch_call(tool_name, fmt):
        # A file writer, never a capture. The warden reads its paths.
        return None
    step_type = _classify_tool(tool_name, fmt=fmt)
    if step_type is None:
        return None
    inp = payload.get("tool_input") or payload.get("args") or payload.get("input") or {}
    record = {
        "captured_at": time.time(),
        "session_id": _safe_session_id(payload.get("session_id")),
        "tool_name": tool_name,
        "step_type": step_type,
    }
    if step_type == "shell":
        record["command"] = inp.get("command") or inp.get("cmd") or ""
    elif step_type in ("file_read", "file_write"):
        # OpenCode's file tools name their path filePath.
        record["path"] = (
            inp.get("file_path")
            or inp.get("path")
            or inp.get("filePath")
            or inp.get("pattern")
            or ""
        )
        if fmt == "openclaw" and tool_name == "apply_patch":
            # The patch text names its files; the first one is the record's path.
            named = parse_apply_patch(inp.get("patch") or inp.get("input") or "")
            record["path"] = named[0] if named else ""
        # A relative path names a file under the call's working directory,
        # so the warden checks that file. With no absolute cwd the path stays
        # as written, and no workspace pattern matches it. A path that starts
        # with ~ or names a $variable also stays as written: the hard-deny
        # rules expand those, and a join would hide them from those rules.
        cwd = payload.get("cwd")
        path = record["path"]
        if (
            isinstance(path, str)
            and path
            and not os.path.isabs(path)
            and not path.startswith("~")
            and "$" not in path
            and isinstance(cwd, str)
            and os.path.isabs(cwd)
        ):
            record["path"] = os.path.normpath(os.path.join(cwd, path))
        if step_type == "file_write":
            # Don't store full content — captures are for distillation,
            # not exfil. Hash + length is enough.
            content = inp.get("content") or inp.get("new_string") or inp.get("newString") or ""
            record["content_len"] = len(content)
    elif step_type == "network":
        urls = inp.get("urls")
        first_url = urls[0] if isinstance(urls, list) and urls else None
        record["url"] = inp.get("url") or first_url or inp.get("query") or ""
    elif step_type == "mcp":
        if tool_name == APPLY_PATCH_TOOL:
            # A file writer, never an MCP call. The warden reads its paths
            # with parse_apply_patch, and a capture never names it.
            return None
        parsed = _parse_mcp_tool_name(tool_name)
        if parsed is None:
            if fmt != "pi":
                return None
            parsed = ("pi", tool_name)
        record["mcp_server"], record["mcp_tool"] = parsed
        record["arguments"] = _mcp_call_arguments(payload, inp)
    record.update(join_keys(payload))
    return record


def _safe_session_id(raw: object) -> str:
    """Sanitize a host-supplied session id for safe use as a filename.

    The session id is attacker-influenceable (it comes from the host's hook
    payload) and is used as a capture filename, so neutralize path traversal
    and separators. Returns ``"no-session"`` for empty/all-dot inputs.
    """
    safe = re.sub(r"[^A-Za-z0-9._-]", "_", str(raw or ""))
    safe = safe.strip(".")[:128]
    return safe or "no-session"


def record_call(
    payload: dict[str, Any], *, root: Path = DEFAULT_CAPTURES_ROOT, fmt: str = "claude"
) -> Path | None:
    """Append a normalized tool-call capture to ``<root>/<session_id>.jsonl``.

    ``fmt`` is the host format the payload came in (claude, codex, pi, ...).
    It decides which tool names classify.

    Returns the file path written, or ``None`` if the payload didn't
    contain a recognizable tool call (in which case nothing is written).
    """
    record = _payload_to_record(payload, fmt=fmt)
    if record is None:
        return None
    # 0o700 dir / 0o600 files so other local users can't read captured
    # commands / paths / URLs that may contain secrets. mkdir's mode only
    # applies to newly-created dirs and is umask-masked, so chmod explicitly
    # to also tighten a pre-existing loose dir.
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(root, 0o700)
    except OSError:
        pass
    path = root / f"{record['session_id']}.jsonl"  # session_id already sanitized
    newly_created = not path.exists()
    with path.open("a", encoding="utf-8") as f:
        f.write(json.dumps(record) + "\n")
    if newly_created:
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
    return path


def list_sessions(*, root: Path = DEFAULT_CAPTURES_ROOT) -> list[dict[str, Any]]:
    """Return a summary of captured sessions in ``root``.

    Each row: ``{session_id, calls, first_at, last_at}``. Sorted newest-first.
    """
    if not root.exists():
        return []
    rows: list[dict[str, Any]] = []
    for f in sorted(root.glob("*.jsonl")):
        # One pass: track first/last non-empty line and the count together.
        first_line = last_line = None
        calls = 0
        with f.open(encoding="utf-8") as fh:
            for ln in fh:
                if not ln.strip():
                    continue
                if first_line is None:
                    first_line = ln
                last_line = ln
                calls += 1
        if first_line is None:
            continue
        try:
            first = json.loads(first_line)
            last = json.loads(last_line)
        except json.JSONDecodeError:
            continue
        rows.append(
            {
                "session_id": f.stem,
                "calls": calls,
                "first_at": first.get("captured_at"),
                "last_at": last.get("captured_at"),
            }
        )
    rows.sort(key=lambda r: r["last_at"] or 0, reverse=True)
    return rows


def capture_roots(data_dir: Path) -> list[Path]:
    """The directories the journal feed scans for captures, primary first.

    ``<data>/captures`` holds what the capture hook writes. ``<data>/warden/
    captures`` holds what the resident warden mirrors (pi, OpenCode, Codex).
    A Claude Code session with the resident warden up lands in both; the
    primary copy wins and the second is skipped by session id.
    """
    return [data_dir / "captures", data_dir / "warden" / "captures"]


def list_sessions_in(roots: list[Path]) -> list[dict[str, Any]]:
    """``list_sessions`` over several roots: one row per session id.

    A session found in more than one root appears once. Its row comes from the
    first root that holds it. Each row gains ``root``. Sorted newest-first.
    """
    rows: list[dict[str, Any]] = []
    seen: set[str] = set()
    for root in roots:
        for row in list_sessions(root=root):
            if row["session_id"] in seen:
                continue
            seen.add(row["session_id"])
            rows.append({**row, "root": str(root)})
    rows.sort(key=lambda r: r["last_at"] or 0, reverse=True)
    return rows


def find_capture(session_id: str, roots: list[Path]) -> Path | None:
    """The first ``<root>/<session_id>.jsonl`` that exists, in root order."""
    for root in roots:
        path = root / f"{session_id}.jsonl"
        if path.exists():
            return path
    return None


def _glob_for_path(path: str) -> str:
    """Choose a permission glob for an observed path. Defaults to the
    parent dir + ``/**``; handles bare filenames by globbing the cwd."""
    if not path:
        return "**"
    p = Path(path)
    if p.is_absolute():
        parent = str(p.parent) if p.parent != p else "/"
        return f"{parent.rstrip('/')}/**"
    # Relative path — glob the parent or current dir.
    parent = str(p.parent)
    if parent in ("", "."):
        return "./**"
    return f"./{parent.rstrip('/')}/**"


_MAX_OBSERVE_DEPTH = 4  # mirrors verify's _MAX_INTERPRETER_DEPTH


def _observed_effects(command: str, *, decompose: bool) -> tuple[list[str], list[str], list[str]]:
    """Every allowlist key and file-scope path the verifier will check for this
    command: ``(heads, redirect_reads, redirect_writes)``.

    Extraction mirrors the verifier exactly, never a local guess —
    ``_extract_shell_head`` picks the allowlist key, ``decompose_command``
    supplies the simple commands and redirect targets (ADR-0014), and
    ``parse_interpreter`` supplies wrapped payloads (``timeout 30 git fetch``
    checks ``git`` too; ``sh -c 'pytest -q'`` checks ``pytest``). Anything the
    verifier will check that inference fails to collect silently under-admits a
    command that really ran, so the recursion here is the verifier's recursion.

    A command decomposition refuses (a non-literal target or head) is still
    evidence: it contributes its raw head and stays rejected downstream.
    """
    from opendaisugi.interpreter_parse import parse_interpreter
    from opendaisugi.verify import _extract_shell_head

    heads: list[str] = []
    reads: list[str] = []
    writes: list[str] = []

    def _collect(cmd: str, depth: int) -> None:
        if depth > _MAX_OBSERVE_DEPTH:
            return
        simple_commands: tuple[str, ...] | None = None
        if decompose:
            from opendaisugi.shell_decompose import decompose_command

            result = decompose_command(cmd)
            if result.ok:
                reads.extend(result.reads)
                writes.extend(result.writes)
                simple_commands = result.commands
        if simple_commands is None:
            simple_commands = (cmd,)
        for simple in simple_commands:
            head = _extract_shell_head(simple.strip())
            if head:
                heads.append(head)
            payload = parse_interpreter(simple)
            if payload is not None and not payload.opaque:
                for inner in payload.inner_commands:
                    _collect(inner, depth + 1)

    _collect(command, 0)
    return heads, reads, writes


def _has_metachar(command: str) -> bool:
    from opendaisugi.verify import _SHELL_METACHAR_RE

    return bool(_SHELL_METACHAR_RE.search(command))


def _glob_for_access(path: str, tree: bool) -> str:
    """The file-scope glob that admits one shell access. A tree needs a
    ``/**`` glob (SG-2); a path takes its parent directory's glob."""
    if not tree:
        return _glob_for_path(path)
    if path in ("", "."):
        return "./**"
    return f"{path.rstrip('/')}/**"


def _observed_accesses(command: str) -> tuple[set[str], set[str]]:
    """The file-scope globs for the operand accesses of ``command`` (SG-1):
    what the verifier will check besides redirects. An access that cannot be
    placed adds nothing, so the command stays rejected downstream."""
    from opendaisugi.verify import _SANCTIONED_READ_SOURCES, _SANCTIONED_WRITE_SINKS
    from opendaisugi.write_paths import shell_accesses

    acc = shell_accesses(command, None)
    reads = {
        _glob_for_access(a.path, a.tree)
        for a in acc.reads
        if a.via == "operand" and a.path not in _SANCTIONED_READ_SOURCES
    }
    writes = {
        _glob_for_access(a.path, a.tree)
        for a in acc.writes
        if a.via == "operand" and a.path not in _SANCTIONED_WRITE_SINKS
    }
    return reads, writes


def infer_envelope(
    records: list[dict[str, Any]],
    *,
    task: str = "captured-session",
    allow_shell_decomposition: bool = False,
) -> Envelope:
    """Synthesize an envelope that admits every captured tool call.

    Permissions are reverse-engineered from the observed evidence:
    - shell_allowlist gets every observed shell head (with decomposition:
      every simple command's head, wrapped payloads' heads, and substitution
      inner heads — the exact set the verifier checks)
    - shell redirect targets land in file_read / file_write (ADR-0014)
    - file_read / file_write each get parent-dir globs for their paths
    - network is True iff any captured network call exists; hosts are
      reverse-engineered from observed URLs
    - stakes='low' — captures are post-hoc evidence, not forward
      enforcement; the resulting envelope is for distillation input,
      not for policy.

    ``allow_shell_decomposition`` opts the synthesized envelope into ADR-0010
    and widens head collection to match: without both halves a captured
    ``cd /repo && pytest`` still fails, since the flag alone leaves ``pytest``
    off the allowlist and the heads alone leave the metachar warden in force. The
    field is set whenever it is asked for, even if nothing compound was
    captured — the distiller ANDs permissions across a cluster and re-verifies
    the plan template against the result, so a session-by-session flag would
    intersect the opt-in away and kill any mixed cluster.
    """
    from opendaisugi.verify import _SANCTIONED_READ_SOURCES, _SANCTIONED_WRITE_SINKS

    shell_heads: set[str] = set()
    file_read_globs: set[str] = set()
    file_write_globs: set[str] = set()
    network_seen = False
    network_hosts: set[str] = set()
    mcp_tools: set[str] = set()
    for r in records:
        if r["step_type"] == "shell":
            cmd = (r.get("command") or "").strip()
            if cmd:
                obs_heads, obs_reads, obs_writes = _observed_effects(
                    cmd, decompose=allow_shell_decomposition
                )
                shell_heads.update(obs_heads)
                if allow_shell_decomposition or not _has_metachar(cmd):
                    op_reads, op_writes = _observed_accesses(cmd)
                    file_read_globs.update(op_reads)
                    file_write_globs.update(op_writes)
                # Redirect targets are file accesses the verifier will scope-check
                # (ADR-0014); sanctioned pathless sinks (/dev/null, std streams)
                # are never checked, so gloving them would only widen the scope.
                file_read_globs.update(
                    _glob_for_path(p) for p in obs_reads if p not in _SANCTIONED_READ_SOURCES
                )
                file_write_globs.update(
                    _glob_for_path(p) for p in obs_writes if p not in _SANCTIONED_WRITE_SINKS
                )
        elif r["step_type"] == "file_read":
            file_read_globs.add(_glob_for_path(r.get("path") or ""))
        elif r["step_type"] == "file_write":
            file_write_globs.add(_glob_for_path(r.get("path") or ""))
        elif r["step_type"] == "network":
            network_seen = True
            url = r.get("url") or ""
            m = re.match(r"https?://([^/]+)", url)
            if m:
                network_hosts.add(m.group(1).lower())
        elif r["step_type"] == "mcp":
            server = r.get("mcp_server") or ""
            tool = r.get("mcp_tool") or ""
            # apply_patch writes files, so an envelope never admits it by
            # name. The warden checks each path it names instead.
            if server and tool and (server, tool) != ("opencode", "apply_patch"):
                mcp_tools.add(f"{server}/{tool}")
    return Envelope(
        generated_by="opendaisugi.hook.infer_envelope",
        task=task,
        permissions=Permission(
            shell=bool(shell_heads),
            shell_allowlist=sorted(shell_heads),
            shell_allow_decomposition=allow_shell_decomposition,
            mcp_allowlist=sorted(mcp_tools),
            file_read=sorted(file_read_globs) or [],
            file_write=sorted(file_write_globs) or [],
            network=network_seen,
            network_hosts=sorted(network_hosts) or [],
            max_execution_time_s=60,
            max_output_size_mb=20,
        ),
        stakes="low",
    )


def _records_to_steps(records: list[dict[str, Any]]) -> list:
    steps: list = []
    prev_id: str | None = None
    for i, r in enumerate(records):
        sid = f"s{i}"
        deps = [prev_id] if prev_id else []
        if r["step_type"] == "shell":
            steps.append(ShellStep(id=sid, command=r.get("command") or "", depends_on=deps))
        elif r["step_type"] == "file_read":
            steps.append(FileReadStep(id=sid, path=r.get("path") or "", depends_on=deps))
        elif r["step_type"] == "file_write":
            steps.append(
                FileWriteStep(
                    id=sid,
                    path=r.get("path") or "",
                    content="",
                    depends_on=deps,
                )
            )
        elif r["step_type"] == "network":
            steps.append(NetworkStep(id=sid, url=r.get("url") or "", depends_on=deps))
        elif r["step_type"] == "mcp":
            raw = r.get("arguments")
            step = MCPStep(
                id=sid,
                server=r.get("mcp_server") or "",
                tool=r.get("mcp_tool") or "",
                arguments=raw if isinstance(raw, dict) else {},
                depends_on=deps,
            )
            if "arguments" in r and not isinstance(raw, dict):
                step.keep_raw_arguments(raw)
            steps.append(step)
        prev_id = sid
    return steps


def captures_to_trace(
    session_jsonl: Path,
    journal,
    *,
    task: str | None = None,
    allow_shell_decomposition: bool = False,
) -> str:
    """Convert a captured session to a journal trace.

    Reads the JSONL, synthesizes an envelope that admits everything that
    happened, builds an ActionPlan with sequential depends_on, runs
    verify() (which by construction passes against the synthesized
    envelope — minus the metachar warden, which catches genuinely complex
    captured commands; those steps drop out), and appends the trace via
    ``journal.log``. Returns the trace id.

    ``allow_shell_decomposition`` narrows that drop-out to the commands the
    bash grammar genuinely cannot vouch for: compound commands are decomposed
    and every head checked, while redirection, substitution, wrappers and parse
    errors stay rejected.
    """
    from opendaisugi.verify import verify

    records: list[dict[str, Any]] = []
    with session_jsonl.open(encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    if not records:
        raise ValueError(f"no records in {session_jsonl}")
    derived_task = task or f"captured session {session_jsonl.stem}"
    env = infer_envelope(
        records, task=derived_task, allow_shell_decomposition=allow_shell_decomposition
    )
    steps = _records_to_steps(records)
    plan = ActionPlan(source="hook-capture", task=derived_task, steps=steps)
    result = verify(plan, env)
    return journal.log(
        task=derived_task,
        envelope=env,
        plan=plan,
        result=result,
    )


def _spawn_detached_auto_tend(data_dir: Path) -> None:
    """Fire-and-forget `daisugi hook auto-tend` in a fully detached process."""
    import subprocess

    from opendaisugi.install import daisugi_exe

    # The absolute entry point, as install writes it: a hook runs with the
    # agent's PATH, which often lacks the directory `daisugi` is in.
    subprocess.Popen(
        [daisugi_exe(), "hook", "auto-tend", "--data-dir", str(data_dir)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        stdin=subprocess.DEVNULL,
        start_new_session=True,
    )


def maybe_trigger_background_tend(
    data_dir: Path,
    *,
    now: float,
    spawn: Callable[[Path], None] | None = None,
    min_interval_s: int = 1800,
) -> bool:
    """The "no cron" trigger: kick off distillation from the capture hook itself.

    The capture hook already runs on every tool call, so it can start a
    background tend without the user scheduling anything. Fires at most once per
    ``min_interval_s`` and only when the user consented (``config.auto_tend``);
    the spawn is fully detached, so it never blocks or disrupts the host runtime.
    Returns True iff it spawned. Swallows every error and returns False rather
    than ever letting a background concern break the hook's allow contract.
    """
    try:
        # Cheap interval check FIRST — this runs on every tool call, so avoid the
        # config YAML parse in the common (not-due) case. Config is loaded only
        # once the interval has actually elapsed.
        stamp = data_dir / ".hook-record-tend-trigger"
        last = 0.0
        if stamp.exists():
            try:
                last = float(stamp.read_text().strip())
            except ValueError:
                last = 0.0
        if (now - last) < min_interval_s:
            return False

        from opendaisugi.config import auto_tend_enabled, load_config

        if not auto_tend_enabled(load_config(data_dir / "config.yaml")):
            return False
        data_dir.mkdir(parents=True, exist_ok=True)
        stamp.write_text(f"{now}")
        (spawn or _spawn_detached_auto_tend)(data_dir)
        return True
    except Exception:
        return False
