"""Resident gate cases: requests over gate.sock, answered by the Python server, the oracle.

    uv run --no-sync python clients/gate_server_cases.py [--out clients/fixtures/gate_server]

`daisugi gate serve` answers one JSON line per connection on
``<root>/gate.sock``: the pi extension, the OpenCode plugin and
gate_client.py all speak it. A case here is either one request (the bytes a
client sends, the gate state it runs against, the fake coppice and herdr it
may reach) or a scenario (concurrent clients, a slow client, a client that
goes away mid-request, the server's own start and stop). The oracle is the
Python server (``python -m opendaisugi.cli gate serve``); what it did is
recorded: the reply bytes, and every line written to the audit log, the
session tree, a fake coppice socket and a fake herdr binary.
``clients/gate_server_compare.py`` runs a binary's ``gate serve`` on the
same cases and compares.

Paths are normalized: a request's own scratch directory is ``{ROOT}``, the
server's scratch directory is ``{SRV}``. HOME is /home/user, a directory
that does not exist, as in the gate cases. The cases hold no content from
any real session and may be committed.

The file is content-addressed as the gate cases are: each case's ``id`` is
the first 16 hex digits of the SHA-256 of its canonical JSON without the
``id`` key, lines are sorted by id, and a manifest pins the file's bytes.

A case may carry ``port_expect``: fields where a port's answer differs from
the oracle's by a ruling (clients/ADJUDICATIONS.md, G2-n), and
``rust_expect``: fields where the Rust port's alone does (an RG ruling).
The compare checks the port against the oracle's answer with those fields
replaced.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import shutil
import signal
import socket
import stat
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, Callable

sys.path.insert(0, str(Path(__file__).resolve().parent))

from gate_cases import (  # noqa: E402 - sibling module, run as a script
    _HEX8,
    BASE,
    REPO,
    _norm_value,
    _read_jsonl,
    _stamp,
    bash,
    canonical_json,
    case_id,
    envelope,
    host_verdict,
    payload,
)

FIXTURE_DIR = REPO / "clients" / "fixtures" / "gate_server"
SCRATCH = Path(
    os.environ.get("DAISUGI_GATE_SERVER_SCRATCH")
    or Path.home() / "opendaisugi-scratch" / "g2" / "runs"
)
CASE_VERSION = 1
FAKE_HOME = "/home/user"
ENFORCE = "gate_mode: enforce\n"
CLIENT_PID = "<client pid>"
BAD_REQUEST_LIMIT = 4 * 1024 * 1024


def oracle_cmd() -> list[str]:
    return [sys.executable, "-m", "opendaisugi.cli", "gate", "serve"]


def binary_cmd(binary: str) -> list[str]:
    return [binary, "gate", "serve"]


# ---------------------------------------------------------------------------
# Fakes: coppice and herdr
# ---------------------------------------------------------------------------


class FakeCoppice:
    """A coppice socket. It records every line it is sent and answers the
    way ``mode`` says:

    - ``ok``: a hello is placed as the pane it names; a report is ok.
    - ``other``: a hello is placed as another pane (w9:p9).
    - ``bare``: every line gets ``{"id": "r", "ok": true}`` and nothing more,
      the answer the gate cases' fake gives.
    """

    def __init__(self, path: Path, mode: str = "ok") -> None:
        self.path = path
        self.mode = mode
        self.lines: list[str] = []
        self._lock = threading.Lock()
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(str(path))
        self.sock.listen(16)
        self.sock.settimeout(0.05)
        self._stop = False
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _answer(self, line: bytes) -> bytes:
        try:
            msg = json.loads(line)
        except ValueError:
            msg = {}
        if not isinstance(msg, dict):
            msg = {}
        if self.mode == "bare":
            return b'{"id": "r", "ok": true}\n'
        if msg.get("cmd") == "hello":
            pane = msg.get("pane") if self.mode == "ok" else "w9:p9"
            body = {"id": msg.get("id"), "ok": True, "result": {"role": "pane", "pane": pane}}
            return json.dumps(body).encode() + b"\n"
        return json.dumps({"id": msg.get("id"), "ok": True}).encode() + b"\n"

    def _conn(self, conn: socket.socket) -> None:
        with conn:
            conn.settimeout(2.0)
            buf = b""
            try:
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    buf += chunk
                    while b"\n" in buf:
                        line, buf = buf.split(b"\n", 1)
                        with self._lock:
                            self.lines.append(line.decode("utf-8", "replace"))
                        conn.sendall(self._answer(line))
            except OSError:
                pass
            if buf:
                with self._lock:
                    self.lines.append(buf.decode("utf-8", "replace"))

    def _serve(self) -> None:
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except (TimeoutError, OSError):
                continue
            threading.Thread(target=self._conn, args=(conn,), daemon=True).start()

    def take(self) -> list[str]:
        """The lines so far, and forget them."""
        time.sleep(0.05)
        with self._lock:
            out, self.lines = self.lines, []
        return out

    def close(self) -> list[str]:
        out = self.take()
        self._stop = True
        self.thread.join(1.0)
        self.sock.close()
        return out


FAKE_HERDR = """#!/bin/sh
printf '%s\\n' "$*" >> "$HERDR_LOG"
"""


# ---------------------------------------------------------------------------
# The server under test
# ---------------------------------------------------------------------------


def _default_sigint() -> None:
    # The server is started as a terminal would start it: SIGINT at its
    # default, whatever this runner inherited.
    signal.signal(signal.SIGINT, signal.SIG_DFL)


def sock_ready(path: Path) -> bool:
    """The socket a client trusts: a socket this uid owns, mode 0600, that
    accepts a connection."""
    try:
        st = os.lstat(path)
    except OSError:
        return False
    if not stat.S_ISSOCK(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600:
        return False
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(1.0)
            s.connect(str(path))
    except OSError:
        return False
    return True


class Server:
    """One `gate serve` process with its own scratch directory."""

    def __init__(
        self,
        cmd: list[str],
        root: Path,
        env: dict[str, str],
        logs: Path,
        *,
        wait: bool = True,
        name: str = "server",
    ) -> None:
        self.root = root
        self.sock = root / "gate.sock"
        logs.mkdir(parents=True, exist_ok=True)
        self.out_path = logs / f"{name}.out"
        self.err_path = logs / f"{name}.err"
        self.proc = subprocess.Popen(  # noqa: S603 - the server under test
            cmd + ["--root", str(root)],
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=self.out_path.open("wb"),
            stderr=self.err_path.open("wb"),
            cwd=logs,
            preexec_fn=_default_sigint,  # noqa: PLW1509 - no threads touch this
        )
        if wait:
            self.wait_ready()

    def wait_ready(self, timeout: float = 60.0) -> None:
        t0 = time.monotonic()
        while time.monotonic() - t0 < timeout:
            if self.proc.poll() is not None:
                raise RuntimeError(f"server exited {self.proc.returncode}: {self.stderr()!r}")
            if sock_ready(self.sock):
                return
            time.sleep(0.02)
        raise RuntimeError("server never became ready")

    def stderr(self) -> str:
        return self.err_path.read_bytes().decode("utf-8", "replace")

    def stop(self, sig: int = signal.SIGINT, timeout: float = 15.0) -> int | None:
        if self.proc.poll() is None:
            self.proc.send_signal(sig)
            try:
                self.proc.wait(timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(5)
                return None
        return self.proc.returncode


def exchange(
    sock: Path, data: bytes, *, shut: bool = False, timeout: float = 60.0
) -> tuple[bytes, float]:
    """Send one request; read until the server closes. (reply, seconds)."""
    t0 = time.perf_counter()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(timeout)
        # The oracle listens with a backlog of 5 (socketserver's default),
        # so a burst of clients can find the queue full: connect again.
        for _ in range(500):
            try:
                s.connect(str(sock))
                break
            except BlockingIOError:
                time.sleep(0.01)
        try:
            s.sendall(data)
            if shut:
                s.shutdown(socket.SHUT_WR)
        except OSError:
            pass
        buf = b""
        while True:
            try:
                chunk = s.recv(65536)
            except ConnectionResetError:
                break
            if not chunk:
                break
            buf += chunk
    return buf, time.perf_counter() - t0


# ---------------------------------------------------------------------------
# Request cases
# ---------------------------------------------------------------------------


def _sub(v: Any, root: str) -> Any:
    if isinstance(v, str):
        return v.replace("{ROOT}", root)
    if isinstance(v, list):
        return [_sub(x, root) for x in v]
    if isinstance(v, dict):
        return {k: _sub(x, root) for k, x in v.items()}
    return v


def request_bytes(case: dict[str, Any], root: str) -> bytes:
    """The bytes the client sends for this case."""
    if "raw_hex" in case:
        return bytes.fromhex(case["raw_hex"]).replace(b"{ROOT}", root.encode())
    r = case["request"]
    body: dict[str, Any] = {"v": 1}
    if "argv" in r:
        body["argv"] = _sub(r["argv"], root)
    if "stdin_b64" in r:
        body["stdin_b64"] = r["stdin_b64"]
    elif "stdin" in r:
        body["stdin_b64"] = base64.b64encode(_sub(r["stdin"], root).encode("utf-8")).decode()
    body.update(_sub(r.get("fields") or {}, root))
    if r.get("style") == "js":
        # JSON.stringify: compact, non-ASCII as UTF-8.
        text = json.dumps(body, separators=(",", ":"), ensure_ascii=False)
    else:
        text = json.dumps(body)
    return text.encode("utf-8") + (b"" if r.get("no_newline") else b"\n")


def prepare(case: dict[str, Any], workdir: Path) -> None:
    if workdir.exists():
        shutil.rmtree(workdir)
    data = workdir / "data"
    gate_root = data / "gate"
    gate_root.mkdir(parents=True)
    st = case.get("state") or {}
    root = str(workdir)
    if st.get("envelopes"):
        envd = gate_root / "envelopes"
        envd.mkdir()
        for name, body in st["envelopes"].items():
            (envd / f"{name}.json").write_text(body.replace("{ROOT}", root), encoding="utf-8")
    if st.get("disarmed"):
        (gate_root / "DISARMED").write_text("disarmed by operator\n", encoding="utf-8")
    if st.get("config") is not None:
        (data / "config.yaml").write_text(st["config"], encoding="utf-8")
    for sid, text in (st.get("sessions") or {}).items():
        sd = data / "sessions"
        sd.mkdir(exist_ok=True)
        (sd / f"{sid}.jsonl").write_text(text, encoding="utf-8")


def _norm_text(s: str, root: str, srv: str) -> str:
    if root:
        s = s.replace(root, "{ROOT}")
    return s.replace(srv, "{SRV}")


def _norm_all(v: Any, root: str, srv: str) -> Any:
    v = _norm_value(v, root)
    return _norm_value_srv(v, srv)


def _norm_value_srv(v: Any, srv: str) -> Any:
    if isinstance(v, str):
        return v.replace(srv, "{SRV}")
    if isinstance(v, list):
        return [_norm_value_srv(x, srv) for x in v]
    if isinstance(v, dict):
        return {k: _norm_value_srv(x, srv) for k, x in v.items()}
    return v


def norm_coppice(lines: list[str], root: str, srv: str) -> list[Any]:
    out = []
    for line in lines:
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            out.append({"<unparseable line>": _norm_text(line, root, srv)})
            continue
        if isinstance(msg, dict):
            if isinstance(msg.get("event"), dict):
                _stamp(msg["event"], ("ts",))
                if isinstance(msg["event"].get("ask"), dict):
                    _stamp(msg["event"]["ask"], ("deadline",))
            pids = msg.get("peer_pids")
            if isinstance(pids, list):
                msg["peer_pids"] = [CLIENT_PID if p == os.getpid() else p for p in pids]
        out.append(_norm_all(msg, root, srv))
    return out


def side_effects(workdir: Path, srv: str) -> dict[str, Any]:
    root = str(workdir)
    data = workdir / "data"
    audit = []
    for f in sorted((data / "gate" / "audit").glob("*.jsonl")):
        for rec in _read_jsonl(f):
            if isinstance(rec, dict):
                _stamp(rec, ("at", "elapsed_ms"))
            audit.append({"file": f.name, "record": _norm_all(rec, root, srv)})
    tree: dict[str, list[Any]] = {}
    sd = data / "sessions"
    for f in sorted(sd.glob("*.jsonl")) if sd.exists() else []:
        ids: dict[str, str] = {}
        rows = []
        for row in _read_jsonl(f):
            if isinstance(row, dict):
                _stamp(row, ("ts", "latencyMs"))
                if isinstance(row.get("ask"), dict):
                    _stamp(row["ask"], ("deadline",))
                for k in ("id", "parentId", "leafId"):
                    if isinstance(row.get(k), str) and _HEX8.match(row[k]):
                        row[k] = ids.setdefault(row[k], f"#{len(ids) + 1}")
            rows.append(_norm_all(row, root, srv))
        tree[f.name] = rows
    modes = {}
    if sd.exists():
        modes["sessions"] = oct(stat.S_IMODE(os.lstat(sd).st_mode))
        for f in sorted(sd.glob("*.jsonl")):
            modes[f"sessions/{f.name}"] = oct(stat.S_IMODE(os.lstat(f).st_mode))
    captures: dict[str, list[Any]] = {}
    capd = workdir / "captures"
    for f in sorted(capd.glob("*.jsonl")) if capd.exists() else []:
        crow = []
        for row in _read_jsonl(f):
            if isinstance(row, dict):
                _stamp(row, ("captured_at",))
            crow.append(_norm_all(row, root, srv))
        captures[f.name] = crow
    return {"audit": audit, "tree": tree, "modes": modes, "captures": captures}


def reply_obs(raw: bytes, root: str, srv: str) -> str:
    return _norm_text(raw.decode("utf-8", "replace"), root, srv)


def reply_verdict(reply: str, fmt: str = "claude") -> str:
    """allow or deny, as a client reads the reply. No reply is a deny."""
    try:
        body = json.loads(reply)
    except ValueError:
        return "deny"
    if not isinstance(body, dict) or not isinstance(body.get("exit_code"), int):
        return "deny"
    return host_verdict({"exit": body["exit_code"], "stdout": str(body.get("stdout") or "")})


class Group:
    """One running server and the fakes around it, for the request cases
    of one server environment."""

    def __init__(self, cmd: list[str], name: str, base: Path) -> None:
        self.name = name
        self.dir = base / name
        if self.dir.exists():
            shutil.rmtree(self.dir)
        (self.dir / "bin").mkdir(parents=True)
        herdr = self.dir / "bin" / "herdr"
        herdr.write_text(FAKE_HERDR, encoding="utf-8")
        herdr.chmod(0o755)
        self.herdr_log = self.dir / "herdr.log"
        env = {
            "HOME": FAKE_HOME,
            "PATH": f"{self.dir / 'bin'}:/usr/local/bin:/usr/bin:/bin",
            "PYTHONPATH": str(REPO / "src"),
            "CUDA_VISIBLE_DEVICES": "",
            "HERDR_LOG": str(self.herdr_log),
        }
        self.server_coppice = None
        if name == "paneenv":
            self.server_coppice = FakeCoppice(self.dir / "srvcop.sock", "ok")
            env.update(
                {
                    "COPPICE_SOCK": str(self.dir / "srvcop.sock"),
                    "COPPICE_PANE": "w5:p5",
                    "HERDR_PANE_ID": "h9",
                    "HERDR_PANE": "h8",
                    "TMUX_PANE": "%9",
                    "COPPICE_DATA_DIR": "/srv/sdata",
                }
            )
        self.server = Server(cmd, self.dir / "gate", env, self.dir)

    def herdr_lines(self) -> list[str]:
        try:
            lines = self.herdr_log.read_text(encoding="utf-8").splitlines()
        except FileNotFoundError:
            return []
        self.herdr_log.unlink()
        return lines

    def run(self, case: dict[str, Any], workdir: Path) -> tuple[dict[str, Any], float]:
        prepare(case, workdir)
        root, srv = str(workdir), str(self.dir)
        cop = FakeCoppice(workdir / "c.sock", case["coppice"]) if case.get("coppice") else None
        if self.server_coppice:
            self.server_coppice.take()
        self.herdr_lines()
        try:
            raw, secs = exchange(
                self.server.sock, request_bytes(case, root), shut=case.get("shut", False)
            )
        finally:
            sent = cop.close() if cop else []
        # The report goes out after the reply, and the herdr fake runs on
        # its own; give both a moment.
        time.sleep(0.15 if case.get("coppice") or case.get("herdr") else 0.0)
        obs: dict[str, Any] = {"reply": reply_obs(raw, root, srv)}
        obs.update(side_effects(workdir, srv))
        obs["coppice"] = norm_coppice(sent, root, srv)
        obs["herdr"] = [_norm_text(x, root, srv) for x in self.herdr_lines()]
        if self.server_coppice:
            obs["server_coppice"] = norm_coppice(self.server_coppice.take(), root, srv)
        return obs, secs

    def close(self) -> None:
        self.server.stop()
        if self.server_coppice:
            self.server_coppice.close()


# ---------------------------------------------------------------------------
# Building cases
# ---------------------------------------------------------------------------

PI_ARGV = ["--format", "pi", "--root", "{ROOT}/data/gate", "--verify-timeout", "4"]
OC_ARGV = ["--format", "opencode", "--root", "{ROOT}/data/gate", "--verify-timeout", "4"]


def client_argv(fmt: str = "claude", mode: str | None = "enforce", *extra: str) -> list[str]:
    """The argv gate_client.py sends: the hook command's own flags."""
    argv = []
    if mode is not None:
        argv += ["--mode", mode]
    return argv + ["--root", "{ROOT}/data/gate", "--format", fmt, "--verify-timeout", "5.0", *extra]


def state(
    envelopes: dict[str, Any] | None = None,
    config: str | None = None,
    sessions: dict[str, str] | None = None,
    disarmed: bool = False,
) -> dict[str, Any]:
    return {
        "disarmed": disarmed,
        "envelopes": {
            k: v if isinstance(v, str) else json.dumps(v, indent=2)
            for k, v in (BASE if envelopes is None else envelopes).items()
        },
        "config": config,
        "sessions": sessions or {},
    }


def req(
    name: str,
    argv: Any,
    stdin: str | None = None,
    *,
    group: str = "plain",
    st: dict[str, Any] | None = None,
    fields: dict[str, Any] | None = None,
    style: str = "py",
    coppice: str | None = None,
    herdr: bool = False,
    shut: bool = False,
    tags: list[str] | None = None,
    **extra: Any,
) -> dict[str, Any]:
    r: dict[str, Any] = {"argv": argv, "style": style}
    if stdin is not None:
        r["stdin"] = stdin
    if fields:
        r["fields"] = fields
    r.update({k: v for k, v in extra.items() if k in ("stdin_b64", "no_newline")})
    return {
        "kind": "request",
        "v": CASE_VERSION,
        "name": name,
        "group": group,
        "tags": tags or [],
        "request": r,
        "state": st or state(),
        "coppice": coppice,
        "herdr": herdr,
        "shut": shut,
    }


