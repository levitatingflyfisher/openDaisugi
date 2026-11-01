"""Reading a branch worktree without trusting what the child wrote in it.

A branch child can write any file in its worktree, including the ``.git``
pointer. So the parent never takes the repository or the base commit from
the worktree. It takes them from what it recorded itself (the branch's
``spawn`` row in the tree ledger, or the ``where`` of an attempt the parent
wrote), and checks, before each git call, that the worktree still belongs
to that repository. The base is a full object id, never read as an option.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from collections.abc import Callable
from pathlib import Path
from typing import Any

from opendaisugi.tree import is_object_id

# No external diff driver and no textconv: both would run a program that a
# config the child can reach names.
# No submodule check either: a nested repository the child made would be asked
# for its status, and that status reads the nested config and runs its filters.
DIFF_FLAGS = ("--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all")

# The shared config and the hooks of a repository are a place a branch child
# may reach, so every git call the parent makes in a branch worktree, or in
# the tree an adopt writes, switches off the two ways that config runs a
# program with no verb of ours: a file system monitor and a hooks directory.
# /dev/null holds no hook, so no hook is found.
GUARD = (
    "-c",
    "core.fsmonitor=false",
    "-c",
    "core.hooksPath=/dev/null",
    "-c",
    "diff.ignoreSubmodules=all",
    "-c",
    "submodule.recurse=false",
)


def argv(path: Path | str, *args: str) -> list[str]:
    """``git`` run in ``path`` with the guard on."""
    return ["git", *GUARD, "-C", str(path), *args]


def common_dir(path: Path | str, git: Callable[..., Any] = subprocess.run) -> str | None:
    """The absolute common git directory ``path`` resolves to now."""
    try:
        r = git(
            argv(path, "rev-parse", "--path-format=absolute", "--git-common-dir"),
            capture_output=True,
        )
    except OSError:
        return None
    if r.returncode != 0:
        return None
    return r.stdout.decode("utf-8", "replace").strip() or None


def _same(a: str, b: str) -> bool:
    try:
        return os.path.realpath(a) == os.path.realpath(b)
    except (OSError, ValueError):
        return False


def belongs(path: Path | str, repo: str, git: Callable[..., Any] = subprocess.run) -> bool:
    """True when the work tree at ``path`` resolves to ``repo``. For a tree
    the owner named (an adopt target); a branch worktree needs ``owns``."""
    if Path(path).is_symlink():
        return False
    found = common_dir(path, git)
    return found is not None and _same(found, repo)


def owns(path: Path | str, repo: str, git: Callable[..., Any] = subprocess.run) -> bool:
    """True when ``path`` is the linked worktree git made for it in ``repo``:
    its ``.git`` is a regular file, and its git directory is exactly
    ``<repo>/worktrees/<name>``. The common directory alone also admits the
    repository's main git directory and a sibling's."""
    p = Path(path)
    try:
        if p.is_symlink() or not stat.S_ISREG(os.lstat(p / ".git").st_mode):
            return False
        # With extensions.worktreeConfig on, git reads this file, which the
        # child can write, and the -c guard cannot switch that off.
        if os.path.lexists(os.path.join(repo, "worktrees", p.name, "config.worktree")):
            return False
        # A store of the child's own that was not folded away: the worktree's
        # HEAD and index may name objects only that store holds.
        if os.path.lexists(store(repo, p)):
            return False
        r = git(argv(p, "rev-parse", "--path-format=absolute", "--git-dir"), capture_output=True)
    except OSError:
        return False
    if r.returncode != 0:
        return False
    found = r.stdout.decode("utf-8", "replace").strip()
    return (
        bool(found)
        and _same(found, os.path.join(repo, "worktrees", p.name))
        and belongs(p, repo, git)
    )


def store(repo: str, worktree: Path | str) -> str:
    """The object store a fenced branch child writes: inside its own admin
    directory. The parent never reads it and never writes it."""
    return os.path.join(repo, "worktrees", Path(worktree).name, "objects")


def _drop(path: str) -> None:
    """Remove ``path`` without following a symbolic link at any level."""
    try:
        st = os.lstat(path)
    except OSError:
        return
    try:
        if stat.S_ISDIR(st.st_mode):
            shutil.rmtree(path, ignore_errors=True)
        else:
            os.unlink(path)
    except OSError:
        pass


def reclaim(admin: str) -> None:
    """Give the parent back its rights on a branch admin directory and all
    under it: owner rwx on each directory, owner rw on each regular file.
    The child has the same uid and may have taken them away. Each entry is
    read with lstat; a link is never followed and nothing outside ``admin``
    is changed. Call it only once the child and all its processes are gone."""
    todo = [admin]
    while todo:
        path = todo.pop()
        try:
            st = os.lstat(path)
        except OSError:
            continue
        mode = stat.S_IMODE(st.st_mode)
        if stat.S_ISDIR(st.st_mode):
            want = mode | 0o700
        elif stat.S_ISREG(st.st_mode):
            want = mode | 0o600
        else:
            continue
        if want != mode:
            try:
                os.chmod(path, want)
            except OSError:
                pass
        if stat.S_ISDIR(st.st_mode):
            try:
                with os.scandir(path) as it:
                    todo.extend(e.path for e in it)
            except OSError:
                pass


def fold(
    repo: str, worktree: Path | str, base: str, git: Callable[..., Any] = subprocess.run
) -> bool:
    """Fold a branch child's own object store away, once the child and all
    its processes are gone and before the parent reads its worktree.

    The worktree's HEAD goes back to ``base``, its index to ``base``'s tree,
    and its reflogs, its own refs and its object store are deleted. The
    child's commits then survive only as the files in the worktree, and the
    diff against ``base`` reads those files. Without this, the operator's
    repository holds a HEAD, an index and reflogs that name objects which
    only the child's store holds, so ``gc``, ``fsck`` and ``log --all`` fail,
    and a parent write skips an object the child store already has.

    git runs on the recorded git directory, never through the worktree's
    ``.git`` pointer, which the child can rewrite. A link the child made in
    its admin directory is removed, never followed. True when the store is
    gone and HEAD and the index name ``base``.

    It first gives the parent back its rights on the admin directory, so a
    child that took them cannot keep its logs, refs or store. True only when
    the logs, the refs, any ``reftable`` directory and the store are gone,
    HEAD names ``base`` and the index holds ``base``'s tree."""
    if not is_object_id(base) or not repo:
        return False
    name = Path(worktree).name
    admin = os.path.join(repo, "worktrees", name)
    try:
        if not stat.S_ISDIR(os.lstat(admin).st_mode):
            return False
    except OSError:
        return False
    reclaim(admin)
    # A fenced run refuses a reftable repository before any worktree is
    # made, so a reftable directory here is one the child planted. A lock the
    # child put on its worktree would keep prune from removing it.
    for part in ("logs", "refs", "reftable", "locked", "HEAD.lock", "index.lock"):
        _drop(os.path.join(admin, part))
    for part in ("HEAD", "index"):
        path = os.path.join(admin, part)
        try:
            if not stat.S_ISREG(os.lstat(path).st_mode):
                _drop(path)
        except OSError:
            pass
    at = ["git", *GUARD, "-c", "core.logAllRefUpdates=false", f"--git-dir={repo}"]
    env = dict(os.environ)
    for k in ("GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_DIR"):
        env.pop(k, None)
    env["GIT_INDEX_FILE"] = os.path.join(admin, "index")
    try:
        r = git(
            [*at, "update-ref", "--no-deref", f"worktrees/{name}/HEAD", base],
            capture_output=True,
            env=env,
        )
        if r.returncode != 0:
            return False
        r = git([*at, "read-tree", base], capture_output=True, env=env)
        if r.returncode != 0:
            return False
    except OSError:
        return False
    _drop(os.path.join(admin, "logs"))
    _drop(store(repo, worktree))
    for part in ("logs", "refs", "reftable", "objects"):
        if os.path.lexists(os.path.join(admin, part)):
            return False
    try:
        head = os.path.join(admin, "HEAD")
        if not stat.S_ISREG(os.lstat(head).st_mode):
            return False
        with open(head, encoding="utf-8", errors="replace") as f:
            if f.read().strip() != base:
                return False
        if not stat.S_ISREG(os.lstat(os.path.join(admin, "index")).st_mode):
            return False
        r = git(
            [*at, "diff-index", "--cached", "--quiet", base, "--"], capture_output=True, env=env
        )
    except OSError:
        return False
    return r.returncode == 0


