"""Synthetic CLI cases: the gate commands and `install --gate`, run through
the Python CLI, the oracle.

    uv run --no-sync python clients/cli_cases.py [--out clients/fixtures/cli]

A case is a file tree plus one command. The runner lays the tree out as a
scratch HOME, runs the command there with that HOME, and records what the
command did: exit code, stdout, stderr and the tree after it. The Go
`daisugi` binary runs the same cases (`clients/cli_compare.py`) and the two
are compared structurally.

Every path in a case is fake. The scratch HOME is written ``{HOME}``, the
Python interpreter ``{PYTHON}``. The cases hold no content from any real
session and may be committed.

Two command kinds need an oracle other than the plain CLI:

- ``install --gate``: Python's install also writes the skill, MCP, capture
  and instruction layers. The Go binary writes only the gate layer, so the
  oracle runs the Python CLI with ``DEFAULT_LAYERS`` emptied.
- ``install --gate --uninstall``: Python's uninstall reverses every layer.
  The oracle runs it with every reversal but the gate's own made a no-op.

A case marked ``go_refuses`` holds a flag the Go binary does not handle.
There the check is only that Go exits non-zero, prints one line naming the
Python command, and leaves the tree as it was.

The file is content-addressed like the gate cases: each ``id`` is the first
16 hex digits of the SHA-256 of the case's canonical JSON without the id,
lines are sorted by id, and a manifest pins the file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "cli"
SCRATCH = Path(
    os.environ.get("DAISUGI_CLI_SCRATCH") or Path.home() / "opendaisugi-scratch" / "2c" / "runs"
)
CASE_VERSION = 1

# The oracle, with the install layers the Go binary does not write turned
# off. Both shims then run the real CLI entry.
_GATE_ONLY_INSTALL = """
import sys
import opendaisugi.install as i
i.DEFAULT_LAYERS = frozenset()
from opendaisugi.cli import main
sys.argv = ["daisugi"] + sys.argv[1:]
main()
"""

_GATE_ONLY_UNINSTALL = """
import sys
import opendaisugi.install as i
_pop = i._pop_json_hook
def _gate_pop(path, *, match, events=("PreToolUse",)):
    if match is i.is_record_hook and tuple(events) == ("PreToolUse",):
        return []
    return _pop(path, match=match, events=events)
i._pop_json_hook = _gate_pop
none = lambda *a, **k: []
for name in ("_remove_skill_both", "_remove_skill", "_pop_json_mcp", "_unpatch_instructions",
             "_pop_json_env_key", "_unpatch_codex_base_url"):
    setattr(i, name, none)
i.HermesRuntime.reverse = lambda self, home: []
i.OpenClawRuntime.reverse = lambda self, home: []
from opendaisugi.cli import main
sys.argv = ["daisugi"] + sys.argv[1:]
main()
"""


def python_cmd(case: dict[str, Any]) -> list[str]:
    oracle = case.get("oracle", "cli")
    if oracle == "gate-only-install":
        return [sys.executable, "-c", _GATE_ONLY_INSTALL]
    if oracle == "gate-only-uninstall":
        return [sys.executable, "-c", _GATE_ONLY_UNINSTALL]
    return [sys.executable, "-m", "opendaisugi.cli"]


def canonical_json(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def case_id(body: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()[:16]


# ---------------------------------------------------------------------------
# Laying out and reading back a tree
# ---------------------------------------------------------------------------


def _sub(s: str, home: str) -> str:
    return s.replace("{HOME}", home)


def lay_out(tree: dict[str, Any], home: Path) -> None:
    """Write a tree spec under home. Directories are made first, deepest
    last, so a file's mode is never changed by a later mkdir."""
    h = str(home)
    home.mkdir(parents=True)
    os.chmod(home, 0o755)
    items = sorted(tree.items(), key=lambda kv: kv[0])
    for rel, spec in items:
        p = home / rel
        if "dir" in spec:
            p.mkdir(parents=True, exist_ok=True)
    for rel, spec in items:
        p = home / rel
        if "dir" in spec:
            continue
        p.parent.mkdir(parents=True, exist_ok=True)
        if "link" in spec:
            os.symlink(_sub(spec["link"], h), p)
        else:
            p.write_bytes(_sub(spec["text"], h).encode("utf-8"))
    for rel, spec in sorted(items, key=lambda kv: -len(kv[0])):
        if "mode" in spec and "link" not in spec:
            os.chmod(home / rel, spec["mode"])


_BAK = re.compile(r"\.bak\d+(\.\d+)?$")
_ENV_ID = re.compile(r"^env_[0-9a-f]{8}$")


def norm_command(cmd: str) -> str:
    """A gate hook command with its entry point replaced by {GATE}, so the
    Python form (python -m opendaisugi.gate_client ...) and the Go form
    (NAME=... daisugi gate check ...) compare on the flags they pass."""
    if "opendaisugi.gate" not in cmd and " gate check" not in cmd:
        return cmd
    try:
        toks = shlex.split(cmd)
    except ValueError:
        return cmd
    while toks and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", toks[0]):
        toks = toks[1:]
    if len(toks) >= 3 and toks[1] == "-m" and toks[2].startswith("opendaisugi.gate"):
        toks = ["{GATE}"] + toks[3:]
    elif len(toks) >= 3 and toks[1:3] == ["gate", "check"]:
        toks = ["{GATE}"] + toks[3:]
    return " ".join(toks)


def norm_json(v: Any, home: str) -> Any:
    if isinstance(v, str):
        v = v.replace(home, "{HOME}").replace(sys.executable, "{PYTHON}")
        if _ENV_ID.match(v):
            return "<env>"
        return v
    if isinstance(v, list):
        return [norm_json(x, home) for x in v]
    if isinstance(v, dict):
        out = {}
        for k, x in v.items():
            if k == "command" and isinstance(x, str):
                x = norm_command(x)
            out[k] = norm_json(x, home)
        return out
    return v


# Machine paths that may reach output: the oracle's own source tree and
# the interpreter's install. No fixture may carry them (fixture_paths.py).
_MACHINE_PATHS = sorted(
    {(str(REPO / "src"), "{SRC}"), (sys.prefix, "{PY}"), (sys.base_prefix, "{PY}")},
    key=lambda kv: -len(kv[0]),
)


def norm_text(s: str, home: str) -> str:
    s = s.replace(home, "{HOME}").replace(sys.executable, "{PYTHON}")
    for path, mark in _MACHINE_PATHS:
        s = s.replace(path, mark)
    return s


