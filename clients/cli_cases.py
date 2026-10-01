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
import socket
import stat
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import base_install  # noqa: E402 - sibling module, run as a script

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "cli"
SCRATCH = Path(
    os.environ.get("DAISUGI_CLI_SCRATCH") or Path.home() / "opendaisugi-scratch" / "2c" / "runs"
)
CASE_VERSION = 1


def as_daisugi(binary: str | Path, scratch: Path = SCRATCH, name: str = "daisugi") -> str:
    """The binary under the file name ``name`` (daisugi unless a case sets
    `prog`): a symlink in the scratch directory, returned unresolved.

    The binary names itself in the hooks it writes by the path it ran
    from. Running it through a link of a known name means no case depends
    on where the build put the binary; a case with `prog` runs it under
    another file name, as a user may install it (ruling INS-1)."""
    target = Path(binary).resolve()
    link = scratch / "prog" / name
    link.parent.mkdir(parents=True, exist_ok=True)
    if link.is_symlink() or link.exists():
        link.unlink()
    link.symlink_to(target)
    return str(link)


GATE_SOCK = ".opendaisugi/gate/gate.sock"

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
for name in ("_remove_skill_both", "_remove_skill", "_pop_json_mcp", "_unpatch_instructions"):
    setattr(i, name, none)
i.HermesRuntime.reverse = lambda self, home: []


def _openclaw_providers_only(self, home):
    # The provider half of OpenClawRuntime.reverse: the gateway layer the
    # Go binary also reverses. Its MCP and plugin layers are not the Go
    # binary's.
    import json as _json
    cfg_path = home / ".openclaw" / "openclaw.json"
    if not cfg_path.exists():
        return []
    try:
        cfg = _json.loads(cfg_path.read_text())
    except _json.JSONDecodeError:
        try:
            cfg = _json.loads(i._strip_json5_comments(cfg_path.read_text()))
        except _json.JSONDecodeError:
            cfg = None
    if not isinstance(cfg, dict):
        return []
    providers = cfg.get("models", {}).get("providers", {})
    if "opendaisugi" not in providers:
        return []
    del providers["opendaisugi"]
    if not providers:
        cfg["models"].pop("providers", None)
        if not cfg["models"]:
            del cfg["models"]
    i._backup(cfg_path)
    cfg_path.write_text(_json.dumps(cfg, indent=2) + "\\n")
    return [cfg_path]


i.OpenClawRuntime.reverse = _openclaw_providers_only
from opendaisugi.cli import main
sys.argv = ["daisugi"] + sys.argv[1:]
main()
"""


# `daisugi start` detaches `python -m opendaisugi.cli gate serve` and then
# opens the view. The oracle runs with the spawn replaced by a fake that
# logs the argv it was given and makes the socket the server would make,
# so no case leaves a process running. It runs as the binaries are built
# (clients/base_install.py): without the Textual extra it opens the plain
# live view, which draws one frame when stdout is not a terminal.
_START_SHIM = base_install.shim(
    """
import json, os
from pathlib import Path
import opendaisugi.start as st
def _fake_detach(argv):
    with open(os.environ["FAKE_SPAWN_LOG"], "a", encoding="utf-8") as f:
        f.write(json.dumps(argv) + "\\n")
    Path(argv[argv.index("--root") + 1], "gate.sock").touch()
