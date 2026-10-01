"""Synthetic stage-K3 cases: `daisugi mcp serve` (the MCP server over
stdio: the handshake, tools/list, each tool with good and bad input, and
the protocol's errors), `daisugi onboard` over synthetic transcripts, and
`daisugi tiers setup` with a fake hardware probe, run through the Python
oracle.

    uv run --no-sync python clients/k3_cases.py [--out clients/fixtures/k3] [--only NAME]

A case runs as garden_cases runs one: a scratch HOME, the fake `claude`
and the fake model server answering only exact request bytes, and the exit
code, stdout, stderr, the tree after and the model requests recorded. An
MCP case holds a session: the lines sent to the server's stdin, one at a
time. After each request the driver waits for its reply; after a line that
gets none, it sends a ping and waits for that, so every line's effect is
in stdout before the next is sent. Then stdin is closed and the exit code
read. The server's stderr is its SDK's log (width and time dependent) and
is not part of the protocol: it is not recorded, and an oracle server that
exits other than 0 fails the recording.

A fake `nvidia-smi` shadows the real one in every case, so no case probes
the box's GPU. Every path, key and task is synthetic.
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import re
import shutil
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import k2_cases  # noqa: E402, F401 - its normalization (run ids, receipts)
from garden_cases import PY_CLI, body_id, record_replies, run_case, write_jsonl  # noqa: E402
from pathway_cases import REPO  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "k3"
SCRATCH = Path(
    os.environ.get("DAISUGI_K3_SCRATCH") or Path.home() / "opendaisugi-scratch" / "k3" / "runs"
)
CASE_VERSION = 1
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
KEY = {"ANTHROPIC_API_KEY": "sk-test-k3-000000000000"}
API = {**KEY, "OPENDAISUGI_LLM_BACKEND": "api"}
CC = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}

# How long the driver waits for one reply. The oracle takes seconds to
# start; the wait costs nothing when the reply comes.
REPLY_WAIT = 180.0

# ---------------------------------------------------------------------------
# Normalization
# ---------------------------------------------------------------------------

# duration_ms in every form a line holds it: compact in structuredContent,
# and inside a text block (escaped once in the line, twice in the fixture).
_MS_ANY = re.compile(r'(duration_ms(?:\\)*":\s?)-?[0-9][0-9.eE+-]*')
_AGE_ANY = re.compile(r'(age_seconds(?:\\)*":\s?)-?[0-9][0-9.eE+-]*')
# The box's own CPU count and memory, which a fixture replayed elsewhere
# does not share.
_CPU_JSON = re.compile(r'("cpu_count\\?": )[0-9]+')
_RAM_JSON = re.compile(r'("ram_gb\\?": )[0-9.]+')
_CPU_RAM_TEXT = re.compile(r", [0-9]+ CPU, [0-9.]+GB RAM")
_HASH_ANY = re.compile(r'(evidence_hash(?:\\)*":\s?(?:\\)*")[0-9a-f]{64}')
_ELAPSED_ANY = re.compile(r'(elapsed_ms(?:\\)*":\s?)-?[0-9][0-9.eE+-]*')
_k2_normalize = garden_cases.normalize


def _source_hashes(result: dict[str, Any], home: str) -> dict[str, str]:
    """An ingested trace's id holds the SHA-256 of its transcript's path,
    which names the scratch HOME: each such hash, by the path it stands
    for."""
    import hashlib

    out = {}
    for rel in result.get("tree") or {}:
        if rel.endswith(".jsonl"):
            h = hashlib.sha256(f"{home}/{rel}".encode()).hexdigest()[:8]
            out["import-" + h + "-"] = "import-{SRC " + rel + "}-"
    return out


# The model-host probe's paths. Its requests go out with the HTTP client
# library's own user-agent and accept-encoding, which name the library,
# not the request (ruling SR-1); the rest is compared.
_PROBE_PATHS = ("/api/tags", "/api/show", "/v1/models")


def _probe_headers(result: dict[str, Any]) -> None:
    for r in result.get("requests") or []:
        if not isinstance(r, dict) or r.get("kind") != "http":
            continue
        probe = r.get("path") in _PROBE_PATHS or '"daisugi-probe"' in str(r.get("body", ""))
        if probe and isinstance(r.get("headers"), dict):
            for h in ("user-agent", "accept-encoding"):
                if h in r["headers"]:
                    r["headers"][h] = "{CLIENT}"


def normalize(result: dict[str, Any], home: str, t0: float) -> dict[str, Any]:
    _probe_headers(result)
    hashes = _source_hashes(result, home)
    out = _k2_normalize(result, home, t0)
    text = json.dumps(out, ensure_ascii=True)
    for h, name in hashes.items():
        text = text.replace(h, name)
    text = _MS_ANY.sub(r"\1{MS}", text)
    text = _ELAPSED_ANY.sub(r"\1{MS}", text)
    text = _HASH_ANY.sub(r"\1{HASH}", text)
    text = _AGE_ANY.sub(r"\1{AGE}", text)
    text = _CPU_JSON.sub(r"\1{CPU}", text)
    text = _RAM_JSON.sub(r"\1{RAM}", text)
    text = _CPU_RAM_TEXT.sub(", {CPU} CPU, {RAM}GB RAM", text)
    return json.loads(text)


garden_cases.normalize = normalize
_k2_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """k2_cases' lay_out, an "mtime" on an entry (seconds from the start),
    and an "sql" entry: statements run on a database once everything else
    is laid out."""
    _k2_lay_out({k: v for k, v in tree.items() if "sql" not in v}, home, t0)
    import sqlite3

    for rel, spec in sorted(tree.items()):
        if "mtime" in spec:
            at = t0 + spec["mtime"]
            os.utime(home / rel, (at, at), follow_symlinks=False)

    for rel, spec in sorted(tree.items()):
        if "sql" in spec:
            con = sqlite3.connect(home / rel)
            for stmt in spec["sql"]:
                con.execute(stmt)
            con.commit()
            con.close()


garden_cases.lay_out = lay_out

# ---------------------------------------------------------------------------
# Running a case: the fakes, and the MCP driver
# ---------------------------------------------------------------------------

FAKE_SMI = """#!/bin/sh
printf '%s' '{OUT}'
exit {EXIT}
"""


def write_fakes(case: dict[str, Any], work: Path) -> None:
    """Every case gets a fake nvidia-smi: the case's own ("smi": stdout and
    exit), or one that fails as a box with no GPU driver does."""
    smi = case.get("smi") or {"out": "", "exit": 9}
    text = FAKE_SMI.replace("{OUT}", smi["out"].replace("'", "'\\''")).replace(
        "{EXIT}", str(smi.get("exit", 0))
    )
    p = work / "bin" / "nvidia-smi"
    p.write_text(text, encoding="utf-8")
    p.chmod(0o755)


def drive(
    argv: list[str], env: dict[str, str], cwd: Path, session: list[dict[str, Any]], timeout: float
) -> tuple[int, str, str]:
    """Run an MCP server and speak a session to it. Returns the exit code,
    what it wrote to stdout (a missing reply as a {NO REPLY id} line) and
    its stderr."""
    p = subprocess.Popen(
        argv,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        cwd=cwd,
    )
    lines: queue.Queue[bytes | None] = queue.Queue()

    def read_out() -> None:
        assert p.stdout is not None
        for ln in p.stdout:
            lines.put(ln)
        lines.put(None)

    err: list[bytes] = []
    t_out = threading.Thread(target=read_out, daemon=True)
    t_err = threading.Thread(target=lambda: err.append(p.stderr.read()), daemon=True)  # type: ignore[union-attr]
    t_out.start()
    t_err.start()
    out: list[str] = []
    ended = False
    assert p.stdin is not None
    for item in session:
        raw = bytes.fromhex(item["hex"]) if "hex" in item else item["send"].encode("utf-8")
        try:
            p.stdin.write(raw)
            p.stdin.flush()
        except BrokenPipeError:
            out.append("{STDIN CLOSED}\n")
            break
        if "await" not in item:
            continue
        want = item["await"]
        deadline = time.time() + REPLY_WAIT
        got = False
        while not ended and time.time() < deadline:
            try:
                ln = lines.get(timeout=max(0.01, deadline - time.time()))
            except queue.Empty:
                break
            if ln is None:
                ended = True
                break
            out.append(ln.decode("utf-8", "replace"))
            try:
                msg = json.loads(ln)
            except ValueError:
                continue
            if isinstance(msg, dict) and "id" in msg and msg["id"] == want and "method" not in msg:
                got = True
                break
        if not got:
            out.append("{NO REPLY " + json.dumps(want) + "}\n")
    try:
        p.stdin.close()
    except BrokenPipeError:
        pass
    try:
        rc = p.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait()
        rc = -9
    t_out.join(10)
    while True:
        try:
            ln = lines.get_nowait()
        except queue.Empty:
            break
        if ln is not None:
            out.append(ln.decode("utf-8", "replace"))
    t_err.join(10)
    return rc, "".join(out), (err[0] if err else b"").decode("utf-8", "replace")


def invoke(
    case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, work: Path
) -> subprocess.CompletedProcess:
    write_fakes(case, work)
    port = env["ANTHROPIC_API_BASE"].rsplit(":", 1)[1]
    argv = [a.replace("{PORT}", port) for a in argv]
    if "session" not in case:
        return subprocess.run(
            argv,
            capture_output=True,
            env=env,
            cwd=cwd,
            timeout=case.get("timeout", 300),
            check=False,
        )
    home = env["HOME"]
    session = [
        {k: (v.replace("{HOME}", home) if k == "send" else v) for k, v in item.items()}
        for item in case["session"]
    ]
    rc, out, err = drive(argv, env, cwd, session, case.get("timeout", 120))
    if argv[: len(PY_CLI)] == PY_CLI and rc != 0:
        raise SystemExit(f"{case['name']}: the oracle's server exited {rc}:\n{err[-3000:]}")
    # A binary that does not carry the server says so on stderr, which is
    # then kept, so the compare reads it as not ported.
    kept = err if rc == 2 and "is not in this binary yet." in err else ""
    return subprocess.CompletedProcess(argv, rc, out.encode("utf-8"), kept.encode("utf-8"))


garden_cases.invoke = invoke


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# Building an MCP session
# ---------------------------------------------------------------------------

INIT = {
    "protocolVersion": "2025-06-18",
    "capabilities": {},
    "clientInfo": {"name": "k3", "version": "0"},
}


def line(msg: dict[str, Any]) -> str:
    return json.dumps(msg, separators=(",", ":"), ensure_ascii=False) + "\n"


class Session:
    """The lines of one MCP session. Requests get increasing ids; a line
    that gets no reply is followed by a ping, the barrier."""

    def __init__(self, init: bool = True, version: str = "2025-06-18"):
        self.items: list[dict[str, Any]] = []
        self.next_id = 1
        self.barriers = 0
        if init:
            self.req("initialize", {**INIT, "protocolVersion": version})
            self.note("notifications/initialized")

    def req(self, method: str, params: Any = None, *, rid: Any = None) -> Session:
        if rid is None:
            rid = self.next_id
            self.next_id += 1
        msg: dict[str, Any] = {"jsonrpc": "2.0", "id": rid, "method": method}
        if params is not None:
            msg["params"] = params
        self.items.append({"send": line(msg), "await": rid})
        return self

    def call(self, tool: str, arguments: Any = None, **kw: Any) -> Session:
        params: dict[str, Any] = {"name": tool}
        if arguments is not None:
            params["arguments"] = arguments
        params.update(kw)
        return self.req("tools/call", params)

    def barrier(self) -> Session:
        self.barriers += 1
        rid = f"b{self.barriers}"
        self.items.append(
            {"send": line({"jsonrpc": "2.0", "id": rid, "method": "ping"}), "await": rid}
        )
        return self

    def note(self, method: str, params: Any = None) -> Session:
        msg: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        self.items.append({"send": line(msg)})
        return self.barrier()

    def raw(self, text: str, reply: Any = "none") -> Session:
        """A line sent as it is: reply is the id it answers, or "none"."""
        if reply == "none":
            self.items.append({"send": text})
            return self.barrier()
        self.items.append({"send": text, "await": reply})
        return self

    def raw_bytes(self, data: bytes, reply: Any = "none") -> Session:
        if reply == "none":
            self.items.append({"hex": data.hex()})
            return self.barrier()
        self.items.append({"hex": data.hex(), "await": reply})
        return self

    def tail(self, text: str) -> Session:
        """Bytes sent last, with no barrier: stdin closes after them."""
        self.items.append({"send": text})
        return self


def mcp(name: str, s: Session, *, flags=(), before=None, env=None, **kw) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "mcp",
        "name": name,
        "argv": ["mcp", "serve", *flags],
        "session": s.items,
        "before": before or {},
    }
    if env:
        c["env"] = env
    c.update(kw)
    return c


# ---------------------------------------------------------------------------
# MCP: the protocol
# ---------------------------------------------------------------------------


def build_protocol_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    add(mcp("mcp initialize and list", Session().req("tools/list")))
    add(mcp("mcp initialize only", Session()))
    add(mcp("mcp no lines", Session(init=False)))
    add(mcp("mcp initialize older version", Session(version="2024-11-05")))
    add(mcp("mcp initialize latest version", Session(version="2025-11-25")))
    add(mcp("mcp initialize unknown version", Session(version="1999-01-01").req("tools/list")))
    add(
        mcp(
            "mcp initialize version int",
            Session(init=False).req("initialize", {**INIT, "protocolVersion": 5}).req("tools/list"),
        )
    )
    add(
        mcp(
            "mcp initialize without client info",
            Session(init=False)
            .req("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}})
            .req("ping"),
        )
    )
    add(
        mcp(
            "mcp initialize client info without version",
            Session(init=False).req("initialize", {**INIT, "clientInfo": {"name": "k3"}}),
        )
    )
    add(mcp("mcp initialize without params", Session(init=False).req("initialize")))
    add(mcp("mcp initialize twice", Session().req("initialize", INIT).req("tools/list")))
    add(
        mcp(
            "mcp request before initialize",
            Session(init=False)
            .req("tools/list")
            .req("ping")
            .req("initialize", INIT)
            .req("tools/list"),
        )
    )
    add(
        mcp(
            "mcp call before initialize",
            Session(init=False).req("tools/call", {"name": "pathway_stats"}),
        )
    )
    add(
        mcp(
            "mcp ping",
            Session().req("ping").req("ping", {}).req("ping", {"_meta": {"progressToken": 1}}),
        )
    )
    add(mcp("mcp unknown method", Session().req("nope").req("tools/lists", {})))
    add(mcp("mcp method not found", Session().req("logging/setLevel", {"level": "debug"})))
    add(mcp("mcp set level bad", Session().req("logging/setLevel", {"level": "loud"})))
    add(
        mcp(
            "mcp empty lists",
            Session()
            .req("resources/list")
            .req("resources/templates/list")
            .req("prompts/list", {"cursor": "x"}),
        )
    )
    # The requests the ports once answered with -32000 (K3-3).
    for name, method, params in [
        ("resources read", "resources/read", {"uri": "file:///x"}),
        ("resources read http", "resources/read", {"uri": "HTTP://Example.COM"}),
        ("resources read https port", "resources/read", {"uri": "https://a.b:443/c?q=1#f"}),
        ("resources read http port", "resources/read", {"uri": "http://a.b:8080"}),
        ("resources read custom", "resources/read", {"uri": "Custom:Thing/x"}),
        ("resources read mailto", "resources/read", {"uri": "mailto:A@B.c"}),
        ("resources read no scheme", "resources/read", {"uri": "notaurl"}),
        ("resources read empty host", "resources/read", {"uri": "http://"}),
        ("resources read empty", "resources/read", {"uri": ""}),
        ("resources subscribe bad uri", "resources/subscribe", {"uri": 5}),
        ("tasks list cursor", "tasks/list", {"cursor": "c"}),
        ("tasks get bad id", "tasks/get", {"taskId": 5}),
        (
            "completion complete context",
            "completion/complete",
            {
                "ref": {"type": "ref/prompt", "name": "p"},
                "argument": {"name": "a", "value": "b"},
                "context": {"arguments": {"x": "y"}},
            },
        ),
        (
            "completion complete no argument",
            "completion/complete",
            {"ref": {"type": "ref/prompt", "name": "p"}},
        ),
        ("resources read no uri", "resources/read", {}),
        ("resources read bad uri", "resources/read", {"uri": 5}),
        ("resources subscribe", "resources/subscribe", {"uri": "file:///x"}),
        ("resources unsubscribe", "resources/unsubscribe", {"uri": "file:///x"}),
        ("resources subscribe no params", "resources/subscribe", None),
        (
            "completion complete",
            "completion/complete",
            {"ref": {"type": "ref/prompt", "name": "p"}, "argument": {"name": "a", "value": "b"}},
        ),
        (
            "completion complete resource",
            "completion/complete",
            {
                "ref": {"type": "ref/resource", "uri": "file:///x"},
                "argument": {"name": "a", "value": ""},
            },
        ),
        ("completion complete bad", "completion/complete", {"ref": {"type": "x"}}),
        ("tasks get", "tasks/get", {"taskId": "t1"}),
        ("tasks list", "tasks/list", {}),
        ("tasks cancel", "tasks/cancel", {"taskId": "t1"}),
        ("tasks result", "tasks/result", {"taskId": "t1"}),
        ("tasks get no id", "tasks/get", {}),
    ]:
        add(mcp(f"mcp {name}", Session().req(method, params).req("ping")))
    for version in ("2025-06-18", "2025-11-25"):
        add(
            mcp(
                f"mcp task-augmented call {version}",
                Session(version=version)
                .call("find_pathway", {"task": "x"}, task={"ttl": 60000})
                .call("find_pathway", {"task": "x"}, task={})
                .call("find_pathway", {"task": "x"}, task={"ttl": None})
                .call("find_pathway", {"task": "x"}, task=5)
                .req("ping"),
            )
        )
    add(mcp("mcp tools list with cursor", Session().req("tools/list", {"cursor": "c"})))
    add(mcp("mcp tools list bad cursor", Session().req("tools/list", {"cursor": 5})))
    add(mcp("mcp prompts get unknown", Session().req("prompts/get", {"name": "x"})))
    add(mcp("mcp prompts get no name", Session().req("prompts/get", {})))
    add(
        mcp(
            "mcp cancelled as a request", Session().req("notifications/cancelled", {"requestId": 1})
        )
    )
    add(
        mcp(
            "mcp notifications",
            Session()
            .note("notifications/cancelled", {"requestId": 99})
            .note("notifications/progress", {"progressToken": 1, "progress": 1})
            .note("notifications/zzz")
            .note("notifications/roots/list_changed"),
        )
    )
    add(mcp("mcp not json", Session().raw("not json\n").raw("{\n")))
    add(mcp("mcp blank line", Session().raw("\n").raw("   \n")))
    add(mcp("mcp batch", Session().raw('[{"jsonrpc":"2.0","id":7,"method":"ping"}]\n')))
    add(mcp("mcp jsonrpc 1.0", Session().raw('{"jsonrpc":"1.0","id":7,"method":"ping"}\n')))
    add(mcp("mcp jsonrpc missing", Session().raw('{"id":7,"method":"ping"}\n')))
    add(mcp("mcp a scalar", Session().raw("7\n").raw('"x"\n').raw("null\n")))
    add(
        mcp(
            "mcp a response",
            Session()
            .raw('{"jsonrpc":"2.0","id":99,"result":{}}\n')
            .raw('{"jsonrpc":"2.0","id":98,"error":{"code":1,"message":"m"}}\n')
            .raw('{"jsonrpc":"2.0","id":97,"result":5}\n'),
        )
    )
    add(
        mcp(
            "mcp ids",
            Session()
            .raw('{"jsonrpc":"2.0","id":"s","method":"ping"}\n', "s")
            .raw('{"jsonrpc":"2.0","id":-4,"method":"ping"}\n', -4)
            .raw(
                '{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"ping"}\n',
                123456789012345678901234567890,
            )
            .raw('{"jsonrpc":"2.0","id":"","method":"ping"}\n', "")
            .raw('{"jsonrpc":"2.0","id":"é\\u2028","method":"ping"}\n', "é\u2028"),
        )
    )
    add(
        mcp(
            "mcp ids no reply",
            Session()
            .raw('{"jsonrpc":"2.0","id":true,"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":1.5,"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":5.0,"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":null,"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":[1],"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":1e2,"method":"ping"}\n'),
        )
    )
    add(
        mcp(
            "mcp duplicate keys",
            Session().raw('{"jsonrpc":"2.0","id":"6","method":"ping","id":7}\n', 7),
        )
    )
    add(
        mcp(
            "mcp params not an object",
            Session()
            .raw('{"jsonrpc":"2.0","id":3,"method":"tools/call","params":[1]}\n')
            .raw('{"jsonrpc":"2.0","id":4,"method":"ping","params":null}\n', 4)
            .raw('{"jsonrpc":"2.0","id":5,"method":"ping","params":"x"}\n'),
        )
    )
    add(mcp("mcp method not a string", Session().raw('{"jsonrpc":"2.0","id":3,"method":5}\n')))
    add(
        mcp(
            "mcp line endings",
            Session()
            .raw('{"jsonrpc":"2.0","id":9,"method":"ping"}\r\n', 9)
            .raw('{"jsonrpc":"2.0",\r"id":10,"method":"ping"}\n')
            .raw('{"jsonrpc":"2.0","id":11,"method":"ping"}\r', "none")
            .raw('\t{"jsonrpc" : "2.0" , "id" : 12 , "method" : "ping" }  \n', 12),
        )
    )
    add(
        mcp(
            "mcp not utf8",
            Session()
            .raw_bytes(b'{"jsonrpc":"2.0","id":"\xff\xfe","method":"ping"}\n', "\ufffd\ufffd")
            .raw_bytes(b'{"jsonrpc":"2.0","id":"\xe2\x82","method":"ping"}\n', "\ufffd")
            .raw_bytes(b'{"jsonrpc":"2.0","id":"\xc0\xaf","method":"ping"}\n', "\ufffd\ufffd"),
        )
    )
    add(
        mcp(
            "mcp escapes",
            Session()
            .raw(
                '{"jsonrpc":"2.0","id":"\\u00e9\\ud83d\\ude00\\t\\u0000","method":"ping"}\n',
                "é😀\t\x00",
            )
            .raw('{"jsonrpc":"2.0","id":"\\ud800","method":"ping"}\n'),
        )
    )
    add(
        mcp(
            "mcp deep nesting",
            Session().raw(
                '{"jsonrpc":"2.0","id":1,"method":"ping","x":' + "[" * 300 + "]" * 300 + "}\n"
            ),
        )
    )
    add(
        mcp(
            "mcp nan in the message",
            Session().raw('{"jsonrpc":"2.0","id":8,"method":"ping","x":NaN}\n', 8),
        )
    )
    add(
        mcp(
            "mcp closed stdin mid-request",
            Session().tail(
                '{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"verify_plan"'
            ),
        )
    )
    add(
        mcp(
            "mcp closed stdin after a whole line with no newline",
            Session().tail('{"jsonrpc":"2.0","id":12,"method":"ping"}'),
        )
    )
    add(mcp("mcp a long line", Session().req("ping", {"_meta": {"x": "y" * 200_000}})))
    # tools/call shapes.
    add(mcp("mcp call without name", Session().req("tools/call", {"arguments": {}})))
    add(mcp("mcp call name not a string", Session().req("tools/call", {"name": 5})))
    add(
        mcp(
            "mcp call arguments a list",
            Session().req("tools/call", {"name": "pathway_stats", "arguments": [1]}),
        )
    )
    add(
        mcp(
            "mcp call arguments null",
            Session().req("tools/call", {"name": "pathway_stats", "arguments": None}),
        )
    )
    add(mcp("mcp call arguments absent", Session().req("tools/call", {"name": "pathway_stats"})))
    add(
        mcp(
            "mcp call with meta",
            Session().req("tools/call", {"name": "pathway_stats", "_meta": {"progressToken": "t"}}),
        )
    )
    add(mcp("mcp unknown tool", Session().call("zzz", {})))
    add(mcp("mcp extra argument", Session().call("pathway_stats", {"extra": 1})))
    add(
        mcp(
            "mcp flags",
            Session().call("pathway_stats"),
            flags=("--data-dir", "{HOME}/d", "--model", "anthropic/m"),
        )
    )
    return C


# ---------------------------------------------------------------------------
# MCP: the tools
# ---------------------------------------------------------------------------

SONNET = "anthropic/claude-sonnet-4-20250514"


def env_dict(i: int = 1, **perm: Any) -> dict[str, Any]:
    from pathway_cases import envelope

    return envelope(i, **perm).model_dump(mode="json")


def reply_env(task: str = "t", **perm: Any) -> str:
    """A model's envelope reply: the fields a model writes."""
    p = {
        "file_read": ["/work/**"],
        "file_write": [],
        "network": False,
        "shell": False,
        "shell_allowlist": [],
        "max_execution_time_s": 30,
        "max_output_size_mb": 10,
    }
    p.update(perm)
    return json.dumps(
        {
            "generated_by": "model",
            "task": task,
            "permissions": p,
            "invariants": [],
            "postconditions": [],
        }
    )


def db_of(*pws) -> dict[str, Any]:
    from pathway_cases import put_row, row_spec

    return {".opendaisugi/pathways.db": {"db": {"rows": [row_spec(put_row(p)) for p in pws]}}}


def file_plan(path: str = "/work/a.txt", i: int = 1):
    from opendaisugi.models import ActionPlan, FileReadStep, ShellStep

    return ActionPlan(
        id=f"plan_{i:08x}",
        source="script",
        task=f"task {i}",
        steps=[
            ShellStep(id="s1", command="make test"),
            FileReadStep(id="s2", path=path, depends_on=["s1"]),
        ],
    )


def store_pathways() -> dict[str, Any]:
    from pathway_cases import lexical, pathway

    from opendaisugi.pathway import PathwayParameter

    typed = pathway(
        2,
        "read the log file",
        lexical("read the log file"),
        pl=file_plan(),
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
    frozen = pathway(1, "run the unit tests", lexical("run the unit tests"))
    return {**db_of(frozen, typed), **LEX}


def perm(**p: Any) -> dict[str, Any]:
    base: dict[str, Any] = {
        "file_read": [],
        "file_write": [],
        "network": False,
        "network_hosts": [],
        "shell": False,
        "shell_allowlist": [],
    }
    base.update(p)
    return base


def env_doc(i: int = 1, *, post=None, stakes=None, **p: Any) -> dict[str, Any]:
    d: dict[str, Any] = {
        "id": f"env_{i:08x}",
        "generated_by": "test",
        "task": f"task {i}",
        "permissions": perm(**p),
    }
    if post is not None:
        d["postconditions"] = post
    if stakes is not None:
        d["stakes"] = stakes
    return d


def plan_doc(steps: list[dict[str, Any]], i: int = 1) -> dict[str, Any]:
    return {"id": f"plan_{i:08x}", "source": "script", "task": f"task {i}", "steps": steps}


def sh(sid: str, cmd: str, deps=(), **kw: Any) -> dict[str, Any]:
    return {"id": sid, "type": "shell", "command": cmd, "depends_on": list(deps), **kw}


ECHO = env_doc(shell=True, shell_allowlist=["echo", "true", "false"])
W = "{HOME}/w"


def answers(*entries: dict[str, Any]) -> dict[str, Any]:
    lines = []
    for e in entries:
        text = json.dumps(
            {
                "signature": e.get("signature", "sig"),
                "task": e["task"],
                "answer": e.get("answer", "the answer"),
                "created_at": "{CREATED}",
                "ground_hash": e.get("ground_hash"),
            }
        )
        lines.append(text.replace('"{CREATED}"', "{NOWF:" + str(-e.get("age", 60)) + "}"))
    return {".opendaisugi/gateway/answers.jsonl": {"text": "\n".join(lines) + "\n"}}


def build_tool_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    store = store_pathways()
    # pathway_stats and list_pathways.
    add(
        mcp(
            "mcp list and stats",
            Session().call("list_pathways").call("pathway_stats"),
            before=store,
        )
    )
    add(mcp("mcp list empty", Session().call("list_pathways", {})))
    # find_pathway.
    add(
        mcp(
            "mcp find hit",
            Session().call("find_pathway", {"task": "run the unit tests"}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp find miss",
            Session().call("find_pathway", {"task": "summarize the release notes"}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp find twice",
            Session().call("find_pathway", {"task": "run the unit tests"}).call("pathway_stats"),
            before=store,
        )
    )
    add(mcp("mcp find no task", Session().call("find_pathway", {}), before=store))
    add(
        mcp(
            "mcp find task not a string",
            Session().call("find_pathway", {"task": ["x"]}),
            before=store,
        )
    )
    # recall.
    ok = env_dict(9)
    narrow = env_dict(9, file_read=["/work/a.txt"])
    add(
        mcp(
            "mcp recall frozen",
            Session().call("recall", {"task": "run the unit tests", "envelope": ok}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall miss",
            Session().call("recall", {"task": "summarize the release notes", "envelope": ok}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall typed bound",
            Session().call("recall", {"task": "read the log file", "envelope": ok}),
            before=store,
            env=CC,
            replies=[{"claude": json.dumps({"values": {"path": "/work/log.txt"}})}],
        )
    )
    add(
        mcp(
            "mcp recall typed model fails",
            Session().call("recall", {"task": "read the log file", "envelope": ok}),
            before=store,
            env=CC,
            replies=[{"claude_is_error": "down"}],
        )
    )
    add(
        mcp(
            "mcp recall fails verify",
            Session().call(
                "recall",
                {
                    "task": "run the unit tests",
                    "envelope": env_dict(9, shell=False, shell_allowlist=[]),
                },
            ),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall typed narrow",
            Session().call("recall", {"task": "read the log file", "envelope": narrow}),
            before=store,
            env=CC,
            replies=[{"claude": json.dumps({"values": {"path": "/work/log.txt"}})}],
        )
    )
    add(
        mcp(
            "mcp recall envelope invalid",
            Session().call("recall", {"task": "t", "envelope": {"task": "t"}}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall envelope as a string",
            Session().call("recall", {"task": "run the unit tests", "envelope": json.dumps(ok)}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall z3 not a number",
            Session().call("recall", {"task": "t", "envelope": ok, "z3_timeout_ms": "abc"}),
            before=store,
        )
    )
    add(
        mcp(
            "mcp recall z3 as a string",
            Session().call(
                "recall", {"task": "run the unit tests", "envelope": ok, "z3_timeout_ms": "800"}
            ),
            before=store,
        )
    )
    nan_env = json.dumps(ok).replace('"max_execution_time_s": 30', '"max_execution_time_s": NaN')
    add(
        mcp(
            "mcp recall nan in the envelope",
            Session().raw(
                '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall","arguments":'
                '{"task":"run the unit tests","envelope":' + nan_env + "}}}\n",
                2,
            ),
            before=store,
        )
    )
    # recall_answer.
    add(
        mcp(
            "mcp answer no store",
            Session().call("recall_answer", {"task": "what is the build command"}),
        )
    )
    ans = answers({"task": "what is the build command", "ground_hash": "g1"})
    add(
        mcp(
            "mcp answer hit",
            Session().call("recall_answer", {"task": "what is the build command"}),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer ground same",
            Session().call(
                "recall_answer", {"task": "what is the build command", "current_ground_hash": "g1"}
            ),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer ground changed",
            Session().call(
                "recall_answer", {"task": "what is the build command", "current_ground_hash": "g2"}
            ),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer too old",
            Session().call(
                "recall_answer", {"task": "what is the build command", "max_age_seconds": 10}
            ),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer max age as a string",
            Session().call(
                "recall_answer", {"task": "what is the build command", "max_age_seconds": "3600"}
            ),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer max age nan",
            Session().call(
                "recall_answer", {"task": "what is the build command", "max_age_seconds": "NaN"}
            ),
            before={**answers({"task": "what is the build command", "age": 10**7}), **LEX},
        )
    )
    add(
        mcp(
            "mcp answer max age infinity",
            Session().call(
                "recall_answer",
                {"task": "what is the build command", "max_age_seconds": "-Infinity"},
            ),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer dissimilar",
            Session().call("recall_answer", {"task": "draw a picture of a cat"}),
            before={**ans, **LEX},
        )
    )
    add(
        mcp(
            "mcp answer ground null string",
            Session().call(
                "recall_answer",
                {"task": "what is the build command", "current_ground_hash": "null"},
            ),
            before={**ans, **LEX},
        )
    )
    add(mcp("mcp answer task not a string", Session().call("recall_answer", {"task": 5})))
    # verify_plan.
    add(
        mcp(
            "mcp verify ok",
            Session().call(
                "verify_plan", {"plan": plan_doc([sh("s1", "echo hi")]), "envelope": ECHO}
            ),
        )
    )
    add(
        mcp(
            "mcp verify fails",
            Session().call(
                "verify_plan", {"plan": plan_doc([sh("s1", "rm -rf /work")]), "envelope": ECHO}
            ),
        )
    )
    add(
        mcp(
            "mcp verify cycle",
            Session().call(
                "verify_plan",
                {
                    "plan": plan_doc([sh("a", "echo a", ["b"]), sh("b", "echo b", ["a"])]),
                    "envelope": ECHO,
                },
            ),
        )
    )
    add(
        mcp(
            "mcp verify plan invalid",
            Session().call(
                "verify_plan", {"plan": {"steps": [{"id": "s1", "type": "warp"}]}, "envelope": ECHO}
            ),
        )
    )
    add(
        mcp(
            "mcp verify envelope invalid",
            Session().call("verify_plan", {"plan": plan_doc([]), "envelope": {"task": 5}}),
        )
    )
    add(
        mcp(
            "mcp verify as strings",
            Session().call(
                "verify_plan",
                {"plan": json.dumps(plan_doc([sh("s1", "echo hi")])), "envelope": json.dumps(ECHO)},
            ),
        )
    )
    add(mcp("mcp verify missing envelope", Session().call("verify_plan", {"plan": plan_doc([])})))
    add(
        mcp(
            "mcp verify high stakes",
            Session().call(
                "verify_plan",
                {
                    "plan": plan_doc([sh("s1", "echo hi")]),
                    "envelope": {**ECHO, "stakes": "high", "postconditions": [{"type": "custom"}]},
                },
            ),
        )
    )
    add(
        mcp(
            "mcp verify file scope",
            Session().call(
                "verify_plan",
                {
                    "plan": plan_doc([{"id": "r", "type": "file_read", "path": "/etc/passwd"}]),
                    "envelope": env_doc(file_read=["/work/**"]),
                },
            ),
        )
    )
    # verify_completed_step.
    exit0 = [{"type": "exit_code", "expected": 0}]
    # B4: stage 2 over ints past 2^63 (RF-14): Python compares them exactly.
    p63, p64 = 2**63, 2**64
    for name, n, pred in [
        ("2^63+5 equals 2^63-1", p63 + 5, {"op": "equals", "value": p63 - 1}),
        ("2^63-1 equals 2^63+5", p63 - 1, {"op": "equals", "value": p63 + 5}),
        ("2^64-1 in set 2^63", p64 - 1, {"op": "in_set", "values": [p63]}),
        ("2^63 not in set 2^64-1", p63, {"op": "not_in_set", "values": [p64 - 1]}),
        ("2^63 equals float 2^63", p63, {"op": "equals", "value": float(p63)}),
        ("2^64-1 range to 1e19", p64 - 1, {"op": "numeric_range", "min": 0, "max": 1e19}),
        ("2^64+5 equals 2^64", p64 + 5, {"op": "equals", "value": p64}),
    ]:
        post = [
            {
                "type": "n_rule",
                "expr": {"op": "forall_steps", "pred": {**pred, "path": "metadata.n"}},
            }
        ]
        add(
            mcp(
                f"mcp step big int {name}",
                Session().call(
                    "verify_completed_step",
                    {"step": sh("s1", "echo", metadata={"n": n}), "envelope": env_doc(post=post)},
                ),
            )
        )
    add(
        mcp(
            "mcp step no postconditions",
            Session().call("verify_completed_step", {"step": sh("s1", "echo"), "envelope": ECHO}),
        )
    )
    add(
        mcp(
            "mcp step missing permissions",
            Session().call(
                "verify_completed_step", {"step": sh("s1", "echo"), "envelope": {"task": "t"}}
            ),
        )
    )
    add(
        mcp(
            "mcp step exit code ok",
            Session().call(
                "verify_completed_step",
                {"step": sh("s1", "echo", metadata={"rc": 0}), "envelope": env_doc(post=exit0)},
            ),
        )
    )
    add(
        mcp(
            "mcp step exit code differs",
            Session().call(
                "verify_completed_step",
                {"step": sh("s1", "echo", metadata={"rc": 3}), "envelope": env_doc(post=exit0)},
            ),
        )
    )
    add(
        mcp(
            "mcp step exit code true",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo", metadata={"rc": True}),
                    "envelope": env_doc(post=[{"type": "exit_code", "expected": 1}]),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step exit code missing rc",
            Session().call(
                "verify_completed_step", {"step": sh("s1", "echo"), "envelope": env_doc(post=exit0)}
            ),
        )
    )
    add(
        mcp(
            "mcp step exit code missing expected",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo", metadata={"rc": 0}),
                    "envelope": env_doc(post=[{"type": "exit_code"}]),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step file exists",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo"),
                    "envelope": env_doc(
                        post=[
                            {"type": "file_exists", "path": f"{W}/a.txt"},
                            {"type": "file_exists", "path": f"{W}/none"},
                            {"type": "file_exists"},
                        ]
                    ),
                },
            ),
            before={"w/a.txt": {"text": "hello\n"}},
        )
    )
    add(
        mcp(
            "mcp step file size",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo"),
                    "envelope": env_doc(
                        post=[
                            {"type": "file_size_range", "path": f"{W}/a.txt", "min": 1, "max": 3},
                            {"type": "file_size_range", "path": f"{W}/a.txt", "min": 2},
                            {"type": "file_size_range", "path": f"{W}/a.txt"},
                            {"type": "file_size_range", "path": f"{W}/none", "max": 9},
                            {"type": "file_size_range", "max": 9},
                        ]
                    ),
                },
            ),
            before={"w/a.txt": {"text": "hello\n"}},
        )
    )
    add(
        mcp(
            "mcp step strict opaque",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo"),
                    "envelope": env_doc(
                        post=[{"type": "custom"}, {"type": "doc", "enforce": False}], stakes="high"
                    ),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step lenient opaque",
            Session().call(
                "verify_completed_step",
                {"step": sh("s1", "echo"), "envelope": env_doc(post=[{"type": "custom"}])},
            ),
        )
    )
    no_err = {
        "type": "clean_output",
        "description": "no ERROR in the output",
        "expr": {
            "op": "forall_steps",
            "pred": {"op": "not_matches", "path": "metadata.output", "regex": "ERROR"},
        },
    }
    add(
        mcp(
            "mcp step predicate holds",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo", metadata={"output": "fine"}),
                    "envelope": env_doc(post=[no_err]),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step predicate fails",
            Session().call(
                "verify_completed_step",
                {
                    "step": sh("s1", "echo", metadata={"output": "ERROR: x"}),
                    "envelope": env_doc(post=[no_err]),
                },
            ),
        )
    )
    # A postcondition that asks a model, an alias no registry resolves,
    # and an expr that is not a dict.
    judge = {"type": "judge", "expr": {"op": "llm_check", "rule": "is the output kind"}}
    yes = json.dumps({"satisfied": True, "rationale": "kind"})
    no = json.dumps({"satisfied": False, "rationale": "rude"})
    out_step = sh("s1", "echo", metadata={"output": "fine", "rc": 0})
    for name, replies, env in [
        ("holds", [{"http": yes}], API),
        ("not satisfied", [{"http": no}], API),
        ("not json", [{"http": "maybe"}], API),
        ("claude holds", [{"claude_raw": yes}], CC),
        ("claude fails", [], CC),
    ]:
        add(
            mcp(
                f"mcp step llm_check {name}",
                Session().call(
                    "verify_completed_step", {"step": out_step, "envelope": env_doc(post=[judge])}
                ),
                env=env,
                replies=replies or None,
            )
        )
    add(
        mcp(
            "mcp step llm_check physical",
            Session().call(
                "verify_completed_step",
                {"step": out_step, "envelope": env_doc(post=[judge], stakes="physical")},
            ),
            env=API,
        )
    )
    alias = {"op": "alias", "name": "no_secrets", "args": {}}
    add(
        mcp(
            "mcp step alias",
            Session().call(
                "verify_completed_step",
                {
                    "step": out_step,
                    "envelope": env_doc(
                        post=[
                            {"type": "named", "expr": alias},
                            {"type": "nested", "expr": {"op": "not", "child": alias}},
                        ]
                    ),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step expr not a dict",
            Session().call(
                "verify_completed_step",
                {
                    "step": out_step,
                    "envelope": env_doc(
                        post=[{"type": "s", "expr": "x"}, {"type": "l", "expr": [1]}]
                    ),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp verify llm_check invariant",
            Session().call(
                "verify_plan",
                {
                    "plan": plan_doc([sh("s1", "echo hi")]),
                    "envelope": {
                        **ECHO,
                        "invariants": [
                            {
                                "type": "judge",
                                "description": "d",
                                "expr": {"op": "llm_check", "rule": "is it kind"},
                            }
                        ],
                    },
                },
            ),
            env=API,
            replies=[{"http": no}],
        )
    )
    add(
        mcp(
            "mcp verify alias invariant",
            Session().call(
                "verify_plan",
                {
                    "plan": plan_doc([sh("s1", "echo hi")]),
                    "envelope": {
                        **ECHO,
                        "invariants": [{"type": "named", "description": "d", "expr": alias}],
                    },
                },
            ),
        )
    )
    add(
        mcp(
            "mcp step invalid",
            Session().call("verify_completed_step", {"step": {"id": "s1"}, "envelope": ECHO}),
        )
    )
    add(
        mcp(
            "mcp step task not a string",
            Session().call(
                "verify_completed_step", {"step": sh("s1", "echo"), "envelope": {**ECHO, "task": 7}}
            ),
        )
    )
    # run_plan.
    add(
        mcp(
            "mcp run dry",
            Session()
            .call(
                "run_plan",
                {
                    "plan": plan_doc([sh("s1", "echo hi"), sh("s2", "echo two", ["s1"])]),
                    "envelope": ECHO,
                },
            )
            .call("recent_runs"),
        )
    )
    add(
        mcp(
            "mcp run rejected",
            Session().call(
                "run_plan", {"plan": plan_doc([sh("s1", "rm -rf /work")]), "envelope": ECHO}
            ),
        )
    )
    add(
        mcp(
            "mcp run live allowlisted",
            Session().call(
                "run_plan",
                {"plan": plan_doc([sh("s1", "echo live")]), "envelope": ECHO, "dry_run": False},
            ),
        )
    )
    add(
        mcp(
            "mcp run live failed step",
            Session().call(
                "run_plan",
                {
                    "plan": plan_doc([sh("s1", "false"), sh("s2", "echo no", ["s1"])]),
                    "envelope": ECHO,
                    "dry_run": "false",
                },
            ),
        )
    )
    rw = env_doc(file_read=[f"{W}/**"], file_write=[f"{W}/**"])
    add(
        mcp(
            "mcp run live not approved",
            Session().call(
                "run_plan",
                {
                    "plan": plan_doc([{"id": "r", "type": "file_read", "path": f"{W}/a.txt"}]),
                    "envelope": rw,
                    "dry_run": False,
                },
            ),
            before={"w/a.txt": {"text": "one\n"}},
        )
    )
    add(
        mcp(
            "mcp run live approved by env",
            Session().call(
                "run_plan",
                {
                    "plan": plan_doc(
                        [
                            {
                                "id": "w1",
                                "type": "file_write",
                                "path": f"{W}/out.txt",
                                "content": "hi\n",
                            }
                        ]
                    ),
                    "envelope": rw,
                    "dry_run": False,
                },
            ),
            before={"w": {"dir": True}},
            env={"DAISUGI_APPROVE": "always"},
        )
    )
    add(
        mcp(
            "mcp run dry every kind",
            Session().call(
                "run_plan",
                {
                    "plan": plan_doc(
                        [
                            sh("s1", "echo a"),
                            {"id": "r", "type": "file_read", "path": f"{W}/a.txt"},
                            {"id": "n", "type": "network", "url": "https://example.com/x"},
                        ]
                    ),
                    "envelope": env_doc(
                        shell=True,
                        shell_allowlist=["echo"],
                        file_read=[f"{W}/**"],
                        network=True,
                        network_hosts=["example.com"],
                    ),
                },
            ),
        )
    )
    add(
        mcp(
            "mcp run plan invalid",
            Session().call("run_plan", {"plan": {"task": "t"}, "envelope": ECHO}),
        )
    )
    add(
        mcp(
            "mcp run dry run not a bool",
            Session().call(
                "run_plan", {"plan": plan_doc([]), "envelope": ECHO, "dry_run": "maybe"}
            ),
        )
    )
    add(
        mcp(
            "mcp run no steps", Session().call("run_plan", {"plan": plan_doc([]), "envelope": ECHO})
        )
    )
    # receipts_for_run and recent_runs.
    traces = {
        ".opendaisugi": {
            "journal": {
                "traces": [
                    {
                        "id": f"t{i}",
                        "task": f"task {i}",
                        "created_at": "{ISO:" + str(-86400 * i) + "}",
                        "envelope": env_doc(i, shell=True, shell_allowlist=["echo"]),
                        "plan": plan_doc([sh("s1", "echo hi")], i),
                    }
                    for i in (1, 2, 3)
                ]
            }
        }
    }
    add(
        mcp(
            "mcp recent runs",
            Session()
            .call("recent_runs")
            .call("recent_runs", {"limit": 1})
            .call("recent_runs", {"limit": "2"}),
            before=traces,
        )
    )
    add(mcp("mcp recent runs empty", Session().call("recent_runs", {"limit": 0})))
    add(
        mcp("mcp recent runs negative", Session().call("recent_runs", {"limit": -1}), before=traces)
    )
    receipts = {
        ".opendaisugi/journal/index.db": {
            "sql": [
                "INSERT INTO receipts (run_id, step_id, timestamp, evidence_hash, verify_result, verify_details, "
                f"evidence_json, model_id) VALUES ('run_00000001', 's{i}', {1_700_000_000.5 + i}, '{'ab' * 32}', "
                f"{i % 2}, 'details {i}', '{{\"rc\": 0}}', {'NULL' if i == 1 else repr('m' + str(i))})"
                for i in (2, 1)
            ]
        }
    }
    add(
        mcp(
            "mcp receipts",
            Session()
            .call("receipts_for_run", {"run_id": "run_00000001"})
            .call("receipts_for_run", {"run_id": "run_x"}),
            before={**traces, **receipts},
        )
    )
    add(mcp("mcp receipts no run id", Session().call("receipts_for_run", {})))
    # envelope_for.
    T = "Read /work/a.txt and count its lines"
    add(
        mcp(
            "mcp envelope ok",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env=API,
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope cached",
            Session().call("envelope_for", {"task": T}).call("envelope_for", {"task": T}),
            before=LEX,
            env=API,
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope high stakes",
            Session().call("envelope_for", {"task": T, "stakes": "high"}),
            before=LEX,
            env=API,
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope context",
            Session().call("envelope_for", {"task": T, "context": "the file is small"}),
            before=LEX,
            env=API,
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope context null string",
            Session().call("envelope_for", {"task": T, "context": "null"}),
            before=LEX,
            env=API,
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope low stakes",
            Session().call("envelope_for", {"task": T, "stakes": "low"}),
            before=LEX,
            env=API,
        )
    )
    add(
        mcp(
            "mcp envelope bad stakes",
            Session().call("envelope_for", {"task": T, "stakes": "huge"}),
            before=LEX,
            env=API,
        )
    )
    add(
        mcp(
            "mcp envelope empty task",
            Session().call("envelope_for", {"task": "  "}),
            before=LEX,
            env=API,
        )
    )
    add(
        mcp(
            "mcp envelope task too long",
            Session().call("envelope_for", {"task": "x" * 4001}),
            before=LEX,
            env=API,
        )
    )
    add(
        mcp(
            "mcp envelope claude code",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env=CC,
            replies=[{"claude": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope model fails",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env=API,
            replies=[{"http": "not json"}, {"http": "still not"}, {"http": "{}"}, {"http": "[]"}],
        )
    )
    add(
        mcp(
            "mcp envelope model error",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env=API,
            replies=[{"http_status": 500, "body": '{"error": "down"}'}] * 4,
        )
    )
    add(
        mcp(
            "mcp envelope no key",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env={"OPENDAISUGI_LLM_BACKEND": "api"},
        )
    )
    add(
        mcp(
            "mcp envelope other model",
            Session().call("envelope_for", {"task": T}),
            before=LEX,
            env=API,
            flags=("--model", "anthropic/claude-haiku-4-5"),
            replies=[{"http": reply_env(T)}],
        )
    )
    add(
        mcp(
            "mcp envelope tier0",
            Session().call("envelope_for", {"task": "run the unit tests"}),
            before=store,
            env=API,
        )
    )
    return C


# ---------------------------------------------------------------------------
# tiers setup
# ---------------------------------------------------------------------------


def smi(mib: str, name: str = "Fake GPU") -> dict[str, Any]:
    return {"out": f"{mib}, {name}\n", "exit": 0}


def setup(name: str, *flags: str, **kw: Any) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": ["tiers", "setup", *flags],
        "before": {},
    }
    c.update(kw)
    return c


def remote_cases(add: Any) -> None:
    """`tiers setup --remote`: probe a model host on the fake server and
    record it. The fake answers each request in turn: a GET by its path,
    a POST by its body."""

    def ok(body: Any) -> dict[str, Any]:
        return {"http_status": 200, "body": body if isinstance(body, str) else json.dumps(body)}

    nf = {"http_status": 404, "body": '{"error": "not found"}'}
    tags = ok({"models": [{"name": "llama3.2:3b"}, {"name": "qwen:7b"}, {"x": 1}]})
    show = ok(
        {
            "modelfile": "FROM x\nPARAMETER num_ctx 8192\n",
            "model_info": {"general.arch": "llama", "llama.context_length": 131072},
        }
    )
    show_info = ok({"model_info": {"general.arch": "llama", "llama.context_length": 65536}})
    show_params = ok({"parameters": "stop <x>\nnum_ctx 40960", "model_info": {}})
    remote = ("--remote", "127.0.0.1:{PORT}")
    cases = [
        ("setup remote ollama", (), [tags, show]),
        ("setup remote ollama auto", ("--kind", "auto"), [tags, show_info]),
        ("setup remote ollama parameters", ("--kind", "ollama"), [tags, show_params]),
        ("setup remote ollama show fails", ("--kind", "ollama"), [tags, nf]),
        ("setup remote ollama show not json", ("--kind", "ollama"), [tags, ok("nope")]),
        ("setup remote ollama no models", ("--kind", "ollama"), [ok({"models": []})]),
        (
            "setup remote ollama model and context",
            ("--kind", "ollama", "--model", "mine:1b", "--context", "4096"),
            [tags, show],
        ),
        ("setup remote ollama tags not a list", ("--kind", "ollama"), [ok({"models": "x"})]),
        (
            "setup remote openai",
            (),
            [
                nf,
                ok(
                    {
                        "data": [
                            {"id": "m-a", "max_model_len": 32768},
                            {"id": "m-b", "context_length": 9},
                        ]
                    }
                ),
            ],
        ),
        ("setup remote openai kind", ("--kind", "openai"), [ok({"data": [{"id": "m-a"}]})]),
        (
            "setup remote openai context key order",
            ("--kind", "openai"),
            [ok({"data": [{"id": "m", "n_ctx_train": 4096, "context_window": 50000}]})],
        ),
        ("setup remote openai empty", ("--kind", "openai"), [ok({"data": []})]),
        (
            "setup remote anthropic",
            (),
            [nf, nf, ok({"type": "message", "model": "local-claude", "content": []})],
        ),
        (
            "setup remote anthropic echo",
            ("--kind", "anthropic"),
            [ok({"type": "message", "model": "daisugi-probe"})],
        ),
        ("setup remote anthropic not message", ("--kind", "anthropic"), [ok({"type": "error"})]),
        ("setup remote unknown", (), [nf, nf, nf]),
        ("setup remote unknown kind answered", ("--kind", "openai"), [nf]),
    ]
    for name, flags, replies in cases:
        add(setup(name, *remote, *flags, smi=smi("8192"), replies=replies))
    add(
        setup(
            "setup remote ollama keeps config",
            *remote,
            smi=smi("8192"),
            replies=[tags, show],
            before={CONFIG: {"text": "matcher_model: lexical\nz3_timeout_ms: 900\n"}},
        )
    )
    add(setup("setup remote scheme", "--remote", "http://box:11434", smi=smi("8192")))
    add(setup("setup remote bad kind", "--remote", "box:1", "--kind", "grpc", smi=smi("8192")))
    add(setup("setup remote unreachable auto", "--remote", "127.0.0.1:1", smi=smi("8192")))
    add(
        setup(
            "setup remote bad context",
            "--remote",
            "127.0.0.1:1",
            "--context",
            "big",
            smi=smi("8192"),
        )
    )


def build_setup_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    for mib in ("2048", "6144", "8192", "16384", "24576", "49152"):
        add(setup(f"setup gpu {mib}", smi=smi(mib)))
        add(setup(f"setup gpu {mib} json", "--json", smi=smi(mib)))
    add(setup("setup gpu two lines", smi={"out": "12288, First\n4096, Second\n", "exit": 0}))
    add(setup("setup gpu fraction", "--json", smi=smi("12000.5", "Odd GPU é")))
    add(setup("setup gpu name with a comma", smi=smi("8192", "GPU, rev 2")))
    add(
        setup(
            "setup endpoint and remote",
            "--endpoint",
            "http://127.0.0.1:{PORT}/v1",
            "--remote",
            "h:1",
            smi=smi("8192"),
        )
    )
    add(setup("setup remote", "--remote", "127.0.0.1:1", "--kind", "ollama", smi=smi("8192")))
    remote_cases(add)
    add(
        setup(
            "setup endpoint without model",
            "--endpoint",
            "http://127.0.0.1:{PORT}/v1",
            smi=smi("8192"),
        )
    )
    add(setup("setup bad threshold", "--threshold", "high", smi=smi("8192")))
    add(setup("setup bad repeats", "--repeats", "x", smi=smi("8192")))
    good = [{"chat": reply_env("t")}] * 3
    oai = {"OPENDAISUGI_LLM_BACKEND": "api", "OPENAI_API_KEY": "sk-test-k3-openai-0000"}
    ep = ("--endpoint", "http://127.0.0.1:{PORT}/v1", "--model", "local-m")
    add(setup("setup endpoint passes", *ep, smi=smi("8192"), replies=good, env=oai))
    add(setup("setup endpoint passes wire", *ep, "--wire", smi=smi("8192"), replies=good, env=oai))
    add(
        setup(
            "setup endpoint passes wire json",
            *ep,
            "--wire",
            "--json",
            smi=smi("8192"),
            env=oai,
            replies=good,
        )
    )
    add(
        setup(
            "setup endpoint fails",
            *ep,
            "--wire",
            smi=smi("8192"),
            env=oai,
            replies=[
                {"chat": "not json"},
                {"chat": "no"},
                {"chat": reply_env("t")},
                {"chat": "{}"},
                {"chat": "x"},
            ],
        )
    )
    add(
        setup(
            "setup endpoint low threshold",
            *ep,
            "--threshold",
            "0.3",
            "--repeats",
            "0",
            "--json",
            smi=smi("8192"),
            env=oai,
            replies=[
                {"chat": reply_env("t")},
                {"chat": "no"},
                {"chat": "no"},
                {"chat": "no"},
                {"chat": "no"},
            ],
        )
    )
    add(
        setup(
            "setup endpoint down",
            *ep,
            smi=smi("8192"),
            env=oai,
            replies=[{"http_status": 500, "body": "{}"}] * 6,
        )
    )
    add(
        setup(
            "setup endpoint claude code",
            *ep,
            smi=smi("8192"),
            env=CC,
            replies=[{"claude": reply_env("t")}] * 3,
        )
    )
    add(
        setup(
            "setup endpoint prefixed model",
            "--endpoint",
            "http://127.0.0.1:{PORT}/v1",
            "--model",
            "openai/m2",
            "--wire",
            "--data-dir",
            "{HOME}/d",
            smi=smi("8192"),
            env=oai,
            replies=good,
        )
    )
    # --matcher: potion is the one-step opt-in (DS-3); lexical is the default.
    add(setup("setup matcher potion", "--matcher", "potion", "--data-dir", "{HOME}/d"))
    add(setup("setup matcher lexical again", "--matcher", "lexical"))
    add(setup("setup matcher bogus", "--matcher", "bogus"))
    add(setup("setup matcher empty", "--matcher", ""))
    add(
        setup(
            "setup matcher over a config",
            "--matcher",
            "potion",
            before={CONFIG: {"text": "matcher_model: all-MiniLM-L6-v2\ngate_mode: enforce\n"}},
        )
    )
    add(
        setup(
            "setup matcher over a bad config",
            "--matcher",
            "potion",
            before={CONFIG: {"text": "z3_timeout_ms: lots\n"}},
        )
    )
    C.append({"kind": "cli", "name": "setup moved", "argv": ["setup"], "before": {}})
    C.append(
        {"kind": "cli", "name": "setup moved with flags", "argv": ["setup", "--json"], "before": {}}
    )
    C.extend(build_tiers_stats_cases())
    C.extend(build_viz_cases())
    C.extend(build_lora_export_cases())
    return C


def build_lora_export_cases() -> list[dict[str, Any]]:
    """`lora export`: the journal's successful traces as training JSONL."""

    def tr(i: int, task: str, days_ago: float = 1, **kw: Any) -> dict[str, Any]:
        t: dict[str, Any] = {
            "id": f"t{i}",
            "task": task,
            "created_at": "{ISO:" + str(int(-86400 * days_ago)) + "}",
            "envelope": env_doc(i, shell=True, shell_allowlist=["echo"]),
            "plan": plan_doc([sh("s1", "echo hi")], i),
        }
        t.update(kw)
        return t

    traces = [
        tr(1, "Run the unit tests and report"),
        tr(2, "  short  "),
        tr(3, "Read the log file é and summarize it", 5),
        tr(4, "A failed one that is long enough", ok=False),
        tr(5, "Gone trace with a long enough task"),
        tr(6, "An old one that is long enough", 40),
    ]
    jt = {".opendaisugi": {"journal": {"traces": traces, "remove_yaml": ["t5"]}}}

    def le(name: str, *flags: str, before: Any = jt) -> dict[str, Any]:
        c: dict[str, Any] = {
            "kind": "cli",
            "name": name,
            "argv": ["lora", "export", *flags],
            "before": before or {},
        }
        if before is jt:
            # t5's YAML is gone on purpose: the compare's read-back guard
            # (k3_compare) cannot load this journal, whoever wrote it.
            c["guard"] = False
        return c

    return [
        le("lora export alpaca", "out.jsonl"),
        le("lora export chat", "out.jsonl", "--format", "chat"),
        le(
            "lora export chat system",
            "sub/dir/out.jsonl",
            "--format",
            "chat",
            "--system-prompt",
            "Be brief.",
        ),
        le("lora export days", "out.jsonl", "--days", "2"),
        le("lora export min chars", "out.jsonl", "--min-task-chars", "3"),
        le("lora export bad format", "out.jsonl", "--format", "csv"),
        le("lora export empty journal", "out.jsonl", before=None),
        le("lora export no output", before=None),
        le("lora export bad days", "out.jsonl", "--days", "x", before=None),
    ]


def build_viz_cases() -> list[dict[str, Any]]:
    """`daisugi viz`: a distilled pathway's plan as a standalone page."""
    from pathway_cases import envelope, lexical, pathway

    from opendaisugi.models import (
        ActionPlan,
        FileReadStep,
        FileWriteStep,
        MCPStep,
        NetworkStep,
        ShellStep,
        SkillStep,
        TaskStep,
    )

    wide = ActionPlan(
        id="plan_00000003",
        source="script",
        task="task 3",
        steps=[
            ShellStep(id="s1", command="make test"),
            FileReadStep(id="s2", path="/work/out.txt"),
            NetworkStep(id="s3", url="https://example.com/x", depends_on=["s1"]),
            MCPStep(id="s4", server="fs", tool="read", arguments={"p": 1}),
            TaskStep(id="s5", prompt="summarize </script><b>it</b> é", depends_on=["s2", "s3"]),
            SkillStep(id="s6", skill_id="sk-1", depends_on=["s5"]),
            FileWriteStep(id="s7", path="/etc/x", content="y", depends_on=["s1"]),
            ShellStep(id="s8", command="rm -rf /", depends_on=["s5", "s7"]),
        ],
    )
    pw_wide = pathway(
        3,
        "a wide plan with every kind of step, and a task description that runs long",
        lexical("a wide plan"),
        pl=wide,
        env=envelope(3, network=True, network_allowlist=["example.com"], mcp_allowlist=["fs/*"]),
    )
    frozen = pathway(1, "run the unit tests", lexical("run the unit tests"))
    store = db_of(frozen, pw_wide)

    def viz(name: str, *flags: str, before: dict[str, Any] | None = None) -> dict[str, Any]:
        return {"kind": "cli", "name": name, "argv": ["viz", *flags], "before": before or {}}

    return [
        viz("viz list", before=store),
        viz("viz list empty", before=db_of()),
        viz("viz no store"),
        viz("viz wide", "pw_0003", before=store),
        viz("viz frozen out", "pw_0001", "-o", "page.html", before=store),
        viz("viz output long flag", "pw_0001", "--output", "p2.html", before=store),
        viz("viz unknown id", "pw_9999", before=store),
        viz("viz output dir missing", "pw_0001", "-o", "no/such/dir.html", before=store),
        viz(
            "viz data dir",
            "pw_0003",
            "--data-dir",
            "{HOME}/.opendaisugi",
            "-o",
            "{HOME}/w.html",
            before=store,
        ),
    ]


def build_tiers_stats_cases() -> list[dict[str, Any]]:
    """`tiers stats`: per-tier counts over the journal's successful traces."""

    def tr(i: int, gb: str, days_ago: float = 1, **kw: Any) -> dict[str, Any]:
        env = env_doc(i, shell=True, shell_allowlist=["echo"])
        env["generated_by"] = gb
        t: dict[str, Any] = {
            "id": f"t{i}",
            "task": f"task {i}",
            "created_at": "{ISO:" + str(int(-86400 * days_ago)) + "}",
            "envelope": env,
            "plan": plan_doc([sh("s1", "echo hi")], i),
        }
        t.update(kw)
        return t

    traces = [
        tr(1, "compiled-pathway:pw1"),
        tr(2, "compiled-pathway:pw2", 2),
        tr(3, "tier1:llamafile"),
        tr(4, "tier1:llamafile", 3),
        tr(5, "tier1:ollama"),
        tr(6, "tier1:"),
        tr(7, "anthropic/claude-sonnet"),
        tr(8, "distilled"),
        tr(9, "tier1:skipped-bad", ok=False),
        tr(10, "tier1:skipped-failed", run_status="failed", run_id="r10"),
        tr(11, "compiled-pathway:old", 40),
        tr(12, "tier1:ran", run_status="succeeded", run_id="r12"),
        tr(13, "tier1:gone"),
    ]
    jt = {".opendaisugi": {"journal": {"traces": traces, "remove_yaml": ["t13"]}}}
    one = {".opendaisugi": {"journal": {"traces": [tr(1, "tier1:x")]}}}

    def ts(name: str, *flags: str, before: dict[str, Any] | None = None) -> dict[str, Any]:
        return {
            "kind": "cli",
            "name": name,
            "argv": ["tiers", "stats", *flags],
            "before": before or {},
        }

    return [
        ts("tiers stats empty"),
        ts("tiers stats empty json", "--json"),
        ts("tiers stats", before=jt),
        ts("tiers stats json", "--json", before=jt),
        ts("tiers stats window 2", "--days", "2", before=jt),
        ts("tiers stats window 2 json", "--days", "2", "--json", before=jt),
        ts("tiers stats window 100", "--days", "100", before=jt),
        ts("tiers stats window 0", "--days", "0", before=jt),
        ts("tiers stats one tier1", before=one),
        ts("tiers stats data dir", "--data-dir", "{HOME}/.opendaisugi", before=jt),
        ts("tiers stats other data dir", "--data-dir", "{HOME}/elsewhere"),
        ts("tiers stats bad days", "--days", "x"),
    ]


# ---------------------------------------------------------------------------
# onboard
# ---------------------------------------------------------------------------


def jrow(**kw: Any) -> str:
    return json.dumps(kw, ensure_ascii=False)


def asst(*blocks: dict[str, Any]) -> str:
    return jrow(type="assistant", message={"role": "assistant", "content": list(blocks)})


def tu(name: str, **inp: Any) -> dict[str, Any]:
    return {"type": "tool_use", "id": "toolu_x", "name": name, "input": inp}


def user(text: Any) -> str:
    return jrow(type="user", message={"role": "user", "content": text}, uuid="u", sessionId="S")


def cc_transcript(*turns: tuple[str, list[dict[str, Any]]]) -> str:
    lines = []
    for text, tools in turns:
        lines.append(user(text))
        lines.append(asst(*tools))
    return "\n".join(lines) + "\n"


CC_ONE = cc_transcript(
    (
        "Run the unit tests",
        [tu("Bash", command="make test"), tu("Read", file_path="/work/out.txt")],
    ),
    (
        "Show the git log",
        [tu("Bash", command="git log --oneline"), tu("Bash", command="git status")],
    ),
)
CC_TWO = cc_transcript(
    (
        "Review and file an issue",
        [
            tu("Agent", prompt="review the diff"),
            tu("Skill", skill="deslop", args="tighten"),
            tu("mcp__github__create_issue", title="t"),
            tu("Write", file_path="/work/notes.md", content="x"),
        ],
    ),
    (
        "Fetch the docs",
        [
            tu("WebFetch", url="https://example.com/doc"),
            tu("Bash", command="cd /work && ls | wc -l"),
        ],
    ),
)
CODEX = (
    "\n".join(
        [
            jrow(type="session_meta", payload={"id": "s"}),
            jrow(type="event_msg", payload={"type": "user_message", "message": "Run the tests"}),
            jrow(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "shell",
                    "arguments": json.dumps({"command": ["bash", "-lc", "pytest -q"]}),
                },
            ),
            jrow(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "shell",
                    "arguments": json.dumps({"command": ["git", "status"]}),
                },
            ),
        ]
    )
    + "\n"
)
BIG = cc_transcript(
    ("Tidy the repo", [tu("Bash", command=f"echo step{i}") for i in range(5)]),
)


def ob(name: str, *flags: str, before=None, env=None, **kw: Any) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": ["onboard", *flags],
        "before": {**LEX, **(before or {})},
    }
    if env:
        c["env"] = env
    c.update(kw)
    return c


def build_onboard_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    P = ".claude/projects/proj"
    three = {
        f"{P}/one.jsonl": {"text": CC_ONE, "mtime": -300},
        f"{P}/two.jsonl": {"text": CC_TWO, "mtime": -200},
        ".codex/sessions/2026/01/c.jsonl": {"text": CODEX, "mtime": -100},
    }
    m1 = ("--min-tools", "1")
    # A tend that clusters asks a model with a prompt that names the
    # scratch HOME; tend itself is the garden cases' (garden_cases.py).
    nt = ("--min-traces", "99")
    add(ob("onboard nothing", env=CC))
    add(ob("onboard nothing json", "--json", env=CC))
    add(ob("onboard nothing dry", "--dry-run", env=CC))
    add(ob("onboard dry run", "--dry-run", *m1, before=three, env=CC))
    add(ob("onboard dry run json", "--dry-run", "--json", *m1, before=three, env=CC))
    add(ob("onboard dry run merged", "--dry-run", before=three, env=CC))
    add(
        ob(
            "onboard dry run decomposition",
            "--dry-run",
            "--allow-shell-decomposition",
            *m1,
            before=three,
            env=CC,
        )
    )
    add(
        ob(
            "onboard dry run quiet",
            "--dry-run",
            *m1,
            before=three,
            env=CC,
            argv=["-q", "onboard", "--dry-run", *m1],
        )
    )
    add(ob("onboard real", *m1, *nt, before=three, env=CC))
    add(ob("onboard real json", "--json", *m1, *nt, before=three, env=CC))
    add(ob("onboard real api", *m1, *nt, before=three, env=API))
    add(
        ob(
            "onboard real no decomposition",
            *nt,
            "--no-allow-shell-decomposition",
            *m1,
            before={
                **three,
                CONFIG: {"text": "matcher_model: lexical\nshell_allow_decomposition: true\n"},
            },
            env=CC,
        )
    )
    add(ob("onboard harness codex", "--harness", "codex", *m1, before=three, env=CC))
    add(
        ob(
            "onboard harness two",
            "--harness",
            "codex",
            "--harness",
            "claude-code",
            "--dry-run",
            *m1,
            before=three,
            env=CC,
        )
    )
    add(ob("onboard harness none match", "--harness", "pi", before=three, env=CC))
    add(ob("onboard limit 1", "--limit", "1", "--dry-run", *m1, before=three, env=CC))
    add(ob("onboard limit 0", "--limit", "0", before=three, env=CC))
    add(ob("onboard limit negative", "--limit", "-1", "--dry-run", *m1, before=three, env=CC))
    add(ob("onboard bad limit", "--limit", "x", env=CC))
    add(ob("onboard bad llm", "--llm", "gpt", env=CC))
    add(ob("onboard llm flag", "--llm", "claude-code", "--dry-run", *m1, before=three))
    add(
        ob(
            "onboard skips",
            "--dry-run",
            *m1,
            before={
                f"{P}/journal.jsonl": {"text": CC_ONE, "mtime": -50},
                f"{P}/empty.jsonl": {"text": "", "mtime": -40},
                f"{P}/notes.txt": {"text": CC_ONE},
                f"{P}/dir.jsonl/x.jsonl": {"text": CC_TWO, "mtime": -30},
                f"{P}/.hidden.jsonl": {"text": CODEX, "mtime": -20},
            },
            env=CC,
        )
    )
    add(
        ob(
            "onboard symlink counted once",
            "--dry-run",
            *m1,
            before={
                f"{P}/one.jsonl": {"text": CC_ONE, "mtime": -300},
                "other/link.jsonl": {"link": f"{{HOME}}/{P}/one.jsonl"},
            },
            env={**CC, "OPENDAISUGI_TRANSCRIPT_ROOTS": "{HOME}/other"},
        )
    )
    add(
        ob(
            "onboard env roots",
            "--dry-run",
            *m1,
            before={
                "a/x.jsonl": {"text": CC_ONE, "mtime": -300},
                "b/y.jsonl": {"text": CC_TWO, "mtime": -200},
                "c/z.jsonl": {"text": CODEX, "mtime": -100},
                "d/w.jsonl": {"text": CC_ONE, "mtime": -50},
            },
            env={
                **CC,
                "OPENDAISUGI_TRANSCRIPT_ROOTS": " {HOME}/a : claude-code={HOME}/b ::codex=~/c: =d ",
            },
        )
    )
    add(
        ob(
            "onboard relative root skipped trace",
            *nt,
            *m1,
            before={
                "t/a.jsonl": {"text": CC_ONE, "mtime": -300},
                ".opendaisugi": {
                    "journal": {
                        "traces": [
                            {
                                "id": "import-"
                                + __import__("hashlib").sha256(b"t/a.jsonl").hexdigest()[:8]
                                + "-ep-0",
                                "task": "Run the unit tests",
                                "created_at": "{ISO:-86400}",
                                "envelope": env_doc(shell=True, shell_allowlist=["make"]),
                                "plan": plan_doc([sh("s1", "make test")]),
                            }
                        ]
                    }
                },
            },
            env={**CC, "OPENDAISUGI_TRANSCRIPT_ROOTS": "claude-code=t"},
        )
    )
    add(
        ob(
            "onboard relative root claude code",
            *m1,
            before={"t/a.jsonl": {"text": CC_ONE, "mtime": -300}},
            env={**CC, "OPENDAISUGI_TRANSCRIPT_ROOTS": "claude-code=t"},
        )
    )
    split = [
        {
            "claude": json.dumps(
                {
                    "subtasks": [
                        {"start_index": 0, "end_index": 1, "task": "first"},
                        {"start_index": 2, "end_index": 4, "task": "second"},
                    ]
                }
            )
        }
    ]
    big = {f"{P}/big.jsonl": {"text": BIG, "mtime": -100}}
    add(
        ob(
            "onboard split",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before=big,
            env=CC,
            replies=split,
        )
    )
    add(
        ob(
            "onboard split cached in the run",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before={**big, f"{P}/big2.jsonl": {"text": BIG, "mtime": -50}},
            env=CC,
            replies=split,
        )
    )
    add(
        ob(
            "onboard split api",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before=big,
            env=API,
            replies=[{"http": json.loads(split[0]["claude"]) and split[0]["claude"]}],
        )
    )
    add(
        ob(
            "onboard split fails",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before=big,
            env=CC,
            replies=[{"claude_is_error": "down"}],
        )
    )
    add(
        ob(
            "onboard split no subtasks",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before=big,
            env=CC,
            replies=[{"claude": "{}"}],
        )
    )
    add(
        ob(
            "onboard split api error",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            before=big,
            env=API,
            replies=[{"http_status": 400, "body": '{"error": "bad"}'}],
        )
    )
    add(
        ob(
            "onboard dry too large",
            "--max-tools",
            "2",
            "--min-tools",
            "1",
            "--dry-run",
            before={**big, **three},
            env=CC,
        )
    )
    add(
        ob(
            "onboard dry too large json",
            "--max-tools",
            "2",
            "--dry-run",
            "--json",
            before={**big, **three},
            env=CC,
        )
    )
    add(ob("onboard data dir", "--data-dir", "{HOME}/dd", *m1, *nt, before=three, env=CC))
    # No config: the default matcher is lexical (DS-3), which the binaries carry.
    add(ob("onboard no matcher config", "--dry-run", *m1, before=three, env=CC) | {"before": three})
    minilm = {**three, CONFIG: {"text": "matcher_model: all-MiniLM-L6-v2\n"}}
    add(ob("onboard minilm config", "--dry-run", *m1, before=minilm, env=CC, go_refuses=True))
    nope = {**three, CONFIG: {"text": "matcher_model: nope\n"}}
    add(ob("onboard unbuilt matcher", *m1, *nt, before=nope, env=CC))
    add(ob("onboard unbuilt matcher dry", "--dry-run", *m1, before=nope, env=CC))
    add(ob("onboard unbuilt matcher dry json", "--dry-run", "--json", *m1, before=nope, env=CC))
    add(
        ob(
            "onboard unbuilt matcher allowed",
            "--allow-no-embedder",
            *m1,
            *nt,
            before=nope,
            env=CC,
        )
    )
    return C


def build_delegate_cases() -> list[dict[str, Any]]:
    """The delegate tool (bulk read) with a fake worker. The
    local worker is the chat wire at the fake server (OPENAI_API_BASE); a
    remote worker is reached through the fake proxy. Each call writes one
    row of the delegation journal."""
    C: list[dict[str, Any]] = []
    add = C.append
    big = "{HOME}/work/big.py"
    text = "".join(f"def f{i}():\n    return {i}\n" for i in range(4))
    rule = {
        "id": "big-read",
        "version": 1,
        "shape": "deny_redirect",
        "state": "active",
        "match": {"tool": "Read", "file_lines_over": 5},
    }
    gate = ".opendaisugi/gate"
    tier1 = ".opendaisugi/local_tier1.json"
    local = {
        "work/big.py": {"text": text},
        f"{gate}/grafts/big-read.json": {"text": json.dumps(rule)},
        tier1: {"text": json.dumps({"model": "openai/qwen"})},
    }
    oai = {"OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1"}

    def env_file(**perm: Any) -> dict[str, Any]:
        p = {"file_read": ["{HOME}/work/**"], **perm}
        stakes = p.pop("stakes", "medium")
        return {
            "text": json.dumps(
                {"generated_by": "t", "task": "t", "stakes": stakes, "permissions": p}
            )
        }

    def call(args: dict[str, Any]) -> Session:
        return Session().call("delegate", args)

    q = {"path": big, "question": "What does f2 return?"}

    def reply(answer: Any = "It returns 2.", quotes: Any = None, **extra: Any) -> str:
        body: dict[str, Any] = {"answer": answer}
        if quotes is not None:
            body["quotes"] = quotes
        body.update(extra)
        return json.dumps(body)

    def d(name: str, s: Session, *, before=None, env=None, **kw: Any) -> None:
        add(
            mcp(
                f"delegate {name}",
                s,
                before=local if before is None else before,
                env=oai if env is None else env,
                **kw,
            )
        )

    d("ok", call(q), replies=[{"chat": reply(quotes=["    return 2", "return 9"])}])
    d("ok mode given", call({**q, "mode": "bulk_read"}), replies=[{"chat": reply(quotes=[])}])
    d("ok no quotes key", call(q), replies=[{"chat": reply()}])
    d(
        "fenced reply",
        call(q),
        replies=[{"chat": "```json\n" + reply(quotes=["def f2():"]) + "\n```"}],
    )
    d(
        "quotes dropped",
        call(q),
        replies=[
            {
                "chat": reply(
                    quotes=["def f1():", 7, "", "   ", "def f1():", "x" * 2001, "def f9():", None]
                )
            }
        ],
    )
    d(
        "more than 20 quotes",
        call(q),
        replies=[{"chat": reply(quotes=["def f0():"] * 25 + ["nope"])}],
    )
    d("long answer", call(q), replies=[{"chat": reply("y" * 4100, ["def f3():"])}])
    d("reply not json", call(q), replies=[{"chat": "It returns 2."}])
    d("reply a list", call(q), replies=[{"chat": "[1]"}])
    d("reply no answer", call(q), replies=[{"chat": json.dumps({"quotes": []})}])
    d("reply quotes not a list", call(q), replies=[{"chat": reply(quotes="def f2():")}])
    d("worker http 500", call(q), replies=[{"http_status": 500, "body": '{"error": "down"}'}])
    d("worker cut", call(q), replies=[{"chat": reply(quotes=["def f2():"]), "finish": "length"}])
    d(
        "two calls",
        call(q).call("delegate", {"path": big, "question": "Which functions are there?"}),
        replies=[{"chat": reply(quotes=["def f0():"])}, {"chat": reply("f0 to f3", ["def f3():"])}],
    )
    d(
        "messages wire",
        call(q),
        before={**local, tier1: {"text": json.dumps({"model": "anthropic/claude-haiku-4-5"})}},
        env=KEY,
        replies=[{"http": reply(quotes=["def f2():"])}],
    )
    # Refusals: nothing reaches a worker.
    d("no worker", call(q), before={k: v for k, v in local.items() if k != tier1})
    d(
        "no rule",
        call(q),
        before={k: v for k, v in local.items() if "grafts" not in k},
        replies=[{"chat": reply(quotes=["def f2():"])}],
    )
    d("relative path", call({**q, "path": "work/big.py"}))
    d("tilde path", call({**q, "path": "~/work/big.py"}))
    d("empty question", call({**q, "question": "  "}))
    d("unknown mode", call({**q, "mode": "edit"}))
    d("missing file", call({**q, "path": "{HOME}/work/none.py"}))
    d("a directory", call({**q, "path": "{HOME}/work"}))
    d(
        "nul file",
        call({**q, "path": "{HOME}/work/nul.bin"}),
        before={**local, "work/nul.bin": {"hex": "610a00"}},
    )
    d(
        "latin file",
        call({**q, "path": "{HOME}/work/latin.txt"}),
        before={**local, "work/latin.txt": {"hex": "63616fe90a"}},
    )
    d(
        "dotdot path",
        call({**q, "path": "{HOME}/work/sub/../big.py"}),
        replies=[{"chat": reply(quotes=["def f2():"])}],
    )
    d("no path", call({"question": "q"}))
    d("path not a string", call({"path": 5, "question": "q"}))
    d("no question", call({"path": "/w/big.py"}))
    d(
        "physical stakes",
        call(q),
        before={**local, f"{gate}/envelopes/default.json": env_file(stakes="physical")},
    )
    d(
        "envelope unreadable",
        call(q),
        before={**local, f"{gate}/envelopes/default.json": {"text": "{"}},
    )
    remote_t1 = {"text": json.dumps({"model": "qwen", "base_url": "http://worker.invalid/v1"})}
    remote_rule = {"text": json.dumps({**rule, "worker": {"allow_remote": True}})}
    px = {"HTTP_PROXY": "http://127.0.0.1:{PX}"}
    d("remote not allowed", call(q), before={**local, tier1: remote_t1})
    d(
        "remote no envelope",
        call(q),
        before={**local, tier1: remote_t1, f"{gate}/grafts/big-read.json": remote_rule},
    )
    d(
        "remote any host",
        call(q),
        before={
            **local,
            tier1: remote_t1,
            f"{gate}/grafts/big-read.json": remote_rule,
            f"{gate}/envelopes/default.json": env_file(network=True),
        },
    )
    granted = {
        **local,
        tier1: remote_t1,
        f"{gate}/grafts/big-read.json": remote_rule,
        f"{gate}/envelopes/default.json": env_file(network=True, network_hosts=["worker.invalid"]),
    }
    d(
        "remote granted",
        call(q),
        before=granted,
        env={**px},
        fake_proxy={"mode": "forward"},
        replies=[{"chat": reply(quotes=["def f2():"])}],
    )
    d(
        "remote granted priced",
        call(q),
        before={
            **granted,
            tier1: {
                "text": json.dumps(
                    {"model": "anthropic/claude-haiku-4-5", "base_url": "http://worker.invalid"}
                )
            },
        },
        env={**KEY, **px},
        fake_proxy={"mode": "forward"},
        replies=[{"http": reply(quotes=["def f2():"])}],
    )
    # Code write: the worker returns a draft; nothing is written.
    w = {"path": big, "question": "Make f2 return 22.", "mode": "code_write"}

    def draft(form: Any = "diff", text: Any = None, **extra: Any) -> str:
        body: dict[str, Any] = {"form": form}
        if text is not None:
            body["text"] = text
        body.update(extra)
        return json.dumps(body)

    ok_diff = (
        "--- a/big.py\n+++ b/big.py\n@@ -5,2 +5,2 @@\n def f2():\n-    return 2\n+    return 22\n"
    )
    for name, reply_text in (
        ("diff", draft(text=ok_diff)),
        (
            "diff two hunks",
            draft(text="@@\n def f0():\n-    return 0\n+    return 9\n@@\n def f3():\n"),
        ),
        ("diff no match", draft(text="@@\n def f9():\n")),
        ("diff second hunk no match", draft(text="@@\n-    return 1\n+    return 1\n@@\n def f\n")),
        ("diff removes the last newline", draft(text="@@\n-\n")),
        ("diff out of order", draft(text="@@\n def f3():\n@@\n def f1():\n")),
        ("diff no hunk", draft(text="--- a\n+++ b\n")),
        ("diff empty", draft(text="")),
        ("diff only added", draft(text="@@\n+x\n")),
        ("diff header junk", draft(text="Here is the diff:\n@@\n def f0():\n")),
        ("diff bad line", draft(text="@@\n def f0():\n*x\n")),
        ("diff backslash line", draft(text="@@\n-    return 3\n\\ No newline at end of file\n")),
        ("diff blank context", draft(text="@@\n    return 0\n\n def f1():\n")),
        ("diff no final newline", draft(text="@@\n def f1():")),
        ("file", draft("file", "def f():\n    return 1\n")),
        ("file empty", draft("file", "")),
        ("file with ticks", draft("file", "x = '```'\ny = '````'\n")),
        ("file unicode", draft("file", "s = '\u00e9\u4e2d'\n")),
        ("fenced reply", "```json\n" + draft("file", "x\n") + "\n```"),
        ("reply bad form", draft("patch", "x")),
        ("reply form not a string", draft(1, "x")),
        ("reply no text", draft("file")),
        ("reply text not a string", draft("file", 5)),
        ("reply not json", "here you go"),
        ("reply a list", "[]"),
    ):
        d(f"code write {name}", call(w), replies=[{"chat": reply_text}])
    new_target = {**w, "path": "{HOME}/work/sub/new.py", "question": "Write a hello function."}
    d(
        "code write new file",
        call(new_target),
        replies=[{"chat": draft("file", "def hello():\n    pass\n")}],
    )
    d("code write new file diff", call(new_target), replies=[{"chat": draft(text="@@\n+x\n")}])
    d(
        "code write crlf",
        call({**w, "path": "{HOME}/work/crlf.py"}),
        before={**local, "work/crlf.py": {"hex": "610d0a620d0a"}},
        replies=[{"chat": draft(text="@@\n-a\r\n+c\r\n b\r\n")}],
    )
    d(
        "code write crlf plain diff",
        call({**w, "path": "{HOME}/work/crlf.py"}),
        before={**local, "work/crlf.py": {"hex": "610d0a620d0a"}},
        replies=[{"chat": draft(text="@@\n-a\n+c\n")}],
    )
    d(
        "code write diff two places",
        call({**w, "path": "{HOME}/work/twice.py"}),
        before={**local, "work/twice.py": {"text": "a\nb\na\nb\n"}},
        replies=[{"chat": draft(text="@@\n a\n-b\n+c\n")}],
    )
    d("code write a directory", call({**w, "path": "{HOME}/work"}))
    d(
        "code write link",
        call({**w, "path": "{HOME}/work/link.py"}),
        before={**local, "work/link.py": {"link": "{HOME}/work/big.py"}},
        replies=[{"chat": draft(text=ok_diff)}],
    )
    # The draft limit counts characters: one two-byte character puts the
    # draft over 512 KiB in bytes and leaves it at the limit in characters.
    limit = 512 * 1024
    d(
        "code write draft at the limit",
        call(w),
        replies=[{"chat": draft("file", "\u00e9" + "x" * (limit - 1))}],
    )
    d(
        "code write draft over the limit",
        call(w),
        replies=[{"chat": draft("file", "\u00e9" + "x" * limit)}],
    )
    d(
        "code write latin file",
        call({**w, "path": "{HOME}/work/latin.txt"}),
        before={**local, "work/latin.txt": {"hex": "63616fe90a"}},
    )
    d("code write empty question", call({**w, "question": " "}))
    d("code write relative path", call({**w, "path": "work/big.py"}))
    d("code write no worker", call(w), before={k: v for k, v in local.items() if k != tier1})
    d("code write worker http 500", call(w), replies=[{"http_status": 500, "body": "{}"}])
    d(
        "code write physical stakes",
        call(w),
        before={**local, f"{gate}/envelopes/default.json": env_file(stakes="physical")},
    )
    d(
        "code write messages wire",
        call(w),
        before={**local, tier1: {"text": json.dumps({"model": "anthropic/claude-haiku-4-5"})}},
        env=KEY,
        replies=[{"http": draft(text=ok_diff)}],
    )
    d(
        "code write remote granted",
        call(w),
        before=granted,
        env={**px},
        fake_proxy={"mode": "forward"},
        replies=[{"chat": draft(text=ok_diff)}],
    )
    d(
        "code write then read",
        call(w).call("delegate", q),
        replies=[{"chat": draft(text=ok_diff)}, {"chat": reply(quotes=["def f2():"])}],
    )
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = (
        build_protocol_cases()
        + build_tool_cases()
        + build_setup_cases()
        + build_onboard_cases()
        + build_delegate_cases()
    )
    seen: set[str] = set()
    for c in cases:
        if c["name"] in seen:
            raise SystemExit(f"duplicate case name: {c['name']}")
        seen.add(c["name"])
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    ap.add_argument(
        "--redo",
        action="append",
        default=[],
        help="rerun the cases whose name holds this text and write them; keep the rest",
    )
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    cache_path = SCRATCH / "gen-cache.jsonl"
    if cache_path.exists() and not args.fresh:
        for ln in cache_path.read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old.setdefault(c["id"], c)
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        redo = any(r in c["name"] for r in args.redo)
        if prev is not None and not args.only and not redo:
            c["expect"] = prev["expect"]
            if "model" in prev:
                c["model"] = prev["model"]
            continue
        work = SCRATCH / "gen" / f"{i:04d}"
        if c.get("replies"):
            record_replies(c, work)
        c["expect"] = run_case(c, cmd_for(c, None), work)
        if any(r.get("key_ok") is False for r in c["expect"]["requests"]):
            raise SystemExit(f"{c['name']}: the oracle sent a credential that is not the case's")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
            )
        else:
            with cache_path.open("a", encoding="utf-8") as fh:
                rec = {"id": body_id(c), "expect": c["expect"]}
                if "model" in c:
                    rec["model"] = c["model"]
                fh.write(json.dumps(rec) + "\n")
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    cache_path.unlink(missing_ok=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
