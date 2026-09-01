"""Workspace checkpoints as private refs in the user's own repo.

A snapshot is a commit object under ``refs/daisugi/checkpoints/<session>/<id>``
built through a temporary index, so the user's index, HEAD, branches, tags and
stash never change. The refs live under ``refs/`` on purpose: ``git gc`` keeps
what they reach. The honest cost: ``git log --all`` and ``git for-each-ref`` do
list them; plain ``git log``, ``git branch``, ``git tag`` and ``git status`` do
not. Restore first snapshots the current state to ``refs/daisugi/rollback/...``,
then checks the target out. Never a shadow repo; never per tool call (opt-in at
prompt boundaries).

A checkpoint is always WHOLE-REPO scoped: ``snapshot`` and ``restore``
normalize ``repo`` to the git toplevel at entry (:func:`_toplevel`), so a
``repo`` that is a subdirectory of the toplevel (the common case — a gate hook
passes the agent's cwd, routinely a monorepo subdir) still captures and
restores the whole tree, and every git command and filesystem join here speaks
ONE path namespace instead of mixing toplevel-relative and subdir-relative
names for the same blob.

The one rule every function here answers to: **never touch an on-disk path
that is not FULLY RECOVERABLE from the rollback ref.** "Fully recoverable"
is stricter than "``git add -A`` walked it" (:attr:`Checkpoint.covers`): a
gitignored path, a nested git repo (recorded as a gitlink — only its commit
SHA, not its content), and a directory (never a real blob) can all appear
to be "captured" by a naive path-string check while being nothing of the
sort. ``restore`` builds its recoverable set fresh from the rollback ref's
own tree (:func:`_recoverable_paths`, mode-filtered) and refuses outright —
before deleting, overwriting, or force-removing anything — the moment any
at-risk path isn't in it.
"""

from __future__ import annotations

import contextlib
import errno
import os
import stat
import subprocess
import tempfile
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

_AUTHOR = {
    "GIT_AUTHOR_NAME": "daisugi",
    "GIT_AUTHOR_EMAIL": "daisugi@localhost",
    "GIT_COMMITTER_NAME": "daisugi",
    "GIT_COMMITTER_EMAIL": "daisugi@localhost",
}


class NotAGitRepo(RuntimeError):
    """``repo`` is not inside a git working tree."""


class RestoreRefused(RuntimeError):
    """A restore was refused rather than risk an unrecoverable loss.

    Raised in either of two cases, both BEFORE anything is mutated (no
    read-tree, no checkout, no deletion):

    - the rollback snapshot taken at the start of ``restore`` could not
      fully cover the current working tree (some path was oversize or
      unreadable);
    - a path in the checkpoint's own target tree — or a non-directory
      parent path component blocking one — is currently on disk but is NOT
      fully recoverable from the rollback (:func:`_recoverable_paths`): the
      overwrite lane. A path can fail to be recoverable for more than one
      reason — a currently-gitignored path (``git add -A`` silently omits
      it), a nested git repo (recorded as a gitlink, only a commit SHA, not
      its content), or simply being a directory (never a real blob) — and
      the first case above can't see any of them, yet
      ``checkout-index -a -f`` would still overwrite or force-remove the
      path, because it IS part of the target tree, or blocks one that is.

    The rollback ref itself still exists for whatever it DID manage to
    capture.
    """


class RestorePartiallyFailed(RuntimeError):
    """A restore's checkout step itself failed partway through git.

    Unlike :class:`RestoreRefused` (which always means nothing was
    touched), this means the working tree may now be in a MIXED state:
    some paths from the checkpoint may have been written before git erred
    on another. The message names the rollback ref taken at the start of
    this restore, so there is always a recovery pointer even here.

    NOTE what this is NOT: ``checkout-index -f`` is aggressive about
    reconciling an on-disk type conflict on its own — it FORCE-REMOVES a
    conflicting file, or even a non-empty directory, to make room for what
    the target tree wants at that path (verified empirically). So "a path
    component that used to be a directory is now a plain file" is never
    what raises this — that is exactly the sibling data-loss lane the
    endangered-set check in :func:`restore` exists to catch BEFORE any
    mutation, precisely because ``checkout-index`` would otherwise
    force-remove it silently and successfully (exit 0, no error at all).
    What genuinely raises this is something git structurally cannot do
    regardless of ``-f`` — e.g. a permission-denied containing directory.
    """


@dataclass(frozen=True)
class Checkpoint:
    ref: str
    commit: str
    covers: list[str]
    skipped: list[str]