st._detach = _fake_detach
"""
)


def python_cmd(case: dict[str, Any]) -> list[str]:
    oracle = case.get("oracle", "cli")
    if oracle == "start-shim":
        return [sys.executable, "-c", _START_SHIM]
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
# An interpreter installed at a system prefix (a CI runner's Python has
# sys.base_prefix /usr) names no machine, and a case's own text holds such
# paths (/usr/bin/python3 in a hook command), so it is never replaced.
_SYSTEM_PREFIXES = {"/", "/usr", "/usr/local"}
_MACHINE_PATHS = sorted(
    {
        (path, mark)
        for path, mark in (
            (str(REPO / "src"), "{SRC}"),
            (sys.prefix, "{PY}"),
            (sys.base_prefix, "{PY}"),
        )
        if path.rstrip("/") not in _SYSTEM_PREFIXES and path != "/"
    },
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
        # The voice engine choice reads this, never the real box (VO-17).
        "OPENDAISUGI_VOICE_HARDWARE": "16,8,0",
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


def mk(
    name: str, argv: list[str], *, before: dict[str, Any] | None = None, **kw: Any
) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "v": CASE_VERSION,
        "name": name,
        "argv": argv,
        "before": before or {},
    }
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
PY_AUDIT = "/usr/bin/python3 -m opendaisugi.gate_client --mode audit --root /x --format claude --verify-timeout 10.0"


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    # -- arm / disarm -------------------------------------------------------
    add(mk("disarm fresh", ["gate", "disarm"]))
    add(
        mk(
            "disarm loose root",
            ["gate", "disarm"],
            before={GROOT: d(0o755), ".opendaisugi": d(0o755)},
        )
    )
    add(
        mk(
            "disarm twice",
            ["gate", "disarm"],
            before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("old\n")},
        )
    )
    add(mk("disarm custom root", ["gate", "disarm", "--root", "{HOME}/r/g"]))
    add(mk("disarm root equals", ["gate", "disarm", "--root={HOME}/r2"]))
    add(
        mk("arm disarmed", ["gate", "arm"], before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x\n")})
    )
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
    odd.update(
        {
            f"{ENVD}/.hidden.json": f("{}"),
            f"{ENVD}/X.JSON": f("{}"),
            f"{ENVD}/notes.txt": f("x"),
            f"{ENVD}/dir.json": d(),
            f"{ENVD}/b c.json": f("{}"),
            f"{ENVD}/a.json.json": f("{}"),
        }
    )
    add(mk("status odd names", ["gate", "status", "--json"], before=odd))
    add(
        mk(
            "status disarmed",
            ["gate", "status"],
            before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x")},
        )
    )
    # A hook whose program is gone fails on every call; status warns.
    go_hook = (
        "DAISUGI_GATE_HOOK=opendaisugi.gate {HOME}/%s gate check --mode %s --root /x "
        "--format claude --verify-timeout 10.0"
    )
    add(
        mk(
            "status hook program gone enforce",
            ["gate", "status"],
            before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "enforce") + " || exit 2"))},
        )
    )
    add(
        mk(
            "status hook program gone audit json",
            ["gate", "status", "--json"],
            before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "audit")))},
        )
    )
    add(
        mk(
            "status hook program present",
            ["gate", "status"],
            before={
                "bin/daisugi": f("#!/bin/sh\n", 0o755),
                CLAUDE: f(hook_settings(go_hook % ("bin/daisugi", "enforce") + " || exit 2")),
            },
        )
    )
    add(
        mk(
            "status custom root",
            ["gate", "status", "--root", "{HOME}/g"],
            before={"g/envelopes/default.json": f("{}"), "config.yaml": f("gate_mode: enforce\n")},
        )
    )
    configs = {
        "cfg enforce": "gate_mode: enforce\n",
        "cfg audit": "gate_mode: audit\n",
        # The old name of audit mode: an unknown value, so audit.
        "cfg old shadow": "gate_mode: shadow\n",
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
        "cfg dup key": "gate_mode: enforce\ngate_mode: audit\n",
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
        add(
            mk(
                f"status {name}",
                ["gate", "status", "--json"],
                before={".opendaisugi/config.yaml": f(text)},
            )
        )
    hooks = {
        "hook global enforce": {CLAUDE: f(hook_settings(PY_ENFORCE))},
        "hook global audit": {
            CLAUDE: f(hook_settings(PY_AUDIT)),
            ".opendaisugi/config.yaml": f("gate_mode: enforce\n"),
        },
        "hook project": {"proj/.claude/settings.json": f(hook_settings(PY_ENFORCE))},
        "hook both": {
            CLAUDE: f(hook_settings(PY_AUDIT)),
            "proj/.claude/settings.json": f(hook_settings(PY_ENFORCE)),
        },
        "hook both audit": {
            CLAUDE: f(hook_settings(PY_AUDIT)),
            "proj/.claude/settings.json": f(hook_settings(PY_AUDIT)),
        },
        "hook bad json": {
            CLAUDE: f("{nope"),
            ".opendaisugi/config.yaml": f("gate_mode: enforce\n"),
        },
        "hook no mode": {CLAUDE: f(hook_settings("python3 -m opendaisugi.gate --root /x"))},
        "hook capture only": {
            CLAUDE: f(hook_settings("daisugi hook record --format claude --mode enforce"))
        },
        "hook mode suffix": {
            CLAUDE: f(hook_settings("python3 -m opendaisugi.gate --mode enforced"))
        },
        "hook go form": {
            CLAUDE: f(
                hook_settings(
                    "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
                )
            )
        },
        "hook hooks null": {CLAUDE: f('{"hooks": null}')},
        "hook pre null": {CLAUDE: f('{"hooks": {"PreToolUse": null}}')},
        "hook two entries": {
            CLAUDE: f(
                json.dumps(
                    {
                        "hooks": {
                            "PreToolUse": [
                                {
                                    "hooks": [
                                        {"command": "python3 -m opendaisugi.gate --mode audit"}
                                    ]
                                },
                                {
                                    "hooks": [
                                        {"command": "python3 -m opendaisugi.gate --mode enforce"}
                                    ]
                                },
                            ]
                        }
                    }
                )
            )
        },
        "hook bom": {CLAUDE: f("﻿" + hook_settings(PY_ENFORCE))},
        # A hook installed before the rename: the gate refuses --mode
        # shadow, so the hook denies every call. Status says so.
        "hook old shadow": {CLAUDE: f(hook_settings(PY_AUDIT.replace("audit", "shadow")))},
        "hook old shadow project": {
            "proj/.claude/settings.json": f(
                hook_settings(
                    "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode=shadow --root /x"
                )
            )
        },
        "hook old shadow beside enforce": {
            CLAUDE: f(hook_settings(PY_AUDIT.replace("audit", "shadow"))),
            "proj/.claude/settings.json": f(hook_settings(PY_ENFORCE)),
        },
    }
    for name, tree in hooks.items():
        add(mk(f"status {name}", ["gate", "status", "--json"], before=tree, cwd="proj"))
        add(mk(f"status {name} text", ["gate", "status"], before=tree, cwd="proj"))

    # -- report -------------------------------------------------------------
    def rec(tool: str, would_deny: bool, reason: str | None, detail: str = "x", **kw: Any) -> str:
        r = {
            "at": 1.5,
            "session_id": "s1",
            "tool_name": tool,
            "step_type": "shell",
            "detail": detail,
            "mode": "audit",
            "allow": True,
            "would_deny": would_deny,
            "reason": reason,
            "elapsed_ms": 1.0,
        }
        r.update(kw)
        return json.dumps(r)

    log1 = (
        "\n".join(
            [
                rec("Bash", False, None, "git status"),
                rec("Bash", True, "shell command contains metacharacters", "a && b"),
                rec("TodoWrite", True, "unrecognized tool 'TodoWrite'", ""),
                rec("Read", True, "path '/etc/passwd' not in file_read", "/etc/passwd"),
                "",
                "not json",
                rec("Write", True, "x" * 200, 'it\'s "q" é\n'),
            ]
        )
        + "\n"
    )
    log2 = rec("Bash", True, "path outside", "rm -rf /") + "\n"
    audit = {
        f"{GROOT}/audit/s1.jsonl": f(log1, 0o600),
        f"{GROOT}/audit/s2.jsonl": f(log2, 0o600),
    }
    add(mk("report none", ["gate", "report"]))
    add(mk("report none json", ["gate", "report", "--json"]))
    # The dialect audit: a word's would-deny rides in word_audit. The
    # report counts and lists them; gate status shows the count, the
    # dialect hash and the pin config.yaml names.
    words_log = (
        "\n".join(
            [
                rec(
                    "Write",
                    False,
                    "verified in envelope",
                    "/w/src/a.py",
                    word_audit=["dialect audit: one", "dialect audit: two"],
                ),
                rec("Bash", True, "x", "echo > /w/src/b", word_audit=["dialect audit: three"]),
                rec("Read", False, None, "/w/a", word_audit=[]),
                rec("Read", False, None, "/w/b", word_audit="not a list"),
                rec("Read", False, None, "/w/c", word_audit=[3, None]),
            ]
        )
        + "\n"
    )
    words = {f"{GROOT}/audit/w.jsonl": f(words_log, 0o600)}
    add(mk("report words", ["gate", "report"], before=words))
    add(mk("report words json", ["gate", "report", "--json"], before=words))
    add(mk("status words", ["gate", "status"], before=words))
    add(mk("status words json", ["gate", "status", "--json"], before=words))
    from opendaisugi.dialect import DIALECT_HASH

    for pname, cfg in (
        ("enforce", f"dialect_enforce: {DIALECT_HASH}\n"),
        ("mismatch", 'dialect_enforce: "0000000000000000"\n'),
        ("empty", 'dialect_enforce: ""\n'),
        ("int", "dialect_enforce: 12\n"),
    ):
        pinned = {**words, ".opendaisugi/config.yaml": f(cfg)}
        add(mk(f"status dialect pin {pname}", ["gate", "status"], before=pinned))
        add(mk(f"status dialect pin {pname} json", ["gate", "status", "--json"], before=pinned))
    bad_logs = {
        **words,
        f"{GROOT}/audit/x.jsonl": f('not json\n[1]\n{"would_deny": true, "reason": 5}\n'),
        f"{GROOT}/shadow/y.jsonl": f(
            rec("Write", True, None, "/w", word_audit=["dialect audit: old"]) + "\n"
        ),
    }
    add(mk("status words bad logs", ["gate", "status"], before=bad_logs))
    add(mk("report all", ["gate", "report"], before=audit))
    add(mk("report all json", ["gate", "report", "--json"], before=audit))
    add(mk("report session", ["gate", "report", "--session", "s2"], before=audit))
    add(mk("report missing session", ["gate", "report", "--session", "nope"], before=audit))
    add(mk("report unsafe session", ["gate", "report", "--session", "../s1"], before=audit))
    # The log written while audit mode was called shadow mode is read
    # first, then the audit log.
    legacy = {
        f"{GROOT}/shadow/s1.jsonl": f(log2, 0o600),
        f"{GROOT}/shadow/s3.jsonl": f(rec("Read", False, None, "/a") + "\n", 0o600),
        **audit,
    }
    add(mk("report legacy dir", ["gate", "report"], before=legacy))
    add(mk("report legacy dir json", ["gate", "report", "--json"], before=legacy))
    add(
        mk(
            "report legacy dir session",
            ["gate", "report", "--session", "s1", "--json"],
            before=legacy,
        )
    )
    add(
        mk(
            "report legacy dir only",
            ["gate", "report", "--json"],
            before={f"{GROOT}/shadow/s1.jsonl": f(log1, 0o600)},
        )
    )
    add(
        mk(
            "report reason null",
            ["gate", "report"],
            before={
                f"{GROOT}/audit/a.jsonl": f(
                    json.dumps({"would_deny": True, "reason": None})
                    + "\n"
                    + json.dumps({"would_deny": 1, "tool_name": None})
                    + "\n"
                )
            },
        )
    )
    add(
        mk(
            "report list line",
            ["gate", "report"],
            before={
                f"{GROOT}/audit/a.jsonl": f(
                    json.dumps({"would_deny": True, "reason": "x"}) + "\n[1, 2]\n"
                )
            },
        )
    )
    add(
        mk(
            "report no detail",
            ["gate", "report", "--json"],
            before={
                f"{GROOT}/audit/a.jsonl": f(
                    json.dumps({"would_deny": True, "reason": "metacharacters here"}) + "\n"
                )
            },
        )
    )

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
    add(mk("init quoted session", ["gate", "init", "--session", "a b'c", "--workspace", "/work"]))
    add(
        mk(
            "init exists",
            ["gate", "init", "--workspace", "/work"],
            before={f"{ENVD}/default.json": f("{}", 0o600)},
        )
    )
    add(
        mk(
            "init force",
            ["gate", "init", "--workspace", "/work", "--force"],
            before={f"{ENVD}/default.json": f("{}", 0o644), ENVD: d(0o755)},
        )
    )
    add(
        mk(
            "init decomposition",
            ["gate", "init", "--workspace", "/work", "--allow-shell-decomposition"],
        )
    )
    add(mk("init root", ["gate", "init", "--workspace", "/work", "--root", "{HOME}/g"]))

    # -- register -----------------------------------------------------------
    add(
        mk(
            "register default",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(id="env_fixed01"))},
        )
    )
    add(
        mk(
            "register session",
            ["gate", "register", "{HOME}/e.json", "--session", "s1"],
            before={"e.json": f(envelope_json(stakes="high", summary="short"))},
        )
    )
    add(
        mk(
            "register yaml",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: me\ntask: t\npermissions:\n  shell: true\n  shell_allowlist: [ls]\n"
                )
            },
        )
    )
    add(mk("register missing", ["gate", "register", "{HOME}/nope.json"]))
    add(
        mk(
            "register invalid",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f('{"task": "t"}')},
        )
    )
    add(
        mk(
            "register coerced",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(max_execution_time_s="60"))},
        )
    )
    add(
        mk(
            "register unknown keys",
            ["gate", "register", "{HOME}/e.json"],
            before={
                "e.json": f(
                    json.dumps(
                        {
                            "generated_by": "g",
                            "task": "t",
                            "extra": 1,
                            "permissions": {"file_read": ["/a"], "bogus": True},
                        }
                    )
                )
            },
        )
    )

    # -- install --gate -----------------------------------------------------
    claude_dir = {".claude": d()}
    gi = {"oracle": "gate-only-install"}
    # Enforce installs only with a registered envelope; with none it refuses.
    policy = {".opendaisugi/gate/envelopes/default.json": f(envelope_json(id="env_fixed01"))}
    add(mk("install gate claude", ["install", "--gate", "--yes"], before=claude_dir, **gi))
    add(
        mk(
            "install gate enforce",
            ["install", "--gate", "--enforce", "-y"],
            before={**claude_dir, **policy},
            **gi,
        )
    )
    add(
        mk(
            "install gate enforce no policy",
            ["install", "--gate", "--enforce", "-y"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gate enforce no policy dry run",
            ["install", "--gate", "--enforce", "--dry-run"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gate enforce empty envelopes dir",
            ["install", "--gate", "--enforce", "-y"],
            before={
                **claude_dir,
                ".opendaisugi/gate/envelopes": d(),
                ".opendaisugi/gate/envelopes/notes.txt": f("x"),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate enforce session policy",
            ["install", "--gate", "--enforce", "-y"],
            before={
                **claude_dir,
                ".opendaisugi/gate/envelopes/s1.json": f(envelope_json(id="env_fixed02")),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate ask",
            ["install", "--gate", "--enforce", "--ask", "--yes"],
            before=claude_dir,
            go_refuses=True,
        )
    )
    add(
        mk(
            "install gate ask audit",
            ["install", "--gate", "--ask", "--yes"],
            before=claude_dir,
            go_refuses=True,
        )
    )
    add(
        mk(
            "install gate old shadow flag",
            ["install", "--gate", "--shadow", "--yes"],
            before=claude_dir,
        )
    )
    add(mk("install old shadow flag alone", ["install", "--shadow"], before=claude_dir))
    add(
        mk(
            "install enforce no gate",
            ["install", "--enforce", "--yes"],
            before=claude_dir,
            go_refuses=True,
        )
    )
    other = json.dumps(
        {
            "permissions": {"allow": ["Bash(ls)"]},
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "Bash",
                        "hooks": [
                            {"type": "command", "command": "daisugi hook record --format claude"}
                        ],
                    }
                ]
            },
            "env": {"X": "é"},
        },
        indent=2,
    )
    add(
        mk(
            "install gate existing settings",
            ["install", "--gate", "--yes"],
            before={".claude": d(), CLAUDE: f(other, 0o600)},
            **gi,
        )
    )
    add(
        mk(
            "install gate invalid settings",
            ["install", "--gate", "--yes"],
            before={".claude": d(), CLAUDE: f("{oops")},
            **gi,
        )
    )
    add(
        mk(
            "install gate settings list",
            ["install", "--gate", "--yes"],
            before={".claude": d(), CLAUDE: f('{"hooks": []}')},
            **gi,
        )
    )
    add(
        mk(
            "install gate other mode",
            ["install", "--gate", "--enforce", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(PY_AUDIT)), **policy},
            **gi,
        )
    )
    add(
        mk(
            "install gate old module",
            ["install", "--gate", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "python3 -m opendaisugi.gate --mode audit --root {HOME}/.opendaisugi/gate --format claude --verify-timeout 10.0"
                    )
                ),
            },
            **gi,
        )
    )
    add(mk("install gate dry run", ["install", "--gate", "--dry-run"], before=claude_dir, **gi))
    add(
        mk(
            "install gate confirm yes",
            ["install", "--gate"],
            stdin="y\n",
            before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")},
            **gi,
        )
    )
    add(mk("install gate confirm no", ["install", "--gate"], stdin="n\n", before=claude_dir, **gi))
    add(mk("install gate none detected", ["install", "--gate", "--yes"], **gi))
    add(mk("install gate codex", ["install", "--gate", "--yes", "--runtime", "codex"], **gi))
    add(
        mk(
            "install gate codex path",
            ["install", "--gate", "--enforce", "--yes"],
            path_bins=["codex"],
            before=policy,
            **gi,
        )
    )
    add(
        mk(
            "install gate codex existing",
            ["install", "--gate", "--yes", "--runtime", "codex"],
            before={
                ".codex/hooks.json": f(
                    json.dumps(
                        {
                            "hooks": {
                                "PreToolUse": [
                                    {
                                        "matcher": ".*",
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "python3 -m opendaisugi.gate_client --mode enforce",
                                            }
                                        ],
                                    }
                                ]
                            }
                        }
                    )
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate all runtimes",
            ["install", "--gate", "--yes"],
            before={".claude": d(), ".codex": d(), ".hermes": d(), ".openclaw": d()},
            **gi,
        )
    )
    add(
        mk(
            "install gate runtime prefix",
            ["install", "--gate", "--yes", "--runtime", "cl", "--runtime", "hermes"],
            **gi,
        )
    )
    add(
        mk("install gate runtime ambiguous", ["install", "--gate", "--yes", "--runtime", "c"], **gi)
    )
    add(
        mk("install gate runtime unknown", ["install", "--gate", "--yes", "--runtime", "vim"], **gi)
    )
    add(
        mk(
            "install gate idempotent",
            ["install", "--gate", "--enforce", "--yes"],
            before={".claude": d(), **policy},
            twice=True,
            **gi,
        )
    )
    add(
        mk(
            "install gate idempotent renamed",
            ["install", "--gate", "--enforce", "--yes"],
            before={".claude": d(), **policy},
            twice=True,
            prog="daisugi-copy",
            **gi,
        )
    )
    # -- install --gateway (the BASE_URL layer) and --router ----------------
    add(
        mk(
            "install gateway claude",
            ["install", "--gate", "--gateway", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(mk("install gateway only", ["install", "--gateway", "--yes"], before=claude_dir, **gi))
    add(
        mk(
            "install gateway base url",
            ["install", "--gateway", "--base-url", "http://127.0.0.1:9999", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gateway empty base url",
            ["install", "--gateway", "--base-url", "", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gateway existing env",
            ["install", "--gateway", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {
                            "env": {"A": "1", "ANTHROPIC_BASE_URL": "http://old"},
                            "permissions": {"allow": ["Bash"]},
                        },
                        indent=2,
                    ),
                    0o600,
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gateway already set",
            ["install", "--gateway", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(json.dumps({"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"}})),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gateway invalid settings",
            ["install", "--gate", "--gateway", "--yes"],
            before={".claude": d(), CLAUDE: f("{nope")},
            **gi,
        )
    )
    add(
        mk(
            "install gateway env not a dict",
            ["install", "--gateway", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": ["x"]}))},
            **gi,
        )
    )
    add(
        mk(
            "install gateway gate then env fails",
            ["install", "--gate", "--gateway", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": "x"}))},
            **gi,
        )
    )
    add(
        mk(
            "install gateway settings list",
            ["install", "--gateway", "--yes"],
            before={".claude": d(), CLAUDE: f("[1]")},
            **gi,
        )
    )
    add(
        mk(
            "install gateway no claude dir",
            ["install", "--gateway", "--yes", "--runtime", "claude"],
            **gi,
        )
    )
    add(
        mk(
            "install gateway dry run",
            ["install", "--gate", "--gateway", "--dry-run"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gateway enforce no gate",
            ["install", "--gateway", "--enforce", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install gateway codex",
            ["install", "--gate", "--gateway", "--yes", "--runtime", "codex"],
            **gi,
        )
    )
    add(
        mk(
            "install gateway codex existing",
            ["install", "--gateway", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('model = "gpt-5"\n\n[profiles.x]\nmodel = "o3"\n\n\n')},
            **gi,
        )
    )
    add(
        mk(
            "install gateway codex selected",
            ["install", "--gateway", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('model_provider = "opendaisugi"\nmodel = "x"\n')},
            **gi,
        )
    )
    add(
        mk(
            "install gateway codex present",
            ["install", "--gateway", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('[model_providers.opendaisugi]\nbase_url = "x"\n')},
            **gi,
        )
    )
    add(
        mk(
            "install gateway all runtimes",
            ["install", "--gate", "--gateway", "--yes"],
            before={".claude": d(), ".codex": d(), ".hermes": d(), ".openclaw": d()},
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw existing",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            before={
                ".openclaw/openclaw.json": f(
                    json.dumps(
                        {"models": {"providers": {"x": {"baseUrl": "y"}}}, "agents": [1]}, indent=2
                    )
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw json5",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            before={
                ".openclaw/openclaw.json": f(
                    '{\n  // mine\n  "a": "http://x/y", /* b */ "b": [1, 2,],\n}\n'
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw present",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            before={
                ".openclaw/openclaw.json": f(
                    json.dumps({"models": {"providers": {"opendaisugi": {}}}})
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw invalid",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            before={".openclaw/openclaw.json": f("{nope")},
            **gi,
        )
    )
    add(
        mk(
            "install gateway openclaw models list",
            ["install", "--gateway", "--yes", "--runtime", "openclaw"],
            before={".openclaw/openclaw.json": f(json.dumps({"models": []}))},
            **gi,
        )
    )
    add(
        mk("install gateway hermes", ["install", "--gateway", "--yes", "--runtime", "hermes"], **gi)
    )
    add(
        mk(
            "install gateway idempotent",
            ["install", "--gate", "--gateway", "--yes"],
            before={".claude": d(), ".codex": d(), ".openclaw": d()},
            twice=True,
            **gi,
        )
    )
    # The binary under another file name: the second install finds its own
    # hooks by their shape (ruling INS-1).
    add(
        mk(
            "install gateway idempotent renamed",
            ["install", "--gate", "--gateway", "--yes"],
            before={".claude": d(), ".codex": d(), ".openclaw": d()},
            twice=True,
            prog="daisugi-copy",
            **gi,
        )
    )
    add(
        mk(
            "install router rules",
            ["install", "--gateway", "--router", "rules", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router off keeps config",
            ["install", "--gateway", "--router", "off", "--yes"],
            before={
                ".claude": d(),
                ".opendaisugi/config.yaml": f(
                    "gate_mode: enforce\nmax_task_chars: '12'\nauto_tend: yes\nzzz: 1\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "llama3",
                "--yes",
            ],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard claude key",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "claude-haiku-4-5",
                "--api-key-env",
                " MYKEY ",
                "--yes",
            ],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard key set",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "claude-haiku-4-5",
                "--api-key-env",
                "MYKEY",
                "--yes",
            ],
            before=claude_dir,
            env={"MYKEY": "k"},
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard clear key",
            ["install", "--gateway", "--router", "switchyard", "--api-key-env", "", "--yes"],
            before={
                ".claude": d(),
                ".opendaisugi/config.yaml": f(
                    "switchyard_efficient_model: m\nswitchyard_api_key_env: K\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard no efficient",
            ["install", "--gateway", "--router", "switchyard", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard same model",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "claude-sonnet-5",
                "--yes",
            ],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router switchyard capable",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "qwen3",
                "--capable-model",
                "claude-opus-4-8",
                "--yes",
            ],
            before={
                ".claude": d(),
                ".opendaisugi/config.yaml": f(
                    "llm_base_url: http://10.0.0.2:11434/\nllm_host_kind: openai\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install router unknown",
            ["install", "--gateway", "--router", "magic", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router without gateway",
            ["install", "--gate", "--router", "rules", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router flags without router",
            ["install", "--gateway", "--efficient-model", "m", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router dry run",
            [
                "install",
                "--gateway",
                "--router",
                "switchyard",
                "--efficient-model",
                "m",
                "--dry-run",
            ],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router invalid config",
            ["install", "--gateway", "--router", "switchyard", "--efficient-model", "m", "--yes"],
            before={".claude": d(), ".opendaisugi/config.yaml": f("gateway_router: [1]\n")},
            **gi,
        )
    )
    add(
        mk(
            "install report refused",
            ["install", "--gate", "--report", "herdr", "--yes"],
            before=claude_dir,
            go_refuses=True,
        )
    )
    add(mk("install plain refused", ["install", "--yes"], before=claude_dir, go_refuses=True))
    add(
        mk(
            "install decomposition refused",
            ["install", "--gate", "--allow-shell-decomposition", "--yes"],
            before=claude_dir,
            go_refuses=True,
        )
    )

    # -- uninstall ----------------------------------------------------------
    gu = {"oracle": "gate-only-uninstall"}
    installed = json.dumps(
        {
            "env": {"A": "1"},
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "Bash",
                        "hooks": [
                            {"type": "command", "command": "daisugi hook record --format claude"}
                        ],
                    },
                    {"matcher": "*", "hooks": [{"type": "command", "command": PY_ENFORCE}]},
                ],
                "Stop": [
                    {
                        "hooks": [
                            {
                                "type": "command",
                                "command": "daisugi hook record --format claude --event stop",
                            }
                        ]
                    }
                ],
                "SubagentStart": [
                    {
                        "hooks": [
                            {
                                "type": "command",
                                "command": "daisugi hook record --format claude --event subagent_start",
                            }
                        ]
                    }
                ],
            },
        },
        indent=2,
    )
    add(
        mk(
            "uninstall gate claude",
            ["install", "--gate", "--uninstall"],
            before={".claude": d(), CLAUDE: f(installed, 0o600)},
            **gu,
        )
    )
    add(
        mk(
            "uninstall gate only hook",
            ["install", "--gate", "--uninstall"],
            before={".claude": d(), CLAUDE: f(hook_settings(PY_AUDIT))},
            **gu,
        )
    )
    add(mk("uninstall gate nothing", ["install", "--gate", "--uninstall"], before=claude_dir, **gu))
    add(
        mk(
            "uninstall gate codex",
            ["install", "--gate", "--uninstall", "--runtime", "codex"],
            before={".codex/hooks.json": f(hook_settings(PY_ENFORCE))},
            **gu,
        )
    )
    add(
        mk(
            "uninstall gate codex renamed",
            ["install", "--gate", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/hooks.json": f(
                    hook_settings(
                        "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/bin/daisugi-copy gate check "
                        "--mode audit --root /x --format codex --verify-timeout 10.0"
                    )
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall gate go form",
            ["install", "--gate", "--uninstall"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
                    )
                ),
            },
            **gu,
        )
    )
    add(mk("uninstall gate none detected", ["install", "--gate", "--uninstall"], **gu))
    gateway_settings = json.dumps(
        {
            "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787", "B": "2"},
            "hooks": {
                "PreToolUse": [
                    {"matcher": "*", "hooks": [{"type": "command", "command": PY_AUDIT}]}
                ]
            },
        },
        indent=2,
    )
    add(
        mk(
            "uninstall gateway claude",
            ["install", "--gateway", "--uninstall"],
            before={".claude": d(), CLAUDE: f(gateway_settings)},
            **gu,
        )
    )
    add(
        mk(
            "uninstall gate and gateway env only key",
            ["install", "--gate", "--uninstall"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": {"ANTHROPIC_BASE_URL": "x"}}))},
            **gu,
        )
    )
    add(
        mk(
            "uninstall gateway codex",
            ["install", "--gateway", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/config.toml": f(
                    'model_provider = "opendaisugi"\nmodel = "m"\n\n[model_providers.opendaisugi]\n'
                    'name = "openDaisugi gateway"\nbase_url = "http://127.0.0.1:8787/v1"\n'
                    'wire_api = "chat"\n\n[profiles.p]\nx = 1\n'
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall gateway codex only block",
            ["install", "--gateway", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/config.toml": f(
                    'model_provider = "opendaisugi"\n\n[model_providers.opendaisugi]\nname = "x"\n'
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall gateway openclaw",
            ["install", "--gateway", "--uninstall", "--runtime", "openclaw"],
            before={
                ".openclaw/openclaw.json": f(
                    json.dumps(
                        {"models": {"providers": {"opendaisugi": {"baseUrl": "x"}}}, "other": 1}
                    )
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall gateway openclaw keeps others",
            ["install", "--gateway", "--uninstall", "--runtime", "openclaw"],
            before={
                ".openclaw/openclaw.json": f(
                    '{"models": {"providers": {"opendaisugi": {}, "x": {}}, "k": 1}, // c\n}'
                )
            },
            **gu,
        )
    )

    # -- harness extensions -------------------------------------------------
    add(mk("harness pi", ["install", "--harness", "pi"]))
    add(mk("harness opencode", ["install", "--harness", "opencode"]))
    add(mk("harness both", ["install", "--harness", "pi", "--harness", "opencode"]))
    add(
        mk(
            "harness opencode xdg",
            ["install", "--harness", "opencode"],
            env={"XDG_CONFIG_HOME": "{HOME}/xdg"},
        )
    )
    add(
        mk(
            "harness opencode xdg relative",
            ["install", "--harness", "opencode"],
            env={"XDG_CONFIG_HOME": "rel"},
        )
    )
    add(mk("harness dry run", ["install", "--harness", "pi", "--harness", "opencode", "--dry-run"]))
    add(mk("harness unknown", ["install", "--harness", "vim"]))
    add(
        mk(
            "harness symlink dir",
            ["install", "--harness", "opencode"],
            before={".config/opencode/plugins": {"link": "{HOME}/elsewhere"}, "elsewhere": d()},
        )
    )
    add(
        mk(
            "harness planted file link",
            ["install", "--harness", "pi"],
            before={
                ".pi/agent/extensions/daisugi-gate/index.ts": {"link": "{HOME}/target"},
                "target": f("mine"),
            },
        )
    )
    add(mk("harness same content", ["install", "--harness", "pi"], twice=True))
    add(
        mk(
            "harness uninstall",
            ["install", "--harness", "pi", "--harness", "opencode", "--uninstall"],
            before={
                ".pi/agent/extensions/daisugi-gate/index.ts": f("x"),
                ".config/opencode/plugins/daisugi-gate.ts": f("x"),
                ".config/opencode/plugins/mine.ts": f("y"),
            },
        )
    )
    add(
        mk(
            "harness uninstall nothing",
            ["install", "--harness", "pi", "--harness", "opencode", "--uninstall"],
        )
    )
    add(
        mk(
            "harness uninstall dry",
            ["install", "--harness", "pi", "--harness", "opencode", "--uninstall", "--dry-run"],
        )
    )

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
        add(
            mk(
                f"status {name}",
                ["gate", "status", "--json"],
                before={".opendaisugi/config.yaml": f(text)},
            )
        )
    add(
        mk(
            "init cfg decomposition",
            ["gate", "init", "--workspace", "/work"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: yes\n")},
        )
    )
    add(
        mk(
            "init cfg invalid",
            ["gate", "init", "--workspace", "/work"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: maybe\n")},
        )
    )
    add(
        mk(
            "init no decomposition flag",
            ["gate", "init", "--workspace", "/work", "--no-allow-shell-decomposition"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: true\n")},
        )
    )
    add(mk("init relative workspace", ["gate", "init", "--workspace", "sub/../w"], cwd="proj"))
    add(
        mk(
            "init symlink workspace",
            ["gate", "init", "--workspace", "{HOME}/link/x"],
            before={"real": d(), "link": {"link": "{HOME}/real"}},
        )
    )

    # -- register: YAML, coercions, robotics, predicates -------------------
    add(
        mk(
            "register yaml block",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "# my envelope\ngenerated_by: me\ntask: 'a task'\nstakes: high\npermissions:\n  file_read:\n  - /work/**\n"
                    "  - \"/data/**\"\n  shell: yes\n  shell_allowlist: [ls, git, 'rg']\n  max_execution_time_s: '45'\n"
                    "fallback:\n  include_refinement: off\n"
                )
            },
        )
    )
    add(
        mk(
            "register yaml seq of maps",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: me\ntask: t\npermissions: {}\ninvariants:\n- type: file_unchanged\n  description: keep it\n"
                    "  target: /etc/x\n- type: no_side_effects\n  description: none\n  enforce: false\n"
                )
            },
        )
    )
    add(
        mk(
            "register coerced bool",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(shell="yes", network=0))},
        )
    )
    add(
        mk(
            "register bad bool",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(shell="maybe"))},
        )
    )
    add(
        mk(
            "register float int",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(max_execution_time_s=60.0))},
        )
    )
    add(
        mk(
            "register robotics",
            ["gate", "register", "{HOME}/e.json"],
            before={
                "e.json": f(
                    envelope_json(
                        workspace_bounds=[[0, 0, 0], [1.5, 2, 1e-7]],
                        obstacles=[[[0.1, 0.2, 0.3], [1, 1, 1]]],
                        velocity_limit=2,
                        joint_limits={"j1": [-1.5, 1.5], "j2": [0, 3.14159]},
                        torque_limit=1e22,
                    )
                )
            },
        )
    )
    add(
        mk(
            "register float notation",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: g\ntask: t\npermissions:\n  velocity_limit: 0.000025\n  torque_limit: 9999999999999998.0\n"
                    "  joint_limits:\n    a: [0.0001, 0.00001]\n    b: [1000000000000000.0, 0.30000000000000004]\n"
                    "  workspace_bounds: [[-0.00002, 0.5, 100.0], [123456789012345.6, 1.5, 2.0]]\n"
                    "invariants:\n- type: t\n  description: d\n  expr: [0.00001, 1.0e-10, 2.5e-05]\n"
                )
            },
        )
    )
    add(
        mk(
            "register bad bounds",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(workspace_bounds=[[0, 0], [1, 1]]))},
        )
    )
    add(
        mk(
            "register predicates",
            ["gate", "register", "{HOME}/e.json", "--session", "p"],
            before={
                "e.json": f(
                    json.dumps(
                        {
                            "generated_by": "g",
                            "task": "t",
                            "permissions": {"shell": True},
                            "invariants": [
                                {
                                    "type": "pred",
                                    "description": "d",
                                    "expr": {"op": "lt", "args": [1.5, 2, "x", None, 1e-9]},
                                }
                            ],
                            "postconditions": [
                                {
                                    "type": "file_exists",
                                    "path": "/w/o",
                                    "expected": 1,
                                    "min": "2",
                                    "max": None,
                                }
                            ],
                        }
                    )
                )
            },
        )
    )
    add(
        mk(
            "register summary long",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(summary="x" * 81))},
        )
    )
    add(
        mk(
            "register bad stakes",
            ["gate", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(stakes="extreme"))},
        )
    )
    add(
        mk(
            "register unicode",
            ["gate", "register", "{HOME}/e.json"],
            before={
                "e.json": f(
                    json.dumps(
                        {
                            "generated_by": "g",
                            "task": "t\u00e9\u2603",
                            "permissions": {"file_read": ["/\u00e9/**"]},
                        }
                    )
                )
            },
        )
    )
    add(
        mk(
            "register top list",
            ["gate", "register", "{HOME}/e.yaml"],
            before={"e.yaml": f("- a\n")},
        )
    )
    # NaN or an infinity in any number is invalid (models.non_finite_error):
    # refused, nothing written. yaml.safe_load reads the JSON NaN,
    # Infinity and 1e999 as strings, which pydantic reads as floats; YAML's
    # .nan and .inf are floats already.
    for name, value in [
        ("nan", "NaN"),
        ("infinity", "Infinity"),
        ("minus infinity", "-Infinity"),
        ("1e999", "1e999"),
        ("nan string", '"nan"'),
        ("big int", "1" + "0" * 400),
    ]:
        add(
            mk(
                f"register {name} velocity",
                ["gate", "register", "{HOME}/e.json"],
                before={"e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", value))},
            )
        )
    for value in (".nan", ".NaN", ".inf", "-.inf", "+.inf", ".Inf"):
        add(
            mk(
                f"register yaml {value}",
                ["gate", "register", "{HOME}/e.yaml"],
                before={
                    "e.yaml": f(
                        f"generated_by: g\ntask: t\npermissions:\n  workspace_bounds: [[0, 0, 0], [1, {value}, 1]]\n"
                    )
                },
            )
        )
    add(
        mk(
            "register nan in expr",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: g\ntask: t\npermissions: {}\ninvariants:\n- type: t\n  description: d\n"
                    "  expr: {op: equals, path: x, value: [1, .nan]}\n"
                )
            },
        )
    )
    add(
        mk(
            "register nan joint limit",
            ["gate", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: g\ntask: t\npermissions:\n  joint_limits:\n    j: [-.inf, 1]\n"
                )
            },
        )
    )
    add(
        mk(
            "register nan keeps the old envelope",
            ["gate", "register", "{HOME}/e.json"],
            before={
                "e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", "NaN")),
                f"{GROOT}/envelopes/default.json": f(envelope_json(id="env_old00001")),
            },
        )
    )
    add(
        mk(
            "register nan and a bad field",
            ["gate", "register", "{HOME}/e.json"],
            before={
                "e.json": f(envelope_json(velocity_limit=1.5, shell="maybe").replace("1.5", "NaN"))
            },
        )
    )

    # -- report values -----------------------------------------------------
    add(
        mk(
            "report floats unicode",
            ["gate", "report"],
            before={
                f"{GROOT}/audit/a.jsonl": f(
                    "\n".join(
                        [
                            json.dumps(
                                {"would_deny": True, "reason": "r", "tool_name": 5, "detail": 1.5}
                            ),
                            json.dumps(
                                {
                                    "would_deny": True,
                                    "reason": "r\u00e9",
                                    "tool_name": "T",
                                    "detail": "\u00e9\u200b\u2028x\u3000",
                                }
                            ),
                            json.dumps({"would_deny": 0, "reason": ["x"]}),
                            json.dumps(
                                {
                                    "would_deny": "yes",
                                    "reason": "unrecognized tool 'X'",
                                    "detail": None,
                                }
                            ),
                        ]
                    )
                    + "\n"
                )
            },
        )
    )

    # -- proposals ---------------------------------------------------------
    props = {
        f"{GROOT}/proposals/b.json": f(
            json.dumps({"id": "p2", "kind": "widen", "scope": "s1", "expiresAt": 5})
        ),
        f"{GROOT}/proposals/a.json": f(json.dumps({"id": "p1", "kind": "narrow"})),
        f"{GROOT}/proposals/c.json": f("[1]"),
        f"{GROOT}/proposals/d.json": f("{bad"),
    }
    add(mk("proposals none", ["gate", "proposals"]))
    add(mk("proposals none json", ["gate", "proposals", "--json"]))
    add(mk("proposals some", ["gate", "proposals"], before=props))
    add(mk("proposals some json", ["gate", "proposals", "--json"], before=props))

    # -- the root and the commands this binary does not carry -------------
    add(mk("version", ["--version"]))
    add(mk("replay not ported", ["gate", "replay", "x", "--envelope", "y"], go_refuses=True))
    add(
        mk(
            "uninstall all not ported",
            ["install", "--uninstall"],
            before=claude_dir,
            go_refuses=True,
        )
    )
    add(mk("status unknown option", ["gate", "status", "--bogus"]))
    add(mk("register no argument", ["gate", "register"]))
    add(mk("install gate confirm eof", ["install", "--gate"], stdin="", before=claude_dir, **gi))
    add(
        mk(
            "install gate confirm again",
            ["install", "--gate"],
            stdin="maybe\ny\n",
            before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")},
            **gi,
        )
    )
    add(
        mk(
            "install gate claude forced missing",
            ["install", "--gate", "--yes", "--runtime", "claude"],
            **gi,
        )
    )
    add(
        mk(
            "install gate int command",
            ["install", "--gate", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": 5}]}]}}
                    )
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate no command key",
            ["install", "--gate", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps({"hooks": {"PreToolUse": [{"hooks": [{"type": "command"}]}]}})
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate pre dict",
            ["install", "--gate", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"hooks": {"PreToolUse": {}}}))},
            **gi,
        )
    )
    add(
        mk(
            "install gate other keys kept",
            ["install", "--gate", "--enforce", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {
                            "model": "x",
                            "hooks": {
                                "Stop": [{"hooks": [{"type": "command", "command": "a"}]}],
                                "PreToolUse": [],
                            },
                            "z": [1, 2.5, None, chr(0xE9) + " " + chr(0x2603)],
                        },
                        indent=4,
                    ),
                    0o600,
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate codex go form",
            ["install", "--gate", "--yes", "--runtime", "codex"],
            before={
                ".codex/hooks.json": f(
                    hook_settings(
                        "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode audit --root /x "
                        "--format claude --verify-timeout 10.0"
                    )
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install gate claude go form",
            ["install", "--gate", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce --root /x "
                        "--format claude --verify-timeout 10.0 || exit 2"
                    )
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "uninstall gate report hooks",
            ["install", "--gate", "--uninstall"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {
                            "hooks": {
                                "Notification": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "daisugi hook record --format claude --event notification",
                                            },
                                            {"type": "command", "command": "mine"},
                                        ]
                                    }
                                ],
                                "SubagentStop": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "daisugi hook record --event subagent_stop",
                                            }
                                        ]
                                    }
                                ],
                            }
                        }
                    )
                ),
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall gate bad json",
            ["install", "--gate", "--uninstall"],
            before={".claude": d(), CLAUDE: f("{x")},
            **gu,
        )
    )
    add(mk("bare", []))

    # -- hooks are found by their words, not a substring ---------------------
    foreign = "/opt/opendaisugi.gateway-watch --mode enforce"
    quoted = "'/opt/x --mode enforce/python' -m opendaisugi.gate --root /r"
    add(
        mk(
            "status foreign hook",
            ["gate", "status", "--json"],
            before={CLAUDE: f(hook_settings(foreign))},
        )
    )
    add(
        mk(
            "status quoted mode",
            ["gate", "status", "--json"],
            before={CLAUDE: f(hook_settings(quoted))},
        )
    )
    add(
        mk(
            "install gate foreign hook",
            ["install", "--gate", "--enforce", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(foreign))},
            **gi,
        )
    )
    add(
        mk(
            "install gate codex foreign hook",
            ["install", "--gate", "--yes", "--runtime", "codex"],
            before={".codex/hooks.json": f(hook_settings(foreign))},
            **gi,
        )
    )
    add(
        mk(
            "uninstall gate foreign hooks",
            ["install", "--gate", "--uninstall"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {
                            "hooks": {
                                "PreToolUse": [
                                    {
                                        "hooks": [
                                            {"type": "command", "command": foreign},
                                            {"type": "command", "command": PY_ENFORCE},
                                        ]
                                    }
                                ],
                                "Stop": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "mydaisugi hook record --event stop",
                                            },
                                            {
                                                "type": "command",
                                                "command": "daisugi hook record --event stop",
                                            },
                                        ]
                                    }
                                ],
                            }
                        },
                        indent=2,
                    )
                ),
            },
            **gu,
        )
    )
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
        "renamed": "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/bin/daisugi-copy gate check "
        "--mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2",
        "renamed no marker": "/opt/bin/daisugi-copy gate check --mode enforce --root /x "
        "--format claude --verify-timeout 10.0 || exit 2",
        "renamed few flags": "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/bin/daisugi-copy "
        "gate check --mode enforce || exit 2",
        "unknown": "nice python3 -m opendaisugi.gate --mode enforce",
        # Hooks from before audit mode was renamed: the warning's fix,
        # install --gate --uninstall, removes them.
        "old shadow python": "python3 -m opendaisugi.gate --mode shadow --root /x",
        "old shadow daisugi": "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check "
        "--mode shadow --root /x --format claude",
    }
    for name, command in written.items():
        tree = {".claude": d(), CLAUDE: f(hook_settings(command))}
        add(mk(f"status written {name}", ["gate", "status", "--json"], before=tree))
        add(
            mk(f"uninstall written {name}", ["install", "--gate", "--uninstall"], before=tree, **gu)
        )
    add(
        mk(
            "status written unknown text",
            ["gate", "status"],
            before={CLAUDE: f(hook_settings(written["unknown"]))},
        )
    )
    add(
        mk(
            "status unknown beside audit",
            ["gate", "status", "--json"],
            before={
                CLAUDE: f(hook_settings(PY_AUDIT)),
                "proj/.claude/settings.json": f(hook_settings(written["unknown"])),
            },
            cwd="proj",
        )
    )
    add(
        mk(
            "install gate beside unknown",
            ["install", "--gate", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(written["unknown"]))},
            **gi,
        )
    )
    add(
        mk(
            "install gate settings symlink",
            ["install", "--gate", "--yes"],
            before={
                ".claude": d(),
                "dot/settings.json": f("{}"),
                CLAUDE: {"link": "{HOME}/dot/settings.json"},
            },
            **gi,
        )
    )
    graft_cases(add)
    return C


def graft_cases(add: Any) -> None:
    """`daisugi graft install|status|remove`: the rule file in the gate
    root, and the refusal beside a PreToolUse hook that may rewrite input
    (GR-3). Every settings file is in the scratch HOME."""
    gate_cmd = "/usr/bin/python3 -m opendaisugi.gate --mode audit --format claude"
    record = "daisugi hook record --event pre"
    grafts = f"{GROOT}/grafts"
    rule = {
        "id": "big-read",
        "version": 3,
        "shape": "deny_redirect",
        "state": "active",
        "match": {"tool": "Read", "file_lines_over": 40},
    }

    def g(name: str, argv: list[str], **kw: Any) -> None:
        add(mk(f"graft {name}", ["graft", *argv], **kw))

    g("install default", ["install"])
    g("install active", ["install", "--state", "active", "--file-lines-over", "40"])
    g("install remote", ["install", "--allow-remote"])
    g("install id", ["install", "--id", "r.2"])
    g("install bad id", ["install", "--id", "../x"])
    g("install bad state", ["install", "--state", "on"])
    g("install zero lines", ["install", "--file-lines-over", "0"])
    g("install data dir", ["install", "--data-dir", "{HOME}/dd"])
    g(
        "install replaces",
        ["install", "--state", "active"],
        before={f"{grafts}/big-read.json": f(json.dumps(rule))},
    )
    g(
        "install replaces other id",
        ["install"],
        before={f"{grafts}/big-read.json": f(json.dumps({**rule, "id": "other"}))},
    )
    g(
        "install replaces bad file",
        ["install"],
        before={f"{grafts}/big-read.json": f("{")},
    )
    g(
        "install beside gate and capture",
        ["install"],
        before={CLAUDE: f(hook_settings(gate_cmd)), ".codex/hooks.json": f(hook_settings(record))},
    )
    g("install beside rival", ["install"], before={CLAUDE: f(hook_settings("rtk rewrite"))})
    g(
        "install beside codex rival",
        ["install"],
        before={".codex/hooks.json": f(hook_settings("shunt --pre"))},
    )
    g(
        "install beside project rival",
        ["install"],
        cwd="proj",
        before={"proj/.claude/settings.local.json": f(hook_settings("x\ny"))},
    )
    g(
        "install beside project settings",
        ["install"],
        cwd="proj",
        before={"proj/.claude/settings.json": f(hook_settings(gate_cmd))},
    )
    g("install bad settings", ["install"], before={CLAUDE: f("{")})
    g("install bom settings", ["install"], before={CLAUDE: f("\ufeff{}")})
    g(
        "install hook not a command",
        ["install"],
        before={
            CLAUDE: f(
                json.dumps({"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "x"}]}]}})
            )
        },
    )
    g(
        "install hooks odd shapes",
        ["install"],
        before={CLAUDE: f(json.dumps({"hooks": {"PreToolUse": [1, {"hooks": "x"}]}}))},
    )
    g(
        "install gate hook unread form",
        ["install"],
        before={CLAUDE: f(hook_settings("eval python -m opendaisugi.gate"))},
    )
    g("install trial", ["install", "--state", "trial"])
    g("install trial seed", ["install", "--state", "trial", "--seed", "42"])
    g("install trial seed max", ["install", "--state", "trial", "--seed", "9007199254740992"])
    g("install trial seed over", ["install", "--state", "trial", "--seed", "9007199254740993"])
    g(
        "install trial seed huge",
        ["install", "--state", "trial", "--seed", "99999999999999999999999"],
    )
    g("install trial seed negative", ["install", "--state", "trial", "--seed", "-1"])
    g("install trial seed not int", ["install", "--state", "trial", "--seed", "x"])
    g("install seed without trial", ["install", "--seed", "3"])
    g("status none", ["status"])
    g("status none json", ["status", "--json"])
    for label, fmt in (("text", []), ("json", ["--json"])):
        g(
            f"status rules {label}",
            ["status", *fmt],
            before={
                f"{grafts}/a.json": f(json.dumps({**rule, "id": "a", "state": "retired"})),
                f"{grafts}/b.json": f(json.dumps({**rule, "id": "b"})),
                f"{grafts}/c.json": f(json.dumps({**rule, "id": "c", "state": "audit"})),
                f"{grafts}/d.json": f("[]"),
                CLAUDE: f(hook_settings("rtk rewrite")),
            },
        )
        g(
            f"status trial {label}",
            ["status", *fmt],
            before={
                f"{grafts}/t.json": f(
                    json.dumps({**rule, "id": "t", "state": "trial", "trial": {"seed": 5}})
                ),
                f"{grafts}/u.json": f(json.dumps({**rule, "id": "u", "state": "trial"})),
            },
        )
    g("remove", ["remove"], before={f"{grafts}/big-read.json": f(json.dumps(rule))})
    g("remove missing", ["remove"])
    g("remove id", ["remove", "--id", "r.2"], before={f"{grafts}/r.2.json": f("{}")})
    g("remove bad id", ["remove", "--id", "a/b"])


# ---------------------------------------------------------------------------
# The everyday commands: status, config, journal and start
# ---------------------------------------------------------------------------
#
# These cases run through the garden runner (clients/garden_cases.py): a
# tree may hold a journal or a pathway store laid out by the oracle's own
# classes, a fake `claude` answers from a table of recorded replies, every
# database is dumped whole, and times and minted ids are normalized. On
# top of that, the path of the fake `claude` is written {BIN}, the digest
# `start` takes of the working directory {CWD8}, the interpreter
# {PYTHON}, Python's warnings as the binary prints them, JSON files
# parsed (a gate hook command compared by its flags), and a backup's
# time stamp dropped.
#
# A case may carry `go_expect`: fields where the binary's answer differs
# from the oracle's by a ruling (clients/ADJUDICATIONS.md, C-10 on). The
# generator derives them from the oracle's answer, so the compare stays
# exact. A `live` case depends on the box (its memory size): the compare
# runs the oracle again for it.

RICH_CONFIG = ".opendaisugi/config.yaml"
_WARN_LINE = re.compile(r"^.*?:\d+: \w*Warning: (.*)$")
_MS_JSON = re.compile(r'("duration_ms": )-?[0-9][0-9.e+-]*')
# `daisugi verify` prints how long the check took.
_MS_LINE = re.compile(r"(?m)^(  duration: )[0-9.]+ms$")


def _cwd8(home: Path, cwd: str) -> str:
    return hashlib.sha256(str((home / cwd).resolve()).encode()).hexdigest()[:8]


def rich_normalize(res: dict[str, Any], work: Path, case: dict[str, Any]) -> dict[str, Any]:
    home = work / "home"
    text = json.dumps(res, sort_keys=False, ensure_ascii=True)
    for raw, mark in (
        (str(work / "bin"), "{BIN}"),
        (sys.executable, "{PYTHON}"),
        (_cwd8(home, case.get("cwd", "")), "{CWD8}"),
    ):
        text = text.replace(json.dumps(raw)[1:-1], mark)
    res = json.loads(text)
    out: list[str] = []
    skip = False
    for line in res["stderr"]:
        if skip:
            skip = False
            if line.startswith("  "):
                continue
        m = _WARN_LINE.match(line)
        if m:
            out.append("warning: " + m.group(1))
            skip = True
            continue
        out.append(line)
    res["stderr"] = out
    if isinstance(res["stdout"], str):
        res["stdout"] = _MS_JSON.sub(r"\1{MS}", res["stdout"])
        res["stdout"] = _MS_LINE.sub(r"\1{MS}ms", res["stdout"])
    tree = {}
    for rel, entry in res["tree"].items():
        rel = _BAK.sub(".bak", rel)
        if "text" in entry and (rel.endswith(".json") or ".json.bak" in rel):
            try:
                parsed = json.loads(entry["text"])
            except ValueError:
                parsed = None
            if parsed is not None:
                entry = {"mode": entry["mode"], "json": norm_json(parsed, "{HOME}")}
        n = 2
        base = rel
        while rel in tree:
            rel = f"{base}#{n}"
            n += 1
        tree[rel] = entry
    res["tree"] = tree
    return res


def _run_once(
    case: dict[str, Any], cmd: list[str], work: Path, home: Path, raw: list | None
) -> subprocess.CompletedProcess:
    import garden_cases as g

    h = str(home)
    table = case.get("model") or {}
    (work / "table.json").write_text(json.dumps(table), encoding="utf-8")
    log_path = work / "model.log"
    log_path.touch()
    env = g.base_env(home, work)
    env["FAKE_MODEL_LOG"] = str(log_path)
    env["FAKE_MODEL_TABLE"] = str(work / "table.json")
    http_log: list[dict[str, Any]] = []
    with g.FakeServer(table, http_log) as srv:
        env["ANTHROPIC_API_BASE"] = f"http://127.0.0.1:{srv.port}"
        env.update(
            {
                k: v.replace("{HOME}", h).replace("{PORT}", str(srv.port))
                for k, v in (case.get("env") or {}).items()
            }
        )
        for k in case.get("unset") or []:
            env.pop(k, None)
        srv.key, srv.token = env.get("ANTHROPIC_API_KEY"), env.get("ANTHROPIC_AUTH_TOKEN")
        srv.openai_key = env.get("OPENAI_API_KEY")
        cwd = home / case.get("cwd", "")
        cwd.mkdir(parents=True, exist_ok=True)
        argv = [a.replace("{HOME}", h) for a in case["argv"]]
        old = os.umask(0o022)
        try:
            proc = subprocess.run(
                cmd + argv,
                capture_output=True,
                env=env,
                cwd=cwd,
                input=case.get("stdin", "").replace("{HOME}", h).encode("utf-8"),
                timeout=case.get("timeout", 300),
                check=False,
            )
        finally:
            os.umask(old)
    requests = [
        json.loads(ln) for ln in log_path.read_text(encoding="utf-8").splitlines() if ln.strip()
    ]
    log_path.unlink()
    requests += http_log
    if raw is not None:
        raw.extend(requests)
    proc.requests = requests  # type: ignore[attr-defined]
    return proc


def run_rich(
    case: dict[str, Any], cmd: list[str], work: Path, raw: list | None = None
) -> dict[str, Any]:
    """Run one case as garden_cases.run_case does, with the extras above:
    {CWD8} in a tree path is the digest of the case's working directory,
    and a `twice` case records its second run on the tree the first left."""
    import garden_cases as g

    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    (work / "bin").mkdir(parents=True)
    (work / "tmp").mkdir()
    t0 = time.time()
    digest = _cwd8(home, case.get("cwd", ""))
    before = {k.replace("{CWD8}", digest): v for k, v in (case.get("before") or {}).items()}
    g.lay_out(before, home, t0)
    if not case.get("no_claude"):
        fake = work / "bin" / "claude"
        fake.write_text(g.FAKE_CLAUDE.replace("{PYTHON}", sys.executable), encoding="utf-8")
        fake.chmod(0o755)
    live = None
    if case.get("live_gate"):
        # A gate that answers: a socket that listens while the command runs.
        live_path = home / case["live_gate"]
        live_path.parent.mkdir(parents=True, exist_ok=True)
        live = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        live.bind(str(live_path))
        live.listen()
    try:
        proc = _run_once(case, cmd, work, home, raw)
        if case.get("twice"):
            (work / "spawn.log").unlink(missing_ok=True)
            again = {**case, "argv": case.get("argv2") or case["argv"]}
            proc = _run_once(again, cmd, work, home, raw)
    finally:
        if live is not None:
            live.close()
            # The tree reader reads files; the socket was never the
            # command's to write.
            (home / case["live_gate"]).unlink(missing_ok=True)
    h = str(home)
    res: dict[str, Any] = {
        "exit": proc.returncode,
        "stdout": proc.stdout.decode("utf-8", "replace"),
        "stderr": g.norm_stderr(proc.stderr.decode("utf-8", "replace")),
        "tree": g.read_tree(home),
        "requests": [
            {k: v for k, v in r.items() if k != "headers" or r["kind"] == "http"}
            for r in proc.requests
        ],  # type: ignore[attr-defined]
    }
    spawn = work / "spawn.log"
    if case.get("oracle") == "start-shim" or spawn.exists():
        lines = spawn.read_text(encoding="utf-8").splitlines() if spawn.exists() else []
        res["spawned"] = [json.loads(ln) for ln in lines]
    res = g.normalize(res, h, t0)
    return rich_normalize(res, work, case)


def rich_lay_out(case: dict[str, Any], work: Path) -> float:
    """Lay the case's tree out under work/home; returns the time it did."""
    import garden_cases as g

    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    t0 = time.time()
    digest = _cwd8(home, case.get("cwd", ""))
    g.lay_out(
        {k.replace("{CWD8}", digest): v for k, v in (case.get("before") or {}).items()}, home, t0
    )
    (home / case.get("cwd", "")).mkdir(parents=True, exist_ok=True)
    return t0


