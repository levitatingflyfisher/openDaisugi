"""Synthetic gate cases: hook payloads run through the Python gate, the oracle.

    uv run --no-sync python clients/gate_cases.py [--out clients/fixtures/gate]

Each case is one call of ``python -m opendaisugi.gate``: the argv, the raw
stdin, the gate root it starts from (registered envelopes, a disarm marker,
a config.yaml, an existing session tree), the environment, and what the
oracle did: exit code, stdout, stderr, and every line it wrote to the
shadow log, the session tree, the captures mirror and the coppice socket.
Volatile values (times, latencies, random ids) are normalized so a case is
a pure function of its inputs; ``clients/gate_compare.py`` runs any other
gate binary against the same cases and compares structurally.

Every path in a case is fake (/work, /home/user) except the run's own
scratch directory, written as ``{ROOT}``. HOME is /home/user for every run,
so the floor and credential rules see the same home on every machine. The
cases hold no content from any real session and may be committed.

The file is content-addressed like the conformance corpus
(docs/spec/conformance.md): each case's ``id`` is the first 16 hex digits
of the SHA-256 of its canonical JSON without the ``id`` key, lines are
sorted by id, and a manifest pins the file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "gate"
# DAISUGI_GATE_SCRATCH lets two runs on one box keep apart: each run
# empties its own work directories as it goes.
SCRATCH = Path(os.environ.get("DAISUGI_GATE_SCRATCH") or Path.home() / "opendaisugi-scratch" / "2b" / "runs")
FAKE_HOME = "/home/user"
CASE_VERSION = 1


def python_gate_cmd() -> list[str]:
    return [sys.executable, "-m", "opendaisugi.gate"]


def base_env() -> dict[str, str]:
    """The environment every gate run starts from: a fake HOME, and the
    repo's own src first on the import path so the oracle is this tree's."""
    return {
        "HOME": FAKE_HOME,
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
    }


def canonical_json(obj: Any) -> str:
    """Sorted keys, compact separators, and every non-ASCII character
    escaped: a gate case can hold a lone surrogate (it tests one), which
    raw UTF-8 cannot carry."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def case_id(body: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()[:16]


# ---------------------------------------------------------------------------
# Running one case
# ---------------------------------------------------------------------------


