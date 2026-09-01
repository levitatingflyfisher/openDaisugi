"""The coppice socket client, against a fake JSONL server in a thread.

The real binary is optional; a handful of tests ask for the `coppice_server`
sandbox fixture and skip with its own reason when the toolchain is absent.
What must hold everywhere: `available()` never raises, a server error is an
error and never an empty success, and autostart is off unless the caller
asks for it. Opening a screen must not spawn a daemon.
"""

from __future__ import annotations

import json
import os
import socket
import threading
import time
from pathlib import Path

import pytest

from opendaisugi.floor import ERROR_CODES, Frame, PaneRef, PaneStateEvent
from opendaisugi.floor.coppice_backend import (
    KNOWN_KEYS,
    CoppiceBackend,
    CoppiceError,
    default_data_dir,
    default_socket_path,
)
from tests.floor import hostfacts
from tests.floor.coppice_sandbox import coppice_binary, coppice_server  # noqa: F401


class FakeServer:
    """One JSONL request per line, one reply per line. Handlers keyed by cmd.

    Replies use the real server's shapes. `pane.list` rows key `id`, carry
    argv under `cmd`, and carry `state`, `source`, `detail`, an optional
    `ask`, `ts` and `session_id` flat on the row rather than nested.
    Measured against a real sandboxed server; see
    `test_fake_pane_list_row_matches_a_recorded_real_reply`.
    """

    def __init__(
        self,
        path: Path,
        handlers: dict,
        *,
        events: list[dict] | None = None,
        subscribe_error: dict | None = None,
    ):
        self.path = path
        self.handlers = handlers
        self.events = events or []
        self.subscribe_error = subscribe_error
        self.seen: list[dict] = []
        self.connections = 0
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.bind(str(path))
        os.chmod(path, 0o600)
        self._sock.listen(8)
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def _serve(self):
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except OSError:
                return
            self.connections += 1
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn):
        # attach()/detach() leave their connection open and unread past the
        # test's own assertions; when the owning CoppiceBackend is garbage
        # collected the socket closes out from under this thread's blocking
        # read. That is an ordinary client hangup, not a test failure - it
        # is caught here rather than left to crash this daemon thread and
        # print a PytestUnhandledThreadExceptionWarning for something no
        # test actually got wrong.
        try:
            with conn, conn.makefile("rwb") as stream:
                for line in stream:
                    req = json.loads(line)
                    self.seen.append(req)
                    if req.get("cmd") == "events.subscribe":
                        if self.subscribe_error is not None:
                            stream.write(
                                json.dumps(
                                    {
                                        "id": req["id"],
                                        "ok": False,
                                        "error": self.subscribe_error,
                                    }
                                ).encode()
                                + b"\n"
                            )
                            stream.flush()
                            return
                        stream.write(json.dumps({"id": req["id"], "ok": True}).encode() + b"\n")
                        stream.flush()
                        for event in self.events:
                            stream.write(json.dumps(event).encode() + b"\n")
                            stream.flush()
                        return
                    handler = self.handlers.get(req.get("cmd"))
                    reply = (
                        handler(req)
                        if handler
                        else {
                            "id": req.get("id"),
                            "ok": False,
                            "error": {"code": "bad_request", "message": "no handler"},
                        }
                    )
                    stream.write(json.dumps(reply).encode() + b"\n")
                    stream.flush()
        except OSError:
            return

    def close(self):
        self._stop.set()
        self._sock.close()


@pytest.fixture
def server_factory(tmp_path):
    made: list[FakeServer] = []

    def make(handlers, events=None, subscribe_error=None):
        srv = FakeServer(
            tmp_path / "server.sock", handlers, events=events, subscribe_error=subscribe_error
        )
        made.append(srv)
        return srv

    yield make
    for srv in made:
        srv.close()


@pytest.fixture(autouse=True)
def _default_no_autostart(monkeypatch):
    """Every test here keeps COPPICE_NO_AUTOSTART set. A test that genuinely
    wants the real binary asks for the coppice_server fixture, which starts
    and stops its own server explicitly and is exempt from this by design.
    See coppice_sandbox._run_cli."""
    monkeypatch.setenv("COPPICE_NO_AUTOSTART", "1")


def _wait_until(predicate, timeout: float = 2.0, interval: float = 0.01) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(interval)
    raise AssertionError("condition was not met before the timeout")


def test_default_socket_prefers_xdg_runtime_dir(tmp_path):
    got = default_socket_path({"XDG_RUNTIME_DIR": str(tmp_path)})
    assert got == tmp_path / "coppice" / "server.sock"


def test_default_socket_falls_back_to_the_data_dir():
    got = default_socket_path({})
    assert got == Path.home() / ".opendaisugi" / "coppice" / "server.sock"


def test_default_data_dir_is_never_the_runtime_dir(monkeypatch):
    monkeypatch.setenv("XDG_RUNTIME_DIR", "/run/user/1000")
    assert default_data_dir() == Path.home() / ".opendaisugi" / "coppice"


def test_available_is_false_with_no_socket_and_never_raises(tmp_path):
    backend = CoppiceBackend(sock_path=tmp_path / "nothing.sock")
    assert backend.available() is False


