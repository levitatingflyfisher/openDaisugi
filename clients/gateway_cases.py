"""Synthetic gateway cases: `daisugi gateway`, `gateway-report`, `router
status|stop`, `route` and `install --gateway`, run through the Python oracle.

    uv run --no-sync python clients/gateway_cases.py [--out clients/fixtures/gateway] [--only NAME]

A proxy case starts the gateway as a real process on a free loopback port,
in front of a fake upstream: a local HTTP server that answers from the
case's table, keyed by the SHA-256 of the request path and the exact body
bytes. A request with no recorded answer gets HTTP 597, so a body that
differs by one byte shows up. The client is a raw socket, so the bytes it
sends are exact. The case then stops the gateway with a signal and records:

- what the client got back: status, the headers that matter, the body
  (chunked framing removed; a body cut short is marked so), and whether a
  streamed body arrived while the upstream was still sending;
- what the upstream got: path, the headers that matter (a credential kept
  as a name, never its value) and the body bytes;
- the gateway's exit code, stdout and stderr, less the server's own log
  lines (the uvicorn grammar, ruled GW-1);
- the tree after: the turn journal, the answer store, and any Switchyard
  files.

Switchyard cases put a fake `switchyard-server` first on PATH. It answers
--version, writes what it was started with (argv, the config it loaded and
that file's mode) to a log, serves /health and the case's table on the
port it was given, and marks when it is stopped. The case records whether
the child was still alive after the gateway exited.

The other commands run once each in a scratch HOME, as clients/cli_cases.py
runs its cases: exit code, stdout, stderr and the tree after.

Nothing here reaches a real host or a real model: PATH is the case's bin
directory (a fake `claude` that fails loudly, and the fake
`switchyard-server` when a case wants one) then /usr/bin:/bin, and every
upstream is 127.0.0.1. Every path is written {HOME}; ports are {GW}, {UP}
and {SY}; times near the run are offsets.
"""

from __future__ import annotations

import argparse
import contextlib
import gzip
import hashlib
import http.server
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import zlib
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fake_proxy import FakeProxy  # noqa: E402 - sibling module, run as a script
from garden_cases import norm_stderr, read_tree  # noqa: E402
from pathway_cases import REPO, canonical_json, case_id  # noqa: E402
from ports import PortPool, bind_clash, sub_number  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "gateway"
SCRATCH = Path(
    os.environ.get("DAISUGI_GATEWAY_SCRATCH")
    or Path.home() / "opendaisugi-scratch" / "e-go" / "runs"
)
CASE_VERSION = 1
PY_CLI = [sys.executable, "-m", "opendaisugi.cli"]
CONFIG = ".opendaisugi/config.yaml"
TOKEN = "sk-ant-oat01-case-token-7f3a"
KEY = "sk-ant-api03-case-key-19bd"

# ---------------------------------------------------------------------------
# The fake upstream
# ---------------------------------------------------------------------------


def upstream_key(path: str, body: bytes) -> str:
    return hashlib.sha256(path.encode("utf-8") + b"\0" + body).hexdigest()


# The request headers the fixture keeps. Credentials are kept as a name.
KEPT_REQUEST = ("anthropic-version", "anthropic-beta", "content-type", "accept-encoding")


def redact_headers(pairs: list[tuple[str, str]]) -> tuple[list[list[str]], bool]:
    out: list[list[str]] = []
    ok = True
    for k, v in pairs:
        name = k.lower()
        if name == "authorization":
            good = v == "Bearer " + TOKEN
            out.append([name, "Bearer {TOKEN}" if good else "{WRONG AUTHORIZATION}"])
            ok &= good
        elif name == "x-api-key":
            good = v == KEY
            out.append([name, "{KEY}" if good else "{WRONG KEY}"])
            ok &= good
        elif name in KEPT_REQUEST or name.startswith("x-case"):
            out.append([name, v])
    out.sort()
    return out, ok


def encode_body(data: bytes, enc: str | None) -> bytes:
    if enc == "gzip":
        return gzip.compress(data, mtime=0)
    if enc == "deflate":
        return zlib.compress(data)
    if enc == "raw-deflate":
        c = zlib.compressobj(wbits=-15)
        return c.compress(data) + c.flush()
    return data


def answer_bytes(part: dict[str, Any]) -> bytes:
    if "hex" in part:
        return bytes.fromhex(part["hex"])
    return part["text"].encode("utf-8")


def serve_answer(
    handler: http.server.BaseHTTPRequestHandler,
    ans: dict[str, Any] | None,
    extra_headers: list[list[str]] | None = None,
) -> None:
    """Write one recorded answer: a whole body, or chunks with pauses,
    possibly cut before the end."""
    w = handler.wfile
    if ans is None:
        status, headers, chunks = (
            597,
            [["content-type", "application/json"]],
            [{"text": '{"error": "no recorded answer"}'}],
        )
        ans = {}
    else:
        status = ans.get("status", 200)
        headers = [list(h) for h in ans.get("headers", [["content-type", "application/json"]])]
        chunks = ans.get("chunks") or [
            {"text": ans.get("body", ""), **({"hex": ans["body_hex"]} if "body_hex" in ans else {})}
        ]
    headers += extra_headers or []
    if ans.get("sleep"):
        time.sleep(ans["sleep"])
    enc = ans.get("encoding")
    try:
        handler.send_response(status)
        for k, v in headers:
            handler.send_header(k, v)
        if ans.get("chunks") or ans.get("cut"):
            handler.send_header("transfer-encoding", "chunked")
            handler.end_headers()
            w.flush()
            for part in chunks:
                if part.get("delay"):
                    time.sleep(part["delay"])
                data = answer_bytes(part)
                w.write(b"%x\r\n%s\r\n" % (len(data), data))
                w.flush()
            if ans.get("cut"):
                handler.close_connection = True
                handler.connection.shutdown(socket.SHUT_RDWR)
                return
            w.write(b"0\r\n\r\n")
            w.flush()
        else:
            data = encode_body(b"".join(answer_bytes(p) for p in chunks), enc)
            if ans.get("cut_bytes") is not None:
                handler.send_header("content-length", str(len(data)))
                handler.end_headers()
                w.write(data[: ans["cut_bytes"]])
                w.flush()
                handler.close_connection = True
                handler.connection.shutdown(socket.SHUT_RDWR)
                return
            handler.send_header("content-length", str(len(data)))
            handler.end_headers()
            w.write(data)
            w.flush()
    except (BrokenPipeError, ConnectionResetError, OSError):
        pass