def rich_before(case: dict[str, Any], work: Path) -> dict[str, Any]:
    """The case's tree before the command, normalized as a run's is."""
    import garden_cases as g

    home = work / "home"
    t0 = rich_lay_out(case, work)
    res = g.normalize(
        {"exit": 0, "stdout": "", "stderr": [], "tree": g.read_tree(home), "requests": []},
        str(home),
        t0,
    )
    tree = rich_normalize(res, work, case)["tree"]
    shutil.rmtree(work)
    return tree


def rich_expect(case: dict[str, Any]) -> dict[str, Any]:
    """What the binary must answer: the oracle's answer with the ruled
    fields replaced."""
    return {**case["expect"], **(case.get("go_expect") or {})}


def rich(
    name: str, argv: list[str], *, before: dict[str, Any] | None = None, **kw: Any
) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "v": CASE_VERSION,
        "runner": "rich",
        "name": name,
        "argv": argv,
        "before": before or {},
    }
    c.update(kw)
    return c


def fake_bin(name: str, script: str) -> dict[str, Any]:
    return {f".fakebin/{name}": {"text": "#!/bin/sh\n" + script, "mode": 0o755}}


GPU = fake_bin("nvidia-smi", 'echo "6144, Fake GPU 6GB"\n')
FAKE_PATH = {"PATH": "{HOME}/../bin:{HOME}/.fakebin:/usr/bin:/bin"}