def test_list_read_and_close_never_raise_when_the_socket_is_entirely_gone(tmp_path):
    """Master §3.2: only spawn, send_text and send_keys may raise. A socket
    path with nothing listening on it, and no directory even present, is
    the honest shape of a host that has gone away."""
    backend = CoppiceBackend(sock_path=tmp_path / "gone" / "server.sock", data_dir=tmp_path)
    assert backend.list() == []
    assert backend.read(PaneRef("coppice", "x")) == ""
    backend.close(PaneRef("coppice", "x"))  # must not raise


def test_list_proven_is_false_when_the_socket_is_entirely_gone(tmp_path):
    backend = CoppiceBackend(sock_path=tmp_path / "gone" / "server.sock", data_dir=tmp_path)
    assert backend.list_proven() == (False, [])


def test_list_proven_is_true_with_rows_on_a_real_answer(tmp_path, server_factory):
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"panes": [{"id": "w1:p1", "label": "l", "cwd": "/", "cmd": ["sh"]}]},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    proven, rows = backend.list_proven()
    assert proven is True
    assert [i.ref.id for i in rows] == ["w1:p1"]


def test_available_does_not_start_a_server_unless_asked(tmp_path, monkeypatch):
    calls = []
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", lambda *a, **k: calls.append(a) or None)
    assert CoppiceBackend(sock_path=tmp_path / "nothing.sock").available() is False
    assert calls == [], "opening the floor must not spawn a daemon by itself"


def test_autostart_tries_once_and_stays_false_when_the_start_fails(tmp_path, monkeypatch):
    calls = []
    # This test wants the genuine spawn attempt, not the autouse
    # COPPICE_NO_AUTOSTART every other test here keeps set.
    monkeypatch.delenv("COPPICE_NO_AUTOSTART", raising=False)

    def fake_run(argv, **kwargs):
        calls.append(argv)
        raise OSError("no such binary")

    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", fake_run)
    backend = CoppiceBackend(sock_path=tmp_path / "nothing.sock", autostart=True)
    assert backend.available() is False
    assert backend.available() is False
    assert len(calls) == 1, "autostart must try once per process, not on every probe"