def _git(repo: Path, *args: str, env: dict[str, str] | None = None) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=True,
        capture_output=True,
        text=True,
        env={**os.environ, **(env or {})},
    )
    # rstrip("\n") — NOT .strip(): several callers (ls-files -z, ls-tree -z)
    # NUL-join filenames, and a filename may legally start with a literal
    # space on Linux. .strip() removes ANY leading/trailing whitespace from
    # the WHOLE string, which would eat a real leading space off the FIRST
    # such filename before it is split. Every non-z command here only ever
    # trails a single newline, so rstrip("\n") is a no-op difference for them.
    return proc.stdout.rstrip("\n")


def is_repo(path: Path) -> bool:
    try:
        return _git(path, "rev-parse", "--is-inside-work-tree") == "true"
    except (subprocess.CalledProcessError, OSError):
        return False


def _require_daisugi_ref(ref: str) -> None:
    """Refuse anything but one of this module's own refs, before it reaches
    a git invocation.

    A ref starting with ``-`` would be read by the git CLI as an OPTION to
    ``read-tree``/``ls-tree`` rather than the refname it claims to be (flag
    injection); requiring the ``refs/daisugi/`` prefix rules that out
    structurally rather than just blocking a leading dash, and also keeps
    ``restore`` from ever being pointed at an ordinary branch/tag ref.
    """
    if not ref.startswith("refs/daisugi/"):
        raise ValueError(f"refusing a non-daisugi ref: {ref!r}")


def _blocking_prefix(repo: Path, name: str) -> str | None:
    """Find the path-component PREFIX of ``name`` that blocks it, if any.

    ``lstat`` on a wanted path can fail for a reason that is NOT "nothing
    is here yet": a path component earlier in ``name`` (e.g. ``a`` in
    ``a/b.txt``) exists on disk as a non-directory — a plain file, OR a
    symlink (dangling or not; a symlink's own lstat mode is never
    ``S_ISDIR`` regardless of what it points to). That is exactly what
    ``checkout-index -a -f`` force-removes to create the directory it
    needs — so if THAT prefix was not captured by the rollback, restoring
    would destroy it with no git object holding it, the same shape of loss
    as the direct file-overwrite case, one level removed.

    Walked from the repo root down; the first existing non-directory
    prefix is the one ``checkout-index`` would remove (nothing can exist
    "under" it, so there is nothing deeper to check). A missing prefix
    means nothing blocks the rest of the path either — checkout will
    simply create it, same as any ordinary "not on disk yet" path. Callers
    trigger this walk on BOTH ``ENOTDIR`` (a plain-file prefix) and
    ``ENOENT`` (which also covers a dangling-symlink prefix: resolving
    through a broken symlink fails with "no such file", not "not a
    directory" — the symlink itself is still there and still at risk).
    """
    prefix = repo
    for part in name.split("/")[:-1]:
        prefix = prefix / part
        try:
            st = prefix.lstat()
        except OSError:
            return None  # this prefix doesn't exist — nothing blocks anything deeper
        if not stat.S_ISDIR(st.st_mode):
            return str(prefix.relative_to(repo))
    return None


# Only these tree-entry MODES are real, reconstructable blobs — the shapes
# `restore()` can actually get back from a git ref. 160000 (a GITLINK: a
# nested git repo) records only a commit SHA; the repo's actual content
# lives in ITS OWN `.git`, a wholly separate object store this module
# never reads from or writes to — so a gitlink is not recoverable even
# though its path looks like an ordinary captured entry. A directory is
# never its own tree entry at all (only the files inside it are), so a
# bare directory path can never appear in this set either.
_RECOVERABLE_MODES = frozenset({"100644", "100755", "120000"})


def _recoverable_paths(repo: Path, ref: str) -> set[str]:
    """Every path ``ref``'s tree holds as a real, reconstructable blob.

    Built FRESH from ``git ls-tree -r --full-tree``, not trusted from any
    Python-side bookkeeping like :attr:`Checkpoint.covers` — ``covers`` is
    "every path ``git add -A`` walked," which is not the same claim as
    "every path this ref can actually reconstruct," and the two diverge
    exactly on a gitlink: ``git add -A`` walks it (with a warning) and
    records a path string, but the resulting tree entry (mode 160000)
    holds no retrievable content at all.
    """
    out: set[str] = set()
    raw = _git(repo, "ls-tree", "-r", "-z", "--full-tree", ref)
    for entry in filter(None, raw.split("\0")):
        meta, _, name = entry.partition("\t")
        mode = meta.split(" ", 1)[0]
        if mode in _RECOVERABLE_MODES:
            out.add(name)
    return out