def _status_go_expect(case: dict[str, Any]) -> dict[str, Any] | None:
    """C-12: under a matcher this binary does not carry (MiniLM or int8),
    the binary reports pathway reuse as off. The default is lexical."""
    cfg = (case.get("before") or {}).get(RICH_CONFIG, {}).get("text", "")
    if "matcher_model: lexical" in cfg or "matcher_model: potion" in cfg:
        return None
    exp = case["expect"]
    if exp["exit"] != 0:
        return None
    m = re.search(r"matcher_model: (\S+)", cfg)
    key = m.group(1) if m else "lexical"
    if key in ("lexical", "potion"):
        return None
    out = exp["stdout"]
    if "--json" in case["argv"]:
        body = json.loads(out)
        body["search_extra_installed"] = False
        body["token_savings_ready"] = False
        return {"stdout": json.dumps(body, indent=2) + "\n"}
    go_line = (
        f"  ✗ matcher {key} is not in this binary — pathways disabled; "
        "set matcher_model: lexical or potion\n"
    )
    out = out.replace("  ✓ [search] extra installed\n", go_line)
    # A matcher the oracle has not built at all: the same line, in the
    # binary's words.
    out = out.replace(
        f"  ✗ matcher {key} is not a built embedder — pathways disabled; "
        "set matcher_model: lexical or potion\n",
        go_line,
    )
    out = out.replace("  → ✓ token savings are LIVE\n", "  → ✗ not yet — run `daisugi onboard`\n")
    return {"stdout": out}


