"""report_state (master spec §3.1's delivery side): coppice socket / herdr
CLI / no-op, and the Herdr pane-id discovery (spec-01 plan task 1)."""

from __future__ import annotations

import json
import os
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
    # herdr_pane is last, after "--": a flag AFTER it in argv would be
    # misread as positional too under standard "--" semantics, so the
    # pane id is the one thing that can safely follow it.
    assert (
        argv_line
        == "pane report-agent --source daisugi --agent claude-code --state blocked -- w1:p1"
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


# --- explicit sock/pane/herdr_pane: the resident-gate-forwarded path ------


def _fake_coppice(sock_path, replies=None):
    """A fake coppice server that records every line it receives (each one
    JSON-decoded) and replies once per line, ``{"ok": true}`` by default.
    Returns the ``received`` list the caller reads after joining the thread.
    """
    received: list = []
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        f = conn.makefile("rb")
        i = 0
        while True:
            line = f.readline()
            if not line:
                break
            received.append(json.loads(line))
            body = (replies[i] if replies else '{"ok": true}').encode() + b"\n"
            i += 1
            try:
                conn.sendall(body)
            except OSError:
                break
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    return received, t


def test_explicit_pane_sends_hello_before_pane_report_state(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    hello_ok = '{"ok": true, "result": {"role": "pane", "pane": "w1:p3"}}'
    received, t = _fake_coppice(sock_path, replies=[hello_ok, '{"ok": true}'])
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p3")
    t.join(2)
    assert sink == "coppice"
    assert len(received) == 2
    assert received[0] == {"id": "h", "cmd": "hello", "role": "pane", "pane": "w1:p3"}
    assert received[1]["cmd"] == "pane.report_state"
    assert received[1]["pane"] == "w1:p3"
    assert received[1]["event"]["session_id"] == "s1"


def test_hello_carries_peer_pid_as_peer_pids(tmp_path):
    """peer_pid is the caller's real OS pid, read by gate_server.py with
    SO_PEERCRED on the connection that reached it, and it rides on the
    hello as peer_pids so coppice's own kernel-pid placement (not this
    module's say-so) decides which pane, if any, that pid belongs to."""
    sock_path = tmp_path / "coppice.sock"
    hello_ok = '{"ok": true, "result": {"role": "pane", "pane": "w1:p3"}}'
    received, t = _fake_coppice(sock_path, replies=[hello_ok, '{"ok": true}'])
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p3", peer_pid=4242)
    t.join(2)
    assert sink == "coppice"
    assert received[0]["peer_pids"] == [4242]


def test_no_peer_pid_omits_peer_pids_from_the_hello(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    hello_ok = '{"ok": true, "result": {"role": "pane", "pane": "w1:p3"}}'
    received, t = _fake_coppice(sock_path, replies=[hello_ok, '{"ok": true}'])
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p3")
    t.join(2)
    assert sink == "coppice"
    assert "peer_pids" not in received[0]


def test_a_hello_reply_that_disagrees_with_the_claim_sends_no_report(tmp_path):
    """The core protection: coppice's own kernel-pid placement (walking
    peer_pids) can silently resolve a hello to a DIFFERENT pane than the
    one claimed. The caller's real pid genuinely lives inside that other
    pane's process tree. When the hello reply says a different pane than
    the one asked for, the report itself must never be sent, and the
    return value must say so (never 'coppice' for a claim coppice did not
    confirm)."""
    sock_path = tmp_path / "coppice.sock"
    disagreeing_hello = '{"ok": true, "result": {"role": "pane", "pane": "w1:p1"}}'
    received, t = _fake_coppice(sock_path, replies=[disagreeing_hello, '{"ok": true}'])
    sink = report_state(_ev(), sock=str(sock_path), pane="w9:p9", peer_pid=1234)
    t.join(2)
    assert sink == "none"
    assert len(received) == 1  # the hello only; pane.report_state was never sent
    assert received[0]["pane"] == "w9:p9"  # the claim it made, not what coppice answered


def test_a_hello_reply_with_no_result_sends_no_report(tmp_path):
    """A bare {"ok": true} (this module's own OLD behavior, and a fake
    server that has not been taught the real hello reply shape) must not
    be read as confirmation. No result means no proof coppice placed
    this connection as the claimed pane at all."""
    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(sock_path, replies=['{"ok": true}', '{"ok": true}'])
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p1")
    t.join(2)
    assert sink == "none"
    assert len(received) == 1


def test_a_hello_reply_with_ok_false_sends_no_report(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(
        sock_path, replies=['{"ok": false, "error": {"code": "bad_request"}}', '{"ok": true}']
    )
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p1")
    t.join(2)
    assert sink == "none"
    assert len(received) == 1


def test_a_report_reply_with_ok_false_is_not_read_as_delivered(tmp_path):
    """The hello confirms the claimed pane, but pane.report_state itself
    can still answer an error (a malformed event, say). That must read as
    'none' too, not 'coppice'. The return value means coppice accepted
    the report, not merely that the hello succeeded."""
    sock_path = tmp_path / "coppice.sock"
    hello_ok = '{"ok": true, "result": {"role": "pane", "pane": "w1:p1"}}'
    received, t = _fake_coppice(
        sock_path, replies=[hello_ok, '{"ok": false, "error": {"code": "bad_request"}}']
    )
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p1")
    t.join(2)
    assert sink == "none"
    assert len(received) == 2  # both were sent; the SECOND reply is what refused


def test_a_post_connect_peer_uid_mismatch_sends_nothing(tmp_path, monkeypatch):
    """Beyond the pre-connect lstat (which already passes here: sock_path
    really is our own socket at the moment it is checked), the peer on
    the far end of the connection actually made is checked again with
    SO_PEERCRED right after connect(), closing the gap an intermediate
    directory symlink swapped between that lstat and this connect could
    otherwise open. This box cannot fake a second real uid, so
    socket.socket.getsockopt is patched to answer SO_PEERCRED with one,
    isolating the POST-connect check specifically: os.getuid() itself
    stays real, so the pre-connect lstat check is untouched and this
    failure can only come from the later getsockopt call."""
    import socket as socket_mod
    import struct as struct_mod

    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(sock_path)
    real_getsockopt = socket_mod.socket.getsockopt

    def _fake_getsockopt(self, level, optname, buflen=0):
        if optname == socket_mod.SO_PEERCRED:
            return struct_mod.pack("3i", 1, os.getuid() + 99999, 0)
        return real_getsockopt(self, level, optname, buflen)

    monkeypatch.setattr(socket_mod.socket, "getsockopt", _fake_getsockopt)
    sink = report_state(_ev(), sock=str(sock_path), pane="w1:p1")
    assert sink == "none"
    assert received == []


def test_explicit_mode_never_falls_back_to_env(tmp_path):
    """Explicit mode is triggered by herdr_pane alone here; the env carries
    a perfectly valid COPPICE_SOCK/COPPICE_PANE pair, which must be ignored
    entirely. A resident call reports ONLY on the request fields it was
    given, never on anything env happens to carry."""
    sock_path = tmp_path / "coppice.sock"
    received = []
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        srv.settimeout(1)
        try:
            conn, _ = srv.accept()
            received.append(1)
            conn.close()
        except OSError:
            pass
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)

    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)

    sink = report_state(
        _ev(),
        env={"COPPICE_SOCK": str(sock_path), "COPPICE_PANE": "w1:p1", "PATH": str(tmp_path)},
        herdr_pane="w2:p9",
    )
    t.join(2)
    assert sink == "herdr"
    assert received == [], "the coppice socket in env was dialed despite explicit herdr_pane"


@pytest.mark.parametrize(
    "kwargs",
    [
        {"sock": "will-be-set", "pane": None},
        {"sock": None, "pane": "w1:p1"},
    ],
)
def test_an_incomplete_explicit_pair_reports_nothing(tmp_path, kwargs):
    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(sock_path)
    if kwargs["sock"] == "will-be-set":
        kwargs = {**kwargs, "sock": str(sock_path)}
    sink = report_state(
        _ev(),
        env={"COPPICE_SOCK": str(sock_path), "COPPICE_PANE": "w1:p1"},
        **kwargs,
    )
    assert sink == "none"
    assert received == []


def test_explicit_sock_must_be_absolute(tmp_path):
    sink = report_state(_ev(), sock="relative/coppice.sock", pane="w1:p1")
    assert sink == "none"


@pytest.mark.parametrize(
    "bad_pane",
    [
        "p1",
        "w1",
        "w1:1",
        "1:p1",
        "w1:p1 ",
        "",
        "w:p1",
        "w1:p3\n",  # trailing newline: "$" alone matches just before one
        "w" + "9" * 100 + ":p1",  # digits capped at 9, this has 100
    ],
)
def test_explicit_pane_must_match_coppices_own_shape(tmp_path, bad_pane):
    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(sock_path)
    sink = report_state(_ev(), sock=str(sock_path), pane=bad_pane)
    assert sink == "none"
    assert received == []


def test_a_pane_id_at_the_nine_digit_cap_is_still_valid(tmp_path):
    """The cap is on digit COUNT, not on plausibility: nine 9s is still a
    shape coppice's own format could produce, and must not be refused for
    being merely large."""
    sock_path = tmp_path / "coppice.sock"
    received, t = _fake_coppice(
        sock_path,
        replies=[
            '{"ok": true, "result": {"role": "pane", "pane": "w999999999:p999999999"}}',
            '{"ok": true}',
        ],
    )
    sink = report_state(_ev(), sock=str(sock_path), pane="w999999999:p999999999")
    t.join(2)
    assert sink == "coppice"


def test_explicit_sock_must_be_a_real_socket_not_a_regular_file(tmp_path):
    """The practical stand-in for 'owned by another uid': anything at the
    path that lstat says is not a socket this process opened is refused the
    same way, without needing a second real uid to prove it."""
    not_a_socket = tmp_path / "coppice.sock"
    not_a_socket.write_text("not a socket")
    sink = report_state(_ev(), sock=str(not_a_socket), pane="w1:p1")
    assert sink == "none"


def test_explicit_sock_rejects_a_symlink_to_a_real_socket(tmp_path):
    real_dir = tmp_path / "real"
    real_dir.mkdir()
    real_sock = real_dir / "coppice.sock"
    _received, t = _fake_coppice(real_sock)
    link = tmp_path / "link.sock"
    link.symlink_to(real_sock)
    sink = report_state(_ev(), sock=str(link), pane="w1:p1")
    assert sink == "none"


def test_explicit_herdr_pane_receives_the_exact_argv(tmp_path):
    log = tmp_path / "argv.log"
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text(f'#!/bin/sh\necho "$@" > {log}\n')
    herdr_script.chmod(0o755)
    sink = report_state(_ev(state="blocked"), env={"PATH": str(tmp_path)}, herdr_pane="w1:p1")
    assert sink == "herdr"
    argv_line = log.read_text().strip()
    assert (
        argv_line
        == "pane report-agent --source daisugi --agent claude-code --state blocked -- w1:p1"
    )


@pytest.mark.parametrize(
    "bad_herdr_pane",
    [
        "-w1:p1",  # a leading dash: the exact shape "--" defends against
        "--flag",
        "",
        "x" * 200,  # over the 128-byte cap
        "has spaces",
    ],
)
def test_a_malformed_herdr_pane_id_is_refused(tmp_path, bad_herdr_pane):
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)
    sink = report_state(_ev(), env={"PATH": str(tmp_path)}, herdr_pane=bad_herdr_pane)
    assert sink == "none"


def test_explicit_mode_with_no_matches_reports_nothing(tmp_path):
    """sock/pane both None but the caller still says something explicit
    (an empty-string herdr_pane, say) must still refuse to fall back to
    env. It is explicit mode, just with nothing usable in it."""
    sink = report_state(
        _ev(),
        env={"COPPICE_SOCK": str(tmp_path / "x.sock"), "COPPICE_PANE": "w1:p1"},
        herdr_pane="",
    )
    assert sink == "none"


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
