"""Workspace snapshots as refs under refs/daisugi/, never touching the user's index or HEAD.

The blocker this file exists to pin: restore() must NEVER delete a file the
rollback snapshot did not capture. Every "dangerous case" test below sets up
a workspace state that broke the naive `present - wanted` delete-set (an
oversize untracked file, a broken symlink, a symlink to something huge, a
.gitignore'd file, a non-repo cwd, detached HEAD, a dirty index) and asserts
either full recovery or a clean refusal — never a silent, unrecoverable loss.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from opendaisugi.checkpoints import (
    Checkpoint,
    NotAGitRepo,
    RestorePartiallyFailed,
    RestoreRefused,
    is_repo,
    list_refs,
    restore,
    snapshot,
)


def _git(repo: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", str(repo), *args], check=True, capture_output=True, text=True
    ).stdout.strip()


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    r = tmp_path / "r"
    r.mkdir()
    _git(r, "init", "-q")
    _git(r, "config", "user.email", "t@t")
    _git(r, "config", "user.name", "t")
    (r / "a.txt").write_text("one\n")
    _git(r, "add", "a.txt")
    _git(r, "commit", "-q", "-m", "init")
    return r


def test_is_repo(repo: Path, tmp_path: Path):
    assert is_repo(repo) and not is_repo(tmp_path)


def test_snapshot_records_tracked_and_untracked_without_touching_status(repo: Path):
    (repo / "a.txt").write_text("two\n")
    (repo / "b.txt").write_text("new\n")
    (repo / ".gitignore").write_text("ignored.txt\n")
    (repo / "ignored.txt").write_text("x\n")
    before = _git(repo, "status", "--porcelain")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert cp.ref == "refs/daisugi/checkpoints/s1/e1"
    assert _git(repo, "rev-parse", cp.ref) == cp.commit
    assert "a.txt" in cp.covers and "b.txt" in cp.covers and ".gitignore" in cp.covers
    assert "ignored.txt" not in cp.covers
    assert _git(repo, "status", "--porcelain") == before  # index and worktree untouched
    assert _git(repo, "show", f"{cp.ref}:b.txt") == "new"
    assert list_refs(repo, "s1") == [cp.ref]


def test_oversize_files_are_skipped_and_named(repo: Path):
    (repo / "big.bin").write_bytes(b"0" * 2048)
    cp = snapshot(repo, session_id="s1", entry_id="e1", max_file_bytes=1024)
    assert "big.bin" in cp.skipped and "big.bin" not in cp.covers


def test_restore_puts_files_back_and_keeps_a_rollback(repo: Path):
    (repo / "a.txt").write_text("two\n")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    (repo / "a.txt").write_text("three\n")
    (repo / "c.txt").write_text("later\n")
    head_before = _git(repo, "rev-parse", "HEAD")
    rb = restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "a.txt").read_text() == "two\n"
    assert not (repo / "c.txt").exists()  # not in the snapshot, removed
    assert rb.ref.startswith("refs/daisugi/rollback/s1/")
    assert _git(repo, "show", f"{rb.ref}:c.txt") == "later"
    assert _git(repo, "rev-parse", "HEAD") == head_before  # HEAD unchanged


def test_refs_stay_out_of_branches_tags_and_plain_log(repo: Path):
    snapshot(repo, session_id="s1", entry_id="e1")
    assert "daisugi" not in _git(repo, "log", "--oneline")
    assert "daisugi" not in _git(repo, "branch", "-a")
    assert "daisugi" not in _git(repo, "tag")
    # honest limit: `git log --all` and `git for-each-ref` do list refs/daisugi/*
    assert "refs/daisugi/checkpoints/s1/e1" in _git(repo, "for-each-ref", "--format=%(refname)")


# --- dangerous cases: never delete a file the rollback did not capture -----


def test_restore_refuses_when_the_rollback_could_not_cover_everything(repo: Path):
    """An untracked >5MB file present at restore time skips the rollback's
    coverage of it — restore must refuse outright, not delete it anyway."""
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    before = sorted(p.name for p in repo.iterdir() if p.name != ".git")
    (repo / "huge.bin").write_bytes(b"0" * (6 * 1024 * 1024))  # over the 5MB default
    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    # nothing was deleted or checked out — the refusal changed nothing else
    assert (repo / "huge.bin").exists()
    after = sorted(p.name for p in repo.iterdir() if p.name != ".git")
    assert after == sorted(before + ["huge.bin"])


def test_broken_symlink_is_captured_not_skipped(repo: Path):
    """lstat(), never stat(): a dangling symlink must not raise and land in
    skipped — its git blob is the (small) target text, always capturable."""
    (repo / "dangling").symlink_to("nowhere/nothing")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert "dangling" in cp.covers
    assert "dangling" not in cp.skipped


def test_symlink_to_a_large_file_is_captured_not_skipped(repo: Path, tmp_path: Path):
    """A symlink's blob is its target text, not the target's bytes — must
    never be size-skipped no matter how large the thing it points at is."""
    big = tmp_path / "outside-big.bin"
    big.write_bytes(b"0" * (6 * 1024 * 1024))
    (repo / "link-to-big").symlink_to(big)
    cp = snapshot(repo, session_id="s1", entry_id="e1", max_file_bytes=1024)
    assert "link-to-big" in cp.covers
    assert "link-to-big" not in cp.skipped


def test_restore_recovers_a_broken_symlink_it_deletes_via_the_rollback_ref(repo: Path):
    """A broken symlink not part of the checkpoint gets removed on restore,
    but it was captured into the rollback first — fully recoverable."""
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # no symlink yet
    (repo / "dangling").symlink_to("nowhere/nothing")
    rb = restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert not (repo / "dangling").exists() and not (repo / "dangling").is_symlink()
    assert "dangling" in rb.covers
    assert _git(repo, "show", f"{rb.ref}:dangling") == "nowhere/nothing"


def test_restore_never_touches_a_gitignored_file(repo: Path):
    (repo / ".gitignore").write_text("secret.env\n")
    _git(repo, "add", ".gitignore")
    _git(repo, "commit", "-q", "-m", "ignore secret.env")
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # no secret.env yet
    (repo / "secret.env").write_text("API_KEY=x\n")
    restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "secret.env").read_text() == "API_KEY=x\n"


def test_restore_works_from_a_detached_head(repo: Path):
    commit = _git(repo, "rev-parse", "HEAD")
    _git(repo, "checkout", "-q", "--detach", commit)
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    (repo / "a.txt").write_text("changed\n")
    restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "a.txt").read_text() == "one\n"
    assert _git(repo, "rev-parse", "HEAD") == commit  # still detached at the same commit


def test_restore_leaves_the_real_index_untouched_when_dirty(repo: Path):
    (repo / "a.txt").write_text("staged\n")
    _git(repo, "add", "a.txt")  # a dirty, staged real index
    staged_diff_before = _git(repo, "diff", "--cached")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    # the real staged index is exactly as the user left it
    assert _git(repo, "diff", "--cached") == staged_diff_before


def test_snapshot_on_a_non_repo_raises(tmp_path: Path):
    with pytest.raises(NotAGitRepo):
        snapshot(tmp_path, session_id="s1", entry_id="e1")


def test_restore_on_a_non_repo_raises(tmp_path: Path):
    with pytest.raises(NotAGitRepo):
        restore(tmp_path, ref="refs/daisugi/checkpoints/s1/e1", session_id="s1", entry_id="e2")


def test_checkpoint_is_a_frozen_dataclass_with_the_spec_fields():
    cp = Checkpoint(ref="r", commit="c", covers=["a"], skipped=["b"])
    assert cp.ref == "r" and cp.commit == "c" and cp.covers == ["a"] and cp.skipped == ["b"]


def test_dotted_ids_are_sanitized_like_session_tree(repo: Path):
    # A leading/trailing/all dot component is an invalid git refname — strip
    # it the same way session_tree._safe_id does, rather than a raw git
    # error (or, for an all-dot input, silently writing under "none").
    cp = snapshot(repo, session_id="..", entry_id=".hidden.")
    assert ".." not in cp.ref
    assert cp.ref == "refs/daisugi/checkpoints/none/hidden"


# --- fix round 1: the OVERWRITE lane, partial-checkout recovery, ref safety ---


def test_restore_refuses_when_a_now_gitignored_file_changed_since_the_checkpoint(repo: Path):
    """The overwrite lane. `git add -A` silently OMITS a currently-gitignored
    path from a snapshot: it lands in neither `covers` nor `skipped`, so the
    `rollback.skipped` refusal never fires. But checkout-index -a -f still
    writes every path in the TARGET tree — if that path was captured, tracked,
    and un-ignored back when the checkpoint was taken, checkout overwrites the
    user's newer (now-ignored) content with the old value. Unrecoverable
    unless restore separately refuses on this."""
    (repo / "secret.env").write_text("API_KEY=old\n")
    _git(repo, "add", "secret.env")
    _git(repo, "commit", "-q", "-m", "add secret.env (not yet ignored)")
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # captures secret.env = "old"

    (repo / ".gitignore").write_text("secret.env\n")
    (repo / "secret.env").write_text("API_KEY=new-and-precious\n")  # now ignored AND changed

    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    # nothing was overwritten — the refusal happened before any mutation
    assert (repo / "secret.env").read_text() == "API_KEY=new-and-precious\n"


def test_restore_wraps_a_partial_checkout_failure_naming_the_rollback_ref(repo: Path):
    """A mid-restore git failure (checkout-index can't create a file because
    its containing directory lost write permission) must not surface as a
    raw CalledProcessError with no recovery pointer — it must name the
    rollback ref taken just before the attempt.

    Verified empirically (git's checkout-index -f is otherwise extremely
    aggressive: it force-removes a conflicting file OR a non-empty
    directory to make room, so neither of those is an actual git failure —
    only something git structurally cannot do, like write into a
    permission-denied directory, produces a non-zero exit here).
    """
    (repo / "locked").mkdir()
    (repo / "locked" / "file.txt").write_text("x\n")
    _git(repo, "add", "locked/file.txt")
    _git(repo, "commit", "-q", "-m", "add locked/file.txt")
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # captures locked/file.txt

    (repo / "locked" / "file.txt").unlink()  # not on disk — endangered check can't flag it
    os.chmod(repo / "locked", 0o555)  # read+execute only: checkout-index can't create inside it
    try:
        with pytest.raises(RestorePartiallyFailed) as exc_info:
            restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
        assert "refs/daisugi/rollback/s1/" in str(exc_info.value)
    finally:
        os.chmod(repo / "locked", 0o755)  # so pytest's tmp_path cleanup can remove it


def test_restore_refuses_a_ref_outside_the_daisugi_namespace(repo: Path):
    # A ref starting with '-' would otherwise be read by the git CLI as an
    # OPTION to read-tree/ls-tree rather than the refname it claims to be.
    for bad_ref in ("-x", "refs/heads/main", "--upload-pack=evil"):
        with pytest.raises(ValueError):
            restore(repo, ref=bad_ref, session_id="s1", entry_id="e1")


def test_git_output_with_a_leading_space_filename_is_preserved(repo: Path):
    # git ls-files -z sorts by pathname; a filename starting with a literal
    # space (legal on Linux) sorts before ordinary names, so a blanket
    # .strip() on the WHOLE -z-joined stdout string would eat that leading
    # space off the first entry before it is split on NUL.
    (repo / " leading-space.txt").write_text("hi\n")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert " leading-space.txt" in cp.covers


# --- fix round 2: the sibling overwrite lane — a blocking PARENT component ---


def test_restore_refuses_when_a_directory_prefix_is_replaced_by_a_gitignored_file(repo: Path):
    """A sibling of the overwrite-lane bug. The target tree wants `a/b.txt`,
    so `a` must be a directory for checkout to create it. But on disk, `a`
    is now a plain, gitignored FILE holding new content. `lstat("a/b.txt")`
    raises NotADirectoryError (ENOTDIR), not ENOENT — the old endangered
    check's blanket `except OSError: continue` treated ANY OSError as
    "not on disk yet, nothing to lose", missing that `checkout-index -a -f`
    would FORCE-REMOVE the file `a` to create the directory `a/`. Since `a`
    is gitignored, it is in neither `covers` nor `skipped`, so no other
    check catches it either — the file and its content would be destroyed
    with no git object holding them."""
    (repo / "a").mkdir()
    (repo / "a" / "b.txt").write_text("original\n")
    _git(repo, "add", "a/b.txt")
    _git(repo, "commit", "-q", "-m", "add a/b.txt")
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # captures a/b.txt, "a" is a dir

    import shutil

    shutil.rmtree(repo / "a")
    (repo / ".gitignore").write_text("a\n")
    (repo / "a").write_text("precious file content, not a directory\n")  # now ignored + a file

    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    # nothing was overwritten or force-removed — the refusal happened before any mutation
    assert (repo / "a").is_file()
    assert (repo / "a").read_text() == "precious file content, not a directory\n"


def test_restore_refuses_when_a_deeper_prefix_component_is_a_gitignored_file(repo: Path):
    """The same bug, one level deeper: the target tree wants `a/b/c.txt`;
    `a` stays a real directory, but `a/b` — a normal subdirectory when the
    checkpoint was taken — has since become a gitignored file. The blocking
    prefix walk must find `a/b` specifically (not stop at `a`, which is
    still fine), and `a/b` must not be in `rollback.covers` for the refusal
    to fire."""
    (repo / "a" / "b").mkdir(parents=True)
    (repo / "a" / "b" / "c.txt").write_text("original\n")
    _git(repo, "add", "a/b/c.txt")
    _git(repo, "commit", "-q", "-m", "add a/b/c.txt")
    cp = snapshot(repo, session_id="s1", entry_id="e1")

    import shutil

    shutil.rmtree(repo / "a" / "b")
    (repo / ".gitignore").write_text("a/b\n")
    (repo / "a" / "b").write_text("precious, not a directory\n")

    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "a" / "b").is_file()
    assert (repo / "a" / "b").read_text() == "precious, not a directory\n"


# --- fix round 3: the CLASS of the bug, not just the instances -------------
#
# The root cause: restore()'s safety relied on `covers` MEMBERSHIP (a path
# string in the list `git add -A` walked), but that hides two more ways a
# path can be "in covers" without being genuinely recoverable: a NESTED GIT
# REPO is recorded as a gitlink (mode 160000) — only its commit SHA, not its
# content, which lives in its own separate .git; and a directory is never a
# real blob at all. The fix replaces `covers` membership with a fresh,
# mode-filtered `_recoverable_paths(repo, rollback.ref)` — only mode 100644
# (file) / 100755 (exec) / 120000 (symlink) entries in the ACTUAL rollback
# tree count as recoverable.


def test_restore_refuses_when_a_nested_git_repo_occupies_a_wanted_file_path(repo: Path):
    """The headline sibling: the target tree wants `x` as a plain file.
    Currently `x` is a NESTED GIT REPO with its own commit. `git add -A`
    records it as a GITLINK (mode 160000) in the rollback tree — a path
    string that a naive `covers`-membership check would call "captured",
    but the nested repo's actual content lives in `x/.git`, a completely
    separate object store this module never reads from or writes to.
    Restoring must refuse and leave the nested repo (and its commit) fully
    intact — not destroy someone's whole embedded checkout."""
    (repo / "x").write_text("plain file, not a repo\n")
    _git(repo, "add", "x")
    _git(repo, "commit", "-q", "-m", "add x as a plain file")
    cp = snapshot(repo, session_id="s1", entry_id="e1")  # captures x = plain file

    (repo / "x").unlink()
    (repo / "x").mkdir()
    _git(repo / "x", "init", "-q")
    _git(repo / "x", "config", "user.email", "t@t")
    _git(repo / "x", "config", "user.name", "t")
    (repo / "x" / "inner.txt").write_text("nested repo content\n")
    _git(repo / "x", "add", "inner.txt")
    _git(repo / "x", "commit", "-q", "-m", "inner commit")
    nested_commit = _git(repo / "x", "rev-parse", "HEAD")

    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "x" / ".git").is_dir()
    assert _git(repo / "x", "rev-parse", "HEAD") == nested_commit
    assert (repo / "x" / "inner.txt").read_text() == "nested repo content\n"


