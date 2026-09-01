from __future__ import annotations

import json
import os
import socketserver
import subprocess
import threading
import time
from pathlib import Path

import pytest

from tests.harness_pi._node_check import node_status

HERE = Path(__file__).parent
_ok, _reason = node_status()
_NODE = pytest.mark.skipif(not _ok, reason=_reason)
_ENV_BASE = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}


class _FakeGateHandler(socketserver.StreamRequestHandler):
    reply: dict = {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}

    def handle(self) -> None:
        line = self.rfile.readline()
        json.loads(line)  # the request must at least be valid JSON
        self.wfile.write(json.dumps(self.reply).encode() + b"\n")


def _serve(sock_path: Path, handler, mode: int = 0o600) -> None:
    srv = socketserver.UnixStreamServer(str(sock_path), handler)
    os.chmod(sock_path, mode)
    threading.Thread(target=srv.serve_forever, daemon=True).start()


def _fake_gate(tmp_path: Path, reply: dict, mode: int = 0o600) -> Path:
    sock_path = tmp_path / "g.sock"
    handler = type("H", (_FakeGateHandler,), {"reply": reply})
    _serve(sock_path, handler, mode)
    return sock_path


def _ask(
    sock_path: Path,
    tool_name: str,
    tool_input: dict,
    cwd: str = "/repo",
    extra_env: dict | None = None,
) -> dict:
    result = subprocess.run(
        [
            "node",
            "--experimental-strip-types",
            str(HERE / "run_tool_call.mjs"),
            tool_name,
            json.dumps(tool_input),
            cwd,
        ],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock_path), **(extra_env or {}), **_ENV_BASE},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout.strip().splitlines()[-1])


@_NODE
def test_allow_and_argv_has_no_mode_but_has_verify_timeout(tmp_path):
    captured: list = []

    class _CapturingHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            line = self.rfile.readline()
            captured.append(json.loads(line))
            self.wfile.write(
                json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode() + b"\n"
            )

    sock_path = tmp_path / "g.sock"
    _serve(sock_path, _CapturingHandler)

    verdict = _ask(sock_path, "read", {"path": "/x"})
    assert verdict == {"allow": True, "reason": ""}
    argv = captured[0]["argv"]
    assert "--mode" not in argv
    assert argv == ["--format", "pi", "--root", str(tmp_path), "--verify-timeout", "4"]
    import base64

    payload = json.loads(base64.b64decode(captured[0]["stdin_b64"]))
    assert "session_id" not in payload  # unset: the gate falls back to the default envelope


@_NODE
def test_session_id_travels_when_the_env_names_one(tmp_path):
    captured: list = []

    class _CapturingHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            captured.append(json.loads(self.rfile.readline()))
            self.wfile.write(
                json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode() + b"\n"
            )

    sock_path = tmp_path / "g.sock"
    _serve(sock_path, _CapturingHandler)
    _ask(sock_path, "read", {"path": "/x"}, extra_env={"OPENDAISUGI_SESSION_ID": "sess-9"})
    import base64

    payload = json.loads(base64.b64decode(captured[0]["stdin_b64"]))
    assert payload["session_id"] == "sess-9"


@_NODE
def test_deny_carries_stderr_reason(tmp_path):
    sock = _fake_gate(
        tmp_path, {"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED - no", "exit_code": 2}
    )
    verdict = _ask(sock, "bash", {"command": "rm -rf /"})
    assert verdict["allow"] is False
    assert "DENIED" in verdict["reason"]


@_NODE
def test_extension_blocks_on_an_exit_code_it_does_not_recognise(tmp_path):
    """Neither 0 (allow) nor 2 (deny) is a verdict this extension knows how
    to read. A crashed gate, or a future gate.py bug, must not default to
    allow just because it is not exit 2."""
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "crashed", "exit_code": 1})
    verdict = _ask(sock, "read", {})
    assert verdict["allow"] is False


@_NODE
def test_extension_blocks_on_a_malformed_gate_reply(tmp_path):
    sock_path = tmp_path / "g.sock"

    class _GarbageHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            self.rfile.readline()
            self.wfile.write(b"not json at all\n")

    _serve(sock_path, _GarbageHandler)
    verdict = _ask(sock_path, "read", {})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_socket_absent_blocks_with_teaching_reason(tmp_path):
    verdict = _ask(tmp_path / "nope.sock", "read", {"path": "/x"})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_extension_blocks_when_the_socket_is_not_a_private_socket_we_own(tmp_path):
    """A world-writable socket that answers exit_code 0 must still block:
    a local process that wins the path must not be able to grant an allow.
    Mirrors gate_client.py's _socket_is_trustworthy."""
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}, mode=0o666)
    verdict = _ask(sock, "read", {"path": "/x"})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_slow_socket_blocks_within_five_and_a_half_seconds(tmp_path):
    class _SlowHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            self.rfile.readline()
            time.sleep(6)
            try:
                self.wfile.write(
                    json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode()
                    + b"\n"
                )
            except OSError:
                pass  # the client already gave up, as it must

    sock_path = tmp_path / "g.sock"
    _serve(sock_path, _SlowHandler)

    started = time.monotonic()
    verdict = _ask(sock_path, "read", {"path": "/x"})
    elapsed = time.monotonic() - started
    assert verdict["allow"] is False
    assert elapsed < 5.5, f"took {elapsed:.2f}s, want < 5.5s"


# --- reportState: fire-and-forget lifecycle reporting -------------------------------------


class _CapturingReportHandler(socketserver.StreamRequestHandler):
    captured: list = []

    def handle(self) -> None:
        import base64

        line = self.rfile.readline()
        req = json.loads(line)
        payload = json.loads(base64.b64decode(req["stdin_b64"]))
        type(self).captured.append((req, payload))
        self.wfile.write(
            json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode() + b"\n"
        )


def _report(sock_path: Path, state: str, extra_env: dict | None = None):
    return subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "run_report_state.mjs"), state],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock_path), **(extra_env or {}), **_ENV_BASE},
        capture_output=True,
        text=True,
        timeout=10,
    )


@_NODE
def test_report_state_sends_hook_report_shape_with_session_id_pane_split(tmp_path):
    handler = type("H", (_CapturingReportHandler,), {"captured": []})
    sock_path = tmp_path / "g.sock"
    _serve(sock_path, handler)

    result = _report(sock_path, "working", {"COPPICE_PANE": "w1:p3"})
    assert result.returncode == 0, result.stderr
    assert len(handler.captured) == 1
    req, payload = handler.captured[0]
    assert req["argv"] == ["hook", "report"]
    assert payload["harness"] == "pi"
    assert payload["state"] == "working"
    assert payload["source"] == "headless"
    assert payload["pane"] == "w1:p3"
    assert payload["session_id"] != "w1:p3"  # the pane id must not leak into session_id
    assert payload["session_id"]  # never empty: see reportState's fallback chain


@_NODE
def test_report_state_never_raises_when_gate_unreachable(tmp_path):
    result = _report(tmp_path / "nope.sock", "idle")
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout


@_NODE
def test_report_state_never_raises_on_a_malformed_reply(tmp_path):
    """A gate socket that exists but answers garbage must not throw or retry."""

    class _MalformedHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            self.rfile.readline()
            self.wfile.write(b"not json at all\n")

    sock_path = tmp_path / "g.sock"
    _serve(sock_path, _MalformedHandler)

    result = _report(sock_path, "idle")
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout


@_NODE
def test_report_state_skips_an_untrustworthy_socket_without_raising(tmp_path):
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}, mode=0o666)
    result = _report(sock, "idle")
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout
