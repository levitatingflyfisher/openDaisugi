"""Run the sprig cases on the Go and the Rust sprig and compare them.

    python3 clients/sprig_compare.py --go DIR --rust DIR \\
        [--daisugi [LABEL=]PATH ...] [--only NAME] [--verbose] [--json out.json] \\
        [--max-refused N]

DIR holds the four binaries a sprig build makes: sprig, sprig-hook,
sprig-mcp and weave. The cases are in clients/sprig_cases.py. Pass the Go
DIR as --rust as well to check that the Go side gives the same results
twice, so that the masking below is complete.

Each case runs once per side, in a scratch root of its own: HOME, TMPDIR
and TMUX_TMPDIR in it, the work dir as the cwd, and PATH set to the
side's fake binaries, then /usr/bin:/bin. No other variable is passed,
except the case's own. Nothing real is called: claude is a fake that
answers from the case's script and records each call (argv, cwd and
stdin); the gate is a fake that rules by the case's rules and records each
payload; the API is a local HTTP server that answers from the case's
script and records each request (path, the four headers sprig sets, and
the body); a case marked tls serves it over https with a scratch CA made
by openssl, which the client trusts through SSL_CERT_FILE. A case marked real_gate runs only with --daisugi: once per
binary given, with that binary on PATH as daisugi, after `daisugi gate
init --workspace WORK` in the side's HOME.

What is compared, per case: the exit code, stdout and stderr; every fake
claude call, gate call and API request; and the files of the work dir and
the session dir after the run (path, mode and bytes). Before the compare,
the side's root becomes {ROOT}, the API port {PORT}, and SP-R-1 masks the
values that differ by process: each id in a session file (and the session
id in stderr and in gate payloads) is renamed by first appearance, so a
broken parentId link still shows; ts and latencyMs are masked, and the
API session header keeps only its shape.

A case is:

- agree: every compared part is the same.
- refused: the Rust side says "not ported".
- fail-open: the Rust side refused fewer gate calls than Go, or let a
  hook call through that Go blocked.
- disagree: any other difference.

Exit 1 on any disagree or fail-open, or more refused than --max-refused.
Each run's process group is killed after the run.
"""

from __future__ import annotations

import argparse
import difflib
import fcntl
import json
import os
import re
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from sprig_cases import CASES  # noqa: E402

FAKE_CLAUDE = r"""#!/usr/bin/python3
import json, os, re, sys, time
FDS = []
for n in range(3, 12):
    try:
        os.fstat(n)
        FDS.append(n)
    except OSError:
        pass
d = os.environ["SPRIG_FAKE"]
with open(os.path.join(d, "claude.json")) as f:
    replies = json.load(f)
n_path = os.path.join(d, "claude.n")
n = int(open(n_path).read()) if os.path.exists(n_path) else 0
with open(n_path, "w") as f:
    f.write(str(n + 1))
data = sys.stdin.buffer.read()
rec = {"argv": sys.argv[1:], "cwd": os.getcwd(), "stdin": data.decode("latin-1")}
rec["fds"] = FDS
rec["env"] = sorted(k for k in os.environ if re.match(os.environ.get("SPRIG_WATCH", "^$"), k))
tmp = os.path.join(os.path.dirname(os.path.join(d, "calls", "claude-%03d.json" % n)), "." + os.path.basename(os.path.join(d, "calls", "claude-%03d.json" % n)))
with open(tmp, "w") as f:
    json.dump(rec, f)
os.replace(tmp, os.path.join(d, "calls", "claude-%03d.json" % n))  # whole or absent: a kill mid-write leaves no torn record
if n >= len(replies):
    sys.stderr.write("fake claude: no reply left\n")
    sys.exit(1)
r = replies[n]
time.sleep(r.get("sleep", 0))
sys.stdout.buffer.write(r["raw"].encode("latin-1"))
sys.stderr.write("fake claude stderr is never shown\n")
sys.exit(r.get("exit", 0))
"""