def test_try_start_passes_socket_and_data_dir(tmp_path, monkeypatch):
    calls = []
    monkeypatch.delenv("COPPICE_NO_AUTOSTART", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", lambda argv, **k: calls.append(argv) or None)
    sock = tmp_path / "nothing.sock"
    data = tmp_path / "data"
    backend = CoppiceBackend(sock_path=sock, data_dir=data, autostart=True)
    backend.available()
    assert calls == [["coppice", "--socket", str(sock), "--data-dir", str(data), "server", "start"]]


def test_try_start_honours_no_autostart_without_spawning(tmp_path, monkeypatch):
    """Minor 4: the Python side checks COPPICE_NO_AUTOSTART itself, before
    even looking up the binary, rather than relying only on the real
    coppice process to refuse."""
    calls = []
    monkeypatch.setenv("COPPICE_NO_AUTOSTART", "1")
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", lambda *a, **k: calls.append(a) or None)
    backend = CoppiceBackend(sock_path=tmp_path / "nothing.sock", autostart=True)
    assert backend.available() is False
    assert calls == []


def test_available_is_true_when_status_answers(tmp_path, server_factory):
    server_factory({"server.status": lambda r: {"id": r["id"], "ok": True, "result": {}}})
    assert CoppiceBackend(sock_path=tmp_path / "server.sock").available() is True


def test_spawn_sends_pane_create_and_returns_the_ref(tmp_path, server_factory):
    srv = server_factory(
        {"pane.create": lambda r: {"id": r["id"], "ok": True, "result": {"pane": "w1:p1"}}}
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    ref = backend.spawn(cwd=tmp_path, cmd=["claude"], env={"K": "V"}, label="auth fix", kind="pty")
    assert ref == PaneRef("coppice", "w1:p1")
    req = next(r for r in srv.seen if r["cmd"] == "pane.create")
    assert req["cwd"] == str(tmp_path)
    assert req["cmd_argv"] == ["claude"]
    assert req["env"] == {"K": "V"}
    assert req["label"] == "auth fix" and req["kind"] == "pty"


def test_spawn_passes_the_bare_command_not_a_home_path(tmp_path, server_factory, monkeypatch):
    """The server resolves a bare name on PATH at spawn. The client never
    rewrites it to a path under the caller's home, so a harness installed
    anywhere on PATH starts, and one that is missing fails with the
    server's own message naming PATH."""
    monkeypatch.setenv("HOME", str(tmp_path))
    srv = server_factory(
        {"pane.create": lambda r: {"id": r["id"], "ok": True, "result": {"pane": "w1:p1"}}}
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.spawn(cwd=Path("/"), cmd=["claude"], env={}, label="x", kind="pty")
    req = next(r for r in srv.seen if r["cmd"] == "pane.create")
    assert req["cmd_argv"][0] == "claude"
    assert "/" not in req["cmd_argv"][0]


def test_a_server_error_raises_with_its_code_never_an_empty_success(tmp_path, server_factory):
    server_factory(
        {
            "pane.read": lambda r: {
                "id": r["id"],
                "ok": False,
                "error": {"code": "no_such_pane", "message": "gone"},
            }
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(CoppiceError) as e:
        backend.read(PaneRef("coppice", "w1:p9"))
    assert e.value.code == "no_such_pane" and "gone" in str(e.value)


def test_a_server_closed_reply_raises_with_that_code(tmp_path, server_factory):
    """server_closed joins the wire's error enum once Close has started; the
    Python side's own ERROR_CODES carries it too, never an internal fallback."""
    server_factory(
        {
            "pane.create": lambda r: {
                "id": r["id"],
                "ok": False,
                "error": {"code": "server_closed", "message": "this server has closed"},
            }
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(CoppiceError) as e:
        backend.spawn(cwd=tmp_path, cmd=["sh"], env={}, label="x", kind="pty")
    assert e.value.code == "server_closed"
    assert "server_closed" in ERROR_CODES


def test_a_dropped_connection_raises_a_coppiceerror(tmp_path):
    """Minor 3: the server closing the connection before answering is a
    wire failure too, the same CoppiceError family as an ok:false reply.

    Re-review 1's Informational 4: the earlier version of this test passed
    even with its own empty-line guard removed, because a dropped
    connection with no bytes at all also reaches json.loads(b"") and
    raises through the JSON-error wrap with the same code, "internal",
    and only a different message. Asserting on the message closes that:
    the empty-line guard's own words, not the JSON-parse wrap's, must be
    what a caller actually sees.
    """
    sock_path = tmp_path / "server.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)

    def accept_and_close():
        conn, _ = srv.accept()
        conn.recv(65536)  # read the request so closing is a clean EOF, not a reset
        conn.close()

    threading.Thread(target=accept_and_close, daemon=True).start()
    backend = CoppiceBackend(sock_path=sock_path)
    with pytest.raises(CoppiceError) as e:
        backend.read(PaneRef("coppice", "w1:p1"))
    assert e.value.code == "internal"
    assert "closed the connection" in str(e.value)
    srv.close()


def test_a_malformed_reply_line_raises_a_coppiceerror(tmp_path):
    """Minor 3: a reply this client cannot parse as JSON is a wire failure
    too, surfaced the same way, not a bare ValueError a caller has to know
    to catch separately."""
    sock_path = tmp_path / "server.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)

    def reply_garbage():
        conn, _ = srv.accept()
        with conn, conn.makefile("rwb") as stream:
            stream.readline()
            stream.write(b"not json at all\n")
            stream.flush()

    threading.Thread(target=reply_garbage, daemon=True).start()
    backend = CoppiceBackend(sock_path=sock_path)
    with pytest.raises(CoppiceError) as e:
        backend.read(PaneRef("coppice", "w1:p1"))
    assert e.value.code == "internal"
    srv.close()


def test_a_missing_pane_key_on_create_raises_keyerror(tmp_path, server_factory):
    """Minor 3: the server always sends pane on a successful pane.create;
    if it ever did not, that is a protocol bug worth a loud KeyError, not
    a silently empty PaneRef."""
    server_factory({"pane.create": lambda r: {"id": r["id"], "ok": True, "result": {}}})
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(KeyError):
        backend.spawn(cwd=tmp_path, cmd=["sh"], env={}, label="x", kind="pty")


def test_read_passes_the_source_through(tmp_path, server_factory):
    srv = server_factory(
        {"pane.read": lambda r: {"id": r["id"], "ok": True, "result": {"text": "READY\n"}}}
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    assert backend.read(PaneRef("coppice", "w1:p1"), source="detection") == "READY\n"
    assert next(r for r in srv.seen if r["cmd"] == "pane.read")["source"] == "detection"


def test_read_rejects_an_unknown_source_before_the_call(tmp_path, server_factory):
    srv = server_factory({})  # no handler; a call reaching the wire is itself a failure
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(ValueError):
        backend.read(PaneRef("coppice", "w1:p1"), source="bogus")
    assert srv.seen == []


def test_prompt_uses_agent_prompt_not_typed_text(tmp_path, server_factory):
    srv = server_factory({"agent.prompt": lambda r: {"id": r["id"], "ok": True, "result": {}}})
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    assert backend.prompt(PaneRef("coppice", "w1:p1"), "fix the test", wait=True) == "sent"
    req = next(r for r in srv.seen if r["cmd"] == "agent.prompt")
    assert req["text"] == "fix the test" and req["wait"] is True


def test_subscribe_yields_state_events_and_frames(tmp_path, server_factory):
    events = [
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "claude-code",
            "state": "blocked",
            "source": "gate",
            "harness_session_id": None,
            "ask": {"id": "a1", "tool": "Bash", "summary": "rm -rf", "deadline": 9999.0},
            "detail": "",
        },
        {
            "event": "frame",
            "pane": "w1:p1",
            "seq": 3,
            "cols": 80,
            "rows": 24,
            "cursor": [1, 2],
            "rows_changed": {"0": [["hi", "white", "black", 0]]},
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    got = []
    for item in backend.subscribe():
        got.append(item)
        if len(got) == 2:
            break
    assert isinstance(got[0], PaneStateEvent) and got[0].state == "blocked"
    assert isinstance(got[1], Frame) and got[1].seq == 3 and got[1].cursor == (1, 2)


def test_an_unparsable_event_line_is_skipped_not_fatal(tmp_path, server_factory):
    events = [
        {"event": "state", "pane": "w1:p1", "state": "not-a-state"},
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "process",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    first = next(iter(backend.subscribe()))
    assert isinstance(first, PaneStateEvent) and first.state == "idle"


def test_an_out_of_enum_state_never_becomes_a_painted_state_in_list(tmp_path, server_factory):
    """A server that says `ready` must not paint `ready` where `unknown`
    belongs. `PaneStateEvent.__post_init__` rejects it, `from_json`
    re-raises, and `_validated` turns that raise into None; this pins the
    outcome, not a second check `_validated` runs on top of it."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        _fake_pane_list_row(
                            label="x",
                            cwd="/",
                            cmd=[],
                            state="ready",
                            source="process",
                            ts=1.0,
                            session_id="s",
                        ),
                    ]
                },
            }
        }
    )
    infos = CoppiceBackend(sock_path=tmp_path / "server.sock").list()
    assert infos[0].state is None


def test_an_out_of_enum_source_never_becomes_a_painted_state_in_subscribe(tmp_path, server_factory):
    """Same outcome as the list case, on the subscribe path: an unknown
    source raises inside PaneStateEvent's own construction, and
    `_validated` drops the event rather than yielding a painted guess."""
    events = [
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "vibes",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "process",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    first = next(iter(CoppiceBackend(sock_path=tmp_path / "server.sock").subscribe()))
    assert first.source == "process"


def test_the_backend_declares_that_it_yields_frames():
    assert CoppiceBackend.yields_frames is True


def test_list_reads_id_and_cmd_off_the_flat_pane_list_row(tmp_path, server_factory):
    """pane.list keys the row id and carries the argv under cmd, not the
    shape agent.list uses."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        _fake_pane_list_row(
                            label="x", cwd="/home", cmd=["claude"], harness="claude-code"
                        ),
                    ]
                },
            }
        }
    )
    infos = CoppiceBackend(sock_path=tmp_path / "server.sock").list()
    assert infos[0].ref == PaneRef("coppice", "w1:p1")
    assert infos[0].cmd == ["claude"]


def test_list_reassembles_a_reported_state_from_the_flat_row(tmp_path, server_factory):
    """R4: state, source, detail, ask, ts and session_id sit beside the
    pane's own fields; list() rebuilds one PaneStateEvent from them."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        _fake_pane_list_row(
                            label="auth fix",
                            cwd="/home/x",
                            cmd=["claude"],
                            harness="claude-code",
                            state="blocked",
                            source="gate",
                            detail="needs an answer",
                            ts=1234.5,
                            session_id="s1",
                            ask={
                                "id": "a1",
                                "tool": "Bash",
                                "summary": "rm -rf",
                                "deadline": 9999.0,
                            },
                        ),
                    ]
                },
            }
        }
    )
    info = CoppiceBackend(sock_path=tmp_path / "server.sock").list()[0]
    assert info.state.state == "blocked" and info.state.source == "gate"
    assert info.state.session_id == "s1" and info.state.ts == 1234.5
    assert info.state.detail == "needs an answer"
    assert info.state.ask is not None and info.state.ask.tool == "Bash"


def test_list_reports_no_state_when_the_pane_has_never_reported_one(tmp_path, server_factory):
    """ts absent means no stored event: never a fabricated receive-time stamp."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        _fake_pane_list_row(label="x", cwd="/", cmd=[]),
                    ]
                },
            }
        }
    )
    infos = CoppiceBackend(sock_path=tmp_path / "server.sock").list()
    assert infos[0].state is None


