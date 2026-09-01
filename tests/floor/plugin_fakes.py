"""A fake coppice socket and a fake gh for the example plugin tests.

The fake socket accepts one connection. It answers each request from a
table, sends a list of event lines after the subscribe reply, and records
every request line. It closes the connection once the plugin has been
quiet for a moment, which ends the plugin's event loop.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import threading
from collections.abc import Callable
from pathlib import Path

PLUGINS = Path(__file__).resolve().parents[2] / "harness" / "coppice" / "plugins"


class FakeFloor:
    def __init__(
        self,
        path: Path,
        answers: dict[str, Callable[[dict], dict]] | None = None,
        events: list[dict] | None = None,
        quiet: float = 0.6,
    ) -> None:
        self.path = path
        self.answers = answers or {}
        self.events = events or []
        self.quiet = quiet
        self.requests: list[dict] = []
        self.srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.srv.bind(str(path))
        self.srv.listen(1)
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        self.srv.settimeout(20)
        try:
            conn, _ = self.srv.accept()
        except OSError:
            return
        buf = b""
        conn.settimeout(self.quiet)
        with conn:
            while True:
                try:
                    chunk = conn.recv(65536)
                except TimeoutError:
                    return
                if not chunk:
                    return
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    req = json.loads(line)
                    self.requests.append(req)
                    self._answer(conn, req)

    def _answer(self, conn: socket.socket, req: dict) -> None:
        cmd = req.get("cmd")
        fn = self.answers.get(cmd)
        if fn is None:
            reply = {"id": req.get("id"), "ok": True, "result": {}}
        else:
            out = fn(req)
            if "error" in out:
                reply = {"id": req.get("id"), "ok": False, "error": out["error"]}
            else:
                reply = {"id": req.get("id"), "ok": True, "result": out}
        conn.sendall(json.dumps(reply).encode() + b"\n")
        if cmd == "events.subscribe":
            for ev in self.events:
                conn.sendall(json.dumps(ev).encode() + b"\n")

    def close(self) -> None:
        self.srv.close()

    def sent(self, cmd: str) -> list[dict]:
        return [r for r in self.requests if r.get("cmd") == cmd]


def state(pane: str, st: str, ts: float, ask: dict | None = None) -> dict:
    ev = {
        "event": "state",
        "v": 1,
        "ts": ts,
        "session_id": "s-" + pane,
        "harness_session_id": None,
        "harness": "claude-code",
        "pane": pane,
        "state": st,
        "source": "gate",
        "detail": "",
    }
    if ask is not None:
        ev["ask"] = ask
    return ev


def run_plugin(
    plugin: str,
    script: str,
    sock: Path,
    config: dict | None = None,
    extra_env: dict | None = None,
) -> subprocess.CompletedProcess:
    """Run a shipped plugin script against the fake socket until it exits."""
    env = {
        "PATH": os.environ.get("PATH", ""),
        "COPPICE_SOCKET": str(sock),
        "COPPICE_PLUGIN": plugin,
        "COPPICE_PLUGIN_CONFIG": json.dumps(config or {}),
    }
    env.update(extra_env or {})
    return subprocess.run(
        [sys.executable, str(PLUGINS / plugin / script)],
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )


def fake_gh(bin_dir: Path, log: Path, pr_state: str, checks: list[int]) -> None:
    """Write a gh that logs its argv and answers pr view and pr checks.

    pr_state is the state pr view reports, or "" for no PR. checks is the
    exit code of each pr checks call in turn; the last repeats.
    """
    bin_dir.mkdir(parents=True, exist_ok=True)
    counter = bin_dir / "checks-calls"
    body = f"""#!{sys.executable}
import json, pathlib, sys
log = pathlib.Path({str(log)!r})
with log.open("a") as f:
    f.write(json.dumps(sys.argv[1:]) + "\\n")
args = sys.argv[1:]
if args[:2] == ["pr", "view"]:
    if not {pr_state!r}:
        print("no pull requests found", file=sys.stderr)
        sys.exit(1)
    print(json.dumps({{"state": {pr_state!r}, "number": 7}}))
    sys.exit(0)
if args[:2] == ["pr", "checks"]:
    c = pathlib.Path({str(counter)!r})
    n = int(c.read_text()) if c.exists() else 0
    c.write_text(str(n + 1))
    codes = {checks!r}
    sys.exit(codes[min(n, len(codes) - 1)])
sys.exit(2)
"""
    gh = bin_dir / "gh"
    gh.write_text(body)
    gh.chmod(0o755)


def assert_ended(proc: subprocess.CompletedProcess) -> None:
    """The plugin ran until the fake closed the socket.

    A lost connection is a failure, so the runner starts the plugin again.
    Exit 0 is kept for a plugin with nothing to do.
    """
    assert proc.returncode == 1, (proc.returncode, proc.stderr)
    assert "the server closed the connection" in proc.stderr, proc.stderr
