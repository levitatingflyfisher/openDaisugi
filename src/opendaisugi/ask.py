"""A file protocol between the gate process and a present operator.

The gate never waits for a human who is not there: presence is a heartbeat
file with a live pid. An ask is a file; an answer is a file; a proposal
("remember this") is a file nobody applies automatically. Every path in this
module fails toward deny: no operator, no answer, malformed answer, a
mismatched or missing nonce, or an answer that predates the ask it claims to
answer all resolve to ``None`` (no honored answer) — never an allow.

S1 (adversarial review, 2026-08-27): ``wait_answer`` originally honored any
``answers/<id>.json`` with ``decision == "allow"`` by existence alone, before
the deadline — so a stale or pre-planted answer file was accepted, and
neither the ask nor the answer was ever cleaned up. Fixed here: ``post_ask``
mints a random nonce; an answer is honored only when it echoes that nonce AND
its file is not older than the ask's. Every answer this module observes
(valid or not) retires both files, so ``asks/``/``answers/`` never grow
unbounded and a stale leftover can't resurface if a ``tool_use_id`` is
reused. The nonce correlates an answer to one specific ask instance — it is
not an authentication check; the real trust boundary is filesystem write
access to this private (0700) directory tree, same as everywhere else in the
gate's state.
"""

from __future__ import annotations

import json
import os
import re
import secrets
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

PRESENCE = "operator.json"
ASKS = "asks"
ANSWERS = "answers"
PROPOSALS = "proposals"


def _safe(raw: object) -> str:
    return re.sub(r"[^A-Za-z0-9._-]", "_", str(raw or "")).strip(".")[:128] or "none"


def _mkdir(p: Path) -> None:
    p.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(p, 0o700)
    except OSError:
        pass


def _write_json(path: Path, body: dict[str, Any]) -> Path:
    _mkdir(path.parent)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(body), encoding="utf-8")
    try:
        os.chmod(tmp, 0o600)
    except OSError:
        pass
    os.replace(tmp, path)  # readers never see a half-written file
    return path


def _read_json(path: Path) -> dict[str, Any] | None:
    try:
        body = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return body if isinstance(body, dict) else None


# --- presence -------------------------------------------------------------
def write_presence(
    root: Path, *, pid: int | None = None, clock: Callable[[], float] = time.time
) -> Path:
    return _write_json(
        root / PRESENCE, {"pid": pid if pid is not None else os.getpid(), "at": clock()}
    )


def clear_presence(root: Path) -> None:
    try:
        (root / PRESENCE).unlink()
    except FileNotFoundError:
        pass


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def operator_present(root: Path, *, now: float | None = None, max_age_s: float = 15.0) -> bool:
    body = _read_json(root / PRESENCE)
    if not body:
        return False
    try:
        age = (time.time() if now is None else now) - (root / PRESENCE).stat().st_mtime
    except OSError:
        return False
    if age > max_age_s:
        return False
    pid = body.get("pid")
    return isinstance(pid, int) and _pid_alive(pid)


# --- asks and answers ------------------------------------------------------
def _sweep_expired(root: Path, *, now: float) -> None:
    """Delete any ask (and its matching answer, if any) whose deadline has
    passed, and any answer left with no matching ask at all. The ask
    channel's only janitor — nothing else visits this directory on a
    schedule, and either kind of leftover would otherwise sit there
    forever: an ask that timed out without ever being consumed by its own
    ``wait_answer`` call (e.g. the gate process died mid-wait), or an
    answer that outlived its ask (``wait_answer`` retired the ask a moment
    before a late ``answer()`` write landed). An orphaned answer is worse
    than inert clutter — unlike an ask it carries no deadline of its own,
    so it would sit there and insta-kill the NEXT ask that reuses the same
    ``tool_use_id``: its very first poll would find the leftover, judge it
    invalid (wrong nonce), and give up immediately instead of waiting for a
    genuine operator reply.
    """
    asks_dir = root / ASKS
    if asks_dir.exists():
        for p in list(asks_dir.glob("*.json")):
            body = _read_json(p)
            try:
                deadline = float(body.get("deadline", 0)) if body else 0.0
            except (TypeError, ValueError):
                deadline = 0.0
            if body is None or deadline <= now:
                try:
                    p.unlink()
                except FileNotFoundError:
                    pass
                try:
                    (root / ANSWERS / p.name).unlink()
                except FileNotFoundError:
                    pass

    answers_dir = root / ANSWERS
    if answers_dir.exists():
        for p in list(answers_dir.glob("*.json")):
            if not (asks_dir / p.name).exists():
                try:
                    p.unlink()
                except FileNotFoundError:
                    pass