def test_list_reports_no_state_when_ts_is_absent_even_with_an_otherwise_valid_row(
    tmp_path, server_factory
):
    """Minor 1: state and source alone are not what stops this row. Both
    are valid enum members here; only ts is missing, and that alone must
    still yield no state. The other no-state test uses source: None, which
    from_json would reject on its own regardless of ts."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        _fake_pane_list_row(
                            label="x", cwd="/", cmd=[], state="idle", source="process"
                        ),
                    ]
                },
            }
        }
    )
    infos = CoppiceBackend(sock_path=tmp_path / "server.sock").list()
    assert infos[0].state is None


def test_attach_sends_pane_attach_on_the_shared_connection(tmp_path, server_factory):
    srv = server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)
    _wait_until(lambda: any(r.get("cmd") == "pane.attach" for r in srv.seen))
    req = next(r for r in srv.seen if r["cmd"] == "pane.attach")
    assert req["pane"] == "w1:p1" and req["cols"] == 80 and req["rows"] == 24
    assert srv.connections == 1


def test_detach_sends_pane_detach_on_the_shared_connection(tmp_path, server_factory):
    srv = server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
            "pane.detach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": False},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)
    backend.detach(PaneRef("coppice", "w1:p1"))
    _wait_until(lambda: any(r.get("cmd") == "pane.detach" for r in srv.seen))
    req = next(r for r in srv.seen if r["cmd"] == "pane.detach")
    assert req["pane"] == "w1:p1"
    assert srv.connections == 1, "attach and detach share one connection, not one each"


def test_attach_and_subscribe_share_one_events_connection(tmp_path, server_factory):
    """attach() writes on the same connection subscribe() reads. Its own ok
    reply carries no "event" key, so subscribe()'s loop reads past it to
    the frame without ever yielding the reply itself."""
    events = [
        {
            "event": "frame",
            "pane": "w1:p1",
            "seq": 1,
            "cols": 80,
            "rows": 24,
            "cursor": [0, 0],
            "rows_changed": {},
        },
    ]
    srv = server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
        },
        events=events,
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)
    first = next(iter(backend.subscribe()))
    assert isinstance(first, Frame) and first.pane == "w1:p1"
    assert srv.connections == 1


def test_a_closed_events_connection_is_reopened_by_the_next_call(tmp_path, server_factory):
    """Minor 6: once subscribe()'s stream ends, whatever ended it, the
    connection state is cleared. The next attach() must open a fresh
    connection rather than write to the dead file object."""
    events = [
        {
            "event": "frame",
            "pane": "w1:p1",
            "seq": 1,
            "cols": 80,
            "rows": 24,
            "cursor": [0, 0],
            "rows_changed": {},
        },
    ]
    srv = server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
        },
        events=events,
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    list(backend.subscribe())  # the FakeServer streams its events then closes
    assert backend._events_stream is None, "subscribe() must clear the connection on its way out"
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)  # must not raise
    _wait_until(lambda: srv.connections == 2)


def test_close_connection_releases_the_shared_events_socket(tmp_path, server_factory):
    """close_connection() is the optional bare close the contract calls in
    teardown. It must actually close the socket, not just drop the
    reference: nothing else in this client ever calls socket.close() on
    the shared events connection."""
    server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)
    sock = backend._events_sock
    assert sock is not None
    backend.close_connection()
    assert backend._events_sock is None and backend._events_stream is None
    assert sock.fileno() == -1, "the fd must actually be closed, not just forgotten"


def test_close_connection_is_safe_with_nothing_open(tmp_path):
    CoppiceBackend(sock_path=tmp_path / "server.sock").close_connection()  # must not raise


def test_close_connection_is_safe_to_call_twice(tmp_path, server_factory):
    server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {"pane": r["pane"], "attached": True},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend.attach(PaneRef("coppice", "w1:p1"), cols=80, rows=24)
    backend.close_connection()
    backend.close_connection()  # must not raise


def test_close_connection_unblocks_a_thread_blocked_mid_read(tmp_path):
    """A pump thread abandoned mid-subscribe leaves the shared stream blocked
    on its next readline, exactly the shape a caller cleaning up from a
    different thread must be able to interrupt. Reproduced with a raw
    server that acks the subscribe and then sends nothing more, the same
    as the real server between events."""
    sock_path = tmp_path / "server.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)

    def accept_ack_and_hold():
        conn, _ = srv.accept()
        with conn, conn.makefile("rwb") as stream:
            req = json.loads(stream.readline())
            stream.write(json.dumps({"id": req["id"], "ok": True}).encode() + b"\n")
            stream.flush()
            time.sleep(10)  # sends nothing more; an open, idle subscription

    threading.Thread(target=accept_ack_and_hold, daemon=True).start()
    backend = CoppiceBackend(sock_path=sock_path)

    def pump():
        try:
            for _ in backend.subscribe():
                pass
        except Exception:
            pass

    threading.Thread(target=pump, daemon=True).start()
    time.sleep(0.3)  # let the generator reach its blocking readline

    done = threading.Event()

    def closer():
        backend.close_connection()
        done.set()

    closer_thread = threading.Thread(target=closer, daemon=True)
    closer_thread.start()
    closer_thread.join(timeout=5.0)
    assert done.is_set(), "close_connection() deadlocked against the reading thread"
    srv.close()


def test_request_ids_never_collide_across_two_threads(tmp_path):
    """_call, _send_event_cmd and subscribe() all bump the same counter,
    from three different threads in a running cockpit: the roster poll,
    the UI thread's attach or detach, and the pump. A race on the plain
    increment can hand two callers the same id, so a last_error(id)
    lookup then answers for the wrong request."""
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    ids_a: list[str] = []
    ids_b: list[str] = []
    errors: list[BaseException] = []

    def take(out: list[str]) -> None:
        try:
            for _ in range(1000):
                out.append(backend._next_id())
        except BaseException as exc:  # noqa: BLE001 - re-raised on the main thread below
            errors.append(exc)

    t1 = threading.Thread(target=take, args=(ids_a,))
    t2 = threading.Thread(target=take, args=(ids_b,))
    t1.start()
    t2.start()
    t1.join()
    t2.join()

    if errors:
        raise errors[0]
    combined = ids_a + ids_b
    assert len(combined) == 2000
    assert len(combined) == len(set(combined)), "two threads produced the same request id"


def test_a_subscribe_generators_finally_only_closes_its_own_connection_generation(
    tmp_path, server_factory
):
    """close_connection() versus a concurrent _events_conn(): each
    connection subscribe() opens is numbered, and a generator's own
    finally compares its own number against the current one before
    closing, so a generator whose connection has already been replaced
    never reaches into the new one.
    """
    events = [
        {
            "event": "state",
            "v": 1,
            "ts": time.time(),
            "session_id": "s",
            "harness": "shell",
            "pane": "p1",
            "state": "working",
            "source": "process",
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")

    gen = backend.subscribe()
    first = next(gen)
    assert isinstance(first, PaneStateEvent)
    assert backend._events_generation == 1

    # Simulate a second caller replacing the connection while this
    # generator is still alive: clear it, then open a fresh one directly.
    backend._events_sock = None
    backend._events_stream = None
    new_stream = backend._events_conn()
    assert backend._events_generation == 2

    # The fake server sent its one queued event and closed; this
    # generator's own read now sees EOF and runs its finally.
    with pytest.raises(StopIteration):
        next(gen)

    assert backend._events_stream is new_stream
    assert backend._events_sock is not None
    assert backend._events_sock.fileno() != -1
    backend.close_connection()


def test_close_connection_blocks_a_concurrent_open_until_it_finishes(
    tmp_path, server_factory, monkeypatch
):
    """close_connection() took no lock of its own: every line inside it
    re-reads self._events_sock and self._events_stream fresh, so a second
    caller could replace either one between this method's own shutdown
    call and its close call, and this method would then close the
    connection that caller just opened instead of the one it meant to. A
    delay inside the real socket shutdown call stands in for a scheduler
    preemption there; a concurrent open must not proceed until the close
    holding the lock has finished.
    """
    server_factory({})
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    backend._events_conn_with_generation()
    assert backend._events_generation == 1

    inside = threading.Event()
    release = threading.Event()
    real_shutdown = socket.socket.shutdown

    def slow_shutdown(self, how):
        inside.set()
        release.wait(timeout=2.0)
        return real_shutdown(self, how)

    monkeypatch.setattr(socket.socket, "shutdown", slow_shutdown)

    closer = threading.Thread(target=backend.close_connection)
    closer.start()
    assert inside.wait(timeout=2.0), "close_connection never reached the socket shutdown"

    opened = threading.Event()

    def open_fresh():
        backend._events_conn_with_generation()
        opened.set()

    opener = threading.Thread(target=open_fresh)
    opener.start()
    assert not opened.wait(timeout=0.3), (
        "a concurrent open was not blocked by the in-progress close"
    )

    release.set()
    closer.join(timeout=2.0)
    opener.join(timeout=2.0)
    assert opened.is_set()
    assert backend._events_generation == 3, "close_connection's own bump should count too"


def test_subscribe_finally_blocks_a_concurrent_open_until_its_close_finishes(
    tmp_path, server_factory
):
    """The pump's own finally held the lock only long enough to compare
    generations, then called close_connection() after letting go: a
    concurrent open could land in that gap and be torn down by a close
    that was never meant for it. The compare and the close now share one
    critical section, so an open blocks until the pump's own close, even
    a slow one, has actually finished.
    """
    events = [
        {
            "event": "state",
            "v": 1,
            "ts": time.time(),
            "session_id": "s",
            "harness": "shell",
            "pane": "p1",
            "state": "working",
            "source": "process",
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")

    gen = backend.subscribe()
    next(gen)  # suspended inside the try, holding generation 1
    assert backend._events_generation == 1

    inside = threading.Event()
    release = threading.Event()
    real_close_locked = backend._close_locked

    def slow_close_locked():
        inside.set()
        release.wait(timeout=2.0)
        real_close_locked()

    backend._close_locked = slow_close_locked

    closer = threading.Thread(target=gen.close)
    closer.start()
    assert inside.wait(timeout=2.0), "the generator's own finally never reached the close"

    opened = threading.Event()

    def open_fresh():
        backend._events_conn_with_generation()
        opened.set()

    opener = threading.Thread(target=open_fresh)
    opener.start()
    assert not opened.wait(timeout=0.3), "a concurrent open was not blocked by the pump's own close"

    release.set()
    closer.join(timeout=2.0)
    opener.join(timeout=2.0)
    assert opened.is_set()
    assert backend._events_generation == 3, "the pump's own close should bump the generation too"


def test_subscribe_drops_a_malformed_frame_instead_of_raising(tmp_path, server_factory):
    """The frame branch built a Frame from bare lookups: a payload with no
    rows_changed raised KeyError, a non-dict rows_changed raised
    AttributeError, and a wrong-typed cols raised ValueError, ending the
    whole subscription on one bad frame. The state branch already guards
    itself through _validated(); the frame branch must degrade the same
    way, and record the drop the same way an ok:false reply already does,
    so a caller can still ask what went wrong."""

    def good_frame(seq: int) -> dict:
        return {
            "event": "frame",
            "pane": "p1",
            "seq": seq,
            "cols": 80,
            "rows": 24,
            "cursor": [0, 0],
            "rows_changed": {},
        }

    events = [
        good_frame(1),
        {"event": "frame", "pane": "p1", "seq": 2, "cols": 80, "rows": 24, "cursor": [0, 0]},
        good_frame(3),
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")

    gen = backend.subscribe()
    frames = list(gen)

    assert [f.seq for f in frames] == [1, 3]
    errors = list(backend._event_errors.values())
    assert len(errors) == 1
    assert isinstance(errors[0], CoppiceError)
    backend.close_connection()


def test_event_errors_is_capped_and_drops_the_oldest(tmp_path, server_factory):
    """A backend that runs for a long time and keeps failing attaches must
    not grow this map without bound."""
    from opendaisugi.floor.coppice_backend import _EVENT_ERRORS_MAX

    server_factory(
        {
            "pane.attach": lambda r: {
                "id": r["id"],
                "ok": False,
                "error": {"code": "bad_request", "message": "nope"},
            },
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    total = _EVENT_ERRORS_MAX + 10
    ids = [backend.attach(PaneRef("coppice", f"w1:p{i}"), cols=80, rows=24) for i in range(total)]
    list(backend.subscribe())  # drains every reply; the fake server then closes

    assert len(backend._event_errors) == _EVENT_ERRORS_MAX
    assert ids[0] not in backend._event_errors, "the oldest entry must be dropped"
    assert ids[-1] in backend._event_errors, "the newest entry must survive"


def test_subscribe_raises_when_the_server_refuses_it(tmp_path, server_factory):
    """New Issue 2: a refused events.subscribe must not be a silent, empty
    generator forever. subscribe() keeps its own request id and raises
    CoppiceError the moment the reply to THAT id comes back ok:false,
    instead of filing it under an id no caller could ever look up."""
    server_factory(
        {},
        subscribe_error={
            "code": "bad_request",
            "message": 'kind "bogus" is not state, layout or frame',
        },
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(CoppiceError) as e:
        next(iter(backend.subscribe()))
    assert e.value.code == "bad_request"


def test_known_keys_are_loaded_from_the_go_source_of_truth():
    assert {"enter", "esc", "ctrl+a", "f1", "f12"} <= KNOWN_KEYS


def test_tmux_key_map_has_no_gap_against_coppices_named_keys():
    """FW2's whole point: one key vocabulary, so the floor cannot bind a key
    coppice lacks. tmux's KEY_MAP used to carry ctrl+z, which coppice's own
    namedKeys in testdata/keys.json does not accept; dropped so the two
    vocabularies agree. coppice separately accepts f5 through f12, which
    tmux's KEY_MAP does not carry; nothing outside tmux_backend.py ever asks
    it for those, so that direction is not a floor-vocabulary gap.
    """
    from opendaisugi.floor import tmux_backend

    assert set(tmux_backend.KEY_MAP) <= KNOWN_KEYS


def test_registry_fix_text_names_the_same_build_command_as_hostfacts():
    """Task 3's report: _FIX['coppice']'s build text drifted from hostfacts'.
    Bound here to the one substring that must actually match, not the whole
    message - _FIX also names the fix for a backend that IS built but not
    running, which hostfacts does not speak to at all."""
    from opendaisugi.floor.registry import _FIX

    build_cmd = "go build -o build/coppice ./cmd/coppice"
    assert build_cmd in hostfacts.host_facts()["coppice"].fix
    assert build_cmd in _FIX["coppice"]


def test_against_the_real_binary_when_it_is_built(tmp_path, coppice_server):
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    assert backend.available() is True
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="probe", kind="pty"
    )
    try:
        assert ref.id
    finally:
        backend.close(ref)


def test_frames_only_arrive_after_attach_on_the_real_binary(tmp_path, coppice_server):
    """F10/F11: events.subscribe alone never delivers a frame. Proven end to
    end against the real server, not just the fake's own recorded shape."""
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="frame probe", kind="pty"
    )
    seen: list = []
    stop = threading.Event()

    def pump():
        for item in backend.subscribe():
            seen.append(item)
            if stop.is_set():
                return

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    try:
        time.sleep(0.3)
        assert not any(isinstance(item, Frame) for item in seen), (
            "a frame arrived before attach() was ever called"
        )
        backend.attach(ref, cols=80, rows=24)
        _wait_until(lambda: any(isinstance(item, Frame) for item in seen), timeout=3.0)
        frame = next(item for item in seen if isinstance(item, Frame))
        assert frame.pane == ref.id
    finally:
        stop.set()
        backend.detach(ref)
        backend.close(ref)


