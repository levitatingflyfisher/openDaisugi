"""FN-R-12: inside daisugi fence the hook only asks the resident gate.

The fence keeps the gate's state read only, so an in-process fallback
could not write asks, audit or sessions. A fenced hook sends every call to
gate.sock, an ask too, and denies when nothing answers.
"""

from __future__ import annotations

import io
import json
import os
import socket
import threading

from opendaisugi import gate_client


def _serve_once(path, reply, seen):
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(path))
    os.chmod(path, 0o600)
    srv.listen(1)

    def run():
        c, _ = srv.accept()
        buf = b""
        while not buf.endswith(b"\n"):
            buf += c.recv(65536)
        seen.append(json.loads(buf))
        c.sendall(json.dumps(reply).encode() + b"\n")
        c.close()
        srv.close()

    t = threading.Thread(target=run, daemon=True)
    t.start()
    return t


def _main(monkeypatch, argv, capsys):
    monkeypatch.setattr("sys.stdin", io.TextIOWrapper(io.BytesIO(b'{"tool_name":"Read"}')))
    code = gate_client.main(argv)
    return code, capsys.readouterr()


def test_a_fenced_hook_with_no_server_denies(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("DAISUGI_FENCED", "landlock")
    code, out = _main(monkeypatch, ["--mode", "enforce", "--root", str(tmp_path)], capsys)
    assert code == 2
    assert "cannot reach the resident gate" in out.err


def test_a_fenced_ask_goes_to_the_server(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("DAISUGI_FENCED", "landlock")
    monkeypatch.setenv("COPPICE_SOCK", "/run/c.sock")
    monkeypatch.setenv("COPPICE_PANE", "w1:p1")
    seen: list[dict] = []
    t = _serve_once(
        tmp_path / "gate.sock", {"v": 1, "stdout": "ok", "stderr": "", "exit_code": 0}, seen
    )
    argv = ["--mode", "enforce", "--root", str(tmp_path), "--ask", "--ask-timeout", "5"]
    code, out = _main(monkeypatch, argv, capsys)
    t.join(5)
    assert code == 0 and out.out == "ok\n"
    # --root only found the socket: the server has its own (FN-R-21).
    assert seen and seen[0]["argv"] == ["--mode", "enforce", "--ask", "--ask-timeout", "5"]
    assert seen[0]["coppice_pane"] == "w1:p1"


def test_the_fenced_deadline_covers_the_ask():
    assert gate_client.fenced_deadline_s([]) == 20.0
    assert (
        gate_client.fenced_deadline_s(["--ask", "--ask-timeout", "60", "--verify-timeout=3"])
        == 73.0
    )