def test_restore_refuses_when_a_gitignored_dangling_symlink_blocks_a_wanted_path(repo: Path):
    """The second sibling: the blocking-prefix walk was gated on
    errno.ENOTDIR only. A gitignored, DANGLING symlink parent makes
    `lstat("a/b.txt")` raise ENOENT — not ENOTDIR (resolving through `a`
    fails because ITS OWN target doesn't exist) — which the old code
    treated identically to "nothing there yet, nothing to lose." But `a`
    the symlink itself IS on disk and would be force-removed by
    checkout-index to create the directory `a/` it needs."""
    (repo / "a").mkdir()
    (repo / "a" / "b.txt").write_text("original\n")
    _git(repo, "add", "a/b.txt")
    _git(repo, "commit", "-q", "-m", "add a/b.txt")
    cp = snapshot(repo, session_id="s1", entry_id="e1")

    import shutil

    shutil.rmtree(repo / "a")
    (repo / ".gitignore").write_text("a\n")
    (repo / "a").symlink_to("nowhere/nothing")  # gitignored, dangling

    with pytest.raises(RestoreRefused):
        restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert (repo / "a").is_symlink()
    assert not (repo / "a").exists()  # still dangling — untouched, not resolved or removed


# --- fix round 4: git PATH-NAMESPACE mixing when repo is a git SUBDIRECTORY ---
#
# gate.py passes Path(cwd) straight through, and cwd is routinely a monorepo
# SUBDIRECTORY (is_repo blesses subdirs on purpose). When `repo` is below the
# git toplevel, restore() mixed two path namespaces:
#   - `_recoverable_paths` used `git ls-tree --full-tree` -> TOPLEVEL-relative
#     names (e.g. "sub/victim");
#   - `wanted` used `git ls-tree` WITHOUT --full-tree -> subdir-relative names
#     (e.g. "victim");
# and EVERY filesystem join used the subdir as its base. So a toplevel-relative
# name "sub/victim" in the delete-set resolved to (T/sub / "sub/victim") =
# T/sub/sub/victim — a path the endangered gate never checked and, if
# gitignored, the rollback never captured. The fix normalizes `repo` to the git
# toplevel at the entry of both snapshot() and restore(), so ONE namespace
# drives every git command and every filesystem join.