def read_tree(home: Path) -> dict[str, Any]:
    """Every entry under home: its kind, mode and content. JSON files are
    compared parsed and normalized; backups lose their time stamp."""
    h = str(home)
    out: dict[str, Any] = {}
    if not home.exists():
        return out
    for dirpath, dirnames, filenames in os.walk(home):
        dirnames[:] = [d for d in dirnames if d != "__pycache__"]
        for name in sorted(dirnames) + sorted(filenames):
            p = Path(dirpath) / name
            rel = str(p.relative_to(home))
            rel = _BAK.sub(".bak", rel)
            st = os.lstat(p)
            if stat.S_ISLNK(st.st_mode):
                entry: dict[str, Any] = {"link": norm_text(os.readlink(p), h)}
            elif stat.S_ISDIR(st.st_mode):
                entry = {"dir": True, "mode": stat.S_IMODE(st.st_mode)}
            else:
                raw = p.read_bytes()
                entry = {"mode": stat.S_IMODE(st.st_mode)}
                try:
                    text = raw.decode("utf-8")
                except UnicodeDecodeError:
                    entry["hex"] = raw.hex()
                else:
                    parsed = None
                    if name.endswith(".json") or ".json.bak" in rel:
                        try:
                            parsed = json.loads(text)
                        except ValueError:
                            parsed = None
                    if parsed is not None:
                        entry["json"] = norm_json(parsed, h)
                    else:
                        entry["text"] = norm_text(text, h)
            if rel in out:  # two backups of one file: keep both
                n = 2
                while f"{rel}#{n}" in out:
                    n += 1
                rel = f"{rel}#{n}"
            out[rel] = entry
    return out


def norm_stdout(s: str, home: str, argv: list[str]) -> Any:
    s = norm_text(s, home)
    # gate settings prints one JSON document; compare it parsed.
    if argv[:2] == ["gate", "settings"]:
        try:
            return {"json": norm_json(json.loads(s), home)}
        except ValueError:
            return s
    return s


_WARN = re.compile(r"^.*?:\d+: (\w*Warning): (.*)$")


def norm_stderr(s: str, home: str) -> list[str]:
    """Python's warnings print as `file:line: UserWarning: text` plus the
    source line; keep only `UserWarning: text`. A traceback keeps only its
    last line, the exception: its frames name files on this machine."""
    out: list[str] = []
    lines = norm_text(s, home).splitlines()
    if "Traceback (most recent call last):" in lines:
        start = lines.index("Traceback (most recent call last):")
        # The frames are indented; the exception is the first line after
        # them that is not (a pydantic message goes on below it).
        exc = next((ln for ln in lines[start + 1 :] if ln and not ln.startswith(" ")), "")
        lines = lines[:start] + ["Traceback (most recent call last): ...", exc]
    skip = False
    for line in lines:
        if skip:
            skip = False
            if line.startswith("  "):
                continue
        m = _WARN.match(line)
        if m:
            out.append(f"{m.group(1)}: {m.group(2)}")
            skip = True
            continue
        out.append(line)
    return out


# ---------------------------------------------------------------------------
# Running one case
# ---------------------------------------------------------------------------


def prepare(case: dict[str, Any], work: Path) -> tuple[Path, list[str], dict[str, str], Path]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    lay_out(case.get("before") or {}, home)
    bindir = work / "bin"
    bindir.mkdir(parents=True)
    for name in case.get("path_bins") or []:
        b = bindir / name
        b.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(b, 0o755)
    h = str(home)
    env = {
        "HOME": h,
        "PATH": f"{bindir}:/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "COLUMNS": "100",
    }
    env.update({k: _sub(v, h) for k, v in (case.get("env") or {}).items()})
    cwd = home / case.get("cwd", "")
    cwd.mkdir(parents=True, exist_ok=True)
    argv = [_sub(a, h) for a in case["argv"]]
    return home, argv, env, cwd


def run_case(
    case: dict[str, Any], cmd: list[str], work: Path, extra_env: dict[str, str] | None = None
) -> tuple[dict[str, Any], float]:
    home, argv, env, cwd = prepare(case, work)
    if extra_env:
        env.update(extra_env)
    old = os.umask(0o022)
    try:
        t0 = time.perf_counter()
        proc = subprocess.run(  # noqa: S603 - the CLI under test
            cmd + argv,
            input=_sub(case.get("stdin", ""), str(home)).encode("utf-8"),
            capture_output=True,
            env=env,
            cwd=cwd,
            timeout=120,
            check=False,
        )
        elapsed = time.perf_counter() - t0
    finally:
        os.umask(old)
    h = str(home)
    obs = {
        "exit": proc.returncode,
        "stdout": norm_stdout(proc.stdout.decode("utf-8", "replace"), h, case["argv"]),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace"), h),
        "tree": read_tree(home),
    }
    return obs, elapsed


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------


def envelope_json(**perms: Any) -> str:
    p: dict[str, Any] = {
        "file_read": ["/work/**"],
        "file_write": ["/work/**"],
        "shell": True,
        "shell_allowlist": ["git", "ls"],
        "network": False,
        "max_execution_time_s": 60,
    }
    top = {k: perms.pop(k) for k in list(perms) if k in ("stakes", "id", "summary", "invariants")}
    p.update(perms)
    body: dict[str, Any] = {"generated_by": "cli_cases", "task": "synthetic", "permissions": p}
    body.update(top)
    return json.dumps(body, indent=2)


def f(text: str, mode: int = 0o644) -> dict[str, Any]:
    return {"text": text, "mode": mode}


def d(mode: int = 0o755) -> dict[str, Any]:
    return {"dir": True, "mode": mode}


def mk(name: str, argv: list[str], *, before: dict[str, Any] | None = None, **kw: Any) -> dict[str, Any]:
    c: dict[str, Any] = {"kind": "cli", "v": CASE_VERSION, "name": name, "argv": argv, "before": before or {}}
    c.update(kw)
    return c


GROOT = ".opendaisugi/gate"
ENVD = f"{GROOT}/envelopes"
CLAUDE = ".claude/settings.json"


def hook_settings(cmd: str, extra: dict[str, Any] | None = None) -> str:
    s: dict[str, Any] = {
        "hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": cmd}]}]}
    }
    if extra:
        s.update(extra)
    return json.dumps(s, indent=2)


