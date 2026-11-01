"""Synthetic CLI cases: the warden commands and `install --warden`, run through
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

``install`` writes every layer in both: the ports carry the skill, MCP,
capture and instruction layers (IN-1). The oracle's skill is a symlink to
its package directory; a port copies the files, and the compare reads the
link as that copy.

A case marked ``go_refuses`` holds a flag the Go binary does not handle.
There the check is only that Go exits non-zero, prints one line naming the
Python command, and leaves the tree as it was.

The file is content-addressed like the warden cases: each ``id`` is the first
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
import garden_cases  # noqa: E402 - sibling module, run as a script
from fixture_paths import fixed_root  # noqa: E402 - sibling module, run as a script

# Cases that stamp "about now" (the journal ones): up to 90 s after the case
# starts reads as +0, so a loaded box cannot move a minute bucket.
garden_cases.NOW_SNAP_S = 90.0

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "cli"
SCRATCH = Path(
    os.environ.get("DAISUGI_CLI_SCRATCH") or Path.home() / "opendaisugi-scratch" / "2c" / "runs"
)
CASE_VERSION = 1

# Cases whose answer depends on the box that writes them (whether the
# search extra is installed). The writer keeps the fixture's answer for each
# and the manifest names them, so a regeneration cannot flip one silently.
# See clients/ADJUDICATIONS.md, CL-R-1.
BOX_DEPENDENT = (
    "status pathways minilm",
    "journal search minilm",
    "status gpu fails",
    "status gpu no comma",
    "status gpu bad number",
    "journal search int8",
)


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


WARDEN_SOCK = ".opendaisugi/warden/warden.sock"

# `daisugi start` detaches `python -m opendaisugi.cli warden serve` and then
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
    Path(argv[argv.index("--root") + 1], "warden.sock").touch()
st._detach = _fake_detach
"""
)


def python_cmd(case: dict[str, Any]) -> list[str]:
    oracle = case.get("oracle", "cli")
    if oracle == "start-shim":
        return [sys.executable, "-c", _START_SHIM]
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
    """A warden hook command with its entry point replaced by {WARDEN}, so the
    Python form (python -m opendaisugi.warden_client ...) and the Go form
    (NAME=... daisugi warden check ...) compare on the flags they pass."""
    if "opendaisugi.warden" not in cmd and " warden check" not in cmd:
        return cmd
    try:
        toks = shlex.split(cmd)
    except ValueError:
        return cmd
    while toks and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", toks[0]):
        toks = toks[1:]
    if len(toks) >= 4 and toks[1:3] == ["-I", "-m"] and toks[3].startswith("opendaisugi.warden"):
        toks = ["{WARDEN}"] + toks[4:]  # the hook's isolated form (SX-R-9)
    elif len(toks) >= 3 and toks[1] == "-m" and toks[2].startswith("opendaisugi.warden"):
        toks = ["{WARDEN}"] + toks[3:]
    elif len(toks) >= 3 and toks[1:3] == ["warden", "check"]:
        toks = ["{WARDEN}"] + toks[3:]
    return " ".join(toks)


_STAMP = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ")


def norm_json(v: Any, home: str) -> Any:
    if isinstance(v, str):
        for path, mark in exe_marks(home):
            v = v.replace(path, mark)
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
            if k == "applied_at" and isinstance(x, str) and _STAMP.fullmatch(x):
                x = "<time>"  # a recipe receipt's time: its shape is checked, not its value
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


def entry_dir() -> Path:
    """Where `daisugi` is for a case: one directory of a fixed length, on the
    PATH of every run. The oracle finds a fake executable there. A port is run
    by the link there, so it writes that path. The length is fixed because
    YAML folds a long line at a word, so a hook line in a Hermes config reads
    differently for a longer path (ruling INST-1)."""
    return fixed_root(SCRATCH, "x")


def exe_marks(home: str) -> list[tuple[str, str]]:
    """The paths `daisugi install` writes for itself, read as {DAISUGI}: the
    entry point of every run, and the renamed copy two cases install from."""
    return [
        (str(entry_dir() / "daisugi"), "{DAISUGI}"),
        (str(SCRATCH / "prog" / "daisugi-copy"), "{DAISUGI}"),
    ]


def entry_cmd(case: dict[str, Any], cmd: list[str]) -> list[str]:
    """The command to run: a port is run by the link in entry_dir, so the
    path it writes for itself is the oracle's. A case that sets `prog` runs
    the binary under that name instead."""
    if cmd and cmd[0] != sys.executable and not case.get("prog"):
        return [str(entry_dir() / "daisugi")] + cmd[1:]
    return cmd


def norm_text(s: str, home: str) -> str:
    for path, mark in exe_marks(home):
        s = s.replace(path, mark)
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


def _zero_elapsed(v: Any) -> Any:
    if isinstance(v, list):
        return [_zero_elapsed(x) for x in v]
    if isinstance(v, dict):
        return {k: (0 if k == "elapsed_ms" else _zero_elapsed(x)) for k, x in v.items()}
    return v


def norm_stdout(s: str, home: str, argv: list[str]) -> Any:
    s = norm_text(s, home)
    # warden replay --json carries each call's time; compare it as 0.
    if argv[:2] == ["warden", "replay"] and "--json" in argv:
        try:
            return {"json": _zero_elapsed(norm_json(json.loads(s), home))}
        except ValueError:
            return s
    # recipe apply --yes --json prints the receipt, which carries its time.
    if argv[:2] == ["recipe", "apply"] and "--json" in argv and "--yes" in argv:
        try:
            return {"json": norm_json(json.loads(s), home)}
        except ValueError:
            return s
    # warden settings prints one JSON document; compare it parsed.
    if argv[:2] == ["warden", "settings"]:
        try:
            return {"json": norm_json(json.loads(s), home)}
        except ValueError:
            return s
    return s


_WARN = re.compile(r"^.*?:\d+: (\w*Warning): (.*)$")
_WARN_BARE = re.compile(r"^\w*Warning: (.*)$")