def _safe(raw: str) -> str:
    """Sanitize a session/entry id for use as a git refname component.

    Mirrors ``session_tree._safe_id``'s ``.strip(".")``: a leading/trailing
    dot (or an all-dot string like ``".."``) is a git refname violation, so
    ``update-ref`` would fail on it — strip it here instead of surfacing
    that as a raw git error, or (for a component that is ALL dots) silently
    writing under a different, unexpected ref.
    """
    cleaned = "".join(ch if ch.isalnum() or ch in "._-" else "_" for ch in raw)
    return cleaned.strip(".")[:128] or "none"


def _toplevel(repo: Path) -> Path:
    """The git working-tree ROOT of ``repo``.

    ``snapshot`` and ``restore`` normalize ``repo`` to this at entry, so ONE
    path namespace — the toplevel — drives every git command and every
    filesystem join below. Without it, when ``repo`` is a SUBDIRECTORY of the
    toplevel, ``git ls-tree --full-tree`` (toplevel-relative) and a plain
    ``git ls-tree`` (subdir-relative) name the same blob differently, and
    joining a toplevel-relative name onto the subdir resolves a path the safety
    checks never examined — name ``sub/x`` under base ``T/sub`` becomes
    ``T/sub/sub/x``, which the delete/overwrite lanes would then touch without
    the rollback ever having captured it. Normalizing here also makes a
    checkpoint whole-repo scoped, the correct scope for a workspace rewind (a
    subtree-only snapshot could silently miss sibling changes on restore).

    Called only after :func:`is_repo` has confirmed a working tree, so
    ``--show-toplevel`` succeeds; an empty result (a bare repo or the ``.git``
    dir slipping past ``is_repo``) is treated as not-a-work-tree rather than
    returned, because ``Path("")`` is ``.`` and would silently rebase every
    join onto the process cwd.
    """
    top = _git(repo, "rev-parse", "--show-toplevel")
    if not top:
        raise NotAGitRepo(f"not a git working tree: {repo}")
    return Path(top)


def _git_dir(repo: Path) -> Path:
    d = Path(_git(repo, "rev-parse", "--git-dir"))
    return d if d.is_absolute() else repo / d


def _head(repo: Path) -> str | None:
    try:
        return _git(repo, "rev-parse", "--verify", "-q", "HEAD")
    except subprocess.CalledProcessError:
        return None


@contextlib.contextmanager
def _temp_index(repo: Path) -> Iterator[dict[str, str]]:
    """A ``GIT_INDEX_FILE`` env pointing at a fresh index only this call sees.

    Created then unlinked before use: git treats a missing index file as an
    empty index and initializes a clean one on first write, whereas a
    zero-byte file it opens itself reads as corrupt. Every index-touching
    git call in this module passes the returned env — the user's own
    ``.git/index`` is never opened, so their real staged state, HEAD,
    branches, tags and working tree are never touched by any of this.
    """
    git_dir = _git_dir(repo)
    with tempfile.NamedTemporaryFile(dir=git_dir, prefix="daisugi-index-", delete=False) as tmp:
        index = tmp.name
    os.unlink(index)
    try:
        yield {"GIT_INDEX_FILE": index}
    finally:
        try:
            os.unlink(index)
        except OSError:
            pass


