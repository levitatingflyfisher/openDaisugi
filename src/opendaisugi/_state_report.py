"""Layer-internal PaneStateEvent construction, delivery, and (for one path
that cannot import opendaisugi.floor) validation.

Stdlib only. No import from opendaisugi.floor, opendaisugi.voice, or
opendaisugi.coppice — layer purity (spec-01, master spec §4). gate.py and
hook.py build and deliver PaneStateEvents through this module without ever
reaching into the package that actually OWNS the PaneStateEvent contract
(opendaisugi.floor.events) — that package re-exports report_state from here
instead (opendaisugi/floor/report.py), the one direction layer purity
allows.

Herdr's pane-id environment variable (discovery, spec-01 plan task 1): on
this box, `command -v herdr` found no Herdr installation (Task 0 Step 2
records `herdr` as absent), so the name below is UNVERIFIED against a real
Herdr process. HERDR_PANE_ID is used as the primary candidate (matching
Herdr's own naming convention for its other *_ID variables), with
HERDR_PANE kept as a documented fallback for older Herdr releases. The
Herdr CLI invocation this module shells out to
(`pane report-agent <pane> --source daisugi --agent <harness> --state
<state>`) is EQUALLY unverified — the verb, every flag name, and the env
var are all guesses from Herdr's public docs, not a real run. Re-run the
whole probe (`herdr pane create`, then inspect the child process's
environment AND try the report-agent invocation itself) on a box with
Herdr installed and update this paragraph before spec-06 (the
phone/coppice cutover) ships.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import stat
import subprocess
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")

HERDR_PANE_ENV_CANDIDATES: tuple[str, ...] = ("HERDR_PANE_ID", "HERDR_PANE")

_DETAIL_MAX = 200
_DEFAULT_ROOT = Path.home() / ".opendaisugi" / "gate"  # mirrors opendaisugi.gate.DEFAULT_GATE_ROOT
_HERDR_STATE_MAP = {"working": "working", "blocked": "blocked", "idle": "idle", "done": "idle"}

# coppice's own pane id shape (harness/coppice/internal/layout/layout.go: a
# workspace is "w<N>", a pane is "<workspace>:p<N>"), so "w1:p3", never a
# bare number or a Herdr-style id. A caller-claimed pane that does not match
# this is not a coppice pane at all, and gets no report. re.ASCII: Python's
# \d matches non-ASCII decimal digits under the default Unicode mode, and a
# pane id is ASCII-only. Digits capped at 9 (a real workspace or pane count
# never gets near 1e9): unbounded digits let a caller hand this a
# multi-kilobyte string for no reason. fullmatch, not match()+"$": "$"
# alone also matches just before one trailing newline, so "w1:p1\n" passed
# the old pattern.
_COPPICE_PANE_ID_RE = re.compile(r"w[0-9]{1,9}:p[0-9]{1,9}", re.ASCII)


def _valid_coppice_pane_id(pane: object) -> bool:
    return isinstance(pane, str) and bool(_COPPICE_PANE_ID_RE.fullmatch(pane))


# Herdr's own pane-id shape is unverified (see the module docstring), so
# this is deliberately conservative rather than exact: the same charset
# PROTOCOL.md already uses for coppice's own harness_session_id (letters,
# digits, ., _, : and -), capped at 128 bytes, never leading with "-" (a
# leading dash is how a positional argument gets misread as a flag by most
# CLI parsers; report_state also passes "--" before this value in the
# herdr argv as a second, independent layer against that).
_HERDR_PANE_ID_RE = re.compile(r"[A-Za-z0-9._:-]{1,128}", re.ASCII)


def _valid_herdr_pane_id(pane: object) -> bool:
    return (
        isinstance(pane, str)
        and not pane.startswith("-")
        and bool(_HERDR_PANE_ID_RE.fullmatch(pane))
    )


def _coppice_sock_trustworthy(sock_path: object) -> bool:
    """A caller-claimed coppice socket path, trusted only when it is an
    absolute path to a real unix socket this same uid owns.

    A relative path resolves differently depending on cwd, which this
    process (the resident gate) does not share with the caller; a socket
    owned by another uid could be a different user's coppice server, or a
    rogue listener planted to collect what a caller reports. ``os.lstat``,
    never ``os.stat``: a symlink to a socket this process does not control
    must not be followed and trusted on the symlink target's say-so.
    """
    if not isinstance(sock_path, str) or not sock_path or not os.path.isabs(sock_path):
        return False
    try:
        st = os.lstat(sock_path)
    except OSError:
        return False
    return stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid()


def _clip(s: str, n: int = _DETAIL_MAX) -> str:
    return s[:n]


def build_event(
    *,
    session_id: str,
    harness: str,
    state: str,
    source: str = "gate",
    harness_session_id: str | None = None,
    pane: str | None = None,
    ask: dict[str, Any] | None = None,
    detail: str = "",
    ts: float | None = None,
    v: int = 1,
    transcript_path: str | None = None,
    verdict: dict[str, Any] | None = None,
    mode: str | None = None,
) -> str:
    """Serialize one PaneStateEvent-shaped JSON line (master spec §3.1).

    ``transcript_path``, ``verdict`` and ``mode`` are the gate report
    fields coppice reads beside the event: the harness's own transcript
    file, the last verdict as ``{decision, tool, clause}``, and
    ``enforcing`` or ``watching``. Each is added only when given. A reader
    that does not know them ignores them.

    Trusted-caller helper: gate.py and hook.py are the only callers, and
    both pass known-good literals for state/source, so this never validates
    them — that is opendaisugi.floor.events.PaneStateEvent's job for
    untrusted input. ``detail`` IS clamped here: an operator-authored
    ``reason``/``clause`` string can be arbitrarily long; the wire format
    cannot.
    """
    row: dict[str, Any] = {
        "v": v,
        "ts": time.time() if ts is None else ts,
        "session_id": session_id,
        "harness_session_id": harness_session_id,
        "harness": harness,
        "pane": pane,
        "state": state,
        "source": source,
        "detail": _clip(str(detail)),
    }
    if ask is not None:
        row["ask"] = ask
    if transcript_path is not None:
        row["transcript_path"] = transcript_path
    if verdict is not None:
        row["verdict"] = {
            "decision": str(verdict.get("decision", "")),
            "tool": _clip(str(verdict.get("tool") or "")),
            "clause": _clip(str(verdict.get("clause") or "")),
        }
    if mode is not None:
        row["mode"] = mode
    return json.dumps(row)


def report_transcript_path(raw: object) -> str | None:
    """A hook payload's transcript_path, kept only when it is an absolute
    path string. The payload is untrusted input, and coppice refuses a
    report whose path is not absolute, which would lose the state with it.
    """
    if isinstance(raw, str) and raw and os.path.isabs(raw):
        return raw
    return None


def _send_coppice_requests(
    sock_path: str, requests: list[dict[str, Any]], remaining: Callable[[], float]
) -> None:
    """Open one connection to ``sock_path`` and send each of ``requests`` in
    order, waiting for one reply line after each before sending the next.
    Raises on any failure; the caller decides what that means. The
    remaining time budget is re-applied before every connect/send/recv, the
    same discipline ``report_state`` has always kept on this hot path.

    Used only by the env-derived legacy path, which never sends hello (the
    calling process IS the pane's own process there, so there is nothing
    to authenticate). The explicit-argument path uses
    ``_report_as_pane_via_coppice`` instead, which reads and checks every
    reply rather than discarding it.
    """
    import socket

    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(remaining())
        s.connect(sock_path)
        for req in requests:
            s.settimeout(remaining())
            s.sendall((json.dumps(req) + "\n").encode("utf-8"))
            s.settimeout(remaining())
            s.recv(65536)


def _read_one_reply(sock: Any, remaining: Callable[[], float]) -> dict[str, Any] | None:
    """One reply line off an already-connected socket, JSON-decoded. None
    for anything that is not a well-formed JSON object: garbage, a partial
    read, or a connection that closed without a full line. Never raises;
    a caller that needs a real reply to proceed treats None as a refusal.
    """
    sock.settimeout(remaining())
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = sock.recv(65536)
        if not chunk:
            return None
        buf += chunk
        if len(buf) > 65536:
            return None
    try:
        reply = json.loads(buf)
    except Exception:
        return None
    return reply if isinstance(reply, dict) else None


def _report_as_pane_via_coppice(
    sock_path: str,
    pane: str,
    peer_pid: int | None,
    ev: dict[str, Any],
    remaining: Callable[[], float],
) -> str:
    """The explicit-argument coppice leg of ``report_state``: hello as
    ``pane``, with ``peer_pids`` naming the caller's real OS pid when
    known, then ``pane.report_state`` only once the hello reply confirms
    coppice actually placed the connection as ``pane``.

    ``peer_pids`` is not decoration. coppice's own kernel-pid placement
    (``placeNamedPeers`` in role.go) walks each named pid's real process
    ancestry on every hello and can only NARROW the resulting role, never
    widen it (PROTOCOL.md "Roles": "The server places each named pid the
    way it places a socket peer. A named pid ... can only take rights
    away, never give them"): a caller whose real pid the kernel finds
    inside pane A's process tree gets placed as A regardless of what
    ``pane`` here claims, and the mismatch is then this function's own
    job to catch, since a resident gate's own socket connection to
    coppice carries none of the caller's pane facts on its own. Without
    ``peer_pids``, or when the named pid cannot be placed at all, coppice
    has no evidence against the claim and the hello wins as claimed
    (PROTOCOL.md: "A caller in no pane keeps the hello pane"), the same
    trust a direct in-process report already carries, since there the
    caller and the reporting process are one and the same.

    Two checks close the loop that a bare "send hello, ignore the reply"
    version would leave open: this reads the hello reply and sends the
    actual report only when it says ``{"role": "pane", "pane": pane}``
    exactly, and it reads the report's own reply too, returning
    ``'coppice'`` only on ``ok: true``. A caller can no longer read a
    silent refusal as a delivered report.

    Also verifies, right after connecting, that the peer on the other end
    of this NEW connection is still owned by this uid (SO_PEERCRED),
    beyond the pre-connect ``lstat`` in ``_coppice_sock_trustworthy``: an
    intermediate PATH COMPONENT (a parent directory), not just the final
    socket entry, can be replaced between that lstat and this connect,
    ``lstat`` only inspects the last component named, and a symlinked
    ancestor directory swapped in that window can point the actual
    ``connect()`` at a completely different socket than the one checked.
    """
    import socket as _socket
    import struct as _struct

    with _socket.socket(_socket.AF_UNIX, _socket.SOCK_STREAM) as s:
        s.settimeout(remaining())
        s.connect(sock_path)
        creds = s.getsockopt(_socket.SOL_SOCKET, _socket.SO_PEERCRED, _struct.calcsize("3i"))
        _peer_pid, peer_uid, _peer_gid = _struct.unpack("3i", creds)
        if peer_uid != os.getuid():
            return "none"
        hello: dict[str, Any] = {"id": "h", "cmd": "hello", "role": "pane", "pane": pane}
        if peer_pid is not None:
            hello["peer_pids"] = [peer_pid]
        s.settimeout(remaining())
        s.sendall((json.dumps(hello) + "\n").encode("utf-8"))
        hello_reply = _read_one_reply(s, remaining)
        if hello_reply is None or hello_reply.get("ok") is not True:
            return "none"
        result = hello_reply.get("result")
        if (
            not isinstance(result, dict)
            or result.get("role") != "pane"
            or result.get("pane") != pane
        ):
            return "none"
        req = {"id": "r", "cmd": "pane.report_state", "pane": pane, "event": ev}
        s.settimeout(remaining())
        s.sendall((json.dumps(req) + "\n").encode("utf-8"))
        report_reply = _read_one_reply(s, remaining)
        if report_reply is None or report_reply.get("ok") is not True:
            return "none"
        return "coppice"


def _send_herdr_report(
    herdr_pane: str, ev: dict[str, Any], env: Mapping[str, str], remaining: Callable[[], float]
) -> str:
    """The Herdr CLI leg of ``report_state``, shared by the env-derived and
    the explicit-argument callers. ``env`` here is only ever consulted for
    ``PATH`` (finding the ``herdr`` binary) and as the subprocess's own
    environment, never as a source of pane identity. That is ``herdr_pane``
    alone.
    """
    if not _valid_herdr_pane_id(herdr_pane):
        return "none"
    mapped = _HERDR_STATE_MAP.get(ev.get("state"))
    if mapped is None:
        return "none"  # e.g. 'unknown': Herdr's CLI has no such state
    herdr_bin = shutil.which("herdr", path=env.get("PATH"))
    if not herdr_bin:
        return "none"
    try:
        proc = subprocess.Popen(
            [
                herdr_bin,
                "pane",
                "report-agent",
                "--source",
                "daisugi",
                "--agent",
                str(ev.get("harness") or ""),
                "--state",
                mapped,
                # "--" last, right before the positional pane id: with no
                # flag after it, this is the one placement where "--"
                # only affects the value it precedes. herdr_pane is never
                # shell-interpreted (argv is a list, not a shell string),
                # but a value that starts with "-" could otherwise be
                # misread as a new flag by herdr's own argument parser.
                "--",
                herdr_pane,
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=dict(env),
        )
        try:
            proc.wait(timeout=remaining())
        except subprocess.TimeoutExpired:
            proc.kill()
            # A short, bounded reap (whole-branch review, minor 3): this
            # module also runs inside the long-lived resident gate, not
            # just the ~200ms hot path, so a 1 s wait() here could add a
            # full second past the caller's budget on every wedged
            # herdr. kill()'s SIGKILL reaps almost instantly in the
            # ordinary case; TimeoutExpired from THIS wait is swallowed
            # too, not left to escape to the outer except. A process
            # stuck in an uninterruptible state must not cost the
            # caller any more than this short cap.
            try:
                proc.wait(timeout=0.05)
            except subprocess.TimeoutExpired:
                pass
            return "none"
        return "herdr"
    except Exception:
        return "none"


def report_state(
    ev_json: str,
    *,
    env: Mapping[str, str] = os.environ,
    sock: str | None = None,
    pane: str | None = None,
    herdr_pane: str | None = None,
    peer_pid: int | None = None,
    budget_s: float = 0.2,
) -> str:
    """Deliver one PaneStateEvent line. Returns which sink took it:
    ``'coppice' | 'herdr' | 'none'``. Never raises; any failure is 'none'.

    ``sock``/``pane``/``herdr_pane`` are a CALLER's own pane identity,
    carried explicitly on the request that reached this call. A resident
    gate server forwards them from the wire request that named them; a
    direct in-process caller (hook.py, ``daisugi hook report``, gate.py's
    non-resident path) leaves all three ``None``. Passing any one of them
    switches this call into explicit mode: ``env`` is not read for identity
    at all, so an incomplete pair (only ``sock`` or only ``pane``, and no
    ``herdr_pane``) reports nowhere rather than falling back. A resident
    server serves every session from one process, and must never guess
    which caller a bare ``sock`` or a bare ``pane`` belonged to. ``sock``
    and ``pane`` are re-validated here regardless of anything the caller or
    the wire handler already checked (an absolute path to a real socket
    this uid owns, and a pane id shaped like coppice's own, e.g. ``w1:p3``;
    see ``_coppice_sock_trustworthy``/``_valid_coppice_pane_id``). A
    resident server must never trust a caller's claim about its own
    identity further than it can verify on disk.

    ``peer_pid`` is the OS pid of the actual process that connected to the
    resident gate's own socket (gate_server.py reads it with
    ``SO_PEERCRED`` on that connection), never a value the caller's own
    request body could set. It rides on the hello as ``peer_pids: [pid]``.
    Without it, this claimed ``pane`` alone would be nothing more than an
    unverified string the wire request happened to carry: pane A's own
    caller could put pane B's id in ``coppice_pane`` and, since the
    resident gate's own connection to coppice sits outside every pane's
    process tree, coppice's kernel-pid placement of THAT connection has
    nothing of A's or B's to check the claim against, so a bare
    ``hello role pane pane=B`` would simply be believed. ``peer_pids``
    fixes that at its root: coppice's own placement code
    (``placeNamedPeers``, role.go) walks the NAMED pid's real process
    ancestry on every hello, the same way it would place a raw socket
    peer, and folds the result into this connection's role, and a named
    pid can only narrow that role, never widen it (PROTOCOL.md "Roles":
    "A named pid ... can only take rights away, never give them"). A
    caller whose real pid the kernel finds inside pane A's tree is placed
    as A no matter what ``pane`` claims, so a claim of B is silently
    replaced, not honored, and this function catches that itself, below,
    by checking the hello's own reply rather than trusting the request
    succeeded. A caller whose real pid cannot be placed in any pane keeps
    the claimed pane, the same trust a direct in-process report already
    carries (there, the reporting process and the caller are one process,
    with nothing separate to verify).

    The connection opens with that hello first (PROTOCOL.md "Roles") in
    this mode, since the resident server's own process is not in the
    caller's pane's process tree, so the Linux kernel-pid placement a
    direct in-process report relies on would place this connection as the
    resident server itself, or as no pane at all, without the hello and
    ``peer_pids`` together. The report itself is then sent only once the
    hello's own reply confirms ``{"role": "pane", "pane": pane}`` exactly
    (see ``_report_as_pane_via_coppice``); this function no longer sends a
    report under a claim coppice did not confirm, and no longer returns
    ``'coppice'`` for one either. The return value now means coppice
    accepted the report, not merely that a line was sent. Right after
    connecting, the peer on the far end of the socket is checked with
    ``SO_PEERCRED`` too, beyond the pre-connect ``lstat`` in
    ``_coppice_sock_trustworthy``: an intermediate PATH COMPONENT, not
    just the socket file itself, can be swapped between that lstat and
    this connect, so the post-connect check closes a window the
    pre-connect one cannot see.

    Known residual: if the resident gate's own process is itself started
    from a shell inside a coppice-managed pane's process tree, coppice
    places the resident gate's OWN connection as THAT pane before
    ``peer_pids`` is even considered, and every hello this function then
    sends for some OTHER caller pane is refused, not honored. PROTOCOL.md's
    Roles section refuses ``pane.report_state`` "about any pane but its
    own", so the failure mode is a report that never lands, never one that
    lands on the wrong pane. Fixing this needs either a change on the Go
    side (out of scope here) or a different way for this process to reach
    coppice's socket that the kernel cannot place inside a pane at all.

    With no explicit argument given, this keeps the direct in-process
    contract every existing caller already relies on: the calling process
    IS the pane's own process (a descendant of it, or it), so the
    environment IS the caller's identity, and kernel pid placement already
    attributes the connection correctly with no hello needed. coppice wins
    when both COPPICE_SOCK/COPPICE_PANE and a Herdr pane env var are
    present (a Herdr pane hosting coppice is not a supported nesting, so
    first match wins), same as always.

    ``budget_s`` is a TOTAL wall-clock budget, not a per-operation one: one
    deadline is taken up front and the remaining time is re-applied before
    each connect/sendall/recv (and to the Herdr subprocess wait, and to
    each leg of the explicit path's hello-then-report pair), so a wedged
    peer costs at most ~budget_s on the gate's blocking hot path, never 2x
    or 3x that. The Herdr path uses Popen + wait(timeout=) + kill() with
    stdout/stderr sent to DEVNULL rather than subprocess.run(timeout=):
    run()'s timeout path kills the child but then calls communicate() again,
    which blocks until every inherited pipe closes — a herdr that forks a
    daemon could hold the gate open past the budget regardless of the
    timeout value. Popen + an explicit kill()/wait() has no such pipe to
    wait on.
    """
    try:
        ev = json.loads(ev_json)
    except Exception:
        return "none"
    if not isinstance(ev, dict):
        return "none"
    deadline = time.monotonic() + budget_s

    def _remaining() -> float:
        return max(0.001, deadline - time.monotonic())

    if sock is not None or pane is not None or herdr_pane is not None:
        if _coppice_sock_trustworthy(sock) and _valid_coppice_pane_id(pane):
            try:
                return _report_as_pane_via_coppice(
                    sock,  # type: ignore[arg-type]
                    pane,  # type: ignore[arg-type]
                    peer_pid,
                    ev,
                    _remaining,
                )
            except Exception:
                return "none"
        if herdr_pane:
            return _send_herdr_report(herdr_pane, ev, env, _remaining)
        return "none"

    sock_path = env.get("COPPICE_SOCK")
    coppice_pane = env.get("COPPICE_PANE")
    if sock_path and coppice_pane:
        try:
            _send_coppice_requests(
                sock_path,
                [{"id": "r", "cmd": "pane.report_state", "pane": coppice_pane, "event": ev}],
                _remaining,
            )
            return "coppice"
        except Exception:
            return "none"
    herdr_pane_env = next((env[k] for k in HERDR_PANE_ENV_CANDIDATES if env.get(k)), None)
    if herdr_pane_env:
        return _send_herdr_report(herdr_pane_env, ev, env, _remaining)
    return "none"


def report_child(
    child: str,
    state: str,
    label: str = "",
    *,
    env: Mapping[str, str] = os.environ,
    budget_s: float = 0.2,
) -> str:
    """Report one subagent of this pane's harness to coppice as
    ``pane.report_child``. Returns ``'coppice'`` when the line was sent and
    ``'none'`` otherwise. Only coppice shows subagents, so there is no Herdr
    path. Never raises. ``budget_s`` is the total time for the connect, the
    send, and the reply, the same budget report_state keeps.
    """
    sock_path = env.get("COPPICE_SOCK")
    pane = env.get("COPPICE_PANE")
    if not sock_path or not pane or not child:
        return "none"
    import socket

    deadline = time.monotonic() + budget_s

    def _remaining() -> float:
        return max(0.001, deadline - time.monotonic())

    req = {
        "id": "c",
        "cmd": "pane.report_child",
        "pane": pane,
        "child": child,
        "state": state,
        "label": _clip(str(label)),
    }
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(_remaining())
            s.connect(sock_path)
            s.settimeout(_remaining())
            s.sendall((json.dumps(req) + "\n").encode("utf-8"))
            s.settimeout(_remaining())
            s.recv(65536)
        return "coppice"
    except Exception:
        return "none"


def _validate_hook_report_row(row: dict[str, Any]) -> None:
    """A narrow, independent re-implementation of the master spec §3.1
    rules, used ONLY by `hook_report_argv` below — the one path
    (`daisugi hook report`, reachable through gate.sock) that a layer
    module must serve without importing opendaisugi.floor.events, which
    owns the real contract. Deliberately duplicated, not imported.
    tests/floor/test_events.py::test_layer_validator_and_dataclass_agree
    pins the two implementations to the same accept/reject verdict on
    every rule below.

    Every field is type-checked, not just presence-checked: a wrong type
    (a string `ts`, a non-dict `ask`) must raise ValueError here, the same
    as it already did in opendaisugi.floor.events.PaneStateEvent.from_json
    — an uncaught TypeError from this function would escape
    hook_report_argv's `except ValueError` and crash the caller instead of
    returning exit code 1 (fix round 1, Finding 2). A `gate` `blocked`
    event with no ask is also rejected: without an ask's deadline, nothing
    can ever release that hold (fix round 1, Finding 3).
    """
    for key in ("session_id", "harness", "state", "source", "ts"):
        if key not in row:
            raise ValueError(f"event missing required field {key!r}")
    for key in ("session_id", "harness", "state", "source"):
        if not isinstance(row[key], str):
            raise ValueError(f"{key} must be a string")
    ts = row["ts"]
    if isinstance(ts, bool) or not isinstance(ts, (int, float)):
        raise ValueError("ts must be a number")
    if row["state"] not in STATES:
        raise ValueError(f"unknown state {row['state']!r}")
    if row["source"] not in SOURCES:
        raise ValueError(f"unknown source {row['source']!r}")
    if row["state"] == "done" and row["source"] not in ("process", "headless"):
        raise ValueError("state 'done' may only come from source 'process' or 'headless'")
    detail = row.get("detail", "")
    if not isinstance(detail, str):
        raise ValueError("detail must be a string")
    if len(detail) > _DETAIL_MAX:
        raise ValueError(f"detail exceeds {_DETAIL_MAX} characters")
    # harness_session_id and pane are optional. Absent or JSON null both
    # mean "not reported". A PRESENT non-string value is still rejected,
    # the same as opendaisugi.floor.events.PaneStateEvent.from_json rejects
    # it. hook_report_argv only overwrites row["pane"] when --pane was
    # passed, so a malformed pane or harness_session_id in the piped-in
    # JSON would otherwise reach the session tree and coppice unchecked.
    harness_session_id = row.get("harness_session_id")
    if harness_session_id is not None and not isinstance(harness_session_id, str):
        raise ValueError("harness_session_id must be a string")
    pane = row.get("pane")
    if pane is not None and not isinstance(pane, str):
        raise ValueError("pane must be a string")
    ask = row.get("ask")
    if ask is not None:
        if not isinstance(ask, dict):
            raise ValueError("ask must be an object")
        if row["state"] != "blocked":
            raise ValueError("ask is only valid when state == 'blocked'")
        for key in ("id", "tool", "summary", "deadline"):
            if key not in ask:
                raise ValueError(f"ask missing required field {key!r}")
        # Type-checked, not just presence-checked (Important 1, whole-branch
        # review): must agree with opendaisugi.floor.events.PaneStateEvent's
        # own ask checks, or the two validators accept different malformed
        # events (test_layer_validator_and_dataclass_agree_on_every_rule).
        for key in ("id", "tool", "summary"):
            if not isinstance(ask[key], str):
                raise ValueError(f"ask {key} must be a string")
        deadline = ask["deadline"]
        if isinstance(deadline, bool) or not isinstance(deadline, (int, float)):
            raise ValueError("ask deadline must be a number")
        # tier is optional; a present value must be a string. An unknown
        # string reads as permanent downstream, so it is not refused here.
        tier = ask.get("tier")
        if tier is not None and not isinstance(tier, str):
            raise ValueError("ask tier must be a string")
    if row["state"] == "blocked" and row["source"] == "gate" and ask is None:
        raise ValueError("a gate blocked event needs an ask")
    v = row.get("v", 1)
    if isinstance(v, bool) or not isinstance(v, int):
        raise ValueError(f"invalid schema version {v!r}")
    if v != 1:
        raise ValueError(f"unsupported event schema version {v!r}")


@dataclass(frozen=True)
class HookReportOutcome:
    stdout: str
    stderr: str
    exit_code: int


def _build_hook_report_parser():
    import argparse

    p = argparse.ArgumentParser(prog="daisugi hook report", add_help=False)
    p.add_argument("--pane", default=None)
    p.add_argument("--root", type=Path, default=_DEFAULT_ROOT)
    return p


def hook_report_argv(
    argv: list[str],
    raw: bytes,
    *,
    coppice_sock: str | None = None,
    coppice_pane: str | None = None,
    herdr_pane: str | None = None,
    peer_pid: int | None = None,
    env: Mapping[str, str] | None = None,
) -> HookReportOutcome:
    """`daisugi hook report [--pane P] [--root R]` — validate, downgrade a
    claimed gate/operator source to headless, append to the session tree,
    and deliver.

    Exit 0 once stdin parses to a JSON object AND passes validation; exit 1
    (message on stderr) for anything malformed. Never raises out to the
    caller — this runs on a socket handler's path (gate_server.py) too.

    ``coppice_sock``/``coppice_pane``/``herdr_pane`` are the caller's own
    pane identity, forwarded explicitly by gate_server.py's gate.sock
    dispatch from that one request's fields. Never read from this process's
    own environment, which belongs to the resident gate, not the caller.
    The direct CLI entry (`daisugi hook report`, cli.py) leaves all
    three unset, so it keeps reading the environment it actually shares
    with its caller (see report_state).

    ``peer_pid`` is the caller's real OS pid, read off the gate.sock
    connection itself with SO_PEERCRED by gate_server.py, never taken from
    anything the request body claims. Passed straight through to
    report_state, where it authenticates ``coppice_pane`` against coppice's
    own kernel-pid placement rather than trusting the claim as-is.

    ``env``, when given, is passed through to report_state's own ``env``
    argument too. A pane-free copy of the resident process's real
    environment, not an empty one, so a report_state call that ends up on
    the explicit herdr_pane leg still hands the herdr subprocess a real
    environment to run in rather than none at all. Left unset (the CLI
    entry's case), report_state keeps its own default of the live
    ``os.environ``, exactly as before this argument existed.
    """
    try:
        args = _build_hook_report_parser().parse_args(argv)
    except SystemExit:
        return HookReportOutcome("", "daisugi hook report: bad arguments", 1)
    try:
        row = json.loads(raw.decode("utf-8"))
    except Exception as exc:
        return HookReportOutcome("", f"daisugi hook report: not valid JSON: {exc}", 1)
    if not isinstance(row, dict):
        return HookReportOutcome("", "daisugi hook report: event must be a JSON object", 1)
    if row.get("source") in ("gate", "operator"):
        row["source"] = "headless"
    if args.pane is not None:
        row["pane"] = args.pane
    try:
        _validate_hook_report_row(row)
    except ValueError as exc:
        return HookReportOutcome("", f"daisugi hook report: {exc}", 1)
    try:
        from opendaisugi.session_tree import SessionTree

        tree = SessionTree.open_or_create(
            args.root.parent / "sessions",
            session_id=str(row["session_id"]),
            harness=str(row["harness"]),
            cwd="",
            harness_session_id=row.get("harness_session_id"),
        )
        tree.append("state", row)
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    try:
        report_kwargs: dict[str, Any] = {
            "sock": coppice_sock,
            "pane": coppice_pane,
            "herdr_pane": herdr_pane,
            "peer_pid": peer_pid,
        }
        if env is not None:
            report_kwargs["env"] = env
        report_state(json.dumps(row), **report_kwargs)
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    return HookReportOutcome("", "", 0)