FAKE_GATE = r"""#!/usr/bin/python3
import json, os, re, sys, time
FDS = []
for n in range(3, 12):
    try:
        os.fstat(n)
        FDS.append(n)
    except OSError:
        pass
d = os.environ["SPRIG_FAKE"]
data = sys.stdin.buffer.read()
calls = os.path.join(d, "calls")
n = len([x for x in os.listdir(calls) if x.startswith("gate-")])
rec = {"argv": sys.argv[1:], "cwd": os.getcwd(), "stdin": data.decode("latin-1")}
rec["fds"] = FDS
rec["env"] = sorted(k for k in os.environ if re.match(os.environ.get("SPRIG_WATCH", "^$"), k))
tmp = os.path.join(os.path.dirname(os.path.join(calls, "gate-%03d.json" % n)), "." + os.path.basename(os.path.join(calls, "gate-%03d.json" % n)))
with open(tmp, "w") as f:
    json.dump(rec, f)
os.replace(tmp, os.path.join(calls, "gate-%03d.json" % n))  # whole or absent: a kill mid-write leaves no torn record
with open(os.path.join(d, "gate.json")) as f:
    rules = json.load(f)
try:
    name = json.loads(data).get("tool_name")
except Exception:
    name = None
text = data.decode("latin-1")
for r in rules:
    if r["tool"] in ("*", name) and r["match"] in text:
        time.sleep(r["sleep"])
        sys.stdout.write(r["stdout"])
        sys.stderr.write(r["stderr"])
        sys.stdout.flush()
        sys.exit(r["exit"])
sys.exit(0)
"""

# The variables the fakes name in their log when they see them: the key
# names and the ones the cases scrub or keep.
WATCH = r"^(ANTHROPIC_|OPENAI_|SPRIG_API|SECRET_|KEEP_)"

# The fake tree command (daisugi tree run) of the spawn cases: it records
# its argv, stdin, the variables it sees and the size of what descriptor 3
# holds (never the bytes), then answers from the case's script.
FAKE_TREE = r"""#!/usr/bin/python3
import json, os, re, sys
FDS = []
for n in range(3, 12):
    try:
        os.fstat(n)
        FDS.append(n)
    except OSError:
        pass
fd3 = None
if 3 in FDS:
    chunks = []
    while True:
        b = os.read(3, 65536)
        if not b:
            break
        chunks.append(b)
    fd3 = "<KEYS:%d bytes>" % len(b"".join(chunks))
d = os.environ["SPRIG_FAKE"]
data = sys.stdin.buffer.read()
calls = os.path.join(d, "calls")
n = len([x for x in os.listdir(calls) if x.startswith("tree-")])
rec = {"argv": sys.argv[1:], "cwd": os.getcwd(), "stdin": data.decode("latin-1"),
       "fds": FDS, "fd3": fd3,
       "env": sorted(k for k in os.environ if re.match(os.environ.get("SPRIG_WATCH", "^$"), k))}
tmp = os.path.join(os.path.dirname(os.path.join(calls, "tree-%03d.json" % n)), "." + os.path.basename(os.path.join(calls, "tree-%03d.json" % n)))
with open(tmp, "w") as f:
    json.dump(rec, f)
os.replace(tmp, os.path.join(calls, "tree-%03d.json" % n))  # whole or absent: a kill mid-write leaves no torn record
with open(os.path.join(d, "tree.json")) as f:
    replies = json.load(f)
if n >= len(replies):
    sys.stderr.write("fake tree: no reply left\n")
    sys.exit(9)
r = replies[n]
if r.get("big"):
    try:
        sys.stdout.write("x" * r["big"])
        sys.stdout.flush()
    except BrokenPipeError:
        sys.exit(1)
sys.stdout.write(r.get("stdout", ""))
sys.stderr.write(r.get("stderr", ""))
sys.exit(r.get("exit", 0))
"""