def test_attach_to_a_closed_pane_records_the_error(tmp_path, coppice_server):
    """I3: a failed attach must not be silent. subscribe()'s loop records
    the ok:false reply keyed by attach()'s own request id, and last_error
    answers it once that reply has actually been read."""
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="closed probe", kind="pty"
    )
    backend.close(ref)
    request_id = backend.attach(ref, cols=80, rows=24)

    def pump():
        for _ in backend.subscribe():
            pass

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    _wait_until(lambda: backend.last_error(request_id) is not None, timeout=3.0)
    error = backend.last_error(request_id)
    assert error.code in ("no_such_pane", "pane_closed")


def _fake_pane_list_row(
    *,
    id="w1:p1",
    label="probe",
    cwd="/tmp",
    cmd=None,
    kind="pty",
    harness="",
    closed=False,
    cols=120,
    rows=40,
    workspace="w1",
    tab="w1:t1",
    state="unknown",
    source=None,
    detail="",
    ts=None,
    session_id=None,
    ask=None,
    quiet_for=None,
) -> dict:
    """The row shape a FakeServer pane.list handler hands back. The drift
    check below compares this same function's output against a recorded
    real reply, not a separate hand-typed copy of it.

    Named keyword-only parameters, not **overrides: a caller who typos or
    renames a field, cwd to workdir say, gets a TypeError at the call site
    instead of a row that silently carries a stray key nothing checks.

    ts, session_id, ask and quiet_for default to None and are left out of
    the row entirely, not set to null: pane.list itself omits those keys,
    rather than nulling them, for a record with no stored event and no live
    grid. A live pane always carries all but ask, because its process
    source reports from the moment it starts.
    """
    if cmd is None:
        cmd = ["sh", "-c", "sleep 300"]
    row = {
        "id": id,
        "label": label,
        "cwd": cwd,
        "cmd": cmd,
        "kind": kind,
        "harness": harness,
        "closed": closed,
        "cols": cols,
        "rows": rows,
        "workspace": workspace,
        "tab": tab,
        "state": state,
        "source": source,
        "detail": detail,
    }
    if ts is not None:
        row["ts"] = ts
    if session_id is not None:
        row["session_id"] = session_id
    if ask is not None:
        row["ask"] = ask
    if quiet_for is not None:
        row["quiet_for"] = quiet_for
    return row


