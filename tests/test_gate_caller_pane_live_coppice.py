"""Live-coppice proof: a caller cannot report as a pane that is not its own.

Security review finding: the resident gate's own connection to coppice
sits outside every pane's process tree, so a bare
``hello role pane pane=<claimed>`` was simply believed. Pane A's own
caller could put pane B's id in its request, and the resident gate's
connection to coppice had nothing of A's or B's to check that claim
against. The fix threads the caller's REAL OS pid (read with SO_PEERCRED
on the gate.sock connection itself, gate_server.py's own job, never taken
from the request body) through ``report_state`` as ``peer_pids`` on the
hello, so coppice's own kernel-pid placement (``placeNamedPeers``,
role.go) decides which pane the connection actually is, not the claim.

Every unit-level test elsewhere in this suite (tests/floor/test_report.py)
proves the CODE reads and checks the hello reply correctly, against a fake
coppice server the test itself scripts to answer however it likes. This
module is the one place that runs against a REAL, freshly built coppice
server with a REAL live pane and a REAL OS pid, so the claim "coppice's
own placement actually resolves peer_pids this way" is proven against the
real binary, not merely asserted about it.

ISOLATION: every coppice invocation here takes an explicit ``--socket``
under ``$XDG_RUNTIME_DIR`` and ``--data-dir`` under
``~/opendaisugi-scratch``, never the operator's own default socket, never
``~/.opendaisugi``, never ``/tmp``. ``COPPICE_NO_AUTOSTART=1`` is set on
the server process's own environment; every call this module makes
afterward goes straight at that one already-running server, never through
the CLI's own autostart path.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import time
import uuid
from pathlib import Path

import pytest

from opendaisugi import _state_report as sr
from tests.floor.coppice_sandbox import coppice_binary  # noqa: F401 (fixture)

SCRATCH_ROOT = Path.home() / "opendaisugi-scratch"


def _raw_call(sock_path: Path, req: dict, timeout: float = 5.0) -> dict:
    """One request, one reply, off a fresh connection. Independent of
    ``_state_report`` on purpose: this is the instrument that sets up and
    checks the real server's state, so it must not reuse the code under
    test to do it."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(str(sock_path))
    s.sendall((json.dumps(req) + "\n").encode())
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = s.recv(65536)
        if not chunk:
            break
        buf += chunk
    s.close()
    return json.loads(buf)


