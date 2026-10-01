"""The fence's rule list: what a harness may reach under Landlock.

``daisugi fence`` wraps any agent harness in a Landlock ruleset (FN-1). The
kernel half, which applies a ruleset and executes the harness, lives in the
Go and Rust clients only. This module is the oracle for the other half: a
pure function from the inputs to the list of rules. It reads no file and
asks the kernel nothing, so a golden case is the same on every box. The
Go and Rust clients must print the same list (``clients/fence_cases.py``).

A rule is a path and a mode. Mode ``ro`` is read and execute. Mode ``rw``
is every file-system right (read, write, execute, create, remove, rename,
truncate, device ioctl) except the right to connect to a unix socket.
Mode ``unix`` is only that right: connect to a socket under the path.
``sock`` is ``ro`` and ``unix``, and ``rw+unix`` is ``rw`` and ``unix``.
Landlock only grants: a path no rule covers is unreachable. So the work
here is to pick the grants, and to refuse or drop a grant that would also
open a path that must stay shut.

The grants (FN-3):

- the system: /usr /bin /sbin /lib* /etc /opt /proc /sys read and execute,
  /run/systemd/resolve read (a resolv.conf link points there), the null,
  zero, random, urandom, tty and ptmx devices and /dev/pts read and write;
- the workspace, each work dir and the fence's private temp dir, read and
  write (/tmp is not granted: FN-R-14);
- the harness's own paths from one table (``HARNESS``): read only, with
  read and write on the parts the harness writes (FN-R-13);
- each directory on PATH, as the entry itself, read and execute, and the
  directory of the harness's own binary and of this binary;
- the gate root and config.yaml of each data dir, read only. The gate
  root also takes ``unix``, so the fenced hook reaches the resident gate on
  ``<root>/gate.sock``; that server, outside the fence, makes every write
  (FN-R-12);
- the directory of the coppice socket, ``unix`` only;
- the envelope's file_read and file_write globs, each cut to its longest
  literal directory (the widening is FN-R-4);
- ``--allow PATH[:rw]``, read only unless it says ``:rw``.

Never reachable unless named by ``--allow`` (``SECRETS`` under each HOME,
and the coppice directories): an ``--allow`` that is the secret itself, or
inside it, names it and stands; one above it is refused. The workspace, a
work dir, the temp dir and a harness path that is, holds or is inside a
secret are refused: only an explicit ``--allow`` opens a secret (FN-R-11),
and one of them that lies inside a standing ``--allow`` stands with it.
An ambient grant (a PATH entry) or an envelope prefix that touches a secret
is dropped. A grant that would let the agent write the gate's state is
refused, named or not. The one exception is the fence's own temp dir: a
leaf directly under ``<data dir>/fence-tmp/`` or ``<coppice dir>/fence-tmp/``.

Paths come in absolute and are joined and normalized as text. The caller
resolves symlinks first: ``real`` maps a harness path or an envelope prefix
to its resolved spelling, and ``never_extra`` and ``state_extra`` carry
every extra spelling of a protected path.
"""

from __future__ import annotations

import posixpath
from typing import Any

# Paths under HOME that hold keys and tokens. One list for the fence.
SECRETS = (
    ".ssh",
    ".gnupg",
    ".aws",
    ".azure",
    ".kube",
    ".docker",
    ".config/gh",
    ".config/gcloud",
    ".config/rclone",
    ".config/hub",
    ".config/doctl",
    ".config/op",
    ".config/Bitwarden",
    ".config/google-chrome",
    ".config/chromium",
    ".config/BraveSoftware",
    ".config/Code",
    ".config/git/credentials",
    ".config/sops",
    ".config/age",
    ".config/huggingface",
    ".config/tailscale",
    ".mozilla",
    ".thunderbird",
    ".age",
    ".pki",
    ".ollama",
    ".cache/huggingface/token",
    ".netrc",
    ".npmrc",
    ".pypirc",
    ".git-credentials",
    ".password-store",
    ".local/share/keyrings",
    ".cargo/credentials",
    ".cargo/credentials.toml",
    ".gem/credentials",
    ".vault-token",
    ".terraform.d/credentials.tfrc.json",
)