class Upstream:
    """The fake upstream: every request is logged, every answer comes from
    the table."""

    def __init__(self, table: dict[str, Any], log: list[dict[str, Any]], kind: str = "upstream"):
        self.table = table
        self.log = log
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def handle_any(self):
                n = int(self.headers.get("content-length", 0) or 0)
                body = self.rfile.read(n)
                if self.headers.get("transfer-encoding", "").lower() == "chunked":
                    body = read_chunked(self.rfile)
                key = upstream_key(self.path, body)
                headers, ok = redact_headers(list(self.headers.items()))
                entry = {
                    "kind": kind,
                    "method": self.command,
                    "path": self.path,
                    "headers": headers,
                }
                try:
                    entry["body"] = body.decode("utf-8")
                except UnicodeDecodeError:
                    entry["body_hex"] = body.hex()
                if not ok:
                    entry["credential_ok"] = False
                outer.log.append(entry)
                serve_answer(self, outer.table.get(key))

            do_POST = do_GET = do_PUT = handle_any  # noqa: N815 - http.server's names

            def log_message(self, *a):
                pass

        class Quiet(http.server.ThreadingHTTPServer):
            def handle_error(self, request, client_address):
                pass  # a client that drops its connection is not a finding

        self.httpd = Quiet(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()


def read_chunked(f) -> bytes:
    out = b""
    while True:
        line = f.readline()
        n = int(line.split(b";")[0].strip() or b"0", 16)
        if n == 0:
            f.readline()
            return out
        out += f.read(n)
        f.readline()


# ---------------------------------------------------------------------------
# The raw client
# ---------------------------------------------------------------------------

# Response headers not compared: the server's own date and name, and the
# framing each side picks (ruled GW-2).
SKIP_RESPONSE = {"date", "server", "transfer-encoding", "content-length", "connection"}


def parse_response(raw: bytes, times: list[tuple[float, int]]) -> dict[str, Any]:
    head, sep, rest = raw.partition(b"\r\n\r\n")
    if not sep:
        return {"status": None, "raw_hex": raw.hex()}
    lines = head.decode("latin-1").split("\r\n")
    status = int(lines[0].split(" ")[1])
    headers = []
    chunked = False
    length = None
    for ln in lines[1:]:
        k, _, v = ln.partition(":")
        k, v = k.strip().lower(), v.strip()
        if k == "transfer-encoding" and v.lower() == "chunked":
            chunked = True
        if k == "content-length":
            length = int(v)
        if k not in SKIP_RESPONSE:
            headers.append([k, v])
    headers.sort()
    complete = True
    if chunked:
        body = b""
        buf = rest
        while True:
            i = buf.find(b"\r\n")
            if i < 0:
                complete = False
                break
            n = int(buf[:i].split(b";")[0] or b"0", 16)
            if n == 0:
                break
            chunk = buf[i + 2 : i + 2 + n]
            body += chunk
            if len(chunk) < n or buf[i + 2 + n : i + 4 + n] != b"\r\n":
                complete = False
                break
            buf = buf[i + 4 + n :]
    else:
        body = rest if length is None else rest[:length]
        complete = length is None or len(rest) >= length
    out: dict[str, Any] = {"status": status, "headers": headers}
    try:
        out["body"] = body.decode("utf-8")
    except UnicodeDecodeError:
        out["body_hex"] = body.hex()
    if not complete:
        out["cut"] = True
    return out


def client_request(port: int, req: dict[str, Any]) -> dict[str, Any]:
    """Send one request over a raw socket and read the whole answer."""
    method = req.get("method", "POST")
    path = req.get("path", "/v1/messages")
    if "body_hex" in req:
        body = bytes.fromhex(req["body_hex"])
    elif "json" in req:
        body = json.dumps(req["json"]).encode("utf-8")
    else:
        body = req.get("body", "").encode("utf-8")
    headers = [["Host", f"127.0.0.1:{port}"]]
    headers += [
        [k, v.replace("{TOKEN}", TOKEN).replace("{KEY}", KEY)] for k, v in req.get("headers", [])
    ]
    if not any(k.lower() == "content-type" for k, _ in headers) and not req.get("no_content_type"):
        headers.append(["content-type", "application/json"])
    headers.append(["Content-Length", str(len(body))])
    headers.append(["Connection", "close"])
    data = (
        f"{method} {path} HTTP/1.1\r\n".encode()
        + b"".join(f"{k}: {v}\r\n".encode("latin-1") for k, v in headers)
        + b"\r\n"
        + body
    )
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    if req.get("source"):
        s.bind((req["source"], 0))
    s.settimeout(60)
    t0 = time.monotonic()
    s.connect(("127.0.0.1", port))
    s.sendall(data)
    if req.get("disconnect"):
        time.sleep(0.2)
        s.close()
        return {"disconnected": True}
    raw = b""
    times: list[tuple[float, int, int]] = []
    while True:
        try:
            chunk = s.recv(65536)
        except (ConnectionResetError, TimeoutError):
            break
        if not chunk:
            break
        times.append((time.monotonic() - t0, len(raw), len(raw) + len(chunk)))
        raw += chunk
    s.close()
    out = parse_response(raw, [])
    if req.get("check_streamed"):
        # The first body bytes arrived well before the last ones: the
        # gateway passed the stream on as it came.
        head_end = raw.find(b"\r\n\r\n") + 4
        body_times = [t for (t, start, end) in times if end > head_end]
        out["streamed"] = bool(body_times) and (body_times[-1] - body_times[0]) > 0.8
    return out


# ---------------------------------------------------------------------------
# Running a gateway case
# ---------------------------------------------------------------------------

FAKE_CLAUDE = """#!/bin/sh
echo "fake claude: a gateway case must never run claude" >&2
exit 97
"""

FAKE_SWITCHYARD = r"""#!{PYTHON}
import hashlib, http.server, json, os, signal, socket, sys, threading, time
argv = sys.argv[1:]
log = os.environ["FAKE_SY_LOG"]
def note(**kw):
    with open(log, "a", encoding="utf-8") as f:
        f.write(json.dumps(kw) + "\n")
if argv == ["--version"]:
    ver = os.environ.get("FAKE_SY_VERSION", "switchyard-server 0.3.0")
    if ver == "fail":
        sys.exit(1)
    print(ver)
    sys.exit(0)
opts = dict(zip(argv[0::2], argv[1::2]))
cfg = opts.get("--config", "")
try:
    text = open(cfg, encoding="utf-8").read()
    mode = oct(os.stat(cfg).st_mode & 0o777)
except OSError as exc:
    text, mode = None, str(exc)
note(event="start", argv=argv, config=text, config_mode=mode, pid=os.getpid())
behaviour = os.environ.get("FAKE_SY_MODE", "ok")
if behaviour == "exit":
    sys.stderr.write("error: route daisugi: unknown target\n")
    sys.exit(2)
def stop(signum, frame):
    note(event="signal", signum=signum)
    os._exit(0)
signal.signal(signal.SIGTERM, stop)
table = json.load(open(os.environ["FAKE_UPSTREAM_TABLE"], encoding="utf-8"))
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        ok = self.path == "/health" and behaviour != "unhealthy"
        out = b"ok" if ok else b"no"
        self.send_response(200 if ok else 503)
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)
    def do_POST(self):
        n = int(self.headers.get("content-length", 0) or 0)
        body = self.rfile.read(n)
        key = hashlib.sha256(self.path.encode() + b"\0" + body).hexdigest()
        auth = self.headers.get("authorization")
        note(event="request", path=self.path, body=body.decode("utf-8", "replace"), key=key,
             auth="Bearer {TOKEN}" if auth == "Bearer " + os.environ.get("FAKE_TOKEN", "") else auth and "{WRONG}")
        sys.path.insert(0, os.environ["FAKE_HARNESS_DIR"])
        from gateway_cases import serve_answer
        serve_answer(self, table.get(key))
    def log_message(self, *a):
        pass
host = opts.get("--host", "0.0.0.0")
if behaviour == "slow":
    time.sleep(float(os.environ.get("FAKE_SY_SLOW", "30")))
class Quiet(http.server.ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        pass
srv = Quiet((host, int(opts["--port"])), H)
srv.daemon_threads = True
srv.serve_forever()
"""

# The server's own log lines: uvicorn's grammar (ruled GW-1).
_LOG_LINE = re.compile(r"^(INFO|WARNING|ERROR|DEBUG|CRITICAL|TRACE):\s")


def drop_server_log(text: str) -> list[str]:
    out = []
    skipping = False
    for ln in text.splitlines():
        if _LOG_LINE.match(ln):
            skipping = "Exception in ASGI application" in ln or "Traceback" in ln
            continue
        if skipping:
            continue
        out.append(ln)
    return out


def base_env(home: Path, work: Path) -> dict[str, str]:
    return {
        "HOME": str(home),
        "PATH": f"{work / 'bin'}:/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "COLUMNS": "100",
        "HF_HUB_OFFLINE": "1",
        "XDG_CACHE_HOME": str(home / ".cache"),
        "TMPDIR": str(work / "tmp"),
    }


def lay_out(tree: dict[str, Any], home: Path, subst) -> None:
    home.mkdir(parents=True, exist_ok=True)
    os.chmod(home, 0o755)
    for rel, spec in sorted(tree.items()):
        p = home / subst(rel)
        if "dir" in spec:
            p.mkdir(parents=True, exist_ok=True)
        elif "db" in spec:
            from garden_cases import lay_out_pathways

            lay_out_pathways(p, spec["db"], str(home), time.time())
        else:
            p.parent.mkdir(parents=True, exist_ok=True)
            if "hex" in spec:
                p.write_bytes(bytes.fromhex(spec["hex"]))
            else:
                p.write_bytes(subst(spec["text"]).encode("utf-8"))
        if "mode" in spec:
            os.chmod(p, spec["mode"])


def check_fakes(env: dict[str, str], work: Path) -> None:
    """Only the case's fakes may answer to claude and switchyard-server."""
    for name in ("claude", "switchyard-server"):
        found = shutil.which(name, path=env["PATH"])
        if found is not None and Path(found).parent != work / "bin":
            raise SystemExit(f"refusing to run: {name} resolves to {found}, not a fake")


def wait_ready(port: int, proc: subprocess.Popen, limit: float = 30.0) -> bool:
    end = time.monotonic() + limit
    while time.monotonic() < end:
        if proc.poll() is not None:
            return False
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return True
        except OSError:
            time.sleep(0.05)
    return False


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    try:
        stat = Path(f"/proc/{pid}/stat").read_text()
        return stat.split(")")[-1].split()[0] != "Z"
    except OSError:
        return False


def run_proxy(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    (work / "bin").mkdir(parents=True)
    (work / "tmp").mkdir()
    fake = work / "bin" / "claude"
    fake.write_text(FAKE_CLAUDE, encoding="utf-8")
    fake.chmod(0o755)
    t0 = time.time()
    table: dict[str, Any] = {}
    log: list[dict[str, Any]] = []
    plog: list[dict[str, Any]] = []
    with contextlib.ExitStack() as stack:
        # The in-process servers listen first; the pool then holds the
        # other ports bound until the gateway starts, so no two roles share one.
        up = stack.enter_context(Upstream(table, log))
        pool = stack.enter_context(PortPool(avoid=[up.port]))
        if case.get("fake_proxy") is not None:
            px = stack.enter_context(
                FakeProxy(plog, up.port, case["fake_proxy"].get("mode", "forward"))
            )
            pool.avoid.add(px.port)
        gw, sy = pool.take(), pool.take()
        ports = {"{GW}": str(gw), "{UP}": str(up.port), "{SY}": str(sy)}
        if case.get("fake_proxy") is not None:
            ports["{PX}"] = str(px.port)
            ports["{DEAD}"] = str(pool.take())

        def subst(s: str) -> str:
            s = s.replace("{HOME}", str(home))
            for k, v in ports.items():
                s = s.replace(k, v)
            return s

        for path, body, ans in case.get("table", []):
            table[
                upstream_key(
                    path,
                    subst(body).encode("utf-8")
                    if isinstance(body, str)
                    else bytes.fromhex(body["hex"]),
                )
            ] = ans
        (work / "table.json").write_text(json.dumps(table), encoding="utf-8")
        lay_out(case.get("before") or {}, home, subst)
        env = base_env(home, work)
        sy_log = work / "switchyard.log"
        if case.get("switchyard"):
            p = work / "bin" / "switchyard-server"
            p.write_text(FAKE_SWITCHYARD.replace("{PYTHON}", sys.executable), encoding="utf-8")
            p.chmod(0o755)
            env.update(
                {
                    "FAKE_SY_LOG": str(sy_log),
                    "FAKE_UPSTREAM_TABLE": str(work / "table.json"),
                    "FAKE_HARNESS_DIR": str(Path(__file__).resolve().parent),
                    "FAKE_TOKEN": TOKEN,
                }
            )
            env.update(case["switchyard"] if isinstance(case["switchyard"], dict) else {})
        env.update({k: subst(v) for k, v in (case.get("env") or {}).items()})
        check_fakes(env, work)
        argv = [
            "gateway",
            "--port",
            str(gw),
            "--upstream",
            subst(case.get("upstream", "http://127.0.0.1:{UP}")),
            "--openai-upstream",
            f"http://127.0.0.1:{up.port}",
        ]
        argv += [subst(a) for a in case.get("argv", [])]
        old = os.umask(0o022)
        pool.release()
        try:
            proc = subprocess.Popen(
                cmd + argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, cwd=home
            )
        finally:
            os.umask(old)
        ready = wait_ready(gw, proc, case.get("ready_limit", 30.0))
        responses: list[Any] = []
        if ready:
            for step in case.get("steps", []):
                if "req" in step:
                    req = json.loads(subst(json.dumps(step["req"])))
                    responses.append(client_request(gw, req))
                elif "signal" in step:
                    proc.send_signal(getattr(signal, "SIG" + step["signal"]))
                    time.sleep(step.get("wait", 0.5))
                elif "write" in step:
                    p = home / step["write"]
                    p.parent.mkdir(parents=True, exist_ok=True)
                    p.write_text(subst(step["text"]), encoding="utf-8")
                    st = p.stat()
                    os.utime(p, (st.st_atime + 5, st.st_mtime + 5))
                elif "sleep" in step:
                    time.sleep(step["sleep"])
            time.sleep(case.get("settle", 0.2))
            proc.send_signal(getattr(signal, "SIG" + case.get("stop", "INT")))
        try:
            out, err = proc.communicate(timeout=30)
        except subprocess.TimeoutExpired:
            proc.kill()
            out, err = proc.communicate()
            err += b"\n{HARNESS: the gateway did not stop in 30s}"
        child: dict[str, Any] = {}
        if case.get("switchyard"):
            events = (
                [json.loads(ln) for ln in sy_log.read_text(encoding="utf-8").splitlines()]
                if sy_log.exists()
                else []
            )
            pids = [e["pid"] for e in events if e.get("event") == "start"]
            for pid in pids:
                end = time.monotonic() + 3
                while pid_alive(pid) and time.monotonic() < end:
                    time.sleep(0.05)
                child.setdefault("alive_after", []).append(pid_alive(pid))
                if pid_alive(pid):
                    os.kill(pid, signal.SIGKILL)
            child["events"] = [{k: v for k, v in e.items() if k != "pid"} for e in events]
    result = {
        "ready": ready,
        "exit": proc.returncode,
        "stdout": drop_server_log(out.decode("utf-8", "replace")),
        "stderr": norm_stderr("\n".join(drop_server_log(err.decode("utf-8", "replace")))),
        "responses": responses,
        "upstream": log,
        "tree": read_tree(home),
    }
    if child:
        result["switchyard"] = child
    if case.get("fake_proxy") is not None:
        if case["fake_proxy"].get("unique"):
            # The health probe polls, so how often it asks is timing.
            plog = [e for i, e in enumerate(plog) if e not in plog[:i]]
        result["proxy"] = plog
    blob = json.dumps(result)
    if TOKEN in blob or KEY in blob:
        raise SystemExit(f"{case['name']}: a credential reached the record")
    return normalize(result, str(home), t0, ports)


_ISOTIME = re.compile(r"\b(20[0-9]{2}-[0-9]{2}-[0-9]{2})T([0-9]{2}:[0-9]{2}:[0-9]{2})Z")
_FLOATTIME = re.compile(r"(?<![0-9.])1[0-9]{9}\.[0-9]+(?![0-9])")
_PID = re.compile(r'(\\?"pid\\?": |\bpid )([0-9]+)')
# A turn's time in the proxy, in every form a line holds it.
_ELAPSED = re.compile(r'((?<![a-z_])elapsed_ms(?:\\)*":\s?)-?[0-9][0-9.eE+-]*')


def normalize(
    result: dict[str, Any], home: str, t0: float, ports: dict[str, str]
) -> dict[str, Any]:
    text = json.dumps(result, ensure_ascii=True)
    text = text.replace(json.dumps(home)[1:-1], "{HOME}")
    text = text.replace(json.dumps(str(Path(home).parent))[1:-1], "{WORK}")
    for k, v in sorted(ports.items(), key=lambda kv: -len(kv[1])):
        text = sub_number(text, v, k)

    def iso_sub(m: re.Match[str]) -> str:
        from datetime import datetime, timezone

        try:
            v = (
                datetime.strptime(m.group(1) + m.group(2), "%Y-%m-%d%H:%M:%S")
                .replace(tzinfo=timezone.utc)
                .timestamp()
            )
        except ValueError:
            # Not a real date (a case's own bad row): kept as written.
            return m.group(0)
        off = v - t0
        return m.group(0) if abs(off) > 3600 else "{ISO}"

    def float_sub(m: re.Match[str]) -> str:
        return "{NOW}" if abs(float(m.group(0)) - t0) < 3600 else m.group(0)

    text = _ISOTIME.sub(iso_sub, text)
    text = _FLOATTIME.sub(float_sub, text)
    text = _PID.sub(lambda m: m.group(1) + "{PID}", text)
    text = _ELAPSED.sub(r"\1{MS}", text)
    return json.loads(text)


# ---------------------------------------------------------------------------
# One-shot command cases
# ---------------------------------------------------------------------------


def run_cli(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    (work / "bin").mkdir(parents=True)
    (work / "tmp").mkdir()
    fake = work / "bin" / "claude"
    fake.write_text(FAKE_CLAUDE, encoding="utf-8")
    fake.chmod(0o755)
    t0 = time.time()
    env = base_env(home, work)
    procs: list[subprocess.Popen] = []
    ports: dict[str, str] = {}
    health: list[http.server.ThreadingHTTPServer] = []
    if case.get("switchyard"):
        p = work / "bin" / "switchyard-server"
        p.write_text(FAKE_SWITCHYARD.replace("{PYTHON}", sys.executable), encoding="utf-8")
        p.chmod(0o755)
        env.update(
            {
                "FAKE_SY_LOG": str(work / "switchyard.log"),
                "FAKE_UPSTREAM_TABLE": str(work / "table.json"),
                "FAKE_HARNESS_DIR": str(Path(__file__).resolve().parent),
            }
        )
        (work / "table.json").write_text("{}", encoding="utf-8")
        env.update(case["switchyard"] if isinstance(case["switchyard"], dict) else {})
    pool = PortPool()
    for name in case.get("ports", []):
        ports["{" + name + "}"] = str(pool.take())
    pool.release()

    def subst(s: str) -> str:
        s = s.replace("{HOME}", str(home))
        for k, v in ports.items():
            s = s.replace(k, v)
        return s

    # Children a state file names: a real fake switchyard-server started
    # here, so `router status` and `router stop` see a live process.
    for spec in case.get("children", []):
        argv = [
            str(work / "bin" / "switchyard-server"),
            "--config",
            subst(spec.get("config", "{HOME}/c.toml")),
            "--host",
            "127.0.0.1",
            "--port",
            subst(spec["port"]),
        ]
        if spec.get("other_program"):
            argv = ["/bin/sleep", "300"]
        proc = subprocess.Popen(
            argv,
            env={**env, **({"FAKE_SY_MODE": spec["mode"]} if "mode" in spec else {})},
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        procs.append(proc)
        ports["{PID" + str(len(procs)) + "}"] = str(proc.pid)
        if not spec.get("other_program") and spec.get("mode") != "unhealthy":
            wait_health(int(subst(spec["port"])))
    lay_out(case.get("before") or {}, home, subst)
    env.update({k: subst(v) for k, v in (case.get("env") or {}).items()})
    for k in case.get("unset", []):
        env.pop(k, None)
    check_fakes(env, work)
    argv = [subst(a) for a in case["argv"]]
    old = os.umask(0o022)
    try:
        proc = subprocess.run(
            cmd + argv,
            capture_output=True,
            env=env,
            cwd=home,
            input=case.get("stdin", "").encode(),
            timeout=120,
            check=False,
        )
    finally:
        os.umask(old)
    alive = []
    for p in procs:
        end = time.monotonic() + 3
        while p.poll() is None and time.monotonic() < end:
            time.sleep(0.05)
        alive.append(p.poll() is None)
        if p.poll() is None:
            p.kill()
        p.wait()
    for h in health:
        h.shutdown()
    result = {
        "exit": proc.returncode,
        "stdout": proc.stdout.decode("utf-8", "replace"),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace")),
        "tree": read_tree(home),
    }
    if procs:
        result["children_alive_after"] = alive
    pid_ports = {k: v for k, v in ports.items() if k.startswith("{PID")}
    other = {k: v for k, v in ports.items() if not k.startswith("{PID")}
    out = normalize(result, str(home), t0, other)
    text = json.dumps(out)
    for k, v in pid_ports.items():
        text = sub_number(text, v, k)
    return json.loads(text)


def wait_health(port: int) -> None:
    end = time.monotonic() + 15
    while time.monotonic() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)


ORACLE_RECALL = r"""
import asyncio, json, sys
from dataclasses import asdict
from pathlib import Path
from opendaisugi import gateway_answers, gateway_recall
from opendaisugi.models import Envelope
from opendaisugi.pathway_store import PathwayStore

for ln in sys.stdin:
    q = json.loads(ln)
    try:
        if q["kind"] == "recall":
            store = PathwayStore(Path(q["db"]))
            env = Envelope.model_validate(q["envelope"])
            r = asyncio.run(gateway_recall.recall(q["task"], env, pathway_store=store, z3_timeout_ms=q["z3_timeout_ms"]))
            out = {"hit": r.hit, "reason": r.reason,
                   "plan": r.plan.model_dump(mode="json") if r.plan is not None else None,
                   "provenance": asdict(r.provenance) if r.provenance is not None else None}
        else:
            entries = gateway_answers.AnswerStore(path=Path(q["answers"])).load()
            kw = {}
            if q.get("max_age_seconds") is not None:
                kw["max_age_seconds"] = q["max_age_seconds"]
            r = gateway_answers.recall_answer(q["task"], entries, now=q["now"],
                                              current_ground_hash=q.get("ground_hash"), **kw)
            out = {"hit": r.hit, "reason": r.reason, "answer": r.answer,
                   "provenance": asdict(r.provenance) if r.provenance is not None else None}
    except Exception as exc:
        out = {"error": type(exc).__name__}
    print(json.dumps(out), flush=True)
"""


def run_recall(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    """Recall queries against a laid-out store: the oracle in process, the
    binary's side through recall-probe (DAISUGI_RECALL_PROBE)."""
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    (work / "bin").mkdir(parents=True)
    (work / "tmp").mkdir()
    fake = work / "bin" / "claude"
    fake.write_text(FAKE_CLAUDE, encoding="utf-8")
    fake.chmod(0o755)
    subst = lambda s: s.replace("{HOME}", str(home))  # noqa: E731
    lay_out(case.get("before") or {}, home, subst)
    env = base_env(home, work)
    check_fakes(env, work)
    data = "".join(json.dumps(json.loads(subst(json.dumps(q)))) + "\n" for q in case["queries"])
    if cmd == PY_CLI:
        argv = [sys.executable, "-c", ORACLE_RECALL]
    else:
        argv = [os.environ["DAISUGI_RECALL_PROBE"]]
    proc = subprocess.run(
        argv, input=data.encode(), capture_output=True, env=env, cwd=home, timeout=300, check=False
    )
    outs = [json.loads(ln) for ln in proc.stdout.decode().splitlines() if ln.strip()]
    for o in outs:
        prov = o.get("provenance") or {}
        if isinstance(prov.get("similarity"), float):
            # A cosine agrees within 1e-9: numpy's dot and the binary's
            # loop may round the last bits apart (PW-4).
            prov["similarity"] = round(prov["similarity"], 9)
        if "error" in o:
            o["error"] = "error"
    return {"exit": proc.returncode, "results": outs, "tree": read_tree(home)}


def run_case(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    if case["kind"] == "recall":
        return run_recall(case, cmd, work)
    run = run_proxy if case["kind"] == "proxy" else run_cli
    result = run(case, cmd, work)
    if bind_clash(result):
        # Another process on the box took a port between the pool's
        # release and the bind: run once more on new ports.
        result = run(case, cmd, work)
    return result


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------


def msg_body(
    text: Any = "say hi",
    *,
    model: Any = "claude-opus-4-8",
    stream: bool | None = None,
    system: Any = None,
    extra: dict | None = None,
    messages: Any = None,
) -> str:
    body: dict[str, Any] = {"model": model, "max_tokens": 64}
    if system is not None:
        body["system"] = system
    body["messages"] = messages if messages is not None else [{"role": "user", "content": text}]
    if stream is not None:
        body["stream"] = stream
    body.update(extra or {})
    return json.dumps(body)


def swapped(body: str, model: str) -> str:
    """The body as the gateway sends it on a downgrade: json.dumps of the
    same dict with model replaced."""
    d = json.loads(body)
    d["model"] = model
    return json.dumps(d)


def reserialized(body: str) -> str:
    return json.dumps(json.loads(body))


def anthropic_reply(
    text: str = "hi", *, model: str = "claude-haiku-4-5", usage: Any = None, **extra
) -> str:
    out = {
        "id": "msg_1",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": [{"type": "text", "text": text}],
        "stop_reason": "end_turn",
        "usage": usage if usage is not None else {"input_tokens": 12, "output_tokens": 5},
    }
    out.update(extra)
    return json.dumps(out)


def sse(*events: dict[str, Any], ascii: bool = True) -> str:
    return "".join(
        f"event: {e.get('type', 'x')}\ndata: {json.dumps(e, ensure_ascii=ascii)}\n\n"
        for e in events
    )


def anthropic_events(
    text: str = "hello there",
    *,
    model: str = "claude-haiku-4-5",
    usage_in: dict | None = None,
    out_tokens: int = 7,
) -> list[dict[str, Any]]:
    evs: list[dict[str, Any]] = [
        {
            "type": "message_start",
            "message": {
                "id": "msg_1",
                "type": "message",
                "role": "assistant",
                "model": model,
                "content": [],
                "usage": usage_in
                or {
                    "input_tokens": 20,
                    "output_tokens": 1,
                    "cache_read_input_tokens": 100,
                    "cache_creation_input_tokens": 30,
                },
            },
        },
        {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
    ]
    for word in text.split(" "):
        evs.append(
            {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": word + " "},
            }
        )
    evs += [
        {"type": "content_block_stop", "index": 0},
        {
            "type": "message_delta",
            "delta": {"stop_reason": "end_turn"},
            "usage": {"output_tokens": out_tokens},
        },
        {"type": "message_stop"},
    ]
    return evs


def sse_answer(
    events: list[dict[str, Any]], *, delay: float = 0.0, split: int | None = None, **kw
) -> dict[str, Any]:
    text = sse(*events)
    chunks: list[dict[str, Any]] = []
    parts = [text] if split is None else [text[i : i + split] for i in range(0, len(text), split)]
    for i, p in enumerate(parts):
        chunks.append({"text": p, **({"delay": delay} if delay and i else {})})
    return {
        "headers": [["content-type", "text/event-stream"], ["x-case-up", "1"]],
        "chunks": chunks,
        **kw,
    }


def json_answer(body: str, status: int = 200, headers: list | None = None, **kw) -> dict[str, Any]:
    return {
        "status": status,
        "headers": headers or [["content-type", "application/json"], ["x-case-up", "1"]],
        "body": body,
        **kw,
    }


AUTH = [["authorization", "Bearer {TOKEN}"], ["anthropic-version", "2023-06-01"]]


def build_proxy_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def add(name: str, steps: list[dict[str, Any]], table: list | None = None, **kw) -> None:
        c = {"kind": "proxy", "name": name, "steps": steps, "table": table or []}
        c.update(kw)
        C.append(c)

    def req(body: str, **kw) -> dict[str, Any]:
        return {"req": {"body": body, "headers": kw.pop("headers", AUTH), **kw}}

    easy = msg_body("say hi")
    hard = msg_body("design a distributed consensus algorithm and prove its security")
    # --- buffered, rules ---
    add(
        "buffered easy downgraded",
        [req(easy)],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    add(
        "buffered hard kept",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
    )
    add(
        "buffered cache usage",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(
                    anthropic_reply(
                        usage={
                            "input_tokens": 3,
                            "output_tokens": 9,
                            "cache_read_input_tokens": 1000,
                            "cache_creation_input_tokens": 200,
                        }
                    )
                ),
            ]
        ],
    )
    add(
        "buffered x-api-key and extra headers",
        [
            req(
                easy,
                headers=[
                    ["x-api-key", "{KEY}"],
                    ["anthropic-version", "2023-06-01"],
                    ["anthropic-beta", "oauth-2025-04-20"],
                    ["x-case-trace", "abc"],
                    ["x-case-trace", "def"],
                ],
            )
        ],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    add(
        "buffered query string",
        [req(easy, path="/v1/messages?beta=true")],
        [
            [
                "/v1/messages?beta=true",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(anthropic_reply()),
            ]
        ],
    )
    add(
        "buffered 4xx on cheap retries original",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer('{"type":"error"}', status=404),
            ],
            ["/v1/messages", easy, json_answer(anthropic_reply(model="claude-opus-4-8"))],
        ],
    )
    add(
        "buffered 5xx on cheap retries original",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer('{"type":"error"}', status=529),
            ],
            ["/v1/messages", easy, json_answer(anthropic_reply(model="claude-opus-4-8"))],
        ],
    )
    add(
        "buffered both attempts fail",
        [req(easy)],
        [
            ["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer('{"a":1}', status=400)],
            [
                "/v1/messages",
                easy,
                json_answer('{"type":"error","error":{"type":"overloaded_error"}}', status=529),
            ],
        ],
    )
    add(
        "buffered 4xx on hard no retry",
        [req(hard)],
        [["/v1/messages", reserialized(hard), json_answer('{"type":"error"}', status=400)]],
    )
    add(
        "buffered unparseable body passes through",
        [req("{not json")],
        [["/v1/messages", "{not json", json_answer(anthropic_reply())]],
    )
    add(
        "buffered list body passes through",
        [req("[1, 2]")],
        [["/v1/messages", "[1, 2]", json_answer("{}")]],
    )
    add("buffered empty body", [req("")], [["/v1/messages", "", json_answer("{}")]])
    add(
        "buffered 3xx not followed",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer("", status=307, headers=[["location", "http://127.0.0.1:1/x"]]),
            ]
        ],
    )
    add("buffered upstream refused", [req(easy)], upstream="http://127.0.0.1:{SY}")
    add(
        "buffered cut body",
        [req(hard)],
        [["/v1/messages", reserialized(hard), json_answer(anthropic_reply(), cut_bytes=10)]],
    )
    add(
        "buffered slow upstream",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(anthropic_reply(model="claude-opus-4-8"), sleep=2.0),
            ]
        ],
    )
    add(
        "buffered gzip answer",
        [req(hard, headers=AUTH + [["accept-encoding", "gzip"]])],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(
                    anthropic_reply("zipped", model="claude-opus-4-8"),
                    headers=[["content-type", "application/json"], ["content-encoding", "gzip"]],
                    encoding="gzip",
                ),
            ]
        ],
    )
    add(
        "buffered deflate answer",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(
                    anthropic_reply("deflated", model="claude-opus-4-8"),
                    headers=[["content-type", "application/json"], ["content-encoding", "deflate"]],
                    encoding="deflate",
                ),
            ]
        ],
    )
    add(
        "buffered br passed raw",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                {
                    "headers": [["content-type", "application/json"], ["content-encoding", "br"]],
                    "body_hex": "0b028068690a03",
                },
            ]
        ],
    )
    add(
        "buffered non json answer",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer("plain text answer", headers=[["content-type", "text/plain"]]),
            ]
        ],
    )
    add(
        "buffered answer without usage",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer('{"model": "claude-haiku-4-5"}'),
            ]
        ],
    )
    add(
        "buffered usage odd values",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(
                    json.dumps(
                        {
                            "usage": {
                                "input_tokens": " 12 ",
                                "output_tokens": 3.9,
                                "cache_read_input_tokens": True,
                            }
                        }
                    )
                ),
            ]
        ],
    )
    add(
        "buffered usage not a dict",
        [req(easy)],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer('{"usage": [1, 2]}')]],
    )
    add(
        "buffered usage null",
        [req(easy)],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer('{"usage": null}')]],
    )
    add(
        "buffered usage bad string",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer('{"usage": {"input_tokens": "1.5"}}'),
            ]
        ],
    )
    add(
        "buffered huge usage",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(
                    '{"usage": {"input_tokens": 123456789012345678901234567890, "output_tokens": 1e308}}'
                ),
            ]
        ],
    )
    add(
        "buffered usage overflow float",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer('{"usage": {"input_tokens": ' + "9" * 400 + "}}"),
            ]
        ],
    )
    add(
        "buffered content not text",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(
                    json.dumps(
                        {"content": [{"type": "text", "text": 5}], "usage": {"input_tokens": 1}}
                    )
                ),
            ]
        ],
    )
    # --- routing shapes ---
    tool_loop = msg_body(
        messages=[
            {"role": "user", "content": "list the files"},
            {
                "role": "assistant",
                "content": [{"type": "tool_use", "id": "t1", "name": "ls", "input": {}}],
            },
            {
                "role": "user",
                "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "a b"}],
            },
        ]
    )
    add(
        "tool loop continuation",
        [req(tool_loop)],
        [["/v1/messages", swapped(tool_loop, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    no_text = msg_body(messages=[{"role": "assistant", "content": "x"}])
    add(
        "no routing signal",
        [req(no_text)],
        [["/v1/messages", reserialized(no_text), json_answer(anthropic_reply())]],
    )
    big_prefix = msg_body("hi", system="S" * 20000)
    add(
        "sticky prefix keeps frontier",
        [req(big_prefix)],
        [
            [
                "/v1/messages",
                reserialized(big_prefix),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
    )
    blocks = msg_body(
        messages=[
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "read"},
                    {"type": "image", "source": {}},
                    {"type": "text", "text": "this"},
                ],
            }
        ],
        system=[{"type": "text", "text": "sys"}, "raw", {"content": [{"text": "nested"}]}],
    )
    add(
        "content blocks",
        [req(blocks)],
        [["/v1/messages", swapped(blocks, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    no_model = json.dumps({"messages": [{"role": "user", "content": "hi"}]})
    add(
        "no model field",
        [req(no_model)],
        [["/v1/messages", swapped(no_model, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    int_model = msg_body("hi", model=7)
    add(
        "int model",
        [req(int_model)],
        [["/v1/messages", swapped(int_model, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    list_model = msg_body("hi", model=["a"])
    add(
        "list model",
        [req(list_model)],
        [["/v1/messages", swapped(list_model, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    bad_msgs = json.dumps(
        {"model": "claude-opus-4-8", "messages": [1, {"role": "user", "content": "hi"}]}
    )
    add(
        "message not a dict",
        [req(bad_msgs)],
        [["/v1/messages", bad_msgs, json_answer(anthropic_reply())]],
    )
    str_msgs = json.dumps({"model": "claude-opus-4-8", "messages": "hi"})
    add(
        "messages a string",
        [req(str_msgs)],
        [["/v1/messages", str_msgs, json_answer(anthropic_reply())]],
    )
    empty_str_msgs = json.dumps({"model": "claude-opus-4-8", "messages": ""})
    add(
        "messages empty string",
        [req(empty_str_msgs)],
        [["/v1/messages", reserialized(empty_str_msgs), json_answer(anthropic_reply())]],
    )
    join_int = msg_body(messages=[{"role": "user", "content": [{"type": "text", "text": 5}]}])
    add(
        "text block not a string",
        [req(join_int)],
        [["/v1/messages", join_int, json_answer(anthropic_reply())]],
    )
    surrogate = (
        '{"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "hi \\ud800"}]}'
    )
    add(
        "lone surrogate in ask",
        [req(surrogate)],
        [["/v1/messages", surrogate, json_answer(anthropic_reply())]],
    )
    unicode_ask = msg_body("résumé ÇA VA? İstanbul ✓ " * 3)
    add(
        "unicode ask",
        [req(unicode_ask)],
        [
            [
                "/v1/messages",
                swapped(unicode_ask, "claude-haiku-4-5"),
                json_answer(anthropic_reply()),
            ]
        ],
    )
    dup = '{"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "hi"}], "model": "x", "stream": false}'
    add(
        "duplicate keys",
        [req(dup)],
        [
            [
                "/v1/messages",
                json.dumps(
                    {
                        "model": "claude-haiku-4-5",
                        "messages": [{"role": "user", "content": "hi"}],
                        "stream": False,
                    }
                ),
                json_answer(anthropic_reply()),
            ]
        ],
    )
    bom = "﻿" + easy
    add(
        "utf8 bom body",
        [{"req": {"body_hex": bom.encode("utf-8").hex(), "headers": AUTH}}],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    utf16 = easy.encode("utf-16-le")
    add(
        "utf16 body",
        [{"req": {"body_hex": utf16.hex(), "headers": AUTH}}],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    bad_utf8 = b'{"model": "m", "messages": [{"role": "user", "content": "\xff"}]}'
    add(
        "invalid utf8 body",
        [{"req": {"body_hex": bad_utf8.hex(), "headers": AUTH}}],
        [["/v1/messages", {"hex": bad_utf8.hex()}, json_answer(anthropic_reply())]],
    )
    floats = (
        '{"model": "claude-opus-4-8", "temperature": 1e-07, "top_p": 1.0, "x": NaN, "big": 1e400, '
        '"n": 10000000000000000000000, "messages": [{"role": "user", "content": "hi"}]}'
    )
    add(
        "numbers reserialized",
        [req(floats)],
        [["/v1/messages", swapped(floats, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    deep = (
        '{"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "hi"}], "x": '
        + "[" * 3000
        + "]" * 3000
        + "}"
    )
    add(
        "deep nesting passes through",
        [req(deep)],
        [["/v1/messages", deep, json_answer(anthropic_reply())]],
    )
    # --- sticky conversations ---
    t1 = msg_body("fix the typo in the readme")
    t2 = msg_body(
        messages=[
            {"role": "user", "content": "fix the typo in the readme"},
            {"role": "assistant", "content": "done"},
            {"role": "user", "content": "now the other one " + "x" * 18000},
        ]
    )
    add(
        "sticky cheap across turns",
        [req(t1), req(t2)],
        [
            ["/v1/messages", swapped(t1, "claude-haiku-4-5"), json_answer(anthropic_reply())],
            ["/v1/messages", swapped(t2, "claude-haiku-4-5"), json_answer(anthropic_reply())],
        ],
    )
    add(
        "sticky lost on reload",
        [req(t1), {"req": {"method": "POST", "path": "/_reload", "body": ""}}, req(t2)],
        [
            ["/v1/messages", swapped(t1, "claude-haiku-4-5"), json_answer(anthropic_reply())],
            [
                "/v1/messages",
                reserialized(t2),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ],
        ],
    )
    add(
        "sticky lost on sighup",
        [req(t1), {"signal": "HUP"}, req(t2)],
        [
            ["/v1/messages", swapped(t1, "claude-haiku-4-5"), json_answer(anthropic_reply())],
            [
                "/v1/messages",
                reserialized(t2),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ],
        ],
    )
    # --- local rung, config, reload ---
    add(
        "local model flag",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "qwen2.5:7b"),
                json_answer(anthropic_reply(model="qwen2.5:7b")),
            ]
        ],
        argv=["--local-model", "qwen2.5:7b"],
    )
    add(
        "local model from config",
        [req(easy)],
        [["/v1/messages", swapped(easy, "llama3"), json_answer(anthropic_reply(model="llama3"))]],
        before={CONFIG: {"text": "gateway_local_model: llama3\n"}},
    )
    add(
        "config change then reload",
        [
            req(easy),
            {"write": CONFIG, "text": "gateway_local_model: m2\n"},
            {"req": {"method": "POST", "path": "/_reload", "body": ""}},
            req(easy),
        ],
        [
            ["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())],
            ["/v1/messages", swapped(easy, "m2"), json_answer(anthropic_reply(model="m2"))],
        ],
    )
    add(
        "config change seen after interval",
        [
            req(easy),
            {"write": CONFIG, "text": "gateway_local_model: m3\n"},
            {"sleep": 2.5},
            req(easy),
        ],
        [
            ["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())],
            ["/v1/messages", swapped(easy, "m3"), json_answer(anthropic_reply(model="m3"))],
        ],
    )
    add(
        "bad config reload keeps running",
        [
            req(easy),
            {"write": CONFIG, "text": "gateway_local_model: [1, 2]\n"},
            {"req": {"method": "POST", "path": "/_reload", "body": ""}},
            req(easy),
        ],
        [["/v1/messages", swapped(easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    add(
        "local flag outranks config",
        [req(easy)],
        [["/v1/messages", swapped(easy, "flagged"), json_answer(anthropic_reply(model="flagged"))]],
        argv=["--local-model", "flagged"],
        before={CONFIG: {"text": "gateway_local_model: filed\n"}},
    )
    add(
        "cheap model flag",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-sonnet-5"),
                json_answer(anthropic_reply(model="claude-sonnet-5")),
            ]
        ],
        argv=["--cheap-model", "claude-sonnet-5"],
    )
    add(
        "cheap model unpriced books nothing",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "tiny-model"),
                json_answer(anthropic_reply(model="tiny-model")),
            ]
        ],
        argv=["--cheap-model", "tiny-model"],
    )
    add(
        "reload endpoint methods",
        [
            {"req": {"method": "GET", "path": "/_reload", "body": ""}},
            {"req": {"method": "POST", "path": "/_reload", "body": "", "source": "127.0.0.2"}},
            {"req": {"method": "POST", "path": "/_reload", "body": ""}},
            {"req": {"method": "POST", "path": "/_reload/", "body": ""}},
        ],
        [["/_reload/", "", json_answer("{}")]],
    )
    add(
        "non loopback client is served",
        [req(hard, source="127.0.0.2")],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
    )
    # --- count_tokens ---
    ct = json.dumps(
        {
            "model": "claude-opus-4-8",
            "system": "sys",
            "messages": [{"role": "user", "content": "count me"}],
        }
    )
    add(
        "count tokens forwarded in rules",
        [req(ct, path="/v1/messages/count_tokens")],
        [
            [
                "/v1/messages/count_tokens",
                swapped(ct, "claude-haiku-4-5"),
                json_answer('{"input_tokens": 9}'),
            ]
        ],
    )
    add(
        "count tokens answered locally",
        [
            req(ct, path="/v1/messages/count_tokens"),
            req("[1]", path="/v1/messages/count_tokens/"),
            req('{"messages": 5}', path="/v1/messages/count_tokens"),
            req("nope", path="/v1/messages/count_tokens"),
        ],
        argv=["--upstream-kind", "ollama"],
    )
    add(
        "count tokens deep body",
        [req('{"system": ' + "[" * 3000 + "]" * 3000 + "}", path="/v1/messages/count_tokens")],
        argv=["--upstream-kind", "openai-compatible"],
    )
    add(
        "count tokens off mode not journaled",
        [req(ct, path="/v1/messages/count_tokens")],
        [["/v1/messages/count_tokens", reserialized(ct), json_answer('{"input_tokens": 9}')]],
        argv=["--router", "off"],
    )
    add(
        "upstream kind from recorded host",
        [req(ct, path="/v1/messages/count_tokens")],
        before={CONFIG: {"text": "llm_base_url: http://127.0.0.1:{UP}\nllm_host_kind: ollama\n"}},
    )
    add(
        "upstream kind recorded other host",
        [req(ct, path="/v1/messages/count_tokens")],
        [
            [
                "/v1/messages/count_tokens",
                swapped(ct, "claude-haiku-4-5"),
                json_answer('{"input_tokens": 9}'),
            ]
        ],
        before={CONFIG: {"text": "llm_base_url: http://127.0.0.1:1\nllm_host_kind: ollama\n"}},
    )
    # --- streaming ---
    s_easy = msg_body("say hi", stream=True)
    add(
        "stream easy",
        [req(s_easy, check_streamed=True)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                sse_answer(anthropic_events(), delay=0.6, split=150),
            ]
        ],
    )
    add(
        "stream hard",
        [req(msg_body("debug the race condition in the scheduler", stream=True))],
        [
            [
                "/v1/messages",
                reserialized(msg_body("debug the race condition in the scheduler", stream=True)),
                sse_answer(anthropic_events(model="claude-opus-4-8")),
            ]
        ],
    )
    add(
        "stream 4xx on cheap retries",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                json_answer('{"type":"error"}', status=429),
            ],
            ["/v1/messages", s_easy, sse_answer(anthropic_events(model="claude-opus-4-8"))],
        ],
    )
    add(
        "stream cut mid way",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                {**sse_answer(anthropic_events(), split=200), "cut": True},
            ]
        ],
    )
    full = sse(*anthropic_events("naïve café"), ascii=False).encode("utf-8")
    cut = full.index("ï".encode("utf-8")) + 1
    add(
        "stream split utf8",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                {
                    "headers": [["content-type", "text/event-stream"]],
                    "chunks": [{"hex": full[:cut].hex()}, {"hex": full[cut:].hex(), "delay": 0.3}],
                },
            ]
        ],
        argv=["--capture-answers"],
    )
    odd = [
        {"type": "message_start", "message": "not a dict"},
        {
            "type": "message_start",
            "message": {"model": 5, "usage": {"input_tokens": 4, "x": "7", "flag": True}},
        },
        {"type": "message_delta", "usage": {"output_tokens": 2.5}},
        {"type": "content_block_delta", "delta": {"type": "text_delta", "text": 9}},
        [1, 2],
    ]
    add(
        "stream odd events",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                {
                    "headers": [["content-type", "text/event-stream"]],
                    "chunks": [
                        {
                            "text": "".join(f"data: {json.dumps(e)}\n\n" for e in odd)
                            + "data: {bad\n\ndata: [DONE]\n\n"
                            + ': comment\n\ndata: {"type": "message_delta", "usage": {"output_tokens": 3}}'
                        }
                    ],
                },
            ]
        ],
    )
    add(
        "stream disconnect still journaled",
        [{"req": {"body": s_easy, "headers": AUTH, "disconnect": True}}, {"sleep": 2.0}],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                sse_answer(anthropic_events(), delay=0.5, split=120),
            ]
        ],
    )
    add(
        "stream flag not true",
        [req(msg_body("say hi", stream="yes"))],
        [
            [
                "/v1/messages",
                swapped(msg_body("say hi", stream="yes"), "claude-haiku-4-5"),
                json_answer(anthropic_reply()),
            ]
        ],
    )
    add(
        "stream answer is json",
        [req(s_easy)],
        [["/v1/messages", swapped(s_easy, "claude-haiku-4-5"), json_answer(anthropic_reply())]],
    )
    # --- capture answers ---
    add(
        "capture answers buffered",
        [req(easy), req(tool_loop)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(anthropic_reply("Hi! How can I help?")),
            ],
            [
                "/v1/messages",
                swapped(tool_loop, "claude-haiku-4-5"),
                json_answer(anthropic_reply("a b")),
            ],
        ],
        argv=["--capture-answers"],
    )
    add(
        "capture answers stream",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "claude-haiku-4-5"),
                sse_answer(anthropic_events("streamed words here")),
            ]
        ],
        argv=["--capture-answers"],
    )
    ring = "".join(
        json.dumps(
            {
                "signature": f"s{i}",
                "task": f"t{i}",
                "answer": "a",
                "created_at": 1.5,
                "ground_hash": None,
            }
        )
        + "\n"
        for i in range(1000)
    )
    add(
        "capture answers ring trims",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(anthropic_reply("fresh")),
            ]
        ],
        argv=["--capture-answers"],
        before={
            ".opendaisugi/gateway/answers.jsonl": {
                "text": ring
                + "{bad\n[1]\n"
                + json.dumps(
                    {"signature": "x", "task": "y", "answer": "z", "created_at": 2, "extra": 1}
                )
                + "\n"
            }
        },
    )
    add(
        "capture answers off by default",
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "claude-haiku-4-5"),
                json_answer(anthropic_reply("not kept")),
            ]
        ],
    )
    # --- openai wire ---
    oa = json.dumps({"model": "gpt-5", "messages": [{"role": "user", "content": "say hi"}]})
    oa_reply = json.dumps(
        {
            "id": "c1",
            "object": "chat.completion",
            "model": "gpt-5-mini",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "hi!"}}],
            "usage": {
                "prompt_tokens": 50,
                "completion_tokens": 4,
                "prompt_tokens_details": {"cached_tokens": 20},
            },
        }
    )
    add(
        "openai buffered",
        [req(oa, path="/v1/chat/completions")],
        [["/v1/chat/completions", swapped(oa, "gpt-5-mini"), json_answer(oa_reply)]],
    )
    oa_s = json.dumps(
        {
            "model": "gpt-5",
            "stream": True,
            "stream_options": {"include_usage": True},
            "messages": [{"role": "user", "content": "say hi"}],
        }
    )
    chunks = [
        {"choices": [{"delta": {"content": "hel"}}]},
        {"choices": [{"delta": {"content": "lo"}}]},
        {"choices": {"a": 1}, "usage": {"prompt_tokens": 1}},
        {
            "choices": [],
            "usage": {
                "prompt_tokens": 30,
                "completion_tokens": 2,
                "prompt_tokens_details": {"cached_tokens": True},
            },
        },
    ]
    add(
        "openai stream",
        [req(oa_s, path="/v1/chat/completions")],
        [
            [
                "/v1/chat/completions",
                swapped(oa_s, "gpt-5-mini"),
                {
                    "headers": [["content-type", "text/event-stream"]],
                    "chunks": [
                        {
                            "text": "".join(f"data: {json.dumps(c)}\n\n" for c in chunks)
                            + "data: [DONE]\n\n"
                        }
                    ],
                },
            ]
        ],
        argv=["--capture-answers"],
    )
    add(
        "openai routing off passes through",
        [req(oa, path="/chat/completions/")],
        [["/chat/completions/", oa, json_answer(oa_reply)]],
        argv=["--openai-cheap-model", ""],
    )
    add(
        "openai cheap flag",
        [req(oa, path="/v1/chat/completions")],
        [["/v1/chat/completions", swapped(oa, "gpt-4o-mini"), json_answer(oa_reply)]],
        argv=["--openai-cheap-model", "gpt-4o-mini"],
    )
    add(
        "openai odd usage",
        [req(oa, path="/v1/chat/completions")],
        [
            [
                "/v1/chat/completions",
                swapped(oa, "gpt-5-mini"),
                json_answer(
                    json.dumps(
                        {
                            "choices": "abc",
                            "usage": {
                                "prompt_tokens": 5,
                                "completion_tokens": True,
                                "prompt_tokens_details": {"cached_tokens": 9},
                            },
                        }
                    )
                ),
            ]
        ],
    )
    # --- router off ---
    add(
        "router off meters only",
        [req(easy)],
        [
            [
                "/v1/messages",
                reserialized(easy),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
        argv=["--router", "off"],
    )
    add(
        "router from config off",
        [req(easy)],
        [
            [
                "/v1/messages",
                reserialized(easy),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
        before={CONFIG: {"text": "gateway_router: 'off'\n"}},
    )
    # --- start-up refusals and signals ---
    add("unknown router", [], argv=["--router", "magic"])
    add("unknown upstream kind", [], argv=["--upstream-kind", "grpc"])
    add("bad port", [], argv=["--port", "http"])
    add("invalid config", [], before={CONFIG: {"text": "gateway_router: [1]\n"}})
    add("port in use", [], argv=["--port", "{UP}"])
    add(
        "stop with sigterm",
        [req(hard)],
        [
            [
                "/v1/messages",
                reserialized(hard),
                json_answer(anthropic_reply(model="claude-opus-4-8")),
            ]
        ],
        stop="TERM",
    )
    add(
        "banner capture and local",
        [],
        argv=[
            "--capture-answers",
            "--local-model",
            "m1",
            "--data-dir",
            "{HOME}/dd/",
            "--host",
            "127.0.0.1",
        ],
    )
    return C


SY_CFG = "gateway_router: switchyard\nswitchyard_efficient_model: llama3\n"


def build_switchyard_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def add(name: str, steps: list[dict[str, Any]], table: list | None = None, **kw) -> None:
        c = {
            "kind": "proxy",
            "name": name,
            "steps": steps,
            "table": table or [],
            "switchyard": kw.pop("switchyard", True),
            "argv": ["--switchyard-port", "{SY}"] + kw.pop("argv", []),
        }
        c.update(kw)
        C.append(c)

    def req(body: str, **kw) -> dict[str, Any]:
        return {"req": {"body": body, "headers": kw.pop("headers", AUTH), **kw}}

    def routed(
        target: str, header: str | None = None, *, body_model: str | None = None, usage=None
    ) -> dict[str, Any]:
        hs = [["content-type", "application/json"]]
        if header is not None:
            hs.append(["x-model-router-selected-model", header])
        return json_answer(anthropic_reply(model=body_model or target, usage=usage), headers=hs)

    easy = msg_body("say hi")
    hard = msg_body("design a distributed consensus algorithm")
    via = swapped(easy, "daisugi")
    cfg = {CONFIG: {"text": SY_CFG}}
    add(
        "switchyard efficient local served",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "llama3")]],
        before=cfg,
    )
    add(
        "switchyard capable served",
        [req(hard)],
        [["/v1/messages", swapped(hard, "daisugi"), routed("claude-sonnet-5", "claude-sonnet-5")]],
        before=cfg,
    )
    add(
        "switchyard no header is unknown",
        [req(easy)],
        [["/v1/messages", via, routed("llama3")]],
        before=cfg,
    )
    add(
        "switchyard header disagrees",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "other")]],
        before=cfg,
    )
    add(
        "switchyard names the route id",
        [req(easy)],
        [["/v1/messages", via, routed("daisugi", "daisugi")]],
        before=cfg,
    )
    add(
        "switchyard header padded",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "  llama3 ", body_model="llama3 ")]],
        before=cfg,
    )
    s_easy = msg_body("say hi", stream=True)
    add(
        "switchyard stream",
        [req(s_easy)],
        [
            [
                "/v1/messages",
                swapped(s_easy, "daisugi"),
                {
                    **sse_answer(anthropic_events(model="llama3")),
                    "headers": [
                        ["content-type", "text/event-stream"],
                        ["x-model-router-selected-model", "llama3"],
                    ],
                },
            ]
        ],
        before=cfg,
    )
    cloud = {
        CONFIG: {
            "text": "gateway_router: switchyard\nswitchyard_efficient_model: claude-haiku-4-5\n"
        }
    }
    add(
        "switchyard claude efficient",
        [req(easy)],
        [["/v1/messages", via, routed("claude-haiku-4-5", "claude-haiku-4-5")]],
        before=cloud,
    )
    add(
        "switchyard flag over config",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "llama3")]],
        before={CONFIG: {"text": "switchyard_efficient_model: llama3\n"}},
        argv=["--router", "switchyard"],
    )
    add(
        "switchyard count tokens forwarded",
        [req(easy, path="/v1/messages/count_tokens")],
        [["/v1/messages/count_tokens", via, json_answer('{"input_tokens": 3}')]],
        before=cfg,
        argv=["--upstream-kind", "ollama"],
    )
    add(
        "switchyard route id from config",
        [req(easy)],
        [["/v1/messages", swapped(easy, "house"), routed("llama3", "llama3")]],
        before={CONFIG: {"text": SY_CFG + "switchyard_route_id: house\n"}},
    )
    add(
        "switchyard recorded host anthropic",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "llama3")]],
        before={
            CONFIG: {
                "text": SY_CFG + "llm_base_url: http://10.0.0.5:8080/\nllm_host_kind: anthropic\n"
            }
        },
    )
    add(
        "switchyard recorded host openai",
        [],
        before={
            CONFIG: {
                "text": SY_CFG + "llm_base_url: http://10.0.0.5:8080/v1\nllm_host_kind: openai\n"
            }
        },
    )
    add(
        "switchyard api key env set",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "llama3")]],
        before={CONFIG: {"text": SY_CFG + "switchyard_api_key_env: CASE_KEY_VAR\n"}},
        env={"CASE_KEY_VAR": "x"},
    )
    add(
        "switchyard api key env unset",
        [],
        before={CONFIG: {"text": SY_CFG + "switchyard_api_key_env: CASE_KEY_VAR\n"}},
    )
    add(
        "switchyard no efficient model",
        [],
        before={CONFIG: {"text": "gateway_router: switchyard\n"}},
    )
    add(
        "switchyard same model both tiers",
        [],
        before={CONFIG: {"text": SY_CFG.replace("llama3", "claude-sonnet-5")}},
    )
    add("switchyard binary missing", [], before=cfg, switchyard=False)
    add("switchyard child exits", [], before=cfg, switchyard={"FAKE_SY_MODE": "exit"})
    add(
        "switchyard stopped by sigterm",
        [req(easy)],
        [["/v1/messages", via, routed("llama3", "llama3")]],
        before=cfg,
        stop="TERM",
    )
    add(
        "switchyard port already answers",
        [],
        [["/health", "", {"status": 200, "body": "ok"}]],
        before=cfg,
        argv=["--switchyard-port", "{UP}"],
    )
    own = (
        'schema_version = 1\n\n[llm_clients.big]\nformat = "anthropic_messages"\nbase_url = "https://api.anthropic.com"\n'
        'forward_auth = true\n\n[llm_clients.small]\nformat = "openai_chat"\nbase_url = "http://127.0.0.1:11434/v1"\n\n'
        '[targets.s]\nid = "claude-sonnet-5"\nllm_client = "big"\n\n[targets.w]\nid = "qwen3"\nllm_client = "small"\n\n'
        '[routes.r]  # the two-tier route\nid = "daisugi"\ntype = "llm_classifier"\nmode = "escalation"\n'
        "strong_target = 's'\nweak_target = \"w\"\n"
    )
    add(
        "switchyard own config",
        [req(easy)],
        [["/v1/messages", via, routed("qwen3", "qwen3")]],
        before={CONFIG: {"text": "gateway_router: switchyard\n"}, "mine.toml": {"text": own}},
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    add(
        "switchyard own config missing",
        [],
        before={CONFIG: {"text": "gateway_router: switchyard\n"}},
        argv=["--switchyard-config", "{HOME}/nope.toml"],
    )
    add(
        "switchyard own config no route",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": own.replace('id = "daisugi"', 'id = "x"')},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    add(
        "switchyard own config one tier",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": own.replace("weak_target", "other")},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    add(
        "switchyard own config target missing",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": own.replace("'s'", "'zz'")},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    add(
        "switchyard own config same ids",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": own.replace('"qwen3"', '"claude-sonnet-5"')},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    add(
        "switchyard own config not toml",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": "[routes\nid = 1\n"},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
        go_refuses=True,
    )
    add(
        "switchyard own config key auth",
        [],
        before={
            CONFIG: {"text": "gateway_router: switchyard\n"},
            "mine.toml": {"text": own.replace("forward_auth = true", 'api_key_env = "K"')},
        },
        argv=["--switchyard-config", "{HOME}/mine.toml"],
    )
    return C


def turn_line(
    task: str,
    *,
    tier: str = "tier1-cheap",
    model: Any = "claude-haiku-4-5",
    requested: Any = "claude-opus-4-8",
    downgraded: bool = True,
    tokens=(100, 20, 0, 0),
    actual: float = 0.0002,
    cf: float = 0.003,
    sig: str | None = None,
    **extra,
) -> str:
    rec = {
        "created_at": "2026-01-02T03:04:05Z",
        "signature": hashlib.sha256(task.encode()).hexdigest()[:16] if sig is None else sig,
        "task": task,
        "tier": tier,
        "requested_model": requested,
        "model": model,
        "difficulty": 0.1,
        "downgraded": downgraded,
        "estimated": downgraded,
        "input_tokens": tokens[0],
        "output_tokens": tokens[1],
        "frontier_tokens_saved": (sum(tokens) if downgraded else 0),
        "actual_dollars": actual,
        "counterfactual_dollars": cf,
        "cache_read_tokens": tokens[2],
        "cache_creation_tokens": tokens[3],
    }
    rec.update(extra)
    return json.dumps(rec)


def router_measure_cases(add: Any, turns: str) -> None:
    """`router status` per ISO week over the turn journal,
    the delegation journal and the gate's graft records, and the delegate
    rule and worker in force. Every date is fixed, and in 2025: the
    normalizer writes a time within an hour of the run as {ISO} or {NOW},
    so a fixed date near the day the cases run would read differently by
    the time of day (RP-15)."""
    dj = ".opendaisugi/router/delegations.jsonl"
    audit = ".opendaisugi/gate/audit"
    grafts = ".opendaisugi/gate/grafts"
    tier1 = ".opendaisugi/local_tier1.json"
    envf = ".opendaisugi/gate/envelopes/default.json"
    rule = {
        "id": "big-read",
        "version": 1,
        "shape": "deny_redirect",
        "state": "active",
        "match": {"tool": "Read", "file_lines_over": 400},
    }

    def lines(rows: list[Any]) -> dict[str, str]:
        return {"text": "".join((r if isinstance(r, str) else json.dumps(r)) + "\n" for r in rows)}

    def row(at: Any, ok: bool = True, **kw: Any) -> dict[str, Any]:
        r = {
            "at": at,
            "mode": "bulk_read",
            "ok": ok,
            "reason": None if ok else "no worker",
            "path": "/w/big.py",
            "file_bytes": 40000,
            "file_lines": 900,
            "worker_model": "openai/qwen",
            "worker_tier": "local",
            "worker_host": "127.0.0.1",
            "route_reason": "r",
            "worker_input_tokens": 10000 if ok else None,
            "worker_output_tokens": 120 if ok else None,
            "worker_dollars": 0.0 if ok else None,
            "elapsed_ms": 12500.5 if ok else 0.25,
            "quotes": 3 if ok else 0,
            "dropped": 1 if ok else 0,
            "frontier_tokens_kept": 9900 if ok else None,
            "frontier_dollars_kept": 0.037125 if ok else None,
            "estimated": True,
            "task_ok": None,
            "kind": "delegate",
        }
        r.update(kw)
        return r

    week40 = [
        row("2025-09-29T09:00:00Z"),
        row("2025-09-30T09:00:00Z", task_ok=True),
        row("2025-10-01T23:59:59Z", ok=False),
        row(
            "2025-10-02T10:00:00Z",
            worker_tier="remote",
            worker_model="anthropic/claude-haiku-4-5",
            worker_dollars=0.0106,
            task_ok=False,
        ),
        row("2025-10-05T23:59:59Z", worker_tier="remote", worker_dollars=None),
    ]
    week39 = [row("2025-09-22T00:00:00Z"), row("2025-09-28T12:00:00Z", quotes=0, dropped=4)]
    graft = {"rule_id": "big-read", "version": 1, "shape": "deny_redirect", "state": "active"}
    audit_rows = [
        # 2025-10-01T00:00:00Z, 2025-09-22T00:00:00Z, 2025-09-28T23:59:59.9Z
        {"at": 1759276800.0, "allow": False, "graft": {**graft, "applied": True}},
        {"at": 1759276800.5, "allow": True, "graft": {**graft, "applied": False, "why": "x"}},
        {"at": 1758499200, "allow": False, "graft": {**graft, "applied": True}},
        {"at": 1759103999.9, "allow": True},
    ]
    t40 = [
        turn_line("say hi", created_at="2025-09-30T08:00:00Z", elapsed_ms=812.25),
        turn_line(
            "design it",
            created_at="2025-10-01T08:00:00Z",
            tier="tier2-frontier",
            model="claude-opus-4-8",
            downgraded=False,
            actual=0.02,
            cf=0.02,
        ),
    ]
    t_old = [turn_line("say hi", created_at="2025-01-03T03:04:05Z")]
    measure = {
        turns: lines(t40 + t_old),
        dj: lines(week40 + week39),
        f"{audit}/s1.jsonl": lines(audit_rows[:2]),
        f"{audit}/a0.jsonl": lines(audit_rows[2:]),
        f"{grafts}/big-read.json": {"text": json.dumps(rule)},
        tier1: {"text": json.dumps({"model": "qwen", "base_url": "http://127.0.0.1:8080/v1"})},
    }
    add("router status measure", ["router", "status"], measure)
    add("router status measure json", ["router", "status", "--json"], measure)
    bad = [
        "not json",
        "[1]",
        "",
        "   ",
        row("2026-9-30T00:00:00Z"),
        row("2026-02-30T00:00:00Z"),
        row("\uff12\uff10\uff12\uff16-09-30T00:00:00Z"),
        row(" 2025-10-01T00:00:00Z"),
        row("2025-10-01T00:00:00"),
        row(1759276800),
        row(None),
        row(
            "2025-10-01T01:00:00Z",
            ok="yes",
            quotes=True,
            dropped=2**60,
            worker_input_tokens=-5,
            worker_output_tokens=2.5,
            frontier_tokens_kept=2**53,
            frontier_dollars_kept=float("nan"),
            worker_dollars=float("inf"),
            task_ok=1,
        ),
        row("2025-10-01T02:00:00Z", elapsed_ms=float("inf"), frontier_dollars_kept=-1.5),
        row("2025-10-01T03:00:00Z", worker_dollars=10**400, frontier_dollars_kept=10**400),
        row("2025-10-01T04:00:00Z", worker_dollars=2**60, quotes=-(2**53), dropped=2**53 + 1),
    ]
    bad_audit = [
        {"at": "1759276800", "graft": {"applied": True}},
        {"at": True, "graft": {"applied": True}},
        {"at": float("nan"), "graft": {"applied": True}},
        {"at": 1e300, "graft": {"applied": True}},
        {"at": -62135596800, "graft": {"applied": "yes"}},
        {"at": -62135596801, "graft": {"applied": True}},
        {"at": 253402300799.9, "graft": {"applied": True}},
        {"at": 1759276800, "graft": "x"},
        {"at": -0.5, "graft": {}},
    ]
    bad_turns = [
        turn_line("a", created_at="2025-10-01T00:00:00Z", frontier_tokens_saved=-1234567),
        turn_line("b", created_at="2025-10-01T00:00:00Z", estimated="yes", elapsed_ms="x"),
        turn_line("c", created_at="not a date"),
        "{",
    ]
    add(
        "router status measure bad rows",
        ["router", "status"],
        {turns: lines(bad_turns), dj: lines(bad), f"{audit}/s.jsonl": lines(bad_audit)},
    )
    add(
        "router status measure bad rows json",
        ["router", "status", "--json"],
        {turns: lines(bad_turns), dj: lines(bad), f"{audit}/s.jsonl": lines(bad_audit)},
    )
    many = [row(f"2025-{m:02d}-15T00:00:00Z") for m in range(1, 13)]
    add("router status measure many weeks", ["router", "status"], {dj: lines(many)})
    add("router status measure audit dir a file", ["router", "status"], {audit: {"text": "x"}})
    # The delegate rule and worker in force.
    for label, extra in {
        "audit rule": {f"{grafts}/big-read.json": {"text": json.dumps({**rule, "state": "audit"})}},
        "rule files": {
            f"{grafts}/a.json": {"text": json.dumps({**rule, "id": "a", "state": "retired"})},
            f"{grafts}/b.json": {"text": "{"},
            f"{grafts}/c.json": {"text": json.dumps({**rule, "shape": "x"})},
            f"{grafts}/d.json": {"text": json.dumps({**rule, "id": "d"})},
            f"{grafts}/e.json": {"text": json.dumps({**rule, "id": "e"})},
            f"{grafts}/f.txt": {"text": "x"},
        },
        "remote not allowed": {
            f"{grafts}/big-read.json": {"text": json.dumps(rule)},
            tier1: {
                "text": json.dumps({"model": "qwen", "base_url": "http://gpu.invalid:8080/v1"})
            },
        },
        "remote granted": {
            f"{grafts}/big-read.json": {
                "text": json.dumps({**rule, "worker": {"allow_remote": True}})
            },
            tier1: {
                "text": json.dumps({"model": "qwen", "base_url": "http://gpu.invalid:8080/v1"})
            },
            envf: {
                "text": json.dumps(
                    {
                        "generated_by": "t",
                        "task": "t",
                        "permissions": {"network": True, "network_hosts": ["gpu.invalid"]},
                    }
                )
            },
        },
        "envelope unreadable": {
            f"{grafts}/big-read.json": {"text": json.dumps(rule)},
            tier1: {"text": json.dumps({"model": "ollama/qwen"})},
            envf: {"text": "{"},
        },
        "physical": {
            tier1: {"text": json.dumps({"model": "ollama/qwen"})},
            envf: {
                "text": json.dumps(
                    {"generated_by": "t", "task": "t", "stakes": "physical", "permissions": {}}
                )
            },
        },
        "no wire": {tier1: {"text": json.dumps({"model": "mistral/x"})}},
    }.items():
        add(f"router status delegate {label}", ["router", "status"], extra)
        add(f"router status delegate {label} json", ["router", "status", "--json"], extra)


def build_cli_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    turns = ".opendaisugi/gateway/turns.jsonl"
    lex = {CONFIG: {"text": "matcher_model: lexical\n"}}

    def add(name: str, argv: list[str], before: dict | None = None, **kw) -> None:
        c = {"kind": "cli", "name": name, "argv": argv, "before": before or {}}
        c.update(kw)
        C.append(c)

    day = [
        turn_line("summarize the auth module"),
        turn_line("summarize the auth module", tokens=(50, 5, 1000, 20)),
        turn_line(
            "design the schema",
            tier="tier2-frontier",
            model="claude-opus-4-8",
            downgraded=False,
            actual=0.02,
            cf=0.02,
        ),
        turn_line("run the unit tests", tier="tier1-local", model="qwen", actual=0.0, cf=0.001),
        turn_line(
            "run the unit tests please", tier="tier1-local", model="qwen", actual=0.0, cf=0.001
        ),
        turn_line("", sig=""),
        turn_line("continuation", sig="", tokens=(7, 1, 3000, 0)),
    ]
    sy = [
        turn_line(
            "say hi",
            tier="tier-switchyard",
            model="llama3",
            requested="claude-opus-4-8",
            actual=0.0,
        ),
        turn_line(
            "say hi",
            tier="tier-switchyard",
            model="llama3",
            requested="claude-opus-4-8",
            actual=0.0,
        ),
        turn_line(
            "design x",
            tier="tier-switchyard",
            model="claude-sonnet-5",
            downgraded=False,
            actual=0.01,
            cf=0.01,
        ),
        turn_line(
            "odd", tier="tier-switchyard", model="unknown", downgraded=False, actual=0.01, cf=0.01
        ),
        turn_line(
            "Résumé ✓ " * 20,
            tier="tier-switchyard",
            model="a-very-long-target-model-name-here",
            downgraded=False,
        ),
    ]
    text = lambda lines: {"text": "".join(ln + "\n" for ln in lines)}  # noqa: E731
    # --- gateway-report ---
    add("report no journal", ["gateway-report"])
    add("report empty journal", ["gateway-report"], {turns: {"text": "\n\n"}})
    add("report a day", ["gateway-report"], {**lex, turns: text(day)})
    add(
        "report unsigned only default matcher",
        ["gateway-report"],
        {turns: text([turn_line("x", sig="")])},
    )
    add("report switchyard targets", ["gateway-report"], {**lex, turns: text(sy + day[:2])})
    add(
        "report bad lines",
        ["gateway-report"],
        {**lex, turns: text(day[:3] + ["{bad", "[1]", '"s"', "7", json.dumps({"task": "x"})])},
    )
    add(
        "report data dir",
        ["gateway-report", "--data-dir", "{HOME}/dd"],
        {**lex, "dd/gateway/turns.jsonl": text(day)},
    )
    add(
        "report unknown matcher",
        ["gateway-report"],
        {CONFIG: {"text": "matcher_model: bogus\n"}, turns: text(day)},
    )
    add(
        "report zero dollars",
        ["gateway-report"],
        {**lex, turns: text([turn_line("a", actual=0.0, cf=0.0)] * 2)},
    )
    add(
        "report big numbers",
        ["gateway-report"],
        {
            **lex,
            turns: text(
                [turn_line("big", tokens=(10**15, 10**12, 7, 3), actual=1234567.891, cf=9876543.21)]
                * 3
            ),
        },
    )
    add(
        "report ties and paraphrase",
        ["gateway-report"],
        {
            **lex,
            turns: text(
                [
                    turn_line("run the unit tests"),
                    turn_line("run the unit tests"),
                    turn_line("run the unit tests please"),
                    turn_line("deploy docs", actual=1.5),
                    turn_line("deploy docs", actual=2.25),
                ]
            ),
        },
    )
    add(
        "report float tokens",
        ["gateway-report"],
        {**lex, turns: text([turn_line("a", tokens=(1.5, 0, 0, 0))])},
        go_refuses=True,
    )
    add("report bad option", ["gateway-report", "--nope"])
    # --- route ---
    add("route easy", ["route", "fix the typo"])
    add("route hard", ["route", "design a distributed consensus algorithm"])
    add("route hard json", ["route", "refactor the scheduler for thread-safety", "--json"])
    add("route hard codex", ["route", "refactor the scheduler", "--harness", " Codex "])
    add(
        "route easy json models",
        ["route", "list files", "--json", "--cheap-model", "c", "--frontier-model", "f"],
    )
    add("route no task", ["route"])
    add("route bad threshold", ["route", "x", "--threshold", "abc"])
    add("route unknown matcher", ["route", "x"], {CONFIG: {"text": "matcher_model: bogus\n"}})
    from pathway_cases import lexical, pathway, put_row, row_spec

    store = {
        ".opendaisugi/pathways.db": {
            "db": {
                "rows": [
                    row_spec(
                        put_row(pathway(1, "run the unit tests", lexical("run the unit tests")))
                    )
                ]
            }
        }
    }
    add("route pathway hit", ["route", "run the unit tests"], {**lex, **store})
    add("route pathway hit json", ["route", "run the unit tests", "--json"], {**lex, **store})
    add("route pathway miss", ["route", "summarize the release notes"], {**lex, **store})
    add(
        "route pathway threshold",
        ["route", "run unit tests now", "--threshold", "0.99"],
        {**lex, **store},
    )
    add("route unicode", ["route", "ΣΑΣ résumé İstanbul " * 30, "--json"])
    # --- router status / stop ---
    add("router status nothing", ["router", "status"])
    add("router status nothing json", ["router", "status", "--json"])
    add(
        "router status configured",
        ["router", "status"],
        {CONFIG: {"text": SY_CFG}},
        switchyard=True,
    )
    add(
        "router status configured json",
        ["router", "status", "--json"],
        {CONFIG: {"text": SY_CFG + "switchyard_api_key_env: K\n"}},
        switchyard=True,
    )
    add(
        "router status version fails",
        ["router", "status"],
        {CONFIG: {"text": SY_CFG}},
        switchyard={"FAKE_SY_VERSION": "fail"},
    )
    state = (
        '{"pid": {PID1}, "host": "127.0.0.1", "port": {SYP}, "config_path": "{HOME}/.opendaisugi/gateway/'
        'switchyard-{SYP}.toml", "log_path": null, "route_id": "daisugi", "auth": {"capable": "forwards your own '
        'login, as the gateway does", "efficient": "no credential: a local host"}}'
    )
    live = {"ports": ["SYP"], "children": [{"port": "{SYP}"}]}
    add(
        "router status live child",
        ["router", "status"],
        {
            CONFIG: {"text": SY_CFG},
            ".opendaisugi/gateway/switchyard-{SYP}.json": {"text": state},
            turns: text(sy),
        },
        switchyard=True,
        **live,
    )
    add(
        "router status live child json",
        ["router", "status", "--json"],
        {
            CONFIG: {"text": SY_CFG},
            ".opendaisugi/gateway/switchyard-{SYP}.json": {"text": state},
            turns: text(sy * 3),
        },
        switchyard=True,
        **live,
    )
    add(
        "router status unhealthy child",
        ["router", "status"],
        {
            ".opendaisugi/gateway/switchyard-{SYP}.json": {
                "text": state.replace(
                    ', "auth": {"capable": "forwards your own login, as the gateway does", "efficient": "no credential: a local host"}',
                    ', "auth": {}',
                )
            }
        },
        switchyard=True,
        ports=["SYP"],
        children=[{"port": "{SYP}", "mode": "unhealthy"}],
    )
    add(
        "router status other program",
        ["router", "status"],
        {".opendaisugi/gateway/switchyard-4000.json": {"text": state.replace("{SYP}", "4000")}},
        switchyard=True,
        ports=["SYP"],
        children=[{"port": "4000", "other_program": True}],
    )
    add(
        "router status stale and unreadable",
        ["router", "status"],
        {
            ".opendaisugi/gateway/switchyard-4001.json": {
                "text": '{"pid": 999999999, "host": "127.0.0.1", "port": 4001}'
            },
            ".opendaisugi/gateway/switchyard-4002.json": {
                "text": '{"pid": "7", "host": "h", "port": 1}'
            },
            ".opendaisugi/gateway/switchyard-4003.json": {"text": "{nope"},
        },
        switchyard=True,
    )
    add(
        "router status stale json",
        ["router", "status", "--json"],
        {
            ".opendaisugi/gateway/switchyard-4001.json": {
                "text": '{"pid": 999999999, "host": "127.0.0.1", "port": 4001, '
                '"config_path": [1, "a"], "route_id": "", "auth": "x"}'
            },
            ".opendaisugi/gateway/switchyard-4003.json": {"text": "[]"},
        },
        switchyard=True,
    )
    add(
        "router status invalid config",
        ["router", "status"],
        {CONFIG: {"text": "gateway_router: [1]\n"}},
    )
    add(
        "router status data dir",
        ["router", "status", "--data-dir", "{HOME}/dd"],
        {"dd/config.yaml": {"text": "gateway_router: 'off'\n"}, "dd/gateway/turns.jsonl": text(sy)},
    )
    router_measure_cases(add, turns)
    add("router stop nothing", ["router", "stop"])
    add(
        "router stop live child",
        ["router", "stop"],
        {".opendaisugi/gateway/switchyard-{SYP}.json": {"text": state}},
        switchyard=True,
        **live,
    )
    add(
        "router stop other and stale",
        ["router", "stop"],
        {
            ".opendaisugi/gateway/switchyard-4000.json": {"text": state.replace("{SYP}", "4000")},
            ".opendaisugi/gateway/switchyard-4001.json": {
                "text": '{"pid": 999999999, "host": "127.0.0.1", "port": 4001}'
            },
            ".opendaisugi/gateway/switchyard-4003.json": {"text": "{nope"},
        },
        switchyard=True,
        ports=["SYP"],
        children=[{"port": "4000", "other_program": True}],
    )
    add("router bad command", ["router", "go"])
    return C


def build_recall_cases() -> list[dict[str, Any]]:
    from pathway_cases import envelope, lexical, pathway, put_row, row_spec

    from opendaisugi.pathway import PathwayParameter

    C: list[dict[str, Any]] = []
    db = "{HOME}/.opendaisugi/pathways.db"
    frozen = pathway(1, "run the unit tests", lexical("run the unit tests"))
    typed = pathway(
        2,
        "read the log file",
        lexical("read the log file"),
        parameters=[
            PathwayParameter(
                name="path",
                step_index=1,
                step_id="s2",
                field="path",
                head="/work",
                observed=["/work/out.txt"],
            )
        ],
    )
    store = {
        ".opendaisugi/pathways.db": {
            "db": {"rows": [row_spec(put_row(frozen)), row_spec(put_row(typed))]}
        }
    }
    empty = {".opendaisugi/pathways.db": {"db": {"rows": []}}}
    ok_env = envelope(9).model_dump(mode="json")
    no_shell = envelope(9, shell=False).model_dump(mode="json")
    narrow = envelope(9, file_read=["/other/**"]).model_dump(mode="json")

    def q(task: str, env: dict, **kw) -> dict[str, Any]:
        return {
            "kind": "recall",
            "db": db,
            "matcher": "lexical",
            "task": task,
            "envelope": env,
            "z3_timeout_ms": 500,
            **kw,
        }

    C.append(
        {
            "kind": "recall",
            "name": "recall frozen and typed",
            "before": {**store, CONFIG: {"text": "matcher_model: lexical\n"}},
            "queries": [
                q("run the unit tests", ok_env),
                q("run the unit tests", no_shell),
                q("run the unit tests", narrow),
                q("summarize the release notes", ok_env),
                q("read the log file", ok_env),
                q("read the log file", narrow),
                q("RUN   the unit tests!", ok_env),
            ],
        }
    )
    C.append(
        {
            "kind": "recall",
            "name": "recall empty store",
            "before": {**empty, CONFIG: {"text": "matcher_model: lexical\n"}},
            "queries": [q("run the unit tests", ok_env)],
        }
    )
    ans = ".opendaisugi/gateway/answers.jsonl"
    lines = [
        {
            "signature": "a1",
            "task": "explain the auth flow",
            "answer": "It uses OAuth.",
            "created_at": 1000.0,
            "ground_hash": None,
        },
        {
            "signature": "a2",
            "task": "what does parse do",
            "answer": "It parses.",
            "created_at": 5000.0,
            "ground_hash": "g1",
        },
        {"signature": "", "task": "ignored", "answer": "x", "created_at": 9000.0},
        {
            "signature": "a3",
            "task": "explain the auth flow please",
            "answer": "",
            "created_at": 9000.0,
        },
        {
            "signature": "a4",
            "task": "Base directory for this skill: /s\nlist the tables",
            "answer": "t1, t2",
            "created_at": 7000,
        },
    ]
    text = "".join(json.dumps(x) + "\n" for x in lines) + "{bad\n[1]\n"

    def a(task: str, now: float, **kw) -> dict[str, Any]:
        return {
            "kind": "answer",
            "answers": "{HOME}/" + ans,
            "matcher": "lexical",
            "task": task,
            "now": now,
            **kw,
        }

    lexcfg = {CONFIG: {"text": "matcher_model: lexical\n"}}
    C.append(
        {
            "kind": "recall",
            "name": "recall answers",
            "before": {ans: {"text": text}, **lexcfg},
            "queries": [
                a("explain the auth flow", 2000.0),
                a("explain the auth flow", 1000.0 + 7 * 86400 + 1),
                a("explain the auth flow", 2000.0, max_age_seconds=500.0),
                a("what does parse do", 6000.0, ground_hash="g1"),
                a("what does parse do", 6000.0, ground_hash="g2"),
                a("what does parse do", 6000.0),
                a("completely unrelated words here", 2000.0),
                a("list the tables", 8000.0),
                a("### Skill: q\nlist the tables", 8000.0),
            ],
        }
    )
    C.append(
        {
            "kind": "recall",
            "name": "recall answers empty",
            "before": {ans: {"text": "\n"}, **lexcfg},
            "queries": [a("explain", 1.0)],
        }
    )
    return C


def build_env_proxy_cases() -> list[dict[str, Any]]:
    """The upstream calls through a proxy the environment names, as httpx
    reads it, with a fake proxy on 127.0.0.1 ({PX}) that records what it
    got. {DEAD} is a port nothing listens on. No host a URL names is ever
    reached: the fake proxy sends a request on only to a loopback port or
    to the fake upstream, and a tunnel is never forwarded."""
    C: list[dict[str, Any]] = []

    def add(
        name: str,
        env: dict[str, str],
        steps: list[dict[str, Any]],
        table: list | None = None,
        mode: str = "forward",
        unique: bool = False,
        **kw,
    ) -> None:
        c = {
            "kind": "proxy",
            "name": "env proxy " + name,
            "steps": steps,
            "table": table or [],
            "env": env,
            "fake_proxy": {"mode": mode, **({"unique": True} if unique else {})},
        }
        c.update(kw)
        C.append(c)

    def req(body: str, **kw) -> dict[str, Any]:
        return {"req": {"body": body, "headers": kw.pop("headers", AUTH), **kw}}

    hard = msg_body("design a distributed consensus algorithm and prove its security")
    ok = [
        ["/v1/messages", reserialized(hard), json_answer(anthropic_reply(model="claude-opus-4-8"))]
    ]
    px = "http://127.0.0.1:{PX}"
    turn = [req(hard)]
    # Which variable applies, and which wins.
    add("http_proxy forwards", {"HTTP_PROXY": px}, turn, ok)
    add("lower case wins", {"http_proxy": px, "HTTP_PROXY": "http://127.0.0.1:{DEAD}"}, turn, ok)
    add("empty lower case removes it", {"http_proxy": "", "HTTP_PROXY": px}, turn, ok)
    add("mixed case name", {"Http_Proxy": px}, turn, ok)
    add("all_proxy without a scheme", {"ALL_PROXY": "127.0.0.1:{PX}"}, turn, ok)
    add(
        "http_proxy over all_proxy",
        {"HTTP_PROXY": px, "ALL_PROXY": "http://127.0.0.1:{DEAD}"},
        turn,
        ok,
    )
    add("https_proxy not for http", {"HTTPS_PROXY": px}, turn, ok)
    add("cgi drops HTTP_PROXY", {"HTTP_PROXY": px, "REQUEST_METHOD": "GET"}, turn, ok)
    add("cgi keeps http_proxy", {"http_proxy": px, "REQUEST_METHOD": "GET"}, turn, ok)
    add("two turns", {"HTTP_PROXY": px}, [req(hard), req(hard)], ok)
    s_hard = msg_body(
        "design a distributed consensus algorithm and prove its security", stream=True
    )
    add(
        "stream",
        {"HTTP_PROXY": px},
        [req(s_hard)],
        [
            [
                "/v1/messages",
                reserialized(s_hard),
                sse_answer(anthropic_events(model="claude-opus-4-8")),
            ]
        ],
    )
    # Credentials in the proxy URL.
    add("credentials", {"HTTP_PROXY": "http://us%65r:p%40ss@127.0.0.1:{PX}"}, turn, ok)
    add("user only", {"HTTP_PROXY": "http://user@127.0.0.1:{PX}"}, turn, ok)
    add("empty password", {"HTTP_PROXY": "http://u:@127.0.0.1:{PX}"}, turn, ok)
    add("empty credentials", {"HTTP_PROXY": "http://:@127.0.0.1:{PX}"}, turn, ok)
    # NO_PROXY: hosts, domains, a leading dot, *, IPs, ports, URLs.
    up = "http://127.0.0.1:{UP}"
    for no, upstream in [
        ("127.0.0.1", up),
        ("*", up),
        ("a.b, *", up),
        (" 127.0.0.1 ,x", up),
        ("127.0.0.1:{UP}", up),
        ("127.0.0.1:1", up),
        ("127.0.0.0/8", up),
        ("127.0.0.1/32", up),
        ("http://127.0.0.1", up),
        ("https://127.0.0.1", up),
        ("http://127.0.0.1:{UP}", up),
        ("localhost", "http://localhost:{UP}"),
        ("LOCALHOST", "http://localhost:{UP}"),
        ("localhost", "http://up.localhost:{UP}"),
        (".localhost", "http://localhost:{UP}"),
        (".localdomain", "http://localhost.localdomain:{UP}"),
        ("localdomain", "http://localhost.localdomain:{UP}"),
        ("localhost.localdomain", "http://localhost.localdomain:{UP}"),
        ("calhost", "http://localhost:{UP}"),
        ("::1", up),
        ("", up),
        (",", up),
    ]:
        add(
            f"no_proxy {no!r} to {upstream}",
            {"HTTP_PROXY": px, "NO_PROXY": no},
            turn,
            ok,
            upstream=upstream,
        )
    add(
        "lower case no_proxy wins",
        {"HTTP_PROXY": px, "no_proxy": "x", "NO_PROXY": "127.0.0.1"},
        turn,
        ok,
    )
    # HTTPS through a CONNECT tunnel. The fake proxy answers 200 and reads
    # the TLS handshake the client starts, so the turn fails on every side.
    add("https tunnel", {"HTTPS_PROXY": px}, turn, ok, upstream="https://api.localhost")
    add(
        "https tunnel port and creds",
        {"HTTPS_PROXY": "http://u:p@127.0.0.1:{PX}"},
        turn,
        ok,
        upstream="https://api.localhost:8443/base",
    )
    add("https tunnel to an ip", {"ALL_PROXY": px}, turn, ok, upstream="https://127.0.0.1:{UP}")
    add("https tunnel to ipv6", {"HTTPS_PROXY": px}, turn, ok, upstream="https://[::1]:{UP}")
    add("https upper case host", {"HTTPS_PROXY": px}, turn, ok, upstream="https://API.Localhost")
    add(
        "https no_proxy port",
        {"HTTPS_PROXY": px, "NO_PROXY": "api.localhost:443"},
        turn,
        ok,
        upstream="https://api.localhost",
    )
    add(
        "https no_proxy domain",
        {"HTTPS_PROXY": px, "NO_PROXY": "other.localhost,localhost"},
        turn,
        ok,
        upstream="https://localhost:{DEAD}",
    )
    add(
        "https refused connect",
        {"HTTPS_PROXY": px},
        turn,
        ok,
        mode="refuse",
        upstream="https://api.localhost",
    )
    # Failures: a proxy that denies, one nothing listens on, schemes httpx
    # cannot build a client for, and TLS to the proxy itself.
    add("proxy answers 407", {"HTTP_PROXY": px}, turn, ok, mode="deny")
    add("unreachable", {"HTTP_PROXY": "http://127.0.0.1:{DEAD}"}, turn, ok)
    add("socks refused even for http", {"HTTPS_PROXY": "socks5://127.0.0.1:{PX}"}, turn, ok)
    add("socks5h", {"ALL_PROXY": "socks5h://127.0.0.1:{PX}"}, turn, ok)
    add(
        "socks under no_proxy star",
        {"HTTPS_PROXY": "socks5://127.0.0.1:{PX}", "NO_PROXY": "*"},
        turn,
        ok,
    )
    add("unknown scheme", {"HTTP_PROXY": "ftp://127.0.0.1:{PX}"}, turn, ok)
    add("https scheme proxy", {"HTTP_PROXY": "https://127.0.0.1:{PX}"}, turn, ok)
    # The Switchyard child is reached through the proxy too: the health
    # probe (urllib) and each turn (httpx).
    sy_cfg = {CONFIG: {"text": SY_CFG}}
    easy = msg_body("say hi")
    add(
        "switchyard",
        {"HTTP_PROXY": px},
        [req(easy)],
        [
            [
                "/v1/messages",
                swapped(easy, "daisugi"),
                json_answer(
                    anthropic_reply(model="llama3"),
                    headers=[
                        ["content-type", "application/json"],
                        ["x-model-router-selected-model", "llama3"],
                    ],
                ),
            ]
        ],
        switchyard=True,
        argv=["--switchyard-port", "{SY}"],
        before=sy_cfg,
        unique=True,
    )
    return C


def build_all() -> list[dict[str, Any]]:
    cases = (
        build_proxy_cases()
        + build_switchyard_cases()
        + build_env_proxy_cases()
        + build_cli_cases()
        + build_recall_cases()
    )
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def body_id(case: dict[str, Any]) -> str:
    return case_id({k: v for k, v in case.items() if k not in ("id", "expect")})


def write_jsonl(path: Path, items: list[dict[str, Any]]) -> dict[str, Any]:
    for it in items:
        it.pop("id", None)
        it["id"] = body_id(it)
    items.sort(key=lambda c: c["id"])
    data = "".join(canonical_json(c) + "\n" for c in items).encode("utf-8")
    path.write_bytes(data)
    return {"count": len(items), "sha256": hashlib.sha256(data).hexdigest()}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument(
        "--fresh", action="store_true", help="rerun every case, not only new or changed ones"
    )
    ap.add_argument(
        "--redo",
        action="append",
        default=[],
        help="rerun the cases whose name holds this text, or whose recorded result holds "
        "it with --redo-in-expect; keep the rest",
    )
    ap.add_argument("--redo-in-expect", action="store_true")
    ap.add_argument(
        "--range", default="", help="A:B, rerun only redo cases whose index is in [A, B)"
    )
    args = ap.parse_args()
    lo, hi = 0, 1 << 30
    if args.range:
        a, b = args.range.split(":")
        lo, hi = int(a or 0), int(b or 1 << 30)
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = build_all()
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        where = (
            json.dumps(prev["expect"]) if prev is not None and args.redo_in_expect else c["name"]
        )
        redo = lo <= i < hi and any(r in where for r in args.redo)
        if prev is not None and not args.only and not redo:
            c["expect"] = prev["expect"]
            continue
        c["expect"] = run_case(c, PY_CLI, SCRATCH / "gen" / f"{i:04d}")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:12000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