_START_VIEW = "the multi-session view (daisugi dashboard) is not in this binary yet"


def _start_go_expect(case: dict[str, Any]) -> dict[str, Any] | None:
    """G2-4: the binary starts its own `gate serve` where the oracle
    starts Python's; the compare runs it behind a wrapper that logs that
    spawn as the oracle's fake spawn logs its own."""
    exp = case["expect"]
    if case.get("go_refuses") or exp["exit"] == 2:
        return None
    spawned = []
    for argv in exp.get("spawned") or []:
        # {PYTHON} -c <shim> gate serve --root R
        spawned.append(["{DAISUGI}", *argv[argv.index("gate") :]])
    return {"spawned": spawned}


def start_without_view(case: dict[str, Any], expect: dict[str, Any]) -> dict[str, Any]:
    """C-11: a binary that does not carry `daisugi dashboard` has no
    view. Its view step reads `view  skipped  <_START_VIEW>`, and a case
    that opens the view (`view`) is refused before any step runs."""
    lines = []
    for line in expect["stdout"].splitlines():
        m = re.match(r"^  (\S+)(\s+)(\S+)(\s+)(.*)$", line)
        if m and m.group(1) == "view":
            lines.append(f"  {m.group(1)}{m.group(2)}skipped  {_START_VIEW}")
            break
        lines.append(line)
    return {**expect, "stdout": "\n".join(lines) + "\n" if lines else ""}


def settings_with(cmd: str) -> dict[str, Any]:
    return {"text": hook_settings(cmd)}


