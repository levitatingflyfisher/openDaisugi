"""The resident gate takes no path from a request (FN-R-21, review N-1).

It runs outside the fence and makes every write, so a fenced process that
reaches gate.sock must not steer where it writes: no --root or
--captures-root from the request, a session id that names files only when
it is plain, and a checkpoint only inside the workspace a fence run bound.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import threading
from pathlib import Path

import pytest

from opendaisugi import fence_runs
from opendaisugi.gate import register_envelope, starter_envelope
from opendaisugi.gate_client import ask_server
from opendaisugi.gate_server import SOCK_NAME, serve

_READ = {"file_path": "/nowhere/README.md"}


def _payload(**kw) -> bytes:
    body = {"session_id": "s1", "tool_name": "Read", "tool_input": _READ}
    body.update(kw)
    return json.dumps(body).encode()


@pytest.fixture
def root(tmp_path: Path, monkeypatch) -> Path:
    r = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), root=r)
    monkeypatch.setitem(_READ, "file_path", str(tmp_path / "README.md"))
    return r


def _start(root: Path, **kw):
    ready, stop = threading.Event(), threading.Event()
    t = threading.Thread(
        target=serve, args=(root,), kwargs={"ready": ready, "stop": stop, **kw}, daemon=True
    )
    t.start()
    assert ready.wait(5), "server did not start"
    return stop, t


@pytest.fixture
def server(root: Path):
    stop, t = _start(root)
    yield root / SOCK_NAME
    stop.set()
    t.join(5)


def _raw(sock: Path, argv: list[str], payload: bytes) -> dict:
    """One request as a fenced process could send it: argv untouched."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(30)
        s.connect(str(sock))
        req = {"v": 1, "argv": argv, "stdin_b64": base64.b64encode(payload).decode()}
        s.sendall(json.dumps(req).encode() + b"\n")
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = s.recv(65536)
            assert chunk, "no reply"
            buf += chunk
    return json.loads(buf)


@pytest.mark.parametrize(
    "words",
    [
        ["--root", "{X}"],
        ["--root={X}"],
        ["--ro", "{X}"],
        ["--r={X}"],
        ["--captures-root", "{X}"],
        ["--captures-root={X}"],
        ["--cap", "{X}"],
    ],
)
def test_a_request_that_names_a_root_is_denied(server: Path, tmp_path: Path, words):
    x = tmp_path / "elsewhere"
    argv = ["--mode", "enforce", "--format", "claude"] + [w.replace("{X}", str(x)) for w in words]
    reply = _raw(server, argv, _payload())
    assert reply["exit_code"] == 2
    assert "takes no path from a request" in reply["stderr"]
    assert not x.exists(), "the server made the directory a request named"


def test_a_hook_report_that_names_a_root_is_denied(server: Path, tmp_path: Path):
    x = tmp_path / "elsewhere"
    row = {"v": 1, "ts": 1.0, "session_id": "s9", "harness": "pi", "state": "idle"}
    reply = _raw(server, ["hook", "report", "--root", str(x)], json.dumps(row).encode())
    assert reply["exit_code"] == 1
    assert "takes no path from a request" in reply["stderr"]
    assert not x.exists()


def test_the_server_writes_under_its_own_root(root: Path, server: Path):
    reply = _raw(server, ["--mode", "enforce", "--format", "claude"], _payload())
    assert reply["exit_code"] == 0, reply
    assert (root / "audit").exists() or any(root.rglob("*.jsonl"))


def test_the_client_strips_the_root_it_uses_to_find_the_socket(root: Path, server: Path):
    reply = ask_server(server, ["--mode", "enforce", "--root", str(root)], _payload())
    assert reply is not None and reply["exit_code"] == 0, reply
    reply = ask_server(server, ["hook", "report", f"--root={root}"], b"not json")
    assert reply is not None and reply["exit_code"] == 1
    assert "no path" not in reply["stderr"]


def test_strip_path_flags_keeps_every_other_word():
    argv = ["--mode", "enforce", "--root", "/r", "--captures-root=/c", "--format", "pi"]
    assert fence_runs.strip_path_flags(argv) == ["--mode", "enforce", "--format", "pi"]


@pytest.mark.parametrize("sid", ["../x", ".", "..", "a/b", "", "a" * 129, "é", "a b", 7, ["s"]])
def test_a_bad_session_id_is_denied(server: Path, sid):
    reply = _raw(server, ["--mode", "enforce"], _payload(session_id=sid))
    assert reply["exit_code"] == 2
    assert "session id" in reply["stderr"]


def test_a_bad_session_pin_is_denied(server: Path):
    reply = _raw(server, ["--mode", "enforce", "--session", "../up"], _payload())
    assert reply["exit_code"] == 2
    assert "session id" in reply["stderr"]


@pytest.mark.parametrize("sid", ["a" * 128, "A.b_c-9", None])
def test_a_plain_or_absent_session_id_passes_the_rule(server: Path, sid):
    reply = _raw(server, ["--mode", "audit"], _payload(session_id=sid))
    assert "session id" not in reply["stderr"]


def test_a_checkpoint_with_no_fence_run_is_denied(server: Path, tmp_path: Path):
    reply = _raw(server, ["--mode", "enforce", "--checkpoints"], _payload(cwd=str(tmp_path)))
    assert reply["exit_code"] == 2
    assert "fence run" in reply["stderr"]


def test_a_checkpoint_inside_the_bound_workspace_passes(root: Path, server: Path, tmp_path: Path):
    ws = tmp_path / "ws"
    (ws / "sub").mkdir(parents=True)
    # This test process is the caller the server sees; a fence run above it
    # (here: itself) binds the workspace.
    fence_runs.register(root, os.getpid(), str(ws))
    reply = _raw(server, ["--mode", "enforce", "--checkpoints"], _payload(cwd=str(ws / "sub")))
    assert "fence run" not in reply["stderr"], reply


def test_a_checkpoint_outside_the_bound_workspace_is_denied(
    root: Path, server: Path, tmp_path: Path
):
    ws = tmp_path / "ws"
    ws.mkdir()
    out = tmp_path / "out"
    out.mkdir()
    (ws / "link").symlink_to(out)
    fence_runs.register(root, os.getpid(), str(ws))
    for cwd in (str(out), str(ws / "link"), str(tmp_path / "wsx"), "ws"):
        reply = _raw(server, ["--mode", "enforce", "--checkpoints"], _payload(cwd=cwd))
        assert reply["exit_code"] == 2, cwd
        # Any fenced checkpoint call is denied first (FN-R-36).
        assert "checkpoints are off inside the fence" in reply["stderr"], cwd


def test_a_stale_fence_run_binds_nothing(root: Path, server: Path, tmp_path: Path):
    fence_runs.register(root, os.getpid(), str(tmp_path))
    f = root / "fence-runs" / f"{os.getpid()}.json"
    rec = json.loads(f.read_text())
    rec["start"] = "1"
    f.write_text(json.dumps(rec))
    reply = _raw(server, ["--mode", "enforce", "--checkpoints"], _payload(cwd=str(tmp_path)))
    assert reply["exit_code"] == 2
    assert "no fence run" in reply["stderr"]


def test_a_captures_root_is_fixed_when_the_server_starts(root: Path, tmp_path: Path):
    caps = tmp_path / "caps"
    stop, t = _start(root, captures_root=caps)
    try:
        reply = _raw(root / SOCK_NAME, ["--mode", "enforce"], _payload())
        assert reply["exit_code"] == 0, reply
        assert caps.exists() and any(caps.rglob("*"))
    finally:
        stop.set()
        t.join(5)