@pytest.fixture
def live_coppice(coppice_binary):
    """One real, isolated coppice server for one test: a unique socket
    under ``$XDG_RUNTIME_DIR``, a unique data dir under
    ``~/opendaisugi-scratch``. Never the operator's own default socket
    (``/run/user/*/coppice/server.sock``), never ``~/.opendaisugi``, never
    ``/tmp``. Skips (never fails) when the binary could not be built, or
    when ``$XDG_RUNTIME_DIR`` is not set on this box.
    """
    if coppice_binary.binary is None:
        pytest.skip(coppice_binary.reason)
    xdg_runtime = os.environ.get("XDG_RUNTIME_DIR")
    if not xdg_runtime:
        pytest.skip("XDG_RUNTIME_DIR is not set; the isolated socket needs a place to live")
    unique = uuid.uuid4().hex[:8]
    sock_path = Path(xdg_runtime) / f"cop-callerpane-test-{unique}.sock"
    data_dir = SCRATCH_ROOT / f"gate-caller-pane-live-{unique}"
    data_dir.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    env["COPPICE_NO_AUTOSTART"] = "1"
    proc = subprocess.Popen(
        [
            str(coppice_binary.binary),
            "--socket",
            str(sock_path),
            "--data-dir",
            str(data_dir),
            "server",
            "start",
            "--foreground",
        ],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    deadline = time.monotonic() + 10
    while not sock_path.exists():
        if proc.poll() is not None:
            pytest.skip("the isolated coppice server exited before it started")
        if time.monotonic() > deadline:
            proc.kill()
            pytest.skip("the isolated coppice server never created its socket in time")
        time.sleep(0.05)
    try:
        yield sock_path, data_dir
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
        try:
            sock_path.unlink()
        except OSError:
            pass
        shutil.rmtree(data_dir, ignore_errors=True)


def _spawn_real_pane(sock_path: Path, data_dir: Path, name: str) -> tuple[str, int]:
    """A real pty pane whose own process writes its own pid to a file, then
    sleeps. Returns ``(pane_id, real_pid)``: the pid coppice's own
    kernel-pid walk will find genuinely inside this pane's process tree,
    since it IS that pane's own child process, not a stand-in for it.
    """
    pidfile = data_dir / f"{name}.pid"
    created = _raw_call(
        sock_path,
        {
            "id": "c",
            "cmd": "pane.create",
            "cwd": str(data_dir),
            "kind": "pty",
            "cmd_argv": ["sh", "-c", f"echo $$ > {pidfile}; sleep 30"],
        },
    )
    assert created.get("ok"), created
    pane_id = created["result"]["pane"]
    deadline = time.monotonic() + 5
    while not pidfile.exists():
        if time.monotonic() > deadline:
            raise TimeoutError(f"pane {pane_id} never wrote its pid file")
        time.sleep(0.02)
    pid = int(pidfile.read_text().strip())
    return pane_id, pid


def _panes_by_id(sock_path: Path) -> dict:
    rows = _raw_call(sock_path, {"id": "l", "cmd": "pane.list"})["result"]["panes"]
    return {row["id"]: row for row in rows}


def _ev(session_id: str) -> str:
    return json.dumps(
        {
            "v": 1,
            "ts": time.time(),
            "session_id": session_id,
            "harness": "claude-code",
            "state": "working",
            "source": "gate",
            "detail": "",
        }
    )


def test_a_caller_whose_real_pid_is_in_pane_a_cannot_report_as_pane_b(live_coppice):
    """The attack this fix closes: pane A's own real process claims to be
    pane B. coppice's own placement (walking peer_pid's real ancestry)
    resolves the hello to A regardless of the claim, this function's own
    check catches the mismatch, and B's state is never touched."""
    sock_path, data_dir = live_coppice
    pane_a, pid_a = _spawn_real_pane(sock_path, data_dir, "a")
    pane_b, _pid_b = _spawn_real_pane(sock_path, data_dir, "b")
    assert pane_a != pane_b

    sink = sr.report_state(_ev("s-impersonation"), sock=str(sock_path), pane=pane_b, peer_pid=pid_a)
    assert sink == "none"

    row_b = _panes_by_id(sock_path)[pane_b]
    assert row_b.get("session_id") != "s-impersonation"
    row_a = _panes_by_id(sock_path)[pane_a]
    assert row_a.get("session_id") != "s-impersonation"  # not silently recorded on A either


def test_a_caller_whose_real_pid_is_in_pane_a_reports_correctly_as_pane_a(live_coppice):
    """The legitimate case must keep working: A reporting as A, with A's
    own real pid, succeeds and is visible on A's own row."""
    sock_path, data_dir = live_coppice
    pane_a, pid_a = _spawn_real_pane(sock_path, data_dir, "a")

    sink = sr.report_state(_ev("s-legit"), sock=str(sock_path), pane=pane_a, peer_pid=pid_a)
    assert sink == "coppice"

    row_a = _panes_by_id(sock_path)[pane_a]
    assert row_a.get("session_id") == "s-legit"


def test_without_peer_pid_the_claim_is_simply_believed_the_pre_fix_gap(live_coppice):
    """Documents the gap this fix closes, against the real server: with no
    peer_pids evidence at all, coppice has nothing to check a hello's
    claimed pane against, so the report goes through exactly as claimed.
    This is why gate_server.py must always supply the caller's real pid
    for a resident-forwarded report, not just when it feels like it."""
    sock_path, data_dir = live_coppice
    pane_b, _pid_b = _spawn_real_pane(sock_path, data_dir, "b")

    sink = sr.report_state(_ev("s-no-evidence"), sock=str(sock_path), pane=pane_b, peer_pid=None)
    assert sink == "coppice"

    row_b = _panes_by_id(sock_path)[pane_b]
    assert row_b.get("session_id") == "s-no-evidence"