def raw(
    name: str, data: bytes, *, group: str = "plain", shut: bool = False, **kw: Any
) -> dict[str, Any]:
    c = {
        "kind": "request",
        "v": CASE_VERSION,
        "name": name,
        "group": group,
        "tags": ["malformed"],
        "raw_hex": data.hex(),
        "state": state(),
        "coppice": None,
        "herdr": False,
        "shut": shut,
    }
    c.update(kw)
    return c


def event(**kw: Any) -> str:
    ev: dict[str, Any] = {
        "v": 1,
        "ts": 1700000000.5,
        "session_id": "s1",
        "harness": "pi",
        "state": "idle",
        "source": "headless",
        "detail": "",
    }
    ev.update(kw)
    return json.dumps({k: v for k, v in ev.items() if v is not _DROP})


_DROP = object()
COP = {"coppice_sock": "{ROOT}/c.sock", "coppice_pane": "w1:p2"}
REPORT_ROOT = ["--root", "{ROOT}/data/gate"]


def build_cases() -> list[dict[str, Any]]:  # noqa: PLR0915 - a flat list of cases
    C: list[dict[str, Any]] = []
    add = C.append
    enf = state(config=ENFORCE)

    # -- pi -------------------------------------------------------------------
    pi_read = payload("read", {"path": "/work/a"})
    pi_rm = payload("bash", {"command": "rm -rf /"})
    add(req("pi read audit", PI_ARGV, pi_read, style="js", tags=["pi"]))
    add(req("pi read enforce", PI_ARGV, pi_read, style="js", st=enf, tags=["pi"]))
    add(req("pi rm enforce", PI_ARGV, pi_rm, style="js", st=enf, tags=["pi", "deny"]))
    add(req("pi rm audit", PI_ARGV, pi_rm, style="js", tags=["pi"]))
    add(
        req(
            "pi bash ls",
            PI_ARGV,
            payload("bash", {"command": "ls"}),
            style="js",
            st=enf,
            tags=["pi"],
        )
    )
    add(
        req(
            "pi no envelope",
            PI_ARGV,
            pi_read,
            style="js",
            st=state(envelopes={}, config=ENFORCE),
            tags=["pi", "deny"],
        )
    )
    add(
        req(
            "pi no session id",
            PI_ARGV,
            payload("read", {"path": "/work/a"}, sid=None),
            style="js",
            st=enf,
        )
    )
    add(
        req(
            "pi pane fields",
            PI_ARGV,
            pi_rm,
            style="js",
            st=enf,
            fields=COP,
            coppice="ok",
            tags=["pi", "coppice"],
        )
    )
    # SEC-3: the payload's session id never selects an envelope; only the pin does.
    loose = envelope(
        "env_loose", shell_allowlist=["rm", *envelope()["permissions"]["shell_allowlist"]]
    )
    sec3 = state(envelopes={"default": envelope(), "loose": loose}, config=ENFORCE)
    add(
        req(
            "sec3 unpinned payload names loose session",
            PI_ARGV,
            payload("bash", {"command": "rm x"}, sid="loose"),
            style="js",
            st=sec3,
            tags=["sec3", "deny"],
        )
    )
    add(
        req(
            "sec3 pinned to loose",
            [*PI_ARGV, "--session", "loose"],
            payload("bash", {"command": "rm x"}, sid="other"),
            style="js",
            st=sec3,
            tags=["sec3"],
        )
    )
    add(
        req(
            "sec3 pin with no envelope",
            [*PI_ARGV, "--session", "absent"],
            pi_read,
            style="js",
            st=sec3,
            tags=["sec3", "deny"],
        )
    )
    add(
        req(
            "sec3 client pinned claude",
            client_argv("claude", "enforce", "--session", "loose"),
            bash("rm x", sid="default"),
            st=sec3,
            tags=["sec3"],
        )
    )

    # -- OpenCode ---------------------------------------------------------------
    oc = payload("Bash", {"command": "ls"}, tool_use_id="call_1")
    add(req("opencode ls", OC_ARGV, oc, style="js", st=enf, tags=["opencode"]))
    add(
        req(
            "opencode rm",
            OC_ARGV,
            payload("Bash", {"command": "rm -rf /"}, tool_use_id="call_2"),
            style="js",
            st=enf,
            tags=["opencode", "deny"],
        )
    )
    add(
        req(
            "opencode edit",
            OC_ARGV,
            payload("Edit", {"filePath": "/work/a", "oldString": "a", "newString": "b"}),
            style="js",
            st=enf,
        )
    )
    add(
        req(
            "opencode mcp tool",
            OC_ARGV,
            payload("mcp__opencode__todowrite", {}),
            style="js",
            st=enf,
        )
    )
    add(
        req(
            "opencode write outside",
            OC_ARGV,
            payload("Write", {"filePath": "/etc/x", "content": "x"}),
            style="js",
            st=enf,
        )
    )

    # -- gate_client.py (the hook command's own argv) ---------------------------
    add(req("client claude allow", client_argv(), bash("ls"), tags=["client"]))
    add(req("client claude deny", client_argv(), bash("rm -rf /"), tags=["client", "deny"]))
    add(
        req(
            "client claude audit deny",
            client_argv(mode="audit"),
            bash("rm -rf /"),
            tags=["client"],
        )
    )
    add(req("client hermes deny", client_argv("hermes"), bash("rm -rf /"), tags=["client", "deny"]))
    add(req("client hermes allow", client_argv("hermes"), bash("ls"), tags=["client"]))
    add(
        req(
            "client openclaw deny",
            client_argv("openclaw"),
            bash("rm -rf /"),
            tags=["client", "deny"],
        )
    )
    add(req("client unknown format", client_argv("bogus"), bash("ls"), tags=["client", "deny"]))
    add(
        req(
            "client captures",
            client_argv("claude", "enforce", "--captures-root", "{ROOT}/captures"),
            bash("ls"),
        )
    )
    add(
        req(
            "client surrogate in reason",
            client_argv(),
            '{"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "rm -rf /\\udc80"}, "cwd": "/work"}',
            tags=["client", "deny", "unicode"],
        )
    )
    add(
        req(
            "client non ascii reason",
            client_argv(),
            bash("rm -rf /\u00e9\u4e2d"),
            tags=["client", "deny", "unicode"],
        )
    )
    add(
        req(
            "client disarmed",
            client_argv(),
            bash("rm -rf /"),
            st=state(disarmed=True),
            tags=["client"],
        )
    )
    # The envelope's deadline holds on the resident path too.
    for name, deadline in (
        ("client deadline passed", 1000000000),
        ("client deadline ahead", 4000000000),
    ):
        add(
            req(
                name,
                client_argv(),
                payload("Read", {"file_path": "/work/a"}),
                st=state(envelopes={"default": envelope(deadline=deadline)}),
                tags=["client", "deadline"],
            )
        )
    add(req("client empty stdin", client_argv(), "", tags=["client", "deny"]))
    add(req("client not json stdin", client_argv(), "{x", tags=["client", "deny"]))
    add(
        req(
            "client mode from config",
            client_argv(mode=None),
            bash("rm -rf /"),
            st=enf,
            tags=["client"],
        )
    )
    add(
        req(
            "client ask timeout",
            client_argv("claude", "enforce", "--ask", "--ask-timeout", "0"),
            bash("rm -rf /"),
        )
    )
    add(
        req(
            "client help",
            ["--help"],
            bash("ls"),
            tags=["help"],
        )
    )
    add(req("client -h", ["-h"], bash("ls"), tags=["help"]))
    add(req("client --he abbreviation", ["--he"], bash("ls"), tags=["help"]))
    add(req("client bad flag", [*client_argv(), "--bogus"], bash("ls"), tags=["argv", "deny"]))
    add(
        req(
            "client bad flag hermes",
            ["--format", "hermes", "--bogus"],
            bash("ls"),
            tags=["argv", "deny"],
        )
    )
    add(
        req(
            "client bad mode",
            ["--mode", "loud", "--root", "{ROOT}/data/gate"],
            bash("ls"),
            tags=["argv"],
        )
    )
    add(
        req(
            "client argv surrogate",
            [*client_argv(), "--session", "a\udc80b"],
            bash("ls"),
            tags=["argv", "unicode"],
        )
    )
    add(req("client argv nul", [*client_argv(), "--session", "a\x00b"], bash("ls"), tags=["argv"]))
    add(req("client argv empty", [], bash("ls"), tags=["argv"]))

    # -- the data-dir guard -------------------------------------------------------
    grep_c = payload("Grep", {"pattern": ".", "path": "/srv/cdata/web"})
    add(
        req(
            "data dir named",
            client_argv(),
            grep_c,
            fields={"coppice_data_dir": "/srv/cdata"},
            tags=["data_dir", "deny"],
        )
    )
    add(req("data dir unnamed", client_argv(), grep_c, tags=["data_dir"]))
    add(
        req(
            "data dir relative",
            client_argv(),
            payload("Grep", {"pattern": ".", "path": "/work/cdata"}),
            fields={"coppice_data_dir": "cdata"},
            tags=["data_dir"],
        )
    )
    add(
        req(
            "data dir not a string",
            client_argv(),
            grep_c,
            fields={"coppice_data_dir": 7},
            tags=["data_dir"],
        )
    )
    add(
        req(
            "data dir respelling",
            client_argv(),
            bash("cd /srv/cdata/web && cat ./token"),
            fields={"coppice_data_dir": "/srv/cdata/"},
            tags=["data_dir", "deny"],
        )
    )
    # On a resident call the caller's data dir is its pane's
    # COPPICE_DATA_DIR, in any parameter expansion.
    for name, line in (
        ("var chat", "cat $COPPICE_DATA_DIR/chat/*"),
        ("braced default chat", "cat ${COPPICE_DATA_DIR:-x}/chat/*"),
        ("var cp journal", "cp -r $COPPICE_DATA_DIR/journal /work/x"),
        ("var web", "cat $COPPICE_DATA_DIR/web/*"),
    ):
        add(
            req(
                f"data dir {name}",
                client_argv(),
                bash(line),
                fields={"coppice_data_dir": "/srv/cdata"},
                tags=["data_dir", "deny"],
            )
        )

    # -- reports: coppice and herdr -------------------------------------------
    rd = payload("Read", {"file_path": "/work/a"})
    add(req("coppice allow", client_argv(), rd, fields=COP, coppice="ok", tags=["coppice"]))
    add(
        req(
            "coppice deny",
            client_argv(),
            bash("rm -rf /"),
            fields=COP,
            coppice="ok",
            tags=["coppice"],
        )
    )
    add(
        req(
            "coppice placed elsewhere",
            client_argv(),
            rd,
            fields=COP,
            coppice="other",
            tags=["coppice"],
        )
    )
    add(req("coppice bare fake", client_argv(), rd, fields=COP, coppice="bare", tags=["coppice"]))
    add(
        req(
            "coppice pane bad shape",
            client_argv(),
            rd,
            fields={"coppice_sock": "{ROOT}/c.sock", "coppice_pane": "pane-7"},
            coppice="ok",
            tags=["coppice"],
        )
    )
    add(
        req(
            "coppice sock relative",
            client_argv(),
            rd,
            fields={"coppice_sock": "c.sock", "coppice_pane": "w1:p2"},
            coppice="ok",
            tags=["coppice"],
        )
    )
    add(
        req(
            "coppice sock only",
            client_argv(),
            rd,
            fields={"coppice_sock": "{ROOT}/c.sock"},
            coppice="ok",
        )
    )
    add(req("coppice pane only", client_argv(), rd, fields={"coppice_pane": "w1:p2"}, coppice="ok"))
    add(
        req(
            "coppice sock missing",
            client_argv(),
            rd,
            fields={"coppice_sock": "{ROOT}/none.sock", "coppice_pane": "w1:p2"},
        )
    )
    add(
        req(
            "coppice transcript kept out",
            client_argv(),
            payload(
                "Read", {"file_path": "/work/a"}, transcript_path="/work/t.jsonl", session_id="s1"
            ),
            fields=COP,
            coppice="ok",
            tags=["coppice"],
        )
    )
    add(
        req(
            "coppice fields not strings",
            client_argv(),
            rd,
            fields={"coppice_sock": 1, "coppice_pane": ["w1:p2"], "herdr_pane": None},
            coppice="ok",
        )
    )
    add(
        req(
            "herdr pane", client_argv(), rd, fields={"herdr_pane": "h1"}, herdr=True, tags=["herdr"]
        )
    )
    add(
        req(
            "herdr pane dash",
            client_argv(),
            rd,
            fields={"herdr_pane": "-h1"},
            herdr=True,
            tags=["herdr"],
        )
    )
    add(
        req(
            "herdr and coppice",
            client_argv(),
            rd,
            fields={**COP, "herdr_pane": "h1"},
            coppice="ok",
            herdr=True,
        )
    )
    add(
        req(
            "herdr when coppice pane bad",
            client_argv(),
            rd,
            fields={"coppice_sock": "{ROOT}/c.sock", "coppice_pane": "x", "herdr_pane": "h1"},
            coppice="ok",
            herdr=True,
        )
    )
    add(
        req(
            "coppice ask blocked",
            client_argv("claude", "enforce", "--ask", "--ask-timeout", "0"),
            bash("rm -rf /"),
            fields=COP,
            coppice="ok",
            tags=["coppice", "ask"],
        )
    )

    # -- hook report ------------------------------------------------------------
    hr = ["hook", "report"]
    add(req("report pi idle", hr, event(), style="js", tags=["report"]))
    add(req("report pi idle rooted", [*hr, *REPORT_ROOT], event(), style="js", tags=["report"]))
    add(
        req(
            "report pi coppice",
            [*hr, *REPORT_ROOT],
            event(),
            style="js",
            fields=COP,
            coppice="ok",
            tags=["report"],
        )
    )
    add(
        req(
            "report opencode pane",
            [*hr, "--pane", "w1:p2", *REPORT_ROOT],
            event(harness="opencode", harness_session_id="ses_1", state="working"),
            style="js",
            fields=COP,
            coppice="ok",
            shut=True,
            tags=["report"],
        )
    )
    add(
        req(
            "report opencode blocked ask",
            [*hr, "--pane", "w1:p2", *REPORT_ROOT],
            event(
                harness="opencode",
                state="blocked",
                detail="bash: rm",
                ask={"id": "p1", "tool": "bash", "summary": "rm", "deadline": 1700000090.25},
            ),
            style="js",
            fields=COP,
            coppice="ok",
            shut=True,
        )
    )
    add(req("report placed elsewhere", [*hr, *REPORT_ROOT], event(), fields=COP, coppice="other"))
    add(
        req(
            "report herdr",
            [*hr, *REPORT_ROOT],
            event(state="working"),
            fields={"herdr_pane": "h1"},
            herdr=True,
        )
    )
    add(
        req(
            "report herdr unknown state",
            [*hr, *REPORT_ROOT],
            event(state="unknown"),
            fields={"herdr_pane": "h1"},
            herdr=True,
        )
    )
    add(
        req(
            "report source gate downgraded",
            [*hr, *REPORT_ROOT],
            event(source="gate"),
            fields=COP,
            coppice="ok",
        )
    )
    add(
        req(
            "report pane flag overrides",
            [*hr, "--pane", "w3:p4", *REPORT_ROOT],
            event(pane="w1:p1"),
        )
    )
    add(req("report pane flag equals", [*hr, "--pane=w3:p4", *REPORT_ROOT], event()))
    add(req("report abbreviations", [*hr, "--pa", "w3:p4", "--ro", "{ROOT}/data/gate"], event()))
    add(req("report repeated root", [*hr, "--root", "{ROOT}/x/gate", *REPORT_ROOT], event()))
    add(req("report root trailing slash", [*hr, "--root", "{ROOT}/data/gate/"], event()))
    add(req("report pane negative number", [*hr, "--pane", "-1", *REPORT_ROOT], event()))
    add(
        req(
            "report existing tree",
            [*hr, *REPORT_ROOT],
            event(session_id="s2"),
            st=state(
                sessions={
                    "s2": '{"type": "session", "id": "s2", "ts": 1.0, "v": 1}\n'
                    '{"type": "tool_call", "id": "0a0b0c0d", "parentId": null, "ts": 2.0}\n'
                }
            ),
        )
    )
    add(req("report unsafe session id", [*hr, *REPORT_ROOT], event(session_id="../a b\u00e9")))
    add(
        req(
            "report meta keys dropped",
            [*hr, *REPORT_ROOT],
            event(type="x", id="y", parentId="z", extra=[1, 2.5, None]),
        )
    )
    add(
        req(
            "report non ascii detail",
            [*hr, *REPORT_ROOT],
            event(detail="caf\u00e9 \u4e2d"),
            fields=COP,
            coppice="ok",
        )
    )
    add(
        req(
            "report surrogate detail",
            [*hr, *REPORT_ROOT],
            json.dumps(json.loads(event()))[:-1] + ', "note": "\\udc80"}',
        )
    )
    for name, ev in (
        ("missing ts", event(ts=_DROP)),
        ("missing session", event(session_id=_DROP)),
        ("ts string", event(ts="1")),
        ("ts bool", event(ts=True)),
        ("state bad", event(state="asleep")),
        ("source bad", event(source="me")),
        ("done from gate", event(state="done", source="manifest")),
        ("detail long", event(detail="x" * 201)),
        ("detail 200", event(detail="\u00e9" * 200)),
        ("detail number", event(detail=5)),
        ("harness session number", event(harness_session_id=3)),
        ("pane number", event(pane=3)),
        ("ask on idle", event(ask={"id": "a", "tool": "t", "summary": "s", "deadline": 1})),
        ("ask not object", event(state="blocked", ask=[1])),
        (
            "ask missing id",
            event(state="blocked", ask={"tool": "t", "summary": "s", "deadline": 1}),
        ),
        (
            "ask deadline bool",
            event(state="blocked", ask={"id": "a", "tool": "t", "summary": "s", "deadline": False}),
        ),
        (
            "ask tier number",
            event(
                state="blocked",
                ask={"id": "a", "tool": "t", "summary": "s", "deadline": 1, "tier": 2},
            ),
        ),
        ("gate blocked no ask", event(state="blocked", source="gate")),
        ("v two", event(v=2)),
        ("v string", event(v="1")),
        ("v float", event(v=1.0)),
        ("v list", event(v=[1, "a", None, {"k": 1.5}])),
        ("state number", event(state=1)),
        ("not object", "[1, 2]"),
        ("bad json", "{x"),
        ("empty", ""),
        ("nan ts", event()[:-1] + ', "n": NaN}'),
    ):
        add(req(f"report {name}", [*hr, *REPORT_ROOT], ev, tags=["report", "invalid"]))
    add(
        req(
            "report bad utf8",
            [*hr, *REPORT_ROOT],
            None,
            stdin_b64=base64.b64encode(b"\xff{}").decode(),
        )
    )
    add(req("report bad flag", [*hr, "--x"], event(), tags=["report", "argv"]))
    add(req("report help flag", [*hr, "-h"], event(), tags=["report", "argv"]))
    add(req("report pane no value", [*hr, "--pane"], event(), tags=["report", "argv"]))
    add(
        req(
            "report pane then flag",
            [*hr, "--pane", "--root", "x"],
            event(),
            tags=["report", "argv"],
        )
    )
    add(req("report positional", [*hr, "extra"], event(), tags=["report", "argv"]))
    add(req("report ambiguous prefix", [*hr, "--", "--pane"], event(), tags=["report", "argv"]))

    # -- the server's own environment ---------------------------------------------
    pe = "paneenv"
    add(req("paneenv deny no fields", client_argv(), bash("rm -rf /"), group=pe, tags=["paneenv"]))
    add(
        req(
            "paneenv server data dir",
            client_argv(),
            payload("Grep", {"pattern": ".", "path": "/srv/sdata/web"}),
            group=pe,
            tags=["paneenv", "data_dir"],
        )
    )
    add(
        req(
            "paneenv both data dirs",
            client_argv(),
            payload("Grep", {"pattern": ".", "path": "/srv/cdata/web"}),
            group=pe,
            fields={"coppice_data_dir": "/srv/cdata"},
            tags=["paneenv", "data_dir"],
        )
    )
    add(
        req(
            "paneenv caller fields",
            client_argv(),
            rd,
            group=pe,
            fields=COP,
            coppice="ok",
            herdr=True,
        )
    )
    add(req("paneenv report no fields", [*hr, *REPORT_ROOT], event(), group=pe, herdr=True))
    add(req("paneenv tmux pane", client_argv(), rd, group=pe, fields=COP, coppice="ok"))

    # -- malformed requests -------------------------------------------------------
    good = json.dumps(
        {"v": 1, "argv": client_argv(), "stdin_b64": base64.b64encode(bash("ls").encode()).decode()}
    )
    add(raw("not json", b"hello\n"))
    add(raw("empty line", b"\n"))
    add(raw("json list", b"[1, 2]\n"))
    add(raw("json string", b'"argv"\n'))
    add(raw("json null", b"null\n"))
    add(raw("missing argv", b'{"v": 1, "stdin_b64": ""}\n'))
    add(raw("argv number", b'{"v": 1, "argv": 5}\n'))
    add(raw("argv null", b'{"v": 1, "argv": null}\n'))
    add(raw("argv true", b'{"v": 1, "argv": true}\n'))
    add(raw("argv string", b'{"v": 1, "argv": "ab"}\n'))
    add(raw("argv string help", b'{"v": 1, "argv": "-h"}\n'))
    add(raw("argv object", b'{"v": 1, "argv": {"--format": 1, "hermes": 2}}\n'))
    add(raw("argv object help", b'{"v": 1, "argv": {"--help": 1}}\n'))
    add(
        raw(
            "argv mixed types",
            b'{"v": 1, "argv": ["--bogus", true, false, null, 1, -0.0, 1e100, 1.5, 12345678901234567890, [1, "a\'b", {"k": [null]}], {"x": "y"}, "\xc3\xa9"]}\n',
        )
    )
    add(
        raw(
            "argv numbers as flags",
            b'{"v": 1, "argv": ["--verify-timeout", 4, "--mode", "enforce", "--root", "{ROOT}/data/gate", "--format", "pi"], "stdin_b64": "e30="}\n',
        )
    )
    add(
        raw(
            "argv nan",
            b'{"v": 1, "argv": ["--verify-timeout", NaN, "--root", "{ROOT}/data/gate"], "stdin_b64": "e30="}\n',
        )
    )
    add(
        raw(
            "argv infinity",
            b'{"v": 1, "argv": ["--verify-timeout", Infinity, "--mode", "enforce", "--root", "{ROOT}/data/gate"], "stdin_b64": "e30="}\n',
        )
    )
    add(
        raw(
            "argv hook report string", b'{"v": 1, "argv": ["hook", "report", 7], "stdin_b64": ""}\n'
        )
    )
    add(raw("stdin number", b'{"v": 1, "argv": [], "stdin_b64": 5}\n'))
    add(raw("stdin null", b'{"v": 1, "argv": [], "stdin_b64": null}\n'))
    add(raw("stdin list", b'{"v": 1, "argv": [], "stdin_b64": []}\n'))
    add(raw("stdin non ascii", '{"v": 1, "argv": [], "stdin_b64": "\u00e9"}\n'.encode()))
    add(raw("stdin non ascii escaped", b'{"v": 1, "argv": [], "stdin_b64": "\\u00e9"}\n'))
    add(raw("stdin bad padding", b'{"v": 1, "argv": [], "stdin_b64": "abc"}\n'))
    add(raw("stdin one extra char", b'{"v": 1, "argv": [], "stdin_b64": "abcde"}\n'))
    add(raw("stdin one char", b'{"v": 1, "argv": [], "stdin_b64": "a"}\n'))
    for i, b64 in enumerate(
        (
            "e30=!!garbage",
            "e3 0=",
            "e\n3\t0",
            "e30",
            "e3==0",
            "=e30=",
            "e30===",
            "e=30",
            "ab=c=d",
            "ab==cd==",
            "YQ",
            "YQ=",
            "YQ==",
            "YQ===",
            "Y=Q==",
            "-_+/",
            "\u00ff",
            "",
            "====",
        )
    ):
        body = json.dumps({"v": 1, "argv": client_argv("hermes"), "stdin_b64": b64})
        add(raw(f"stdin base64 lenient {i}", body.encode() + b"\n"))
    add(raw("no stdin field", json.dumps({"v": 1, "argv": client_argv()}).encode() + b"\n"))
    add(raw("utf8 bom", b"\xef\xbb\xbf" + good.encode() + b"\n"))
    add(raw("invalid utf8", b'{"v": 1, "argv": [], "x": "\xff"}\n'))
    add(raw("encoded surrogate", b'{"v": 1, "argv": ["--session", "\xed\xb2\x80"], "x": 1}\n'))
    add(
        raw(
            "utf16 half close",
            ("\ufeff" + good).encode("utf-16-le"),
            shut=True,
            port_expect_note="G2-2",
        )
    )
    add(raw("utf16 newline", ("\ufeff" + good + "\n").encode("utf-16-le")))
    add(raw("trailing data ignored", good.encode() + b"\ntrailing garbage"))
    add(raw("leading whitespace", b"  \t" + good.encode() + b"  \n"))
    add(raw("crlf", good.encode() + b"\r\n"))
    add(
        raw(
            "duplicate argv last wins",
            b'{"argv": ["--bogus"], "argv": '
            + json.dumps(client_argv()).encode()
            + b', "stdin_b64": "e30="}\n',
        )
    )
    add(raw("nan field", good[:-1].encode() + b', "x": NaN}\n'))
    add(raw("big int field", good[:-1].encode() + b', "x": ' + b"9" * 4000 + b"}\n"))
    add(raw("int past digit limit", good[:-1].encode() + b', "x": ' + b"9" * 4301 + b"}\n"))
    add(raw("control char in string", b'{"argv": [], "x": "a\x01b"}\n'))
    add(raw("lone surrogate escape", good[:-1].encode() + b', "x": "\\ud800"}\n'))
    add(raw("half close partial", b'{"v": 1, "argv": ["--form', shut=True))
    add(raw("half close complete", good.encode(), shut=True))
    # The nesting depth the oracle's JSON reader accepts on the server's
    # request thread, found by bisection: 9,992 containers in all.
    deep_ok = 9991
    add(
        raw(
            "deep nesting at limit",
            good[:-1].encode() + b', "x": ' + b"[" * deep_ok + b"]" * deep_ok + b"}\n",
        )
    )
    add(
        raw(
            "deep nesting past limit",
            good[:-1].encode() + b', "x": ' + b"[" * (deep_ok + 1) + b"]" * (deep_ok + 1) + b"}\n",
        )
    )
    add(
        raw(
            "deep argv word",
            b'{"argv": ["--root", "{ROOT}/data/gate", '
            + b"[" * (deep_ok - 1)
            + b"]" * (deep_ok - 1)
            + b"]}\n",
        )
    )
    # readline(4 MiB): a line is cut at 4 MiB. A document that ends just
    # inside the cut is read; one that ends past it is not.
    # No {ROOT} here: the line's length is what is tested.
    pad_to = BAD_REQUEST_LIMIT
    fixed = json.dumps(
        {"v": 1, "argv": ["--mode", "enforce", "--root", "/nonexistent/gate"], "stdin_b64": "e30="}
    )
    head = fixed[:-1].encode() + b', "pad": "'
    fill = pad_to - len(head) - len(b'"}')
    add(raw("line exactly at limit", head + b"a" * fill + b'"}' + b"\n"))
    add(raw("line past limit", head + b"a" * (fill + 1) + b'"}' + b"\n"))
    add(raw("line past limit no newline", head + b"a" * (fill + 1) + b'"}', shut=True))
    return C


# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------


def _group_env(d: Path, home: str = FAKE_HOME) -> dict[str, str]:
    return {
        "HOME": home,
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
    }


def _good_request(root: Path, cmd: str, fmt: str = "claude") -> bytes:
    body = {
        "v": 1,
        "argv": [
            "--mode",
            "enforce",
            "--root",
            str(root / "data" / "gate"),
            "--format",
            fmt,
            "--verify-timeout",
            "5.0",
        ],
        "stdin_b64": base64.b64encode(bash(cmd).encode()).decode(),
    }
    return json.dumps(body).encode() + b"\n"


def _lay_out_base(root: Path) -> None:
    prepare({"state": state()}, root)


def scenario_concurrent(cmd: list[str], d: Path) -> dict[str, Any]:
    """Twelve clients at once, each against its own gate root; one slow
    client holds a connection open the whole time without sending."""
    server = Server(cmd, d / "gate", _group_env(d), d)
    try:
        slow = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        slow.connect(str(server.sock))
        slow.sendall(b'{"v": 1, "argv": [')
        cmds = ["ls", "rm -rf /", "cat /work/a", "curl http://x", "git status", "sudo ls"] * 2
        fmts = ["claude", "pi", "opencode", "hermes"]
        roots, reqs = [], []
        for i, c in enumerate(cmds):
            r = d / f"c{i:02d}"
            _lay_out_base(r)
            roots.append(r)
            reqs.append(_good_request(r, c, fmts[i % len(fmts)]))
        replies: list[bytes | None] = [None] * len(reqs)
        barrier = threading.Barrier(len(reqs))

        def one(i: int) -> None:
            barrier.wait()
            replies[i], _ = exchange(server.sock, reqs[i], timeout=90)

        threads = [threading.Thread(target=one, args=(i,)) for i in range(len(reqs))]
        for t in threads:
            t.start()
        for t in threads:
            t.join(120)
        # The slow client ends its line; its request is malformed.
        slow.settimeout(30)
        slow.sendall(b"\n")
        slow_reply = b""
        while True:
            chunk = slow.recv(65536)
            if not chunk:
                break
            slow_reply += chunk
        slow.close()
        out = []
        for i, r in enumerate(roots):
            fx = side_effects(r, str(d))
            out.append(
                {
                    "reply": reply_obs(replies[i] or b"", str(r), str(d)),
                    "audit": fx["audit"],
                    "tree": fx["tree"],
                }
            )
        return {"clients": out, "slow": reply_obs(slow_reply, "", str(d))}
    finally:
        server.stop()


