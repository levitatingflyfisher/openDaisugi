"""Synthetic stage-K4 cases: `daisugi modules` (the module map, text and
--json), `daisugi dashboard` (the live floor: one frame, --json, and the
Textual views the binaries refuse) and `daisugi metrics` (the Prometheus
text, and the /metrics endpoint served on a loopback port and scraped),
run through the Python oracle.

    uv run --no-sync python clients/k4_cases.py [--out clients/fixtures/k4] [--only NAME]

A case runs as garden_cases runs one: a scratch HOME, the fake `claude`,
and the exit code, stdout, stderr and the tree after recorded. The oracle
runs as a base install with only the extras the binaries carry
(clients/base_install.py), so the map does not depend on the box. PATH
holds only the case's bin directory and its own fakes: the map reports
which of tmux, herdr, coppice, opencode and switchyard-server are on
PATH, and /usr/bin differs from box to box.

A serve case (`scrape`) starts `metrics --serve` on a free loopback port
(clients/ports.py), waits until it listens, sends each request, writes
each file a step names, then sends SIGINT. Each reply's status,
Content-Type, body, and Content-Length on a 200, is recorded in
`scrapes`. A server that exits before it listens is recorded as it is.

A case may carry `go_expect`: the fields where the binaries' answer is
ruled to differ from the oracle's (K4-2).

Every path, key and task is synthetic.
"""

from __future__ import annotations

import argparse
import http.client
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
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
from base_install import shim  # noqa: E402
from garden_cases import body_id, run_case, trace, write_jsonl  # noqa: E402
from pathway_cases import REPO  # noqa: E402
from ports import PortPool  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "k4"
SCRATCH = Path(
    os.environ.get("DAISUGI_K4_SCRATCH") or Path.home() / "opendaisugi-scratch" / "k4" / "runs"
)
CASE_VERSION = 1
ORACLE = [sys.executable, "-c", shim()]
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
SETTINGS = ".claude/settings.json"
TURNS = ".opendaisugi/gateway/turns.jsonl"
# Only the case's bin directory (the fake claude) and its own fakes.
PATH_ENV = {"PATH": "{HOME}/../bin:{HOME}/.fakebin"}

# How long the driver waits for a server to listen. The oracle takes
# seconds to start; the wait costs nothing when the port answers.
LISTEN_WAIT = 120.0

# ---------------------------------------------------------------------------
# Running a case: sockets, and the /metrics driver
# ---------------------------------------------------------------------------

# The scrapes of the last serve case run, read by run().
_SCRAPES: list[dict[str, Any]] | None = None


def _listen_or_exit(p: subprocess.Popen, port: int) -> bool:
    """True once the port accepts a connection; False when the process
    ended first or the wait ran out."""
    deadline = time.time() + LISTEN_WAIT
    while time.time() < deadline:
        if p.poll() is not None:
            return False
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.05)
    return False


def _request(port: int, method: str, path: str) -> dict[str, Any]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        conn.request(method, path)
        r = conn.getresponse()
        body = r.read().decode("utf-8", "replace")
        rec: dict[str, Any] = {
            "method": method,
            "path": path,
            "status": r.status,
            "type": r.getheader("Content-Type"),
            "body": body,
        }
        if r.status == 200:
            rec["length"] = r.getheader("Content-Length")
        return rec
    finally:
        conn.close()


def serve(
    case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, home: Path
) -> tuple[subprocess.CompletedProcess, list[dict[str, Any]]]:
    pool = PortPool()
    port = pool.take()
    argv = [a.replace("{MPORT}", str(port)) for a in argv]
    pool.release()
    p = subprocess.Popen(
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        cwd=cwd,
    )
    scrapes: list[dict[str, Any]] = []
    if _listen_or_exit(p, port):
        for step in case["scrape"]:
            if "write" in step:
                target = home / step["write"]
                target.parent.mkdir(parents=True, exist_ok=True)
                with target.open("a", encoding="utf-8") as fh:
                    fh.write(step["text"])
                continue
            scrapes.append(_request(port, step.get("method", "GET"), step["path"]))
        p.send_signal(signal.SIGINT)
    else:
        scrapes.append({"listened": False})
    try:
        out, err = p.communicate(timeout=30)
    except subprocess.TimeoutExpired:
        p.kill()
        out, err = p.communicate()
    text = json.dumps(scrapes)
    out = out.replace(str(port).encode(), b"{MPORT}")
    err = err.replace(str(port).encode(), b"{MPORT}")
    return subprocess.CompletedProcess(argv, p.returncode, out, err), json.loads(
        text.replace(str(port), "{MPORT}")
    )


def invoke(
    case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, work: Path
) -> subprocess.CompletedProcess:
    """garden_cases.invoke, with the case's unix sockets made before the
    command and removed after (the tree reader reads files), and a serve
    case driven by serve()."""
    global _SCRAPES
    home = work / "home"
    socks = []
    made: list[Path] = []
    for rel in case.get("sockets") or []:
        path = home / rel
        for d in reversed(path.relative_to(home).parents[:-1]):
            if not (home / d).exists():
                (home / d).mkdir()
                made.append(home / d)
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.bind(str(path))
        s.close()
        socks.append(path)
    try:
        if "scrape" in case:
            proc, _SCRAPES = serve(case, argv, env, cwd, home)
            return proc
        _SCRAPES = None
        return subprocess.run(
            argv,
            capture_output=True,
            env=env,
            cwd=cwd,
            timeout=case.get("timeout", 300),
            check=False,
        )
    finally:
        # The socket and the directories made for it are the harness's,
        # not the command's: the tree reader must not see them.
        for path in socks:
            path.unlink(missing_ok=True)
        for d in reversed(made):
            try:
                d.rmdir()
            except OSError:
                pass