# What a harness keeps for itself under HOME: (path under HOME, mode). A
# harness's config, hooks, plugins and extensions stay read only, so the
# agent cannot plant code that runs later outside any fence (FN-R-13).
# ~/.claude.json (mcpServers, per-project settings) and OpenCode's config,
# binaries and plugin packages are read only too (FN-R-22); the harness may
# warn that it cannot save them, and --allow PATH:rw opens one. What stays
# read and write is data the harness writes as it runs: transcripts,
# todos, plans, logs, history, file history, IDE locks, the login token
# (refreshed in place), and the shell snapshots and session env that only
# that session sources. OpenCode's snapshot repos are read only: OpenCode
# runs git on them later, outside any fence (FN-R-31). Claude's projects
# dir is not in the table: only this workspace's own project dir is read
# and write (plan, FN-R-31), so the agent cannot write the auto memory a
# later session in another project loads.
HARNESS: dict[str, tuple[tuple[str, str], ...]] = {
    "claude": (
        (".claude", "ro"),
        (".claude/todos", "rw"),
        (".claude/shell-snapshots", "rw"),
        (".claude/statsig", "rw"),
        (".claude/session-env", "rw"),
        (".claude/file-history", "rw"),
        (".claude/plans", "rw"),
        (".claude/debug", "rw"),
        (".claude/ide", "rw"),
        (".claude/.credentials.json", "rw"),
        (".claude.json", "ro"),
        (".config/claude", "ro"),
        (".cache/claude", "rw"),
        (".local/state/claude", "rw"),
        (".local/share/claude", "ro"),
    ),
    "codex": (
        (".codex", "ro"),
        (".codex/sessions", "rw"),
        (".codex/log", "rw"),
        (".codex/history.jsonl", "rw"),
        (".codex/auth.json", "rw"),
    ),
    "pi": (
        (".pi", "ro"),
        (".pi/agent/sessions", "rw"),
    ),
    "opencode": (
        (".config/opencode", "ro"),
        (".local/share/opencode", "ro"),
        (".local/share/opencode/storage", "rw"),
        (".local/share/opencode/log", "rw"),
        (".local/share/opencode/project", "rw"),
        (".local/share/opencode/auth.json", "rw"),
        (".local/state/opencode", "rw"),
        (".cache/opencode", "ro"),
    ),
    "sprig": ((".local/share/sprig", "ro"),),
}

# Claude Code shortens a project dir name past this length and adds a hash
# the fence cannot know.
CLAUDE_PROJECT_NAME_MAX = 200


def claude_project_name(workspace: str) -> str:
    """The name of Claude Code's dir for workspace under ~/.claude/projects:
    every character but an ASCII letter or digit becomes "-"."""
    return "".join(c if c.isascii() and c.isalnum() else "-" for c in workspace)


SYSTEM_RO = (
    "/usr",
    "/bin",
    "/sbin",
    "/lib",
    "/lib32",
    "/lib64",
    "/libx32",
    "/etc",
    "/opt",
    "/proc",
    "/sys",
    "/run/systemd/resolve",
)

DEVICES = (
    "/dev/null",
    "/dev/zero",
    "/dev/full",
    "/dev/random",
    "/dev/urandom",
    "/dev/tty",
    "/dev/ptmx",
    "/dev/pts",
)

# A socket directory that is, or is above, one of these is too wide to
# take the connect right: the session bus, ssh-agent and tmux live there.
SOCKET_WIDE = ("/tmp", "/run", "/var/run", "/dev")

GLOB_CHARS = "*?[{"

# Each mode as its set of rights: r read, w write, u connect to a socket.
MODES: dict[str, frozenset[str]] = {
    "ro": frozenset("r"),
    "rw": frozenset("rw"),
    "unix": frozenset("u"),
    "sock": frozenset("ru"),
    "rw+unix": frozenset("rwu"),
}