def post_ask(
    root: Path,
    *,
    tool_use_id: str,
    question: dict[str, Any],
    deadline: float,
    clock: Callable[[], float] = time.time,
) -> Path:
    """Post an ask for a present operator, minting a fresh nonce for it.

    Sweeps other expired asks first (never this one — it doesn't exist yet).
    """
    _sweep_expired(root, now=clock())
    body = {
        "toolUseId": tool_use_id,
        "nonce": secrets.token_hex(16),
        "postedAt": clock(),
        "deadline": deadline,
        **question,
    }
    return _write_json(root / ASKS / f"{_safe(tool_use_id)}.json", body)


def answer(
    root: Path,
    *,
    tool_use_id: str,
    decision: str,
    reason: str = "",
    updated_input: dict[str, Any] | None = None,
) -> Path:
    """Record an operator's decision for a pending ask.

    Automatically echoes the nonce of whatever ask is currently live for this
    ``tool_use_id`` (``None`` when there is none) — the caller never handles
    nonces directly. This correlates the answer to ONE specific ask instance
    so a leftover answer from an earlier, already-resolved or expired ask
    cycle can't be replayed against a new one; it does not authenticate the
    answerer (write access to this private directory is that boundary).
    """
    ask_body = _read_json(root / ASKS / f"{_safe(tool_use_id)}.json")
    nonce = ask_body.get("nonce") if ask_body else None
    body = {
        "toolUseId": tool_use_id,
        "decision": decision,
        "reason": reason,
        "updatedInput": updated_input,
        "nonce": nonce,
    }
    return _write_json(root / ANSWERS / f"{_safe(tool_use_id)}.json", body)


def _consume(root: Path, tool_use_id: str) -> None:
    """Remove both the ask and its answer, once an answer has been inspected
    (valid or not) — nothing here is ever left to be honored twice."""
    tid = _safe(tool_use_id)
    for sub in (ASKS, ANSWERS):
        try:
            (root / sub / f"{tid}.json").unlink()
        except FileNotFoundError:
            pass


def wait_answer(
    root: Path,
    *,
    tool_use_id: str,
    timeout_s: float,
    poll_s: float = 0.2,
    sleep: Callable[[float], None] = time.sleep,
    clock: Callable[[], float] = time.monotonic,
) -> dict[str, Any] | None:
    """Poll for the answer file until ``timeout_s`` has passed.

    An answer is honored ONLY when: it is well-formed JSON with
    ``decision in ("allow", "deny")``, it echoes the nonce of the ask this
    call posted, and its file is not older than that ask's — existence
    alone never authorizes an allow, and a pre-planted or leftover answer
    from a previous ask cycle is rejected, not honored. Every answer this
    call observes (valid or not) retires both files, and a timeout retires
    the ask too, so nothing here grows unbounded or lingers to be honored
    by a later, unrelated ask reusing the same ``tool_use_id``.
    """
    tid = _safe(tool_use_id)
    ask_path = root / ASKS / f"{tid}.json"
    answer_path = root / ANSWERS / f"{tid}.json"

    ask_body = _read_json(ask_path)
    expected_nonce = ask_body.get("nonce") if ask_body else None
    try:
        ask_mtime = ask_path.stat().st_mtime
    except OSError:
        ask_mtime = None

    t_end = clock() + timeout_s
    while True:
        if answer_path.exists():
            body = _read_json(answer_path)
            try:
                answer_mtime = answer_path.stat().st_mtime
            except OSError:
                answer_mtime = None
            valid = (
                body is not None
                and body.get("decision") in ("allow", "deny")
                and expected_nonce is not None
                and body.get("nonce") == expected_nonce
                and ask_mtime is not None
                and answer_mtime is not None
                and answer_mtime >= ask_mtime
            )
            _consume(root, tool_use_id)
            if not valid:
                return None
            return {k: v for k, v in body.items() if k != "nonce"}
        if clock() >= t_end:
            _consume(root, tool_use_id)
            return None
        sleep(poll_s)


def pending_asks(root: Path, *, now: float | None = None) -> list[dict[str, Any]]:
    now = time.time() if now is None else now
    _sweep_expired(root, now=now)
    d = root / ASKS
    if not d.exists():
        return []
    out = []
    for p in sorted(d.glob("*.json")):
        body = _read_json(p)
        if body and float(body.get("deadline", 0)) > now:
            out.append(body)
    return out


# --- proposals -------------------------------------------------------------
def propose(
    root: Path,
    *,
    kind: str,
    scope: str,
    expires_at: float,
    body: dict[str, Any],
    clock: Callable[[], float] = time.time,
) -> Path:
    """Record a proposed envelope edit. Nothing applies it; `daisugi gate proposals` lists them."""
    from opendaisugi.session_tree import new_id

    pid = new_id()
    return _write_json(
        root / PROPOSALS / f"{pid}.json",
        {
            "id": pid,
            "kind": kind,
            "scope": scope,
            "expiresAt": expires_at,
            "createdAt": clock(),
            **body,
        },
    )