# The fake MCP server (fakemcp) of the mcp cases: newline JSON-RPC on stdio.
# It records its argv, cwd, descriptors, watched variables and each message
# it gets (method and params), and its pids go to a file the driver reads
# after the run, so the compare can count the ones still alive. The case's
# script (mcp.json) says what each tool does.
FAKE_MCP = r"""#!/usr/bin/python3
import json, os, re, subprocess, sys, time
FDS = []
for n in range(3, 12):
    try:
        os.fstat(n)
        FDS.append(n)
    except OSError:
        pass
d = os.environ["SPRIG_FAKE"]
calls = os.path.join(d, "calls")
n = len([x for x in os.listdir(calls) if x.startswith("mcp-")])
path = os.path.join(calls, "mcp-%03d.json" % n)
with open(os.path.join(d, "mcp.json")) as f:
    script = json.load(f)
rec = {"argv": sys.argv[1:], "cwd": os.getcwd(), "fds": FDS, "msgs": [], "stdin": "",
       "pgrp_is_pid": os.getpgrp() == os.getpid(),
       "env": sorted(k for k in os.environ if re.match(os.environ.get("SPRIG_WATCH", "^$"), k))}
def save():
    tmp = os.path.join(os.path.dirname(path), "." + os.path.basename(path))
    with open(tmp, "w") as f:
        json.dump(rec, f)
    os.replace(tmp, path)  # whole or absent: a kill mid-write leaves no torn record
pids = [os.getpid()]
if script.get("child"):
    pids.append(subprocess.Popen(["sleep", "300"]).pid)
with open(os.path.join(d, "mcp-pids.txt"), "a") as f:
    f.write(" ".join(str(p) for p in pids) + "\n")
save()
def out(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    m = json.loads(line)
    rec["msgs"].append({"method": m.get("method"), "params": m.get("params"),
                        "reply": m.get("error")})
    save()
    if "id" not in m or "method" not in m:
        continue
    i = m["id"]
    if script.get("noise"):
        sys.stdout.write("not json at all\n[1, 2]\nnull\n"
                         + json.dumps({"jsonrpc": "2.0", "method": "notifications/message",
                                       "params": {"level": "info"}}) + "\n")
        sys.stdout.flush()
    if script.get("stray"):
        # Answers to other requests: a later id and an earlier one.
        for other in (i + 100, i - 1):
            out({"jsonrpc": "2.0", "id": other, "result": {"content": [
                {"type": "text", "text": "STALE"}], "tools": []}})
    if script.get("srv_request"):
        out({"jsonrpc": "2.0", "id": "srv-%d" % i, "method": "roots/list"})
    if m["method"] == "initialize":
        out({"jsonrpc": "2.0", "id": i, "result": {"protocolVersion": "2024-11-05",
             "capabilities": {"tools": {}}, "serverInfo": {"name": "fakemcp", "version": "1"}}})
    elif m["method"] == "tools/list":
        out({"jsonrpc": "2.0", "id": i, "result": {"tools": script["tools"]}})
    elif m["method"] == "tools/call":
        act = script["calls"].get(m["params"]["name"], {})
        if act.get("die"):
            sys.exit(3)
        if act.get("hang"):
            time.sleep(120)
        if act.get("big"):
            sys.stdout.write("x" * (33 << 20) + "\n")
            sys.stdout.flush()
            time.sleep(120)
        if act.get("flood"):
            # Requests to the client, and then no read of its replies.
            for k in range(act["flood"]):
                out({"jsonrpc": "2.0", "id": "f-%d" % k, "method": "roots/list"})
            time.sleep(120)
        if "rpc_error" in act:
            out({"jsonrpc": "2.0", "id": i, "error": {"code": -32000, "message": act["rpc_error"]}})
        elif act.get("echo"):
            out({"jsonrpc": "2.0", "id": i, "result": {"content": [
                {"type": "text", "text": json.dumps(m["params"]["arguments"], sort_keys=True)}]}})
        else:
            out({"jsonrpc": "2.0", "id": i, "result": {
                "content": act.get("content", [{"type": "text", "text": act.get("text", "")}]),
                "isError": bool(act.get("error"))}})
    else:
        out({"jsonrpc": "2.0", "id": i, "error": {"code": -32601, "message": "no"}})
"""

ID_KEYS = re.compile(r'"(id|parentId|toolUseId|leafId|session_id)":"([^"]*)"')
SESSION_LINE = re.compile(r"sprig: session (\S+) at ")
TS = re.compile(r'"(ts|latencyMs)":-?[0-9][0-9.eE+-]*')
API_SESSION = re.compile(r"^sprig-[0-9a-f]{16}$")