def merge(a: str, b: str) -> str:
    """The mode that holds the rights of both."""
    want = MODES[a] | MODES[b]
    for name, rights in MODES.items():
        if rights == want:
            return name
    raise ValueError(want)  # pragma: no cover - MODES is closed under union


def norm(path: str) -> str:
    """posixpath.normpath with the POSIX double-slash rule removed."""
    n = posixpath.normpath(path)
    return "/" + n.lstrip("/") if n.startswith("//") else n


def expand(path: str, home: str, base: str) -> str:
    """``~`` and ``~/`` to home, a relative path joined to base, normalized."""
    if path == "~":
        path = home
    elif path.startswith("~/"):
        path = home + path[1:]
    if not path.startswith("/"):
        path = base + "/" + path
    return norm(path)


def covers(a: str, b: str) -> bool:
    """a is b or an ancestor of b."""
    return a == b or b.startswith(a if a.endswith("/") else a + "/")


def glob_prefix(pattern: str, home: str, base: str) -> str:
    """The longest literal directory of an envelope glob, as an absolute path."""
    p = pattern
    if p == "~":
        p = home
    elif p.startswith("~/"):
        p = home + p[1:]
    if not p.startswith("/"):
        p = base + "/" + p
    keep: list[str] = []
    for part in p.split("/"):
        if any(c in part for c in GLOB_CHARS):
            break
        keep.append(part)
    return norm("/".join(keep) or "/")


def _uniq(paths: list[str]) -> list[str]:
    seen: list[str] = []
    for p in paths:
        p = norm(p)
        if p not in seen:
            seen.append(p)
    return seen


def data_dirs(env: dict[str, str], home: str) -> list[str]:
    """datahome.guarded_data_dirs: every data dir the gate may use."""

    def usable(v: str | None) -> str | None:
        if not v:
            return None
        if v == "~":
            v = home
        elif v.startswith("~/"):
            v = home + v[1:]
        return v if v.startswith("/") else None

    out = [home + "/.opendaisugi"]
    d = usable(env.get("OPENDAISUGI_HOME"))
    if d is not None:
        out.append(d)
    x = usable(env.get("XDG_DATA_HOME"))
    if x is not None:
        out.append(x + "/opendaisugi")
    return _uniq(out)


def coppice_dirs(env: dict[str, str], home: str) -> list[str]:
    """The coppice config, state and data dirs: tokens and keys live there."""
    cfg = env.get("XDG_CONFIG_HOME") or home + "/.config"
    state = env.get("XDG_STATE_HOME") or home + "/.local/state"
    data = env.get("XDG_DATA_HOME") or home + "/.local/share"
    out = [cfg + "/coppice", state + "/coppice", data + "/coppice"]
    c = env.get("COPPICE_DATA_DIR")
    if c and c.startswith("/"):
        out.append(c)
    return _uniq(out)


def tmp_parents(env: dict[str, str], homes: list[str]) -> list[str]:
    """The directories whose ``fence-tmp/<leaf>`` the fence makes its temp dir in."""
    out: list[str] = []
    for h in homes:
        out.extend(data_dirs(env, h))
        out.extend(coppice_dirs(env, h))
        out.append((env.get("XDG_DATA_HOME") or h + "/.local/share") + "/opendaisugi")
    return [p + "/fence-tmp" for p in _uniq(out)]


