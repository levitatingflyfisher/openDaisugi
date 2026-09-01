"""report_state (master spec §3.1's delivery side): coppice socket / herdr
CLI / no-op, and the Herdr pane-id discovery (spec-01 plan task 1)."""

from __future__ import annotations

import json
import socket
import threading
import time

import pytest

from opendaisugi._state_report import HERDR_PANE_ENV_CANDIDATES
from opendaisugi.floor.report import report_state


def _ev(**over):
    base = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s1",
        "harness": "claude-code",
        "state": "working",
        "source": "gate",
        "detail": "",
    }
    base.update(over)
    return json.dumps(base)


def test_herdr_pane_env_candidates_are_recorded_in_fallback_order():
    assert HERDR_PANE_ENV_CANDIDATES == ("HERDR_PANE_ID", "HERDR_PANE")


def test_discovery_fact_is_recorded_verified_or_honestly_flagged_unverified():
    import opendaisugi._state_report as mod

    doc = mod.__doc__ or ""
    assert "UNVERIFIED" in doc or "confirmed" in doc


def test_coppice_socket_receives_the_event(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    received = []
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        line = conn.makefile("rb").readline()
        received.append(json.loads(line))
        conn.sendall(b'{"ok": true}\n')
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    sink = report_state(_ev(), env={"COPPICE_SOCK": str(sock_path), "COPPICE_PANE": "w1:p1"})
    t.join(2)
    assert sink == "coppice"
    assert received[0]["id"] == "r"
    assert received[0]["cmd"] == "pane.report_state"
    assert received[0]["pane"] == "w1:p1"
    assert received[0]["event"]["session_id"] == "s1"


def test_herdr_cli_receives_the_exact_argv(tmp_path):
    log = tmp_path / "argv.log"
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text(f'#!/bin/sh\necho "$@" > {log}\n')
    herdr_script.chmod(0o755)
    sink = report_state(
        _ev(state="blocked"),
        env={
            "HERDR_PANE_ID": "w1:p1",
            "PATH": str(tmp_path),
        },
    )
    assert sink == "herdr"
    argv_line = log.read_text().strip()
    assert (
        argv_line == "pane report-agent w1:p1 --source daisugi --agent claude-code --state blocked"
    )


def test_herdr_pane_fallback_name_is_honored(tmp_path):
    log = tmp_path / "argv.log"
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text(f'#!/bin/sh\necho "$@" > {log}\n')
    herdr_script.chmod(0o755)
    sink = report_state(_ev(), env={"HERDR_PANE": "w1:p9", "PATH": str(tmp_path)})
    assert sink == "herdr"
    assert "w1:p9" in log.read_text()


def test_coppice_wins_when_both_env_vars_present(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        conn.recv(65536)
        conn.sendall(b'{"ok": true}\n')
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    sink = report_state(
        _ev(),
        env={
            "COPPICE_SOCK": str(sock_path),
            "COPPICE_PANE": "w1:p1",
            "HERDR_PANE_ID": "w1:p9",
        },
    )
    t.join(2)
    assert sink == "coppice"


def test_no_sink_present_returns_none():
    assert report_state(_ev(), env={}) == "none"


def test_unknown_state_to_herdr_is_skipped(tmp_path):
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)
    sink = report_state(_ev(state="unknown"), env={"HERDR_PANE_ID": "w1:p1", "PATH": str(tmp_path)})
    assert sink == "none"


@pytest.mark.parametrize("bad_json", ["42", "null", "[1,2]", '"hi"'])
def test_report_state_returns_none_for_a_non_object_payload(tmp_path, bad_json):
    """report_state must never raise: a well-formed JSON value that isn't an
    object has no .get(), and the Herdr branch calls ev.get('state') /
    ev.get('harness') without checking first. The coppice branch never calls
    .get() on ev (it embeds ev whole into the outgoing envelope), so it
    already returns 'none' for these inputs without any fix; both are
    exercised here so a future regression on either path is caught."""
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)
    herdr_env = {"HERDR_PANE_ID": "p1", "PATH": str(tmp_path)}
    assert report_state(bad_json, env=herdr_env) == "none"

    coppice_env = {"COPPICE_SOCK": str(tmp_path / "coppice.sock"), "COPPICE_PANE": "w1:p1"}
    assert report_state(bad_json, env=coppice_env) == "none"


def test_herdr_kill_reap_uses_a_short_timeout_not_the_full_budget(monkeypatch, tmp_path):
    """Whole-branch review, minor 3: after kill(), the reap wait() must be
    bounded to a short cap (0.05 s), not the old 1.0 s — this module also
    runs inside the long-lived resident gate, not just the ~200ms hot
    path, so a herdr stuck past its deadline must not cost the caller a
    full extra second on top of that.

    Patches subprocess.Popen with a fake whose .wait() always raises
    TimeoutExpired — the same as a process stuck in an uninterruptible
    D-state would even after SIGKILL — so the timeout value passed to the
    SECOND wait() call (the reap after kill()) can be asserted directly,
    without needing to reproduce a real D-state hang."""
    import subprocess as subprocess_mod

    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)

    wait_calls: list[float | None] = []
    killed = []

    class _FakeProc:
        def wait(self, timeout=None):
            wait_calls.append(timeout)
            raise subprocess_mod.TimeoutExpired(cmd="herdr", timeout=timeout or 0)

        def kill(self):
            killed.append(True)

    monkeypatch.setattr(subprocess_mod, "Popen", lambda *a, **k: _FakeProc())
    start = time.monotonic()
    sink = report_state(_ev(), env={"HERDR_PANE_ID": "w1:p1", "PATH": str(tmp_path)}, budget_s=0.05)
    elapsed = time.monotonic() - start
    assert sink == "none"
    assert killed == [True]
    assert len(wait_calls) == 2
    # The reap wait (after kill()) must be capped well under the old 1.0 s.
    assert wait_calls[1] is not None and wait_calls[1] <= 0.1
    # TimeoutExpired from that reap is swallowed here rather than left to
    # unwind to the outer handler; the bounded elapsed time is the
    # externally-observable proof it never waited anywhere near 1.0 s.
    assert elapsed < 0.3


def test_budget_exceeded_returns_none_quickly(tmp_path):
    """A real socket that ACCEPTS the connection and never replies — not a
    fast ENOENT — so this actually exercises recv()'s share of the total
    budget, not just a fast connection-refused path. budget_s is a TOTAL
    wall-clock budget (§ the crux): connect + sendall + recv together must
    never cost more than ~budget_s, not up to 3x it."""
    sock_path = tmp_path / "wedged.sock"
    ready = threading.Event()
    stop = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        stop.wait(5)  # accept the connection, then never reply, never close
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    budget_s = 0.1
    start = time.monotonic()
    sink = report_state(
        _ev(),
        env={
            "COPPICE_SOCK": str(sock_path),
            "COPPICE_PANE": "w1:p1",
        },
        budget_s=budget_s,
    )
    elapsed = time.monotonic() - start
    stop.set()
    t.join(2)
    assert sink == "none"
    assert elapsed < budget_s + 0.15
