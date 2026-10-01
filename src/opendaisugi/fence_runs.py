"""What the resident gate trusts about a caller, and what it never takes (FN-R-21).

The resident gate runs outside the fence and makes every write. So it takes
no path from a request: its root and its captures root are fixed when it
starts, by the trusted parent that starts it. A request whose argv names
``--root`` or ``--captures-root`` (or a prefix argparse would read as one)
is denied.

A session id names files, so a resident call's session id must match
``[A-Za-z0-9._-]{1,128}`` and must not be ``.`` or ``..``.

A checkpoint writes into the repository at the payload's cwd. The resident
gate takes one only inside the workspace a fence run bound: ``daisugi
fence``, before it applies its ruleset and still outside the fence, writes
``<root>/fence-runs/<pid>.json`` with its pid, the start time the kernel
gives that pid, and the resolved workspace. The gate state is read only
inside the fence, so no fenced process can write such a file. The server
reads the caller's pid with SO_PEERCRED and walks its parents to the fence
run. A caller with no fence run above it has no bound workspace.

Checkpoints are off inside the fence (FN-R-36): a ``--checkpoints`` call
from a caller under a fence run is denied with one line, because the
unfenced server would run git on repo config the fenced agent writes. The
bound snapshot below stays for unfenced use.

A checkpoint never lets git choose the tree (FN-R-27). The work tree is the
bound workspace, and git runs with GIT_DIR, GIT_COMMON_DIR and GIT_WORK_TREE
set to it, so repo config (core.worktree) and a gitfile cannot move it. A
``.git`` that is not a plain directory inside the workspace (a link, a
``commondir`` file, an ``objects`` or ``refs`` dir that is a link) denies
the call: the unfenced server would write through it. A ``.git`` gitfile is
taken only for a git worktree whose link checks out by realpath (W-1,
FN-R-34): the gitfile names ``<common>/worktrees/<name>``, that dir's
``gitdir`` names this ``.git``, and its ``commondir`` is ``<common>``. Then
GIT_DIR is that admin dir and GIT_COMMON_DIR is ``<common>``; the work tree
stays the bound workspace. A workspace
with no ``.git`` gets no checkpoint, and git never walks up to a parent.

The transcript a resident payload names is read only when it is a regular
file under ``~/.claude/projects`` of the password entry's home, opened with
O_NOFOLLOW and O_NONBLOCK (FN-R-29). Anything else gets no checkpoint.
"""

from __future__ import annotations

import json
import os
import pwd
import re
import stat
from pathlib import Path

RUNS_DIR = "fence-runs"
_SESSION = re.compile(r"[A-Za-z0-9._-]{1,128}")
_PATH_FLAGS = ("--root", "--captures-root")
_MAX_DEPTH = 64

SESSION_REFUSAL = (
    "the resident gate takes a session id only when it matches [A-Za-z0-9._-]{1,128}, "
    "so it denies this call"
)
PATH_REFUSAL = (
    "the resident gate takes no path from a request: its root and captures root are "
    "fixed when it starts. Drop --root and --captures-root from the request"
)
NO_WORKSPACE_REFUSAL = (
    "the resident gate takes a checkpoint only inside the workspace a fence run bound, "
    "and this caller runs under no fence run, so it denies this call"
)
GITDIR_REFUSAL = (
    "the workspace's git dir is not a plain directory inside the workspace this caller's "
    "fence run bound (a gitfile, a link, a common dir, or objects or refs that lead out), "
    "so the resident gate denies this call"
)
# A transcript bigger than this is not read: no checkpoint.
TRANSCRIPT_CAP = 128 * 1024 * 1024
# FN-R-36: the unfenced server never runs git for a fenced session, whose
# agent writes the repo config git would read.
FENCED_CHECKPOINT_REFUSAL = "checkpoints are off inside the fence"
OUTSIDE_REFUSAL = (
    "the checkpoint cwd is not inside the workspace this caller's fence run bound, "
    "so the resident gate denies this call"
)


def session_ok(sid: object) -> bool:
    """A session id the resident gate may build a path from."""
    return isinstance(sid, str) and bool(_SESSION.fullmatch(sid)) and sid not in (".", "..")


def names_a_path(argv: list[str]) -> bool:
    """True when a word of argv is an option argparse would read as --root or
    --captures-root: the full name or a prefix of it ("--r", "--ca"), alone
    or with "=value"."""
    for word in argv:
        name = word.split("=", 1)[0]
        if len(name) < 3 or not name.startswith("--"):
            continue
        if any(flag.startswith(name) for flag in _PATH_FLAGS):
            return True
    return False


