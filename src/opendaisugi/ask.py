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

GR-R-10: the channel is read and written through no link. ``operator.json``,
``asks/`` and ``answers/`` are opened as the gate opens the rest of its state
(``_state_files``): the root and each directory with ``O_NOFOLLOW``, each
file by its fd with the owner, link-count and mode checks, the mtime from
the same fd. A file is written under a temp name and renamed inside its
directory, so a link planted at a name is replaced, never written through. A
refused ``asks/`` or ``answers/`` takes no ask and honors no answer.
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

from opendaisugi._state_files import (
    StateWriteRefused,
    probe_file_in,
    probe_state_dirs,
    probe_state_file,
    unlink_state_file,
    write_state_file,
)

PRESENCE = "operator.json"
ASKS = "asks"
ANSWERS = "answers"
PROPOSALS = "proposals"


def _safe(raw: object) -> str:
    return re.sub(r"[^A-Za-z0-9._-]", "_", str(raw or "")).strip(".")[:128] or "none"


def _load(fd: int) -> tuple[dict[str, Any] | None, float]:
    """The JSON object in an open file and its mtime, both from the fd. A
    file that is not readable JSON, or not an object, is (None, mtime)."""
    mtime = os.fstat(fd).st_mtime
    try:
        with os.fdopen(fd, "r", encoding="utf-8") as f:
            body = json.loads(f.read())
    except (OSError, ValueError):
        return None, mtime
    return (body if isinstance(body, dict) else None), mtime


def _read(
    root: Path, dirs: tuple[str, ...], name: str
) -> tuple[dict[str, Any] | None, float | None, bool]:
    """(body, mtime, refused) for ``<root>/<dirs>/<name>``, read through the
    checked open. Missing is (None, None, False); a refusal is (None, None,
    True) and leaves the gate's note."""
    fd, refusal = probe_state_file(root, dirs, name)
    if fd is None:
        return None, None, refusal is not None
    body, mtime = _load(fd)
    return body, mtime, False


def _write(root: Path, dirs: tuple[str, ...], name: str, body: dict[str, Any]) -> Path:
    # Readers never see a half-written file; a link at the name is replaced.
    return write_state_file(root, dirs, name, json.dumps(body), replace_link=True)


def channel_ok(root: Path) -> bool:
    """False when ``asks/`` or ``answers/`` exists and the gate refuses it
    (a link, another user's, writable by others). Missing is fine: the
    first ask makes them. The refusal leaves the gate's note."""
    for sub in (ASKS, ANSWERS):
        fds, why = probe_state_dirs(root, (sub,))
        for f in fds or []:
            os.close(f)
        if fds is None and why is not None:
            return False
    return True


# --- presence -------------------------------------------------------------
def write_presence(
    root: Path, *, pid: int | None = None, clock: Callable[[], float] = time.time
) -> Path:
    _write(root, (), PRESENCE, {"pid": pid if pid is not None else os.getpid(), "at": clock()})
    return root / PRESENCE


def clear_presence(root: Path) -> None:
    unlink_state_file(root, (), PRESENCE)


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def operator_present(root: Path, *, now: float | None = None, max_age_s: float = 15.0) -> bool:
    body, mtime, _ = _read(root, (), PRESENCE)
    if not body or mtime is None:
        return False
    age = (time.time() if now is None else now) - mtime
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
    base = str(Path(root))
    afds, _ = probe_state_dirs(root, (ASKS,))
    asks_fd = afds[-1] if afds else None
    try:
        if asks_fd is not None:
            note_dir = os.path.join(base, ASKS)
            for name in _json_names(asks_fd):
                fd, _ = probe_file_in(asks_fd, note_dir, name)
                body = _load(fd)[0] if fd is not None else None
                try:
                    deadline = float(body.get("deadline", 0)) if body else 0.0
                except (TypeError, ValueError):
                    deadline = 0.0
                if body is None or deadline <= now:
                    try:
                        os.unlink(name, dir_fd=asks_fd)
                    except FileNotFoundError:
                        pass
                    _drop(root, ANSWERS, name)
        wfds, _ = probe_state_dirs(root, (ANSWERS,))
        if wfds is not None:
            try:
                for name in _json_names(wfds[-1]):
                    if asks_fd is None or not _entry_exists(asks_fd, name):
                        try:
                            os.unlink(name, dir_fd=wfds[-1])
                        except FileNotFoundError:
                            pass
            finally:
                for f in wfds:
                    os.close(f)
    finally:
        for f in afds or []:
            os.close(f)