def snapshot(
    repo: Path,
    *,
    session_id: str,
    entry_id: str,
    prefix: str = "checkpoints",
    max_file_bytes: int = 5_000_000,
) -> Checkpoint:
    """Commit the current working tree to ``refs/daisugi/<prefix>/<session>/<id>``.

    Tracked and untracked files are captured (``git add -A``, so
    ``.gitignore`` is honored the same way it always is); a regular file
    over ``max_file_bytes`` is left out of the commit and named in
    ``skipped``. Symlinks are NEVER size-skipped — a symlink's git blob is
    its (small) target text, not the bytes at the far end, and
    ``os.lstat`` (never ``stat``, which follows the link and raises on a
    dangling target) is used to tell a symlink from a regular file so a
    broken symlink is captured like any other, not miscategorized into
    ``skipped`` by an exception.
    """
    if not is_repo(repo):
        raise NotAGitRepo(f"not a git repository: {repo}")
    repo = _toplevel(repo)  # one namespace for every git command and fs join below
    ref = f"refs/daisugi/{prefix}/{_safe(session_id)}/{_safe(entry_id)}"
    with _temp_index(repo) as env:
        _git(repo, "add", "-A", "--", ".", env=env)
        listed = _git(repo, "ls-files", "-z", env=env).split("\0")
        covers: list[str] = []
        skipped: list[str] = []
        for name in filter(None, listed):
            p = repo / name
            try:
                st = p.lstat()
            except OSError:
                skipped.append(name)
                continue
            if not stat.S_ISLNK(st.st_mode) and st.st_size > max_file_bytes:
                skipped.append(name)
                continue
            covers.append(name)
        if skipped:
            _git(repo, "rm", "--cached", "-q", "--", *skipped, env=env)
        tree = _git(repo, "write-tree", env=env)
        parent = _head(repo)
        args = ["commit-tree", tree, "-m", f"daisugi {prefix} {session_id}/{entry_id}"]
        if parent:
            args += ["-p", parent]
        commit = _git(repo, *args, env={**env, **_AUTHOR})
        _git(repo, "update-ref", ref, commit)
    return Checkpoint(ref=ref, commit=commit, covers=sorted(covers), skipped=sorted(skipped))