def strip_path_flags(argv: list[str]) -> list[str]:
    """argv without --root/--captures-root and their values: a client uses
    --root only to find the socket, and the server has its own."""
    out: list[str] = []
    skip = False
    for word in argv:
        if skip:
            skip = False
            continue
        if word in _PATH_FLAGS:
            skip = True
            continue
        if any(word.startswith(flag + "=") for flag in _PATH_FLAGS):
            continue
        out.append(word)
    return out


def _stat_fields(pid: int) -> list[str] | None:
    try:
        text = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    i = text.rfind(")")
    if i < 0:
        return None
    return text[i + 2 :].split()


def proc_start(pid: int) -> str | None:
    """The start time /proc gives pid (field 22), so a reused pid never
    matches an old fence run."""
    f = _stat_fields(pid)
    return f[19] if f is not None and len(f) > 19 else None


def _parent(pid: int) -> int | None:
    f = _stat_fields(pid)
    try:
        return int(f[1]) if f is not None and len(f) > 1 else None
    except ValueError:
        return None


def bound_workspace(root: Path, peer_pid: int | None) -> str | None:
    """The workspace of the fence run the caller runs under, or None."""
    from opendaisugi._state_files import read_state_text

    pid = peer_pid
    for _ in range(_MAX_DEPTH):
        if pid is None or pid <= 1:
            return None
        try:
            text = read_state_text(root, (RUNS_DIR,), f"{pid}.json")
        except OSError:
            text = None
        if text is not None:
            try:
                rec = json.loads(text)
            except ValueError:
                rec = None
            if (
                isinstance(rec, dict)
                and rec.get("pid") == pid
                and isinstance(rec.get("start"), str)
                and rec["start"] == proc_start(pid)
                and isinstance(rec.get("workspace"), str)
                and os.path.isabs(rec["workspace"])
            ):
                return rec["workspace"]
        pid = _parent(pid)
    return None


def register(root: Path, pid: int, workspace: str) -> None:
    """Write the fence run record (the Go and Rust fences write the same)."""
    from opendaisugi._state_files import write_state_file

    rec = {"pid": pid, "start": proc_start(pid), "workspace": os.path.realpath(workspace)}
    write_state_file(root, (RUNS_DIR,), f"{pid}.json", json.dumps(rec), replace_link=True)


def inside(cwd: str, workspace: str) -> bool:
    """cwd, resolved with every link followed, is the workspace or below it."""
    real = os.path.realpath(cwd)
    ws = workspace.rstrip("/") or "/"
    return real == ws or real.startswith(ws if ws == "/" else ws + "/")