class FakeCoppice:
    """A coppice server on a unix socket in the case's HOME: it writes
    each line a client sends to COPPICE_LOG in HOME and answers
    {"ok": true}. The socket is removed when it stops; the log stays in
    the tree."""

    def __init__(self, path: Path, log: Path) -> None:
        self.path, self.log = path, log
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(str(path))
        self.sock.listen(8)
        self.sock.settimeout(0.2)
        self.stop = threading.Event()
        self.threads: list[threading.Thread] = []
        self.accepter = threading.Thread(target=self._accept, daemon=True)
        self.accepter.start()

    def _accept(self) -> None:
        while not self.stop.is_set():
            try:
                conn, _ = self.sock.accept()
            except OSError:
                continue
            t = threading.Thread(target=self._serve, args=(conn,), daemon=True)
            t.start()
            self.threads.append(t)

    def _serve(self, conn: socket.socket) -> None:
        conn.settimeout(10)
        buf = b""
        try:
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    return
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    with self.log.open("ab") as fh:
                        fh.write(line + b"\n")
                    conn.sendall(b'{"ok": true}\n')
        except OSError:
            return
        finally:
            conn.close()

    def close(self) -> None:
        self.stop.set()
        self.accepter.join()
        for t in self.threads:
            t.join(15)
        self.sock.close()
        self.path.unlink(missing_ok=True)


COPPICE_SOCK = "cop.sock"
COPPICE_LOG = "coppice.log"


def _invoke_with_coppice(
    case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, work: Path
) -> subprocess.CompletedProcess:
    """invoke, with stdin fed from the case and, for a case that names
    `coppice`, a listening fake coppice for the command's life."""
    fake_cop = None
    if case.get("coppice"):
        home = work / "home"
        fake_cop = FakeCoppice(home / COPPICE_SOCK, home / COPPICE_LOG)
    try:
        if "stdin" in case or "stdin_hex" in case:
            data = (
                bytes.fromhex(case["stdin_hex"])
                if "stdin_hex" in case
                else case["stdin"].encode("utf-8")
            )
            return subprocess.run(
                argv,
                input=data,
                capture_output=True,
                env=env,
                cwd=cwd,
                timeout=case.get("timeout", 300),
                check=False,
            )
        return invoke(case, argv, env, cwd, work)
    finally:
        if fake_cop is not None:
            fake_cop.close()


garden_cases.invoke = _invoke_with_coppice

# A session tree's entry ids are 8 random hex digits.
_TREE_ID = re.compile(r'(\\"(?:id|parentId)\\": \\")([0-9a-f]{8})(\\")')