PY_ENFORCE = "/usr/bin/python3 -m opendaisugi.gate_client --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
PY_SHADOW = "/usr/bin/python3 -m opendaisugi.gate_client --mode shadow --root /x --format claude --verify-timeout 10.0"


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    # -- arm / disarm -------------------------------------------------------
    add(mk("disarm fresh", ["gate", "disarm"]))
    add(mk("disarm loose root", ["gate", "disarm"], before={GROOT: d(0o755), ".opendaisugi": d(0o755)}))
    add(mk("disarm twice", ["gate", "disarm"], before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("old\n")}))
    add(mk("disarm custom root", ["gate", "disarm", "--root", "{HOME}/r/g"]))
    add(mk("disarm root equals", ["gate", "disarm", "--root={HOME}/r2"]))
    add(mk("arm disarmed", ["gate", "arm"], before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x\n")}))
    add(mk("arm not disarmed", ["gate", "arm"]))
    add(mk("arm custom root", ["gate", "arm", "--root", "{HOME}/g"], before={"g/DISARMED": f("x")}))
    add(mk("arm extra arg", ["gate", "arm", "extra"]))

    # -- status -------------------------------------------------------------
    envs = {
        ENVD: d(0o700),
        f"{ENVD}/default.json": f(envelope_json(), 0o600),
        f"{ENVD}/s-1.json": f(envelope_json(), 0o600),
    }
    add(mk("status empty", ["gate", "status"]))
    add(mk("status empty json", ["gate", "status", "--json"]))
    add(mk("status envelopes", ["gate", "status"], before=envs))
    add(mk("status envelopes json", ["gate", "status", "--json"], before=envs))
    odd = dict(envs)
    odd.update({
        f"{ENVD}/.hidden.json": f("{}"),
        f"{ENVD}/X.JSON": f("{}"),
        f"{ENVD}/notes.txt": f("x"),
        f"{ENVD}/dir.json": d(),
        f"{ENVD}/b c.json": f("{}"),
        f"{ENVD}/a.json.json": f("{}"),
    })
    add(mk("status odd names", ["gate", "status", "--json"], before=odd))
    add(mk("status disarmed", ["gate", "status"], before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x")}))
    # A hook whose program is gone fails on every call; status warns.
    go_hook = ("DAISUGI_GATE_HOOK=opendaisugi.gate {HOME}/%s gate check --mode %s --root /x "
               "--format claude --verify-timeout 10.0")
    add(mk("status hook program gone enforce", ["gate", "status"],
           before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "enforce") + " || exit 2"))}))
    add(mk("status hook program gone shadow json", ["gate", "status", "--json"],
           before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "shadow")))}))
    add(mk("status hook program present", ["gate", "status"],
           before={"bin/daisugi": f("#!/bin/sh\n", 0o755),
                   CLAUDE: f(hook_settings(go_hook % ("bin/daisugi", "enforce") + " || exit 2"))}))
    add(mk("status custom root", ["gate", "status", "--root", "{HOME}/g"],
           before={"g/envelopes/default.json": f("{}"), "config.yaml": f("gate_mode: enforce\n")}))
    configs = {
        "cfg enforce": "gate_mode: enforce\n",
        "cfg shadow": "gate_mode: shadow\n",
        "cfg quoted": "gate_mode: 'enforce'\n",
        "cfg dquoted": 'gate_mode: "enforce"  # on\n',
        "cfg comment": "# my config\n\ngate_mode: enforce # yes\nmodel: x\n",
        "cfg bogus mode": "gate_mode: strict\n",
        "cfg bool mode": "gate_mode: yes\n",
        "cfg int mode": "gate_mode: 5\n",
        "cfg empty": "",
        "cfg list": "- a\n- b\n",
        "cfg scalar": "hello\n",
        "cfg bad yaml": "gate_mode: [enforce\n",
        "cfg bad other field": "gate_mode: enforce\nz3_timeout_ms: lots\n",
        "cfg int other field": "gate_mode: enforce\nz3_timeout_ms: 800\nmax_task_chars: '12'\n",
        "cfg bool other field": "gate_mode: enforce\nauto_tend: maybe\n",
        "cfg yes bool": "gate_mode: enforce\nauto_tend: yes\nshell_allow_decomposition: off\n",
        "cfg null opt": "gate_mode: enforce\nllm_backend: null\ngateway_local_model: ~\n",
        "cfg unknown keys": "gate_mode: enforce\nnot_a_key: [1, 2]\nfoo:\n  bar: 1\n",
        "cfg floor": "gate_mode: enforce\nfloor:\n  backend: coppice\n  notify_cmd: null\n",
        "cfg floor bad": "gate_mode: enforce\nfloor:\n  backend: [x]\n",
        "cfg floor null": "gate_mode: enforce\nfloor: null\n",
        "cfg dup key": "gate_mode: enforce\ngate_mode: shadow\n",
        "cfg anchor": "gate_mode: &m enforce\nmodel: *m\n",
        "cfg tab": "gate_mode:\tenforce\n",
        "cfg str for int": "gate_mode: enforce\nz3_timeout_ms: 1.5\n",
        # YAML's .nan and .inf are floats: invalid in an int or bool field,
        # and a top-level one is truthy, so not an empty config.
        "cfg inf for int": "gate_mode: enforce\nz3_timeout_ms: .inf\n",
        "cfg nan for bool": "gate_mode: enforce\nauto_tend: .nan\n",
        "cfg top nan": ".nan\n",
        "cfg top minus inf": "-.inf\n",
        "cfg data dir": "gate_mode: enforce\ndata_dir: /somewhere/else\n",
        "cfg saved": "auto_tend: false\ndata_dir: /home/user/.opendaisugi\nfloor:\n  backend: auto\n  coppice_socket: null\n  notify_cmd: null\n  tmux_socket: null\nfloor_report: null\ngate_ask: false\ngate_mode: enforce\nllm_context_window: null\nmax_task_chars: 4000\nmodel: anthropic/claude-sonnet-4-20250514\nvoice_server_url: http://127.0.0.1:7477\nz3_timeout_ms: 500\n",
    }
    for name, text in configs.items():
        add(mk(f"status {name}", ["gate", "status", "--json"], before={".opendaisugi/config.yaml": f(text)}))
    hooks = {
        "hook global enforce": {CLAUDE: f(hook_settings(PY_ENFORCE))},
        "hook global shadow": {CLAUDE: f(hook_settings(PY_SHADOW)), ".opendaisugi/config.yaml": f("gate_mode: enforce\n")},
        "hook project": {"proj/.claude/settings.json": f(hook_settings(PY_ENFORCE))},
        "hook both": {CLAUDE: f(hook_settings(PY_SHADOW)), "proj/.claude/settings.json": f(hook_settings(PY_ENFORCE))},
        "hook both shadow": {CLAUDE: f(hook_settings(PY_SHADOW)), "proj/.claude/settings.json": f(hook_settings(PY_SHADOW))},
        "hook bad json": {CLAUDE: f("{nope"), ".opendaisugi/config.yaml": f("gate_mode: enforce\n")},
        "hook no mode": {CLAUDE: f(hook_settings("python3 -m opendaisugi.gate --root /x"))},
        "hook capture only": {CLAUDE: f(hook_settings("daisugi hook record --format claude --mode enforce"))},
        "hook mode suffix": {CLAUDE: f(hook_settings("python3 -m opendaisugi.gate --mode enforced"))},
        "hook go form": {CLAUDE: f(hook_settings(
            "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"))},
        "hook hooks null": {CLAUDE: f('{"hooks": null}')},
        "hook pre null": {CLAUDE: f('{"hooks": {"PreToolUse": null}}')},
        "hook two entries": {CLAUDE: f(json.dumps({"hooks": {"PreToolUse": [
            {"hooks": [{"command": "python3 -m opendaisugi.gate --mode shadow"}]},
            {"hooks": [{"command": "python3 -m opendaisugi.gate --mode enforce"}]}]}}))},
        "hook bom": {CLAUDE: f("﻿" + hook_settings(PY_ENFORCE))},
    }
    for name, tree in hooks.items():
        add(mk(f"status {name}", ["gate", "status", "--json"], before=tree, cwd="proj"))
        add(mk(f"status {name} text", ["gate", "status"], before=tree, cwd="proj"))

    # -- report -------------------------------------------------------------
    def rec(tool: str, would_deny: bool, reason: str | None, detail: str = "x", **kw: Any) -> str:
        r = {"at": 1.5, "session_id": "s1", "tool_name": tool, "step_type": "shell", "detail": detail,
             "mode": "shadow", "allow": True, "would_deny": would_deny, "reason": reason, "elapsed_ms": 1.0}
        r.update(kw)
        return json.dumps(r)

    log1 = "\n".join([
        rec("Bash", False, None, "git status"),
        rec("Bash", True, "shell command contains metacharacters", "a && b"),
        rec("TodoWrite", True, "unrecognized tool 'TodoWrite'", ""),
        rec("Read", True, "path '/etc/passwd' not in file_read", "/etc/passwd"),
        "",
        "not json",
        rec("Write", True, "x" * 200, "it's \"q\" é\n"),
    ]) + "\n"
    log2 = rec("Bash", True, "path outside", "rm -rf /") + "\n"
    shadow = {f"{GROOT}/shadow/s1.jsonl": f(log1, 0o600), f"{GROOT}/shadow/s2.jsonl": f(log2, 0o600)}
    add(mk("report none", ["gate", "report"]))
    add(mk("report none json", ["gate", "report", "--json"]))
    add(mk("report all", ["gate", "report"], before=shadow))
    add(mk("report all json", ["gate", "report", "--json"], before=shadow))
    add(mk("report session", ["gate", "report", "--session", "s2"], before=shadow))
    add(mk("report missing session", ["gate", "report", "--session", "nope"], before=shadow))
    add(mk("report unsafe session", ["gate", "report", "--session", "../s1"], before=shadow))
    add(mk("report reason null", ["gate", "report"], before={
        f"{GROOT}/shadow/a.jsonl": f(json.dumps({"would_deny": True, "reason": None}) + "\n" +
                                     json.dumps({"would_deny": 1, "tool_name": None}) + "\n")}))
    add(mk("report list line", ["gate", "report"], before={
        f"{GROOT}/shadow/a.jsonl": f(json.dumps({"would_deny": True, "reason": "x"}) + "\n[1, 2]\n")}))
    add(mk("report no detail", ["gate", "report", "--json"], before={
        f"{GROOT}/shadow/a.jsonl": f(json.dumps({"would_deny": True, "reason": "metacharacters here"}) + "\n")}))

    # -- settings -----------------------------------------------------------
    add(mk("settings default", ["gate", "settings"]))
    add(mk("settings enforce", ["gate", "settings", "--enforce"]))
    add(mk("settings session", ["gate", "settings", "--enforce", "--session", "s 1"]))
    add(mk("settings root", ["gate", "settings", "--root", "{HOME}/my root"]))
    add(mk("settings format pi", ["gate", "settings", "--enforce", "--format", "pi"]))

    # -- init ---------------------------------------------------------------
    add(mk("init fresh", ["gate", "init"], cwd="work"))
    add(mk("init workspace", ["gate", "init", "--workspace", "{HOME}/w"]))
    add(mk("init session", ["gate", "init", "--session", "s1", "--workspace", "/work"]))
    add(mk("init unsafe session", ["gate", "init", "--session", "a/b", "--workspace", "/work"]))
    add(mk("init exists", ["gate", "init", "--workspace", "/work"], before={f"{ENVD}/default.json": f("{}", 0o600)}))
    add(mk("init force", ["gate", "init", "--workspace", "/work", "--force"],
           before={f"{ENVD}/default.json": f("{}", 0o644), ENVD: d(0o755)}))
    add(mk("init decomposition", ["gate", "init", "--workspace", "/work", "--allow-shell-decomposition"]))
    add(mk("init root", ["gate", "init", "--workspace", "/work", "--root", "{HOME}/g"]))

    # -- register -----------------------------------------------------------
    add(mk("register default", ["gate", "register", "{HOME}/e.json"], before={"e.json": f(envelope_json(id="env_fixed01"))}))
    add(mk("register session", ["gate", "register", "{HOME}/e.json", "--session", "s1"],
           before={"e.json": f(envelope_json(stakes="high", summary="short"))}))
    add(mk("register yaml", ["gate", "register", "{HOME}/e.yaml"],
           before={"e.yaml": f("generated_by: me\ntask: t\npermissions:\n  shell: true\n  shell_allowlist: [ls]\n")}))
    add(mk("register missing", ["gate", "register", "{HOME}/nope.json"]))
    add(mk("register invalid", ["gate", "register", "{HOME}/e.json"], before={"e.json": f('{"task": "t"}')}))
    add(mk("register coerced", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(max_execution_time_s="60"))}))
    add(mk("register unknown keys", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(json.dumps({"generated_by": "g", "task": "t", "extra": 1,
                                            "permissions": {"file_read": ["/a"], "bogus": True}}))}))

    # -- install --gate -----------------------------------------------------
    claude_dir = {".claude": d()}
    gi = {"oracle": "gate-only-install"}
    # Enforce installs only with a registered envelope; with none it refuses.
    policy = {".opendaisugi/gate/envelopes/default.json": f(envelope_json(id="env_fixed01"))}
    add(mk("install gate claude", ["install", "--gate", "--yes"], before=claude_dir, **gi))
    add(mk("install gate enforce", ["install", "--gate", "--enforce", "-y"], before={**claude_dir, **policy}, **gi))
    add(mk("install gate enforce no policy", ["install", "--gate", "--enforce", "-y"], before=claude_dir, **gi))
    add(mk("install gate enforce no policy dry run", ["install", "--gate", "--enforce", "--dry-run"],
           before=claude_dir, **gi))
    add(mk("install gate enforce empty envelopes dir", ["install", "--gate", "--enforce", "-y"],
           before={**claude_dir, ".opendaisugi/gate/envelopes": d(), ".opendaisugi/gate/envelopes/notes.txt": f("x")},
           **gi))
    add(mk("install gate enforce session policy", ["install", "--gate", "--enforce", "-y"],
           before={**claude_dir, ".opendaisugi/gate/envelopes/s1.json": f(envelope_json(id="env_fixed02"))}, **gi))
    add(mk("install gate ask", ["install", "--gate", "--enforce", "--ask", "--yes"], before=claude_dir, go_refuses=True))
    add(mk("install gate ask shadow", ["install", "--gate", "--ask", "--yes"], before=claude_dir, go_refuses=True))
    add(mk("install enforce no gate", ["install", "--enforce", "--yes"], before=claude_dir, go_refuses=True))
    other = json.dumps({"permissions": {"allow": ["Bash(ls)"]}, "hooks": {"PreToolUse": [
        {"matcher": "Bash", "hooks": [{"type": "command", "command": "daisugi hook record --format claude"}]}]},
        "env": {"X": "é"}}, indent=2)
    add(mk("install gate existing settings", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f(other, 0o600)}, **gi))
    add(mk("install gate invalid settings", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f("{oops")}, **gi))
    add(mk("install gate settings list", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f('{"hooks": []}')}, **gi))
    add(mk("install gate other mode", ["install", "--gate", "--enforce", "--yes"],
           before={".claude": d(), CLAUDE: f(hook_settings(PY_SHADOW)), **policy}, **gi))
    add(mk("install gate old module", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f(hook_settings(
               "python3 -m opendaisugi.gate --mode shadow --root {HOME}/.opendaisugi/gate --format claude --verify-timeout 10.0"))}, **gi))
    add(mk("install gate dry run", ["install", "--gate", "--dry-run"], before=claude_dir, **gi))
    add(mk("install gate confirm yes", ["install", "--gate"], stdin="y\n",
           before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")}, **gi))
    add(mk("install gate confirm no", ["install", "--gate"], stdin="n\n", before=claude_dir, **gi))
    add(mk("install gate none detected", ["install", "--gate", "--yes"], **gi))
    add(mk("install gate codex", ["install", "--gate", "--yes", "--runtime", "codex"], **gi))
    add(mk("install gate codex path", ["install", "--gate", "--enforce", "--yes"], path_bins=["codex"],
           before=policy, **gi))
    add(mk("install gate codex existing", ["install", "--gate", "--yes", "--runtime", "codex"],
           before={".codex/hooks.json": f(json.dumps({"hooks": {"PreToolUse": [{"matcher": ".*", "hooks": [
               {"type": "command", "command": "python3 -m opendaisugi.gate_client --mode enforce"}]}]}}))}, **gi))
    add(mk("install gate all runtimes", ["install", "--gate", "--yes"],
           before={".claude": d(), ".codex": d(), ".hermes": d(), ".openclaw": d()}, **gi))
    add(mk("install gate runtime prefix", ["install", "--gate", "--yes", "--runtime", "cl", "--runtime", "hermes"], **gi))
    add(mk("install gate runtime ambiguous", ["install", "--gate", "--yes", "--runtime", "c"], **gi))
    add(mk("install gate runtime unknown", ["install", "--gate", "--yes", "--runtime", "vim"], **gi))
    add(mk("install gate idempotent", ["install", "--gate", "--enforce", "--yes"],
           before={".claude": d(), **policy}, twice=True, **gi))
    add(mk("install gateway refused", ["install", "--gate", "--gateway", "--yes"], before=claude_dir, go_refuses=True))
    add(mk("install report refused", ["install", "--gate", "--report", "herdr", "--yes"], before=claude_dir, go_refuses=True))
    add(mk("install plain refused", ["install", "--yes"], before=claude_dir, go_refuses=True))
    add(mk("install decomposition refused", ["install", "--gate", "--allow-shell-decomposition", "--yes"],
           before=claude_dir, go_refuses=True))

    # -- uninstall ----------------------------------------------------------
    gu = {"oracle": "gate-only-uninstall"}
    installed = json.dumps({"env": {"A": "1"}, "hooks": {
        "PreToolUse": [
            {"matcher": "Bash", "hooks": [{"type": "command", "command": "daisugi hook record --format claude"}]},
            {"matcher": "*", "hooks": [{"type": "command", "command": PY_ENFORCE}]},
        ],
        "Stop": [{"hooks": [{"type": "command", "command": "daisugi hook record --format claude --event stop"}]}],
        "SubagentStart": [{"hooks": [{"type": "command", "command": "daisugi hook record --format claude --event subagent_start"}]}],
    }}, indent=2)
    add(mk("uninstall gate claude", ["install", "--gate", "--uninstall"], before={".claude": d(), CLAUDE: f(installed, 0o600)}, **gu))
    add(mk("uninstall gate only hook", ["install", "--gate", "--uninstall"],
           before={".claude": d(), CLAUDE: f(hook_settings(PY_SHADOW))}, **gu))
    add(mk("uninstall gate nothing", ["install", "--gate", "--uninstall"], before=claude_dir, **gu))
    add(mk("uninstall gate codex", ["install", "--gate", "--uninstall", "--runtime", "codex"],
           before={".codex/hooks.json": f(hook_settings(PY_ENFORCE))}, **gu))
    add(mk("uninstall gate go form", ["install", "--gate", "--uninstall"], before={".claude": d(), CLAUDE: f(hook_settings(
        "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"))}, **gu))
    add(mk("uninstall gate none detected", ["install", "--gate", "--uninstall"], **gu))

    # -- harness extensions -------------------------------------------------
    add(mk("harness pi", ["install", "--harness", "pi"]))
    add(mk("harness opencode", ["install", "--harness", "opencode"]))
    add(mk("harness both", ["install", "--harness", "pi", "--harness", "opencode"]))
    add(mk("harness opencode xdg", ["install", "--harness", "opencode"], env={"XDG_CONFIG_HOME": "{HOME}/xdg"}))
    add(mk("harness opencode xdg relative", ["install", "--harness", "opencode"], env={"XDG_CONFIG_HOME": "rel"}))
    add(mk("harness dry run", ["install", "--harness", "pi", "--harness", "opencode", "--dry-run"]))
    add(mk("harness unknown", ["install", "--harness", "vim"]))
    add(mk("harness symlink dir", ["install", "--harness", "opencode"],
           before={".config/opencode/plugins": {"link": "{HOME}/elsewhere"}, "elsewhere": d()}))
    add(mk("harness planted file link", ["install", "--harness", "pi"],
           before={".pi/agent/extensions/daisugi-gate/index.ts": {"link": "{HOME}/target"}, "target": f("mine")}))
    add(mk("harness same content", ["install", "--harness", "pi"], twice=True))
    add(mk("harness uninstall", ["install", "--harness", "pi", "--harness", "opencode", "--uninstall"],
           before={".pi/agent/extensions/daisugi-gate/index.ts": f("x"),
                   ".config/opencode/plugins/daisugi-gate.ts": f("x"),
                   ".config/opencode/plugins/mine.ts": f("y")}))
    add(mk("harness uninstall nothing", ["install", "--harness", "pi", "--harness", "opencode", "--uninstall"]))
    add(mk("harness uninstall dry", ["install", "--harness", "pi", "--harness", "opencode", "--uninstall", "--dry-run"]))

    # -- more config shapes (status reads the whole file as pydantic does) --
    more_cfg = {
        "cfg multi plain": "gate_mode: enforce\nnotify: aaa\n  bbb\nmodel: x\n",
        "cfg floor seq": "gate_mode: enforce\nfloor:\n- a\n",
        "cfg seq same indent": "gate_mode: enforce\nextra:\n- a\n- b: 1\n  c: 2\n",
        "cfg float int ok": "gate_mode: enforce\nz3_timeout_ms: 800.0\n",
        "cfg str int ok": "gate_mode: enforce\nz3_timeout_ms: ' 1_000 '\n",
        "cfg str bool ok": "gate_mode: enforce\ngate_ask: 'T'\n",
        "cfg str bool bad": "gate_mode: enforce\ngate_ask: 'nope'\n",
        "cfg dquote esc": 'gate_mode: "enf\\x6frce"\n',
        "cfg falsy top": "0\n",
        "cfg flow map": "gate_mode: enforce\nfloor: {backend: tmux, notify_cmd: null}\n",
        "cfg flow map bad": "gate_mode: enforce\nfloor: {backend: [1]}\n",
        "cfg doc marker": "---\ngate_mode: enforce\n",
        "cfg key on": "on: 1\ngate_mode: enforce\n",
        "cfg nested unknown": "gate_mode: enforce\nx:\n  y:\n    z: [1, {a: b}]\n",
        "cfg bool int": "gate_mode: enforce\nmax_task_chars: true\n",
        "cfg path int": "gate_mode: enforce\ndata_dir: 5\n",
    }
    for name, text in more_cfg.items():
        add(mk(f"status {name}", ["gate", "status", "--json"], before={".opendaisugi/config.yaml": f(text)}))
    add(mk("init cfg decomposition", ["gate", "init", "--workspace", "/work"],
           before={".opendaisugi/config.yaml": f("shell_allow_decomposition: yes\n")}))
    add(mk("init cfg invalid", ["gate", "init", "--workspace", "/work"],
           before={".opendaisugi/config.yaml": f("shell_allow_decomposition: maybe\n")}))
    add(mk("init no decomposition flag", ["gate", "init", "--workspace", "/work", "--no-allow-shell-decomposition"],
           before={".opendaisugi/config.yaml": f("shell_allow_decomposition: true\n")}))
    add(mk("init relative workspace", ["gate", "init", "--workspace", "sub/../w"], cwd="proj"))
    add(mk("init symlink workspace", ["gate", "init", "--workspace", "{HOME}/link/x"],
           before={"real": d(), "link": {"link": "{HOME}/real"}}))

    # -- register: YAML, coercions, robotics, predicates -------------------
    add(mk("register yaml block", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
        "# my envelope\ngenerated_by: me\ntask: 'a task'\nstakes: high\npermissions:\n  file_read:\n  - /work/**\n"
        "  - \"/data/**\"\n  shell: yes\n  shell_allowlist: [ls, git, 'rg']\n  max_execution_time_s: '45'\n"
        "fallback:\n  include_refinement: off\n")}))
    add(mk("register yaml seq of maps", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
        "generated_by: me\ntask: t\npermissions: {}\ninvariants:\n- type: file_unchanged\n  description: keep it\n"
        "  target: /etc/x\n- type: no_side_effects\n  description: none\n  enforce: false\n")}))
    add(mk("register coerced bool", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(shell="yes", network=0))}))
    add(mk("register bad bool", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(shell="maybe"))}))
    add(mk("register float int", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(max_execution_time_s=60.0))}))
    add(mk("register robotics", ["gate", "register", "{HOME}/e.json"], before={"e.json": f(envelope_json(
        workspace_bounds=[[0, 0, 0], [1.5, 2, 1e-7]], obstacles=[[[0.1, 0.2, 0.3], [1, 1, 1]]],
        velocity_limit=2, joint_limits={"j1": [-1.5, 1.5], "j2": [0, 3.14159]}, torque_limit=1e22))}))
    add(mk("register float notation", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
        "generated_by: g\ntask: t\npermissions:\n  velocity_limit: 0.000025\n  torque_limit: 9999999999999998.0\n"
        "  joint_limits:\n    a: [0.0001, 0.00001]\n    b: [1000000000000000.0, 0.30000000000000004]\n"
        "  workspace_bounds: [[-0.00002, 0.5, 100.0], [123456789012345.6, 1.5, 2.0]]\n"
        "invariants:\n- type: t\n  description: d\n  expr: [0.00001, 1.0e-10, 2.5e-05]\n")}))
    add(mk("register bad bounds", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(workspace_bounds=[[0, 0], [1, 1]]))}))
    add(mk("register predicates", ["gate", "register", "{HOME}/e.json", "--session", "p"], before={"e.json": f(json.dumps({
        "generated_by": "g", "task": "t", "permissions": {"shell": True},
        "invariants": [{"type": "pred", "description": "d", "expr": {"op": "lt", "args": [1.5, 2, "x", None, 1e-9]}}],
        "postconditions": [{"type": "file_exists", "path": "/w/o", "expected": 1, "min": "2", "max": None}]}))}))
    add(mk("register summary long", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(summary="x" * 81))}))
    add(mk("register bad stakes", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(stakes="extreme"))}))
    add(mk("register unicode", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(json.dumps({"generated_by": "g", "task": "t\u00e9\u2603", "permissions": {"file_read": ["/\u00e9/**"]}}))}))
    add(mk("register top list", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f("- a\n")}))
    # NaN or an infinity in any number is invalid (models.non_finite_error):
    # refused, nothing written. yaml.safe_load reads the JSON NaN,
    # Infinity and 1e999 as strings, which pydantic reads as floats; YAML's
    # .nan and .inf are floats already.
    for name, value in [("nan", "NaN"), ("infinity", "Infinity"), ("minus infinity", "-Infinity"),
                        ("1e999", "1e999"), ("nan string", '"nan"'), ("big int", "1" + "0" * 400)]:
        add(mk(f"register {name} velocity", ["gate", "register", "{HOME}/e.json"],
               before={"e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", value))}))
    for value in (".nan", ".NaN", ".inf", "-.inf", "+.inf", ".Inf"):
        add(mk(f"register yaml {value}", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
            f"generated_by: g\ntask: t\npermissions:\n  workspace_bounds: [[0, 0, 0], [1, {value}, 1]]\n")}))
    add(mk("register nan in expr", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
        "generated_by: g\ntask: t\npermissions: {}\ninvariants:\n- type: t\n  description: d\n"
        "  expr: {op: equals, path: x, value: [1, .nan]}\n")}))
    add(mk("register nan joint limit", ["gate", "register", "{HOME}/e.yaml"], before={"e.yaml": f(
        "generated_by: g\ntask: t\npermissions:\n  joint_limits:\n    j: [-.inf, 1]\n")}))
    add(mk("register nan keeps the old envelope", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", "NaN")),
                   f"{GROOT}/envelopes/default.json": f(envelope_json(id="env_old00001"))}))
    add(mk("register nan and a bad field", ["gate", "register", "{HOME}/e.json"],
           before={"e.json": f(envelope_json(velocity_limit=1.5, shell="maybe").replace("1.5", "NaN"))}))

    # -- report values -----------------------------------------------------
    add(mk("report floats unicode", ["gate", "report"], before={f"{GROOT}/shadow/a.jsonl": f("\n".join([
        json.dumps({"would_deny": True, "reason": "r", "tool_name": 5, "detail": 1.5}),
        json.dumps({"would_deny": True, "reason": "r\u00e9", "tool_name": "T", "detail": "\u00e9\u200b\u2028x\u3000"}),
        json.dumps({"would_deny": 0, "reason": ["x"]}),
        json.dumps({"would_deny": "yes", "reason": "unrecognized tool 'X'", "detail": None}),
    ]) + "\n")}))

    # -- proposals ---------------------------------------------------------
    props = {f"{GROOT}/proposals/b.json": f(json.dumps({"id": "p2", "kind": "widen", "scope": "s1", "expiresAt": 5})),
             f"{GROOT}/proposals/a.json": f(json.dumps({"id": "p1", "kind": "narrow"})),
             f"{GROOT}/proposals/c.json": f("[1]"),
             f"{GROOT}/proposals/d.json": f("{bad")}
    add(mk("proposals none", ["gate", "proposals"]))
    add(mk("proposals none json", ["gate", "proposals", "--json"]))
    add(mk("proposals some", ["gate", "proposals"], before=props))
    add(mk("proposals some json", ["gate", "proposals", "--json"], before=props))

    # -- the root and the commands this binary does not carry -------------
    add(mk("version", ["--version"]))
    add(mk("config not ported", ["config"], go_refuses=True))
    add(mk("replay not ported", ["gate", "replay", "x", "--envelope", "y"], go_refuses=True))
    add(mk("uninstall all not ported", ["install", "--uninstall"], before=claude_dir, go_refuses=True))
    add(mk("status unknown option", ["gate", "status", "--bogus"]))
    add(mk("register no argument", ["gate", "register"]))
    add(mk("install gate confirm eof", ["install", "--gate"], stdin="", before=claude_dir, **gi))
    add(mk("install gate confirm again", ["install", "--gate"], stdin="maybe\ny\n",
           before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")}, **gi))
    add(mk("install gate claude forced missing", ["install", "--gate", "--yes", "--runtime", "claude"], **gi))
    add(mk("install gate int command", ["install", "--gate", "--yes"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": 5}]}]}}))}, **gi))
    add(mk("install gate no command key", ["install", "--gate", "--yes"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"hooks": {"PreToolUse": [{"hooks": [{"type": "command"}]}]}}))}, **gi))
    add(mk("install gate pre dict", ["install", "--gate", "--yes"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"hooks": {"PreToolUse": {}}}))}, **gi))
    add(mk("install gate other keys kept", ["install", "--gate", "--enforce", "--yes"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"model": "x", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "a"}]}], "PreToolUse": []},
         "z": [1, 2.5, None, chr(0xE9) + " " + chr(0x2603)]}, indent=4), 0o600)}, **gi))
    add(mk("install gate codex go form", ["install", "--gate", "--yes", "--runtime", "codex"],
           before={".codex/hooks.json": f(hook_settings(
               "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode shadow --root /x "
               "--format claude --verify-timeout 10.0"))}, **gi))
    add(mk("install gate claude go form", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f(hook_settings(
               "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x "
               "--format claude --verify-timeout 10.0 || exit 2"))}, **gi))
    add(mk("uninstall gate report hooks", ["install", "--gate", "--uninstall"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"hooks": {"Notification": [{"hooks": [
            {"type": "command", "command": "daisugi hook record --format claude --event notification"},
            {"type": "command", "command": "mine"}]}],
            "SubagentStop": [{"hooks": [{"type": "command", "command": "daisugi hook record --event subagent_stop"}]}]}}))},
        **gu))
    add(mk("uninstall gate bad json", ["install", "--gate", "--uninstall"], before={".claude": d(), CLAUDE: f("{x")}, **gu))
    add(mk("bare", []))

    # -- hooks are found by their words, not a substring ---------------------
    foreign = "/opt/opendaisugi.gateway-watch --mode enforce"
    quoted = "'/opt/x --mode enforce/python' -m opendaisugi.gate --root /r"
    add(mk("status foreign hook", ["gate", "status", "--json"], before={CLAUDE: f(hook_settings(foreign))}))
    add(mk("status quoted mode", ["gate", "status", "--json"], before={CLAUDE: f(hook_settings(quoted))}))
    add(mk("install gate foreign hook", ["install", "--gate", "--enforce", "--yes"],
           before={".claude": d(), CLAUDE: f(hook_settings(foreign))}, **gi))
    add(mk("install gate codex foreign hook", ["install", "--gate", "--yes", "--runtime", "codex"],
           before={".codex/hooks.json": f(hook_settings(foreign))}, **gi))
    add(mk("uninstall gate foreign hooks", ["install", "--gate", "--uninstall"], before={".claude": d(), CLAUDE: f(json.dumps(
        {"hooks": {"PreToolUse": [{"hooks": [
            {"type": "command", "command": foreign},
            {"type": "command", "command": PY_ENFORCE}]}],
            "Stop": [{"hooks": [{"type": "command", "command": "mydaisugi hook record --event stop"},
                                {"type": "command", "command": "daisugi hook record --event stop"}]}]}}, indent=2))}, **gu))
    # -- hand-written gate hooks, and ones in a form not read (round 2) -----
    written = {
        "uv run": "uv run --project /p python -m opendaisugi.gate --mode enforce --root /x || exit 2",
        "env prefix": "FOO=1 python3 -m opendaisugi.gate --mode enforce",
        "env command": "env FOO=1 python3 -m opendaisugi.gate --mode enforce",
        "python flag": "python3 -I -m opendaisugi.gate --mode enforce",
        "joined m": "python3 -mopendaisugi.gate --mode enforce",
        "no space op": "python3 -m opendaisugi.gate --mode enforce||exit 2",
        "sh c": "sh -c 'python3 -m opendaisugi.gate --mode enforce'",
        "bare daisugi": "daisugi gate check --mode enforce --root /x || exit 2",
        "echo": "echo -m opendaisugi.gate --mode enforce",
        "true marker": "DAISUGI_GATE_HOOK=opendaisugi.gate /bin/true gate check --mode enforce",
        "unknown": "nice python3 -m opendaisugi.gate --mode enforce",
    }
    for name, command in written.items():
        tree = {".claude": d(), CLAUDE: f(hook_settings(command))}
        add(mk(f"status written {name}", ["gate", "status", "--json"], before=tree))
        add(mk(f"uninstall written {name}", ["install", "--gate", "--uninstall"], before=tree, **gu))
    add(mk("status written unknown text", ["gate", "status"],
           before={CLAUDE: f(hook_settings(written["unknown"]))}))
    add(mk("status unknown beside shadow", ["gate", "status", "--json"],
           before={CLAUDE: f(hook_settings(PY_SHADOW)), "proj/.claude/settings.json": f(hook_settings(written["unknown"]))},
           cwd="proj"))
    add(mk("install gate beside unknown", ["install", "--gate", "--yes"],
           before={".claude": d(), CLAUDE: f(hook_settings(written["unknown"]))}, **gi))
    add(mk("install gate settings symlink", ["install", "--gate", "--yes"],
           before={".claude": d(), "dot/settings.json": f("{}"), CLAUDE: {"link": "{HOME}/dot/settings.json"}}, **gi))
    return C