def restore(repo: Path, *, ref: str, session_id: str, entry_id: str) -> Checkpoint:
    """Put the working tree at ``ref``. Returns the rollback checkpoint taken first.

    Never touches HEAD, the real index, branches, or tags. The invariant
    this function holds: it REFUSES (changes nothing) unless EVERY on-disk
    path it would remove or overwrite is FULLY RECOVERABLE from
    ``rollback.ref`` — present in that tree as a real blob (mode 100644 /
    100755 / 120000; see :func:`_recoverable_paths`). A path string
    appearing in :attr:`Checkpoint.covers` is NOT the same claim: ``covers``
    is "every path ``git add -A`` walked," and that walk can produce a path
    string for something that isn't a reconstructable blob at all — a
    GITLINK (a nested git repo: ``git add -A`` records only its commit SHA,
    with a warning; the repo's actual content lives in its own ``.git``, a
    wholly separate object store this module never touches) or a directory
    (never a blob, never a real tree entry). :func:`_recoverable_paths` is
    the ground truth this function checks against instead.

    Two lanes can lose data — deleting a path that shouldn't be, and
    OVERWRITING or FORCE-REMOVING a path that shouldn't be — and both are
    checked and can refuse BEFORE any mutation (:class:`RestoreRefused`):

    - the rollback snapshot could not fully capture the current working
      tree (the UNLINK lane: some path was oversize or unreadable — see
      :func:`snapshot`'s docstring);
    - a path in ``ref``'s own tree is currently on disk but is not
      recoverable from the rollback (the OVERWRITE lane — a gitignored
      path, a nested git repo, or any other shape ``git add -A``
      under-captures — yet ``checkout-index -a -f`` would still overwrite
      or force-remove it, because it IS part of the target tree, or blocks
      a path that is). This lane has a sibling one level up the path: if a
      PARENT component of a wanted path is now a non-directory — a plain
      file, or a symlink, dangling or not — ``checkout-index -a -f``
      silently FORCE-REMOVES that path to create the directory it needs;
      :func:`_blocking_prefix` finds it (triggered on both ``ENOTDIR``, a
      file blocking the way, and ``ENOENT``, which also covers a dangling
      symlink whose own resolution fails) and it is checked the same way,
      not just the leaf path itself.

    A git failure partway through the checkout itself (not this module's
    own refusal — an actual git error that even ``-f`` cannot route around,
    e.g. a permission-denied containing directory; see
    :class:`RestorePartiallyFailed`'s docstring for what this is NOT) raises
    :class:`RestorePartiallyFailed` instead: unlike the two refusals above,
    the working tree may already be in a mixed state by the time git
    errors, so the message names the rollback ref as a recovery pointer
    rather than claiming nothing changed.

    The delete-set for what a restore removes is
    ``recoverable - wanted``, computed BEFORE any mutation (a strict subset
    of what the rollback captured a moment earlier, and everything in it is
    still on disk at that point — nothing removes an "extra" path except
    this very step): only a path that is both fully recoverable from the
    rollback AND not part of the target tree is ever unlinked.
    """
    if not is_repo(repo):
        raise NotAGitRepo(f"not a git repository: {repo}")
    _require_daisugi_ref(ref)  # before any git work, and before normalization touches git
    repo = _toplevel(repo)  # one namespace for every git command and fs join below
    rollback = snapshot(repo, session_id=session_id, entry_id=entry_id, prefix="rollback")
    if rollback.skipped:
        named = ", ".join(rollback.skipped[:5])
        more = "…" if len(rollback.skipped) > 5 else ""
        raise RestoreRefused(
            f"refusing to restore: the rollback snapshot could not capture "
            f"{len(rollback.skipped)} file(s) ({named}{more}); restoring could delete "
            f"them with no way to recover. The rollback ref {rollback.ref!r} still holds "
            "what it did capture. Move or remove the oversize/unreadable file(s) and retry."
        )

    # The overwrite lane — computed and checked BEFORE any mutation (no
    # read-tree, no checkout yet). Neither `wanted` nor `recoverable` needs
    # GIT_INDEX_FILE: both read a tree object directly, never touching any
    # index. lstat (never stat) so a symlink or a dangling target is
    # inspected, not followed-and-raised. `--full-tree` on BOTH queries (here
    # and in `_recoverable_paths`) pins them to the SAME toplevel-relative
    # namespace regardless of how `repo` was reached — the split that named
    # the same blob two ways (and joined a toplevel name onto a subdir) is
    # gone.
    wanted = set(
        filter(
            None, _git(repo, "ls-tree", "-r", "-z", "--name-only", "--full-tree", ref).split("\0")
        )
    )
    recoverable = _recoverable_paths(repo, rollback.ref)
    endangered = set()
    for name in wanted:
        try:
            (repo / name).lstat()
        except OSError as exc:
            if exc.errno in (errno.ENOTDIR, errno.ENOENT):
                # `name` can't be lstat'd — either genuinely nothing is
                # there (ENOENT, no blocking prefix: `_blocking_prefix`
                # returns None, safe), or a PARENT component is currently a
                # non-directory that checkout-index -a -f would
                # force-remove to create the directory `name` needs (a
                # plain file → ENOTDIR; a symlink, including a dangling
                # one whose own target resolution fails → ENOENT — either
                # way the prefix itself is still there and still at risk).
                blocker = _blocking_prefix(repo, name)
                if blocker is not None and blocker not in recoverable:
                    endangered.add(blocker)
            continue  # not on disk yet (or handled above) — nothing further to check for `name`
        if name not in recoverable:
            endangered.add(name)
    if endangered:
        named = ", ".join(sorted(endangered)[:5])
        more = "…" if len(endangered) > 5 else ""
        raise RestoreRefused(
            f"refusing to restore: {len(endangered)} path(s) on disk ({named}{more}) would be "
            "OVERWRITTEN or FORCE-REMOVED by this checkpoint (directly, or as a non-directory "
            "parent path component checkout-index needs to clear) but are not FULLY RECOVERABLE "
            "from the rollback snapshot — a gitignored path (git add -A silently omits it), a "
            "nested git repo (recorded as a gitlink: only its commit SHA, not its content), or "
            f"some other shape git add -A under-captures. Restoring could lose their current "
            f"content with no way to recover. The rollback ref {rollback.ref!r} still holds "
            "what it did capture."
        )

    # Computed here, BEFORE any mutation: `recoverable` only ever contains
    # real blob paths that were on disk moments ago (the rollback snapshot
    # this function itself just took), and checkout never touches anything
    # outside `wanted` — so this is exactly what will still be on disk,
    # extra, once checkout finishes, with no need to re-derive it from a
    # post-checkout git query.
    deletable = recoverable - wanted

    with _temp_index(repo) as env:
        try:
            _git(repo, "read-tree", ref, env=env)
            _git(repo, "checkout-index", "-a", "-f", env=env)
        except subprocess.CalledProcessError as exc:
            raise RestorePartiallyFailed(
                f"restore failed partway through checkout ({exc}); the working tree may now be "
                "in a mixed state (some paths from the checkpoint written, others not). Recover "
                f"from the rollback ref {rollback.ref!r} — e.g. `git checkout {rollback.ref} -- "
                "<path>` for one file, or a manual read-tree/checkout-index from it for the "
                "whole tree."
            ) from exc
        for name in sorted(deletable):
            try:
                (repo / name).unlink()
            except OSError:
                pass  # already gone, or unremovable — never let a delete crash restore
    return rollback


def list_refs(repo: Path, session_id: str) -> list[str]:
    out = _git(
        repo,
        "for-each-ref",
        "--format=%(refname)",
        f"refs/daisugi/checkpoints/{_safe(session_id)}/",
    )
    return out.splitlines() if out else []