def _json_names(dir_fd: int) -> list[str]:
    """Path.glob("*.json") in one directory: dotfiles and directories too."""
    try:
        return sorted(n for n in os.listdir(dir_fd) if n.endswith(".json"))
    except OSError:
        return []


def _entry_exists(dir_fd: int, name: str) -> bool:
    try:
        os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except OSError:
        return False
    return True


def _drop(root: Path, sub: str, name: str) -> None:
    """Remove ``<root>/<sub>/<name>`` through the directory fd. A directory
    the gate refuses is left alone: nothing is removed through a link."""
    try:
        unlink_state_file(root, (sub,), name)
    except StateWriteRefused:
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
    return _write(root, (ASKS,), f"{_safe(tool_use_id)}.json", body)


# A name is one word of ASCII letters, digits, dash and underscore, up to
# 32 characters. A pane is pane:<id>, or pane alone when coppice cannot
# name it by id, and a plugin is plugin:<id>. The Go twins are
# web.CheckName and Client.Who in the coppice harness.
_NAME = re.compile(r"[A-Za-z0-9_-]{1,32}")
_PANE = re.compile(r"pane(:[A-Za-z0-9._:-]{1,128})?")
_PLUGIN = re.compile(r"plugin:[A-Za-z0-9._:-]{1,128}")
_WHO_FROM = {"token": _NAME, "socket": _NAME, "pane": _PANE, "plugin": _PLUGIN}


def who_of(by: object, who_from: object) -> tuple[str, str]:
    """The name an answer gives and how it was known, checked.

    A name that fits its source is kept. A missing name, a name that breaks
    the rule, or a source nobody defined is ``("local", "none")``, never an
    empty name.
    """
    if not isinstance(who_from, str) or not isinstance(by, str):
        return "local", "none"
    rule = _WHO_FROM.get(who_from)
    if rule is None or not rule.fullmatch(by):
        return "local", "none"
    return by, who_from


def answer(
    root: Path,
    *,
    tool_use_id: str,
    decision: str,
    reason: str = "",
    updated_input: dict[str, Any] | None = None,
    by: str = "",
    who_from: str = "none",
) -> Path:
    """Record an operator's decision for a pending ask.

    ``by`` names who gave it and ``who_from`` says how that name is known:
    token, socket, pane or plugin. No name records ``local`` from ``none``.

    Automatically echoes the nonce of whatever ask is currently live for this
    ``tool_use_id`` (``None`` when there is none) — the caller never handles
    nonces directly. This correlates the answer to ONE specific ask instance
    so a leftover answer from an earlier, already-resolved or expired ask
    cycle can't be replayed against a new one; it does not authenticate the
    answerer (write access to this private directory is that boundary).
    """
    ask_body, _, _ = _read(root, (ASKS,), f"{_safe(tool_use_id)}.json")
    return _write_answer(root, ask_body, tool_use_id, decision, reason, updated_input, by, who_from)


def _write_answer(
    root: Path,
    ask_body: dict[str, Any] | None,
    tool_use_id: str,
    decision: str,
    reason: str,
    updated_input: dict[str, Any] | None,
    by: str,
    who_from: str,
) -> Path:
    nonce = ask_body.get("nonce") if ask_body else None
    body = {
        "toolUseId": tool_use_id,
        "decision": decision,
        "reason": reason,
        "updatedInput": updated_input,
        "nonce": nonce,
    }
    body["by"], body["whoFrom"] = who_of(by, who_from)
    return _write(root, (ANSWERS,), f"{_safe(tool_use_id)}.json", body)