def build_rich_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    lexical = {RICH_CONFIG: {"text": "matcher_model: lexical\n"}}

    # -- config ------------------------------------------------------------
    add(rich("config fresh", ["config"]))
    add(rich("config fresh json", ["config", "--json"]))
    add(rich("config no claude", ["config"], no_claude=True))
    add(rich("config api key", ["config"], env={"ANTHROPIC_API_KEY": "sk-test-0000"}))
    add(rich("config env backend", ["config", "--json"], env={"OPENDAISUGI_LLM_BACKEND": " api "}))
    # config shows the value as set; the old name is refused only where a
    # backend is used (LLM-13).
    add(
        rich(
            "config env backend renamed",
            ["config", "--json"],
            env={"OPENDAISUGI_LLM_BACKEND": " litellm "},
        )
    )
    values = (
        "model: m\nmax_task_chars: '1_000'\nz3_timeout_ms: 800.0\ndata_dir: ~/x//y/./\nauto_tend: yes\n"
        "gateway_local_model: null\nshell_allow_decomposition: 'on'\ngate_mode: enforce\nmatcher_model: potion\n"
        "llm_backend: '  claude-code  '\nllm_context_window: -0\nvoice_cleanup: 1\nfloor:\n  backend: tmux\n"
        "  notify_cmd: notify-send hi\n  bogus: 1\nmystery: 2\nabc: [1]\n"
    )
    add(rich("config values", ["config"], before={RICH_CONFIG: {"text": values}}))
    add(rich("config values json", ["config", "--json"], before={RICH_CONFIG: {"text": values}}))
    add(rich("config falsy top", ["config"], before={RICH_CONFIG: {"text": "0\n"}}))
    add(
        rich(
            "config empty file", ["config", "--json"], before={RICH_CONFIG: {"text": "# nothing\n"}}
        )
    )
    add(
        rich("config invalid bool", ["config"], before={RICH_CONFIG: {"text": "gate_ask: maybe\n"}})
    )
    add(
        rich(
            "config invalid int", ["config"], before={RICH_CONFIG: {"text": "z3_timeout_ms: 1.5\n"}}
        )
    )
    add(rich("config top list", ["config"], before={RICH_CONFIG: {"text": "- a\n"}}))
    add(rich("config top string", ["config"], before={RICH_CONFIG: {"text": "hello\n"}}))
    add(rich("config floor null", ["config"], before={RICH_CONFIG: {"text": "floor: null\n"}}))
    add(
        rich(
            "config anchor",
            ["config"],
            before={RICH_CONFIG: {"text": "model: &a m\nvoice_model: *a\n"}},
        )
    )
    add(rich("config bool key", ["config"], before={RICH_CONFIG: {"text": "on: 1\n"}}))
    add(rich("config extra arg", ["config", "show"]))
    add(rich("config unknown option", ["config", "--bogus"]))
    audit = settings_with(PY_AUDIT)
    enforce = settings_with(PY_ENFORCE)
    add(rich("config global hook", ["config"], before={CLAUDE: enforce}))
    add(
        rich(
            "config project hook",
            ["config"],
            before={"proj/.claude/settings.json": enforce},
            cwd="proj",
        )
    )
    add(
        rich(
            "config both hooks",
            ["config"],
            before={CLAUDE: audit, "proj/.claude/settings.json": enforce},
            cwd="proj",
        )
    )
    add(
        rich(
            "config both hooks json",
            ["config", "--json"],
            before={CLAUDE: enforce, "proj/.claude/settings.json": audit},
            cwd="proj",
        )
    )
    add(
        rich(
            "config hook and file mode",
            ["config"],
            before={CLAUDE: audit, RICH_CONFIG: {"text": "gate_mode: enforce\n"}},
        )
    )
    add(
        rich("config file mode", ["config"], before={RICH_CONFIG: {"text": "gate_mode: enforce\n"}})
    )

    # -- status --------------------------------------------------------------
    st = {**GPU}
    add(rich("status fresh", ["status"], before=st, env=FAKE_PATH))
    add(rich("status fresh json", ["status", "--json"], before=st, env=FAKE_PATH))
    add(rich("status lexical", ["status"], before={**st, **lexical}, env=FAKE_PATH))
    add(
        rich(
            "status potion json",
            ["status", "--json"],
            before={**st, RICH_CONFIG: {"text": "matcher_model: potion\n"}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status int8",
            ["status"],
            before={**st, RICH_CONFIG: {"text": "matcher_model: int8\n"}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status unknown matcher",
            ["status"],
            before={**st, RICH_CONFIG: {"text": "matcher_model: nope\n"}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status unknown matcher threshold",
            ["status", "--threshold", "0.3"],
            before={**st, RICH_CONFIG: {"text": "matcher_model: nope\n"}},
            env=FAKE_PATH,
        )
    )
    add(rich("status threshold", ["status", "--threshold", " 0.7 "], before=st, env=FAKE_PATH))
    add(
        rich(
            "status threshold nan json",
            ["status", "--threshold", "nan", "--json"],
            before=st,
            env=FAKE_PATH,
        )
    )
    add(rich("status threshold inf", ["status", "--threshold", "-inf"], before=st, env=FAKE_PATH))
    add(rich("status threshold bad", ["status", "--threshold", "x"], before=st, env=FAKE_PATH))
    add(
        rich(
            "status home config invalid",
            ["status"],
            before={**st, RICH_CONFIG: {"text": "gate_ask: maybe\n"}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status data dir config invalid",
            ["status", "--data-dir", "{HOME}/d"],
            before={**st, "d/config.yaml": {"text": "max_task_chars: x\n"}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status decomposition on",
            ["status"],
            before={
                **st,
                **lexical,
                "d/config.yaml": {"text": "shell_allow_decomposition: true\n"},
            },
            env=FAKE_PATH,
        )
    )
    C[-1]["argv"] = ["status", "--data-dir", "{HOME}/d"]
    add(
        rich(
            "status relative data dir",
            ["status", "--data-dir", "d//x/"],
            before={**st, **lexical},
            env=FAKE_PATH,
        )
    )
    import garden_cases as g
    from garden_cases import JDIR, rd, sh, trace  # noqa: F401 - the journal helpers

    build = [g.sh("s1", "make test"), g.rd("s2", "/work/out.txt", ["s1"])]
    traces = [g.trace(k, f"build the release {k}", build, hours=k + 1) for k in range(1, 4)]
    traces[2]["ok"] = False
    jr = {".opendaisugi": {"journal": {"traces": traces}}}
    add(rich("status journal", ["status"], before={**st, **lexical, **jr}, env=FAKE_PATH))
    add(rich("status journal json", ["status", "--json"], before={**st, **jr}, env=FAKE_PATH))
    # What the dialect's words would deny: plans in the journal (a trace
    # whose result holds a dialect audit warning) and calls in the gate log.
    audit_line = (
        "dialect audit: invariant 'read_only' is keep_unchanged('**'); "
        "step 's1' writes '/work/out.txt'; enforcing would deny"
    )
    worded = [dict(t) for t in traces]
    worded[0]["warnings"] = [audit_line]
    worded[1]["warnings"] = ["the Z3 check timed out", audit_line, audit_line]
    worded[2]["warnings"] = ["dialect is a word here, not an audit line"]
    jw = {
        ".opendaisugi": {
            "journal": {
                "traces": worded,
                "yaml_text": {
                    "bad": "result: [dialect\n",
                    "odd": "- dialect\n",
                    "str": "result:\n  warnings: 'dialect audit: x'\n",
                    "deep": "result:\n  warnings:\n  - 'dialect audit: x'\n",
                },
            }
        },
        f"{GROOT}/audit/w.jsonl": {
            "text": json.dumps({"word_audit": [audit_line]})
            + "\n"
            + json.dumps({"word_audit": []})
            + "\n"
        },
    }
    add(rich("status word would deny", ["status"], before={**st, **lexical, **jw}, env=FAKE_PATH))
    add(
        rich(
            "status word would deny json",
            ["status", "--json"],
            before={**st, **lexical, **jw},
            env=FAKE_PATH,
        )
    )
    from pathway_cases import pathway, put_row, row_spec

    store = {
        ".opendaisugi/pathways.db": {
            "db": {
                "rows": [
                    row_spec(put_row(pathway(1, "build it", [1.0, 0.0], hit_count=3))),
                    row_spec(put_row(pathway(2, "test it", [0.0, 1.0], hit_count=4))),
                ]
            }
        }
    }
    add(rich("status pathways live", ["status"], before={**st, **lexical, **store}, env=FAKE_PATH))
    minilm = {RICH_CONFIG: {"text": "matcher_model: all-MiniLM-L6-v2\n"}}
    add(rich("status pathways minilm", ["status"], before={**st, **minilm, **store}, env=FAKE_PATH))
    add(rich("status pathways default", ["status"], before={**st, **store}, env=FAKE_PATH))
    add(
        rich(
            "status pathways json",
            ["status", "--json"],
            before={**st, **lexical, **store},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status corrupt store",
            ["status"],
            before={
                **st,
                **lexical,
                ".opendaisugi/pathways.db": {"text": "not a database at all" * 50},
            },
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status data dir is a file",
            ["status", "--data-dir", "{HOME}/f"],
            before={**st, **lexical, "f": {"text": "x"}},
            env=FAKE_PATH,
        )
    )
    tier = ".opendaisugi/local_tier1.json"
    add(
        rich(
            "status tier1 wired",
            ["status"],
            before={
                **st,
                **lexical,
                tier: {
                    "text": json.dumps(
                        {"model": "qwen2.5-3b", "base_url": "http://127.0.0.1:8080/v1"}
                    )
                },
            },
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status tier1 no base url",
            ["status"],
            before={**st, **lexical, tier: {"text": json.dumps({"model": "ollama/llama3"})}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status tier1 null base url",
            ["status"],
            before={**st, **lexical, tier: {"text": json.dumps({"model": "m", "base_url": None})}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status tier1 empty model",
            ["status"],
            before={**st, **lexical, tier: {"text": json.dumps({"model": ""})}},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status tier1 invalid json",
            ["status"],
            before={**st, **lexical, tier: {"text": "{bad"}},
            env=FAKE_PATH,
        )
    )
    for name, out in (
        ("gpu 8gb", 'echo "8192, Big"\necho "4096, Small"'),
        ("gpu 16gb", 'echo " 16384 , X "'),
        ("gpu 40gb", 'echo "40960, A100"'),
        ("gpu tiny", 'echo "1024, T"'),
        ("gpu no comma", 'echo "8192"'),
        ("gpu bad number", 'echo "lots, X"'),
    ):
        add(
            rich(
                f"status {name}",
                ["status"],
                before={**lexical, **fake_bin("nvidia-smi", out + "\n")},
                env=FAKE_PATH,
                live=name in ("gpu no comma", "gpu bad number"),
            )
        )
    add(
        rich(
            "status gpu fails",
            ["status"],
            before={**lexical, **fake_bin("nvidia-smi", "exit 9\n")},
            env=FAKE_PATH,
            live=True,
        )
    )
    add(rich("status extra arg", ["status", "x"]))

    # -- journal stats, replay, search -------------------------------------
    add(rich("journal stats empty", ["journal", "stats"]))
    add(rich("journal stats json", ["journal", "stats", "--json"], before=jr))
    add(rich("journal stats text", ["journal", "stats"], before=jr))
    add(
        rich(
            "journal stats data dir",
            ["journal", "stats", "--data-dir", "{HOME}/j"],
            before={"j": {"journal": {"traces": traces}}},
        )
    )
    add(
        rich(
            "journal stats data dir is a file",
            ["journal", "stats", "--data-dir", "{HOME}/f"],
            before={"f": {"text": "x"}},
        )
    )
    bad = [g.sh("s1", "rm -rf /tmp/x"), g.rd("s2", "/work/out.txt", ["s1"])]
    rtraces = [
        g.trace(1, "build the release", build, hours=3),
        g.trace(2, "build the release again", build, hours=2, ok=False),
        g.trace(3, "clean up", bad, hours=1),
        g.trace(4, "loose ends", [g.sh("s1", "make test", ["zz"])], hours=0.5),
        g.trace(
            5,
            "guarded",
            build,
            hours=0.4,
            env=dict(
                g.t_env(5),
                invariants=[
                    {
                        "type": "no_rm",
                        "description": "d",
                        "expr": {
                            "child": {
                                "op": "exists_step",
                                "pred": {"op": "matches", "path": "command", "regex": "make"},
                            },
                            "op": "not",
                        },
                    }
                ],
            ),
        ),
    ]
    rj = {".opendaisugi": {"journal": {"traces": rtraces}}}
    add(rich("journal replay no drift", ["journal", "replay", "t01"], before=rj))
    add(rich("journal replay no drift json", ["journal", "replay", "t01", "--json"], before=rj))
    add(rich("journal replay drift to pass", ["journal", "replay", "t02"], before=rj))
    add(
        rich("journal replay drift to pass json", ["journal", "replay", "t02", "--json"], before=rj)
    )
    add(rich("journal replay drift to fail", ["journal", "replay", "t03"], before=rj))
    add(
        rich("journal replay drift to fail json", ["journal", "replay", "t03", "--json"], before=rj)
    )
    add(rich("journal replay dag json", ["journal", "replay", "t04", "--json"], before=rj))
    add(rich("journal replay predicate", ["journal", "replay", "t05"], before=rj))
    add(rich("journal replay predicate json", ["journal", "replay", "t05", "--json"], before=rj))
    add(rich("journal replay not found", ["journal", "replay", "nope"], before=rj))
    add(rich("journal replay no journal", ["journal", "replay", "t01"]))
    add(rich("journal replay missing arg", ["journal", "replay"]))
    add(
        rich(
            "journal replay bad yaml key",
            ["journal", "replay", "tx"],
            before={
                ".opendaisugi": {
                    "journal": {"traces": rtraces[:1], "yaml_text": {"tx": "id: tx\ntask: t\n"}}
                }
            },
        )
    )
    tasks = [
        "deploy the web app to staging",
        "build the release notes",
        "run unit tests for the parser",
        "build and deploy the release",
        "write the weekly report",
    ]
    straces = [g.trace(k, t, build, hours=10 - k) for k, t in enumerate(tasks, start=1)]
    sj = {".opendaisugi": {"journal": {"traces": straces}}}
    add(
        rich(
            "journal search lexical",
            ["journal", "search", "build the release", "--limit", "2"],
            before={**lexical, **sj},
        )
    )
    add(
        rich(
            "journal search lexical json",
            ["journal", "search", "deploy release", "--json", "--limit", "3"],
            before={**lexical, **sj},
        )
    )
    add(
        rich(
            "journal search lexical limit",
            ["journal", "search", "unit tests parser", "--limit", "1"],
            before={**lexical, **sj},
        )
    )
    add(
        rich(
            "journal search lexical limit zero",
            ["journal", "search", "report", "--limit", "0"],
            before={**lexical, **sj},
        )
    )
    add(rich("journal search lexical empty", ["journal", "search", "anything"], before=lexical))
    add(
        rich(
            "journal search lexical empty json",
            ["journal", "search", "anything", "--json"],
            before=lexical,
        )
    )
    minilm_cfg = {RICH_CONFIG: {"text": "matcher_model: all-MiniLM-L6-v2\n"}}
    add(rich("journal search minilm", ["journal", "search", "build"], before={**minilm_cfg, **sj}))
    add(rich("journal search default", ["journal", "search", "build"], before=sj))
    add(
        rich(
            "journal search int8",
            ["journal", "search", "build"],
            before={**sj, RICH_CONFIG: {"text": "matcher_model: int8\n"}},
        )
    )
    add(
        rich(
            "journal search unknown matcher",
            ["journal", "search", "build"],
            before={**sj, RICH_CONFIG: {"text": "matcher_model: nope\n"}},
        )
    )
    add(
        rich(
            "journal search config invalid",
            ["journal", "search", "build"],
            before={**sj, RICH_CONFIG: {"text": "gate_ask: maybe\n"}},
        )
    )
    add(
        rich(
            "journal search bad limit",
            ["journal", "search", "build", "--limit", "x"],
            before=lexical,
        )
    )
    from pathway_cases import FIXTURE_DIR as PW

    tiny = {
        "potion-tiny/" + f: {"hex": (PW / "potion-tiny" / f).read_bytes().hex()}
        for f in ("config.json", "tokenizer.json", "model.safetensors")
    }
    potion = {RICH_CONFIG: {"text": "matcher_model: potion\n"}, **tiny}
    add(
        rich(
            "journal search potion",
            ["journal", "search", "build the release", "--limit", "2"],
            before={**potion, **sj},
            env={"OPENDAISUGI_POTION_MODEL": "{HOME}/potion-tiny"},
        )
    )
    add(
        rich(
            "journal search potion missing model",
            ["journal", "search", "build"],
            before={**sj, RICH_CONFIG: {"text": "matcher_model: potion\n"}},
            env={"OPENDAISUGI_POTION_MODEL": "{HOME}/no-such-model"},
        )
    )
    add(
        rich(
            "journal search potion json",
            ["journal", "search", "deploy", "--json", "--limit", "1"],
            before={**potion, **sj},
            env={"OPENDAISUGI_POTION_MODEL": "{HOME}/potion-tiny"},
        )
    )

    # -- journal parse and ingest ----------------------------------------------
    def row(**kw: Any) -> str:
        return json.dumps(kw, ensure_ascii=False)

    def asst(*blocks: dict[str, Any]) -> str:
        return row(type="assistant", message={"role": "assistant", "content": list(blocks)})

    def tu(name: str, **inp: Any) -> dict[str, Any]:
        return {"type": "tool_use", "id": "toolu_x", "name": name, "input": inp}

    def user(text: Any) -> str:
        return row(type="user", message={"role": "user", "content": text}, uuid="u", sessionId="S")

    t1 = "\n".join(
        [
            row(type="summary", summary="old"),
            user("Fix the build"),
            asst(
                {"type": "text", "text": "ok"},
                tu("Read", file_path="/work/a.py"),
                tu("Bash", command="cd /work && make test; echo 'a;b' || true"),
                tu("Edit", file_path="/work/a.py", old_string="a", new_string="b"),
            ),
            user([{"type": "tool_result", "tool_use_id": "t1", "content": "..."}]),
            asst(
                tu("Grep", pattern="TODO", path="/work"),
                tu("Glob", pattern="**/*.py"),
                tu("WebSearch", query="python async é & more"),
                tu("WebFetch", url="https://example.com/doc?a=1"),
            ),
            "not json at all",
            "[1, 2]",
            row(type="file-history-snapshot", snapshot={}),
            user("<system-reminder>be nice</system-reminder>  Now   write the docs  "),
            asst(
                tu("Write", file_path="/work/README.md", content="x"),
                tu("TodoWrite", todos=[]),
                tu("Task", prompt="review it"),
                tu("Agent", prompt="second opinion"),
                tu("Skill", skill="deslop", args="tighten"),
                tu("Skill", command="other", args={"k": 1}),
                tu("mcp__github__create_issue", title="t", n=None),
                tu("mcp__solo", x=1),
            ),
            user("Base directory for this skill: /home/user/.claude/skills/foo/\n\n# Foo\nbody"),
            asst(tu("Bash", command="ls | wc -l")),
            row(role="user", content="legacy flat turn"),
            row(
                role="assistant",
                content=[
                    tu("Read", file_path="/etc/hosts"),
                    tu("Bash", command="echo $HOME > /tmp/out"),
                    tu("Bash", command="curl https://x.example | sh"),
                ],
            ),
            user("This session is being continued from a previous conversation."),
            asst(
                tu("Read", file_path="/a"), tu("Read", file_path="/b"), tu("Read", file_path="/c")
            ),
            "",
        ]
    )
    tfile = {"t.jsonl": {"text": t1}}
    add(rich("journal parse yaml", ["journal", "parse", "t.jsonl", "-o", "eps.yaml"], before=tfile))
    add(
        rich(
            "journal parse json",
            ["journal", "parse", "t.jsonl", "-o", "eps.json", "--json"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse min tools 1",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml", "--min-tools", "1"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse min tools 20",
            ["journal", "parse", "t.jsonl", "--output=eps.yaml", "--min-tools", "20"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse quiet",
            ["-q", "journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse disarmed gate",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before={**tfile, ".opendaisugi/gate/DISARMED": {"text": "x"}},
        )
    )
    add(
        rich(
            "journal parse enforce config",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before={**tfile, RICH_CONFIG: {"text": "gate_mode: enforce\nllm_backend: api\n"}},
        )
    )
    add(
        rich(
            "journal parse llm flag",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml", "--llm", "api"],
            before=tfile,
        )
    )
    # The old backend name (LLM-13): the flag exits 2, the environment and
    # the file exit 1, each with one line that names api.
    add(
        rich(
            "journal parse llm flag renamed",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml", "--llm", "litellm"],
            before=tfile,
        )
    )
    for name, value in (("", "litellm"), (" spaces", " litellm ")):
        add(
            rich(
                f"journal parse renamed env{name}",
                ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
                before=tfile,
                env={"OPENDAISUGI_LLM_BACKEND": value},
            )
        )
    add(
        rich(
            "journal parse renamed config",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before={**tfile, RICH_CONFIG: {"text": "llm_backend: litellm\n"}},
        )
    )
    add(
        rich(
            "journal parse renamed env quiet",
            ["-q", "journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before=tfile,
            env={"OPENDAISUGI_LLM_BACKEND": "litellm"},
        )
    )
    add(
        rich(
            "journal parse bad llm",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml", "--llm", "gpt"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse bad format",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml", "--format", "hermes"],
            before=tfile,
        )
    )
    add(rich("journal parse missing file", ["journal", "parse", "nope.jsonl", "-o", "eps.yaml"]))
    add(
        rich(
            "journal parse directory",
            ["journal", "parse", "d", "-o", "eps.yaml"],
            before={"d": {"dir": True}},
        )
    )
    add(rich("journal parse no output", ["journal", "parse", "t.jsonl"], before=tfile))
    add(
        rich(
            "journal parse bad min tools",
            ["journal", "parse", "t.jsonl", "-o", "x", "--min-tools", "x"],
            before=tfile,
        )
    )
    add(
        rich(
            "journal parse empty",
            ["journal", "parse", "e.jsonl", "-o", "eps.yaml"],
            before={"e.jsonl": {"text": ""}},
        )
    )
    add(
        rich(
            "journal parse output dir missing",
            ["journal", "parse", "t.jsonl", "-o", "no/such/eps.yaml"],
            before=tfile,
        )
    )
    # Splitting a large episode: the fake claude answers the split prompt.
    big = "\n".join(
        [user("Refactor everything")]
        + [asst(tu("Read", file_path=f"/work/f{k}.py")) for k in range(6)]
        + [asst(tu("Bash", command=f"make t{k}")) for k in range(4)]
    )
    bfile = {"big.jsonl": {"text": big}}
    good = {
        "subtasks": [
            {"start_index": 0, "end_index": 5, "task": "read the files"},
            {"start_index": 6, "end_index": 9, "task": "run the tests"},
        ]
    }
    cc = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}
    add(
        rich(
            "journal parse split",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env=cc,
            replies=[{"claude_raw": "Here: " + json.dumps(good) + " done"}],
        )
    )
    unordered = {"subtasks": [good["subtasks"][1], good["subtasks"][0]]}
    add(
        rich(
            "journal parse split unordered",
            ["journal", "parse", "big.jsonl", "-o", "eps.json", "--json", "--max-tools", "5"],
            before=bfile,
            env=cc,
            replies=[{"claude_raw": json.dumps(unordered)}],
        )
    )
    add(
        rich(
            "journal parse split python literal",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env=cc,
            replies=[
                {
                    "claude_raw": "{'subtasks': [{'start_index': 0, 'end_index': 9, 'task': 'all', 'x': None}]}"
                }
            ],
        )
    )
    for name, reply in (
        (
            "gap",
            {
                "subtasks": [
                    {"start_index": 0, "end_index": 3, "task": "a"},
                    {"start_index": 5, "end_index": 9, "task": "b"},
                ]
            },
        ),
        ("float index", {"subtasks": [{"start_index": 0.0, "end_index": 9, "task": "a"}]}),
        ("empty", {"subtasks": []}),
        ("no subtasks", {"other": 1}),
        ("dict subtasks", {"subtasks": {"start_index": 0}}),
        ("missing task", {"subtasks": [{"start_index": 0, "end_index": 9}]}),
    ):
        add(
            rich(
                f"journal parse split {name}",
                ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
                before=bfile,
                env=cc,
                replies=[{"claude_raw": json.dumps(reply)}],
            )
        )
    add(
        rich(
            "journal parse split not json",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env=cc,
            replies=[{"claude_raw": "I cannot help with that."}],
        )
    )
    add(
        rich(
            "journal parse split claude fails",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env=cc,
            replies=[{"claude_raw": "", "stderr": "boom", "exit": 1}],
        )
    )
    add(
        rich(
            "journal parse split no claude",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env=cc,
            no_claude=True,
        )
    )
    add(
        rich(
            "journal parse split extra args",
            ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"],
            before=bfile,
            env={**cc, "DAISUGI_CLAUDE_ARGS": "--max-turns 1"},
            replies=[{"claude_raw": json.dumps(good)}],
        )
    )
    # The split over the api backend: our own model client, the request
    # bytes kept in the case, and every failure a parse error (C-16).
    key = {"ANTHROPIC_API_KEY": "sk-test-000000000000000000000000"}
    split_api = ["journal", "parse", "big.jsonl", "-o", "eps.yaml", "--max-tools", "5"]
    for name, argv, env, replies in (
        ("", [*split_api, "--llm", "api"], key, [{"http": json.dumps(good)}]),
        ("auto", split_api, key, [{"http": json.dumps(good)}]),
        ("env", split_api, {**key, "OPENDAISUGI_LLM_BACKEND": "api"}, [{"http": json.dumps(good)}]),
        ("json", [*split_api, "--llm", "api", "--json"], key, [{"http": json.dumps(unordered)}]),
        (
            "cut",
            [*split_api, "--llm", "api"],
            key,
            [{"http": json.dumps(good), "stop": "max_tokens"}],
        ),
        ("no key", [*split_api, "--llm", "api"], {}, []),
        ("http 500", [*split_api, "--llm", "api"], key, [{"http_status": 500, "body": "boom"}]),
        ("not json", [*split_api, "--llm", "api"], key, [{"http": "Here you go: {}"}]),
        ("empty", [*split_api, "--llm", "api"], key, [{"http": ""}]),
        ("list", [*split_api, "--llm", "api"], key, [{"http": "[1, 2]"}]),
        ("no subtasks", [*split_api, "--llm", "api"], key, [{"http": '{"other": 1}'}]),
        (
            "gap",
            [*split_api, "--llm", "api"],
            key,
            [
                {
                    "http": json.dumps(
                        {
                            "subtasks": [
                                {"start_index": 0, "end_index": 3, "task": "a"},
                                {"start_index": 5, "end_index": 9, "task": "b"},
                            ]
                        }
                    )
                }
            ],
        ),
        (
            "missing task",
            [*split_api, "--llm", "api"],
            key,
            [{"http": json.dumps({"subtasks": [{"start_index": 0, "end_index": 9}]})}],
        ),
        (
            "model",
            [*split_api, "--llm", "api", "--model", "anthropic/claude-haiku-4-5"],
            key,
            [{"http": json.dumps(good)}],
        ),
        ("no wire", [*split_api, "--llm", "api", "--model", "gpt-4o"], key, []),
        (
            "openai",
            [*split_api, "--llm", "api", "--model", "openai/gpt-4o"],
            {
                "OPENAI_API_KEY": "sk-openai-0000000000000000000000",
                "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1",
            },
            [{"chat": json.dumps(good)}],
        ),
        (
            "ollama",
            [*split_api, "--llm", "api", "--model", "ollama/qwen2.5:7b"],
            {"OLLAMA_API_BASE": "http://127.0.0.1:{PORT}"},
            [{"chat": json.dumps(good)}],
        ),
    ):
        add(
            rich(
                f"journal parse split api {name}".rstrip(),
                argv,
                before=bfile,
                env=env,
                replies=replies,
                **({} if replies else {"no_claude": True}),
            )
        )
    codex = "\n".join(
        [
            row(type="session_meta", payload={"id": "s"}),
            row(type="event_msg", payload={"type": "user_message", "message": "Run the tests"}),
            row(
                type="response_item",
                payload={
                    "type": "message",
                    "role": "user",
                    "content": [{"type": "input_text", "text": "Run the tests"}],
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "shell",
                    "arguments": json.dumps({"command": ["bash", "-lc", "pytest -q"]}),
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "exec_command",
                    "arguments": json.dumps({"command": ["git", "status", "--short"]}),
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "local_shell_call",
                    "action": {"command": ["ls", "-la", "my dir"]},
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "custom_tool_call",
                    "name": "apply_patch",
                    "input": "*** Begin Patch\n*** Update File: src/a.py\n@@\n*** Add File: b.md\n",
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "apply_patch",
                    "arguments": json.dumps({"input": "*** Delete File: old.txt\n"}),
                },
            ),
            row(
                type="response_item",
                payload={
                    "type": "function_call",
                    "name": "search",
                    "namespace": "docs",
                    "arguments": json.dumps({"q": "x"}),
                },
            ),
            row(
                type="response_item",
                payload={"type": "function_call", "name": "update_plan", "arguments": "{}"},
            ),
            row(
                type="response_item",
                payload={"type": "function_call", "name": "shell", "arguments": "not json"},
            ),
            row(
                type="response_item",
                payload={"type": "web_search_call", "action": {"query": "codex docs"}},
            ),
            row(type="response_item", payload={"type": "reasoning", "summary": []}),
            row(
                item={
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": "done"}],
                }
            ),
            row(type="event_msg", payload={"type": "user_message", "message": "Now lint"}),
            row(
                type="function_call",
                name="shell",
                arguments=json.dumps({"command": "ruff check ."}),
            ),
            row(
                type="function_call",
                name="shell",
                arguments=json.dumps({"command": ["ruff", "format"]}),
            ),
            row(
                type="function_call",
                name="shell",
                arguments=json.dumps({"command": ["sh", "-c", "a && b"]}),
            ),
        ]
    )
    add(
        rich(
            "journal parse codex",
            [
                "journal",
                "parse",
                "c.jsonl",
                "-o",
                "eps.yaml",
                "--format",
                "codex",
                "--min-tools",
                "1",
            ],
            before={"c.jsonl": {"text": codex}},
        )
    )

    import yaml as _yaml

    def episode(i: int, task: str, steps: list[dict[str, Any]], **kw: Any) -> dict[str, Any]:
        return {
            "id": f"ep_{i:02d}",
            "task": task,
            "steps": steps,
            "source_range": {"first_message": i, "last_message": i + 1},
            **kw,
        }

    def st(i: int, typ: str, **kw: Any) -> dict[str, Any]:
        return {"id": f"s{i}", "depends_on": [], "metadata": {}, "type": typ, **kw}

    good_eps = [
        episode(
            0,
            "build it",
            [
                st(0, "shell", command="make test"),
                st(1, "file_read", path="/work/a.py"),
                st(2, "file_write", path="/work/b.py", content=""),
                st(3, "network", url="https://example.com/x", method="GET", headers={}),
            ],
        ),
        episode(
            1,
            "look around",
            [
                st(4, "shell", command="ls -la"),
                st(5, "mcp", server="gh", tool="list", arguments={}),
            ],
        ),
    ]
    fail_eps = [
        episode(
            2,
            "pipe it",
            [
                st(6, "shell", command="ls | wc -l"),
                st(7, "shell", command="cd /w && make; echo 'x;y'"),
            ],
        ),
        episode(
            3,
            "redirect",
            [st(8, "shell", command="echo hi > /tmp/o.txt"), st(9, "task", prompt="think")],
        ),
        episode(
            4,
            "skill",
            [
                st(10, "skill", skill_id="deslop", skill_input={"args": "x"}),
                st(11, "shell", command="git status"),
            ],
        ),
    ]

    def eps_file(eps: list[dict[str, Any]], *, as_json: bool = False, **top: Any) -> dict[str, Any]:
        body = {
            "source": "claude-code",
            "source_file": "t.jsonl",
            "parsed_at": "2020-01-01T00:00:00Z",
            "episodes": eps,
            **top,
        }
        text = json.dumps(body, indent=2) if as_json else _yaml.safe_dump(body, sort_keys=False)
        return {"eps.json" if as_json else "eps.yaml": {"text": text}}

    add(rich("journal ingest ok", ["journal", "ingest", "eps.yaml"], before=eps_file(good_eps)))
    add(
        rich(
            "journal ingest ok json",
            ["journal", "ingest", "eps.yaml", "--json"],
            before=eps_file(good_eps),
        )
    )
    add(
        rich(
            "journal ingest json file",
            ["journal", "ingest", "eps.json"],
            before=eps_file(good_eps, as_json=True),
        )
    )
    add(
        rich(
            "journal ingest dry run",
            ["journal", "ingest", "eps.yaml", "--dry-run"],
            before=eps_file(good_eps + fail_eps),
        )
    )
    add(
        rich(
            "journal ingest dry run json",
            ["journal", "ingest", "eps.yaml", "--dry-run", "--json"],
            before=eps_file(good_eps + fail_eps),
        )
    )
    add(
        rich(
            "journal ingest failures",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file(good_eps + fail_eps),
        )
    )
    add(
        rich(
            "journal ingest decomposition",
            ["journal", "ingest", "eps.yaml", "--allow-shell-decomposition"],
            before=eps_file(fail_eps),
        )
    )
    add(
        rich(
            "journal ingest decomposition config",
            ["journal", "ingest", "eps.yaml"],
            before={
                **eps_file(fail_eps),
                RICH_CONFIG: {"text": "shell_allow_decomposition: true\n"},
            },
        )
    )
    add(
        rich(
            "journal ingest no decomposition flag",
            ["journal", "ingest", "eps.yaml", "--no-allow-shell-decomposition"],
            before={
                **eps_file(fail_eps[:1]),
                RICH_CONFIG: {"text": "shell_allow_decomposition: true\n"},
            },
        )
    )
    add(
        rich(
            "journal ingest again",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file(good_eps),
            twice=True,
        )
    )
    add(
        rich(
            "journal ingest again dry run",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file(good_eps),
            twice=True,
            argv2=["journal", "ingest", "eps.yaml", "--dry-run"],
        )
    )
    add(
        rich(
            "journal ingest bad trace id",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file([episode(0, "x", [st(0, "shell", command="ls")], id="ep/1")]),
        )
    )
    add(
        rich(
            "journal ingest data dir",
            ["journal", "ingest", "eps.yaml", "--data-dir", "{HOME}/dd"],
            before=eps_file(good_eps[:1]),
        )
    )
    add(
        rich(
            "journal ingest data dir is a file",
            ["journal", "ingest", "eps.yaml", "--data-dir", "{HOME}/f"],
            before={**eps_file(good_eps[:1]), "f": {"text": "x"}},
        )
    )
    add(
        rich(
            "journal ingest invalid",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file(
                [
                    {
                        "id": "ep_00",
                        "task": 5,
                        "steps": [{"id": "s0", "type": "shell"}, {"type": "nope", "id": "s1"}],
                    }
                ]
            ),
        )
    )
    add(
        rich(
            "journal ingest missing episodes",
            ["journal", "ingest", "eps.yaml"],
            before={"eps.yaml": {"text": "source: claude-code\nsource_file: t\nparsed_at: x\n"}},
        )
    )
    add(
        rich(
            "journal ingest top list",
            ["journal", "ingest", "eps.yaml"],
            before={"eps.yaml": {"text": "- 1\n"}},
        )
    )
    add(
        rich(
            "journal ingest empty file",
            ["journal", "ingest", "eps.yaml"],
            before={"eps.yaml": {"text": ""}},
        )
    )
    add(rich("journal ingest missing file", ["journal", "ingest", "nope.yaml"]))

    # `daisugi verify PLAN --envelope ENV`: the verifier on two files.
    def vdoc(name: str, obj: dict[str, Any]) -> dict[str, Any]:
        return {name: {"text": _yaml.safe_dump(obj, sort_keys=False)}}

    v_plan = {
        "id": "plan_00000001",
        "source": "script",
        "task": "list the files",
        "steps": [
            {"id": "s1", "type": "shell", "command": "ls -la", "depends_on": []},
            {"id": "s2", "type": "file_read", "path": "/work/a.txt", "depends_on": ["s1"]},
        ],
    }
    v_perm = {
        "file_read": ["/work/**"],
        "file_write": [],
        "network": False,
        "shell": True,
        "shell_allowlist": ["ls"],
    }
    v_env = {"id": "env_00000001", "generated_by": "test", "task": "t", "permissions": v_perm}
    v_tight = {**v_env, "permissions": {**v_perm, "shell_allowlist": ["cat"], "file_read": []}}
    exists = {"op": "exists", "path": "a"}
    v_taut = {
        **v_env,
        "invariants": [
            {
                "type": "always",
                "description": "d",
                "expr": {"op": "or", "children": [exists, {"op": "not", "child": exists}]},
            }
        ],
    }
    v_alias = {
        **v_env,
        "invariants": [
            {"type": "named", "description": "d", "expr": {"op": "alias", "name": "no_secrets"}}
        ],
    }
    both = {**vdoc("p.yaml", v_plan), **vdoc("e.yaml", v_env)}
    for name, before, extra in [
        ("ok", both, []),
        ("ok json", both, ["--json"]),
        ("violations", {**both, **vdoc("e.yaml", v_tight)}, []),
        ("violations json", {**both, **vdoc("e.yaml", v_tight)}, ["--json"]),
        ("tautology warning", {**both, **vdoc("e.yaml", v_taut)}, []),
        ("alias", {**both, **vdoc("e.yaml", v_alias)}, []),
        ("plan does not parse", {**both, "p.yaml": {"text": "id: x\nsteps: 3\n"}}, []),
        ("envelope does not parse", {**both, "e.yaml": {"text": "id: x\n"}}, []),
        ("plan missing", vdoc("e.yaml", v_env), []),
        ("plan is a directory", {**vdoc("e.yaml", v_env), "p.yaml": {"dir": True}}, []),
        ("envelope missing", vdoc("p.yaml", v_plan), []),
    ]:
        add(
            rich(
                f"verify {name}",
                ["verify", "p.yaml", "--envelope", "e.yaml", *extra],
                before=before,
            )
        )
    add(rich("verify no envelope option", ["verify", "p.yaml"], before=both))

    # `daisugi hook report`: one pane state event on stdin. The gate root's
    # parent is a file here, so the session tree is not written (it is
    # best effort); the gate_server suite cases the tree through gate.sock.
    blocked = {"blocked": {"text": "not a directory\n"}}
    root = ["--root", "{HOME}/blocked/gate"]
    ev = {
        "session_id": "s1",
        "harness": "claude",
        "state": "working",
        "source": "headless",
        "ts": 1.5,
    }
    for name, stdin, extra in [
        ("event", json.dumps(ev), []),
        ("event with pane", json.dumps(ev), ["--pane", "w1:p2"]),
        ("gate source", json.dumps({**ev, "source": "gate"}), []),
        ("done from operator", json.dumps({**ev, "state": "done", "source": "operator"}), []),
        ("done from manifest", json.dumps({**ev, "state": "done", "source": "manifest"}), []),
        ("missing field", json.dumps({k: v for k, v in ev.items() if k != "ts"}), []),
        ("ts a string", json.dumps({**ev, "ts": "1"}), []),
        ("ts a bool", json.dumps({**ev, "ts": True}), []),
        ("unknown state", json.dumps({**ev, "state": "napping"}), []),
        ("detail too long", json.dumps({**ev, "detail": "x" * 201}), []),
        ("pane not a string", json.dumps({**ev, "pane": 3}), []),
        ("not json", "{nope", []),
        ("not an object", "[1, 2]", []),
        ("empty", "", []),
    ]:
        add(
            rich(
                f"hook report {name}",
                ["hook", "report", *root, *extra],
                before=blocked,
                stdin=stdin,
            )
        )
    add(rich("hook report bad option", ["hook", "report", "--nope"], before=blocked, stdin="{}"))
    add(rich("verify no arguments", ["verify"], before=both))
    add(
        rich(
            "journal ingest conformance record",
            ["journal", "ingest", "eps.yaml"],
            before=eps_file(good_eps[:1]),
            env={"OPENDAISUGI_CONFORMANCE_RECORD": "{HOME}/../corpus"},
            go_refuses=True,
        )
    )
    add(
        rich(
            "journal ingest bad trace body",
            ["journal", "ingest", "eps.yaml"],
            before={
                **eps_file(good_eps[:1]),
                ".opendaisugi/journal/traces/import-"
                + hashlib.sha256(b"t.jsonl").hexdigest()[:8]
                + "-ep_00.yaml": {"text": "id: x\n"},
            },
        )
    )

    # -- start ---------------------------------------------------------------
    so = {"oracle": "start-shim", "env": {"FAKE_SPAWN_LOG": "{HOME}/../spawn.log"}, "cwd": "proj"}
    add(rich("start dry run", ["start", "--dry-run"], **so))
    add(rich("start dry run enforce", ["start", "--dry-run", "--enforce"], **so))
    add(rich("start no ui", ["start", "--no-ui"], **so))
    add(rich("start no ui enforce", ["start", "--no-ui", "--enforce"], **so))
    add(rich("start no ui enforce ask", ["start", "--no-ui", "--enforce", "--ask"], **so))
    add(rich("start ask without enforce", ["start", "--no-ui", "--ask"], **so))
    add(rich("start no claude", ["start", "--no-ui"], no_claude=True, **so))
    add(rich("start no claude dry run", ["start", "--dry-run"], no_claude=True, **so))
    add(rich("start data dir", ["start", "--no-ui", "--data-dir", "{HOME}/dd"], **so))
    add(rich("start relative data dir", ["start", "--no-ui", "--data-dir", "../dd"], **so))
    pdir = "proj/.claude/settings.json"
    add(rich("start again", ["start", "--no-ui"], twice=True, **so))
    add(rich("start again enforce", ["start", "--no-ui", "--enforce"], twice=True, **so))
    add(rich("start hook already", ["start", "--no-ui"], before={pdir: audit}, **so))
    add(
        rich("start hook other mode", ["start", "--no-ui", "--enforce"], before={pdir: audit}, **so)
    )
    add(rich("start global hook note", ["start", "--no-ui"], before={CLAUDE: enforce}, **so))
    add(
        rich("start global hook note dry run", ["start", "--dry-run"], before={CLAUDE: audit}, **so)
    )
    add(
        rich(
            "start settings kept",
            ["start", "--no-ui"],
            before={
                pdir: {
                    "text": json.dumps(
                        {
                            "model": "x",
                            "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "a"}]}]},
                        },
                        indent=4,
                    )
                }
            },
            **so,
        )
    )
    add(rich("start settings invalid", ["start", "--no-ui"], before={pdir: {"text": "{x"}}, **so))
    add(
        rich(
            "start envelope already",
            ["start", "--no-ui"],
            before={".opendaisugi/gate/envelopes/proj-{CWD8}.json": {"text": "{}"}},
            **so,
        )
    )
    # G2-9: a gate is running when its socket answers, not when a file is
    # there. The runner holds a listening socket at `live_gate` while the
    # command runs. Its directory is in `before`, so a binary that refuses
    # `start` is seen to leave the tree as it was.
    live = {"before": {".opendaisugi/gate": {"dir": True}}, "live_gate": GATE_SOCK}
    add(rich("start server running", ["start", "--no-ui"], **live, **so))
    add(rich("start server running dry run", ["start", "--dry-run"], **live, **so))
    add(rich("start stale socket", ["start", "--no-ui"], before={GATE_SOCK: {"text": ""}}, **so))
    add(
        rich(
            "start stale socket dry run",
            ["start", "--dry-run"],
            before={GATE_SOCK: {"text": ""}},
            **so,
        )
    )
    add(rich("start odd dir name", ["start", "--no-ui"], **{**so, "cwd": "my proj.é"}))
    # The view: the plain live view draws one frame, since stdout is not a
    # terminal. PATH holds only the case's bin directory, so the frame
    # does not depend on what /usr/bin holds; lexical is the matcher both
    # the oracle and the binaries run (K4-2).
    view = {
        **so,
        "env": {**so["env"], "PATH": "{HOME}/../bin"},
        "before": {RICH_CONFIG: {"text": "matcher_model: lexical\n"}},
        "view": True,
    }
    add(rich("start view", ["start"], **view))
    add(rich("start view enforce", ["start", "--enforce"], **view))
    add(rich("start view quiet", ["-q", "start"], **view))
    add(rich("start extra arg", ["start", "x"], **so))
    return C


def run_twice_aware(
    case: dict[str, Any], cmd: list[str], work: Path, extra_env: dict[str, str] | None = None
) -> tuple[dict[str, Any], float]:
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
        proc = subprocess.run(
            cmd + argv,
            input=case.get("stdin", "").encode(),
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
    return {
        "exit": proc.returncode,
        "stdout": norm_stdout(proc.stdout.decode("utf-8", "replace"), h, case["argv"]),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace"), h),
        "tree": read_tree(home),
    }, elapsed


def record_rich_replies(case: dict[str, Any], work: Path) -> None:
    """garden_cases.record_replies through the rich runner: answer the
    case's model requests in order with its reply specs."""
    import garden_cases as g

    table: dict[str, Any] = {}
    replies = list(case["replies"])
    while True:
        case["model"] = table
        raw: list[dict[str, Any]] = []
        run_rich(case, python_cmd(case), work, raw)
        missing = [r for r in raw if r["key"] not in table]
        if not missing or not replies:
            break
        kind, ans = g.make_answer(replies.pop(0))
        if kind != missing[0]["kind"]:
            raise SystemExit(f"{case['name']}: a {kind} reply for a {missing[0]['kind']} request")
        table[missing[0]["key"]] = ans
    if replies:
        print(f"WARNING {case['name']}: {len(replies)} reply spec(s) never asked for", flush=True)
    case["model"] = table


def run_oracle(case: dict[str, Any], work: Path) -> dict[str, Any]:
    if case.get("runner") == "rich":
        return run_rich(case, python_cmd(case), work)
    obs, _ = run_twice_aware(case, python_cmd(case), work)
    return obs


def go_expect_for(case: dict[str, Any]) -> dict[str, Any] | None:
    if case.get("runner") != "rich":
        return None
    if case["argv"][:1] == ["status"]:
        return _status_go_expect(case)
    if case.get("oracle") == "start-shim":
        return _start_go_expect(case)
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name contains this text; print, write nothing"
    )
    ap.add_argument(
        "--keep",
        action="store_true",
        help="reuse the fixture's answer for a case whose body is unchanged; run only new cases",
    )
    ap.add_argument(
        "--rerun", help="with --keep, run again the cases whose name contains this text"
    )
    ap.add_argument(
        "--resume",
        action="store_true",
        help="reuse the answers a run cut short saved; run only the rest",
    )
    args = ap.parse_args()
    cases = build_cases() + build_rich_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    SCRATCH.mkdir(parents=True, exist_ok=True)
    old: dict[str, dict[str, Any]] = {}
    if args.keep and (args.out / "cases.jsonl").exists():
        for ln in (args.out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                body = {
                    k: v for k, v in c.items() if k not in ("expect", "go_expect", "id", "model")
                }
                old[case_id(body)] = c
    # Each answer is saved as it is made, so a run cut short can go on
    # with --resume. A finished run deletes the file.
    cache_path = SCRATCH / "gen-cache.jsonl"
    if args.resume and cache_path.exists():
        for ln in cache_path.read_text(encoding="utf-8").splitlines():
            if ln.strip():
                rec = json.loads(ln)
                old.setdefault(rec["key"], rec["case"])
    elif cache_path.exists() and not args.only:
        cache_path.unlink()
    out_lines = []
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        work = SCRATCH / "gen" / f"{i:04d}"
        prev = old.get(case_id({k: v for k, v in c.items() if k != "replies"}))
        if args.rerun and args.rerun in c["name"]:
            prev = None
        if prev is not None:
            obs = prev["expect"]
            if "model" in prev:
                c["model"] = prev["model"]
        else:
            if c.get("replies"):
                record_rich_replies(c, work)
            obs = run_oracle(c, work)
            if not args.only:
                saved = {"expect": obs, **({"model": c["model"]} if "model" in c else {})}
                key = case_id({k: v for k, v in c.items() if k not in ("replies", "model")})
                with cache_path.open("a", encoding="utf-8") as fh:
                    fh.write(json.dumps({"key": key, "case": saved}) + "\n")
        if args.only:
            print(json.dumps({"name": c["name"], **obs}, indent=1, ensure_ascii=False))
            c["expect"] = obs
            ge = go_expect_for(c)
            if ge:
                print(json.dumps({"go_expect": ge}, indent=1, ensure_ascii=False))
            continue
        body = dict(c)
        body.pop("replies", None)
        body["expect"] = obs
        ge = go_expect_for(body)
        if ge:
            body["go_expect"] = ge
        body["id"] = case_id({k: v for k, v in body.items() if k != "id"})
        out_lines.append(body)
        print(f"{i + 1:4d}/{len(cases)} exit={obs['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
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
    print(f"wrote {len(out_lines)} cases to {path}")
    cache_path.unlink(missing_ok=True)
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
    out.write_text(
        json.dumps({"config": fields(Config), "floor": fields(FloorConfig)}, indent=1) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    raise SystemExit(main())