def _fake_live_pane_list_row(**overrides) -> dict:
    """The row shape for a live pty pane that nothing but the server's own
    process source has reported on: idle from process, with the event's
    ts and session_id flat on the row, and quiet_for beside them."""
    defaults = {
        "state": "idle",
        "source": "process",
        "ts": 1234.5,
        "session_id": "w1:p1",
        "quiet_for": 0.25,
    }
    defaults.update(overrides)
    return _fake_pane_list_row(**defaults)


def _fake_pane_list_row_with_state(**overrides) -> dict:
    """The row shape a FakeServer pane.list handler hands back for a pane
    that HAS reported a state: ts, session_id and ask sit flat on the row
    too, the shape _fake_pane_list_row alone does not cover."""
    defaults = {
        "state": "blocked",
        "source": "gate",
        "detail": "needs an answer",
        "ts": 1234.5,
        "session_id": "s1",
        "ask": {"id": "a1", "tool": "Bash", "summary": "rm -rf", "deadline": 9999999999.5},
        "quiet_for": 0.25,
    }
    defaults.update(overrides)
    return _fake_pane_list_row(**defaults)


def test_pane_list_row_diff_is_none_when_shapes_agree():
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p2", "cmd": ["sh"], "cols": 80, "source": None}
    recorded = {"id": "w1:p1", "cmd": ["sh", "-c", "x"], "cols": 120, "source": None}
    assert pane_list_row_diff(live, recorded) is None