def norm_stderr(s: str, home: str) -> list[str]:
    """Python's warnings print as `file:line: UserWarning: text` plus the
    source line; keep only `warning: text`, the binary's line, for every
    warning class, so one fixture reads the same on any box. A traceback keeps only its
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
            out.append(f"warning: {m.group(2)}")
            skip = True
            continue
        # Python cuts the file and the source line when a warning is
        # already compiled in; the text is the same either way.
        m = _WARN_BARE.match(line)
        if m:
            out.append(f"warning: {m.group(1)}")
            continue
        out.append(line)
    return out


# ---------------------------------------------------------------------------
# Running one case
# ---------------------------------------------------------------------------


def prepare(
    case: dict[str, Any], work: Path, cmd: list[str] | None = None
) -> tuple[Path, list[str], dict[str, str], Path]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    lay_out(case.get("before") or {}, home)
    bindir = work / "bin"
    bindir.mkdir(parents=True)
    # The installed entry point: install writes its absolute path into every
    # hook and MCP entry, and checks that it can run. The oracle finds a fake
    # one; a port is run through a link to the binary at the same path.
    exe_dir = entry_dir()
    if exe_dir.exists():
        shutil.rmtree(exe_dir)
    exe_dir.mkdir(parents=True)
    entry = exe_dir / "daisugi"
    if cmd and cmd[0] != sys.executable and not case.get("prog"):
        entry.symlink_to(Path(cmd[0]).resolve())
    else:
        entry.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(entry, 0o755)
    for name in case.get("path_bins") or []:
        b = bindir / name
        b.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(b, 0o755)
    h = str(home)
    env = {
        "HOME": h,
        "PATH": f"{bindir}:{exe_dir}:/usr/bin:/bin",
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
    home, argv, env, cwd = prepare(case, work, cmd)
    cmd = entry_cmd(case, cmd)
    if extra_env:
        env.update(extra_env)
    old = os.umask(0o022)
    try:
        # Earlier runs of the same CLI, as a user made them before (pre):
        # their output is not recorded, only the tree they leave.
        for pre in case.get("pre") or []:
            subprocess.run(  # noqa: S603 - the CLI under test
                cmd + [_sub(a, str(home)) for a in pre],
                input=b"",
                capture_output=True,
                env=env,
                cwd=cwd,
                timeout=120,
                check=False,
            )
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


GROOT = ".opendaisugi/warden"
ENVD = f"{GROOT}/envelopes"
CLAUDE = ".claude/settings.json"


def hook_settings(cmd: str, extra: dict[str, Any] | None = None) -> str:
    s: dict[str, Any] = {
        "hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": cmd}]}]}
    }
    if extra:
        s.update(extra)
    return json.dumps(s, indent=2)


PY_ENFORCE = "/usr/bin/python3 -m opendaisugi.warden_client --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
PY_AUDIT = "/usr/bin/python3 -m opendaisugi.warden_client --mode audit --root /x --format claude --verify-timeout 10.0"


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    # -- arm / disarm -------------------------------------------------------
    add(mk("disarm fresh", ["warden", "disarm"]))
    add(
        mk(
            "disarm loose root",
            ["warden", "disarm"],
            before={GROOT: d(0o755), ".opendaisugi": d(0o755)},
        )
    )
    add(
        mk(
            "disarm twice",
            ["warden", "disarm"],
            before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("old\n")},
        )
    )
    add(mk("disarm custom root", ["warden", "disarm", "--root", "{HOME}/r/g"]))
    add(mk("disarm root equals", ["warden", "disarm", "--root={HOME}/r2"]))
    add(
        mk(
            "arm disarmed",
            ["warden", "arm"],
            before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x\n")},
        )
    )
    add(mk("arm not disarmed", ["warden", "arm"]))
    add(
        mk(
            "arm custom root",
            ["warden", "arm", "--root", "{HOME}/g"],
            before={"g/DISARMED": f("x")},
        )
    )
    add(mk("arm extra arg", ["warden", "arm", "extra"]))
    # GR r1: the marker goes through the root's directory fd. A root that is
    # a symlink is refused, nothing behind it is removed, and the command
    # says so (it never reports an armed warden it did not arm).
    add(
        mk(
            "arm root symlink",
            ["warden", "arm"],
            before={
                ".opendaisugi": d(0o755),
                "real": d(0o700),
                "real/DISARMED": f("x\n", 0o600),
                GROOT: {"link": "{HOME}/real"},
            },
        )
    )
    # An operator command repairs a root that group can write, and says so.
    add(mk("disarm group writable root", ["warden", "disarm"], before={GROOT: d(0o775)}))

    # -- status -------------------------------------------------------------
    envs = {
        ENVD: d(0o700),
        f"{ENVD}/default.json": f(envelope_json(), 0o600),
        f"{ENVD}/s-1.json": f(envelope_json(), 0o600),
    }
    add(mk("status empty", ["warden", "status"]))
    add(mk("status empty json", ["warden", "status", "--json"]))
    add(mk("status envelopes", ["warden", "status"], before=envs))
    add(mk("status envelopes json", ["warden", "status", "--json"], before=envs))
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
    add(mk("status odd names", ["warden", "status", "--json"], before=odd))
    add(
        mk(
            "status disarmed",
            ["warden", "status"],
            before={GROOT: d(0o700), f"{GROOT}/DISARMED": f("x")},
        )
    )
    # The fence notice (plan task 4): the warden is on and coppice.toml
    # turns the fence off.
    coppice_off = {".config/coppice/coppice.toml": f('fence = "off"\n')}
    coppice_auto = {".config/coppice/coppice.toml": f('fence = "auto"\n')}
    add(mk("status fence off", ["warden", "status"], before={**envs, **coppice_off}))
    add(mk("status fence off json", ["warden", "status", "--json"], before={**envs, **coppice_off}))
    add(mk("status fence auto", ["warden", "status"], before={**envs, **coppice_auto}))
    add(mk("status fence off no envelopes", ["warden", "status"], before=coppice_off))
    add(
        mk(
            "status fence off disarmed",
            ["warden", "status"],
            before={**envs, **coppice_off, f"{GROOT}/DISARMED": f("x")},
        )
    )
    # A hook whose program is gone fails on every call; status warns.
    go_hook = (
        "DAISUGI_WARDEN_HOOK=opendaisugi.warden {HOME}/%s warden check --mode %s --root /x "
        "--format claude --verify-timeout 10.0"
    )
    add(
        mk(
            "status hook program gone enforce",
            ["warden", "status"],
            before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "enforce") + " || exit 2"))},
        )
    )
    add(
        mk(
            "status hook program gone audit json",
            ["warden", "status", "--json"],
            before={CLAUDE: f(hook_settings(go_hook % ("gone/daisugi", "audit")))},
        )
    )
    add(
        mk(
            "status hook program present",
            ["warden", "status"],
            before={
                "bin/daisugi": f("#!/bin/sh\n", 0o755),
                CLAUDE: f(hook_settings(go_hook % ("bin/daisugi", "enforce") + " || exit 2")),
            },
        )
    )
    add(
        mk(
            "status custom root",
            ["warden", "status", "--root", "{HOME}/g"],
            before={
                "g/envelopes/default.json": f("{}"),
                "config.yaml": f("warden_mode: enforce\n"),
            },
        )
    )
    configs = {
        "cfg enforce": "warden_mode: enforce\n",
        "cfg audit": "warden_mode: audit\n",
        # The old name of audit mode: an unknown value, so audit.
        "cfg old shadow": "warden_mode: shadow\n",
        "cfg quoted": "warden_mode: 'enforce'\n",
        "cfg dquoted": 'warden_mode: "enforce"  # on\n',
        "cfg comment": "# my config\n\nwarden_mode: enforce # yes\nmodel: x\n",
        "cfg bogus mode": "warden_mode: strict\n",
        "cfg bool mode": "warden_mode: yes\n",
        "cfg int mode": "warden_mode: 5\n",
        "cfg empty": "",
        "cfg list": "- a\n- b\n",
        "cfg scalar": "hello\n",
        "cfg bad yaml": "warden_mode: [enforce\n",
        "cfg bad other field": "warden_mode: enforce\nz3_timeout_ms: lots\n",
        "cfg int other field": "warden_mode: enforce\nz3_timeout_ms: 800\nmax_task_chars: '12'\n",
        "cfg bool other field": "warden_mode: enforce\nauto_tend: maybe\n",
        "cfg yes bool": "warden_mode: enforce\nauto_tend: yes\nshell_allow_decomposition: off\n",
        "cfg null opt": "warden_mode: enforce\nllm_backend: null\nrouter_local_model: ~\n",
        "cfg unknown keys": "warden_mode: enforce\nnot_a_key: [1, 2]\nfoo:\n  bar: 1\n",
        "cfg floor": "warden_mode: enforce\nfloor:\n  backend: coppice\n  notify_cmd: null\n",
        "cfg floor bad": "warden_mode: enforce\nfloor:\n  backend: [x]\n",
        "cfg floor null": "warden_mode: enforce\nfloor: null\n",
        "cfg dup key": "warden_mode: enforce\nwarden_mode: audit\n",
        "cfg anchor": "warden_mode: &m enforce\nmodel: *m\n",
        "cfg tab": "warden_mode:\tenforce\n",
        "cfg str for int": "warden_mode: enforce\nz3_timeout_ms: 1.5\n",
        # YAML's .nan and .inf are floats: invalid in an int or bool field,
        # and a top-level one is truthy, so not an empty config.
        "cfg inf for int": "warden_mode: enforce\nz3_timeout_ms: .inf\n",
        "cfg nan for bool": "warden_mode: enforce\nauto_tend: .nan\n",
        "cfg top nan": ".nan\n",
        "cfg top minus inf": "-.inf\n",
        "cfg data dir": "warden_mode: enforce\ndata_dir: /somewhere/else\n",
        "cfg saved": "auto_tend: false\ndata_dir: /home/user/.opendaisugi\nfloor:\n  backend: auto\n  coppice_socket: null\n  notify_cmd: null\n  tmux_socket: null\nfloor_report: null\nwarden_ask: false\nwarden_mode: enforce\nllm_context_window: null\nmax_task_chars: 4000\nmodel: anthropic/claude-sonnet-4-20250514\nvoice_server_url: http://127.0.0.1:7477\nz3_timeout_ms: 500\n",
    }
    for name, text in configs.items():
        add(
            mk(
                f"status {name}",
                ["warden", "status", "--json"],
                before={".opendaisugi/config.yaml": f(text)},
            )
        )
    hooks = {
        "hook global enforce": {CLAUDE: f(hook_settings(PY_ENFORCE))},
        "hook global audit": {
            CLAUDE: f(hook_settings(PY_AUDIT)),
            ".opendaisugi/config.yaml": f("warden_mode: enforce\n"),
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
            ".opendaisugi/config.yaml": f("warden_mode: enforce\n"),
        },
        "hook no mode": {CLAUDE: f(hook_settings("python3 -m opendaisugi.warden --root /x"))},
        "hook capture only": {
            CLAUDE: f(hook_settings("daisugi hook record --format claude --mode enforce"))
        },
        "hook mode suffix": {
            CLAUDE: f(hook_settings("python3 -m opendaisugi.warden --mode enforced"))
        },
        "hook go form": {
            CLAUDE: f(
                hook_settings(
                    "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
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
                                        {"command": "python3 -m opendaisugi.warden --mode audit"}
                                    ]
                                },
                                {
                                    "hooks": [
                                        {"command": "python3 -m opendaisugi.warden --mode enforce"}
                                    ]
                                },
                            ]
                        }
                    }
                )
            )
        },
        "hook bom": {CLAUDE: f("﻿" + hook_settings(PY_ENFORCE))},
        # A hook installed before the rename: the warden refuses --mode
        # shadow, so the hook denies every call. Status says so.
        "hook old shadow": {CLAUDE: f(hook_settings(PY_AUDIT.replace("audit", "shadow")))},
        "hook old shadow project": {
            "proj/.claude/settings.json": f(
                hook_settings(
                    "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check --mode=shadow --root /x"
                )
            )
        },
        "hook old shadow beside enforce": {
            CLAUDE: f(hook_settings(PY_AUDIT.replace("audit", "shadow"))),
            "proj/.claude/settings.json": f(hook_settings(PY_ENFORCE)),
        },
    }
    for name, tree in hooks.items():
        add(mk(f"status {name}", ["warden", "status", "--json"], before=tree, cwd="proj"))
        add(mk(f"status {name} text", ["warden", "status"], before=tree, cwd="proj"))
    # State from before the rename: status names the old data dir and each
    # old config key, in text on stdout and in JSON on stderr.
    stale = {
        "stale dir": {".opendaisugi/gate/envelopes/default.json": f("{}")},  # names-ok: old dir
        "stale dir and new dir": {
            ".opendaisugi/gate/x": f("1"),  # names-ok: old dir
            f"{ENVD}/default.json": f("{}"),
        },
        "stale key": {".opendaisugi/config.yaml": f("gate_mode: enforce\n")},  # names-ok: old key
        "stale keys": {
            ".opendaisugi/config.yaml": f(
                "gateway_router: knn\ngate_ask: true\nwarden_mode: audit\n"  # names-ok: old keys
            )  # names-ok: old keys
        },
    }
    for name, tree in stale.items():
        add(mk(f"status {name}", ["warden", "status", "--json"], before=tree))
        add(mk(f"status {name} text", ["warden", "status"], before=tree))

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
    add(mk("report none", ["warden", "report"]))
    add(mk("report none json", ["warden", "report", "--json"]))
    # The dialect audit: a word's would-deny rides in word_audit. The
    # report counts and lists them; warden status shows the count, the
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
    add(mk("report words", ["warden", "report"], before=words))
    add(mk("report words json", ["warden", "report", "--json"], before=words))
    add(mk("status words", ["warden", "status"], before=words))
    add(mk("status words json", ["warden", "status", "--json"], before=words))
    from opendaisugi.dialect import DIALECT_HASH

    for pname, cfg in (
        ("enforce", f"dialect_enforce: {DIALECT_HASH}\n"),
        ("mismatch", 'dialect_enforce: "0000000000000000"\n'),
        ("empty", 'dialect_enforce: ""\n'),
        ("int", "dialect_enforce: 12\n"),
    ):
        pinned = {**words, ".opendaisugi/config.yaml": f(cfg)}
        add(mk(f"status dialect pin {pname}", ["warden", "status"], before=pinned))
        add(mk(f"status dialect pin {pname} json", ["warden", "status", "--json"], before=pinned))
    enforcing = {**words, CLAUDE: f(hook_settings(PY_ENFORCE))}
    add(mk("status dialect hook enforce", ["warden", "status"], before=enforcing))
    add(mk("status dialect hook enforce json", ["warden", "status", "--json"], before=enforcing))
    for pname, cfg in (
        ("pin", f"dialect_enforce: {DIALECT_HASH}\n"),
        ("mismatch", 'dialect_enforce: "0000000000000000"\n'),
    ):
        both = {**enforcing, ".opendaisugi/config.yaml": f(cfg)}
        add(mk(f"status dialect hook enforce {pname}", ["warden", "status"], before=both))
        add(
            mk(
                f"status dialect hook enforce {pname} json",
                ["warden", "status", "--json"],
                before=both,
            )
        )
    bad_logs = {
        **words,
        f"{GROOT}/audit/x.jsonl": f('not json\n[1]\n{"would_deny": true, "reason": 5}\n'),
        f"{GROOT}/shadow/y.jsonl": f(
            rec("Write", True, None, "/w", word_audit=["dialect audit: old"]) + "\n"
        ),
    }
    add(mk("status words bad logs", ["warden", "status"], before=bad_logs))
    add(mk("report all", ["warden", "report"], before=audit))
    add(mk("report all json", ["warden", "report", "--json"], before=audit))
    add(mk("report session", ["warden", "report", "--session", "s2"], before=audit))
    add(mk("report missing session", ["warden", "report", "--session", "nope"], before=audit))
    add(mk("report unsafe session", ["warden", "report", "--session", "../s1"], before=audit))
    # The log written while audit mode was called shadow mode is read
    # first, then the audit log.
    legacy = {
        f"{GROOT}/shadow/s1.jsonl": f(log2, 0o600),
        f"{GROOT}/shadow/s3.jsonl": f(rec("Read", False, None, "/a") + "\n", 0o600),
        **audit,
    }
    add(mk("report legacy dir", ["warden", "report"], before=legacy))
    add(mk("report legacy dir json", ["warden", "report", "--json"], before=legacy))
    add(
        mk(
            "report legacy dir session",
            ["warden", "report", "--session", "s1", "--json"],
            before=legacy,
        )
    )
    add(
        mk(
            "report legacy dir only",
            ["warden", "report", "--json"],
            before={f"{GROOT}/shadow/s1.jsonl": f(log1, 0o600)},
        )
    )
    add(
        mk(
            "report reason null",
            ["warden", "report"],
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
            ["warden", "report"],
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
            ["warden", "report", "--json"],
            before={
                f"{GROOT}/audit/a.jsonl": f(
                    json.dumps({"would_deny": True, "reason": "metacharacters here"}) + "\n"
                )
            },
        )
    )

    # -- settings -----------------------------------------------------------
    add(mk("settings default", ["warden", "settings"]))
    add(mk("settings enforce", ["warden", "settings", "--enforce"]))
    add(mk("settings session", ["warden", "settings", "--enforce", "--session", "s 1"]))
    add(mk("settings root", ["warden", "settings", "--root", "{HOME}/my root"]))
    add(mk("settings format pi", ["warden", "settings", "--enforce", "--format", "pi"]))

    # -- init ---------------------------------------------------------------
    add(mk("init fresh", ["warden", "init"], cwd="work"))
    add(mk("init workspace", ["warden", "init", "--workspace", "{HOME}/w"]))
    add(mk("init session", ["warden", "init", "--session", "s1", "--workspace", "/work"]))
    add(mk("init unsafe session", ["warden", "init", "--session", "a/b", "--workspace", "/work"]))
    add(mk("init quoted session", ["warden", "init", "--session", "a b'c", "--workspace", "/work"]))
    add(
        mk(
            "init exists",
            ["warden", "init", "--workspace", "/work"],
            before={f"{ENVD}/default.json": f("{}", 0o600)},
        )
    )
    add(
        mk(
            "init force",
            ["warden", "init", "--workspace", "/work", "--force"],
            before={f"{ENVD}/default.json": f("{}", 0o644), ENVD: d(0o755)},
        )
    )
    add(
        mk(
            "init decomposition",
            ["warden", "init", "--workspace", "/work", "--allow-shell-decomposition"],
        )
    )
    add(mk("init root", ["warden", "init", "--workspace", "/work", "--root", "{HOME}/g"]))

    # -- register -----------------------------------------------------------
    add(
        mk(
            "register default",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(id="env_fixed01"))},
        )
    )
    add(
        mk(
            "register session",
            ["warden", "register", "{HOME}/e.json", "--session", "s1"],
            before={"e.json": f(envelope_json(stakes="high", summary="short"))},
        )
    )
    add(
        mk(
            "register yaml",
            ["warden", "register", "{HOME}/e.yaml"],
            before={
                "e.yaml": f(
                    "generated_by: me\ntask: t\npermissions:\n  shell: true\n  shell_allowlist: [ls]\n"
                )
            },
        )
    )
    add(mk("register missing", ["warden", "register", "{HOME}/nope.json"]))
    opaque = [{"type": "no_side_effects", "description": "d"}]
    for rname, invs in (
        ("opaque refused", opaque),
        (
            "opaque two refused",
            [*opaque, {"type": "code_is_correct", "description": "d"}, opaque[0]],
        ),
        (
            "opaque word kept",
            [{"type": "keep_unchanged", "description": "d", "target": "/work/**"}],
        ),
        ("opaque off kept", [{**opaque[0], "enforce": False}]),
        ("opaque robotics kept", [{"type": "joint_limits_respected", "description": "d"}]),
    ):
        add(
            mk(
                f"register {rname}",
                ["warden", "register", "{HOME}/e.json"],
                before={"e.json": f(envelope_json(invariants=invs))},
            )
        )

    # -- argument rules: the schema store and the registration lint ----------
    gh_tools = json.dumps(
        {
            "tools": [
                {
                    "name": "get_issue",
                    "inputSchema": {
                        "type": "object",
                        "properties": {
                            "repo": {"type": "string"},
                            "number": {"type": "integer"},
                            "state": {"enum": ["open", "closed"]},
                        },
                    },
                }
            ]
        }
    )

    def rules_env(*rules: dict[str, Any]) -> str:
        return envelope_json(mcp_rules=list(rules))

    add(
        mk(
            "schemas import",
            ["warden", "schemas", "import", "github", "{HOME}/gh.json", "--root", "{HOME}/g"],
            before={"gh.json": f(gh_tools)},
        )
    )
    add(
        mk(
            "schemas import not a tools list",
            ["warden", "schemas", "import", "github", "{HOME}/gh.json", "--root", "{HOME}/g"],
            before={"gh.json": f('{"tools": 3}')},
        )
    )
    add(
        mk(
            "schemas import bad server name",
            ["warden", "schemas", "import", "../x", "{HOME}/gh.json", "--root", "{HOME}/g"],
            before={"gh.json": f(gh_tools)},
        )
    )
    add(mk("schemas list empty", ["warden", "schemas", "list", "--root", "{HOME}/g"]))
    add(
        mk(
            "schemas list one",
            ["warden", "schemas", "list", "--root", "{HOME}/g"],
            before={
                "g": d(0o700),
                "g/schemas": d(0o700),
                "g/schemas/github.json": f(gh_tools, 0o600),
            },
        )
    )
    in_repo = {"op": "in_set", "path": "repo", "values": ["acme/app"]}
    add(
        mk(
            "register mcp_rules no schema note",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={"e.json": f(rules_env({"tool": "github/get_issue", "args": [in_repo]}))},
        )
    )
    path_rule = {
        "tool": "fs/read",
        "args": [{"op": "path_in", "path": "path", "scope": "file_read"}],
    }
    add(
        mk(
            "register mcp_rules path_in relative glob",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={
                "e.json": f(envelope_json(file_read=["src/**", "/work/**"], mcp_rules=[path_rule]))
            },
        )
    )
    add(
        mk(
            "register mcp_rules path_in absolute globs",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={"e.json": f(envelope_json(file_read=["/work/**"], mcp_rules=[path_rule]))},
        )
    )
    add(
        mk(
            "register mcp_rules unanchored regex",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={
                "e.json": f(
                    rules_env(
                        {
                            "tool": "github/get_issue",
                            "args": [{"op": "matches", "path": "repo", "regex": "acme/.*"}],
                        }
                    )
                )
            },
        )
    )
    for label, rx in (
        ("alternation", "^a|.*$"),
        ("grouped alternation", "^(?:a|b)$"),
        ("unbalanced", "^a)|(b$"),
        ("unclosed", "^(a$"),
        ("lookahead", "^(?=a)a$"),
        ("lookbehind", "^(?<=a)b$"),
        ("backreference", "^(a)\\1$"),
        ("repeat over 1000", "^a{1001}$"),
        ("backtracking shape", "^(a+)+$"),
        ("flag group", "(?x)a|.*"),
        ("big u escape", "\\" + "U00000061"),
        ("set difference", "[!--/]"),
        ("double dash", "[a--b]"),
        ("p name", "\\p{Ascii}"),
        ("p letter", "\\p{Letter}"),
        ("p category", "\\p{Cn}"),
        ("benign class", "^[a-z0-9_-]{1,64}$"),
        ("benign words", "^(foo|bar)+$"),
        ("benign prefix", "^acme/.+$"),
        ("benign caseless", "(?i)^readme\\.md$"),
        ("benign verbs", "^(get|list)_[a-z_]+$"),
        ("caseless unanchored", "(?i)readme"),
        ("caseless alternation", "(?i)a|b"),
    ):
        add(
            mk(
                f"register mcp_rules regex {label}",
                ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
                before={
                    "e.json": f(
                        rules_env(
                            {
                                "tool": "github/get_issue",
                                "args": [{"op": "matches", "path": "repo", "regex": rx}],
                            }
                        )
                    )
                },
            )
        )
    add(
        mk(
            "register mcp_rules schema unknown argument",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={
                "e.json": f(
                    rules_env(
                        {
                            "tool": "github/get_issue",
                            "args": [{"op": "equals", "path": "repo_name", "value": "x"}],
                        }
                    )
                ),
                "g": d(0o700),
                "g/schemas": d(0o700),
                "g/schemas/github.json": f(gh_tools, 0o600),
            },
        )
    )
    add(
        mk(
            "register mcp_rules schema enum miss",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={
                "e.json": f(
                    rules_env(
                        {
                            "tool": "github/get_issue",
                            "args": [
                                {"op": "in_set", "path": "state", "values": ["open", "merged"]}
                            ],
                        }
                    )
                ),
                "g": d(0o700),
                "g/schemas": d(0o700),
                "g/schemas/github.json": f(gh_tools, 0o600),
            },
        )
    )
    add(
        mk(
            "register mcp_rules schema ok",
            ["warden", "register", "{HOME}/e.json", "--root", "{HOME}/g"],
            before={
                "e.json": f(
                    rules_env(
                        {
                            "tool": "github/get_issue",
                            "args": [in_repo, {"op": "exists", "path": "number"}],
                        }
                    )
                ),
                "g": d(0o700),
                "g/schemas": d(0o700),
                "g/schemas/github.json": f(gh_tools, 0o600),
            },
        )
    )
    add(
        mk(
            "register invalid",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f('{"task": "t"}')},
        )
    )
    add(
        mk(
            "register coerced",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(max_execution_time_s="60"))},
        )
    )
    add(
        mk(
            "register unknown keys",
            ["warden", "register", "{HOME}/e.json"],
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

    # -- install --warden -----------------------------------------------------
    claude_dir = {".claude": d()}
    gi: dict[str, Any] = {}
    # Enforce installs only with a registered envelope; with none it refuses.
    policy = {".opendaisugi/warden/envelopes/default.json": f(envelope_json(id="env_fixed01"))}
    add(mk("install warden claude", ["install", "--yes"], before=claude_dir, **gi))
    # A relative HOME gives a relative warden root: one line, exit 2, nothing written.
    add(
        mk(
            "install warden relative home",
            ["install", "--yes"],
            before={"rel/.claude": d()},
            env={"HOME": "rel"},
        )
    )
    # The message names the captures dir under the data home in use.
    add(
        mk(
            "install warden data home",
            ["install", "--yes"],
            before=claude_dir,
            env={"OPENDAISUGI_HOME": "{HOME}/od"},
        )
    )
    add(
        mk(
            "install warden enforce",
            ["install", "--enforce", "-y"],
            before={**claude_dir, **policy},
            **gi,
        )
    )
    add(
        mk(
            "install warden enforce no policy",
            ["install", "--enforce", "-y"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install warden enforce no policy dry run",
            ["install", "--enforce", "--dry-run"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install warden enforce empty envelopes dir",
            ["install", "--enforce", "-y"],
            before={
                **claude_dir,
                ".opendaisugi/warden/envelopes": d(),
                ".opendaisugi/warden/envelopes/notes.txt": f("x"),
            },
            **gi,
        )
    )
    add(
        mk(
            "install warden enforce session policy",
            ["install", "--enforce", "-y"],
            before={
                **claude_dir,
                ".opendaisugi/warden/envelopes/s1.json": f(envelope_json(id="env_fixed02")),
            },
            **gi,
        )
    )
    add(
        mk(
            "install warden ask",
            ["install", "--enforce", "--ask", "--yes"],
            before=claude_dir,
        )
    )
    add(
        mk(
            "install warden ask audit",
            ["install", "--ask", "--yes"],
            before=claude_dir,
        )
    )
    add(
        mk(
            "install warden old shadow flag",
            ["install", "--shadow", "--yes"],
            before=claude_dir,
        )
    )
    add(mk("install old shadow flag alone", ["install", "--shadow"], before=claude_dir))
    add(mk("install old warden flag", ["install", "--warden", "--yes"], before=claude_dir))
    add(mk("install no warden flag", ["install", "--no-warden"], before=claude_dir))
    add(mk("install ask without enforce", ["install", "--ask", "--yes"], before=claude_dir))
    add(
        mk(
            "install report without enforce",
            ["install", "--report", "coppice", "--yes"],
            before=claude_dir,
        )
    )
    add(
        mk(
            "install plain then warden status",
            ["warden", "status"],
            before=claude_dir,
            pre=[["install", "--yes"]],
        )
    )
    add(
        mk(
            "install plain then enforce",
            ["install", "--enforce", "--yes"],
            before={**claude_dir, **policy},
            pre=[["install", "--yes"]],
        )
    )
    add(
        mk(
            "install plain then enforce ask",
            ["install", "--enforce", "--ask", "--yes"],
            before={**claude_dir, **policy},
            pre=[["install", "--yes"]],
        )
    )
    add(
        mk(
            "install enforce then plain keeps enforce",
            ["install", "--yes"],
            before={**claude_dir, **policy},
            pre=[["install", "--enforce", "--yes"]],
        )
    )
    add(
        mk(
            "install codex plain then enforce",
            ["install", "--enforce", "--yes", "--runtime", "codex"],
            before=policy,
            pre=[["install", "--yes", "--runtime", "codex"]],
        )
    )
    add(
        mk(
            "install codex enforce then plain keeps enforce",
            ["install", "--yes", "--runtime", "codex"],
            before=policy,
            pre=[["install", "--enforce", "--yes", "--runtime", "codex"]],
        )
    )
    # -- the entry point is written as an absolute path (ruling INST-1) ------
    bare_hooks = json.dumps(
        {
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "Bash",
                        "hooks": [
                            {"type": "command", "command": "daisugi hook record --format claude"}
                        ],
                    }
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
            }
        },
        indent=2,
    )
    bare_mcp = json.dumps(
        {
            "mcpServers": {
                "opendaisugi": {"type": "stdio", "command": "daisugi", "args": ["mcp", "serve"]}
            }
        },
        indent=2,
    )
    add(
        mk(
            "install rebinds bare claude entries",
            ["install", "--yes", "--runtime", "claude"],
            before={
                ".claude": d(),
                CLAUDE: f(bare_hooks, 0o600),
                ".claude.json": f(bare_mcp, 0o600),
            },
            **gi,
        )
    )
    add(
        mk(
            "install rebinds bare claude entries with report",
            ["install", "--yes", "--runtime", "claude", "--report", "herdr"],
            before={
                ".claude": d(),
                CLAUDE: f(bare_hooks, 0o600),
                ".claude.json": f(bare_mcp, 0o600),
            },
            **gi,
        )
    )
    add(
        mk(
            "install keeps a custom mcp command",
            ["install", "--yes", "--runtime", "claude"],
            before={
                ".claude": d(),
                ".claude.json": f(
                    json.dumps(
                        {
                            "mcpServers": {
                                "opendaisugi": {"command": "/my/wrapper", "args": ["mcp", "serve"]}
                            }
                        }
                    )
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install rebinds bare hermes entries",
            ["install", "--yes", "--runtime", "hermes"],
            before={
                ".hermes": d(),
                ".hermes/config.yaml": f(
                    "mcp_servers:\n  opendaisugi:\n    command: daisugi\n    args:\n    - mcp\n    - serve\n"
                    "hooks:\n  pre_tool_call:\n  - matcher: .*\n    command: daisugi hook record --format hermes\n"
                    "    timeout: 10\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install rebinds bare codex entries",
            ["install", "--yes", "--runtime", "codex"],
            before={
                ".codex": d(),
                ".codex/config.toml": f(
                    'model = "x"\n\n[mcp_servers.opendaisugi]\ncommand = "daisugi"\nargs = ["mcp", "serve"]\n\n[other]\nk = 1\n'
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install rebinds bare openclaw entries",
            ["install", "--yes", "--runtime", "openclaw"],
            before={
                ".openclaw/workspace": d(),
                ".openclaw/openclaw.json": f(
                    json.dumps(
                        {
                            "mcp": {
                                "servers": {
                                    "opendaisugi": {"command": "daisugi", "args": ["mcp", "serve"]}
                                }
                            }
                        }
                    )
                ),
            },
            **gi,
        )
    )

    # -- a hook that is more than one plain command is left as it is --------
    def pre_hooks(*cmds: str) -> str:
        return json.dumps(
            {
                "hooks": {
                    "PreToolUse": [{"hooks": [{"type": "command", "command": c}]} for c in cmds]
                }
            },
            indent=2,
        )

    compound = (
        "daisugi hook record --format claude || true",
        "daisugi hook record --format claude 2>/dev/null",
        "daisugi hook record --format claude; exit 0",
        "env X=1 daisugi hook record --format claude",
    )
    add(
        mk(
            "install leaves compound claude hooks",
            ["install", "--yes", "--runtime", "claude"],
            before={".claude": d(), CLAUDE: f(pre_hooks(*compound), 0o600)},
            **gi,
        )
    )
    add(
        mk(
            "install leaves compound claude hooks capture only",
            ["install", "--yes", "--runtime", "claude", "--only", "capture"],
            before={".claude": d(), CLAUDE: f(pre_hooks(*compound), 0o600)},
            **gi,
        )
    )
    add(
        mk(
            "install rebinds a plain claude hook with two spaces",
            ["install", "--yes", "--runtime", "claude", "--only", "capture"],
            before={
                ".claude": d(),
                CLAUDE: f(pre_hooks("daisugi  hook record  --format   claude  "), 0o600),
            },
            **gi,
        )
    )
    add(
        mk(
            "install rebinds a quoted old claude path",
            ["install", "--yes", "--runtime", "claude", "--only", "capture"],
            before={
                ".claude": d(),
                CLAUDE: f(pre_hooks("'/old path/daisugi' hook record --format claude"), 0o600),
            },
            **gi,
        )
    )
    add(
        mk(
            "install leaves a compound hermes hook",
            ["install", "--yes", "--runtime", "hermes", "--only", "capture"],
            before={
                ".hermes": d(),
                ".hermes/config.yaml": f(
                    "hooks:\n  pre_tool_call:\n  - matcher: .*\n"
                    "    command: daisugi hook record --format hermes || true\n    timeout: 10\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install exits 1 when a left hook cannot run",
            ["install", "--yes", "--runtime", "claude", "--only", "capture"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    pre_hooks(
                        "/nonexistent-daisugi-dir/daisugi hook record --format claude || true"
                    ),
                    0o600,
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install keeps a user wrapper named daisugi-local",
            ["install", "--yes", "--runtime", "claude"],
            before={
                ".claude": d(),
                CLAUDE: f(pre_hooks("/opt/x/daisugi-local hook record --format claude"), 0o600),
                ".claude.json": f(
                    json.dumps(
                        {
                            "mcpServers": {
                                "opendaisugi": {
                                    "command": "/opt/x/daisugi-local",
                                    "args": ["mcp", "serve"],
                                }
                            }
                        }
                    ),
                    0o600,
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "uninstall keeps a user wrapper hook",
            ["install", "--uninstall", "--runtime", "claude"],
            before={
                ".claude": d(),
                CLAUDE: f(pre_hooks("/opt/x/daisugi-local hook record --format claude"), 0o600),
            },
        )
    )
    add(
        mk(
            "install twice writes nothing the second time",
            ["install", "--yes"],
            before={".claude": d(), ".codex": d(), ".hermes": d()},
            pre=[["install", "--yes"]],
            **gi,
        )
    )
    add(
        mk(
            "uninstall removes the bare and the absolute record hook",
            ["install", "--uninstall", "--runtime", "claude"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    json.dumps(
                        {
                            "hooks": {
                                "PreToolUse": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "daisugi hook record --format claude",
                                            }
                                        ]
                                    },
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "/opt/x/daisugi hook record --format claude",
                                            }
                                        ]
                                    },
                                ]
                            }
                        },
                        indent=2,
                    ),
                    0o600,
                ),
            },
        )
    )
    add(
        mk(
            "uninstall removes the bare and the absolute codex block",
            ["install", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/config.toml": f(
                    'model = "x"\n\n[mcp_servers.opendaisugi]\ncommand = "/opt/x/daisugi"\nargs = ["mcp", "serve"]\n'
                )
            },
        )
    )
    add(mk("install plain hermes only", ["install", "--yes"], before={".hermes": d()}))
    # SOUL.md is the file Hermes always loads. A missing one is not made: the
    # file would hold only the note and replace Hermes's built-in identity.
    add(
        mk(
            "install hermes soul exists",
            ["install", "--yes", "--runtime", "hermes"],
            before={".hermes/SOUL.md": f("You are Hermes.\n")},
        )
    )
    add(
        mk(
            "install hermes soul twice",
            ["install", "--yes", "--runtime", "hermes"],
            before={".hermes/SOUL.md": f("You are Hermes.\n")},
            pre=[["install", "--yes", "--runtime", "hermes"]],
        )
    )
    add(
        mk(
            "install hermes soul missing",
            ["install", "--yes", "--runtime", "hermes"],
            before={".hermes": d()},
        )
    )
    add(
        mk(
            "install hermes soul only",
            ["install", "--yes", "--runtime", "hermes", "--only", "instructions"],
            before={".hermes/SOUL.md": f("You are Hermes.\n")},
        )
    )
    # An install made before the pathway text changed: the block is replaced
    # in place, the bytes around it stay, and the plan line says Update.
    old_block = "<!-- opendaisugi-managed -->\n## openDaisugi old text, journaled automatically\n<!-- opendaisugi-managed -->"
    add(
        mk(
            "install claude md old block refreshed",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={
                ".claude/CLAUDE.md": f(
                    "# mine\n\nkeep  this \t exactly\n\n" + old_block + "\n\nafter, no newline"
                )
            },
        )
    )
    add(
        mk(
            "install codex agents old block refreshed",
            ["install", "--yes", "--runtime", "codex", "--only", "instructions"],
            before={".codex/AGENTS.md": f(old_block + "\n")},
        )
    )
    add(
        mk(
            "install hermes soul old block refreshed",
            ["install", "--yes", "--runtime", "hermes", "--only", "instructions"],
            before={".hermes/SOUL.md": f("You are Hermes.\n\n" + old_block + "\n")},
        )
    )
    add(
        mk(
            "install claude md current block is a no-op",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("# mine\n")},
            pre=[["install", "--yes", "--runtime", "claude", "--only", "instructions"]],
        )
    )
    add(
        mk(
            "install claude md a lone marker line is left alone",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("prose\n<!-- opendaisugi-managed -->\nprose\n")},
        )
    )
    # A marker inside a line of prose is not a marker: the block goes on the
    # end, and a real block after it is refreshed without touching the prose.
    add(
        mk(
            "install claude md a prose marker is not a marker",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("say <!-- opendaisugi-managed --> in a line\n")},
        )
    )
    add(
        mk(
            "install claude md prose marker then an old block",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={
                ".claude/CLAUDE.md": f(
                    "I wrote <!-- opendaisugi-managed --> here.\nkeep this\n\n" + old_block + "\n"
                )
            },
        )
    )
    add(
        mk(
            "uninstall claude md prose marker then a block",
            ["install", "--uninstall", "--runtime", "claude", "--only", "instructions"],
            before={
                ".claude/CLAUDE.md": f(
                    "I wrote <!-- opendaisugi-managed --> here.\nkeep this\n\n" + old_block + "\n"
                )
            },
        )
    )
    # CRLF files keep their line ends outside the block; the block takes the
    # line ends of the one it replaces.
    crlf_block = old_block.replace("\n", "\r\n")
    add(
        mk(
            "install claude md old block in a CRLF file",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("# mine\r\n\r\n" + crlf_block + "\r\n\r\nafter\r\n")},
        )
    )
    add(
        mk(
            "install claude md appends to a CRLF file",
            ["install", "--yes", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("one\r\ntwo\r\n")},
        )
    )
    add(
        mk(
            "uninstall claude md block in a CRLF file",
            ["install", "--uninstall", "--runtime", "claude", "--only", "instructions"],
            before={".claude/CLAUDE.md": f("# mine\r\n\r\n" + crlf_block + "\r\n")},
        )
    )
    add(
        mk(
            "uninstall hermes soul",
            ["install", "--uninstall", "--runtime", "hermes", "--only", "instructions"],
            before={
                ".hermes/SOUL.md": f(
                    "You are Hermes.\n\n<!-- opendaisugi-managed -->\nold\n<!-- opendaisugi-managed -->\n"
                )
            },
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
            "install warden existing settings",
            ["install", "--yes"],
            before={".claude": d(), CLAUDE: f(other, 0o600)},
            **gi,
        )
    )
    add(
        mk(
            "install warden invalid settings",
            ["install", "--yes"],
            before={".claude": d(), CLAUDE: f("{oops")},
            **gi,
        )
    )
    add(
        mk(
            "install warden settings list",
            ["install", "--yes"],
            before={".claude": d(), CLAUDE: f('{"hooks": []}')},
            **gi,
        )
    )
    add(
        mk(
            "install warden other mode",
            ["install", "--enforce", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(PY_AUDIT)), **policy},
            **gi,
        )
    )
    add(
        mk(
            "install warden old module",
            ["install", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "python3 -m opendaisugi.warden --mode audit --root {HOME}/.opendaisugi/warden --format claude --verify-timeout 10.0"
                    )
                ),
            },
            **gi,
        )
    )
    add(mk("install warden dry run", ["install", "--dry-run"], before=claude_dir, **gi))
    add(
        mk(
            "install warden confirm yes",
            ["install"],
            stdin="y\n",
            before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")},
            **gi,
        )
    )
    add(mk("install warden confirm no", ["install"], stdin="n\n", before=claude_dir, **gi))
    add(mk("install warden none detected", ["install", "--yes"], **gi))
    add(mk("install warden codex", ["install", "--yes", "--runtime", "codex"], **gi))
    add(
        mk(
            "install warden codex path",
            ["install", "--enforce", "--yes"],
            path_bins=["codex"],
            before=policy,
            **gi,
        )
    )
    add(
        mk(
            "install warden codex existing",
            ["install", "--yes", "--runtime", "codex"],
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
                                                "command": "python3 -m opendaisugi.warden_client --mode enforce",
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
            "install warden all runtimes",
            ["install", "--yes"],
            before={".claude": d(), ".codex": d(), ".hermes": d(), ".openclaw": d()},
            **gi,
        )
    )
    add(
        mk(
            "install warden runtime prefix",
            ["install", "--yes", "--runtime", "cl", "--runtime", "hermes"],
            **gi,
        )
    )
    add(mk("install warden runtime ambiguous", ["install", "--yes", "--runtime", "c"], **gi))
    add(mk("install warden runtime unknown", ["install", "--yes", "--runtime", "vim"], **gi))
    add(
        mk(
            "install warden idempotent",
            ["install", "--enforce", "--yes"],
            before={".claude": d(), **policy},
            twice=True,
            **gi,
        )
    )
    add(
        mk(
            "install warden idempotent renamed",
            ["install", "--enforce", "--yes"],
            before={".claude": d(), **policy},
            twice=True,
            prog="daisugi-copy",
            **gi,
        )
    )
    # -- install --router (the BASE_URL layer) and --routing-mode ----------------
    add(
        mk(
            "install router claude",
            ["install", "--router", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(mk("install router only", ["install", "--router", "--yes"], before=claude_dir, **gi))
    add(
        mk(
            "install router base url",
            ["install", "--router", "--base-url", "http://127.0.0.1:9999", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router empty base url",
            ["install", "--router", "--base-url", "", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router existing env",
            ["install", "--router", "--yes"],
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
            "install router already set",
            ["install", "--router", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(json.dumps({"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"}})),
            },
            **gi,
        )
    )
    add(
        mk(
            "install router invalid settings",
            ["install", "--router", "--yes"],
            before={".claude": d(), CLAUDE: f("{nope")},
            **gi,
        )
    )
    add(
        mk(
            "install router env not a dict",
            ["install", "--router", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": ["x"]}))},
            **gi,
        )
    )
    add(
        mk(
            "install router warden then env fails",
            ["install", "--router", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": "x"}))},
            **gi,
        )
    )
    add(
        mk(
            "install router settings list",
            ["install", "--router", "--yes"],
            before={".claude": d(), CLAUDE: f("[1]")},
            **gi,
        )
    )
    add(
        mk(
            "install router no claude dir",
            ["install", "--router", "--yes", "--runtime", "claude"],
            **gi,
        )
    )
    add(
        mk(
            "install router dry run",
            ["install", "--router", "--dry-run"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router enforce no warden",
            ["install", "--router", "--enforce", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install router codex",
            ["install", "--router", "--yes", "--runtime", "codex"],
            **gi,
        )
    )
    add(
        mk(
            "install router codex existing",
            ["install", "--router", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('model = "gpt-5"\n\n[profiles.x]\nmodel = "o3"\n\n\n')},
            **gi,
        )
    )
    add(
        mk(
            "install router codex selected",
            ["install", "--router", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('model_provider = "opendaisugi"\nmodel = "x"\n')},
            **gi,
        )
    )
    add(
        mk(
            "install router codex present",
            ["install", "--router", "--yes", "--runtime", "codex"],
            before={".codex/config.toml": f('[model_providers.opendaisugi]\nbase_url = "x"\n')},
            **gi,
        )
    )
    add(
        mk(
            "install router all runtimes",
            ["install", "--router", "--yes"],
            before={".claude": d(), ".codex": d(), ".hermes": d(), ".openclaw": d()},
            **gi,
        )
    )
    add(
        mk(
            "install router openclaw",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
            **gi,
        )
    )
    add(
        mk(
            "install router openclaw existing",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
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
            "install router openclaw json5",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
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
            "install router openclaw present",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
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
            "install router openclaw invalid",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
            before={".openclaw/openclaw.json": f("{nope")},
            **gi,
        )
    )
    add(
        mk(
            "install router openclaw models list",
            ["install", "--router", "--yes", "--runtime", "openclaw"],
            before={".openclaw/openclaw.json": f(json.dumps({"models": []}))},
            **gi,
        )
    )
    add(mk("install router hermes", ["install", "--router", "--yes", "--runtime", "hermes"], **gi))
    add(
        mk(
            "install router idempotent",
            ["install", "--router", "--yes"],
            before={".claude": d(), ".codex": d(), ".openclaw": d()},
            twice=True,
            **gi,
        )
    )
    # The binary under a name that is not ours writes the PATH daisugi, so a
    # second install finds its hooks (ruling INS-2).
    add(
        mk(
            "install router idempotent renamed",
            ["install", "--router", "--yes"],
            before={".claude": d(), ".codex": d(), ".openclaw": d()},
            twice=True,
            prog="daisugi-copy",
            **gi,
        )
    )
    add(
        mk(
            "install routing mode rules",
            ["install", "--router", "--routing-mode", "rules", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode knn",
            ["install", "--router", "--routing-mode", "knn", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode knn dry run",
            ["install", "--router", "--routing-mode", "knn", "--dry-run"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode off keeps config",
            ["install", "--router", "--routing-mode", "off", "--yes"],
            before={
                ".claude": d(),
                ".opendaisugi/config.yaml": f(
                    "warden_mode: enforce\nmax_task_chars: '12'\nauto_tend: yes\nzzz: 1\n"
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "install routing mode switchyard",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode switchyard claude key",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode switchyard key set",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode switchyard clear key",
            ["install", "--router", "--routing-mode", "switchyard", "--api-key-env", "", "--yes"],
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
            "install routing mode switchyard no efficient",
            ["install", "--router", "--routing-mode", "switchyard", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode switchyard same model",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode switchyard capable",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode unknown",
            ["install", "--router", "--routing-mode", "magic", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode without router",
            ["install", "--routing-mode", "rules", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode flags without routing mode",
            ["install", "--router", "--efficient-model", "m", "--yes"],
            before=claude_dir,
            **gi,
        )
    )
    add(
        mk(
            "install routing mode dry run",
            [
                "install",
                "--router",
                "--routing-mode",
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
            "install routing mode invalid config",
            [
                "install",
                "--router",
                "--routing-mode",
                "switchyard",
                "--efficient-model",
                "m",
                "--yes",
            ],
            before={".claude": d(), ".opendaisugi/config.yaml": f("routing_mode: [1]\n")},
            **gi,
        )
    )
    add(
        mk(
            "install report herdr",
            ["install", "--report", "herdr", "--yes"],
            before=claude_dir,
        )
    )
    add(mk("install plain", ["install", "--yes"], before=claude_dir))
    add(
        mk(
            "install decomposition",
            ["install", "--allow-shell-decomposition", "--yes"],
            before=claude_dir,
        )
    )

    # -- uninstall ----------------------------------------------------------
    gu: dict[str, Any] = {}
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
            "uninstall warden claude",
            ["install", "--uninstall"],
            before={".claude": d(), CLAUDE: f(installed, 0o600)},
            **gu,
        )
    )
    add(
        mk(
            "uninstall warden only hook",
            ["install", "--uninstall"],
            before={".claude": d(), CLAUDE: f(hook_settings(PY_AUDIT))},
            **gu,
        )
    )
    add(mk("uninstall warden nothing", ["install", "--uninstall"], before=claude_dir, **gu))
    add(
        mk(
            "uninstall warden codex",
            ["install", "--uninstall", "--runtime", "codex"],
            before={".codex/hooks.json": f(hook_settings(PY_ENFORCE))},
            **gu,
        )
    )
    add(
        mk(
            "uninstall warden codex renamed",
            ["install", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/hooks.json": f(
                    hook_settings(
                        "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/bin/daisugi-copy warden check "
                        "--mode audit --root /x --format codex --verify-timeout 10.0"
                    )
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall warden go form",
            ["install", "--uninstall"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
                    )
                ),
            },
            **gu,
        )
    )
    add(mk("uninstall warden none detected", ["install", "--uninstall"], **gu))
    router_settings = json.dumps(
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
            "uninstall router claude",
            ["install", "--router", "--uninstall"],
            before={".claude": d(), CLAUDE: f(router_settings)},
            **gu,
        )
    )
    add(
        mk(
            "uninstall warden and router env only key",
            ["install", "--uninstall"],
            before={".claude": d(), CLAUDE: f(json.dumps({"env": {"ANTHROPIC_BASE_URL": "x"}}))},
            **gu,
        )
    )
    add(
        mk(
            "uninstall router codex",
            ["install", "--router", "--uninstall", "--runtime", "codex"],
            before={
                ".codex/config.toml": f(
                    'model_provider = "opendaisugi"\nmodel = "m"\n\n[model_providers.opendaisugi]\n'
                    'name = "openDaisugi router"\nbase_url = "http://127.0.0.1:8787/v1"\n'
                    'wire_api = "chat"\n\n[profiles.p]\nx = 1\n'
                )
            },
            **gu,
        )
    )
    add(
        mk(
            "uninstall router codex only block",
            ["install", "--router", "--uninstall", "--runtime", "codex"],
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
            "uninstall router openclaw",
            ["install", "--router", "--uninstall", "--runtime", "openclaw"],
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
            "uninstall router openclaw keeps others",
            ["install", "--router", "--uninstall", "--runtime", "openclaw"],
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
                ".pi/agent/extensions/daisugi-warden/index.ts": {"link": "{HOME}/target"},
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
                ".pi/agent/extensions/daisugi-warden/index.ts": f("x"),
                ".config/opencode/plugins/daisugi-warden.ts": f("x"),
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
        "cfg multi plain": "warden_mode: enforce\nnotify: aaa\n  bbb\nmodel: x\n",
        "cfg floor seq": "warden_mode: enforce\nfloor:\n- a\n",
        "cfg seq same indent": "warden_mode: enforce\nextra:\n- a\n- b: 1\n  c: 2\n",
        "cfg float int ok": "warden_mode: enforce\nz3_timeout_ms: 800.0\n",
        "cfg str int ok": "warden_mode: enforce\nz3_timeout_ms: ' 1_000 '\n",
        "cfg str bool ok": "warden_mode: enforce\nwarden_ask: 'T'\n",
        "cfg str bool bad": "warden_mode: enforce\nwarden_ask: 'nope'\n",
        "cfg dquote esc": 'warden_mode: "enf\\x6frce"\n',
        "cfg falsy top": "0\n",
        "cfg flow map": "warden_mode: enforce\nfloor: {backend: tmux, notify_cmd: null}\n",
        "cfg flow map bad": "warden_mode: enforce\nfloor: {backend: [1]}\n",
        "cfg doc marker": "---\nwarden_mode: enforce\n",
        "cfg key on": "on: 1\nwarden_mode: enforce\n",
        "cfg nested unknown": "warden_mode: enforce\nx:\n  y:\n    z: [1, {a: b}]\n",
        "cfg bool int": "warden_mode: enforce\nmax_task_chars: true\n",
        "cfg path int": "warden_mode: enforce\ndata_dir: 5\n",
    }
    for name, text in more_cfg.items():
        add(
            mk(
                f"status {name}",
                ["warden", "status", "--json"],
                before={".opendaisugi/config.yaml": f(text)},
            )
        )
    add(
        mk(
            "init cfg decomposition",
            ["warden", "init", "--workspace", "/work"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: yes\n")},
        )
    )
    add(
        mk(
            "init cfg invalid",
            ["warden", "init", "--workspace", "/work"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: maybe\n")},
        )
    )
    add(
        mk(
            "init no decomposition flag",
            ["warden", "init", "--workspace", "/work", "--no-allow-shell-decomposition"],
            before={".opendaisugi/config.yaml": f("shell_allow_decomposition: true\n")},
        )
    )
    add(mk("init relative workspace", ["warden", "init", "--workspace", "sub/../w"], cwd="proj"))
    add(
        mk(
            "init symlink workspace",
            ["warden", "init", "--workspace", "{HOME}/link/x"],
            before={"real": d(), "link": {"link": "{HOME}/real"}},
        )
    )

    # -- register: YAML, coercions, robotics, predicates -------------------
    add(
        mk(
            "register yaml block",
            ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(shell="yes", network=0))},
        )
    )
    add(
        mk(
            "register bad bool",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(shell="maybe"))},
        )
    )
    add(
        mk(
            "register float int",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(max_execution_time_s=60.0))},
        )
    )
    add(
        mk(
            "register robotics",
            ["warden", "register", "{HOME}/e.json"],
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
            ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(workspace_bounds=[[0, 0], [1, 1]]))},
        )
    )
    add(
        mk(
            "register predicates",
            ["warden", "register", "{HOME}/e.json", "--session", "p"],
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
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(summary="x" * 81))},
        )
    )
    add(
        mk(
            "register bad stakes",
            ["warden", "register", "{HOME}/e.json"],
            before={"e.json": f(envelope_json(stakes="extreme"))},
        )
    )
    add(
        mk(
            "register unicode",
            ["warden", "register", "{HOME}/e.json"],
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
            ["warden", "register", "{HOME}/e.yaml"],
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
                ["warden", "register", "{HOME}/e.json"],
                before={"e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", value))},
            )
        )
    for value in (".nan", ".NaN", ".inf", "-.inf", "+.inf", ".Inf"):
        add(
            mk(
                f"register yaml {value}",
                ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.yaml"],
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
            ["warden", "register", "{HOME}/e.json"],
            before={
                "e.json": f(envelope_json(velocity_limit=1.5).replace("1.5", "NaN")),
                f"{GROOT}/envelopes/default.json": f(envelope_json(id="env_old00001")),
            },
        )
    )
    add(
        mk(
            "register nan and a bad field",
            ["warden", "register", "{HOME}/e.json"],
            before={
                "e.json": f(envelope_json(velocity_limit=1.5, shell="maybe").replace("1.5", "NaN"))
            },
        )
    )

    # -- report values -----------------------------------------------------
    add(
        mk(
            "report floats unicode",
            ["warden", "report"],
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
    add(mk("proposals none", ["warden", "proposals"]))
    add(mk("proposals none json", ["warden", "proposals", "--json"]))
    add(mk("proposals some", ["warden", "proposals"], before=props))
    add(mk("proposals some json", ["warden", "proposals", "--json"], before=props))

    # -- the root and the commands this binary does not carry -------------
    add(mk("version", ["--version"]))
    replay_cases(add)
    help_cases(add)
    layer_cases(add)
    recipe_cases(add)
    add(
        mk(
            "uninstall all",
            ["install", "--uninstall"],
            before=claude_dir,
        )
    )
    add(mk("status unknown option", ["warden", "status", "--bogus"]))
    add(mk("register no argument", ["warden", "register"]))
    add(mk("install warden confirm eof", ["install"], stdin="", before=claude_dir, **gi))
    add(
        mk(
            "install warden confirm again",
            ["install"],
            stdin="maybe\ny\n",
            before={".claude": d(), ".opendaisugi/config.yaml": f("auto_tend: false\n")},
            **gi,
        )
    )
    add(
        mk(
            "install warden claude forced missing",
            ["install", "--yes", "--runtime", "claude"],
            **gi,
        )
    )
    add(
        mk(
            "install warden int command",
            ["install", "--yes"],
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
            "install warden no command key",
            ["install", "--yes"],
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
            "install warden pre dict",
            ["install", "--yes"],
            before={".claude": d(), CLAUDE: f(json.dumps({"hooks": {"PreToolUse": {}}}))},
            **gi,
        )
    )
    add(
        mk(
            "install warden other keys kept",
            ["install", "--enforce", "--yes"],
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
            "install warden codex go form",
            ["install", "--yes", "--runtime", "codex"],
            before={
                ".codex/hooks.json": f(
                    hook_settings(
                        "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check --mode audit --root /x "
                        "--format claude --verify-timeout 10.0"
                    )
                )
            },
            **gi,
        )
    )
    add(
        mk(
            "install warden claude go form",
            ["install", "--yes"],
            before={
                ".claude": d(),
                CLAUDE: f(
                    hook_settings(
                        "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check --mode enforce --root /x "
                        "--format claude --verify-timeout 10.0 || exit 2"
                    )
                ),
            },
            **gi,
        )
    )
    add(
        mk(
            "uninstall warden report hooks",
            ["install", "--uninstall"],
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
            "uninstall warden bad json",
            ["install", "--uninstall"],
            before={".claude": d(), CLAUDE: f("{x")},
            **gu,
        )
    )
    add(mk("bare", []))

    # -- hooks are found by their words, not a substring ---------------------
    foreign = "/opt/opendaisugi.router-watch --mode enforce"
    quoted = "'/opt/x --mode enforce/python' -m opendaisugi.warden --root /r"
    add(
        mk(
            "status foreign hook",
            ["warden", "status", "--json"],
            before={CLAUDE: f(hook_settings(foreign))},
        )
    )
    add(
        mk(
            "status quoted mode",
            ["warden", "status", "--json"],
            before={CLAUDE: f(hook_settings(quoted))},
        )
    )
    add(
        mk(
            "install warden foreign hook",
            ["install", "--enforce", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(foreign))},
            **gi,
        )
    )
    add(
        mk(
            "install warden codex foreign hook",
            ["install", "--yes", "--runtime", "codex"],
            before={".codex/hooks.json": f(hook_settings(foreign))},
            **gi,
        )
    )
    add(
        mk(
            "uninstall warden foreign hooks",
            ["install", "--uninstall"],
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
    # -- hand-written warden hooks, and ones in a form not read (round 2) -----
    written = {
        "uv run": "uv run --project /p python -m opendaisugi.warden --mode enforce --root /x || exit 2",
        "env prefix": "FOO=1 python3 -m opendaisugi.warden --mode enforce",
        "env command": "env FOO=1 python3 -m opendaisugi.warden --mode enforce",
        "python flag": "python3 -I -m opendaisugi.warden --mode enforce",
        "joined m": "python3 -mopendaisugi.warden --mode enforce",
        "no space op": "python3 -m opendaisugi.warden --mode enforce||exit 2",
        "sh c": "sh -c 'python3 -m opendaisugi.warden --mode enforce'",
        "bare daisugi": "daisugi warden check --mode enforce --root /x || exit 2",
        "echo": "echo -m opendaisugi.warden --mode enforce",
        "true marker": "DAISUGI_WARDEN_HOOK=opendaisugi.warden /bin/true warden check --mode enforce",
        "renamed": "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/bin/daisugi-copy warden check "
        "--mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2",
        "renamed no marker": "/opt/bin/daisugi-copy warden check --mode enforce --root /x "
        "--format claude --verify-timeout 10.0 || exit 2",
        "renamed few flags": "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/bin/daisugi-copy "
        "warden check --mode enforce || exit 2",
        "unknown": "nice python3 -m opendaisugi.warden --mode enforce",
        # Hooks from before audit mode was renamed: the warning's fix,
        # install --warden --uninstall, removes them.
        "old shadow python": "python3 -m opendaisugi.warden --mode shadow --root /x",
        "old shadow daisugi": "DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/daisugi warden check "
        "--mode shadow --root /x --format claude",
    }
    for name, command in written.items():
        tree = {".claude": d(), CLAUDE: f(hook_settings(command))}
        add(mk(f"status written {name}", ["warden", "status", "--json"], before=tree))
        add(mk(f"uninstall written {name}", ["install", "--uninstall"], before=tree, **gu))
    add(
        mk(
            "status written unknown text",
            ["warden", "status"],
            before={CLAUDE: f(hook_settings(written["unknown"]))},
        )
    )
    add(
        mk(
            "status unknown beside audit",
            ["warden", "status", "--json"],
            before={
                CLAUDE: f(hook_settings(PY_AUDIT)),
                "proj/.claude/settings.json": f(hook_settings(written["unknown"])),
            },
            cwd="proj",
        )
    )
    add(
        mk(
            "install warden beside unknown",
            ["install", "--yes"],
            before={".claude": d(), CLAUDE: f(hook_settings(written["unknown"]))},
            **gi,
        )
    )
    add(
        mk(
            "install warden settings symlink",
            ["install", "--yes"],
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


# The commands no port carries, as `daisugi help --all` names them: a
# top-level command, or a group's subcommand ("group sub"). A port prints
# the oracle's help --all text with these lines left out (ruling HP-1).
HELP_NOT_PORTED = (
    "bench",
    "conformance",
    "coppice",
    "warden audit",
)


def help_for_port(text: str) -> str:
    """The oracle's help --all text as a port prints it."""
    out = []
    for line in text.splitlines(keepends=True):
        words = line.split()
        if line.startswith("    ") and len(words) >= 2:
            if words[0] in HELP_NOT_PORTED or f"{words[0]} {words[1]}" in HELP_NOT_PORTED:
                continue
        elif line.startswith("  ") and words and words[0] in HELP_NOT_PORTED:
            continue
        out.append(line)
    return "".join(out)


