"""The resident gate: one process holds the verifier warm; the hook talks to it.

One cold gate call is ~0.7 s and ~98% of it is import. Verification itself is
0.6 to 4 ms. The server amortizes the import; the client (gate_client.py) can
only fall back to the in-process gate, never to allow.
"""

from __future__ import annotations

import base64
import json
import os
import socketserver
import threading
from pathlib import Path

SOCK_NAME = "gate.sock"
_MAX_REQUEST = 4 * 1024 * 1024


class _Handler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        from opendaisugi.gate import run_argv

        line = self.rfile.readline(_MAX_REQUEST)
        try:
            req = json.loads(line)
            argv = [str(a) for a in req["argv"]]
            raw = base64.b64decode(req.get("stdin_b64", ""))
        except Exception:  # noqa: BLE001 — a bad request gets a deny, not a crash
            reply = {
                "v": 1,
                "stdout": "",
                "stderr": "openDaisugi gate: DENIED — bad request",
                "exit_code": 2,
            }
        else:
            if argv[:2] == ["hook", "report"]:
                from opendaisugi._state_report import hook_report_argv

                out = hook_report_argv(argv[2:], raw)
            else:
                out = run_argv(argv, raw)
            reply = {"v": 1, "stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
        self.wfile.write(json.dumps(reply).encode() + b"\n")


class _Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    allow_reuse_address = True
    # A slow verdict (a future plan can block a gate call for up to 90s on an
    # operator) must not freeze every other session's fast calls — see
    # serve()'s docstring for the concurrency tradeoff this makes.
    daemon_threads = True


def serve(
    root: Path, *, ready: threading.Event | None = None, stop: threading.Event | None = None
) -> None:
    """Serve gate verdicts on ``<root>/gate.sock`` until ``stop`` is set (or forever).

    Thread-per-connection (``ThreadingMixIn``): a later plan blocks a gate
    call for up to 90 s waiting on an operator, and a single-connection
    server would freeze every other session for that whole window. A stale
    socket file from a dead server is removed.

    ``z3_checks``/``predicate_z3``/``subsumption``/``vacuity`` call bare
    ``z3.Solver()`` — the process-wide default Z3 context — and Z3 releases
    the GIL for native work, so two handler threads inside Z3 at once is a
    genuine data race on shared native state, not merely a crash risk: it
    can produce a wrong, non-crashing verdict. ``z3_checks.Z3_SOLVE_LOCK``
    now serializes every AST-build-and-solve region reachable from
    ``verify()`` (the two ``z3_checks`` functions,
    ``predicate_z3.verify_predicate_z3`` including its ``compile_to_z3``
    call, ``subsumption``'s two solve sites, and ``vacuity``'s contradiction
    + tautology checks). Solves are 0.6-4 ms, so serializing them is free.

    That lock does not close a narrower race it surfaced: a guarded
    function's local Z3 objects are freed when its stack frame is torn
    down, which happens *after* the lock is released — so native ref-count
    decrements (``Z3_dec_ref``) can run off-lock. This is not confined to
    process exit: that off-lock decrement can fire during normal operation
    too, overlapping another thread's in-flight *locked* solve on the same
    shared context. What we actually observed — ``Z3_del_context`` spinning
    at process exit — is accumulated corruption from that overlap
    surfacing when the context finally tears down, not proof the overlap
    itself only happens at exit. The residual fail-open window this leaves
    is structurally bounded, not closed: ``verify()``'s ``allow`` is
    exactly ``violations == []``, so a race has to manufacture that precise
    empty state rather than corrupt arbitrary bookkeeping; violation lists
    are extend-only through the pipeline; the pure-Python Stage-1
    permission check (no Z3 involved) already denies most unsafe plans
    before any solve runs; ``enforce`` denies on any exception, and
    corrupted native state is a plausible way to raise one, not to quietly
    return "allow"; and on this server's own fail-closed fallback path, any
    anomaly routes to a fresh in-process gate on a single, uncontended Z3
    context — no concurrency there, so no version of this race. None of
    that makes the window zero. The airtight fix is a ``z3.Context()`` per
    thread, threaded through the verify pipeline — deferred as a known
    residual, not attempted here; see ADR-0017.
    """
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(root, 0o700)  # mkdir's mode= is a no-op on a pre-existing dir
    sock = root / SOCK_NAME
    if sock.exists():
        sock.unlink()
    with _Server(str(sock), _Handler) as srv:
        os.chmod(sock, 0o600)
        srv.timeout = 0.2
        if ready is not None:
            ready.set()
        while stop is None or not stop.is_set():
            srv.handle_request()
    try:
        sock.unlink()
    except OSError:
        pass