def scenario_mid_request(cmd: list[str], d: Path) -> dict[str, Any]:
    """Clients that go away: one closes mid-line, one half-closes mid-line,
    one connects and closes at once, one closes before reading its reply.
    The server keeps answering after each."""
    server = Server(cmd, d / "gate", _group_env(d), d)
    try:
        r = d / "r"
        _lay_out_base(r)
        obs: dict[str, Any] = {}
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(str(server.sock))
        s.sendall(b'{"v": 1, "argv": ["--mode", "enf')
        s.close()
        obs["after close mid line"] = reply_obs(
            exchange(server.sock, _good_request(r, "ls"))[0], str(r), str(d)
        )
        reply, _ = exchange(server.sock, b'{"v": 1, "argv": ["--mode", "enf', shut=True)
        obs["half close mid line"] = reply_obs(reply, str(r), str(d))
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(str(server.sock))
        s.close()
        obs["after empty connection"] = reply_obs(
            exchange(server.sock, _good_request(r, "rm -rf /"))[0], str(r), str(d)
        )
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(str(server.sock))
        s.sendall(_good_request(r, "ls"))
        s.close()
        time.sleep(0.5)
        obs["after unread reply"] = reply_obs(
            exchange(server.sock, _good_request(r, "ls"))[0], str(r), str(d)
        )
        reply, _ = exchange(server.sock, b"", shut=True)
        obs["connect then shut"] = reply_obs(reply, str(r), str(d))
        # A --help request ends its connection with no reply; the server goes on.
        help_req = json.dumps({"v": 1, "argv": ["--help"]}).encode() + b"\n"
        obs["help"] = reply_obs(exchange(server.sock, help_req)[0], str(r), str(d))
        obs["after help"] = reply_obs(
            exchange(server.sock, _good_request(r, "ls"))[0], str(r), str(d)
        )
        fx = side_effects(r, str(d))
        obs["audit_count"] = len(fx["audit"])
        obs["alive"] = server.proc.poll() is None
        return obs
    finally:
        server.stop()


