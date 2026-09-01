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
import shutil
import subprocess
import time
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")

HERDR_PANE_ENV_CANDIDATES: tuple[str, ...] = ("HERDR_PANE_ID", "HERDR_PANE")

_DETAIL_MAX = 200
_DEFAULT_ROOT = Path.home() / ".opendaisugi" / "gate"  # mirrors opendaisugi.gate.DEFAULT_GATE_ROOT
_HERDR_STATE_MAP = {"working": "working", "blocked": "blocked", "idle": "idle", "done": "idle"}


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
) -> str:
    """Serialize one PaneStateEvent-shaped JSON line (master spec §3.1).

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
    return json.dumps(row)


def report_state(
    ev_json: str, *, env: Mapping[str, str] = os.environ, budget_s: float = 0.2
) -> str:
    """Deliver one PaneStateEvent line. Returns which sink took it:
    ``'coppice' | 'herdr' | 'none'``.

    coppice wins when both COPPICE_SOCK/COPPICE_PANE and a Herdr pane env
    var are present (a Herdr pane hosting coppice is not a supported
    nesting — first match wins). Never raises; any failure is 'none'.

    ``budget_s`` is a TOTAL wall-clock budget, not a per-operation one: one
    deadline is taken up front and the remaining time is re-applied before
    each of connect/sendall/recv (and to the Herdr subprocess wait), so a
    wedged peer costs at most ~budget_s on the gate's blocking hot path,
    never 2x or 3x that. The Herdr path uses Popen + wait(timeout=) + kill()
    with stdout/stderr sent to DEVNULL rather than subprocess.run(timeout=):
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

    sock_path = env.get("COPPICE_SOCK")
    coppice_pane = env.get("COPPICE_PANE")
    if sock_path and coppice_pane:
        import socket

        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                s.settimeout(_remaining())
                s.connect(sock_path)
                s.settimeout(_remaining())
                req = (
                    json.dumps(
                        {"id": "r", "cmd": "pane.report_state", "pane": coppice_pane, "event": ev}
                    )
                    + "\n"
                )
                s.sendall(req.encode("utf-8"))
                s.settimeout(_remaining())
                s.recv(65536)
            return "coppice"
        except Exception:
            return "none"
    herdr_pane = next((env[k] for k in HERDR_PANE_ENV_CANDIDATES if env.get(k)), None)
    if herdr_pane:
        mapped = _HERDR_STATE_MAP.get(ev.get("state"))
        if mapped is None:
            return "none"  # e.g. 'unknown' — Herdr's CLI has no such state
        herdr_bin = shutil.which("herdr", path=env.get("PATH"))
        if not herdr_bin:
            return "none"
        try:
            proc = subprocess.Popen(
                [
                    herdr_bin,
                    "pane",
                    "report-agent",
                    herdr_pane,
                    "--source",
                    "daisugi",
                    "--agent",
                    str(ev.get("harness") or ""),
                    "--state",
                    mapped,
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                env=dict(env),
            )
            try:
                proc.wait(timeout=_remaining())
            except subprocess.TimeoutExpired:
                proc.kill()
                # A short, bounded reap (whole-branch review, minor 3): this
                # module also runs inside the long-lived resident gate, not
                # just the ~200ms hot path, so a 1 s wait() here could add a
                # full second past the caller's budget on every wedged
                # herdr. kill()'s SIGKILL reaps almost instantly in the
                # ordinary case; TimeoutExpired from THIS wait is swallowed
                # too, not left to escape to the outer except — a process
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


def hook_report_argv(argv: list[str], raw: bytes) -> HookReportOutcome:
    """`daisugi hook report [--pane P] [--root R]` — validate, downgrade a
    claimed gate/operator source to headless, append to the session tree,
    and deliver.

    Exit 0 once stdin parses to a JSON object AND passes validation; exit 1
    (message on stderr) for anything malformed. Never raises out to the
    caller — this runs on a socket handler's path (gate_server.py) too.
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
        report_state(json.dumps(row))
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    return HookReportOutcome("", "", 0)