def run_twice_aware(case: dict[str, Any], cmd: list[str], work: Path,
                    extra_env: dict[str, str] | None = None) -> tuple[dict[str, Any], float]:
    """A `twice` case runs the command, then runs it again on the tree the
    first run left; the second run is what is recorded."""
    if not case.get("twice"):
        return run_case(case, cmd, work, extra_env)
    obs, _ = run_case(case, cmd, work, extra_env)
    home = work / "home"
    snap = work.parent / (work.name + ".first")
    if snap.exists():
        shutil.rmtree(snap)
    shutil.copytree(home, snap, symlinks=True)
    again = dict(case)
    again.pop("twice")
    again["before"] = {}
    # Re-run on the first run's tree: lay out nothing, keep the home.
    _, argv, env, cwd = prepare(again, work)
    shutil.rmtree(home)
    shutil.copytree(snap, home, symlinks=True)
    shutil.rmtree(snap)
    if extra_env:
        env.update(extra_env)
    old = os.umask(0o022)
    try:
        t0 = time.perf_counter()
        proc = subprocess.run(cmd + argv, input=case.get("stdin", "").encode(), capture_output=True,
                              env=env, cwd=cwd, timeout=120, check=False)
        elapsed = time.perf_counter() - t0
    finally:
        os.umask(old)
    h = str(home)
    return {
        "exit": proc.returncode,
        "stdout": norm_stdout(proc.stdout.decode("utf-8", "replace"), h, case["argv"]),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace"), h),
        "tree": read_tree(home),
    }, elapsed


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument("--only", help="run only cases whose name contains this text; print, write nothing")
    args = ap.parse_args()
    cases = build_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    SCRATCH.mkdir(parents=True, exist_ok=True)
    out_lines = []
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        work = SCRATCH / "gen" / f"{i:04d}"
        obs, _ = run_twice_aware(c, python_cmd(c), work)
        if args.only:
            print(json.dumps({"name": c["name"], **obs}, indent=1, ensure_ascii=False))
            continue
        body = dict(c)
        body["expect"] = obs
        body["id"] = case_id(body)
        out_lines.append(body)
        print(f"{i + 1:4d}/{len(cases)} exit={obs['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    out_lines.sort(key=lambda c: c["id"])
    args.out.mkdir(parents=True, exist_ok=True)
    path = args.out / "cases.jsonl"
    data = "".join(canonical_json(c) + "\n" for c in out_lines).encode("utf-8")
    path.write_bytes(data)
    manifest = {"v": CASE_VERSION, "count": len(out_lines), "sha256": hashlib.sha256(data).hexdigest()}
    (args.out / "cases.manifest.json").write_text(json.dumps(manifest, indent=1) + "\n", encoding="utf-8")
    print(f"wrote {len(out_lines)} cases to {path}")
    from fixture_paths import leaks

    leaked = leaks(args.out)
    if leaked:
        raise SystemExit("a machine path reached the cases:\n" + "\n".join(leaked[:10]))
    write_config_fields()
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    return 0


def write_config_fields() -> None:
    """The Config and FloorConfig fields with their types, for the Go
    config reader's test: a field Python validates and Go does not know
    would read a bad file as valid."""
    sys.path.insert(0, str(REPO / "src"))
    from opendaisugi.config import Config, FloorConfig

    def fields(model: Any) -> list[list[str]]:
        return [[k, str(f.annotation)] for k, f in model.model_fields.items()]

    out = REPO / "clients" / "go" / "internal" / "config" / "testdata" / "config_fields.json"
    out.write_text(json.dumps({"config": fields(Config), "floor": fields(FloorConfig)}, indent=1) + "\n",
                   encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