def help_cases(add: Any) -> None:
    """`daisugi help`: the start-here text, or every command with --all."""
    add(mk("help", ["help"]))
    add(mk("help all", ["help", "--all"]))
    add(mk("help quiet all", ["-q", "help", "--all"]))
    add(mk("help bad option", ["help", "--bogus"]))
    add(mk("help extra argument", ["help", "x"]))


def layer_cases(add: Any) -> None:
    """The four default layers of `install` and their reverse, per harness."""
    claude = {".claude": d()}
    codex = {".codex": d()}
    hermes = {".hermes": d()}
    openclaw = {".openclaw": d()}
    every = {**claude, **codex, **hermes, **openclaw}
    y = ["install", "--yes"]
    add(mk("layers claude", y, before=claude))
    add(mk("layers codex", y, before=codex))
    add(mk("layers hermes", y, before=hermes))
    add(mk("layers openclaw", y, before=openclaw))
    add(mk("layers every harness", y, before=every))
    add(mk("layers every harness dry run", ["install", "--dry-run"], before=every))
    add(mk("layers every harness router", [*y, "--router"], before=every))
    add(mk("layers twice", y, before=every, pre=[y]))
    add(mk("layers claude skills dir", y, before={**claude, ".claude/skills": d()}))
    add(mk("layers agents dir", y, before={**claude, ".agents/skills": d()}))
    add(mk("layers codex skills dir", y, before={**codex, ".codex/skills": d()}))
    add(mk("layers runtime claude no dir", ["install", "--runtime", "claude", "--yes"]))
    add(
        mk(
            "layers existing configs",
            y,
            before={
                **every,
                ".claude.json": f(
                    json.dumps({"numStartups": 3, "mcpServers": {"x": {"command": "x"}}})
                ),
                ".claude/settings.json": f(
                    json.dumps(
                        {
                            "env": {"A": "1"},
                            "hooks": {"PreToolUse": [{"matcher": "*", "hooks": []}]},
                        }
                    )
                ),
                ".claude/CLAUDE.md": f("# mine\n\n"),
                ".codex/config.toml": f('model = "x"\n'),
                ".codex/AGENTS.md": f("notes"),
                ".hermes/config.yaml": f("model: x\nhooks:\n  other:\n  - a\n"),
                ".openclaw/openclaw.json": f('{\n  // a comment\n  "mcp": {"servers": {}},\n}\n'),
                ".openclaw/workspace/AGENTS.md": f("ws\n"),
            },
        )
    )
    add(
        mk(
            "layers bad configs",
            y,
            before={
                **every,
                ".claude.json": f("{not json"),
                ".claude/settings.json": f("[1, 2"),
                ".hermes/config.yaml": f("a: [\n"),
                ".openclaw/openclaw.json": f("{nope"),
            },
        )
    )
    add(
        mk(
            "layers already there",
            y,
            before={
                **every,
                ".claude.json": f(json.dumps({"mcpServers": {"opendaisugi": {"command": "old"}}})),
                ".claude/CLAUDE.md": f(
                    "<!-- opendaisugi-managed -->\nold\n<!-- opendaisugi-managed -->\n"
                ),
                ".claude/settings.json": f(
                    json.dumps(
                        {
                            "hooks": {
                                "PreToolUse": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "daisugi hook record --format claude",
                                            }
                                        ]
                                    }
                                ],
                                "SessionStart": [
                                    {
                                        "hooks": [
                                            {
                                                "type": "command",
                                                "command": "daisugi install --print-skill",
                                            }
                                        ]
                                    }
                                ],
                            }
                        }
                    )
                ),
                ".codex/config.toml": f('[mcp_servers.opendaisugi]\ncommand = "x"\n'),
            },
        )
    )
    add(
        mk(
            "layers skill was a file",
            y,
            before={**claude, ".agents/skills/opendaisugi-checklist": f("x")},
        )
    )
    add(mk("layers print skill", ["install", "--print-skill"]))
    add(mk("layers confirm yes", ["install"], before=claude, stdin="y\ny\n"))
    add(mk("layers confirm yes then no", ["install"], before=claude, stdin="y\nn\n"))
    add(mk("layers confirm yes then default", ["install"], before=claude, stdin="y\n\n"))
    add(
        mk(
            "layers confirm consent asked before",
            ["install"],
            before={**claude, ".opendaisugi/config.yaml": f("auto_tend: false\n")},
            stdin="y\n",
        )
    )
    add(mk("layers decomposition off", [*y, "--no-allow-shell-decomposition"], before=claude))
    add(mk("layers report coppice", [*y, "--report", "coppice"], before=claude))
    # A save keeps the operator's agent profiles as they are.
    profiles = {
        ".opendaisugi/config.yaml": f(
            "agent_profiles:\n  oai: {wire: openai, base_url: 'http://127.0.0.1:9/v1', key_env: OAI_KEY}\n"
            "  desk: {wire: anthropic, model: 'llama3.3:70b', weight: [1, 2.5, true, null]}\n"
        )
    }
    add(
        mk(
            "layers report coppice keeps agent profiles",
            [*y, "--report", "coppice"],
            before={**claude, **profiles},
        )
    )
    add(mk("layers report without warden", [*y, "--report", "coppice"], before=claude))
    add(mk("layers report bad", [*y, "--report", "tmux"], before=claude))
    for name, before in [
        ("claude", claude),
        ("codex", codex),
        ("hermes", hermes),
        ("openclaw", openclaw),
        ("every harness", every),
    ]:
        add(
            mk(
                f"uninstall layers {name}",
                ["install", "--uninstall"],
                before=before,
                pre=[[*y, "--router"]],
            )
        )
    add(mk("uninstall nothing installed", ["install", "--uninstall"], before=every))
    add(
        mk(
            "uninstall keeps other config",
            ["install", "--uninstall"],
            before={
                **every,
                ".claude.json": f(json.dumps({"numStartups": 3})),
                ".hermes/config.yaml": f("model: x\n"),
            },
            pre=[y],
        )
    )

    # install --only and --list: write (and remove) only the named components.
    add(mk("install list", ["install", "--list"]))
    add(mk("install list ignores runtimes", ["install", "--list"], before=every))
    add(mk("only warden claude", [*y, "--only", "warden"], before=claude))
    add(mk("only warden codex", [*y, "--only", "warden"], before=codex))
    add(mk("only skill mcp every harness", [*y, "--only", "skill,mcp"], before=every))
    add(mk("only repeated flags", [*y, "--only", "skill", "--only", "mcp"], before=claude))
    add(mk("only capture hermes", [*y, "--only", "capture"], before=hermes))
    add(mk("only instructions openclaw", [*y, "--only", "instructions"], before=openclaw))
    add(mk("only router claude", [*y, "--only", "router"], before=claude))
    add(mk("only router flag and name", [*y, "--only", "router", "--router"], before=claude))
    add(mk("only router flag without name", [*y, "--only", "skill", "--router"], before=claude))
    add(mk("only enforce without warden", [*y, "--only", "skill", "--enforce"], before=claude))
    add(
        mk(
            "only enforce with warden and policy",
            [*y, "--only", "warden", "--enforce"],
            before={**claude, f"{ENVD}/default.json": f(envelope_json(shell_allowlist=["ls"]))},
        )
    )
    add(mk("only unknown", [*y, "--only", "warden,journal"], before=claude))
    add(mk("only empty", [*y, "--only", ""], before=claude))
    add(mk("only dry run", ["install", "--dry-run", "--only", "warden,capture"], before=every))
    add(mk("only twice", [*y, "--only", "warden"], before=claude, pre=[[*y, "--only", "warden"]]))
    add(
        mk(
            "only then uninstall only",
            ["install", "--uninstall", "--only", "capture,instructions"],
            before=claude,
            pre=[y],
        )
    )
    add(
        mk(
            "uninstall only unknown",
            ["install", "--uninstall", "--only", "x"],
            before=claude,
            pre=[y],
        )
    )
    add(
        mk(
            "uninstall only every harness",
            ["install", "--uninstall", "--only", "skill"],
            before=every,
            pre=[y],
        )
    )
    add(
        mk(
            "only with harness note",
            ["install", "--only", "skill", "--harness", "pi", "--dry-run"],
        )
    )