def key_shape(v: str, sentinel: str | None) -> str:
    """An auth header as the log keeps it: whether it holds the case's
    sentinel or the old fake key, never the value itself."""
    bearer = v.startswith("Bearer ")
    k = v[len("Bearer ") :] if bearer else v
    if sentinel and k == sentinel:
        k = "<sentinel>"
    elif k == "fake-key-not-real":
        k = "<fake-key>"
    else:
        k = f"<other:{len(k)} bytes>"
    return ("Bearer " if bearer else "") + k


class FakeAPI:
    """A scripted stand-in for the Messages API on 127.0.0.1."""

    def __init__(
        self, replies: list[dict], tls: ssl.SSLContext | None = None, sentinel: str | None = None
    ):
        self.replies = list(replies)
        self.requests: list[dict] = []
        # The placeholders a reply's Location header may name.
        self.ph: dict[str, str] = {}
        api = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n)
                heads = {}
                for h in (
                    "content-type",
                    "x-api-key",
                    "authorization",
                    "anthropic-version",
                    "x-opencode-session",
                ):
                    v = self.headers.get(h)
                    if h == "x-opencode-session" and v is not None:
                        v = "sprig-{SESSION}" if API_SESSION.match(v) else "BAD:" + v
                    if h in ("x-api-key", "authorization") and v is not None:
                        v = key_shape(v, sentinel)
                    if h == "authorization" and v is None:
                        continue
                    heads[h] = v
                try:
                    model = json.loads(body).get("model")
                except (ValueError, AttributeError):
                    model = None
                api.requests.append(
                    {
                        "method": self.command,
                        "path": self.path,
                        "model": model,
                        "headers": heads,
                        "body": body.decode("latin-1"),
                    }
                )
                if not api.replies:
                    self.send_response(599)
                    self.end_headers()
                    return
                r = api.replies.pop(0)
                raw = r["body"].encode("utf-8")
                if r.get("chunked"):
                    self.protocol_version = "HTTP/1.1"
                    self.send_response(r["status"])
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Transfer-Encoding", "chunked")
                    self.send_header("Connection", "close")
                    self.end_headers()
                    size = r.get("chunk", 7)
                    for i in range(0, len(raw), size):
                        part = raw[i : i + size]
                        self.wfile.write(b"%x\r\n%s\r\n" % (len(part), part))
                    self.wfile.write(b"0\r\n\r\n")
                    self.close_connection = True
                    return
                self.send_response(r["status"])
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                if r.get("location"):
                    self.send_header("Location", fill(r["location"], api.ph))
                self.end_headers()
                self.wfile.write(raw)

            # A redirect a client follows with GET (a 302) is logged too.
            do_GET = do_POST

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        if tls is not None:
            self.server.socket = tls.wrap_socket(self.server.socket, server_side=True)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def running(pid: int) -> bool:
    """Whether pid is a live process (a zombie is gone)."""
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except (OSError, IndexError):
        return False


def dead_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def fill(text: str, ph: dict[str, str]) -> str:
    for k, v in ph.items():
        text = text.replace("{" + k + "}", v)
    return text


def as_bytes(v: str | bytes) -> bytes:
    return v if isinstance(v, bytes) else v.encode("utf-8")