def test_restore_from_a_subdir_does_not_destroy_a_namespace_shifted_gitignored_file(repo: Path):
    """The reproduced data-loss lane. Call restore with `repo` set to a
    SUBDIRECTORY of the git toplevel; a precious gitignored file sits at the
    path a toplevel-relative delete-set name resolves to when it is wrongly
    joined onto the subdir (T/sub/sub/victim). Under the namespace-mixing bug
    the unlink phase destroyed it and restore returned success; with the
    toplevel-normalization fix the file is out of the operate set entirely and
    is never touched."""
    sub = repo / "sub"
    sub.mkdir()
    (sub / "gone.txt").write_text(
        "g\n"
    )  # keeps `sub` non-empty so the checkpoint has a wanted path
    cp = snapshot(sub, session_id="s1", entry_id="e1")

    # Now shift the world: ignore sub/sub, drop the wanted path from disk, and
    # plant a precious gitignored file exactly where the mixed namespace lands.
    (repo / ".gitignore").write_text("sub/sub/\n")
    (sub / "gone.txt").unlink()
    (sub / "victim").write_text("v1\n")  # captured into the rollback (non-ignored)
    (sub / "sub").mkdir()
    precious = sub / "sub" / "victim"  # == (sub / "sub/victim"): the mixed-namespace target
    precious.write_text("PRECIOUS — irreplaceable, gitignored\n")

    restore(sub, ref=cp.ref, session_id="s1", entry_id="e2")

    # The only assertion that matters: the gitignored file the rollback could
    # never recover is still on disk, untouched.
    assert precious.exists()
    assert precious.read_text() == "PRECIOUS — irreplaceable, gitignored\n"