def _mode(p: Path) -> str | None:
    try:
        return oct(stat.S_IMODE(os.lstat(p).st_mode))
    except OSError:
        return None


def _kind(p: Path) -> str:
    try:
        m = os.lstat(p).st_mode
    except OSError:
        return "absent"
    if stat.S_ISSOCK(m):
        return "socket"
    if stat.S_ISREG(m):
        return "file"
    if stat.S_ISDIR(m):
        return "dir"
    if stat.S_ISLNK(m):
        return "symlink"
    return "other"


def scenario_lifecycle(cmd: list[str], d: Path) -> dict[str, Any]:
    """Start (parents made, the modes), a request, stop on SIGINT."""
    root = d / "a" / "b" / "gate"
    server = Server(cmd, root, _group_env(d), d)
    obs: dict[str, Any] = {
        "root_mode": _mode(root),
        "sock_mode": _mode(root / "gate.sock"),
        "sock_kind": _kind(root / "gate.sock"),
    }
    r = d / "r"
    _lay_out_base(r)
    obs["reply"] = reply_obs(exchange(server.sock, _good_request(r, "ls"))[0], str(r), str(d))
    code = server.stop(signal.SIGINT)
    obs["exit"] = code
    # The first line, and the newline Ctrl-C prints last. What comes
    # between is each server's own log (a traceback in Python's case for a
    # client that went away before its reply).
    err = _norm_text(server.stderr(), "", str(d))
    obs["stderr_first"] = err.split("\n", 1)[0]
    obs["stderr_ends_blank"] = err.endswith("\n\n")
    obs["sock_after_stop"] = _kind(root / "gate.sock")
    return obs