def write_file(path: Path, text: str | bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(as_bytes(text))


def tree(base: Path) -> list[list]:
    out: list[list] = []
    if not base.exists():
        return out
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames.sort()
        for name in sorted(dirnames + filenames):
            p = Path(dirpath) / name
            rel = str(p.relative_to(base))
            st = p.lstat()
            mode = oct(st.st_mode & 0o7777)
            if p.is_symlink():
                out.append([rel, "link", os.readlink(p)])
            elif p.is_dir():
                out.append([rel, "dir", mode])
            else:
                out.append([rel, "file", mode, p.read_bytes().decode("latin-1")])
    return out


def make_certs(d: Path) -> tuple[ssl.SSLContext, Path]:
    """A scratch CA and a leaf for 127.0.0.1 signed by it, for the https
    cases: the server context, and the CA file the clients trust."""
    d.mkdir(parents=True, exist_ok=True)
    ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes"]
    run = lambda *a: subprocess.run(["openssl", *a], cwd=d, check=True, capture_output=True)  # noqa: E731
    run(
        "req",
        "-x509",
        *ec,
        "-keyout",
        "ca.key",
        "-out",
        "ca.pem",
        "-days",
        "2",
        "-subj",
        "/CN=sprig-compare-ca",
    )
    run("req", *ec, "-keyout", "leaf.key", "-out", "leaf.csr", "-subj", "/CN=127.0.0.1")
    (d / "ext").write_text(
        "subjectAltName=IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n"
    )
    run(
        "x509",
        "-req",
        "-in",
        "leaf.csr",
        "-CA",
        "ca.pem",
        "-CAkey",
        "ca.key",
        "-CAcreateserial",
        "-out",
        "leaf.pem",
        "-days",
        "2",
        "-extfile",
        "ext",
    )
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(d / "leaf.pem", d / "leaf.key")
    return ctx, d / "ca.pem"


TLS: tuple[ssl.SSLContext, Path] | None = None


def run_side(case: dict, binary: Path, root: Path, daisugi: str | None) -> dict:
    """Run one case on one side and return what it observed, normalized."""
    if root.exists():
        shutil.rmtree(root)
    work, sess, fake, fakebin = root / "work", root / "sess", root / "fake", root / "fakebin"
    for d in (root / "home", root / "tmp", work, fake / "calls", fakebin):
        d.mkdir(parents=True)
    tls = TLS if case.get("tls") else None
    api = FakeAPI(case.get("api", []), tls[0] if tls else None, case.get("sentinel"))
    # A second server that a redirect points at. It must see no request.
    other = FakeAPI([], None, case.get("sentinel"))
    ph = {
        "OTHER": f"http://127.0.0.1:{other.port}",
        "OTHERHOST": f"http://localhost:{other.port}",
        "APIS": f"https://127.0.0.1:{api.port}",
        "CERT": str(tls[1]) if tls else "",
        "ROOT": str(root),
        "WORK": str(work),
        "SESS": str(sess),
        "API": f"http://127.0.0.1:{api.port}",
        "DEAD": f"http://127.0.0.1:{dead_port()}",
    }
    api.ph = ph
    try:
        for rel, text in case.get("files", {}).items():
            write_file(work / rel, text)
        for rel, text in case.get("sessions", {}).items():
            write_file(sess / rel, text)
        # The fake writes each reply's raw text back as latin-1, so carry
        # the exact bytes as latin-1 text.
        replies = [
            {**r, "raw": as_bytes(r["raw"]).decode("latin-1")} for r in case.get("claude", [])
        ]
        (fake / "claude.json").write_text(json.dumps(replies))
        (fake / "gate.json").write_text(json.dumps(case.get("gate", [])))
        (fake / "tree.json").write_text(json.dumps(case.get("tree", [])))
        (fake / "mcp.json").write_text(json.dumps(case.get("mcp", {})))
        fakes = case.get("fakes", ["claude", "fakegate"])
        if case.get("tree") is not None:
            fakes = [*fakes, "faketree"]
        if case.get("mcp") is not None:
            fakes = [*fakes, "fakemcp"]
        for name in fakes:
            src = {"claude": FAKE_CLAUDE, "faketree": FAKE_TREE, "fakemcp": FAKE_MCP}.get(
                name, FAKE_GATE
            )
            (fakebin / name).write_text(src)
            (fakebin / name).chmod(0o755)
        env = {
            "HOME": str(root / "home"),
            "PATH": f"{fakebin}:/usr/bin:/bin",
            "TMPDIR": str(root / "tmp"),
            "TMUX_TMPDIR": str(root / "tmp"),
            "SPRIG_FAKE": str(fake),
        }
        if daisugi:
            (fakebin / "daisugi").symlink_to(daisugi)
            subprocess.run(
                ["daisugi", "gate", "init", "--workspace", str(work)],
                env=env,
                cwd=work,
                capture_output=True,
                timeout=60,
                check=True,
            )
        for k, v in case.get("env", {}).items():
            env[k] = fill(v, ph)
        if case.get("pwd_link"):
            (root / "link").symlink_to(work)
            env["PWD"] = str(root / "link")
        argv = [str(binary / case["bin"])]
        argv += [a if isinstance(a, bytes) else fill(a, ph) for a in case["args"]]
        stdin = as_bytes(case.get("stdin") or b"")
        moves = []
        for num, text in case.get("fds", {}).items():
            r, w = os.pipe()
            os.write(w, as_bytes(text))
            os.close(w)
            high = fcntl.fcntl(r, fcntl.F_DUPFD_CLOEXEC, 100)
            os.close(r)
            moves.append((high, int(num)))

        def place_fds():
            for high, num in moves:
                os.dup2(high, num, inheritable=True)

        env["SPRIG_WATCH"] = WATCH
        try:
            proc = subprocess.Popen(
                argv,
                cwd=work,
                env=env,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=True,
                preexec_fn=place_fds if moves else None,
                # Popen closes every other descriptor after preexec_fn.
                pass_fds=tuple(num for _, num in moves),
            )
        finally:
            for high, _ in moves:
                os.close(high)
        try:
            out, err = proc.communicate(stdin, timeout=case.get("timeout", 40))
            code = proc.returncode
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            out, err = proc.communicate()
            code = "timeout"
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        # Each MCP server and what it started must be gone when sprig is.
        alive = 0
        pid_file = fake / "mcp-pids.txt"
        if pid_file.exists():
            pids = [int(x) for x in pid_file.read_text().split()]
            for _ in range(30):  # a killed process may need a moment to go
                alive = sum(1 for pid in pids if running(pid))
                if not alive:
                    break
                time.sleep(0.1)
            for pid in pids:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        calls = {}
        for p in sorted((fake / "calls").iterdir()):
            if p.name.startswith("."):  # a record a kill caught before its rename
                continue
            calls[p.name] = json.loads(p.read_text())
        seen = {
            "exit": code,
            "stdout": out.decode("latin-1"),
            "stderr": err.decode("latin-1"),
            "calls": calls,
            "mcp_alive": alive,
            "api": api.requests,
            "other": other.requests,
            "work": tree(work),
            "sess": tree(sess),
        }
        # The sentinel key may be only where the fake API matched it: no
        # output, call, request body or file may hold it.
        sentinel = case.get("sentinel")
        seen["leaks"] = (
            sorted(k for k, v in seen.items() if sentinel in json.dumps(v)) if sentinel else []
        )
    finally:
        api.close()
        other.close()
    return normalize(seen, root, api.port, other.port)


def normalize(seen: dict, root: Path, port: int, other: int = 0) -> dict:
    """SP-R-1: the side's paths, port and per-process values."""
    text = json.dumps(seen, sort_keys=True)
    for host in ("127.0.0.1", "localhost"):
        text = text.replace(f"{host}:{other}", host + ":{OTHER}")
    # The root as a JSON string fragment: escape it the way json.dumps does.
    for r in {str(root), str(root.resolve())}:
        text = text.replace(json.dumps(r)[1:-1], "{ROOT}")
    text = text.replace(f"127.0.0.1:{port}", "127.0.0.1:{PORT}")
    text = re.sub(
        r"127\.0\.0\.1:\d+",
        lambda m: m.group(0) if "{PORT}" in m.group(0) else "127.0.0.1:{DEAD}",
        text,
    )
    data = json.loads(text)
    ids: dict[str, str] = {}

    def name(v: str) -> str:
        if v not in ids:
            ids[v] = f"ID{len(ids) + 1}"
        return ids[v]

    def ids_in(s: str) -> str:
        # The key text is inside JSON that is itself a JSON string here, so
        # match both the plain and the escaped form.
        for m in SESSION_LINE.finditer(s):
            s = s.replace(m.group(1), name(m.group(1)))
        s = ID_KEYS.sub(lambda m: f'"{m.group(1)}":"{name(m.group(2))}"', s)
        return TS.sub(lambda m: f'"{m.group(1)}":"{{MASKED}}"', s)

    data["stderr"] = ids_in(data["stderr"])
    for entry in data["sess"]:
        if entry[1] == "file":
            entry[3] = ids_in(entry[3])
    # A default session id names the file too.
    for entry in data["sess"]:
        stem = entry[0].removesuffix(".jsonl")
        if stem in ids:
            entry[0] = ids[stem] + ".jsonl"
    for call in data["calls"].values():
        call["stdin"] = ids_in(call["stdin"])
    return data


def refusals(seen: dict) -> int:
    blob = seen["stdout"] + json.dumps(seen["sess"]) + json.dumps(seen["calls"])
    return blob.count("REFUSED by the gate") + blob.count('\\"decision\\":\\"deny\\"')


def classify(case: dict, go: dict, rs: dict) -> str:
    if go["leaks"] or rs["leaks"]:
        return "leak"
    # The second server is only a redirect's target: a request there means
    # a side followed the redirect.
    if go["other"] or rs["other"]:
        return "redirect"
    if go == rs:
        return "agree"
    if "not ported" in rs["stderr"]:
        return "refused"
    if case["bin"] == "sprig-hook" and go["exit"] == 2 and rs["exit"] == 0:
        return "fail-open"
    if refusals(rs) < refusals(go):
        return "fail-open"
    return "disagree"


def show_diff(go: dict, rs: dict) -> str:
    lines = []
    for key in go:
        if go[key] != rs.get(key):
            a = json.dumps(go[key], indent=1, sort_keys=True).splitlines()
            b = json.dumps(rs.get(key), indent=1, sort_keys=True).splitlines()
            lines.append(f"  -- {key}")
            lines.extend(
                "    " + x for x in difflib.unified_diff(a, b, "go", "rust", lineterm="", n=2)
            )
    return "\n".join(lines)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--go", required=True, type=Path)
    ap.add_argument("--rust", required=True, type=Path)
    ap.add_argument(
        "--daisugi",
        action="append",
        default=[],
        help="a real daisugi binary, as PATH or LABEL=PATH",
    )
    ap.add_argument("--only", action="append", default=[])
    ap.add_argument("--match", default="", help="run only the cases whose name holds this text")
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--json", type=Path)
    ap.add_argument("--max-refused", type=int, default=None)
    args = ap.parse_args()

    scratch = Path(tempfile.mkdtemp(prefix="sprig-compare-"))
    global TLS
    TLS = make_certs(scratch / "certs")
    runs = []
    for case in CASES:
        if case.get("real_gate"):
            for d in args.daisugi:
                label, _, path = d.rpartition("=")
                label = label or Path(path).name
                runs.append((f"{case['name']}@{label}", case, str(Path(path).resolve())))
        else:
            runs.append((case["name"], case, None))
    if args.only:
        runs = [r for r in runs if any(o == r[0] or r[0].startswith(o + "@") for o in args.only)]

    if args.match:
        runs = [r for r in runs if args.match in r[0]]

    counts: dict[str, int] = {}
    results = []
    try:
        for name, case, daisugi in runs:
            safe = re.sub(r"[^\w.-]", "_", name)
            go = run_side(case, args.go.resolve(), scratch / "side-go" / safe, daisugi)
            rs = run_side(case, args.rust.resolve(), scratch / "side-rs" / safe, daisugi)
            cls = classify(case, go, rs)
            counts[cls] = counts.get(cls, 0) + 1
            results.append({"case": name, "class": cls, "go": go, "rust": rs})
            if cls != "agree":
                print(f"{cls:9} {name}")
                if args.verbose:
                    print(show_diff(go, rs))
    finally:
        shutil.rmtree(scratch, ignore_errors=True)

    total = len(results)
    summary = ", ".join(f"{v} {k}" for k, v in sorted(counts.items()))
    print(f"{total} cases: {summary}")
    if args.json:
        args.json.write_text(json.dumps(results, indent=1))
    bad = counts.get("disagree", 0) + counts.get("fail-open", 0) + counts.get("leak", 0)
    if args.max_refused is not None and counts.get("refused", 0) > args.max_refused:
        bad += 1
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