def test_pane_list_row_diff_catches_an_added_key():
    """I2's own simulation: a Go change adds a key to the live row."""
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p1", "cmd": []}
    recorded = {"id": "w1:p1", "cmd": [], "new_field": "x"}
    assert pane_list_row_diff(recorded, live) is not None  # live adds new_field
    assert pane_list_row_diff(live, recorded) is not None  # either direction is drift


def test_pane_list_row_diff_catches_a_dropped_key():
    """I2's own simulation: a Go change drops harness from the live row."""
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p1", "cmd": []}
    recorded = {"id": "w1:p1", "cmd": [], "harness": ""}
    assert pane_list_row_diff(live, recorded) is not None


def test_pane_list_row_diff_catches_a_changed_value_type():
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p1", "cols": "120"}  # a string where an int belongs
    recorded = {"id": "w1:p1", "cols": 120}
    assert pane_list_row_diff(live, recorded) is not None


def test_pane_list_row_diff_recurses_into_a_nested_dict():
    """New Issue 3: `ask` matching by its own outer type, a dict, was
    never the bar the rest of this function holds a plain field to."""
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p1", "ask": {"id": "a1", "tool": "Bash", "deadline": 9.0}}
    recorded = {"id": "w1:p1", "ask": {"id": 17, "gone": "wrong shape entirely"}}
    diff = pane_list_row_diff(live, recorded)
    assert diff is not None and "ask" in diff