def run(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    """One case's normalized result, with its scrapes when it serves."""
    res = run_case(case, cmd, work)
    seen: dict[str, str] = {}

    def tree_id(m: re.Match[str]) -> str:
        seen.setdefault(m.group(2), "{ID" + str(len(seen) + 1) + "}")
        return m.group(1) + seen[m.group(2)] + m.group(3)

    res["tree"] = json.loads(_TREE_ID.sub(tree_id, json.dumps(res["tree"])))
    if "scrape" in case:
        text = json.dumps(_SCRAPES)
        text = text.replace(json.dumps(str(work / "home"))[1:-1], "{HOME}")
        res["scrapes"] = json.loads(text)
    return res


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return ORACLE if binary is None else [binary]


# ---------------------------------------------------------------------------
# What the binaries answer where it is ruled to differ (K4-2)
# ---------------------------------------------------------------------------

_LEXICAL_ROWS = ("lexical / intent", "lexical (no model)")
_LEXICAL_DESC = "keyword floor — no model, no download"


def _data_dir_config(case: dict[str, Any]) -> str:
    argv = case["argv"]
    rel = CONFIG
    if "--data-dir" in argv:
        d = argv[argv.index("--data-dir") + 1]
        rel = d.replace("{HOME}/", "").rstrip("/") + "/config.yaml"
    return (case.get("before") or {}).get(rel, {}).get("text", "")


def go_expect_for(case: dict[str, Any]) -> dict[str, Any] | None:
    """K4-2: under MiniLM or int8 the oracle, with the
    package absent, falls back to the lexical matcher and marks its rows
    active. The binaries refuse those matchers and fall back to nothing:
    the lexical rows are available, as under any other matcher."""
    if case["argv"][:1] not in (["modules"], ["dashboard"]):
        return None
    exp = case["expect"]
    if exp["exit"] != 0:
        return None
    m = re.search(r"^matcher_model: *(\S+)", _data_dir_config(case), re.M)
    key = m.group(1).strip("'\"") if m else "lexical"
    if key not in ("all-MiniLM-L6-v2", "int8"):
        return None
    out = exp["stdout"]
    if "--json" in case["argv"]:
        body = json.loads(out)
        for st in body:
            for mod in st["modules"]:
                if mod["name"] in _LEXICAL_ROWS:
                    mod["state"] = "available"
                    mod["note"] = _LEXICAL_DESC
        return {"stdout": json.dumps(body, indent=2) + "\n"}
    for name in _LEXICAL_ROWS:
        out = out.replace(f"* {name}   ", f"o {name}   ").replace(f"● {name}   ", f"○ {name}   ")
    return {"stdout": out}


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def fake(*names: str) -> dict[str, Any]:
    """Programs on PATH that do nothing: only their presence is read."""
    return {f".fakebin/{n}": {"text": "#!/bin/sh\nexit 0\n", "mode": 0o755} for n in names}


def settings(**events: list[str]) -> dict[str, Any]:
    """~/.claude/settings.json with one hook entry per command, by event."""
    hooks = {
        ev: [{"matcher": "*", "hooks": [{"type": "command", "command": c} for c in cmds]}]
        for ev, cmds in events.items()
    }
    return {SETTINGS: {"text": json.dumps({"hooks": hooks}, indent=2) + "\n"}}


GATE_HOOK = "daisugi hook --mode audit"
GATEWAY_HOOK = "daisugi gateway-env"
STOP_HOOK = "daisugi floor report --event stop --backend herdr"
NOTE_HOOK = "daisugi floor report --event notification --backend herdr"


def cfg(text: str) -> dict[str, Any]:
    return {CONFIG: {"text": text}}


def turn(
    task: str,
    *,
    tier: str = "tier1-cheap",
    model: Any = "claude-haiku-4-5",
    downgraded: bool = True,
    tokens=(100, 20, 0, 0),
    actual: Any = 0.0002,
    cf: Any = 0.003,
    **extra: Any,
) -> str:
    rec = {
        "created_at": "2020-01-01T00:00:00Z",
        "signature": "sig-" + task.replace(" ", "-"),
        "task": task,
        "tier": tier,
        "requested_model": "claude-opus-4-8",
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
    return json.dumps(rec) + "\n"


def turns(*lines: str) -> dict[str, Any]:
    return {TURNS: {"text": "".join(lines)}}


DAY = turns(
    turn("summarize the auth module"),
    turn("summarize the auth module", tokens=(50, 5, 1000, 20)),
    turn(
        "design the schema",
        tier="tier2-frontier",
        model="claude-opus-4-8",
        downgraded=False,
        actual=0.02,
        cf=0.02,
    ),
    turn("run the unit tests", tier="tier1-local", model="qwen", actual=0.0, cf=0.001),
)
BIG = turns(
    turn("big one", tokens=(1_234_567, 89_012, 3_000_000, 45_678), actual=12.5, cf=1234.5678),
    turn("big two", tokens=(987_654_321, 1, 0, 0), actual=0.125, cf=2.675),
)


def journal(n_ok: int, n_fail: int, data_dir: str = ".opendaisugi") -> dict[str, Any]:
    ts = []
    for i in range(n_ok + n_fail):
        ok = i < n_ok
        ts.append(
            trace(
                i + 1,
                f"task {i + 1}",
                [{"id": "s1", "type": "shell", "command": "make test", "depends_on": []}],
                hours=2 + i,
                ok=ok,
                **(
                    {}
                    if ok
                    else {"violations": [{"stage": "permissions", "message": "shell not allowed"}]}
                ),
            )
        )
    return {data_dir: {"journal": {"traces": ts}}}


def pathways(*hits: int, data_dir: str = ".opendaisugi") -> dict[str, Any]:
    from pathway_cases import lexical, pathway, put_row, row_spec

    rows = []
    for i, h in enumerate(hits):
        task = f"reusable task {i + 1}"
        rows.append(row_spec(put_row(pathway(i + 1, task, lexical(task), hit_count=h))))
    return {f"{data_dir}/pathways.db": {"db": {"rows": rows}}}


STORES = {**journal(2, 1), **pathways(3, 4), **DAY}


def case(name: str, *argv: str, before=None, env=None, **kw: Any) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": list(argv),
        "before": before or {},
        "env": {**PATH_ENV, **(env or {})},
    }
    c.update(kw)
    return c


# ---------------------------------------------------------------------------
# modules
# ---------------------------------------------------------------------------


def build_modules_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    for fmt in ((), ("--json",)):
        sfx = " json" if fmt else ""
        add(case("modules fresh" + sfx, "modules", *fmt))
        add(case("modules lexical" + sfx, "modules", *fmt, before=LEX))
        add(case("modules stores" + sfx, "modules", *fmt, before={**LEX, **STORES}))
        wired = {
            **LEX,
            **cfg(
                "matcher_model: potion\ngateway_router: switchyard\nllm_backend: claude-code\n"
                "voice_engine: moonshine\nshell_allow_decomposition: true\n"
                "llm_base_url: http://gpu-box:11434/v1\nllm_host_kind: ollama\n"
                "llm_host_model: qwen3:8b\nllm_context_window: 32768\n"
            ),
            **settings(
                PreToolUse=[GATE_HOOK, GATEWAY_HOOK], Stop=[STOP_HOOK], Notification=[NOTE_HOOK]
            ),
            ".claude/projects": {"dir": True},
            ".codex/sessions": {"dir": True},
            ".pi/agent/extensions/daisugi-gate/index.ts": {"text": "// gate\n"},
            ".config/opencode/plugins/daisugi-gate.ts": {"text": "// gate\n"},
            **fake("tmux", "herdr", "coppice", "switchyard-server", "opencode"),
            **STORES,
        }
        add(
            case(
                "modules everything wired" + sfx,
                "modules",
                *fmt,
                before=wired,
                sockets=[".opendaisugi/coppice/server.sock"],
            )
        )
    j = ("--json",)
    add(case("modules potion", "modules", *j, before=cfg("matcher_model: potion\n")))
    add(case("modules int8", "modules", *j, before=cfg("matcher_model: int8\n")))
    add(case("modules int8 text", "modules", before=cfg("matcher_model: int8\n")))
    add(
        case("modules minilm named", "modules", *j, before=cfg("matcher_model: all-MiniLM-L6-v2\n"))
    )
    add(case("modules unbuilt matcher", "modules", before=cfg("matcher_model: nope\n")))
    add(case("modules invalid config", "modules", before=cfg("gate_ask: maybe\n")))
    add(case("modules invalid config json", "modules", *j, before=cfg("z3_timeout_ms: 1.5\n")))
    add(
        case(
            "modules data dir",
            "modules",
            "--data-dir",
            "{HOME}/dd/",
            before={
                "dd/config.yaml": {"text": "matcher_model: lexical\ngateway_router: switchyard\n"},
                **journal(1, 0, "dd"),
            },
        )
    )
    add(
        case(
            "modules data dir json",
            "modules",
            "--json",
            "--data-dir",
            "{HOME}/dd",
            before={"dd/config.yaml": {"text": "matcher_model: potion\n"}, **LEX},
        )
    )
    add(case("modules data dir missing", "modules", "--data-dir", "{HOME}/nowhere", before=LEX))
    # The harness row: pi and opencode found but not installed.
    add(
        case(
            "modules pi and opencode detected",
            "modules",
            *j,
            before={**LEX, ".pi": {"dir": True}, ".config/opencode": {"dir": True}},
        )
    )
    add(case("modules opencode on path", "modules", *j, before={**LEX, **fake("opencode")}))
    add(
        case(
            "modules opencode xdg",
            "modules",
            *j,
            before={**LEX, "xdg/opencode/plugins/daisugi-gate.ts": {"text": "// gate\n"}},
            env={"XDG_CONFIG_HOME": "{HOME}/xdg"},
        )
    )
    add(
        case(
            "modules opencode xdg relative",
            "modules",
            *j,
            before={**LEX, ".config/opencode": {"dir": True}},
            env={"XDG_CONFIG_HOME": "xdg"},
        )
    )
    # The floor: one herdr hook of two, the binary only, and coppice.
    add(case("modules herdr one hook", "modules", *j, before={**LEX, **settings(Stop=[STOP_HOOK])}))
    add(
        case(
            "modules herdr other hook",
            "modules",
            *j,
            before={**LEX, **settings(Notification=[NOTE_HOOK]), **fake("herdr")},
        )
    )
    add(case("modules herdr binary", "modules", *j, before={**LEX, **fake("herdr", "tmux")}))
    add(case("modules coppice on path", "modules", *j, before={**LEX, **fake("coppice")}))
    add(
        case(
            "modules coppice runtime socket",
            "modules",
            *j,
            before=LEX,
            env={"XDG_RUNTIME_DIR": "{HOME}/run"},
            sockets=["run/coppice/server.sock"],
        )
    )
    add(
        case(
            "modules coppice socket not in runtime dir",
            "modules",
            *j,
            before=LEX,
            env={"XDG_RUNTIME_DIR": "{HOME}/run"},
            sockets=[".opendaisugi/coppice/server.sock"],
        )
    )
    add(
        case(
            "modules coppice socket is a file",
            "modules",
            *j,
            before={**LEX, ".opendaisugi/coppice/server.sock": {"text": ""}},
        )
    )
    # The router row.
    add(
        case(
            "modules switchyard selected no binary",
            "modules",
            *j,
            before=cfg("matcher_model: lexical\ngateway_router: switchyard\n"),
        )
    )
    add(
        case(
            "modules switchyard binary", "modules", *j, before={**LEX, **fake("switchyard-server")}
        )
    )
    add(
        case(
            "modules gateway hook",
            "modules",
            *j,
            before={**LEX, **settings(PreToolUse=[GATEWAY_HOOK])},
        )
    )
    add(
        case(
            "modules gateway env",
            "modules",
            *j,
            before=LEX,
            env={"OPENDAISUGI_GATEWAY_BASE_URL": "http://127.0.0.1:1/x"},
        )
    )
    # The backend rows.
    for name, value in (
        ("api", "api"),
        ("anthropic", "anthropic"),
        ("ollama", "ollama"),
        ("llamafile", "llamafile"),
        ("claude-code spaced", " claude-code "),
        ("unknown", "mystery"),
    ):
        add(
            case(
                f"modules backend env {name}",
                "modules",
                *j,
                before=LEX,
                env={"OPENDAISUGI_LLM_BACKEND": value},
            )
        )
    add(
        case(
            "modules backend config",
            "modules",
            *j,
            before=cfg("matcher_model: lexical\nllm_backend: ' ollama '\n"),
        )
    )
    add(
        case(
            "modules backend env over config",
            "modules",
            *j,
            before=cfg("matcher_model: lexical\nllm_backend: ollama\n"),
            env={"OPENDAISUGI_LLM_BACKEND": "api"},
        )
    )
    add(case("modules auto no claude", "modules", *j, before=LEX, no_claude=True))
    add(
        case(
            "modules auto api key",
            "modules",
            *j,
            before=LEX,
            env={"ANTHROPIC_API_KEY": "sk-test-k4-0000"},
        )
    )
    add(
        case(
            "modules auto auth token",
            "modules",
            *j,
            before=LEX,
            no_claude=True,
            env={"ANTHROPIC_AUTH_TOKEN": "tok-test-k4"},
        )
    )
    # A recorded remote host, in the forms the row words.
    for name, text in (
        (
            "full",
            "llm_base_url: http://gpu-box:11434/v1\nllm_host_kind: ollama\nllm_host_model: m\nllm_context_window: 8192\n",
        ),
        ("no scheme", "llm_base_url: gpu-box:11434\n"),
        ("bare host", "llm_base_url: gpu-box\nllm_host_model: ''\n"),
        ("small context", "llm_base_url: https://h.example/v1\nllm_context_window: 1000\n"),
        ("negative context", "llm_base_url: http://h:1\nllm_context_window: -1\n"),
        ("zero context", "llm_base_url: http://h:1\nllm_context_window: 0\n"),
        ("string context", "llm_base_url: http://h:1\nllm_context_window: '65_536'\n"),
        ("bool context", "llm_base_url: http://h:1\nllm_context_window: true\n"),
        ("user and port", "llm_base_url: http://u:p@h.example:8080/v1?q=1#f\n"),
        ("empty url", "llm_base_url: ''\nllm_host_model: m\n"),
    ):
        add(
            case(
                f"modules remote host {name}",
                "modules",
                *j,
                before=cfg("matcher_model: lexical\n" + text),
            )
        )
    add(
        case(
            "modules remote host text",
            "modules",
            before=cfg("matcher_model: lexical\n" + "llm_base_url: http://gpu-box:11434/v1\n"),
        )
    )
    # Transcript roots, from the environment.
    add(
        case(
            "modules transcript roots env",
            "modules",
            *j,
            before={**LEX, "elsewhere": {"dir": True}},
            env={"OPENDAISUGI_TRANSCRIPT_ROOTS": "codex={HOME}/elsewhere:claude-code=missing"},
        )
    )
    # settings.json shapes the reader turns into "not installed".
    add(
        case(
            "modules settings not json", "modules", *j, before={**LEX, SETTINGS: {"text": "{nope"}}
        )
    )
    add(case("modules settings no hooks", "modules", *j, before={**LEX, SETTINGS: {"text": "{}"}}))
    add(
        case(
            "modules settings not utf8",
            "modules",
            *j,
            before={**LEX, SETTINGS: {"hex": "7b22686f6f6b73223a20ff7d"}},
        )
    )
    # A shape the oracle raises on: refused by the binaries (K4-4).
    add(case("modules settings a list", "modules", *j, before={**LEX, SETTINGS: {"text": "[]"}}))
    # Stores the map reads as zero.
    add(
        case(
            "modules corrupt stores",
            "modules",
            before={
                **LEX,
                ".opendaisugi/journal/index.db": {"text": "not a database"},
                ".opendaisugi/pathways.db": {"text": "not a database either"},
            },
        )
    )
    add(case("modules unknown option", "modules", "--bogus"))
    add(case("modules extra arg", "modules", "x"))
    add(case("modules plain", "--plain", "modules", before=LEX))
    return C


# ---------------------------------------------------------------------------
# dashboard
# ---------------------------------------------------------------------------


def build_dashboard_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    add(case("dashboard once fresh", "dashboard", "--once"))
    add(case("dashboard json fresh", "dashboard", "--json"))
    add(case("dashboard once lexical", "dashboard", "--once", before=LEX))
    add(case("dashboard default one frame", "dashboard", before=LEX))
    add(case("dashboard once stores", "dashboard", "--once", before={**LEX, **STORES}))
    add(case("dashboard json stores", "dashboard", "--json", before={**LEX, **STORES}))
    add(
        case(
            "dashboard once big numbers",
            "dashboard",
            "--once",
            before={**LEX, **BIG, **pathways(1234, 5678)},
        )
    )
    add(case("dashboard json big numbers", "dashboard", "--json", before={**LEX, **BIG}))
    odd = turns(
        turn("nan dollars", actual=float("nan"), cf=0.5),
        turn("inf dollars", actual=0.1, cf=float("inf"), downgraded=False),
    )
    add(case("dashboard once nan dollars", "dashboard", "--once", before={**LEX, **odd}))
    add(case("dashboard json nan dollars", "dashboard", "--json", before={**LEX, **odd}))
    add(
        case(
            "dashboard once zero cost",
            "dashboard",
            "--once",
            before={**LEX, **turns(turn("free", actual=0.0, cf=0.0, tokens=(0, 0, 0, 0)))},
        )
    )
    add(
        case(
            "dashboard once negative savings",
            "dashboard",
            "--once",
            before={**LEX, **turns(turn("dearer", actual=2.5, cf=1.25))},
        )
    )
    add(
        case(
            "dashboard once gateway skipped lines",
            "dashboard",
            "--once",
            before={**LEX, TURNS: {"text": "{broken\n\n[1]\n" + turn("kept") + '{"task": "x"}\n'}},
        )
    )
    add(
        case(
            "dashboard once gateway not utf8",
            "dashboard",
            "--once",
            before={**LEX, TURNS: {"hex": "ff0a"}},
        )
    )
    add(
        case(
            "dashboard once gateway empty",
            "dashboard",
            "--once",
            before={**LEX, TURNS: {"text": ""}},
        )
    )
    # A record of other types: the oracle adds what is there (GW-8).
    add(
        case(
            "dashboard once gateway float tokens",
            "dashboard",
            "--once",
            before={**LEX, **turns(turn("floaty", tokens=(1.5, 2, 0, 0)))},
        )
    )
    add(
        case(
            "dashboard once corrupt stores",
            "dashboard",
            "--once",
            before={
                **LEX,
                ".opendaisugi/journal/index.db": {"text": "not a database"},
                ".opendaisugi/pathways.db": {"text": "not a database either"},
            },
        )
    )
    add(
        case(
            "dashboard json data dir",
            "dashboard",
            "--json",
            "--data-dir",
            "{HOME}/dd",
            before={
                **LEX,
                "dd/config.yaml": {"text": "matcher_model: lexical\n"},
                **journal(0, 2, "dd"),
                **pathways(0, data_dir="dd"),
            },
        )
    )
    add(case("dashboard once interval", "dashboard", "--once", "--interval", "0.5", before=LEX))
    add(case("dashboard bad interval", "dashboard", "--interval", "soon", before=LEX))
    add(case("dashboard bad port", "dashboard", "--serve", "--port", "http", before=LEX))
    add(case("dashboard json wins over tui", "dashboard", "--json", "--tui", before=LEX))
    add(case("dashboard once wins over serve", "dashboard", "--once", "--serve", before=LEX))
    add(case("dashboard tui", "dashboard", "--tui", before=LEX, go_refuses=True))
    add(case("dashboard serve", "dashboard", "--serve", before=LEX, go_refuses=True))
    add(
        case(
            "dashboard unbuilt matcher", "dashboard", "--once", before=cfg("matcher_model: nope\n")
        )
    )
    add(case("dashboard invalid config", "dashboard", "--json", before=cfg("gate_mode: [1]\n")))
    add(case("dashboard unknown option", "dashboard", "--bogus"))
    add(case("dashboard plain", "--plain", "dashboard", "--once", before=LEX))
    add(case("dashboard quiet", "-q", "dashboard", "--once", before=LEX))
    return C


# ---------------------------------------------------------------------------
# metrics
# ---------------------------------------------------------------------------


def build_metrics_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    add(case("metrics fresh", "metrics"))
    add(case("metrics stores", "metrics", before=STORES))
    add(case("metrics big numbers", "metrics", before={**BIG, **pathways(1234, 5678)}))
    add(
        case(
            "metrics nan and inf",
            "metrics",
            before=turns(
                turn("nan dollars", actual=float("nan"), cf=0.5),
                turn("inf dollars", actual=0.1, cf=float("inf")),
            ),
        )
    )
    add(
        case(
            "metrics tiny and huge floats",
            "metrics",
            before=turns(turn("tiny", actual=1e-7, cf=3e-5), turn("huge", actual=1.0, cf=1e17)),
        )
    )
    add(
        case(
            "metrics corrupt stores",
            "metrics",
            before={
                ".opendaisugi/journal/index.db": {"text": "not a database"},
                ".opendaisugi/pathways.db": {"text": "not a database either"},
            },
        )
    )
    add(case("metrics gateway not utf8", "metrics", before={TURNS: {"hex": "ff0a"}}))
    add(
        case(
            "metrics gateway float tokens",
            "metrics",
            before=turns(turn("f", tokens=(1.5, 2, 0, 0))),
        )
    )
    add(case("metrics unbuilt matcher", "metrics", before=cfg("matcher_model: nope\n")))
    add(case("metrics invalid config", "metrics", before=cfg("gate_ask: maybe\n")))
    add(
        case(
            "metrics data dir",
            "metrics",
            "--data-dir",
            "{HOME}/dd",
            before={**journal(1, 1, "dd"), **pathways(2, data_dir="dd")},
        )
    )
    add(case("metrics unknown option", "metrics", "--bogus"))
    add(case("metrics bad port", "metrics", "--serve", "--port", "x"))
    paths = [
        {"path": "/metrics"},
        {"path": "/metrics/"},
        {"path": "/"},
        {"path": "//"},
        {"path": "/metrics//"},
        {"path": "/other"},
        {"path": "/metrics?x=1"},
        {"path": "/%6detrics"},
        {"path": "/metrics/.."},
        {"method": "POST", "path": "/metrics"},
    ]
    add(
        case(
            "metrics serve",
            "metrics",
            "--serve",
            "--port",
            "{MPORT}",
            before=STORES,
            scrape=paths,
        )
    )
    add(
        case(
            "metrics serve reads each scrape",
            "metrics",
            "--serve",
            "--host",
            "127.0.0.1",
            "--port",
            "{MPORT}",
            scrape=[
                {"path": "/metrics"},
                {"write": TURNS, "text": turn("later turn")},
                {"path": "/metrics"},
            ],
        )
    )
    add(
        case(
            "metrics serve data dir",
            "metrics",
            "--serve",
            "--port",
            "{MPORT}",
            "--data-dir",
            "{HOME}/dd",
            before={**journal(1, 0, "dd")},
            scrape=[{"path": "/metrics"}],
        )
    )
    return C


# ---------------------------------------------------------------------------
# status: the stores read read-only (K4-8), the count an inf hit sum keeps
# (K4-9). Only --json: the text names this box's memory.
# ---------------------------------------------------------------------------


def build_status_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    add(case("status fresh makes nothing", "status", "--json", before={**LEX}))
    add(
        case("status stores", "status", "--json", before={**LEX, **journal(2, 1), **pathways(3, 4)})
    )
    add(
        case(
            "status inf hit sum keeps the count",
            "status",
            "--json",
            before={**LEX, **_inf_hits()},
        )
    )
    # The views over the same store: the map counts the pathways; the
    # floor and the exporter raise on the hits (K4-4).
    for name, argv in (
        ("modules inf hit sum", ["modules"]),
        ("dashboard once inf hit sum", ["dashboard", "--once"]),
        ("dashboard json inf hit sum", ["dashboard", "--json"]),
        ("metrics inf hit sum", ["metrics"]),
    ):
        add(case(name, *argv, before={**LEX, **_inf_hits()}))
    return C


def _inf_hits() -> dict[str, Any]:
    """Two pathways whose REAL hit counts sum to inf."""
    store = pathways(1, 1)
    for row in store[".opendaisugi/pathways.db"]["db"]["rows"]:
        row["hit_count"] = 1e308
    return store


# ---------------------------------------------------------------------------
# hook record: the capture hook and the floor-report hooks install writes.
# Every case feeds stdin; a fake coppice listens where the case names it.
# ---------------------------------------------------------------------------

CAPTURES = ".opendaisugi/captures"
COPPICE_ENV = {"COPPICE_SOCK": "{HOME}/" + COPPICE_SOCK, "COPPICE_PANE": "w1:p1"}
HERDR_FAKE = {
    ".fakebin/herdr": {
        "text": '#!/bin/sh\nprintf \'%s\\n\' "$*" >> "$HOME/herdr.log"\n',
        "mode": 0o755,
    }
}
CONSENT = {CONFIG: {"text": "matcher_model: lexical\nauto_tend: true\n"}}
STAMP = ".opendaisugi/.hook-record-tend-trigger"


def hook(name: str, *argv: str, stdin: Any = "", **kw: Any) -> dict[str, Any]:
    if not isinstance(stdin, str):
        stdin = json.dumps(stdin)
    return case(name, "hook", "record", *argv, stdin=stdin, **kw)


def bash(cmd: str, sid: str = "sess-A", **extra: Any) -> dict[str, Any]:
    return {"session_id": sid, "tool_name": "Bash", "tool_input": {"command": cmd}, **extra}


def build_hook_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    # The capture hook (pre_tool_use).
    add(hook("hook record bash", stdin=bash("make test", tool_use_id="tu-1", cwd="/w")))
    for fmt in ("claude", "codex", "hermes", "openclaw", "pi", "other"):
        add(hook(f"hook record format {fmt}", "--format", fmt, stdin=bash("ls")))
    add(hook("hook record empty stdin"))
    add(hook("hook record blank stdin", stdin="  \n"))
    add(hook("hook record not json", stdin="{nope"))
    add(hook("hook record json list", stdin=[1, 2]))
    add(hook("hook record json empty object", stdin={}))
    add(hook("hook record unknown tool", stdin={"session_id": "s", "tool_name": "Frobnicate"}))
    add(hook("hook record tool name not a string", stdin={"session_id": "s", "tool_name": 5}))
    add(
        hook(
            "hook record tool input a list",
            stdin={"session_id": "s", "tool_name": "Bash", "tool_input": [1]},
        )
    )
    add(
        hook(
            "hook record write relative path",
            stdin={
                "session_id": "s-w",
                "tool_name": "Write",
                "tool_input": {"file_path": "src/a.py", "content": "héllo"},
                "cwd": "/work/proj",
                "agent_id": "ag-1",
            },
        )
    )
    add(
        hook(
            "hook record read home path",
            stdin={
                "session_id": "s-r",
                "tool_name": "Read",
                "tool_input": {"path": "~/x"},
                "cwd": "/w",
            },
        )
    )
    add(
        hook(
            "hook record write content int",
            stdin={
                "session_id": "s",
                "tool_name": "Edit",
                "tool_input": {"file_path": "/a", "content": 5},
            },
        )
    )
    add(
        hook(
            "hook record web fetch",
            stdin={
                "session_id": "s-n",
                "tool_name": "WebFetch",
                "tool_input": {"url": "https://x.test/?q=1"},
            },
        )
    )
    add(
        hook(
            "hook record mcp",
            stdin={
                "session_id": "s-m",
                "tool_name": "mcp__github__list__issues",
                "tool_input": {"repo": "a/b", "n": 1.5, "big": 10**30},
            },
        )
    )
    add(
        hook(
            "hook record mcp nan argument",
            stdin='{"session_id": "s-m", "tool_name": "mcp__s__t", "tool_input": {"x": NaN}}',
        )
    )
    add(
        hook(
            "hook record apply patch",
            stdin={"session_id": "s", "tool_name": "mcp__opencode__apply_patch"},
        )
    )
    add(hook("hook record mcp no tool", stdin={"session_id": "s", "tool_name": "mcp__srv"}))
    add(
        hook(
            "hook record pi tool under pi format",
            "--format",
            "pi",
            stdin={"session_id": "s", "tool_name": "read"},
        )
    )
    add(hook("hook record session id path", stdin=bash("ls", sid="../../etc/x y")))
    add(hook("hook record session id number", stdin=bash("ls", sid=12)))
    add(
        hook("hook record no session id", stdin={"tool_name": "Bash", "tool_input": {"cmd": "pwd"}})
    )
    add(
        hook(
            "hook record bad utf8",
            stdin_hex=(json.dumps(bash("echo ")).encode()[:-3] + b'\xff"}}').hex(),
        )
    )
    add(
        hook(
            "hook record appends",
            stdin=bash("second"),
            before={f"{CAPTURES}/sess-A.jsonl": {"text": '{"old": 1}\n', "mode": 0o644}},
        )
    )
    add(hook("hook record captures root", "--captures-root", "{HOME}/cap/here", stdin=bash("ls")))
    add(
        hook(
            "hook record relative captures root",
            "--captures-root",
            "rel/caps",
            stdin=bash("ls"),
            cwd="w",
        )
    )
    add(
        hook(
            "hook record captures root is a file",
            "--captures-root",
            "{HOME}/afile",
            stdin=bash("ls"),
            before={"afile": {"text": "x"}},
        )
    )
    # The tend trigger.
    add(hook("hook record consent no daisugi", stdin=bash("ls"), before={**CONSENT}))
    add(hook("hook record consent", stdin=bash("ls"), before={**CONSENT, **fake("daisugi")}))
    add(hook("hook record consent off", stdin=bash("ls"), before={**LEX, **fake("daisugi")}))
    add(
        hook(
            "hook record consent recent stamp",
            stdin=bash("ls"),
            before={**CONSENT, **fake("daisugi"), STAMP: {"text": "{NOWF:-60}"}},
        )
    )
    for label, text in (
        ("old", "{NOWF:-7200}"),
        ("garbage", "soon"),
        ("nan", " nan\n"),
        ("inf", "inf"),
    ):
        add(
            hook(
                f"hook record consent stamp {label}",
                stdin=bash("ls"),
                before={**CONSENT, **fake("daisugi"), STAMP: {"text": text}},
            )
        )
    add(
        hook(
            "hook record consent invalid config",
            stdin=bash("ls"),
            before={CONFIG: {"text": "auto_tend: maybe\n"}, **fake("daisugi")},
        )
    )
    add(hook("hook record not json consent", stdin="nope", before={**CONSENT, **fake("daisugi")}))
    # The option checks.
    add(hook("hook record bad event", "--event", "Stop", stdin=bash("ls")))
    add(hook("hook record unknown option", "--bogus", stdin=bash("ls")))
    # The floor-report hooks.
    stop = {
        "session_id": "sess-S",
        "transcript_path": "/t/s.jsonl",
        "cwd": "/w",
        "hook_event_name": "Stop",
    }
    add(hook("hook stop", "--event", "stop", stdin=stop))
    add(hook("hook stop coppice", "--event", "stop", stdin=stop, env=COPPICE_ENV, coppice=True))
    add(
        hook(
            "hook stop herdr",
            "--event",
            "stop",
            stdin=stop,
            env={"HERDR_PANE_ID": "p-1"},
            before=HERDR_FAKE,
        )
    )
    add(
        hook(
            "hook stop herdr bad pane",
            "--event",
            "stop",
            stdin=stop,
            env={"HERDR_PANE": "-x"},
            before=HERDR_FAKE,
        )
    )
    add(hook("hook stop hermes", "--event", "stop", "--format", "hermes", stdin=stop))
    add(
        hook(
            "hook stop no session",
            "--event",
            "stop",
            stdin={"cwd": "/w"},
            env=COPPICE_ENV,
            coppice=True,
        )
    )
    add(hook("hook stop not json", "--event", "stop", stdin="{", env=COPPICE_ENV, coppice=True))
    add(hook("hook stop json list", "--event", "stop", stdin=[1], env=COPPICE_ENV, coppice=True))
    add(
        hook(
            "hook stop odd fields",
            "--event",
            "stop",
            stdin={"session_id": 7, "cwd": {"a": [1, None]}, "transcript_path": "rel/t.jsonl"},
            env=COPPICE_ENV,
            coppice=True,
        )
    )
    add(
        hook(
            "hook stop session id path",
            "--event",
            "stop",
            stdin={"session_id": "../x/../y"},
        )
    )
    tree = ".opendaisugi/sessions/sess-S.jsonl"
    add(
        hook(
            "hook stop existing tree",
            "--event",
            "stop",
            stdin=stop,
            before={
                tree: {
                    "text": '{"type": "session", "id": "sess-S", "ts": 1.0}\n'
                    '{"type": "tool_call", "id": "tc-one", "parentId": null, "ts": 2.0}\n'
                    '{"type": "state", "id": "st-one", "parentId": "tc-one", "ts": 3.0}\n',
                    "mode": 0o600,
                }
            },
        )
    )
    add(
        hook(
            "hook stop tree head entry",
            "--event",
            "stop",
            stdin=stop,
            before={
                tree: {
                    "text": '{"type": "tool_call", "id": "tc-one", "ts": 2.0}\n'
                    '{"type": "head", "leafId": "tc-zero", "ts": 3.0}\n{torn\n\n',
                    "mode": 0o600,
                }
            },
        )
    )
    add(
        hook(
            "hook stop tree not utf8",
            "--event",
            "stop",
            stdin=stop,
            before={tree: {"hex": "ff0a"}},
        )
    )
    add(
        hook(
            "hook stop captures root",
            "--event",
            "stop",
            "--captures-root",
            "{HOME}/a/b/caps",
            stdin=stop,
        )
    )
    note = {"session_id": "sess-N", "cwd": "/w"}
    for label, extra in (
        (
            "permission prompt",
            {"notification_type": "permission_prompt", "message": "Claude needs your permission"},
        ),
        ("idle prompt", {"notification_type": "idle_prompt", "message": "waiting for permission"}),
        ("no type permission", {"message": "Needs PERMISSION to use Bash"}),
        ("no type idle", {"message": "done"}),
        ("no message", {}),
        ("agent needs input", {"notification_type": "agent_needs_input"}),
        ("type list", {"notification_type": ["permission_prompt"], "message": "x"}),
        ("type number", {"notification_type": 3, "message": "permission"}),
        ("message number", {"message": 42}),
        ("long message", {"notification_type": "permission_prompt", "message": "p" * 300}),
        ("unicode message", {"message": "Ünïcode permission ✓"}),
    ):
        add(
            hook(
                f"hook notification {label}",
                "--event",
                "notification",
                stdin={**note, **extra},
                env=COPPICE_ENV,
                coppice=True,
            )
        )
    add(
        hook(
            "hook notification lone surrogate",
            "--event",
            "notification",
            stdin='{"session_id": "sess-N", "message": "bad \\ud800 permission"}',
        )
    )
    sub = {"session_id": "sess-S", "agent_id": "ag-7", "agent_type": "Explore"}
    for ev in ("subagent_start", "subagent_stop"):
        add(hook(f"hook {ev}", "--event", ev, stdin=sub, env=COPPICE_ENV, coppice=True))
    add(hook("hook subagent_start no coppice", "--event", "subagent_start", stdin=sub))
    add(
        hook(
            "hook subagent_start no agent id",
            "--event",
            "subagent_start",
            stdin={"session_id": "s"},
            env=COPPICE_ENV,
            coppice=True,
        )
    )
    add(
        hook(
            "hook subagent_start label not a string",
            "--event",
            "subagent_start",
            stdin={"agent_id": "ag-8", "agent_type": 5},
            env=COPPICE_ENV,
            coppice=True,
        )
    )
    add(
        hook(
            "hook subagent_stop long label",
            "--event",
            "subagent_stop",
            stdin={"agent_id": "ag-9", "agent_type": "L" * 250},
            env=COPPICE_ENV,
            coppice=True,
        )
    )
    return C


def kid(c: dict[str, Any]) -> str:
    """A case's body id, without what the oracle recorded or what is
    derived from it."""
    return body_id({k: v for k, v in c.items() if k != "go_expect"})


def all_cases() -> list[dict[str, Any]]:
    cases = (
        build_modules_cases()
        + build_dashboard_cases()
        + build_metrics_cases()
        + build_status_cases()
        + build_hook_cases()
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
                old[kid(c)] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(kid(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
        else:
            c["expect"] = run(c, cmd_for(c, None), SCRATCH / "gen" / f"{i:04d}")
        ge = go_expect_for(c)
        if ge:
            c["go_expect"] = ge
        if args.only:
            print(
                json.dumps(
                    {"name": c["name"], **c["expect"], "go_expect": ge},
                    indent=1,
                    ensure_ascii=False,
                )[:12000]
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