def scenario_existing_root(cmd: list[str], d: Path) -> dict[str, Any]:
    """A root that already exists with a loose mode is made 0700."""
    root = d / "gate"
    root.mkdir(parents=True)
    root.chmod(0o755)
    server = Server(cmd, root, _group_env(d), d)
    obs = {"root_mode": _mode(root), "sock_mode": _mode(root / "gate.sock")}
    server.stop()
    return obs


def scenario_stale(cmd: list[str], d: Path) -> dict[str, Any]:
    """A stale socket from a dead server, then a stale regular file: the
    server removes each and serves."""
    obs: dict[str, Any] = {}
    root = d / "gate"
    root.mkdir(parents=True)
    dead = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    dead.bind(str(root / "gate.sock"))
    dead.close()
    obs["before"] = _kind(root / "gate.sock")
    r = d / "r"
    _lay_out_base(r)
    server = Server(cmd, root, _group_env(d), d, name="one")
    obs["stale socket reply"] = reply_obs(
        exchange(server.sock, _good_request(r, "ls"))[0], str(r), str(d)
    )
    server.stop(signal.SIGKILL)
    obs["after kill"] = _kind(root / "gate.sock")
    (root / "gate.sock").unlink()
    (root / "gate.sock").write_text("stale\n")
    server = Server(cmd, root, _group_env(d), d, name="two")
    obs["stale file reply"] = reply_obs(
        exchange(server.sock, _good_request(r, "rm -rf /"))[0], str(r), str(d)
    )
    obs["sock_mode"] = _mode(root / "gate.sock")
    server.stop()
    return obs


def scenario_second_server(cmd: list[str], d: Path) -> dict[str, Any]:
    """A second server on a root a live server answers on refuses and
    exits 1; the first keeps the path. Which one answers shows in where a
    `hook report` with no --root writes: each server has its own HOME."""
    obs: dict[str, Any] = {}
    root = d / "gate"
    home1, home2 = d / "home1", d / "home2"
    home1.mkdir(parents=True)
    home2.mkdir(parents=True)
    first = Server(cmd, root, _group_env(d, str(home1)), d, name="first")
    inode1 = os.lstat(first.sock).st_ino
    second = subprocess.run(  # noqa: S603 - the server under test
        cmd + ["--root", str(root)],
        env=_group_env(d, str(home2)),
        capture_output=True,
        timeout=60,
        check=False,
        preexec_fn=_default_sigint,  # noqa: PLW1509
    )
    obs["second_exit"] = second.returncode
    obs["second_stderr"] = _norm_text(second.stderr.decode("utf-8", "replace"), "", str(d))
    obs["same_socket"] = os.lstat(root / "gate.sock").st_ino == inode1
    ev = event(session_id="who")
    rep = json.dumps(
        {"v": 1, "argv": ["hook", "report"], "stdin_b64": base64.b64encode(ev.encode()).decode()}
    )
    obs["reply"] = reply_obs(exchange(root / "gate.sock", rep.encode() + b"\n")[0], "", str(d))
    obs["home1_tree"] = sorted(str(p.relative_to(home1)) for p in home1.rglob("*.jsonl"))
    obs["home2_tree"] = sorted(str(p.relative_to(home2)) for p in home2.rglob("*.jsonl"))
    obs["first_alive"] = first.proc.poll() is None
    first.stop()
    obs["sock_after_first_stop"] = _kind(root / "gate.sock")
    return obs


def scenario_sigterm(cmd: list[str], d: Path) -> dict[str, Any]:
    """SIGTERM ends the server as Ctrl-C does, with no newline, and the
    socket goes with it."""
    root = d / "gate"
    server = Server(cmd, root, _group_env(d), d)
    obs: dict[str, Any] = {"exit": server.stop(signal.SIGTERM)}
    err = _norm_text(server.stderr(), "", str(d))
    obs["stderr"] = err
    obs["sock_after_stop"] = _kind(root / "gate.sock")
    return obs