class FakeCoppice:
    """A unix socket that records each line a gate sends and answers ok."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.lines: list[str] = []
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(str(path))
        self.sock.listen(8)
        self.sock.settimeout(0.1)
        self._stop = False
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except (TimeoutError, OSError):
                continue
            with conn:
                conn.settimeout(1.0)
                buf = b""
                try:
                    while not buf.endswith(b"\n"):
                        chunk = conn.recv(65536)
                        if not chunk:
                            break
                        buf += chunk
                    conn.sendall(b'{"id": "r", "ok": true}\n')
                except OSError:
                    pass
                if buf:
                    self.lines.append(buf.decode("utf-8", "replace"))

    def close(self) -> list[str]:
        time.sleep(0.05)
        self._stop = True
        self.thread.join(1.0)
        self.sock.close()
        return self.lines


def prepare(case: dict[str, Any], workdir: Path) -> tuple[list[str], bytes, dict[str, str]]:
    """Lay out the case's gate root under workdir and return argv, stdin
    and the environment for one run."""
    if workdir.exists():
        shutil.rmtree(workdir)
    root = str(workdir)
    data = workdir / "data"
    gate_root = data / "gate"
    gate_root.mkdir(parents=True)
    st = case["state"]
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
    argv = [a.replace("{ROOT}", root) for a in case["argv"]]
    if case.get("stdin_hex"):
        stdin = bytes.fromhex(case["stdin_hex"])
    else:
        stdin = case["stdin"].replace("{ROOT}", root).encode("utf-8")
    env = base_env()
    env.update({k: v.replace("{ROOT}", root) for k, v in (case.get("env") or {}).items()})
    return argv, stdin, env


_HEX8 = re.compile(r"^[0-9a-f]{8}$")
_PLAN = re.compile(r"^plan_[0-9a-f]{8}$")
_ENV = re.compile(r"^env_[0-9a-f]{8}$")


def _norm_value(v: Any, root: str) -> Any:
    if isinstance(v, str):
        v = v.replace(root, "{ROOT}")
        if _PLAN.match(v):
            return "<plan>"
        if _ENV.match(v):
            return "<env>"
        return v
    if isinstance(v, list):
        return [_norm_value(x, root) for x in v]
    if isinstance(v, dict):
        return {k: _norm_value(x, root) for k, x in v.items()}
    return v


def _is_number(v: Any) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _stamp(row: dict[str, Any], keys: tuple[str, ...]) -> None:
    """Replace volatile numbers with a marker saying they were numbers."""
    for k in keys:
        if k in row:
            row[k] = "<num>" if _is_number(row[k]) else f"<not a number: {row[k]!r}>"


def _read_jsonl(path: Path) -> list[Any]:
    out = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            out.append({"<unparseable line>": line})
    return out


def observe(workdir: Path, proc: subprocess.CompletedProcess, coppice: list[str]) -> dict[str, Any]:
    """Everything a run did, normalized."""
    root = str(workdir)
    data = workdir / "data"
    shadow = []
    for f in sorted((data / "gate" / "shadow").glob("*.jsonl")):
        for rec in _read_jsonl(f):
            if isinstance(rec, dict):
                _stamp(rec, ("at", "elapsed_ms"))
            shadow.append({"file": f.name, "record": _norm_value(rec, root)})
    tree: dict[str, list[Any]] = {}
    for f in sorted((data / "sessions").glob("*.jsonl")) if (data / "sessions").exists() else []:
        ids: dict[str, str] = {}
        rows = []
        for row in _read_jsonl(f):
            if isinstance(row, dict):
                _stamp(row, ("ts", "latencyMs"))
                for k in ("id", "parentId", "leafId"):
                    if isinstance(row.get(k), str) and _HEX8.match(row[k]):
                        row[k] = ids.setdefault(row[k], f"#{len(ids) + 1}")
            rows.append(_norm_value(row, root))
        tree[f.name] = rows
    captures: dict[str, list[Any]] = {}
    capd = workdir / "captures"
    for f in sorted(capd.glob("*.jsonl")) if capd.exists() else []:
        rows = []
        for row in _read_jsonl(f):
            if isinstance(row, dict):
                _stamp(row, ("captured_at",))
            rows.append(_norm_value(row, root))
        captures[f.name] = rows
    sent = []
    for line in coppice:
        try:
            msg = json.loads(line)
            if isinstance(msg, dict) and isinstance(msg.get("event"), dict):
                _stamp(msg["event"], ("ts",))
            sent.append(_norm_value(msg, root))
        except json.JSONDecodeError:
            sent.append({"<unparseable line>": line})
    return {
        "exit": proc.returncode,
        "stdout": proc.stdout.decode("utf-8", "replace").replace(root, "{ROOT}"),
        "stderr": proc.stderr.decode("utf-8", "replace").replace(root, "{ROOT}"),
        "shadow": shadow,
        "tree": tree,
        "captures": captures,
        "coppice": sent,
    }


def run_case(
    case: dict[str, Any], cmd: list[str], workdir: Path, extra_env: dict[str, str] | None = None
) -> tuple[dict[str, Any], float]:
    """Run one case through one gate command; return (observed, seconds)."""
    argv, stdin, env = prepare(case, workdir)
    if extra_env:
        env.update(extra_env)
    server = None
    if case.get("coppice"):
        sock = workdir / "c.sock"
        env["COPPICE_SOCK"] = str(sock)
        env["COPPICE_PANE"] = "pane-7"
        server = FakeCoppice(sock)
    try:
        t0 = time.perf_counter()
        proc = subprocess.run(  # noqa: S603 - the gate under test
            cmd + argv, input=stdin, capture_output=True, env=env, cwd=workdir, timeout=120, check=False
        )
        elapsed = time.perf_counter() - t0
    finally:
        sent = server.close() if server else []
    return observe(workdir, proc, sent), elapsed


def summary(obs: dict[str, Any]) -> dict[str, Any]:
    """The verdict fields of an observation: what the host was told and
    what the gate logged it decided."""
    last = obs["shadow"][-1]["record"] if obs["shadow"] else {}
    return {
        "exit": obs["exit"],
        "host_verdict": host_verdict(obs),
        "allow": last.get("allow"),
        "would_deny": last.get("would_deny"),
        "tier": last.get("tier"),
        "reason": last.get("reason"),
    }


def host_verdict(obs: dict[str, Any]) -> str:
    """allow or deny, as the host reads the contract."""
    if obs["exit"] != 0:
        return "deny"
    out = obs["stdout"].strip()
    try:
        body = json.loads(out) if out else {}
    except json.JSONDecodeError:
        return "deny"
    if isinstance(body, dict) and (
        body.get("decision") == "block" or body.get("action") == "block" or body.get("block") is True
    ):
        return "deny"
    return "allow"


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------

ALLOW = ["cat", "cd", "echo", "git", "grep", "head", "ls", "pwd", "wc", "sort"]


def envelope(eid: str | None = "env_base", **perms: Any) -> dict[str, Any]:
    p: dict[str, Any] = {
        "file_read": ["/work/**"],
        "file_write": ["/work/**"],
        "shell": True,
        "shell_allowlist": ALLOW,
        "network": False,
        "mcp_allowlist": ["github/*"],
        "max_execution_time_s": 60,
        "max_output_size_mb": 20,
    }
    top = {k: perms.pop(k) for k in list(perms) if k in ("stakes", "shell_interpreter_policy", "invariants", "postconditions")}
    p.update(perms)
    env: dict[str, Any] = {"generated_by": "gate_cases", "task": "synthetic gate case", "permissions": p}
    if eid is not None:
        env["id"] = eid
    env.update(top)
    return env


BASE = {"default": envelope()}


def payload(tool: str | None, inp: Any = None, *, sid: Any = "s1", cwd: Any = "/work", **extra: Any) -> str:
    p: dict[str, Any] = {}
    if sid is not None:
        p["session_id"] = sid
    if tool is not None:
        p["tool_name"] = tool
    if inp is not None:
        p["tool_input"] = inp
    if cwd is not None:
        p["cwd"] = cwd
    p.update(extra)
    return json.dumps(p)


def bash(cmd: str, **kw: Any) -> str:
    return payload("Bash", {"command": cmd}, **kw)


def mk(
    name: str,
    stdin: str,
    *,
    mode: str | None = "enforce",
    fmt: str = "claude",
    envelopes: dict[str, Any] | None = None,
    disarmed: bool = False,
    config: str | None = None,
    sessions: dict[str, str] | None = None,
    env: dict[str, str] | None = None,
    pin: str | None = None,
    captures: bool = False,
    coppice: bool = False,
    extra_args: list[str] | None = None,
    tags: list[str] | None = None,
) -> dict[str, Any]:
    argv: list[str] = []
    if mode is not None:
        argv += ["--mode", mode]
    argv += ["--root", "{ROOT}/data/gate", "--format", fmt, "--verify-timeout", "5.0"]
    if pin is not None:
        argv += ["--session", pin]
    if captures:
        argv += ["--captures-root", "{ROOT}/captures"]
    argv += extra_args or []
    return {
        "kind": "gate",
        "v": CASE_VERSION,
        "name": name,
        "tags": tags or [],
        "argv": argv,
        "stdin": stdin,
        "env": env or {},
        "coppice": coppice,
        "state": {
            # Envelope files are kept as text: their key order is part of
            # what the gate reads, and the case file sorts keys.
            "envelopes": {
                k: v if isinstance(v, str) else json.dumps(v, indent=2)
                for k, v in (BASE if envelopes is None else envelopes).items()
            },
            "disarmed": disarmed,
            "config": config,
            "sessions": sessions or {},
        },
    }


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    WS = {"CLAUDE_PROJECT_DIR": "/work"}

    # -- tool kinds the classifier maps, both modes ------------------------
    for mode in ("enforce", "shadow"):
        add(mk(f"read inside ({mode})", payload("Read", {"file_path": "/work/a.txt"}), mode=mode, tags=["read"]))
        add(mk(f"read outside ({mode})", payload("Read", {"file_path": "/etc/passwd"}), mode=mode, tags=["read"]))
        add(mk(f"write outside ({mode})", payload("Write", {"file_path": "/etc/cron.d/x", "content": "x"}), mode=mode, tags=["write"]))
        add(mk(f"bash allowlist miss ({mode})", bash("rm -rf /work/build"), mode=mode, tags=["shell"]))
        add(mk(f"unknown tool ({mode})", payload("TodoWrite", {"todos": []}), mode=mode, tags=["unknown"]))
    add(mk("glob inside", payload("Glob", {"pattern": "/work/**/*.py"}), tags=["read"]))
    add(mk("grep path", payload("Grep", {"pattern": "x", "path": "/work/src"}), tags=["read"]))
    add(mk("edit inside", payload("Edit", {"file_path": "/work/a.py", "old_string": "a", "new_string": "b"}), tags=["edit"]))
    add(mk("multiedit outside", payload("MultiEdit", {"file_path": "/tmp/a.py", "edits": []}), tags=["edit"]))
    add(mk("write inside", payload("Write", {"file_path": "/work/new.txt", "content": "hello"}), tags=["write"]))
    add(mk("write list content", payload("Write", {"file_path": "/work/n.txt", "content": ["a", "b"]}), captures=True, tags=["write"]))
    add(mk("write numeric content", payload("Write", {"file_path": "/work/n.txt", "content": 5}), tags=["write", "malformed"]))
    add(mk("bash allowlist hit", bash("git status"), tags=["shell"]))
    add(mk("bash env prefix", bash("FOO=1 BAR=2 ls -la"), tags=["shell"]))
    add(mk("bash comment only", bash("# nothing to run"), tags=["shell"]))
    add(mk("bash empty command", bash("   "), tags=["shell"]))
    add(mk("bash cmd key", payload("Bash", {"cmd": "ls"}), tags=["shell"]))
    add(mk("webfetch denied", payload("WebFetch", {"url": "https://example.com/x"}), tags=["network"]))
    add(mk("webfetch allowed host", payload("WebFetch", {"url": "https://Example.com/x"}),
           envelopes={"default": envelope(network=True, network_hosts=["example.com"])}, tags=["network"]))
    add(mk("webfetch other host", payload("WebFetch", {"url": "https://evil.test/x"}),
           envelopes={"default": envelope(network=True, network_hosts=["example.com"])}, tags=["network"]))
    add(mk("webfetch file scheme", payload("WebFetch", {"url": "file:///etc/passwd"}),
           envelopes={"default": envelope(network=True)}, tags=["network"]))
    add(mk("websearch query", payload("WebSearch", {"query": "openDaisugi gate"}),
           envelopes={"default": envelope(network=True)}, tags=["network"]))
    add(mk("webfetch bracket host", payload("WebFetch", {"url": "http://[::1]:8080/"}),
           envelopes={"default": envelope(network=True)}, tags=["network"]))
    add(mk("mcp allowed", payload("mcp__github__get_issue", {"number": 3}), tags=["mcp"]))
    add(mk("mcp denied", payload("mcp__slack__post", {"text": "hi"}), tags=["mcp"]))
    add(mk("mcp nested tool name", payload("mcp__github__list__issues", {}), tags=["mcp"]))
    add(mk("mcp malformed name", payload("mcp__", {}), tags=["mcp", "unknown"]))
    add(mk("mcp list input", payload("mcp__github__x", ["a"]), tags=["mcp"]))
    def patch(text: Any, **kw: Any) -> str:
        return payload("mcp__opencode__apply_patch", {"patchText": text}, **kw)

    ap_narrow = {"default": envelope(file_write=["/work/src/**"])}
    for name, pl in [
        ("apply_patch add inside", patch("*** Begin Patch\n*** Add File: src/a.txt\n+x\n*** End Patch")),
        ("apply_patch update outside", patch("*** Begin Patch\n*** Update File: ../etc/x\n@@\n-a\n+b\n*** End Patch")),
        ("apply_patch mixed tiers", patch("*** Begin Patch\n*** Update File: src/a.py\n*** Add File: docs/b.md\n"
                                          "*** Delete File: /srv/c\n*** End Patch")),
        ("apply_patch move", patch("*** Begin Patch\n*** Update File: src/a.py\n*** Move to: other/a.py\n*** End Patch")),
        ("apply_patch no cwd", patch("*** Begin Patch\n*** Add File: src/a.txt\n*** End Patch", cwd=None)),
        ("apply_patch relative cwd", patch("*** Begin Patch\n*** Add File: src/a.txt\n*** End Patch", cwd="work")),
        ("apply_patch no header", patch("*** Begin Patch\n+x\n*** End Patch")),
        ("apply_patch no end", patch("*** Begin Patch\n*** Add File: src/a.txt\n")),
        ("apply_patch not text", patch(["*** Begin Patch"])),
        ("apply_patch padded", patch("  \n *** Begin Patch \n*** Add File:  src/a.txt  \n *** End Patch\n\n")),
        ("apply_patch heredoc", patch("<<'EOF'\n*** Begin Patch\n*** Add File: src/a.txt\n*** End Patch\nEOF")),
        ("apply_patch floor", patch("*** Begin Patch\n*** Add File: /home/user/.config/coppice/p.py\n*** End Patch")),
        ("apply_patch opencode plugin", patch("*** Begin Patch\n*** Update File: src/a.py\n"
                                              "*** Add File: .opencode/plugin/x.ts\n*** End Patch")),
    ]:
        add(mk(name, pl, fmt="opencode", envelopes=ap_narrow, env=WS, tags=["apply_patch"]))

    # -- formats -------------------------------------------------------------
    deny_bash = bash("rm -rf /")
    for fmt in ("hermes", "openclaw", "pi", "opencode", "codex", "nosuchhost"):
        add(mk(f"format {fmt} deny", deny_bash, fmt=fmt, tags=["format"]))
        add(mk(f"format {fmt} allow", bash("ls"), fmt=fmt, tags=["format"]))
    add(mk("hermes shadow deny", deny_bash, fmt="hermes", mode="shadow", tags=["format"]))
    add(mk("hermes shape", json.dumps({"event": "pre_tool_call", "tool": "shell", "args": {"command": "ls"},
                                       "session_id": "s1"}), fmt="hermes", tags=["format", "shell"]))
    add(mk("pi read", payload("read", {"path": "/work/a"}), fmt="pi", tags=["format", "read"]))
    add(mk("pi grep is mcp", payload("grep", {"pattern": "x"}), fmt="pi", tags=["format", "mcp"]))
    add(mk("pi lowercase under claude", payload("read", {"path": "/work/a"}), tags=["format", "unknown"]))
    add(mk("opencode filePath", payload("write", {"filePath": "src/x.ts", "content": "x"}), fmt="pi",
           tags=["format", "write"]))

    # -- paths ---------------------------------------------------------------
    add(mk("relative path with cwd", payload("Read", {"file_path": "src/a.py"}), tags=["paths"]))
    add(mk("relative path no cwd", payload("Read", {"file_path": "src/a.py"}, cwd=None), tags=["paths"]))
    add(mk("relative path relative cwd", payload("Read", {"file_path": "a.py"}, cwd="work"), tags=["paths"]))
    add(mk("dotdot path", payload("Read", {"file_path": "/work/../etc/passwd"}), tags=["paths"]))
    add(mk("tilde path", payload("Read", {"file_path": "~/notes.txt"}), tags=["paths"]))
    add(mk("dollar home path", payload("Write", {"file_path": "$HOME/x.txt", "content": "x"}), tags=["paths"]))
    add(mk("tilde user path", payload("Read", {"file_path": "~root/x"}), tags=["paths"]))
    add(mk("unicode path", payload("Read", {"file_path": "/work/café.txt"}), tags=["paths"]))
    add(mk("credential read", payload("Read", {"file_path": "/home/user/.ssh/id_rsa"}), env=WS, tags=["paths", "tier"]))
    add(mk("proc environ read", payload("Read", {"file_path": "/proc/self/environ"}), env=WS, tags=["paths", "tier"]))
    add(mk("pem read", payload("Read", {"file_path": "/work/certs/Server.PEM"}),
           envelopes={"default": envelope(file_read=["/work/src/**"])}, env=WS, tags=["paths", "tier"]))

    # -- tiers with a workspace ---------------------------------------------
    narrow = {"default": envelope(file_read=["/work/src/**"], file_write=["/work/allowed/**"], shell_allowlist=["ls"])}
    for name, pl in [
        ("tier write inside", payload("Write", {"file_path": "/work/other.txt", "content": "x"})),
        ("tier write git dir", payload("Write", {"file_path": "/work/.git/config", "content": "x"})),
        ("tier write outside", payload("Write", {"file_path": "/srv/x", "content": "x"})),
        ("tier read inside", payload("Read", {"file_path": "/work/README.md"})),
        ("tier read git dir", payload("Read", {"file_path": "/work/.git/HEAD"})),
        ("tier env file", payload("Read", {"file_path": "/work/.env.local"})),
        ("tier config dir", payload("Read", {"file_path": "/home/user/.config/gh/hosts.yml"})),
        ("tier pytest", bash("pytest -q tests/test_a.py")),
        ("tier pytest bad flag", bash("pytest --junitxml=out.xml")),
        ("tier git log", bash("git log --oneline -5")),
        ("tier git push force", bash("git push --force origin main")),
        ("tier git push", bash("git push origin main")),
        ("tier git commit file", bash("git commit -F /work/msg.txt")),
        ("tier git rev path", bash("git show HEAD:../secret")),
        ("tier curl get", bash("curl -sL https://example.com/a")),
        ("tier curl upload", bash("curl -d @/work/data https://example.com")),
        ("tier curl output", bash("curl -o out.html https://example.com")),
        ("tier rm", bash("rm -rf build")),
        ("tier kubectl", bash("kubectl apply -f x.yaml")),
        ("tier env", bash("env")),
        ("tier gh auth", bash("gh auth token")),
        ("tier gh pr", bash("gh pr create")),
        ("tier npm publish", bash("npm publish")),
        ("tier npm test", bash("npm test")),
        ("tier mkdir", bash("mkdir -p /work/newdir")),
        ("tier touch outside", bash("touch /etc/x")),
        ("tier cat home", bash("cat /home/user/notes")),
        ("tier grep home", bash("grep -r token /home/user")),
        ("tier find", bash("find /work -name '*.py'")),
        ("tier find exec", bash("find /work -exec rm {} ;")),
        ("tier jq env", bash("jq -n env")),
        ("tier head count", bash("head -5 /work/src/a.py")),
        ("tier dollar word", bash("cat $FILE")),
        ("tier glob word", bash("cat *.py")),
        ("tier go test", bash("go test -v ./...")),
        ("tier cargo test", bash("cargo test --release -- --nocapture")),
        ("tier uv pytest", bash("uv run --no-sync pytest -q")),
        ("tier python -m pytest", bash("python3 -m pytest -x")),
        ("tier ruff", bash("ruff check src")),
        ("tier stash drop", bash("git stash drop")),
        ("tier checkout branch", bash("git checkout -b feature")),
        ("tier checkout file", bash("git checkout -- a.py")),
        ("tier fetch refspec", bash("git fetch origin +main:main")),
        ("tier branch list", bash("git branch -a")),
        ("tier tag", bash("git tag v1")),
        ("tier unquoted tilde", bash("cat ~root/x")),
        ("webfetch tier", payload("WebFetch", {"url": "https://example.com"})),
        ("mcp tier", payload("mcp__slack__post", {"text": "x"})),
    ]:
        add(mk(name, pl, envelopes=narrow, env=WS, tags=["tier"]))
    add(mk("tier write inside no project dir", payload("Write", {"file_path": "/work/other.txt", "content": "x"}),
           envelopes=narrow, tags=["tier"]))
    add(mk("tier cwd outside project", payload("Write", {"file_path": "/work/o.txt", "content": "x"}, cwd="/srv"),
           envelopes=narrow, env=WS, tags=["tier"]))

    # -- compound shell ------------------------------------------------------
    dec = {"default": envelope(shell_allow_decomposition=True)}
    compound = [
        "ls && rm -rf /work",
        "ls && git status",
        "ls | grep x | wc -l",
        "cat < /etc/passwd",
        "echo hi > /work/out.txt",
        "echo hi > /etc/out.txt",
        "echo hi 2>/dev/null",
        "echo $(whoami)",
        "git log `whoami`",
        "$CMD foo",
        "echo hi > $OUT",
        "ls; cd /work && ls",
        "cd /tmp && cat x",
        "ls 'a|b' && echo \"c;d\"",
        "(ls)",
        "ls &",
        "ls\ngit status",
        "cat <<EOF\nhi\nEOF",
        "grep x <<< 'text'",
        "if true; then ls; fi",
        "for f in a b; do cat $f; done",
        "ls && sh -c 'rm x'",
    ]
    for i, cmd in enumerate(compound):
        add(mk(f"compound {i} no decomposition", bash(cmd), tags=["compound"]))
        add(mk(f"compound {i} decomposition", bash(cmd), envelopes=dec, env=WS, tags=["compound", "decomposition"]))

    # -- commands a review found the Go parser accepting and the oracle not --
    review = {"default": envelope(shell_allowlist=["echo", "ls"], shell_allow_decomposition=True)}
    for cmd in [
        "echo @(a|$(rm x)) ; ls", "echo !(a|$(rm x)) ; ls", "echo +($(rm x)) ; ls", "echo *(`rm x`) ; ls",
        "echo ?(<(rm x)) ; ls", "ls @($(rm x)) | ls", "[ a '<' b ] && ls", "[ a \"<\" b ] && ls",
        "[ a \\< b ] && ls", "[ \\( a \\) ] && ls", "[ a \\= b ] && ls", "echo @(a|b) ; ls", "{a,b} ; ls",
        "x#y 2>&1",
        # A redirect followed by more words: the oracle refuses it since
        # 0423900 (tree-sitter files the words as more targets).
        "ls | FOO=1 </work/i rm x", "rm </dev/null -rf /work", "cat <a b", "echo hi >out more",
        "ls 2>&1 x", "ls | FOO=1 </x rm", "cat 2>/dev/null <<< c",
    ]:
        add(mk(f"review {cmd}", bash(cmd), envelopes=review, tags=["review", "decomposition"]))
    add(mk("review glob blowup", payload("Read", {"file_path": "/" + "a/" * 40 + "c"}),
           envelopes={"default": envelope(file_read=["/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/b"])},
           tags=["review"]))
    # The glob step limit (verify.GLOB_MATCH_STEP_LIMIT): globs with three or
    # more ** segments just under it (a match, and a miss) and just over it.
    # A miss of "/" + "**/" * k + "c" against "/" + "a/" * n + "b" takes
    # 98,771 steps at k=3, n=80 and 102,341 at n=81; 91,391 and 101,271 at
    # k=4, n=34 and 35; 98,281 and 118,756 at k=5, n=21 and 22.
    def stars(k: int) -> str:
        return "/" + "**/" * k + "c"

    def deep(n: int, last: str = "b") -> str:
        return "/" + "a/" * n + last

    for name, glob, path in [
        ("glob steps three under", stars(3), deep(80)),
        ("glob steps three over", stars(3), deep(81)),
        ("glob steps three match", stars(3), deep(81, "c")),
        ("glob steps four under", stars(4), deep(34)),
        ("glob steps four over", stars(4), deep(35)),
        ("glob steps five under", stars(5), deep(21)),
        ("glob steps five over", stars(5), deep(22)),
        ("glob steps write over", stars(4), deep(35)),
    ]:
        tool, key = ("Write", "file_write") if "write" in name else ("Read", "file_read")
        inp = {"file_path": path, "content": ""} if tool == "Write" else {"file_path": path}
        add(mk(name, payload(tool, inp), envelopes={"default": envelope(**{key: [glob]})}, tags=["review", "glob"]))

    # -- a re-review's redirect enumeration: fds, dups and redirects after
    # words, under an envelope that allows every head and path, so any
    # refusal by the oracle shows as a deny ---------------------------------
    open_env = {"default": envelope(shell_allowlist=["*", "/*/*", "./*"], shell_allow_decomposition=True,
                                    file_read=["/**", "./**"], file_write=["/**", "./**"],
                                    shell_interpreter_policy="allow")}
    enum = (FIXTURE_DIR / "redirects.txt").read_text(encoding="utf-8").splitlines()
    for cmd in dict.fromkeys(c for c in enum if c.strip()):
        add(mk(f"redirect {cmd}", bash(cmd), envelopes=open_env, tags=["redirects"]))

    # -- long lines: many redirects, many distinct cds (50684bb caps the
    # cds followed at 32) -----------------------------------------------------
    long_env = {"default": envelope(shell_allowlist=["cd", "echo", "cp"], shell_allow_decomposition=True,
                                    file_read=["/**"], file_write=["/**"])}
    for n in (400, 1000):
        add(mk(f"long line {n} distinct cds", bash(" ; ".join(f"cd /work/d{i} ; echo {i} > o{i}" for i in range(n))),
               envelopes=long_env, env=WS, tags=["long"]))
    add(mk("long line 400 redirects", bash(" ; ".join(f"echo {i} > /work/o{i}" for i in range(400))),
           envelopes=long_env, env=WS, tags=["long"]))
    add(mk("floor word after the cd cap", bash(" ; ".join(f"cd /work/d{i}" for i in range(33))
                                                 + " ; cp a .config/coppice/x"),
           envelopes=long_env, tags=["long", "hard_deny"]))
    add(mk("floor word before the cd cap", bash(" ; ".join(f"cd /work/d{i}" for i in range(31))
                                                  + " ; cp a .config/coppice/x"),
           envelopes=long_env, tags=["long", "hard_deny"]))

    # -- interpreters --------------------------------------------------------
    strict = {"default": envelope(shell_allowlist=ALLOW + ["sh", "bash", "xargs", "timeout", "python3", "env", "find"],
                                  shell_interpreter_policy="strict")}
    surface = {"default": envelope(shell_allowlist=ALLOW + ["sh", "bash", "xargs", "timeout", "python3", "env", "find"])}
    for cmd in ["sh -c 'ls -la'", "sh -c 'rm -rf x'", "timeout 30 git fetch", "xargs rm", "python3 -c 'print(1)'",
                "env FOO=1 ls", "bash -c \"sh -c 'sh -c \\\"sh -c ls\\\"'\"", "find . -exec rm {} +",
                "sh -c 'ls && rm x'", "timeout 5 sh -c 'git status'"]:
        add(mk(f"interp strict {cmd}", bash(cmd), envelopes=strict, tags=["interpreter"]))
        add(mk(f"interp surface {cmd}", bash(cmd), envelopes=surface, tags=["interpreter"]))

    # -- hard-deny rules -----------------------------------------------------
    hard = [
        ("floor write config", payload("Write", {"file_path": "/home/user/.config/coppice/coppice.toml", "content": "x"})),
        ("floor write tilde", payload("Write", {"file_path": "~/.config/coppice/plugins/p.py", "content": "x"})),
        ("floor write data", payload("Write", {"file_path": "/home/user/.local/share/coppice/p", "content": "x"})),
        ("floor write opendaisugi coppice", payload("Write", {"file_path": "/home/user/.opendaisugi/coppice/x", "content": "x"})),
        ("floor read config allowed", payload("Read", {"file_path": "/home/user/.config/coppice/coppice.toml"})),
        ("floor bash cat", bash("cat ~/.config/coppice/coppice.toml")),
        ("floor bash home var", bash("echo x > $HOME/.local/share/coppice/p")),
        ("floor bash cd relative", bash("cd /home/user/.config && cat coppice/x")),
        ("floor bash cwd inside", bash("ls", cwd="/home/user/.config/coppice")),
        ("floor mcp arg", payload("mcp__github__x", {"path": "/home/user/.config/coppice/c.toml"})),
        ("floor mcp free text", payload("mcp__github__x", {"body": "see ~/.config/coppice for details"})),
        ("floor xdg", payload("Write", {"file_path": "/xdg/cfg/coppice/a", "content": "x"})),
        ("floor word after unfollowed cd", bash("cd .. && cp a .config/coppice/x", cwd="/home/user/work")),
        ("floor redirect after unfollowed cd", bash("cd .. && echo x > .config/coppice/x", cwd="/home/user/work")),
        ("floor plain word after unfollowed cd", bash("cd .. && cp a b/c", cwd="/home/user/work")),
        ("opencode word after unfollowed cd", bash("cd .. && cp a .config/opencode/x", cwd="/home/user/work")),
        ("floor word with no cwd", bash("cp a coppice/x", cwd=None)),
        ("opencode plugin write", payload("Write", {"file_path": "/work/.opencode/plugin/gate.ts", "content": "x"})),
        ("opencode tools write", payload("Write", {"file_path": "/work/.opencode/tools/t.ts", "content": "x"})),
        ("opencode json write", payload("Write", {"file_path": "/work/opencode.json", "content": "{}"})),
        ("opencode global", payload("Write", {"file_path": "/home/user/.config/opencode/x.ts", "content": "x"})),
        ("opencode bash grep", bash("grep plugin opencode.json")),
        ("opencode bash dir", bash("ls .opencode/")),
        ("opencode bash dir plain", bash("ls .opencode")),
        ("opencode bash other dir", bash("ls .opencode-old/x")),
        ("opencode git message", bash("git commit -m 'update opencode.json docs'")),
        ("opencode git message flag", bash("git commit --message=opencode.json")),
        ("opencode read", payload("Read", {"file_path": "/work/opencode.json"})),
        ("pane coppice allow", bash("coppice agent allow 42")),
        ("pane coppice allow opts", bash("coppice --socket /x agent allow 42")),
        ("pane coppice path", bash("/usr/local/bin/coppice agent allow 1")),
        ("pane text", bash("echo hi && coppice   agent 'allow' 3")),
        ("pane wire", bash("echo '{\"cmd\": \"agent.allow\"}' | nc -U s")),
        ("pane web token", bash("coppice web token")),
        ("pane web token file", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/token"})),
        ("pane ca key file", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/ca/ca.key"})),
        ("pane ca key cat", bash("cat /home/user/.opendaisugi/coppice/web/ca/ca.key")),
        ("pane leaf key file", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/ca/leaf.key"})),
        ("pane tailscale key file",
         payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/tls/tailscale.key"})),
        ("pane voice token file", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/voice/token"})),
        ("pane voice token cat", bash("cat /home/user/.opendaisugi/coppice/voice/token")),
        ("pane secret redirect", bash("python steal.py < /home/user/.opendaisugi/coppice/web/ca/ca.key")),
        ("pane secret cp source", bash("cp /home/user/.opendaisugi/coppice/web/ca/leaf.key /work/out")),
        ("pane secret from data dir", bash("cat web/ca/ca.key", cwd="/home/user/.opendaisugi/coppice")),
        ("pane secret mcp arg", payload("mcp__fs__read_file", {"path": "/home/user/.opendaisugi/coppice/voice/token"})),
        ("pane secret read double slash",
         payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web//ca/ca.key"})),
        ("pane secret read dot segment",
         payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/./ca/ca.key"})),
        ("pane secret read dotdot segment",
         payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/bogus/../ca/ca.key"})),
        ("pane secret mcp double slash",
         payload("mcp__fs__read_file", {"path": "/home/user/.opendaisugi/coppice/voice//token"})),
        ("pane ca cert allowed", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/ca/ca.crt"})),
        ("pane leaf cert allowed", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/ca/leaf.crt"})),
        ("pane ca meta allowed", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/ca/meta.json"})),
        ("pane web config allowed", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/web.json"})),
        ("pane answer route", bash("curl http://127.0.0.1:8080/api/ask/answer")),
        ("pane ask file", payload("Write", {"file_path": "{ROOT}/data/gate/asks/x.json", "content": "{}"})),
        ("pane default ask file", payload("Write", {"file_path": "/home/user/.opendaisugi/gate/answers/a.json", "content": "{}"})),
        ("pane ask text", bash("cat gate/asks/1.json")),
        ("pane asks from cwd", bash("ls asks", cwd="{ROOT}/data/gate")),
        ("pane mcp", payload("mcp__github__x", {"cmd": "coppice agent deny 3"})),
        ("pane mention allowed", bash("grep -r agent.allow src")),
        ("pane coppice deny passes", bash("coppice agent deny 42 h1")),
        ("pane deny with a reason passes", bash("coppice agent deny 42 h1 --reason 'not this one'")),
        ("pane deny through a socket", bash("coppice --socket /x agent deny 42 h1")),
        ("pane deny through remote", bash("coppice --remote ssh://box agent deny 42 h1")),
        ("pane deny runtime dir", bash("XDG_RUNTIME_DIR=/x coppice agent deny 42 h1")),
        ("pane deny over ssh", bash("ssh box 'coppice agent deny 42 h1'")),
        ("pane deny setsid", bash("setsid -f coppice agent deny 42 h1")),
        ("pane deny systemd-run", bash("systemd-run --user coppice agent deny 42 h1")),
        ("pane deny nohup", bash("nohup coppice agent deny 42 h1")),
        ("pane deny at", bash("echo 'coppice agent deny 42 h1' | at now")),
        ("pane deny tmux", bash("tmux send-keys -t o 'coppice agent deny 42 h1' Enter")),
        ("pane deny env setsid", bash("env -i A=1 setsid coppice agent deny 42 h1")),
        ("pane wire deny over ssh", bash("echo '{\"cmd\": \"agent.deny\"}' | ssh box nc -U s")),
        ("pane wrapper with no deny", bash("ssh box ls")),
        ("pane grep data dir", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice"})),
        ("pane grep web dir", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice/web"})),
        ("pane grep ca dir", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice/web/ca"})),
        ("pane grep tls dir", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice/web/tls"})),
        ("pane grep voice dir", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice/voice"})),
        ("pane grep web dir respelled",
         payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi/coppice/web/bogus/../"})),
        ("pane grep from web dir", payload("Grep", {"pattern": "."}, cwd="/home/user/.opendaisugi/coppice/web")),
        ("pane grep relative web dir", payload("Grep", {"pattern": ".", "path": "web"},
                                               cwd="/home/user/.opendaisugi/coppice")),
        ("pane grep unrelated web dir", payload("Grep", {"pattern": ".", "path": "/work/src/web"})),
        ("pane grep from unrelated web dir", payload("Grep", {"pattern": "."}, cwd="/work/web")),
        ("pane glob web dir", payload("Glob", {"pattern": "*", "path": "/home/user/.opendaisugi/coppice/web"})),
        ("pane glob from web dir", payload("Glob", {"pattern": "*"}, cwd="/home/user/.opendaisugi/coppice/web")),
        ("pane read from web dir", payload("Read", {"file_path": "/home/user/.opendaisugi/coppice/web/web.json"},
                                           cwd="/home/user/.opendaisugi/coppice/web")),
        ("pane wire deny passes", bash("echo '{\"cmd\": \"agent.deny\"}' | nc -U s")),
    ]
    for name, pl in hard:
        add(mk(name, pl, env={"XDG_CONFIG_HOME": "/xdg/cfg"} if "xdg" in name else None, tags=["hard_deny"]))
    custom = {"COPPICE_DATA_DIR": "/srv/cdata"}
    add(mk("pane custom data dir grep", payload("Grep", {"pattern": ".", "path": "/srv/cdata/web"}),
           env=custom, tags=["hard_deny"]))
    add(mk("pane custom data dir unnamed grep", payload("Grep", {"pattern": ".", "path": "/srv/cdata/web"}),
           tags=["hard_deny"]))
    add(mk("floor custom data dir respelling", bash("cd /srv/cdata/web && cat ./token"),
           env=custom, tags=["hard_deny"]))
    add(mk("floor custom data dir unnamed respelling", bash("cd /srv/cdata/web && cat ./token"),
           tags=["hard_deny"]))
    above = [
        ("search grep home", payload("Grep", {"pattern": ".", "path": "/home/user"})),
        ("search grep tilde", payload("Grep", {"pattern": ".", "path": "~"})),
        ("search grep root", payload("Grep", {"pattern": ".", "path": "/"})),
        ("search grep double slash", payload("Grep", {"pattern": ".", "path": "//"})),
        ("search grep opendaisugi", payload("Grep", {"pattern": ".", "path": "/home/user/.opendaisugi"})),
        ("search grep from home", payload("Grep", {"pattern": "."}, cwd="/home/user")),
        ("search grep narrow from home", payload("Grep", {"pattern": ".", "path": "/work/src"}, cwd="/home/user")),
        ("search glob home", payload("Glob", {"pattern": "*", "path": "/home/user"})),
        ("search glob from root", payload("Glob", {"pattern": "*"}, cwd="/")),
        ("search read in home", payload("Read", {"file_path": "/home/user/notes.txt"}, cwd="/home/user")),
        ("search rg tilde", bash("rg token ~")),
        ("search rg from home", bash("rg token", cwd="/home/user")),
        ("search grep -r tilde", bash("grep -r token ~")),
        ("search grep -rn dot home", bash("grep -rn token .", cwd="/home/user")),
        ("search cd then rg", bash("cd ~ && rg token")),
        ("search find exec", bash("find ~ -type f -exec cat")),
        ("search assign rg", bash("FOO=1 rg token ~/.opendaisugi")),
        ("search rg narrow", bash("rg token src")),
        ("search grep one file", bash("grep token ~/notes.txt")),
        ("search find names", bash("find ~ -name x")),
    ]
    home = "/home/user"
    for i, (cmd, cwd) in enumerate([
        # Each reaches the secrets (review round 1).
        ("grep -r -m 1 token", home), ("rg -t py token", home), ("rg -g '*' token", home),
        ("grep -r -A 2 token", home), ("rg -e token", home), ("echo $'it\\'s' ; rg token ~", "/work"),
        ("grep -d recurse token ~", "/work"), ("grep --directories=recurse token ~", "/work"),
        ("grep --directories recurse token ~", "/work"), ("grep --recur token ~", "/work"),
        ("grep --deref token ~", "/work"), ("grep -2 token ~", "/work"),
        ("grep --depth=3 token ~", "/work"), ("zgrep -r token ~", "/work"), ("ugrep token ~", "/work"),
        ("ug token", home), ("git grep --no-index token", home), ("git -C ~ grep --no-index token", "/work"),
        ("rg token /*", "/work"), ("rg --hidden token ~/.*", "/work"), ("rg --hidden token ~/.o*", "/work"),
        ("rg token /hom?/user", "/work"), ("rg token /[h]ome/user", "/work"),
        ("rg token {/home/user,/x}", "/work"), ("find /* -type f -exec cat", "/work"),
        ("find -- ~ -type f -exec cat", "/work"), ("find -O3 ~ -type f -exec cat", "/work"),
        ("find -D stat ~ -type f -exec cat", "/work"), ("cd - && rg token", "/work"),
        ("cd $HOME/x/.. && rg token", "/work"), ("cd - && rg token src", "/work"),
        ("rg token $PWD", "/work"), ("H=~; rg token $H", "/work"), ("rg token ~+", "/work"),
        ("rg token ~user", "/work"), ("rg token $(echo ~)", "/work"),
        ("for d in ~; do rg token $d; done", "/work"), ("env rg token ~", "/work"),
        ("timeout 5 rg token ~", "/work"), ("nice rg token ~", "/work"), ("\\rg token ~", "/work"),
        ("/usr/bin/rg token ~", "/work"), ("echo ~ | xargs rg token", "/work"),
        ("bash -c 'rg token ~'", "/work"), ("rg token -- ~", "/work"), ("rg --files ~", "/work"),
        ("grep -r token .", "user"), ("rg token", None),
        # Left to the envelope.
        ("rg -t py token src", "/work"), ("grep -r token /work/src", home),
        ("grep -rn token src lib", home), ("rg -e token /work/src", home), ("rg 'foo$' src", "/work"),
        ("rg token /work/*", "/work"), ("rg token src/*.py", "/work"), ("ls ~/.opendaisugi/*", "/work"),
        ("cd /work && rg token", "/"),
        # A glob that names a secret file is the pane rule's.
        ("cat ~/.opendaisugi/*/*/*", "/work"), ("cat ~/.opendaisugi/coppice/web/*", "/work"),
        ("cat ~/.opendaisugi/coppice/web/ca/*.key", "/work"),
        ("cp ~/.opendaisugi/coppice/voice/tok?n /work/x", "/work"),
    ]):
        above.append((f"search r1 {i:02d} {cmd}", bash(cmd, cwd=cwd)))
    many = "{" + ",".join(f"/a{i}" for i in range(1, 65)) + ",/home/user}"
    for i, (cmd, cwd) in enumerate([
        ("rg token /home/use{r..r}", "/work"), ("rg token /{a..z}ome/user", "/work"),
        ("rg token /home/use{01..03}", "/work"), ("rg token /home/{a..1}", "/work"),
        ("rg token " + many, "/work"), ("rg token /home/us{1..100}", "/work"),
        ("rg token {1..100}", home), ("sh -c \"sh -c 'sh -c \\\"rg token ~\\\"'\"", "/work"),
        ("cat ~/.opendaisugi/coppice/web/toke{m..o}", "/work"),
        ("rg token /work/src{1..3}", "/work"), ("rg token /home/{0..9..2}", "/work"),
        ("echo {1..100}", "/work"), ("cat /work/f{1..100}", "/work"),
        ("rg token {/x,{/home/user,/y}}", "/work"),
    ]):
        above.append((f"search r2 {i:02d} {cmd[:60]}", bash(cmd, cwd=cwd)))
    above += [
        ("search r1 grep file_path and path home",
         payload("Grep", {"pattern": ".", "file_path": "/work", "path": "/home/user"})),
        ("search r1 grep file_path and path web",
         payload("Grep", {"pattern": ".", "file_path": "/work", "path": "/home/user/.opendaisugi/coppice/web"})),
        ("search r1 grep no cwd", payload("Grep", {"pattern": "."}, cwd=None)),
        ("search r1 grep relative cwd", payload("Grep", {"pattern": "."}, cwd="user")),
        ("search r1 grep path with no cwd", payload("Grep", {"pattern": ".", "path": "/work"}, cwd=None)),
    ]
    for name, pl in above:
        add(mk(name, pl, tags=["hard_deny"]))
    add(mk("search grep above custom data dir", payload("Grep", {"pattern": ".", "path": "/srv"}),
           env=custom, tags=["hard_deny"]))
    add(mk("pane relative custom data dir", payload("Grep", {"pattern": ".", "path": "/work"}),
           env={"COPPICE_DATA_DIR": "cdata"}, tags=["hard_deny"]))
    add(mk("floor shadow still denies", payload("Write", {"file_path": "/home/user/.config/coppice/x", "content": "x"}),
           mode="shadow", tags=["hard_deny"]))

    # -- malformed payloads ------------------------------------------------
    for name, raw in [
        ("empty stdin", ""),
        ("whitespace stdin", "  \n\t "),
        ("not json", "tool_name=Read"),
        ("json list", "[1, 2]"),
        ("json null", "null"),
        ("json string", "\"Read\""),
        ("no tool name", "{}"),
        ("empty tool name", json.dumps({"tool_name": "", "tool": "Read", "tool_input": {"file_path": "/work/a"}})),
        ("tool key", json.dumps({"tool": "Read", "args": {"path": "/work/a"}, "session_id": "s1"})),
        ("numeric tool name", json.dumps({"tool_name": 5})),
        ("string tool input", json.dumps({"tool_name": "Read", "tool_input": "x", "session_id": "s1"})),
        ("numeric path", json.dumps({"tool_name": "Read", "tool_input": {"file_path": 7}, "session_id": "s1"})),
        ("numeric session", json.dumps({"tool_name": "Read", "tool_input": {"file_path": "/work/a"}, "session_id": 7})),
        ("zero session", json.dumps({"tool_name": "Read", "tool_input": {"file_path": "/work/a"}, "session_id": 0})),
        ("odd session chars", json.dumps({"tool_name": "Read", "tool_input": {"file_path": "/work/a"},
                                          "session_id": "../../etc/x y"})),
        ("numeric cwd", json.dumps({"tool_name": "Read", "tool_input": {"file_path": "a"}, "cwd": 5})),
        ("bom", "﻿" + payload("Read", {"file_path": "/work/a"})),
        ("nan in payload", '{"tool_name": "Read", "tool_input": {"file_path": "/work/a"}, "x": NaN}'),
        ("duplicate keys", '{"tool_name": "Read", "tool_name": "Bash", "tool_input": {"command": "ls"}}'),
        ("trailing garbage", payload("Read", {"file_path": "/work/a"}) + " x"),
        ("lone surrogate", '{"tool_name": "Read", "tool_input": {"file_path": "/work/\\ud800"}}'),
        ("control char command", bash("ls\x0bx")),
        ("join keys", payload("Read", {"file_path": "/work/a"}, tool_use_id="toolu_1", agent_id="ag", agent_type="sub",
                              transcript_path="/work/t.jsonl", hook_event_name="PreToolUse", permission_mode="default",
                              extra=1.5)),
        ("unicode in content", payload("Write", {"file_path": "/work/u.txt", "content": "café — \U0001f600"})),
    ]:
        add(mk(f"payload {name}", raw, tags=["malformed"]))
    bad = mk("invalid utf8 stdin", "", tags=["malformed"])
    bad["stdin_hex"] = b'{"tool_name": "Read", "tool_input": {"file_path": "/work/\xff"}}'.hex()
    add(bad)

    # -- envelopes ---------------------------------------------------------
    add(mk("no envelope", payload("Read", {"file_path": "/work/a"}), envelopes={}, tags=["envelope"]))
    add(mk("no envelope shadow", payload("Read", {"file_path": "/work/a"}), envelopes={}, mode="shadow", tags=["envelope"]))
    bound = {"default": envelope("env_default", shell_allowlist=["ls"]),
             "s-bound": envelope("env_bound", shell_allowlist=["ls", "make"])}
    add(mk("session bound envelope", bash("make build", sid="s-bound"), envelopes=bound, tags=["envelope"]))
    add(mk("session falls back to default", bash("make build", sid="s-other"), envelopes=bound, tags=["envelope"]))
    add(mk("pinned session ignores payload", bash("make build", sid="s-bound"), envelopes=bound, pin="s-other",
           tags=["envelope"]))
    add(mk("pinned session selects bound", bash("make build", sid="x"), envelopes=bound, pin="s-bound", tags=["envelope"]))
    add(mk("envelope without id", payload("Read", {"file_path": "/etc/x"}), envelopes={"default": envelope(None)},
           tags=["envelope"]))
    add(mk("inconsistent envelope", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(shell=False)}, tags=["envelope", "z3"]))
    add(mk("zero time envelope", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(max_execution_time_s=0)}, tags=["envelope", "z3"]))
    add(mk("shell forbidden", bash("ls"), envelopes={"default": envelope(shell=False, shell_allowlist=[])},
           tags=["envelope"]))
    add(mk("envelope with invariant", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(invariants=[{"type": "no_side_effects", "description": "d"}])},
           tags=["envelope", "predicate"]))
    add(mk("envelope with enforced expr", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(invariants=[{"type": "t", "description": "d", "expr": {
               "op": "forall_steps", "pred": {"op": "not_equals", "path": "type", "value": "network"}}}])},
           tags=["envelope", "predicate"]))
    add(mk("envelope with malformed expr", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(invariants=[{"type": "t", "description": "d", "expr": {
               "op": "forall_steps", "body": {"op": "not_equals", "path": "type", "value": "network"}}}])},
           tags=["envelope", "predicate"]))
    # re's FutureWarning patterns (nested sets, set operators), at both
    # compile sites: the vacuity check (sre_parse) and evaluation
    # (re.search), bare and inside nested groups. The hook's stderr must
    # hold the verdict alone.
    for i, (regex, op, where) in enumerate([
        ("[[:alpha:]]", "not_matches", "invariants"),
        ("(a([[b]))", "matches", "invariants"),
        ("((((x[a--b]))))", "not_matches", "postconditions"),
        ("[a&&b]|R", "matches", "postconditions"),
        ("^(?:[a||b]|Read)$", "matches", "invariants"),
        ("[a~~b]", "not_matches", "invariants"),
    ]):
        pred = {"op": op, "path": "tool" if i % 2 else "type", "regex": regex}
        item = {"type": "t", "description": "d", "expr": {"op": "forall_steps", "pred": pred} if i % 3 else pred}
        add(mk(f"envelope warning regex {i}", payload("Read", {"file_path": "/work/a"}),
               envelopes={"default": envelope(**{where: [item]})}, tags=["envelope", "predicate"]))
    # llm_check: no key; a key and a port nothing listens on; physical
    # stakes; inside a quantifier and under or/not. Never a real model.
    llm = {"op": "llm_check", "rule": "reads only"}
    refused = {"ANTHROPIC_API_KEY": "sk-test", "ANTHROPIC_API_BASE": "http://127.0.0.1:1",
               "OPENDAISUGI_LLM_BACKEND": "litellm"}
    for name, expr, env, fmt, extra in [
        ("llm no key", llm, {}, "claude", {}),
        ("llm refused", llm, refused, "claude", {}),
        ("llm refused hermes", llm, refused, "hermes", {}),
        ("llm refused openclaw", {"op": "not", "child": llm}, refused, "openclaw", {}),
        ("llm physical", llm, refused, "claude", {"stakes": "physical"}),
        ("llm under or", {"op": "or", "children": [{"op": "equals", "path": "type", "value": "file_read"}, llm]},
         refused, "claude", {}),
        ("llm in forall", {"op": "forall_steps", "pred": llm}, refused, "claude", {}),
    ]:
        item = {"type": "t", "description": "d", "expr": expr}
        # litellm's import alone can outrun 5 s on a loaded box, which made
        # the expectation depend on the box; 30 s does not.
        add(mk(name, payload("Read", {"file_path": "/work/a"}), fmt=fmt, env=env,
               envelopes={"default": envelope(invariants=[item], **extra)}, tags=["envelope", "predicate", "llm"],
               extra_args=["--verify-timeout", "30"]))
    add(mk("envelope coerced bool", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(network="yes")}, tags=["envelope"]))
    add(mk("envelope broken json", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": "{\"generated_by\": "}, tags=["envelope"]))
    add(mk("envelope missing task", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": {"generated_by": "x", "permissions": {}}}, tags=["envelope"]))
    # A registered envelope with NaN or an infinity in any number is
    # invalid (models.non_finite_error): an unreadable envelope, never a
    # null limit. JSON has no such literal; Python's reader takes NaN,
    # Infinity, -Infinity, and a number too large for a float.
    def raw_env(**perms: Any) -> str:
        return json.dumps(envelope(**perms), indent=2)

    for name, text in [
        ("nan velocity", raw_env(velocity_limit=float("nan"))),
        ("infinity torque", raw_env(torque_limit=float("inf"))),
        ("minus infinity bound", raw_env(workspace_bounds=[[0, 0, 0], [1, float("-inf"), 1]])),
        ("1e999 joint limit", raw_env(joint_limits={"j": [0, 1]}).replace("1\n", "1e999\n", 1)),
        ("nan string", raw_env(velocity_limit=" nan ")),
        ("nan in expr", raw_env(invariants=[{"type": "t", "description": "d", "enforce": False,
                                             "expr": {"op": "equals", "path": "x", "value": [1, float("nan")]}}])),
        ("two non-finite", raw_env(velocity_limit=float("inf"), torque_limit=float("nan"))),
    ]:
        for mode in ("enforce", "shadow"):
            add(mk(f"envelope {name} ({mode})", payload("Read", {"file_path": "/work/a"}), mode=mode,
                   envelopes={"default": text}, tags=["envelope", "non_finite"]))
    add(mk("envelope nan velocity hermes", payload("Read", {"file_path": "/work/a"}), fmt="hermes",
           envelopes={"default": raw_env(velocity_limit=float("nan"))}, tags=["envelope", "non_finite"]))
    # The plan the gate builds for an MCP call is invalid the same way when
    # the call's arguments hold such a number: an internal error, a deny.
    for mode in ("enforce", "shadow"):
        add(mk(f"mcp nan argument ({mode})", payload("mcp__github__get_issue", {"n": float("nan"), "ok": 1.5}),
               mode=mode, tags=["mcp", "non_finite"]))
    add(mk("mcp nested infinities", payload("mcp__github__get_issue", {"a": {"b": [1, float("-inf")]},
                                                                        "c": float("inf")}),
           tags=["mcp", "non_finite"]))
    add(mk("mcp 1e999 argument", payload("mcp__github__get_issue", {"n": 7}).replace(": 7", ": 1e999"),
           tags=["mcp", "non_finite"]))
    add(mk("mcp nan argument hermes", payload("mcp__github__get_issue", {"n": float("nan")}), fmt="hermes",
           tags=["mcp", "non_finite"]))
    add(mk("mcp nan argument not allowed", payload("mcp__slack__post", {"n": float("nan")}),
           tags=["mcp", "non_finite"]))
    add(mk("envelope nan session file", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(), "s1": raw_env(velocity_limit=float("nan"))},
           tags=["envelope", "non_finite"]))
    add(mk("envelope glob allowlist", bash(".venv/bin/pytest -q"),
           envelopes={"default": envelope(shell_allowlist=[".venv/bin/*"])}, tags=["envelope", "shell"]))
    add(mk("envelope physical stakes", payload("Read", {"file_path": "/work/a"}),
           envelopes={"default": envelope(stakes="physical")}, tags=["envelope"]))

    # -- gate root state -----------------------------------------------------
    add(mk("disarmed enforce", bash("rm -rf /"), disarmed=True, tags=["disarm"]))
    add(mk("disarmed hermes", bash("rm -rf /"), disarmed=True, fmt="hermes", tags=["disarm"]))
    add(mk("config names python", bash("ls"), config="verifier_client: python\ngate_mode: enforce\n", tags=["config"]))
    add(mk("config names go", bash("ls"), config="verifier_client: go\n", tags=["config"]))
    add(mk("no mode uses config", bash("rm -rf /"), mode=None, config="gate_mode: enforce\n", tags=["config"]))
    add(mk("no mode no config", bash("rm -rf /"), mode=None, tags=["config"]))
    tree_seed = (
        json.dumps({"type": "session", "id": "s1", "ts": 1.0, "v": 1, "harness": "claude-code", "cwd": "/work"}) + "\n"
        + json.dumps({"type": "tool_call", "id": "aaaa0001", "parentId": None, "ts": 2.0}) + "\n"
        + json.dumps({"type": "verdict", "id": "aaaa0002", "parentId": "aaaa0001", "ts": 3.0}) + "\n"
        + json.dumps({"type": "state", "id": "aaaa0003", "parentId": "aaaa0002", "ts": 4.0}) + "\n"
        + "{\"type\": \"verdict\", \"id\": \"torn\n"
    )
    add(mk("existing session tree", payload("Read", {"file_path": "/work/a"}), sessions={"s1": tree_seed}, tags=["tree"]))
    head_seed = (
        json.dumps({"type": "session", "id": "s1", "ts": 1.0}) + "\n"
        + json.dumps({"type": "prompt", "id": "bbbb0001", "parentId": None, "ts": 2.0}) + "\n"
        + json.dumps({"type": "head", "leafId": "bbbb0000", "ts": 3.0}) + "\n"
        + json.dumps({"type": "label", "id": "bbbb0002", "parentId": "bbbb0001", "ts": 4.0}) + "\n"
    )
    add(mk("session tree head entry", payload("Read", {"file_path": "/work/a"}), sessions={"s1": head_seed}, tags=["tree"]))
    add(mk("captures mirror allow", payload("Read", {"file_path": "/work/a"}, tool_use_id="t9"), captures=True,
           tags=["captures"]))
    add(mk("captures skip deny", payload("Read", {"file_path": "/etc/a"}), captures=True, tags=["captures"]))
    add(mk("captures pi tool", payload("read", {"path": "/work/a"}), fmt="pi", captures=True, tags=["captures"]))
    add(mk("coppice report allow", payload("Read", {"file_path": "/work/a"}), coppice=True, tags=["coppice"]))
    add(mk("coppice report deny", bash("rm -rf /"), coppice=True, tags=["coppice"]))
    add(mk("coppice report unicode clause", payload("TodoWrite", {}), coppice=True, tags=["coppice"]))
    add(mk("coppice tmux pane", payload("Read", {"file_path": "/work/a"}), coppice=True,
           env={"TMUX_PANE": "%3"}, tags=["coppice"]))
    add(mk("herdr pane", payload("Read", {"file_path": "/work/a"}), env={"HERDR_PANE_ID": "h1"}, tags=["coppice"]))
    add(mk("ask flag", bash("rm -rf /"), extra_args=["--ask", "--ask-timeout", "1"], tags=["ask"]))
    add(mk("checkpoints flag", payload("Read", {"file_path": "/work/a"}), extra_args=["--checkpoints"], tags=["ask"]))
    add(mk("abbreviated flag", payload("Read", {"file_path": "/work/a"}), mode=None, extra_args=["--mo", "enforce"],
           tags=["argv"]))
    add(mk("flag equals form", payload("Read", {"file_path": "/work/a"}), mode=None, extra_args=["--mode=enforce"],
           tags=["argv"]))
    # A bad flag denies on stdout for a format whose host never reads the
    # exit code (hermes, openclaw), not just via the empty-stdout exit-2
    # body that format never sees; claude keeps that plain exit-2 deny.
    for fmt in ("hermes", "openclaw", "claude"):
        add(mk(f"format {fmt} bad flag", payload("Read", {"file_path": "/work/a"}), fmt=fmt,
               extra_args=["--no-such-flag"], tags=["argv", "format"]))
    return C


# ---------------------------------------------------------------------------
# Writing the fixture
# ---------------------------------------------------------------------------


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument("--check", action="store_true", help="run every case twice and fail on any difference")
    args = ap.parse_args()
    cases = build_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    SCRATCH.mkdir(parents=True, exist_ok=True)
    out_lines = []
    cmd = python_gate_cmd()
    for i, c in enumerate(cases):
        work = SCRATCH / "gen" / f"{i:04d}"
        obs, _ = run_case(c, cmd, work)
        if args.check:
            again, _ = run_case(c, cmd, work)
            if again != obs:
                raise SystemExit(f"case {c['name']!r} is not deterministic")
        body = dict(c)
        body["expect"] = {"summary": summary(obs), **obs}
        body["id"] = case_id(body)
        out_lines.append(body)
        print(f"{i + 1:4d}/{len(cases)} {summary(obs)['host_verdict']:5s} exit={obs['exit']} {c['name']}", flush=True)
    out_lines.sort(key=lambda c: c["id"])
    args.out.mkdir(parents=True, exist_ok=True)
    path = args.out / "cases.jsonl"
    data = "".join(canonical_json(c) + "\n" for c in out_lines).encode("utf-8")
    path.write_bytes(data)
    manifest = {"v": CASE_VERSION, "count": len(out_lines), "sha256": hashlib.sha256(data).hexdigest()}
    (args.out / "cases.manifest.json").write_text(json.dumps(manifest, indent=1) + "\n", encoding="utf-8")
    print(f"wrote {len(out_lines)} cases to {path}")
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    return 0


if __name__ == "__main__":
    os.environ.setdefault("CUDA_VISIBLE_DEVICES", "")
    raise SystemExit(main())
