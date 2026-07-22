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
from pathlib import Path
from typing import Any

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    FileWriteStep,
    NetworkStep,
    Permission,
    ShellStep,
)

DEFAULT_CAPTURES_ROOT = Path.home() / ".opendaisugi" / "captures"


def stdout_for_format(fmt: str, *, block: bool, reason: str = "") -> str:
    """Return the JSON stdout a given host runtime expects from a pre-tool hook.

    Passive capture always allows the call (block=False). The block branch is
    used by the call-time gate (:mod:`opendaisugi.gate`) on hosts whose deny
    contract is JSON-on-stdout; on the Claude Code path the gate denies via
    exit code 2 instead (see ``gate.gate_and_contract``).

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
            {"decision": "block", "action": "block", "reason": reason}
            if block else {}
        )
    if fmt == "openclaw":
        return json.dumps({"block": True, "blockReason": reason} if block else {})
    return json.dumps({"continue": True})


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
            record_call(payload, root=root)
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


def _classify_tool(name: str) -> str | None:
    """Return our step type for a host's tool name, or None for unknown.

    Unknown tools are dropped from captures rather than guessed at — keeps
    the post-hoc inference honest. v0.22+ may add a registry for custom
    mappings.
    """
    return _TOOL_TYPE_MAP.get(name)


def _payload_to_record(payload: dict[str, Any]) -> dict[str, Any] | None:
    """Convert a raw hook payload into a normalized capture record.

    Handles two shapes:
    - Claude Code: ``{tool_name, tool_input, session_id, ...}``
    - Hermes shell-hook: ``{event, tool, args, session_id, ...}``

    Returns ``None`` for payloads that don't carry a recognizable tool
    call. Caller is expected to skip these (still emitting continue:true
    on stdout to keep the host runtime happy).
    """
    tool_name = (
        payload.get("tool_name")
        or payload.get("tool")
        or payload.get("name")
    )
    if not tool_name:
        return None
    step_type = _classify_tool(tool_name)
    if step_type is None:
        return None
    inp = (
        payload.get("tool_input")
        or payload.get("args")
        or payload.get("input")
        or {}
    )
    record = {
        "captured_at": time.time(),
        "session_id": _safe_session_id(payload.get("session_id")),
        "tool_name": tool_name,
        "step_type": step_type,
    }
    if step_type == "shell":
        record["command"] = inp.get("command") or inp.get("cmd") or ""
    elif step_type in ("file_read", "file_write"):
        record["path"] = (
            inp.get("file_path")
            or inp.get("path")
            or inp.get("pattern")
            or ""
        )
        if step_type == "file_write":
            # Don't store full content — captures are for distillation,
            # not exfil. Hash + length is enough.
            content = inp.get("content") or inp.get("new_string") or ""
            record["content_len"] = len(content)
    elif step_type == "network":
        record["url"] = inp.get("url") or inp.get("query") or ""
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
    payload: dict[str, Any], *, root: Path = DEFAULT_CAPTURES_ROOT
) -> Path | None:
    """Append a normalized tool-call capture to ``<root>/<session_id>.jsonl``.

    Returns the file path written, or ``None`` if the payload didn't
    contain a recognizable tool call (in which case nothing is written).
    """
    record = _payload_to_record(payload)
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
        rows.append({
            "session_id": f.stem,
            "calls": calls,
            "first_at": first.get("captured_at"),
            "last_at": last.get("captured_at"),
        })
    rows.sort(key=lambda r: r["last_at"] or 0, reverse=True)
    return rows


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


def infer_envelope(records: list[dict[str, Any]], *, task: str = "captured-session") -> Envelope:
    """Synthesize an envelope that admits every captured tool call.

    Permissions are reverse-engineered from the observed evidence:
    - shell_allowlist gets every observed shell head (first whitespace token)
    - file_read / file_write each get parent-dir globs for their paths
    - network is True iff any captured network call exists; hosts are
      reverse-engineered from observed URLs
    - stakes='low' — captures are post-hoc evidence, not forward
      enforcement; the resulting envelope is for distillation input,
      not for policy.
    """
    shell_heads: set[str] = set()
    file_read_globs: set[str] = set()
    file_write_globs: set[str] = set()
    network_seen = False
    network_hosts: set[str] = set()
    for r in records:
        if r["step_type"] == "shell":
            cmd = (r.get("command") or "").strip()
            if cmd:
                head = cmd.split()[0]
                # Strip env-var prefixes like "FOO=1 cmd" before extracting head
                if "=" not in head:
                    shell_heads.add(head)
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
    return Envelope(
        generated_by="opendaisugi.hook.infer_envelope",
        task=task,
        permissions=Permission(
            shell=bool(shell_heads),
            shell_allowlist=sorted(shell_heads),
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
            steps.append(FileWriteStep(
                id=sid, path=r.get("path") or "", content="", depends_on=deps,
            ))
        elif r["step_type"] == "network":
            steps.append(NetworkStep(id=sid, url=r.get("url") or "", depends_on=deps))
        prev_id = sid
    return steps


def captures_to_trace(
    session_jsonl: Path,
    journal,
    *,
    task: str | None = None,
) -> str:
    """Convert a captured session to a journal trace.

    Reads the JSONL, synthesizes an envelope that admits everything that
    happened, builds an ActionPlan with sequential depends_on, runs
    verify() (which by construction passes against the synthesized
    envelope — minus the metachar gate, which catches genuinely complex
    captured commands; those steps drop out), and appends the trace via
    ``journal.log``. Returns the trace id.
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
    env = infer_envelope(records, task=derived_task)
    steps = _records_to_steps(records)
    plan = ActionPlan(source="hook-capture", task=derived_task, steps=steps)
    result = verify(plan, env)
    return journal.log(
        task=derived_task,
        envelope=env,
        plan=plan,
        result=result,
    )