_UNSET: Any = object()


def check_allow(
    root: Path,
    *,
    tool_use_id: str,
    name: str,
    confirm: str | None,
    tier: Any = _UNSET,
) -> str | None:
    """The permanent rule for an allow, read from the ask file itself.

    Returns None when the allow may be written, else the sentence that
    refuses it. An ask is undoable only when its file says ``undoable``;
    when the caller also reports the tier it holds for the ask (``tier``,
    None for none), the ask is undoable only when BOTH say so: the worse of
    the two decides, as coppice's web/ask.go does, so a file rewritten after
    the operator armed the allow cannot lower it. Any other ask needs
    ``confirm`` equal to ``name``, and an empty name never matches. The file
    is read through the checked open (GR-R-10): a linked or shared file reads
    as gone.
    """
    body, _, _ = _read(root, (ASKS,), f"{_safe(tool_use_id)}.json")
    return _allow_refusal(body, name=name, confirm=confirm, tier=tier)


def _allow_refusal(
    body: dict[str, Any] | None, *, name: str, confirm: str | None, tier: Any
) -> str | None:
    if body is None:
        return "that ask is gone"
    undoable = body.get("tier") == "undoable"
    if tier is not _UNSET:
        undoable = undoable and tier == "undoable"
    if undoable:
        return None
    typed = (confirm or "").strip()
    if not name or typed != name:
        return f"this cannot be undone. Type the session id to allow: {name}"
    return None


def answer_allow(
    root: Path,
    *,
    tool_use_id: str,
    name: str,
    confirm: str | None,
    tier: Any = _UNSET,
    reason: str = "",
    by: str = "",
    who_from: str = "none",
) -> tuple[str | None, Path | None]:
    """``check_allow`` and ``answer`` on ONE read of the ask file (GR r1 M5):
    the tier that the check passed and the nonce the answer carries belong to
    the same ask, so a retired ask replaced under the same id cannot take an
    allow that was checked against the old one. (refusal, None) when the
    allow may not be written; (None, path) once it is."""
    body, _, _ = _read(root, (ASKS,), f"{_safe(tool_use_id)}.json")
    refusal = _allow_refusal(body, name=name, confirm=confirm, tier=tier)
    if refusal is not None:
        return refusal, None
    return None, _write_answer(root, body, tool_use_id, "allow", reason, None, by, who_from)


def _consume(root: Path, tool_use_id: str) -> None:
    """Remove both the ask and its answer, once an answer has been inspected
    (valid or not) — nothing here is ever left to be honored twice."""
    tid = _safe(tool_use_id)
    for sub in (ASKS, ANSWERS):
        _drop(root, sub, f"{tid}.json")


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
    name = f"{_safe(tool_use_id)}.json"

    ask_body, ask_mtime, _ = _read(root, (ASKS,), name)
    expected_nonce = ask_body.get("nonce") if ask_body else None

    t_end = clock() + timeout_s
    while True:
        fd, refusal = probe_state_file(root, (ANSWERS,), name)
        if fd is not None or refusal is not None:
            # An answer file that is there but refused, or a refused answers/,
            # is not an answer: the wait ends with no honored answer.
            body, answer_mtime = _load(fd) if fd is not None else (None, None)
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
    fds, _ = probe_state_dirs(root, (ASKS,))
    if fds is None:
        return []
    out = []
    try:
        note_dir = os.path.join(str(Path(root)), ASKS)
        for name in _json_names(fds[-1]):
            fd, _ = probe_file_in(fds[-1], note_dir, name)
            body = _load(fd)[0] if fd is not None else None
            if body and float(body.get("deadline", 0)) > now:
                out.append(body)
    finally:
        for f in fds:
            os.close(f)
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
    return _write(
        root,
        (PROPOSALS,),
        f"{pid}.json",
        {
            "id": pid,
            "kind": kind,
            "scope": scope,
            "expiresAt": expires_at,
            "createdAt": clock(),
            **body,
        },
    )
