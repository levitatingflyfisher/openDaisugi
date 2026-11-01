"""The OpenCode warden plugin, run under Node against fake warden sockets.

drive.mjs loads the plugin the way OpenCode's loaders do: OpenCode 1 calls
the default export's server(), OpenCode 2 its setup(ctx). Each test names the failure
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
UNREACHABLE = "openDaisugi warden unreachable: run `daisugi start`, or `daisugi warden serve`"


def _reply(exit_code: int, stderr: str = "") -> dict:
    return {"v": 1, "stdout": "", "stderr": stderr, "exit_code": exit_code}


class _Warden:
    """A fake warden socket that records each request and its decoded payload."""

    def __init__(self, sock_path: Path, reply: dict | bytes, mode: int = 0o600, delay: float = 0):
        self.path = sock_path
        self.requests: list[tuple[dict, dict]] = []
        warden = self

        class _H(socketserver.StreamRequestHandler):
            def handle(self) -> None:
                req = json.loads(self.rfile.readline())
                warden.requests.append((req, json.loads(base64.b64decode(req["stdin_b64"]))))
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

    def warden_requests(self) -> list[tuple[dict, dict]]:
        return [r for r in self.requests if r[0]["argv"][:1] != ["hook"]]

    def reports(self) -> list[tuple[dict, dict]]:
        return [r for r in self.requests if r[0]["argv"][:2] == ["hook", "report"]]


def _drive(step: dict, sock: Path | str, env: dict | None = None, timeout: float = 15) -> dict:
    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "drive.mjs"), json.dumps(step)],
        cwd=HERE,
        env={"OPENDAISUGI_WARDEN_SOCK": str(sock), **(env or {}), **_ENV_BASE},
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout.strip().splitlines()[-1])


def _before(sock, tool: str, args, env: dict | None = None) -> dict:
    return _drive({"do": "before", "tool": tool, "args": args}, sock, env)


# --- the loader ---------------------------------------------------------------------------


def test_the_plugin_file_exports_one_definition_for_both_opencode_apis(tmp_path):
    """OpenCode 2 refuses a plugin whose default export is not an object with
    an id and a setup function, logs one WARN and runs with no warden. Seen
    live on 2.2.6 with the v1-only file. OpenCode 1 calls server()."""
    out = _drive({"do": "exports"}, tmp_path / "unused.sock")
    assert out == {
        "names": ["default"],
        "id": "daisugi-warden",
        "setup": "function",
        "server": "function",
    }


def _v2before(sock, event: dict, env: dict | None = None) -> dict:
    return _drive({"do": "v2before", "event": event}, sock, env)


# The execute.before event OpenCode 2.2.6 sends, as seen live.
_V2_READ = {
    "tool": "read",
    "sessionID": "ses_2",
    "agent": "build",
    "messageID": "msg_1",
    "id": "call_2",
    "input": {"path": "note.txt"},
}


def test_v2_an_allow_sends_the_mapped_payload(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    assert _v2before(warden.path, _V2_READ) == {"threw": False}
    ((req, payload),) = warden.warden_requests()
    assert req["argv"] == ["--format", "opencode", "--verify-timeout", "4"]
    assert payload == {
        "tool_name": "Read",
        "tool_input": {"path": "note.txt"},
        "session_id": "ses_2",
        "cwd": "/proj",
        "tool_use_id": "call_2",
    }


def test_v2_the_shell_tool_is_bash_and_its_workdir_is_the_cwd(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    event = {**_V2_READ, "tool": "shell", "input": {"command": "ls", "workdir": "sub"}}
    assert _v2before(warden.path, event) == {"threw": False}
    ((_req, payload),) = warden.warden_requests()
    assert payload["tool_name"] == "Bash"
    assert payload["cwd"] == "/proj/sub"


def test_v2_a_deny_throws_the_wardens_own_reason(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(2, "no: outside the envelope"))
    assert _v2before(warden.path, _V2_READ) == {
        "threw": True,
        "message": "no: outside the envelope",
    }


def test_v2_no_warden_throws_with_the_start_hint(tmp_path):
    assert _v2before(tmp_path / "missing.sock", _V2_READ) == {"threw": True, "message": UNREACHABLE}


def test_v2_a_finished_turn_reports_idle_and_setup_returns_a_cleanup(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    events = [
        {"type": "session.step.ended", "data": {"sessionID": "ses_2"}},
        {"type": "session.execution.succeeded", "data": {"sessionID": "ses_2"}},
    ]
    out = _drive({"do": "v2events", "events": events}, warden.path, {"COPPICE_PANE": "w1:p1"})
    assert out == {"done": True, "cleanup": "function"}
    ((req, idle),) = warden.reports()
    assert req["argv"] == ["hook", "report", "--pane", "w1:p1"]
    assert idle["state"] == "idle" and idle["harness_session_id"] == "ses_2"


# --- tool.execute.before: allow only on exit 0 ---------------------------------------------


def test_an_allow_sends_the_bare_warden_argv_and_the_mapped_payload(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    out = _before(warden.path, "read", {"filePath": "/proj/a.txt"})
    assert out == {"threw": False}
    ((req, payload),) = warden.warden_requests()
    assert req["v"] == 1
    assert req["argv"] == ["--format", "opencode", "--verify-timeout", "4"]
    assert "--mode" not in req["argv"]
    assert payload == {
        "tool_name": "Read",
        "tool_input": {"filePath": "/proj/a.txt"},
        "session_id": "ses_1",
        "cwd": "/proj",
        "tool_use_id": "call_1",
    }


def test_every_built_in_tool_maps_to_the_wardens_own_name(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    ids = ["bash", "read", "write", "edit", "glob", "grep", "webfetch", "websearch"]
    for tool in ids:
        _before(warden.path, tool, {})
    names = [p["tool_name"] for _, p in warden.warden_requests()]
    assert names == ["Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch"]


def test_any_other_tool_id_goes_to_the_warden_as_an_opencode_mcp_call(tmp_path):
    """An unmapped id must not pass through as itself: a plugin tool named
    Bash or mcp__x__y would then borrow another tool's rules. Object keys
    such as constructor must not resolve through the prototype."""
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    for tool in ("apply_patch", "Bash", "mcp__x__y", "constructor", "__proto__"):
        _before(warden.path, tool, {})
    names = [p["tool_name"] for _, p in warden.warden_requests()]
    assert names == [
        "mcp__opencode__apply_patch",
        "mcp__opencode__Bash",
        "mcp__opencode__mcp__x__y",
        "mcp__opencode__constructor",
        "mcp__opencode____proto__",
    ]


def test_a_bash_workdir_is_the_cwd_the_warden_checks(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    _before(warden.path, "bash", {"command": "rm x", "workdir": "sub"})
    _before(warden.path, "bash", {"command": "rm x", "workdir": "/elsewhere"})
    _before(warden.path, "bash", {"command": "ls"})
    cwds = [p["cwd"] for _, p in warden.warden_requests()]
    assert cwds == ["/proj/sub", "/elsewhere", "/proj"]


def test_a_workdir_that_is_not_a_string_is_a_deny_that_never_asks(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    out = _before(warden.path, "bash", {"command": "ls", "workdir": ["x"]})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert warden.warden_requests() == []


def test_the_daisugi_session_id_names_the_envelope_when_set(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    _before(warden.path, "read", {}, {"OPENDAISUGI_SESSION_ID": "sess-9"})
    assert warden.warden_requests()[0][1]["session_id"] == "sess-9"


def test_a_deny_throws_the_wardens_own_reason(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(2, "openDaisugi warden: DENIED - shell head 'rm'"))
    out = _before(warden.path, "bash", {"command": "rm -rf /"})
    assert out == {"threw": True, "message": "openDaisugi warden: DENIED - shell head 'rm'"}


def test_an_exit_code_that_is_not_a_verdict_throws(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(1, "crashed"))
    assert _before(warden.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_a_malformed_reply_throws(tmp_path):
    warden = _Warden(tmp_path / "g.sock", b"not json at all\n")
    assert _before(warden.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_a_reply_without_the_schema_version_throws(tmp_path):
    warden = _Warden(tmp_path / "g.sock", {"stdout": "", "stderr": "", "exit_code": 0})
    assert _before(warden.path, "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_no_socket_throws_with_the_start_hint(tmp_path):
    assert _before(tmp_path / "nope.sock", "read", {}) == {"threw": True, "message": UNREACHABLE}


def test_no_home_and_no_socket_env_throws(tmp_path):
    out = _drive({"do": "before", "tool": "read", "args": {}}, "", {})
    assert out == {"threw": True, "message": UNREACHABLE}


def test_a_socket_that_is_not_private_throws_and_is_never_asked(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0), mode=0o666)
    assert _before(warden.path, "read", {}) == {"threw": True, "message": UNREACHABLE}
    assert warden.requests == []


def test_a_symlink_to_a_private_socket_throws(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    link = tmp_path / "link.sock"
    link.symlink_to(warden.path)
    assert _before(link, "read", {}) == {"threw": True, "message": UNREACHABLE}
    assert warden.requests == []


def test_a_slow_warden_throws_within_five_and_a_half_seconds(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0), delay=6)
    started = time.monotonic()
    out = _before(warden.path, "read", {})
    elapsed = time.monotonic() - started
    assert out == {"threw": True, "message": UNREACHABLE}
    assert elapsed < 5.5 + 1.0, f"took {elapsed:.2f}s"


# --- state reports: to the warden socket, never to coppice ----------------------------------


def _coppice_listener(tmp_path: Path) -> tuple[Path, list]:
    """A socket at COPPICE_SOCK that records any connection. The plugin must
    never dial it: the warden does the fan-out from its own environment."""
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


def test_idle_and_permission_events_report_to_the_warden_socket(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
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
    assert _drive({"do": "events", "events": events}, warden.path, env) == {"done": True}
    reports = warden.reports()
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
        _validate_hook_report_row(dict(payload))  # the warden's own validator accepts it
    assert hits == [], "the plugin dialed COPPICE_SOCK"
    # The resident warden's own environment names no pane at all. These two
    # request fields are the only way its `hook report` dispatch can still
    # reach THIS pane's coppice socket rather than nobody's.
    for req, _payload in reports:
        assert req["coppice_sock"] == str(coppice)
        assert req["coppice_pane"] == "w1:p1"


def test_an_allow_carries_the_callers_coppice_sock_and_pane_on_the_request(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    coppice, hits = _coppice_listener(tmp_path)
    env = {"COPPICE_PANE": "w2:p7", "COPPICE_SOCK": str(coppice)}
    out = _before(warden.path, "read", {"filePath": "/proj/a.txt"}, env)
    assert out == {"threw": False}
    ((req, _payload),) = warden.warden_requests()
    assert req["coppice_sock"] == str(coppice)
    assert req["coppice_pane"] == "w2:p7"
    assert hits == [], "the plugin dialed COPPICE_SOCK directly"


def test_a_report_with_only_the_pane_set_carries_no_coppice_sock_field(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, warden.path, {"COPPICE_PANE": "w1:p1"})
    ((req, _payload),) = warden.reports()
    assert "coppice_sock" not in req
    assert "coppice_pane" not in req


def test_a_child_sessions_idle_is_not_the_panes_idle(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    events = [
        {
            "type": "session.created",
            "properties": {"info": {"id": "ses_child", "parentID": "ses_1"}},
        },
        {"type": "session.idle", "properties": {"sessionID": "ses_child"}},
    ]
    _drive({"do": "events", "events": events}, warden.path, {"COPPICE_PANE": "w1:p1"})
    assert warden.reports() == []


def test_a_report_outside_coppice_carries_no_pane(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, warden.path)
    ((req, payload),) = warden.reports()
    assert req["argv"] == ["hook", "report"]
    assert "pane" not in payload


def test_permission_ask_reports_blocked_and_never_decides(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    step = {
        "do": "permission.ask",
        "input": {"id": "per_1", "type": "bash", "sessionID": "ses_1", "title": "run rm -rf /"},
    }
    out = _drive(step, warden.path, {"COPPICE_PANE": "w1:p1"})
    assert out == {"output": {}}, "the plugin must never write output.status"
    ((req, payload),) = warden.reports()
    assert payload["state"] == "blocked"
    assert payload["ask"]["summary"] == "run rm -rf /"


def test_events_never_throw_on_garbage_or_an_unreachable_warden(tmp_path):
    events = [None, 3, {"type": "permission.asked"}, {"type": "session.idle", "properties": None}]
    assert _drive({"do": "events", "events": events}, tmp_path / "nope.sock") == {"done": True}


def test_a_report_skips_a_socket_that_is_not_private(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0), mode=0o666)
    events = [{"type": "session.idle", "properties": {"sessionID": "ses_1"}}]
    _drive({"do": "events", "events": events}, warden.path)
    assert warden.requests == []


def test_a_reply_past_the_size_cap_throws_before_the_timeout(tmp_path):
    big = b"x" * (2 * 1024 * 1024)
    warden = _Warden(tmp_path / "g.sock", big + b"\n", delay=0)
    started = time.monotonic()
    out = _before(warden.path, "read", {})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert time.monotonic() - started < 4.5


class _EndlessWarden:
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
    warden = _EndlessWarden(tmp_path / "g.sock")
    started = time.monotonic()
    out = _before(warden.path, "read", {})
    assert out == {"threw": True, "message": UNREACHABLE}
    assert time.monotonic() - started < 3.0


def test_a_relative_workdir_with_no_directory_is_a_deny_that_never_asks(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    out = _drive(
        {"do": "before", "tool": "bash", "args": {"command": "ls", "workdir": "sub"}},
        warden.path,
        {"DRIVE_DIRECTORY": ""},
    )
    assert out == {"threw": True, "message": UNREACHABLE}
    assert warden.warden_requests() == []


def test_a_call_carries_the_callers_coppice_data_dir_on_the_request(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    out = _before(
        warden.path, "read", {"filePath": "/proj/a.txt"}, {"COPPICE_DATA_DIR": "/srv/cdata"}
    )
    assert out == {"threw": False}
    ((req, _payload),) = warden.warden_requests()
    assert req["coppice_data_dir"] == "/srv/cdata"


def test_a_relative_coppice_data_dir_is_not_sent(tmp_path):
    warden = _Warden(tmp_path / "g.sock", _reply(0))
    _before(warden.path, "read", {"filePath": "/proj/a.txt"}, {"COPPICE_DATA_DIR": "cdata"})
    ((req, _payload),) = warden.warden_requests()
    assert "coppice_data_dir" not in req


# --- the socket under a moved data home --------------------------------------------------


@pytest.mark.parametrize("var", ["OPENDAISUGI_HOME", "XDG_DATA_HOME"])
def test_the_socket_is_found_under_a_moved_data_home(tmp_path, var):
    """With no OPENDAISUGI_WARDEN_SOCK, the plugin finds the warden where the
    data home rule puts it (opendaisugi.datahome), as daisugi warden serve does."""
    data = tmp_path / "moved" if var == "OPENDAISUGI_HOME" else tmp_path / "moved" / "opendaisugi"
    (data / "warden").mkdir(parents=True)
    (tmp_path / "home").mkdir()
    warden = _Warden(data / "warden" / "warden.sock", _reply(0))
    step = {"do": "before", "tool": "read", "args": {"filePath": "/proj/a.txt"}}
    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "drive.mjs"), json.dumps(step)],
        cwd=HERE,
        env={"HOME": str(tmp_path / "home"), var: str(tmp_path / "moved"), **_ENV_BASE},
        capture_output=True,
        text=True,
        timeout=15,
    )
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout.strip().splitlines()[-1]) == {"threw": False}
    assert len(warden.warden_requests()) == 1


def test_v1s_second_loader_with_no_tool_domain_stays_quiet(tmp_path):
    """OpenCode 1 may call setup with no tool domain; server() is its warden."""
    for ctx in ({}, {"app": {"version": "1.18.32"}}):
        out = _drive({"do": "v2bare", "ctx": ctx}, tmp_path / "unused.sock")
        assert out == {"threw": False, "cleanup": "undefined"}, ctx


def test_v2_with_no_tool_hook_fails_loud_and_names_the_version(tmp_path):
    """An OpenCode 2 that moves tool.hook must not run with a quiet, absent
    warden: the throw is OpenCode's one WARN line."""
    for ctx in (
        {"app": {"version": "2.9.0"}, "tool": {}},
        {"app": {"version": "2.9.0"}},
        {"tool": {}},
    ):
        out = _drive({"do": "v2bare", "ctx": ctx}, tmp_path / "unused.sock")
        assert out["threw"] is True, ctx
        assert "the warden is OFF" in out["message"]
        assert ("OpenCode 2.9.0" in out["message"]) == ("app" in ctx)