def recipe_cases(add: Any) -> None:
    """`daisugi recipe`: list, show, apply (dry by default) and revert."""
    claude = {".claude": d()}
    with_ws = {**claude, "w": d()}
    ws = "{HOME}/w"
    add(mk("recipe list", ["recipe", "list"]))
    add(mk("recipe list json", ["recipe", "list", "--json"]))
    for name in (
        "bring-your-own",
        "coppice-claude-warden",
        "router-and-routing-mode",
        "warden-only",
    ):
        add(mk(f"recipe show {name}", ["recipe", "show", name]))
    add(mk("recipe show json", ["recipe", "show", "warden-only", "--json"]))
    add(mk("recipe show unknown", ["recipe", "show", "nope"]))
    apply_ws = ["recipe", "apply", "warden-only", "--set", f"workspace={ws}"]
    add(mk("recipe apply dry", apply_ws, before=with_ws))
    add(mk("recipe apply dry json", [*apply_ws, "--json"], before=with_ws))
    add(mk("recipe apply yes", [*apply_ws, "--yes"], before=with_ws))
    add(mk("recipe apply yes json", [*apply_ws, "--yes", "--json"], before=with_ws))
    add(mk("recipe apply twice", [*apply_ws, "--yes"], before=with_ws, pre=[[*apply_ws, "--yes"]]))
    add(
        mk(
            "recipe apply no param skips",
            ["recipe", "apply", "warden-only", "--yes"],
            before=claude,
        )
    )
    add(
        mk(
            "recipe apply missing required",
            ["recipe", "apply", "bring-your-own", "--yes"],
            before=claude,
        )
    )
    add(
        mk(
            "recipe apply unknown set",
            ["recipe", "apply", "warden-only", "--set", "zzz=1"],
            before=claude,
        )
    )
    add(
        mk(
            "recipe apply set without equals",
            ["recipe", "apply", "warden-only", "--set", "zzz"],
            before=claude,
        )
    )
    add(
        mk(
            "recipe apply bad routing mode",
            ["recipe", "apply", "router-and-routing-mode", "--set", "routing_mode=zzz", "--yes"],
            before=claude,
        )
    )
    router = ["recipe", "apply", "router-and-routing-mode", "--yes"]
    knn = {**claude, ".opendaisugi/config.yaml": f("routing_mode: knn\n")}
    add(mk("recipe apply router", router, before=knn))
    add(
        mk(
            "recipe revert dry",
            ["recipe", "revert", "router-and-routing-mode"],
            before=knn,
            pre=[router],
        )
    )
    add(
        mk(
            "recipe revert yes",
            ["recipe", "revert", "router-and-routing-mode", "--yes"],
            before=knn,
            pre=[router],
        )
    )
    add(
        mk(
            "recipe revert warden-only yes",
            ["recipe", "revert", "warden-only", "--yes"],
            before=with_ws,
            pre=[[*apply_ws, "--yes"]],
        )
    )
    # A revert undoes only what the recipe added (the receipt records that).
    old_enforce = (
        "python3 -m opendaisugi.warden --mode enforce --root {HOME}/.opendaisugi/warden "
        "--format claude --verify-timeout 10.0 || exit 2"
    )
    kept_hook = {**with_ws, CLAUDE: f(hook_settings(old_enforce))}
    add(
        mk(
            "recipe revert keeps the earlier enforce hook",
            ["recipe", "revert", "warden-only", "--yes"],
            before=kept_hook,
            pre=[[*apply_ws, "--yes"]],
        )
    )
    preset = {
        **claude,
        ".codex": d(),
        CLAUDE: f(json.dumps({"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"}}, indent=2)),
    }
    add(mk("recipe apply router beside a set base_url", router, before=preset))
    add(
        mk(
            "recipe revert router beside a set base_url",
            ["recipe", "revert", "router-and-routing-mode", "--yes"],
            before=preset,
            pre=[router],
        )
    )
    add(
        mk(
            "recipe revert router beside a set base_url dry",
            ["recipe", "revert", "router-and-routing-mode"],
            before=preset,
            pre=[router],
        )
    )
    receipt = json.dumps(
        {
            "recipe": "warden-only",
            "applied_at": "2026-10-31T00:00:00Z",
            "params": {"workspace": "{HOME}/w"},
            "completed": 3,
            "steps": [
                {"index": 1, "kind": "warden-init", "undo": {"remove_envelope": "default"}},
                {
                    "index": 2,
                    "kind": "install",
                    "undo": {"uninstall": {"claude": ["warden"]}},
                },
                {"index": 3, "kind": "note", "undo": None},
            ],
        },
        indent=2,
    )
    unread = hook_settings(
        old_enforce.replace("--mode enforce", "--mode enforce") + " && echo 'unterminated"
    )
    held = {
        **with_ws,
        ".opendaisugi/recipes/warden-only.json": f(receipt + "\n"),
        ".opendaisugi/warden/envelopes/default.json": f(envelope_json(id="env_fixed01")),
        CLAUDE: f(unread),
    }
    add(
        mk(
            "recipe revert refused keeps the receipt",
            ["recipe", "revert", "warden-only", "--yes"],
            before=held,
        )
    )
    # A skipped step keeps the undo an earlier run recorded.
    add(
        mk(
            "recipe apply again without the param keeps the earlier undo",
            ["recipe", "apply", "warden-only", "--yes"],
            before=with_ws,
            pre=[[*apply_ws, "--yes"]],
        )
    )
    # A failing re-run does not cut the receipt short.
    stuck = {
        "w": d(),
        ".claude": d(0o555),
        ".opendaisugi/recipes/warden-only.json": f(receipt + "\n"),
    }
    add(mk("recipe apply fails and keeps the later undo", [*apply_ws, "--yes"], before=stuck))
    add(
        mk(
            "recipe apply after install",
            [*apply_ws, "--yes"],
            before=with_ws,
            pre=[["install", "--yes"]],
        )
    )
    old_receipt = json.dumps(
        {
            "recipe": "warden-only",
            "applied_at": "2026-10-31T00:00:00Z",
            "params": {},
            "completed": 3,
            "steps": [
                {
                    "index": 2,
                    "kind": "install",
                    "undo": {"uninstall_only": ["warden"], "runtime": ["claude"]},
                }
            ],
        },
        indent=2,
    )
    old_held = {**claude, ".opendaisugi/recipes/warden-only.json": f(old_receipt + "\n")}
    for flag in ([], ["--yes"]):
        add(
            mk(
                "recipe revert old format receipt" + (" yes" if flag else " dry"),
                ["recipe", "revert", "warden-only", *flag],
                before=old_held,
            )
        )
    mode = ["recipe", "apply", "router-and-routing-mode", "--yes", "--set"]
    add(
        mk(
            "recipe revert after two modes restores the first",
            ["recipe", "revert", "router-and-routing-mode", "--yes"],
            before={**claude, ".opendaisugi/config.yaml": f("routing_mode: rules\n")},
            pre=[[*mode, "routing_mode=knn"], [*mode, "routing_mode=off"]],
        )
    )
    add(mk("recipe revert no receipt", ["recipe", "revert", "warden-only", "--yes"], before=claude))
    add(mk("recipe apply no runtime", ["recipe", "apply", "warden-only", "--yes"]))
    add(
        mk(
            "recipe apply router no runtime",
            ["recipe", "apply", "router-and-routing-mode", "--yes"],
        )
    )