def plan(inp: dict[str, Any]) -> dict[str, Any]:
    """The rule list for ``inp``.

    Input keys: harness, workspace, home (the HOME the harness sees),
    other_homes (the password entry's home when it differs: its secrets stay
    shut too), tmpdir (the fence's private temp dir, or ""), env (the XDG,
    data and coppice variables), path_dirs, exe_dirs, gate_root (or ""),
    envelope ({"file_read": [...], "file_write": [...]}), allow
    (["PATH[:rw]"]), workdirs (more dirs to read and write, checked like the
    workspace), sockets (the coppice socket path), real (a harness path or
    envelope prefix to its resolved spelling), never_extra (other spellings
    of a secret), state_extra (other spellings of the gate's state).

    Output: ``{"rules": [{"path", "mode"}], "notes": [...], "refusals": [...]}``.
    Any refusal means the harness must not start under this plan.
    """
    home = norm(inp["home"])
    homes = _uniq([home] + [h for h in inp.get("other_homes") or [] if h.startswith("/")])
    workspace = norm(inp["workspace"])
    env = inp.get("env") or {}
    real = inp.get("real") or {}
    notes: list[str] = []
    refusals: list[str] = []

    home_secrets = [norm(h + "/" + s) for h in homes for s in SECRETS]
    home_secrets.extend(norm(extra) for extra in inp.get("never_extra") or [])
    secrets = list(home_secrets)
    state_dirs: list[str] = []
    for h in homes:
        secrets.extend(coppice_dirs(env, h))
        state_dirs.extend(data_dirs(env, h))
    state_dirs = _uniq(state_dirs)
    gate_root = inp.get("gate_root") or ""
    state = list(state_dirs) + ([norm(gate_root)] if gate_root else [])
    state.extend(norm(extra) for extra in inp.get("state_extra") or [])
    secrets = sorted(set(secrets))
    home_secrets = sorted(set(home_secrets))
    state = sorted(set(state))

    def resolved(p: str) -> str:
        return norm(real.get(p, p))

    grants: dict[str, str] = {}

    def put(path: str, mode: str) -> None:
        grants[path] = merge(grants[path], mode) if path in grants else mode

    def opens_unnamed(path: str) -> str | None:
        for s in secrets:
            if covers(path, s) and path != s:
                return s
        return None

    def secret_hit(path: str) -> str | None:
        for s in secrets:
            if covers(path, s) or covers(s, path):
                return s
        return None

    def state_hit(path: str) -> str | None:
        for s in state:
            if covers(path, s) or covers(s, path):
                return s
        return None

    allow_specs: list[tuple[str, str]] = []
    for spec in inp.get("allow") or []:
        a, m = spec, "ro"
        if spec.endswith(":rw"):
            a, m = spec[:-3], "rw"
        elif spec.endswith(":ro"):
            a = spec[:-3]
        allow_specs.append((expand(a, home, workspace), m))

    def under_allow(path: str, mode: str) -> bool:
        """path lies in an --allow of the same or wider mode that stands."""
        for a, m in allow_specs:
            if not covers(a, path) or not _dominates(m, mode) or opens_unnamed(a) is not None:
                continue
            if m == "rw" and state_hit(a) is not None:
                continue
            return True
        return False

    def strict(path: str, mode: str, who: str) -> None:
        """A grant the fence or coppice made: it may never open a secret,
        unless an explicit --allow already names that place."""
        if under_allow(path, mode):
            put(path, mode)
            return
        s = secret_hit(path)
        if s is not None:
            refusals.append(
                f"{who} {path} would reach the secret {s}. Only --allow opens a secret. "
                "Name another directory, or run with --no-fence."
            )
            return
        if mode != "ro":
            s = state_hit(path)
            if s is not None:
                refusals.append(
                    f"{who} {path} would let the agent write the gate's state under {s}. "
                    "Name another directory, or run with --no-fence."
                )
                return
        put(path, mode)

    def allowed(path: str, mode: str) -> None:
        """An --allow the operator typed: it may name a secret, not open one above it."""
        s = opens_unnamed(path)
        if s is not None:
            refusals.append(
                f"--allow {path} would also open {s}. Name a narrower directory, "
                "or run with --no-fence."
            )
            return
        if mode == "rw":
            s = state_hit(path)
            if s is not None:
                refusals.append(
                    f"--allow {path} would let the agent write the gate's state under {s}. "
                    "Name another directory, or run with --no-fence."
                )
                return
        put(path, mode)

    def ambient(path: str, mode: str) -> None:
        """A grant nobody typed (PATH, a binary's directory): dropped on a clash."""
        if secret_hit(path) or (mode == "rw" and state_hit(path)):
            return
        put(path, mode)

    for p in SYSTEM_RO:
        ambient(p, "ro")
    for p in DEVICES:
        put(p, "rw")

    strict(workspace, "rw", "the workspace")
    for d in inp.get("workdirs") or []:
        strict(expand(d, home, workspace), "rw", "the work dir")

    tmpdir = inp.get("tmpdir") or ""
    if tmpdir:
        tmpdir = norm(tmpdir)
        own = posixpath.dirname(tmpdir) in tmp_parents(env, homes)
        if own and opens_unnamed(tmpdir) is None:
            put(tmpdir, "rw")
        else:
            strict(tmpdir, "rw", "the temp dir")

    for rel, mode in HARNESS.get(inp.get("harness") or "", ()):
        strict(resolved(norm(home + "/" + rel)), mode, "the harness path")
    if inp.get("harness") == "claude":
        projects = resolved(norm(home + "/.claude/projects"))
        if workspace.isascii() and len(workspace) <= CLAUDE_PROJECT_NAME_MAX:
            strict(projects + "/" + claude_project_name(workspace), "rw", "the harness path")
        else:
            strict(projects, "rw", "the harness path")
            notes.append(
                "the fence cannot name Claude's project dir for this workspace; "
                "~/.claude/projects stays writable, the memory of every project with it."
            )

    for d in inp.get("path_dirs") or []:
        if d.startswith("/"):
            ambient(norm(d), "ro")
    for d in inp.get("exe_dirs") or []:
        if d.startswith("/"):
            ambient(norm(d), "ro")

    for d in state_dirs:
        put(d + "/gate", "sock")
        put(d + "/config.yaml", "ro")
    if gate_root:
        put(norm(gate_root), "sock")

    wide = list(SOCKET_WIDE) + homes
    rt = env.get("XDG_RUNTIME_DIR") or ""
    if rt.startswith("/"):
        wide.append(norm(rt))
    for sock in inp.get("sockets") or []:
        if not sock.startswith("/"):
            continue
        d = posixpath.dirname(norm(sock))
        if any(covers(d, w) for w in wide):
            notes.append(f"socket dir {d}: too wide for the fence; dropped.")
            continue
        if any(covers(d, s) or covers(s, d) for s in home_secrets):
            notes.append(f"socket dir {d}: would reach a secret directory; dropped.")
            continue
        put(d, "unix")

    env_in = inp.get("envelope") or {}
    for key, mode in (("file_read", "ro"), ("file_write", "rw")):
        for pat in env_in.get(key) or []:
            pre = resolved(glob_prefix(pat, home, workspace))
            if covers(workspace, pre):
                continue
            if pre == "/" or any(covers(pre, h) for h in homes):
                notes.append(f"envelope {key} {pat}: too wide for the fence; dropped.")
                continue
            if secret_hit(pre) is not None:
                notes.append(f"envelope {key} {pat}: would reach a secret directory; dropped.")
                continue
            if mode == "rw" and state_hit(pre):
                notes.append(f"envelope {key} {pat}: would reach the gate's state; dropped.")
                continue
            put(pre, mode)

    for path, mode in allow_specs:
        allowed(path, mode)

    rules = [{"path": p, "mode": m} for p, m in sorted(grants.items())]
    rules = _prune(rules)
    return {"rules": rules, "notes": notes, "refusals": refusals}


def _dominates(o: str, r: str) -> bool:
    """Rights of mode o include the rights of mode r."""
    return MODES[o] >= MODES[r]


def _prune(rules: list[dict[str, str]]) -> list[dict[str, str]]:
    """Drop a rule an ancestor rule of the same or wider mode already gives."""
    out = []
    for r in rules:
        redundant = False
        for o in rules:
            if o is r or not covers(o["path"], r["path"]) or o["path"] == r["path"]:
                continue
            if _dominates(o["mode"], r["mode"]):
                redundant = True
                break
        if not redundant:
            out.append(r)
    return out
