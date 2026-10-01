"""The pi extension and the OpenCode plugin apply the data home rule's
value check: a leading ~ is expanded, and a value still not absolute is
ignored (the XDG spec), so the socket is never looked for relative to the
agent's cwd."""

from __future__ import annotations

import json
import os
import socketserver
import subprocess
import threading
from pathlib import Path

import pytest

from tests.harness_pi._node_check import node_status

_ok, _reason = node_status()
pytestmark = pytest.mark.skipif(not _ok, reason=_reason)
REPO = Path(__file__).resolve().parents[2]
PI = REPO / "tests" / "harness_pi"
OC = REPO / "tests" / "harness_opencode"
_PATH = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}


class _Allow(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        self.rfile.readline()
        self.wfile.write(b'{"v": 1, "stdout": "", "stderr": "", "exit_code": 0}\n')


def _serve(sock: Path) -> None:
    sock.parent.mkdir(parents=True, exist_ok=True)
    srv = socketserver.UnixStreamServer(str(sock), _Allow)
    os.chmod(sock, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()


def _pi(env: dict, cwd: Path) -> dict:
    r = subprocess.run(
        [
            "node",
            "--experimental-strip-types",
            str(PI / "run_tool_call.mjs"),
            "read",
            '{"path": "/x"}',
            "/repo",
        ],
        cwd=cwd,
        env={**env, **_PATH},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert r.returncode == 0, r.stderr
    return json.loads(r.stdout.strip().splitlines()[-1])


def _oc(env: dict, cwd: Path) -> dict:
    step = {"do": "before", "tool": "read", "args": {"filePath": "/proj/a.txt"}}
    r = subprocess.run(
        ["node", "--experimental-strip-types", str(OC / "drive.mjs"), json.dumps(step)],
        cwd=cwd,
        env={**env, **_PATH},
        capture_output=True,
        text=True,
        timeout=15,
    )
    assert r.returncode == 0, r.stderr
    return json.loads(r.stdout.strip().splitlines()[-1])


@pytest.mark.parametrize("var", ["OPENDAISUGI_HOME", "XDG_DATA_HOME"])
def test_a_leading_tilde_is_expanded(tmp_path, var):
    home = tmp_path / "home"
    data = home / "moved" if var == "OPENDAISUGI_HOME" else home / "moved" / "opendaisugi"
    _serve(data / "gate" / "gate.sock")
    env = {"HOME": str(home), var: "~/moved"}
    assert _pi(env, PI) == {"allow": True, "reason": ""}
    assert _oc(env, OC) == {"threw": False}


@pytest.mark.parametrize("var", ["OPENDAISUGI_HOME", "XDG_DATA_HOME"])
def test_a_relative_value_is_ignored(tmp_path, var):
    """A socket planted under the cwd at the relative value is not used:
    the extension looks under ~/.opendaisugi, finds nothing, and refuses."""
    home = tmp_path / "home"
    home.mkdir()
    work = tmp_path / "work"
    planted = work / "rel" if var == "OPENDAISUGI_HOME" else work / "rel" / "opendaisugi"
    _serve(planted / "gate" / "gate.sock")
    env = {"HOME": str(home), var: "rel"}
    assert _pi(env, work)["allow"] is False
    assert _oc(env, work)["threw"] is True


def test_pi_with_no_home_looks_nowhere(tmp_path):
    """With no HOME and no data home variable, the pi extension does not look
    for .opendaisugi/gate/gate.sock under the cwd."""
    work = tmp_path / "work"
    _serve(work / ".opendaisugi" / "gate" / "gate.sock")
    assert _pi({}, work)["allow"] is False