def replay_cases(add: Any) -> None:
    """`warden replay`: a captured session through the warden, offline."""

    def cap(**kw: Any) -> str:
        r: dict[str, Any] = {"captured_at": 1.5, "session_id": "s1"}
        r.update(kw)
        return json.dumps(r)

    mixed = (
        "\n".join(
            [
                cap(tool_name="Bash", step_type="shell", command="git status"),
                cap(tool_name="Bash", step_type="shell", command="rm -rf /"),
                cap(tool_name="Bash", step_type="shell", command="ls && git log"),
                cap(tool_name="Read", step_type="file_read", path="/work/a.txt"),
                cap(tool_name="Read", step_type="file_read", path="/etc/passwd"),
                cap(tool_name="Write", step_type="file_write", path="/work/b.txt", content_len=3),
                cap(tool_name="Write", step_type="file_write", path='/tmp/it\'s "q" é'),
                cap(tool_name="WebFetch", step_type="network", url="https://example.com/x"),
                cap(
                    tool_name="mcp__fs__read",
                    step_type="mcp",
                    mcp_server="fs",
                    mcp_tool="read",
                    arguments={"p": 1},
                ),
                "",
                "   ",
                "not json",
            ]
        )
        + "\n"
    )
    import yaml

    # The envelope in the form yaml.safe_dump writes, which every reader
    # takes; a JSON envelope is a row-12 input (see "replay json envelope").
    env = yaml.safe_dump(json.loads(envelope_json()), sort_keys=False)
    base = {"caps.jsonl": f(mixed), "env.json": f(env)}
    argv = ["warden", "replay", "caps.jsonl", "--envelope", "env.json"]
    # A rule regex is one small subset; a value that is not valid UTF-8 text
    # (a lone surrogate) denies before matching.
    sur = chr(0xD800)
    rx_rules = [
        {"tool": "t/flag", "args": [{"op": "matches", "path": "a", "regex": "(?x)a|.*"}]},
        {"tool": "t/pname", "args": [{"op": "matches", "path": "a", "regex": "\\p{Letter}"}]},
        {"tool": "t/diff", "args": [{"op": "not_matches", "path": "a", "regex": "[!--/]"}]},
        {"tool": "t/evil", "args": [{"op": "not_matches", "path": "a", "regex": "^evil$"}]},
        {"tool": "t/any", "args": [{"op": "matches", "path": "a", "regex": ".*"}]},
        {"tool": "t/ok", "args": [{"op": "matches", "path": "a", "regex": "^[a-z]+$"}]},
    ]
    rx_env = envelope_json(mcp_rules=rx_rules)
    rx_caps = "\n".join(
        [
            cap(
                tool_name="mcp__t__flag",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="flag",
                arguments={"a": "evil"},
            ),
            cap(
                tool_name="mcp__t__pname",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="pname",
                arguments={"a": "a"},
            ),
            cap(
                tool_name="mcp__t__diff",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="diff",
                arguments={"a": "#"},
            ),
            cap(
                tool_name="mcp__t__evil",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="evil",
                arguments={"a": sur},
            ),
            cap(
                tool_name="mcp__t__any",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="any",
                arguments={"a": sur},
            ),
            cap(
                tool_name="mcp__t__evil",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="evil",
                arguments={"a": "good" + chr(0xFFFD)},
            ),
            cap(
                tool_name="mcp__t__evil",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="evil",
                arguments={"a": "evil"},
            ),
            cap(
                tool_name="mcp__t__ok",
                step_type="mcp",
                mcp_server="t",
                mcp_tool="ok",
                arguments={"a": "fine"},
            ),
        ]
    )
    rx_argv = ["warden", "replay", "caps.jsonl", "--envelope", "env.json"]
    rx_before = {"caps.jsonl": f(rx_caps + "\n"), "env.json": f(rx_env)}
    # A capture whose arguments are no object stays as the host sent it, so an
    # argument rule reads it (hook._synthesize_steps keeps the raw value).
    na_env = envelope_json(
        mcp_allowlist=["t/*"],
        mcp_rules=[{"tool": "t/r", "args": [{"op": "exists", "path": "a"}]}],
    )
    na_caps = "\n".join(
        cap(tool_name="mcp__t__r", step_type="mcp", mcp_server="t", mcp_tool="r", arguments=a)
        for a in ([1, 2], "no", None, {}, {"a": 1})
    )
    add(
        mk(
            "replay mcp arguments not an object",
            rx_argv,
            before={"caps.jsonl": f(na_caps + "\n"), "env.json": f(na_env)},
        )
    )
    add(mk("replay mcp regex subset", rx_argv, before=rx_before))
    add(mk("replay mcp regex subset json", [*rx_argv, "--json"], before=rx_before))
    add(mk("replay mixed", argv, before=base))
    add(mk("replay mixed json", [*argv, "--json"], before=base))
    yaml_env = (
        "generated_by: cli_cases\ntask: synthetic\npermissions:\n  file_read: ['/work/**']\n  shell: true\n"
        "  shell_allowlist: [git]\n  network: true\n  network_allowlist: [example.com]\n"
    )
    add(mk("replay yaml envelope", argv, before={"caps.jsonl": f(mixed), "env.json": f(yaml_env)}))
    add(
        mk(
            "replay json envelope",
            argv,
            before={"caps.jsonl": f(mixed), "env.json": f(envelope_json())},
        )
    )
    odd = (
        "\n".join(
            [
                cap(tool_name="Bash", command="ls"),
                cap(tool_name="Todo", step_type="todo"),
                cap(tool_name=None, step_type=5),
                cap(tool_name="Bash", step_type="shell", command=7),
                cap(tool_name="Bash", step_type="shell"),
                cap(tool_name="Read", step_type="file_read", path=None),
                cap(tool_name="WebFetch", step_type="network", url=["x"]),
                cap(tool_name="m", step_type="mcp", arguments="no"),
                json.dumps({"tool_name": "Bash", "step_type": "shell", "command": "ls"}),
                json.dumps(
                    {
                        "tool_name": "Bash",
                        "step_type": "shell",
                        "command": "ls",
                        "captured_at": "t",
                        "session_id": 3,
                    }
                ),
            ]
        )
        + "\n"
    )
    add(mk("replay odd records", argv, before={"caps.jsonl": f(odd), "env.json": f(env)}))
    add(
        mk(
            "replay odd records json",
            [*argv, "--json"],
            before={"caps.jsonl": f(odd), "env.json": f(env)},
        )
    )
    add(mk("replay empty", argv, before={"caps.jsonl": f(""), "env.json": f(env)}))
    add(
        mk("replay empty json", [*argv, "--json"], before={"caps.jsonl": f(""), "env.json": f(env)})
    )
    add(mk("replay array line", argv, before={"caps.jsonl": f("[1]\n"), "env.json": f(env)}))
    add(mk("replay no captures file", argv, before={"env.json": f(env)}))
    add(mk("replay no envelope file", argv, before={"caps.jsonl": f(mixed)}))
    add(
        mk(
            "replay bad envelope",
            argv,
            before={"caps.jsonl": f(mixed), "env.json": f('{"task": "t"}')},
        )
    )
    add(
        mk(
            "replay envelope not a mapping",
            argv,
            before={"caps.jsonl": f(mixed), "env.json": f("- 1\n")},
        )
    )
    add(
        mk(
            "replay no envelope option",
            ["warden", "replay", "caps.jsonl"],
            before={"caps.jsonl": f(mixed)},
        )
    )
    add(mk("replay no argument", ["warden", "replay"]))
    add(
        mk(
            "replay many denies",
            argv,
            before={
                "caps.jsonl": f(
                    "".join(
                        cap(tool_name="Bash", step_type="shell", command=f"rm {i}") + "\n"
                        for i in range(30)
                    )
                ),
                "env.json": f(env),
            },
        )
    )