def ref_format(repo: str, git: Callable[..., Any] = subprocess.run) -> str:
    """The ref format of the repository whose git directory is ``repo``, from
    its own config (``extensions.refStorage``; none is ``files``). Read
    before any child runs, from the operator's config, never from a path a
    child can write."""
    try:
        r = git(
            [
                "git",
                *GUARD,
                "config",
                "--file",
                os.path.join(repo, "config"),
                "--get",
                "extensions.refStorage",
            ],
            capture_output=True,
        )
    except OSError:
        return "unknown"
    if r.returncode == 1:
        return "files"
    if r.returncode != 0:
        return "unknown"
    return r.stdout.decode("utf-8", "replace").strip().lower() or "files"


def lost(session: str, repo: str) -> str:
    return f"the worktree of {session} no longer belongs to {repo}"


def usable(base: object, repo: object) -> bool:
    return is_object_id(base) and isinstance(repo, str) and bool(repo)


def stage(
    worktree: Path | str, git: Callable[..., Any] = subprocess.run, *, intent: bool = False
) -> None:
    """``git add -A`` (``--intent-to-add`` when ``intent``) of a branch
    worktree, leaving out the nested repositories the index already holds as
    gitlinks. An add refreshes each index entry, and refreshing a gitlink asks
    the nested repository for its status, which reads its config and runs the
    filters the child wrote there."""
    listed = git(argv(worktree, "ls-files", "-s", "-z"), capture_output=True)
    links = []
    if listed.returncode == 0:
        for rec in listed.stdout.decode("utf-8", "replace").split("\0"):
            if rec.startswith("160000 ") and "\t" in rec:
                links.append(rec.split("\t", 1)[1])
    flags = ["-A", "--intent-to-add"] if intent else ["-A"]
    git(
        argv(worktree, "add", *flags, "--", ".", *(f":(exclude,literal){p}" for p in links)),
        capture_output=True,
    )


def intent(worktree: Path | str, git: Callable[..., Any] = subprocess.run) -> None:
    """Mark new files so a diff shows them. Only after ``owns``."""
    stage(worktree, git, intent=True)


def diff(
    worktree: Path | str,
    base: str,
    repo: str,
    git: Callable[..., Any] = subprocess.run,
    *,
    extra: tuple[str, ...] = (),
    mark_new: bool = True,
    timeout: float | None = None,
) -> bytes | None:
    """The worktree's changes against ``base``, or None when the worktree
    does not belong to ``repo``, the base is not an object id, or git fails."""
    if not is_object_id(base) or not owns(worktree, repo, git):
        return None
    if mark_new:
        intent(worktree, git)
    kw: dict[str, Any] = {} if timeout is None else {"timeout": timeout}
    r = git(
        argv(worktree, "diff", *DIFF_FLAGS, *extra, base, "--"),
        capture_output=True,
        **kw,
    )
    return r.stdout if r.returncode == 0 else None
