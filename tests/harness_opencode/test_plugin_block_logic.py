"""The OpenCode gate plugin, run under Node against fake gate sockets.

drive.mjs loads the plugin the way OpenCode's loader does: it calls every
distinct exported value as a plugin function. Each test names the failure
it guards against.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import socketserver
import subprocess
import threading
import time
from pathlib import Path

import pytest

from opendaisugi._state_report import _validate_hook_report_row
from tests.harness_pi._node_check import node_status

HERE = Path(__file__).parent
_ok, _reason = node_status()
pytestmark = pytest.mark.skipif(not _ok, reason=_reason)
_ENV_BASE = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}
UNREACHABLE = "openDaisugi gate unreachable: run daisugi start"


def _reply(exit_code: int, stderr: str = "") -> dict:
    return {"v": 1, "stdout": "", "stderr": stderr, "exit_code": exit_code}


class _Gate:
    """A fake gate socket that records each request and its decoded payload."""

    def __init__(self, sock_path: Path, reply: dict | bytes, mode: int = 0o600, delay: float = 0):
        self.path = sock_path
        self.requests: list[tuple[dict, dict]] = []
        gate = self

        class _H(socketserver.StreamRequestHandler):
            def handle(self) -> None:
                req = json.loads(self.rfile.readline())
                gate.requests.append((req, json.loads(base64.b64decode(req["stdin_b64"]))))
                if delay:
                    time.sleep(delay)
                body = reply if isinstance(reply, bytes) else json.dumps(reply).encode() + b"\n"
                try:
                    self.wfile.write(body)
                except OSError:
                    pass

        self.server = socketserver.UnixStreamServer(str(sock_path), _H)
        os.chmod(sock_path, mode)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def gate_requests(self) -> list[tuple[dict, dict]]:
        return [r for r in self.requests if r[0]["argv"][:1] != ["hook"]]

    def reports(self) -> list[tuple[dict, dict]]:
        return [r for r in self.requests if r[0]["argv"][:2] == ["hook", "report"]]


def _drive(step: dict, sock: Path | str, env: dict | None = None, timeout: float = 15) -> dict:
    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "drive.mjs"), json.dumps(step)],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock), **(env or {}), **_ENV_BASE},
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout.strip().splitlines()[-1])


def _before(sock, tool: str, args, env: dict | None = None) -> dict:
    return _drive({"do": "before", "tool": tool, "args": args}, sock, env)


# --- the loader ---------------------------------------------------------------------------


def test_the_plugin_file_exports_exactly_one_function(tmp_path):
    """OpenCode calls every exported function as a plugin, and a file with a
    non-function export fails to load, which leaves OpenCode ungated."""
    out = _drive({"do": "exports"}, tmp_path / "unused.sock")
    assert out == {"count": 1, "types": ["function"]}


# --- tool.execute.before: allow only on exit 0 ---------------------------------------------


def test_an_allow_sends_the_bare_gate_argv_and_the_mapped_payload(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    out = _before(gate.path, "read", {"filePath": "/proj/a.txt"})
    assert out == {"threw": False}
    ((req, payload),) = gate.gate_requests()
    assert req["v"] == 1
    assert req["argv"] == ["--format", "opencode", "--root", str(tmp_path), "--verify-timeout", "4"]
    assert "--mode" not in req["argv"]
    assert payload == {
        "tool_name": "Read",
        "tool_input": {"filePath": "/proj/a.txt"},
        "session_id": "ses_1",
        "cwd": "/proj",
        "tool_use_id": "call_1",
    }


def test_every_built_in_tool_maps_to_the_gates_own_name(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    ids = ["bash", "read", "write", "edit", "glob", "grep", "webfetch", "websearch"]
    for tool in ids:
        _before(gate.path, tool, {})
    names = [p["tool_name"] for _, p in gate.gate_requests()]
    assert names == ["Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch"]


def test_any_other_tool_id_goes_to_the_gate_as_an_opencode_mcp_call(tmp_path):
    """An unmapped id must not pass through as itself: a plugin tool named
    Bash or mcp__x__y would then borrow another tool's rules. Object keys
    such as constructor must not resolve through the prototype."""
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    for tool in ("apply_patch", "Bash", "mcp__x__y", "constructor", "__proto__"):
        _before(gate.path, tool, {})
    names = [p["tool_name"] for _, p in gate.gate_requests()]
    assert names == [
        "mcp__opencode__apply_patch",
        "mcp__opencode__Bash",
        "mcp__opencode__mcp__x__y",
        "mcp__opencode__constructor",
        "mcp__opencode____proto__",
    ]


def test_a_bash_workdir_is_the_cwd_the_gate_checks(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    _before(gate.path, "bash", {"command": "rm x", "workdir": "sub"})
    _before(gate.path, "bash", {"command": "rm x", "workdir": "/elsewhere"})
    _before(gate.path, "bash", {"command": "ls"})
    cwds = [p["cwd"] for _, p in gate.gate_requests()]
    assert cwds == ["/proj/sub", "/elsewhere", "/proj"]


def test_a_workdir_that_is_not_a_string_is_a_deny_that_never_asks(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    out = _before(gate.path, "bash", {"command": "ls", "workdir": ["x"]})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert gate.gate_requests() == []


def test_the_daisugi_session_id_names_the_envelope_when_set(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    _before(gate.path, "read", {}, {"OPENDAISUGI_SESSION_ID": "sess-9"})
    assert gate.gate_requests()[0][1]["session_id"] == "sess-9"


def test_a_deny_throws_the_gates_own_reason(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(2, "openDaisugi gate: DENIED - shell head 'rm'"))
    out = _before(gate.path, "bash", {"command": "rm -rf /"})
    assert out == {"threw": True, "message": "openDaisugi gate: DENIED - shell head 'rm'"}


def test_an_exit_code_that_is_not_a_verdict_throws(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(1, "crashed"))
    assert _before(gate.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_a_malformed_reply_throws(tmp_path):
    gate = _Gate(tmp_path / "g.sock", b"not json at all\n")
    assert _before(gate.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_a_reply_without_the_schema_version_throws(tmp_path):
    gate = _Gate(tmp_path / "g.sock", {"stdout": "", "stderr": "", "exit_code": 0})
    assert _before(gate.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_no_socket_throws_with_the_start_hint(tmp_path):
    assert _before(tmp_path / "nope.sock", "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_no_home_and_no_socket_env_throws(tmp_path):
    out = _drive({"do": "before", "tool": "read", "args": {}}, "", {})
    assert out == {"threw": True, "message": UNREACHABLE}


def test_a_socket_that_is_not_private_throws_and_is_never_asked(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0), mode=0o666)
    assert _before(gate.path, "read", {}) == {"threw": True, "message": UNREACHABLE}
    assert gate.requests == []


def test_a_symlink_to_a_private_socket_throws(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    link = tmp_path / "link.sock"
    link.symlink_to(gate.path)
    assert _before(link, "read", {}) == {"threw": True, "message": UNREACHABLE}
    assert gate.requests == []


def test_a_slow_gate_throws_within_five_and_a_half_seconds(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0), delay=6)
    started = time.monotonic()
    out = _before(gate.path, "read", {})
    elapsed = time.monotonic() - started
    assert out == {"threw": True, "message": UNREACHABLE}
    assert elapsed < 5.5 + 1.0, f"took {elapsed:.2f}s"


# --- state reports: to the gate socket, never to coppice ----------------------------------


def _coppice_listener(tmp_path: Path) -> tuple[Path, list]:
    """A socket at COPPICE_SOCK that records any connection. The plugin must
    never dial it: the gate does the fan-out from its own environment."""
    path = tmp_path / "c.sock"
    hits: list = []
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(path))
    srv.listen(4)

    def accept():
        while True:
            try:
                conn, _ = srv.accept()
            except OSError:
                return
            hits.append(1)
            conn.close()

    threading.Thread(target=accept, daemon=True).start()
    return path, hits


def test_idle_and_permission_events_report_to_the_gate_socket(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    coppice, hits = _coppice_listener(tmp_path)
    events = [
        {"type": "session.idle", "properties": {"sessionID": "ses_1"}},
        {
            "type": "permission.asked",
            "properties": {
                "id": "per_1",
                "sessionID": "ses_1",
                "permission": "bash",
                "patterns": ["rm -rf *"],
            },
        },
    ]
    env = {"COPPICE_PANE": "w1:p1", "COPPICE_SOCK": str(coppice)}
    assert _drive({"do": "events", "events": events}, gate.path, env) == {"done": True}
    reports = gate.reports()
    assert [r[0]["argv"] for r in reports] == [["hook", "report", "--pane", "w1:p1"]] * 2
    idle, blocked = (r[1] for r in reports)
    assert idle["state"] == "idle" and idle["harness"] == "opencode"
    assert idle["harness_session_id"] == "ses_1"
    assert blocked["state"] == "blocked"
    assert blocked["ask"]["id"] == "per_1"
    assert blocked["ask"]["tool"] == "bash"
    assert blocked["ask"]["summary"] == "rm -rf *"
    assert blocked["ask"]["deadline"] > time.time()
    for _, payload in reports:
        _validate_hook_report_row(dict(payload))  # the gate's own validator accepts it
    assert hits == [], "the plugin dialed COPPICE_SOCK"
    # The resident gate's own environment names no pane at all. These two
    # request fields are the only way its `hook report` dispatch can still
    # reach THIS pane's coppice socket rather than nobody's.
    for req, _payload in reports:
        assert req["coppice_sock"] == str(coppice)
        assert req["coppice_pane"] == "w1:p1"


def test_an_allow_carries_the_callers_coppice_sock_and_pane_on_the_request(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    coppice, hits = _coppice_listener(tmp_path)
    env = {"COPPICE_PANE": "w2:p7", "COPPICE_SOCK": str(coppice)}
    out = _before(gate.path, "read", {"filePath": "/proj/a.txt"}, env)
    assert out == {"threw": False}
    ((req, _payload),) = gate.gate_requests()
    assert req["coppice_sock"] == str(coppice)
    assert req["coppice_pane"] == "w2:p7"
    assert hits == [], "the plugin dialed COPPICE_SOCK directly"


def test_a_report_with_only_the_pane_set_carries_no_coppice_sock_field(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, gate.path, {"COPPICE_PANE": "w1:p1"})
    ((req, _payload),) = gate.reports()
    assert "coppice_sock" not in req
    assert "coppice_pane" not in req


def test_a_child_sessions_idle_is_not_the_panes_idle(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    events = [
        {
            "type": "session.created",
            "properties": {"info": {"id": "ses_child", "parentID": "ses_1"}},
        },
        {"type": "session.idle", "properties": {"sessionID": "ses_child"}},
    ]
    _drive({"do": "events", "events": events}, gate.path, {"COPPICE_PANE": "w1:p1"})
    assert gate.reports() == []


def test_a_report_outside_coppice_carries_no_pane(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, gate.path)
    ((req, payload),) = gate.reports()
    assert req["argv"] == ["hook", "report"]
    assert "pane" not in payload


def test_permission_ask_reports_blocked_and_never_decides(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    step = {
        "do": "permission.ask",
        "input": {"id": "per_1", "type": "bash", "sessionID": "ses_1", "title": "run rm -rf /"},
    }
    out = _drive(step, gate.path, {"COPPICE_PANE": "w1:p1"})
    assert out == {"output": {}}, "the plugin must never write output.status"
    ((req, payload),) = gate.reports()
    assert payload["state"] == "blocked"
    assert payload["ask"]["summary"] == "run rm -rf /"


def test_events_never_throw_on_garbage_or_an_unreachable_gate(tmp_path):
    events = [None, 3, {"type": "permission.asked"}, {"type": "session.idle", "properties": None}]
    assert _drive({"do": "events", "events": events}, tmp_path / "nope.sock") == {"done": True}


def test_a_report_skips_a_socket_that_is_not_private(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0), mode=0o666)
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, gate.path)
    assert gate.requests == []


def test_a_reply_past_the_size_cap_throws_before_the_timeout(tmp_path):
    big = b"x" * (2 * 1024 * 1024)
    gate = _Gate(tmp_path / "g.sock", big + b"\n", delay=0)
    started = time.monotonic()
    out = _before(gate.path, "read", {})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert time.monotonic() - started < 4.5


class _EndlessGate:
    """A trusted socket that streams bytes and never ends its reply."""

    def __init__(self, sock_path: Path):
        self.path = sock_path

        class _H(socketserver.StreamRequestHandler):
            def handle(self) -> None:
                self.rfile.readline()
                chunk = b"x" * 65536
                try:
                    while True:
                        self.wfile.write(chunk)
                        time.sleep(0.01)
                except OSError:
                    pass

        self.server = socketserver.UnixStreamServer(str(sock_path), _H)
        os.chmod(sock_path, 0o600)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()


def test_an_endless_reply_is_cut_at_the_size_cap(tmp_path):
    gate = _EndlessGate(tmp_path / "g.sock")
    started = time.monotonic()
    out = _before(gate.path, "read", {})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert time.monotonic() - started < 3.0


def test_a_relative_workdir_with_no_directory_is_a_deny_that_never_asks(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    out = _drive(
        {"do": "before", "tool": "bash", "args": {"command": "ls", "workdir": "sub"}},
        gate.path,
        {"DRIVE_DIRECTORY": ""},
    )
    assert out == {"threw": True, "message": UNREACHABLE}
    assert gate.gate_requests() == []


def test_a_call_carries_the_callers_coppice_data_dir_on_the_request(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    out = _before(
        gate.path, "read", {"filePath": "/proj/a.txt"}, {"COPPICE_DATA_DIR": "/srv/cdata"}
    )
    assert out == {"threw": False}
    ((req, _payload),) = gate.gate_requests()
    assert req["coppice_data_dir"] == "/srv/cdata"


def test_a_relative_coppice_data_dir_is_not_sent(tmp_path):
    gate = _Gate(tmp_path / "g.sock", _reply(0))
    _before(gate.path, "read", {"filePath": "/proj/a.txt"}, {"COPPICE_DATA_DIR": "cdata"})
    ((req, _payload),) = gate.gate_requests()
    assert "coppice_data_dir" not in req