def graft_cases(add: Any) -> None:
    """`daisugi graft install|status|remove`: the rule file in the warden
    root, and the refusal beside a PreToolUse hook that may rewrite input
    (GR-3). Every settings file is in the scratch HOME."""
    warden_cmd = "/usr/bin/python3 -m opendaisugi.warden --mode audit --format claude"
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
        "install beside warden and capture",
        ["install"],
        before={
            CLAUDE: f(hook_settings(warden_cmd)),
            ".codex/hooks.json": f(hook_settings(record)),
        },
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
        before={"proj/.claude/settings.json": f(hook_settings(warden_cmd))},
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
        "install warden hook unread form",
        ["install"],
        before={CLAUDE: f(hook_settings("eval python -m opendaisugi.warden"))},
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
# parsed (a warden hook command compared by its flags), and a backup's
# time stamp dropped.
#
# A case may carry `go_expect`: fields where the binary's answer differs
# from the oracle's by a ruling (clients/ADJUDICATIONS.md, C-10 on). The
# generator derives them from the oracle's answer, so the compare stays
# exact. A `live` case depends on the box (its memory size): the compare
# runs the oracle again for it.

RICH_CONFIG = ".opendaisugi/config.yaml"
_WARN_LINE = re.compile(r"^.*?:\d+: \w*Warning: (.*)$")
_WARN_DONE = re.compile(r"^\w*Warning: (.*)$")
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
        # garden_cases.norm_stderr has already cut the file and the source
        # line: `UserWarning: text` is the binary's `warning: text`.
        m = _WARN_DONE.match(line)
        if m:
            out.append("warning: " + m.group(1))
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
    if case.get("live_warden"):
        # A warden that answers: a socket that listens while the command runs.
        live_path = home / case["live_warden"]
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
            (home / case["live_warden"]).unlink(missing_ok=True)
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
    """G2-4: the binary starts its own `warden serve` where the oracle
    starts Python's; the compare runs it behind a wrapper that logs that
    spawn as the oracle's fake spawn logs its own."""
    exp = case["expect"]
    if case.get("go_refuses") or exp["exit"] == 2:
        return None
    spawned = []
    for argv in exp.get("spawned") or []:
        # {PYTHON} -c <shim> warden serve --root R
        spawned.append(["{DAISUGI}", *argv[argv.index("warden") :]])
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
    # DH-R-1: the data home rule. Each branch reads config.yaml from the
    # directory the rule picks, and models use writes there.
    od_env = {"OPENDAISUGI_HOME": "{HOME}/od", "XDG_DATA_HOME": "{HOME}/xdg"}
    xdg_env = {"XDG_DATA_HOME": "{HOME}/xdg"}
    enforce = {"text": "warden_mode: enforce\n"}
    add(rich("data home neither", ["config", "--json"]))
    add(rich("data home od", ["config", "--json"], env=od_env, before={"od/config.yaml": enforce}))
    add(
        rich(
            "data home xdg no dot",
            ["config", "--json"],
            env=xdg_env,
            before={"xdg/opendaisugi/config.yaml": enforce},
        )
    )
    add(
        rich(
            "data home xdg with dot",
            ["config", "--json"],
            env=xdg_env,
            before={
                RICH_CONFIG: {"text": "warden_mode: audit\n"},
                "xdg/opendaisugi/config.yaml": enforce,
            },
        )
    )
    add(rich("data home od models use", ["models", "use", "acme/m"], env=od_env))
    add(rich("data home xdg models use", ["models", "use", "acme/m"], env=xdg_env))
    # A leading ~ is the home; a value still not absolute is ignored and the
    # next rule applies.
    for label, value in (("tilde", "~/od"), ("relative", "od"), ("dot", "./od"), ("empty", "")):
        add(
            rich(
                f"data home od {label}",
                ["config", "--json"],
                env={"OPENDAISUGI_HOME": value},
                before={"od/config.yaml": enforce},
            )
        )
        add(
            rich(
                f"data home xdg {label}",
                ["config", "--json"],
                env={"XDG_DATA_HOME": value.replace("od", "xdg")},
                before={"xdg/opendaisugi/config.yaml": enforce},
            )
        )
    add(
        rich(
            "data home od relative models use",
            ["models", "use", "acme/m"],
            env={"OPENDAISUGI_HOME": "od"},
        )
    )
    values = (
        "model: m\nmax_task_chars: '1_000'\nz3_timeout_ms: 800.0\ndata_dir: ~/x//y/./\nauto_tend: yes\n"
        "router_local_model: null\nshell_allow_decomposition: 'on'\nwarden_mode: enforce\nmatcher_model: potion\n"
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
        rich(
            "config invalid bool", ["config"], before={RICH_CONFIG: {"text": "warden_ask: maybe\n"}}
        )
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
    profiles = (
        "agent_profiles:\n  oai: {wire: openai, key_env: OAI_KEY}\n  desk: {wire: anthropic}\n"
    )
    add(rich("config agent profiles", ["config"], before={RICH_CONFIG: {"text": profiles}}))
    add(
        rich(
            "config agent profiles json",
            ["config", "--json"],
            before={RICH_CONFIG: {"text": profiles}},
        )
    )
    add(
        rich(
            "config agent profiles null",
            ["config"],
            before={RICH_CONFIG: {"text": "agent_profiles:\n"}},
        )
    )
    add(
        rich(
            "config agent profiles list",
            ["config"],
            before={RICH_CONFIG: {"text": "agent_profiles: [1]\n"}},
        )
    )
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
            before={CLAUDE: audit, RICH_CONFIG: {"text": "warden_mode: enforce\n"}},
        )
    )
    add(
        rich(
            "config file mode", ["config"], before={RICH_CONFIG: {"text": "warden_mode: enforce\n"}}
        )
    )

    # -- status --------------------------------------------------------------
    st = {**GPU}
    add(rich("status fresh", ["status"], before=st, env=FAKE_PATH))
    add(rich("status fresh json", ["status", "--json"], before=st, env=FAKE_PATH))
    add(rich("status lexical", ["status"], before={**st, **lexical}, env=FAKE_PATH))
    fence_off = {
        ".opendaisugi/warden/envelopes/default.json": {"text": envelope_json()},
        ".config/coppice/coppice.toml": {"text": 'fence = "off"\n'},
    }
    add(
        rich(
            "status fence off notice",
            ["status"],
            before={**st, **lexical, **fence_off},
            env=FAKE_PATH,
        )
    )
    add(
        rich(
            "status fence off notice json",
            ["status", "--json"],
            before={**st, **lexical, **fence_off},
            env=FAKE_PATH,
        )
    )
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
            before={**st, RICH_CONFIG: {"text": "warden_ask: maybe\n"}},
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
    # whose result holds a dialect audit warning) and calls in the warden log.
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
            before={**sj, RICH_CONFIG: {"text": "warden_ask: maybe\n"}},
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
            "journal parse disarmed warden",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before={**tfile, ".opendaisugi/warden/DISARMED": {"text": "x"}},
        )
    )
    add(
        rich(
            "journal parse enforce config",
            ["journal", "parse", "t.jsonl", "-o", "eps.yaml"],
            before={**tfile, RICH_CONFIG: {"text": "warden_mode: enforce\nllm_backend: api\n"}},
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
            {"id": "s1", "type": "shell", "command": "ls -la /work/a.txt", "depends_on": []},
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
    # PW-6: steps given as strings, decoded as coerce_step decodes them.
    v_str = {
        **v_plan,
        "steps": [
            '{"id": "s1", "type": "shell", "command": "ls -la /work/a.txt", "depends_on": []}',
            r"""{'id': 's2', 'type': 'file_read', 'path': '/work/' "a.txt", 'depends_on': ('s1',),"""
            r""" 'metadata': {'n': 0x1F, 'big': 123456789012345678901234567890, 'ok': True}}""",
        ],
    }
    v_str_bad = {**v_plan, "steps": ["ls -la /work/a.txt"]}
    # B1: an int of more than 4300 decimal digits does not decode (Python's
    # int limit), so the step stays a str.
    many = "1" * 5000
    v_str_long = {
        **v_plan,
        "steps": [
            "{'id': 's1', 'type': 'shell', 'command': 'ls -la /work/a.txt', 'metadata': {'n': "
            + many
            + "}}"
        ],
    }
    v_json_long = {
        **v_plan,
        "steps": [
            '{"id": "s1", "type": "shell", "command": "ls -la /work/a.txt", "metadata": {"n": '
            + many
            + "}}"
        ],
    }
    v_str_warn = {**v_plan, "steps": [r"{'id': 's1', 'type': 'shell', 'command': 'ls /work/\d'}"]}
    # PW-6: numbers past 64 bits in a plan and an envelope.
    big = 123456789012345678901234567890
    v_big_plan = {
        **v_plan,
        "steps": [
            {**v_plan["steps"][0], "metadata": {"n": big, "l": [-big, 1]}},
            {"id": "s3", "type": "sim_reset", "seed": big, "depends_on": []},
            {"id": "s4", "type": "vla", "task": "pick", "max_actions": big, "depends_on": []},
        ],
    }
    v_big_env = {
        **v_env,
        "permissions": {**v_perm, "max_output_size_mb": big, "max_execution_time_s": big},
        "postconditions": [
            {"type": "exit_code", "expected": -big},
            {"type": "file_size_range", "path": "/work/a", "min": big, "max": -big},
        ],
    }
    for name, before, extra in [
        ("big ints", {**vdoc("p.yaml", v_big_plan), **vdoc("e.yaml", v_big_env)}, []),
        ("big ints json", {**vdoc("p.yaml", v_big_plan), **vdoc("e.yaml", v_big_env)}, ["--json"]),
        ("string steps", {**both, **vdoc("p.yaml", v_str)}, ["--json"]),
        ("string step not a step", {**both, **vdoc("p.yaml", v_str_bad)}, []),
        ("string step int too long", {**both, **vdoc("p.yaml", v_str_long)}, []),
        ("json step int too long", {**both, **vdoc("p.yaml", v_json_long)}, []),
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
    # A rule regex is one small subset; a value that is not valid UTF-8 text
    # denies before matching (the standalone verifier reads the same rules).
    vrx = [
        ("flag", "matches", "(?x)a|.*", "evil"),
        ("pname", "matches", chr(92) + "p{Letter}", "a"),
        ("diff", "not_matches", "[!--/]", "#"),
        ("diffok", "not_matches", "[!-/]", "#"),
        ("dashes", "matches", "[a--b]", "a"),
        ("range", "matches", "[a-b]", "a"),
        ("word", "matches", chr(92) + "w+", "a"),
        ("caseless", "matches", "(?i)a", "A"),
        ("evil", "not_matches", "^evil$", chr(0xD800)),
        ("evilok", "not_matches", "^evil$", "fine"),
        ("any", "matches", ".*", "x" + chr(0xDFFF)),
    ]
    vrx_env = {
        **v_env,
        "permissions": {
            **v_perm,
            "mcp_allowlist": ["t/*"],
            "mcp_rules": [
                {"tool": f"t/{n}", "args": [{"op": op, "path": "a", "regex": rx}]}
                for n, op, rx, _ in vrx
            ],
        },
    }
    for n, _op, _rx, val in vrx:
        vrx_plan = {
            **v_plan,
            "steps": [
                {
                    "id": "s1",
                    "type": "mcp",
                    "server": "t",
                    "tool": n,
                    "arguments": {"a": val},
                    "depends_on": [],
                }
            ],
        }
        add(
            rich(
                f"verify mcp regex {n}",
                ["verify", "p.yaml", "--envelope", "e.yaml"],
                before={**vdoc("p.yaml", vrx_plan), **vdoc("e.yaml", vrx_env)},
            )
        )
    # B2: PyYAML's int() reads at most 4300 decimal digits; past that the
    # plan or envelope does not parse (a plan file in JSON is read as YAML).
    many = "1" * 5000

    def with_n(obj: dict[str, Any], text: str) -> str:
        return _yaml.safe_dump(obj, sort_keys=False).replace("NNN", text)

    n_plan = {**v_plan, "steps": [{**v_plan["steps"][0], "metadata": {"n": "NNN"}}]}
    for name, files, argv in [
        ("yaml plan int too long", {"p.yaml": {"text": with_n(n_plan, many)}}, "p.yaml"),
        (
            "yaml plan int too long underscores",
            {"p.yaml": {"text": with_n(n_plan, "1_" + many)}},
            "p.yaml",
        ),
        ("yaml plan int at the limit", {"p.yaml": {"text": with_n(n_plan, "1" * 4300)}}, "p.yaml"),
        (
            "yaml plan hex int long",
            {"p.yaml": {"text": with_n(n_plan, "0x" + "f" * 5000)}},
            "p.yaml",
        ),
        (
            "json plan int too long",
            {"p.json": {"text": json.dumps(n_plan).replace('"NNN"', many)}},
            "p.json",
        ),
        (
            "yaml envelope int too long",
            {
                "e.yaml": {
                    "text": with_n({**v_env, "max_execution_time_s": "NNN"}, many),
                }
            },
            "p.yaml",
        ),
    ]:
        add(
            rich(
                f"verify {name}",
                ["verify", argv, "--envelope", "e.yaml"],
                before={**both, **files},
            )
        )
    # B3: Python compares ints exactly, and an int with a float exactly.
    p53, p64 = 2**53, 2**64

    def inv_on_n(pred: dict[str, Any]) -> dict[str, Any]:
        return {
            **v_env,
            "invariants": [
                {
                    "type": "big_n",
                    "description": "n holds",
                    "expr": {"op": "forall_steps", "pred": {**pred, "path": "metadata.n"}},
                }
            ],
        }

    for name, n, pred in [
        ("2^64+5 equals 2^64", p64 + 5, {"op": "equals", "value": p64}),
        ("2^64+5 in set 2^64", p64 + 5, {"op": "in_set", "values": [p64]}),
        ("-(2^64+5) equals -2^64", -(p64 + 5), {"op": "equals", "value": -p64}),
        ("2^53+1 equals 2^53", p53 + 1, {"op": "equals", "value": p53}),
        ("-(2^53+1) equals -2^53", -(p53 + 1), {"op": "equals", "value": -p53}),
        ("2^64 not equals 2^64+5", p64, {"op": "not_equals", "value": p64 + 5}),
        ("2^64 not in set 2^64+5", p64, {"op": "not_in_set", "values": [p64 + 5]}),
        ("2^64 equals 2^63-1", p64, {"op": "equals", "value": 2**63 - 1}),
        ("2^53+1 equals float 2^53", p53 + 1, {"op": "equals", "value": float(p53)}),
        ("2^53 equals float 2^53", p53, {"op": "equals", "value": float(p53)}),
        ("2^53+1 not equals float 2^53", p53 + 1, {"op": "not_equals", "value": float(p53)}),
        ("2^64+5 in set float 2^64", p64 + 5, {"op": "in_set", "values": [float(p64)]}),
        ("2^64 equals float 2^64", p64, {"op": "equals", "value": float(p64)}),
        ("float 2^53 equals 2^53+1", float(p53), {"op": "equals", "value": p53 + 1}),
    ]:
        plan = {**v_plan, "steps": [{**v_plan["steps"][0], "metadata": {"n": n}}]}
        add(
            rich(
                f"verify big int {name}",
                ["verify", "p.yaml", "--envelope", "e.yaml"],
                before={**vdoc("p.yaml", plan), **vdoc("e.yaml", inv_on_n(pred))},
            )
        )

    # RF-14: a skill's contract is proved inside the caller's envelope by
    # Z3, whose numerals Python builds exactly (IntVal(int), RealVal(str(f))).
    def deleg(outer_pred: dict[str, Any], inner_pred: dict[str, Any]) -> dict[str, Any]:
        def inv(pred: dict[str, Any]) -> dict[str, Any]:
            return {
                "type": "n_rule",
                "description": "d",
                "expr": {
                    "op": "forall_steps",
                    "pred": {
                        "op": "implies",
                        "a": {"op": "equals", "path": "type", "value": "shell"},
                        "b": {**pred, "path": "metadata.n"},
                    },
                },
            }

        inner = {**v_env, "id": "env_skill", "invariants": [inv(inner_pred)]}
        plan = {
            **v_plan,
            "steps": [{"id": "s1", "type": "skill", "skill_id": "k", "contract_envelope": inner}],
        }
        return {
            **vdoc("p.yaml", plan),
            **vdoc("e.yaml", {**v_env, "invariants": [inv(outer_pred)]}),
        }

    p53, p60 = 2**53, 2**60
    big = 1000000000000000019884624838656  # the float 1e30's exact value
    for name, outer, inner in [
        (
            "equals 2^53 vs 2^53+1",
            {"op": "equals", "value": p53},
            {"op": "equals", "value": p53 + 1},
        ),
        ("equals same", {"op": "equals", "value": p53 + 1}, {"op": "equals", "value": p53 + 1}),
        (
            "in set 2^53 vs 2^53+1",
            {"op": "in_set", "values": [p53]},
            {"op": "in_set", "values": [p53 + 1]},
        ),
        (
            "not in set 2^53+1 vs 2^53",
            {"op": "not_in_set", "values": [p53 + 1]},
            {"op": "not_in_set", "values": [p53]},
        ),
        (
            "numeric range max 2^53 vs 2^53+1",
            {"op": "numeric_range", "min": 0, "max": p53},
            {"op": "numeric_range", "min": 0, "max": p53 + 1},
        ),
        (
            "numeric range max 2^53 vs 2^53+2",
            {"op": "numeric_range", "min": 0, "max": p53},
            {"op": "numeric_range", "min": 0, "max": p53 + 2},
        ),
        (
            "in set float 2^60 text vs 2^60",
            {"op": "in_set", "values": [1152921504606847000]},
            {"op": "equals", "value": p60},
        ),
        (
            "range 1e30 vs 2^63-1",
            {"op": "numeric_range", "min": 0, "max": 1e30},
            {"op": "equals", "value": 2**63 - 1},
        ),
        (
            "range 1e19 vs 2^64-1",
            {"op": "numeric_range", "min": 0, "max": 1e19},
            {"op": "equals", "value": 2**64 - 1},
        ),
        (
            "range 1e30 vs past 64 bits",
            {"op": "numeric_range", "min": 0, "max": 1e30},
            {"op": "equals", "value": big},
        ),
    ]:
        add(
            rich(
                f"verify delegation {name}",
                ["verify", "p.yaml", "--envelope", "e.yaml"],
                before=deleg(outer, inner),
            )
        )
    # A length_range bound with a fractional part is pydantic's
    # ValidationError (a traceback); the ports refuse it (K2-8, RF-13).
    for name, pred in [
        ("min", {"op": "length_range", "path": "command", "min": 2.5}),
        ("max", {"op": "length_range", "path": "command", "min": 0, "max": 1.5}),
    ]:
        env = {
            **v_env,
            "invariants": [
                {
                    "type": "len",
                    "description": "d",
                    "expr": {"op": "forall_steps", "pred": pred},
                }
            ],
        }
        add(
            rich(
                f"verify length range fractional {name}",
                ["verify", "p.yaml", "--envelope", "e.yaml"],
                before={**both, **vdoc("e.yaml", env)},
                go_refuses=True,
            )
        )
    add(
        rich(
            "verify string step warns",
            ["verify", "p.yaml", "--envelope", "e.yaml"],
            before={**both, **vdoc("p.yaml", v_str_warn)},
            go_refuses=True,
        )
    )

    # `daisugi hook report`: one pane state event on stdin. The warden root's
    # parent is a file here, so the session tree is not written (it is
    # best effort); the warden_server suite cases the tree through warden.sock.
    blocked = {"blocked": {"text": "not a directory\n"}}
    root = ["--root", "{HOME}/blocked/warden"]
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
        ("warden source", json.dumps({**ev, "source": "warden"}), []),
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
    # The refusal is the same in a dry run: a plan must not promise a hook it cannot write.
    add(
        rich("start relative data dir dry run", ["start", "--dry-run", "--data-dir", "../dd"], **so)
    )
    pdir = "proj/.claude/settings.json"
    add(rich("start again", ["start", "--no-ui"], twice=True, **so))
    add(rich("start again enforce", ["start", "--no-ui", "--enforce"], twice=True, **so))
    add(
        rich(
            "start audit then enforce",
            ["start", "--no-ui"],
            argv2=["start", "--no-ui", "--enforce"],
            twice=True,
            **so,
        )
    )
    add(
        rich(
            "start audit then enforce dry run",
            ["start", "--no-ui"],
            argv2=["start", "--dry-run", "--enforce"],
            twice=True,
            **so,
        )
    )
    add(
        rich(
            "start enforce then plain keeps enforce",
            ["start", "--no-ui", "--enforce"],
            argv2=["start", "--no-ui"],
            twice=True,
            **so,
        )
    )
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
            before={".opendaisugi/warden/envelopes/proj-{CWD8}.json": {"text": "{}"}},
            **so,
        )
    )
    # G2-9: a warden is running when its socket answers, not when a file is
    # there. The runner holds a listening socket at `live_warden` while the
    # command runs. Its directory is in `before`, so a binary that refuses
    # `start` is seen to leave the tree as it was.
    live = {"before": {".opendaisugi/warden": {"dir": True}}, "live_warden": WARDEN_SOCK}
    add(rich("start server running", ["start", "--no-ui"], **live, **so))
    add(rich("start server running dry run", ["start", "--dry-run"], **live, **so))
    add(rich("start stale socket", ["start", "--no-ui"], before={WARDEN_SOCK: {"text": ""}}, **so))
    add(
        rich(
            "start stale socket dry run",
            ["start", "--dry-run"],
            before={WARDEN_SOCK: {"text": ""}},
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
    _, argv, env, cwd = prepare(again, work, cmd)
    cmd = entry_cmd(case, cmd)
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
        "--rerun",
        help="with --keep, run again the cases whose name contains this text "
        "(or any of several, separated by commas)",
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
    kept_by_name: dict[str, dict[str, Any]] = {}
    if (args.out / "cases.jsonl").exists():
        for ln in (args.out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip() and json.loads(ln).get("name") in BOX_DEPENDENT:
                kept_by_name[json.loads(ln)["name"]] = json.loads(ln)
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
        if args.rerun and any(t in c["name"] for t in args.rerun.split(",")):
            prev = None
        if c["name"] in kept_by_name and not args.only:
            prev = kept_by_name[c["name"]]
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
        "kept": sorted(n for n in BOX_DEPENDENT if n in kept_by_name),
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