def _read_link_file(path: str) -> str | None:
    """The first line of a small git link file (a gitfile, gitdir,
    commondir), read with O_NOFOLLOW, or None."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    except OSError:
        return None
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            return None
        data = os.read(fd, 4097)
    except OSError:
        return None
    finally:
        os.close(fd)
    if len(data) > 4096 or b"\x00" in data:
        return None
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        return None
    line = text.split("\n", 1)[0].rstrip("\r").rstrip(" \t\n\r\x0b\x0c")
    return line or None


def _worktree_dirs(ws: str, gitfile: str) -> tuple[str, str] | None:
    """(git dir, common dir) of a git worktree whose link checks out, or None.

    By realpath (W-1): the gitfile names ``<common>/worktrees/<name>``; that
    dir's ``gitdir`` file names this workspace's ``.git``; its ``commondir``
    resolves to ``<common>``. A relative path in a file is read from the dir
    that holds the file, as git reads it."""
    line = _read_link_file(gitfile)
    if line is None or not line.startswith("gitdir: "):
        return None
    named = line[len("gitdir: ") :]
    if not named:
        return None
    admin = os.path.realpath(os.path.join(ws, named))
    common = os.path.dirname(os.path.dirname(admin))
    if os.path.basename(os.path.dirname(admin)) != "worktrees" or not os.path.isdir(admin):
        return None
    back = _read_link_file(admin + "/gitdir")
    if back is None or os.path.realpath(os.path.join(admin, back)) != os.path.realpath(gitfile):
        return None
    cd = _read_link_file(admin + "/commondir")
    if cd is None or os.path.realpath(os.path.join(admin, cd)) != common:
        return None
    return admin, common


def git_layout(workspace: str) -> tuple[str | None, tuple[str, str] | None]:
    """(refusal, (git dir, common dir)) for a checkpoint of workspace.

    (None, None) when the workspace has no ``.git``: it gets no checkpoint.
    A plain ``.git`` dir is both dirs. A ``.git`` gitfile is taken only for
    a git worktree whose link checks out (W-1); its common dir may lie
    outside the workspace."""
    ws = workspace
    if os.path.realpath(ws) != ws:
        return GITDIR_REFUSAL, None
    g = ws.rstrip("/") + "/.git"
    try:
        st = os.lstat(g)
    except FileNotFoundError:
        return None, None
    except OSError:
        return GITDIR_REFUSAL, None
    if stat.S_ISDIR(st.st_mode):
        dirs = (g, g)
    elif stat.S_ISREG(st.st_mode):
        wt = _worktree_dirs(ws, g)
        if wt is None:
            return GITDIR_REFUSAL, None
        dirs = wt
    else:
        return GITDIR_REFUSAL, None
    common = dirs[1]
    try:
        if not stat.S_ISDIR(os.lstat(common).st_mode):
            return GITDIR_REFUSAL, None
    except OSError:
        return GITDIR_REFUSAL, None
    if os.path.lexists(common + "/commondir"):
        return GITDIR_REFUSAL, None
    for sub in ("objects", "refs", "refs/daisugi"):
        try:
            st = os.lstat(common + "/" + sub)
        except FileNotFoundError:
            continue
        except OSError:
            return GITDIR_REFUSAL, None
        if not stat.S_ISDIR(st.st_mode):
            return GITDIR_REFUSAL, None
    try:
        with os.scandir(common + "/objects") as it:
            if any(e.is_symlink() for e in it):
                return GITDIR_REFUSAL, None
    except FileNotFoundError:
        pass
    except OSError:
        return GITDIR_REFUSAL, None
    for top, dirs_, files in os.walk(common + "/refs/daisugi", followlinks=False):
        for name in dirs_ + files:
            if os.path.islink(os.path.join(top, name)):
                return GITDIR_REFUSAL, None
    return None, dirs


def git_dir_refusal(workspace: str) -> str | None:
    """Why a checkpoint of workspace would write where git's own layout does
    not lead, or None. None also when the workspace has no ``.git``."""
    return git_layout(workspace)[0]


def transcript_root() -> str:
    """~/.claude/projects of the password entry's home (never $HOME)."""
    return pwd.getpwuid(os.getuid()).pw_dir.rstrip("/") + "/.claude/projects"


def resident_transcript(path: object) -> str | None:
    """The text of the transcript a resident payload names, or None.

    Only a regular file under transcript_root() (realpath) is read, opened
    O_NOFOLLOW|O_NONBLOCK, so a link that leads out, a FIFO or a device is
    never opened for reading."""
    if not isinstance(path, str) or "\x00" in path or not os.path.isabs(path):
        return None
    base = os.path.realpath(transcript_root())
    real = os.path.realpath(path)
    if real == base or not inside(real, base):
        return None
    try:
        fd = os.open(real, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > TRANSCRIPT_CAP:
            return None
        chunks: list[bytes] = []
        left = TRANSCRIPT_CAP + 1
        while left > 0:
            b = os.read(fd, min(left, 1 << 20))
            if not b:
                break
            chunks.append(b)
            left -= len(b)
        data = b"".join(chunks)
        if len(data) > TRANSCRIPT_CAP:
            return None
        return data.decode("utf-8")
    except (OSError, UnicodeDecodeError):
        return None
    finally:
        os.close(fd)


def resident_refusal(
    payload: dict, pin_session: str | None, checkpoints: bool, workspace: str | None
) -> str | None:
    """Why a resident call is denied before it is checked, or None."""
    if pin_session and not session_ok(pin_session):
        return SESSION_REFUSAL
    sid = payload.get("session_id")
    if sid is not None and not session_ok(sid):
        return SESSION_REFUSAL
    if checkpoints and workspace is not None:
        # A fenced session (FN-R-36): the server would run git on config
        # the agent writes, so it takes no checkpoint at all.
        return FENCED_CHECKPOINT_REFUSAL
    cwd = payload.get("cwd")
    if checkpoints and cwd:
        if workspace is None:
            return NO_WORKSPACE_REFUSAL
        if (
            not isinstance(cwd, str)
            or "\x00" in cwd
            or not os.path.isabs(cwd)
            or not inside(cwd, workspace)
        ):
            return OUTSIDE_REFUSAL
        why = git_dir_refusal(workspace)
        if why is not None:
            return why
    return None