def test_snapshot_from_a_subdir_covers_the_whole_repo_not_just_the_subtree(repo: Path):
    """Checkpoints are whole-repo scoped: a snapshot taken from a subdir must
    capture sibling files above/beside it, so a later restore can't silently
    miss changes outside the subtree. `a.txt` (a toplevel sibling of `sub`)
    must be in a snapshot taken from `sub`."""
    sub = repo / "sub"
    sub.mkdir()
    (sub / "inside.txt").write_text("x\n")
    cp = snapshot(sub, session_id="s1", entry_id="e1")
    assert "a.txt" in cp.covers  # the toplevel sibling, captured despite the subdir cwd
    assert "sub/inside.txt" in cp.covers  # named toplevel-relative, one consistent namespace


# --- the WORKSPACE repo's own config must never get to run a program of
# its choosing during a checkpoint --------------------------------------
#
# The gate calls `git add -A` (and friends) in the AGENT's own workspace
# repo, under `--checkpoints`. That repo's local .git/config,
# .gitattributes and .git/info/attributes are all agent-controlled, so
# `git config core.fsmonitor "sh -c '...'"` makes the next checkpoint run a
# program the agent chose, not git's own. Each test below plants the
# marker-touching PoC for one such mechanism, snapshots, and asserts BOTH
# that the marker was never created AND that the checkpoint still worked
# (a content match, not just "didn't crash": this must be a real fix, not
# an add that now silently fails closed).