def test_pane_list_row_diff_finds_a_type_mismatch_nested_inside_ask():
    from tests.floor.coppice_sandbox import pane_list_row_diff

    live = {"id": "w1:p1", "ask": {"deadline": 9.0}}
    recorded = {"id": "w1:p1", "ask": {"deadline": 9}}
    diff = pane_list_row_diff(live, recorded)
    assert diff is not None and "ask.deadline" in diff


def test_fake_pane_list_row_matches_a_recorded_real_reply(coppice_server):
    """R4: the fake cannot drift from the server unnoticed. Records a real
    pane.list reply once, into tests/floor/testdata/coppice/pane_list.json,
    and checks every run since against the live server too, per Important
    2. Also checks the FakeServer's own row-building function against the
    same recorded shape, per Minor 2."""
    from tests.floor import coppice_sandbox

    recorded = coppice_sandbox.record_pane_list_fixture(coppice_server)
    real_row = recorded["panes"][0]
    fake_row = _fake_live_pane_list_row()
    diff = coppice_sandbox.pane_list_row_diff(fake_row, real_row)
    assert diff is None, f"the fake's pane.list row has drifted from the wire: {diff}"


def test_fake_reported_pane_list_row_matches_a_recorded_real_reply(coppice_server):
    """Minor 2: the earlier drift check only ever recorded a pane that
    never reported a state, so the ts/session_id/ask-bearing shape was
    never checked at all. This records a second fixture from a pane that
    HAS reported one."""
    from tests.floor import coppice_sandbox

    recorded = coppice_sandbox.record_reported_pane_list_fixture(coppice_server)
    real_row = recorded["panes"][0]
    fake_row = _fake_pane_list_row_with_state()
    diff = coppice_sandbox.pane_list_row_diff(fake_row, real_row)
    assert diff is None, f"the reported fake's pane.list row has drifted from the wire: {diff}"


def test_recorded_fixture_carries_no_real_path_from_this_box():
    """Minor 5: the committed fixtures must be the same on every machine
    and name nothing about the box that recorded them."""
    from tests.floor import coppice_sandbox

    for path in (coppice_sandbox.PANE_LIST_FIXTURE, coppice_sandbox.PANE_LIST_REPORTED_FIXTURE):
        text = path.read_text()
        assert str(Path.home()) not in text
        assert "pytest-of-" not in text