def _ignore_sigterm() -> None:
    _default_sigint()
    signal.signal(signal.SIGTERM, signal.SIG_IGN)


def scenario_sigterm_ignored(cmd: list[str], d: Path) -> dict[str, Any]:
    """A SIGTERM the server was started with ignored still ends it: a Go
    program cannot see that it was ignored, so no server keeps it so."""
    root = d / "gate"
    logs = d / "logs"
    logs.mkdir(parents=True)
    proc = subprocess.Popen(  # noqa: S603 - the server under test
        cmd + ["--root", str(root)],
        env=_group_env(d),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=(logs / "err").open("wb"),
        preexec_fn=_ignore_sigterm,  # noqa: PLW1509 - no threads touch this
    )
    t0 = time.monotonic()
    while not sock_ready(root / "gate.sock") and time.monotonic() - t0 < 60:
        time.sleep(0.02)
    proc.send_signal(signal.SIGTERM)
    time.sleep(0.5)
    obs: dict[str, Any] = {"alive_after_sigterm": proc.poll() is None}
    proc.send_signal(signal.SIGINT)
    try:
        obs["exit"] = proc.wait(15)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(5)
        obs["exit"] = None
    obs["sock_after_stop"] = _kind(root / "gate.sock")
    return obs


def scenario_bad_root(cmd: list[str], d: Path) -> dict[str, Any]:
    """A root that is a file: the server cannot start. Its first line is
    still the serving line; the rest is each language's own error."""
    d.mkdir(parents=True, exist_ok=True)
    (d / "gate").write_text("x")
    proc = subprocess.run(  # noqa: S603 - the server under test
        cmd + ["--root", str(d / "gate")],
        env=_group_env(d),
        capture_output=True,
        timeout=60,
        check=False,
        preexec_fn=_default_sigint,  # noqa: PLW1509
    )
    err = proc.stderr.decode("utf-8", "replace")
    return {"exit": proc.returncode, "first_line": _norm_text(err.split("\n", 1)[0], "", str(d))}


def scenario_sock_is_dir(cmd: list[str], d: Path) -> dict[str, Any]:
    """A directory where the socket goes: the server cannot start."""
    (d / "gate" / "gate.sock").mkdir(parents=True)
    proc = subprocess.run(  # noqa: S603 - the server under test
        cmd + ["--root", str(d / "gate")],
        env=_group_env(d),
        capture_output=True,
        timeout=60,
        check=False,
        preexec_fn=_default_sigint,  # noqa: PLW1509
    )
    err = proc.stderr.decode("utf-8", "replace")
    return {
        "exit": proc.returncode,
        "first_line": _norm_text(err.split("\n", 1)[0], "", str(d)),
        "sock": _kind(d / "gate" / "gate.sock"),
    }


SCENARIOS: dict[str, Callable[[list[str], Path], dict[str, Any]]] = {
    "concurrent": scenario_concurrent,
    "mid request": scenario_mid_request,
    "lifecycle": scenario_lifecycle,
    "existing root": scenario_existing_root,
    "stale": scenario_stale,
    "second server": scenario_second_server,
    "sigterm": scenario_sigterm,
    "sigterm ignored": scenario_sigterm_ignored,
    "bad root": scenario_bad_root,
    "sock is dir": scenario_sock_is_dir,
}


def scenario_cases() -> list[dict[str, Any]]:
    return [
        {
            "kind": "scenario",
            "v": CASE_VERSION,
            "name": f"scenario {n}",
            "scenario": n,
            "tags": ["scenario"],
        }
        for n in SCENARIOS
    ]


# ---------------------------------------------------------------------------
# Running every case against one server command
# ---------------------------------------------------------------------------


def run_all(
    cases: list[dict[str, Any]],
    cmd: list[str],
    base: Path,
    progress: Callable[[int, dict[str, Any], dict[str, Any]], None] | None = None,
) -> tuple[list[dict[str, Any]], list[float]]:
    """(observation per case, in order; seconds per gate decision)."""
    if base.exists():
        shutil.rmtree(base)
    base.mkdir(parents=True)
    obs: list[dict[str, Any] | None] = [None] * len(cases)
    times: list[float] = []
    groups: dict[str, Group] = {}
    try:
        for i, c in enumerate(cases):
            if c["kind"] != "request":
                continue
            g = groups.get(c["group"])
            if g is None:
                g = groups[c["group"]] = Group(cmd, c["group"], base)
            o, secs = g.run(c, base / "cases" / f"{i:04d}")
            argv = (c.get("request") or {}).get("argv")
            if isinstance(argv, list) and argv[:2] != ["hook", "report"] and o["reply"]:
                times.append(secs)
            obs[i] = o
            if progress:
                progress(i, c, o)
    finally:
        for g in groups.values():
            g.close()
    for i, c in enumerate(cases):
        if c["kind"] == "scenario":
            d = base / "scenarios" / f"{i:04d}"
            if d.exists():
                shutil.rmtree(d)
            d.mkdir(parents=True)
            obs[i] = SCENARIOS[c["scenario"]](cmd, d)
            if progress:
                progress(i, c, obs[i])  # type: ignore[arg-type]
    return obs, times  # type: ignore[return-value]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    cases = build_cases() + scenario_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")

    def show(i: int, c: dict[str, Any], o: dict[str, Any]) -> None:
        brief = o.get("reply", "")[:90] if c["kind"] == "request" else "scenario"
        print(f"{i + 1:4d}/{len(cases)} {c['name']}: {brief}", flush=True)

    obs, times = run_all(cases, oracle_cmd(), SCRATCH / "gen", show)
    out_lines = []
    for c, o in zip(cases, obs, strict=True):
        body = dict(c)
        body["expect"] = o
        pe = port_expect_for(body)
        if pe:
            body["port_expect"] = pe
        re_ = rust_expect_for(body)
        if re_:
            body["rust_expect"] = re_
        body.pop("port_expect_note", None)
        body["id"] = case_id(body)
        out_lines.append(body)
    out_lines.sort(key=lambda c: c["id"])
    args.out.mkdir(parents=True, exist_ok=True)
    path = args.out / "cases.jsonl"
    data = "".join(canonical_json(c) + "\n" for c in out_lines).encode("utf-8")
    path.write_bytes(data)
    manifest = {
        "v": CASE_VERSION,
        "count": len(out_lines),
        "sha256": hashlib.sha256(data).hexdigest(),
    }
    (args.out / "cases.manifest.json").write_text(
        json.dumps(manifest, indent=1) + "\n", encoding="utf-8"
    )
    times.sort()
    if times:
        print(f"oracle decision p50 {times[len(times) // 2] * 1000:.1f} ms over {len(times)}")
    print(f"wrote {len(out_lines)} cases to {path}")
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    return 0


BAD_REQUEST_REPLY = '{"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED \\u2014 bad request", "exit_code": 2}\n'


RUST_UNDECIDED_TIMEOUT = (
    '{"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED \\u2014 gate internal error (denied '
    "fail-closed): the Rust gate cannot decide this call: a --verify-timeout that is not finite, or "
    'under one second", "exit_code": 2}\n'
)


def rust_expect_for(case: dict[str, Any]) -> dict[str, Any] | None:
    """The Rust port's own rulings (RG-12: a --verify-timeout that is not
    finite, or under one second, is not decided, so it is denied)."""
    if case["name"] in ("argv nan", "argv infinity"):
        return {
            "reply": RUST_UNDECIDED_TIMEOUT,
            "audit": [],
            "tree": {},
            "modes": {},
            "captures": {},
        }
    return None


def port_expect_for(case: dict[str, Any]) -> dict[str, Any] | None:
    """The rulings: fields where a port answers differently from the oracle."""
    if case.get("port_expect_note") == "G2-2":
        # G2-2: a port reads a request as UTF-8 only (a BOM allowed); a
        # UTF-16 or UTF-32 request is a bad request.
        return {"reply": BAD_REQUEST_REPLY, "audit": [], "tree": {}, "modes": {}, "captures": {}}
    return None


if __name__ == "__main__":
    os.environ.setdefault("CUDA_VISIBLE_DEVICES", "")
    raise SystemExit(main())