def test_snapshot_does_not_run_the_repos_own_fsmonitor(repo: Path, tmp_path: Path):
    marker = tmp_path / "fsmonitor-ran"
    _git(repo, "config", "core.fsmonitor", f"sh -c 'touch {marker}'")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert not marker.exists()
    assert _git(repo, "show", f"{cp.ref}:a.txt") == "one"


def test_snapshot_does_not_run_a_config_defined_hook(repo: Path, tmp_path: Path):
    """git 2.36's hook.<name>.command/.event is a SECOND hook mechanism,
    entirely separate from the hooks-directory core.hooksPath disables."""
    marker = tmp_path / "confighook-ran"
    _git(repo, "config", "hook.evil.command", f"sh -c 'touch {marker}'")
    _git(repo, "config", "hook.evil.event", "post-index-change")
    reftx_marker = tmp_path / "reftx-ran"
    _git(repo, "config", "hook.evil2.command", f"sh -c 'touch {reftx_marker}'")
    _git(repo, "config", "hook.evil2.event", "reference-transaction")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert not marker.exists()
    assert not reftx_marker.exists()
    assert _git(repo, "show", f"{cp.ref}:a.txt") == "one"


def test_snapshot_neutralizes_a_repo_configured_clean_filter(repo: Path, tmp_path: Path):
    """A clean filter driver named by .gitattributes and defined in the
    repo's own config: the same shape of hole as fsmonitor, one step
    removed. `required = true` (a real git-lfs default) is set too. The
    fix must not turn a legitimate required-filter repo's checkpoints into
    a permanent failure just by neutralizing the command."""
    marker = tmp_path / "filter-ran"
    (repo / ".gitattributes").write_text("*.txt filter=evil\n")
    _git(repo, "config", "filter.evil.clean", f"sh -c 'touch {marker}; cat'")
    _git(repo, "config", "filter.evil.required", "true")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    assert not marker.exists()
    assert _git(repo, "show", f"{cp.ref}:a.txt") == "one"  # content preserved


def test_restore_neutralizes_a_repo_configured_smudge_filter(repo: Path, tmp_path: Path):
    marker = tmp_path / "smudge-ran"
    (repo / ".gitattributes").write_text("*.txt filter=evil\n")
    _git(repo, "config", "filter.evil.clean", "cat")
    cp = snapshot(repo, session_id="s1", entry_id="e1")
    _git(repo, "config", "filter.evil.smudge", f"sh -c 'touch {marker}; cat'")
    (repo / "a.txt").write_text("changed\n")
    restore(repo, ref=cp.ref, session_id="s1", entry_id="e2")
    assert not marker.exists()
    assert (repo / "a.txt").read_text() == "one\n"
