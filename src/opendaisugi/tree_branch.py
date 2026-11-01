"""Branching: one task, N children in parallel, each in its own worktree.

``run_branches`` is what ``daisugi tree run`` does when the request carries
``attempts``. In order: make a git worktree for each branch; reserve every
branch's budget under one lock (all or none); start the children, at most
``parallel`` at once; stop every process they left; fold a fenced child's
own object store away; end them in branch order; run the starter's hard
checks in each worktree; rank the attempts with ``daisugi rank``, ask a
judge when asked to; open a card for the choice. The parent goes on with the
leader and the owner looks at the card later (RK-10). Every branch row in the
tree ledger names the ranking and the reason it was started.

A branch that did not run, or fails a required check, is out. If none is
left, nothing is chosen. The checks come from the starter's side only
(W3K-11): a model cannot write the check that judges its own branch.
"""

from __future__ import annotations

import ctypes
import hashlib
import os
import shutil
import signal
import subprocess
import sys
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from opendaisugi import agent_profiles, branch_git, rank, rank_judge, tree, tree_run
from opendaisugi.models import Envelope

DEFAULT_PARALLEL = 4

#: How long the sweep waits after SIGTERM before SIGKILL, and how long it
#: may take after that to stop what the children left behind.
KILL_GRACE_S = 2.0
SWEEP_S = 5.0
_PR_SET_CHILD_SUBREAPER = 36
_PR_GET_CHILD_SUBREAPER = 37
SWEEP_TEXT = "a process a branch child left could not be stopped; nothing of the branches was read"


class BranchError(Exception):
    """A branch could not be made; the text says why."""


def branches_dir(data_dir: Path) -> Path:
    return Path(data_dir) / "tree" / "branches"


def _git_top(cwd: str, git: Callable[..., Any]) -> str:
    try:
        r = git(
            ["git", *branch_git.GUARD, "-C", cwd, "rev-parse", "--show-toplevel"],
            capture_output=True,
        )
    except OSError as e:
        raise BranchError(f"branching needs a git workspace; {cwd} is not one") from e
    if r.returncode != 0:
        raise BranchError(f"branching needs a git workspace; {cwd} is not one")
    return r.stdout.decode("utf-8", "replace").strip()


def repo_of(cwd: str, git: Callable[..., Any]) -> tuple[str, str]:
    """The commit HEAD names and the repository's git directory, as the
    parent sees them now. The parent records both on the branch rows."""
    top = _git_top(cwd, git)
    head = git(["git", *branch_git.GUARD, "-C", top, "rev-parse", "HEAD"], capture_output=True)
    base = head.stdout.decode("utf-8", "replace").strip() if head.returncode == 0 else ""
    common = branch_git.common_dir(top, git)
    if not tree.is_object_id(base) or common is None:
        raise BranchError(f"branching needs a git workspace with a commit; {cwd} has none")
    return base, common


def make_worktree(
    cwd: str,
    data_dir: Path,
    session: str,
    *,
    base: str | None = None,
    git: Callable[..., Any] = subprocess.run,
) -> Path:
    """A detached worktree of ``cwd``'s repository at ``base`` (HEAD when
    none), for one branch."""
    top = _git_top(cwd, git)
    if base is not None and not tree.is_object_id(base):
        raise BranchError(f"the worktree for {session} could not be made: base is not a commit id")
    dest = branches_dir(data_dir)
    dest.mkdir(mode=0o700, parents=True, exist_ok=True)
    dest = dest / session
    r = git(
        [
            "git",
            *branch_git.GUARD,
            "-C",
            top,
            "worktree",
            "add",
            "--detach",
            str(dest),
            base or "HEAD",
        ],
        capture_output=True,
    )
    if r.returncode != 0:
        err = (r.stderr or b"").decode("utf-8", "replace").strip().splitlines()
        raise BranchError(f"the worktree for {session} could not be made: {err[-1] if err else ''}")
    dest.chmod(0o700)
    return dest


def branch_ref(data_dir: Path, session: str) -> tuple[str, str] | None:
    """The base commit and repository the parent recorded for a branch."""
    n = tree.read_tree(data_dir).nodes.get(session)
    if n is None or not branch_git.usable(n.base, n.repo):
        return None
    return n.base, n.repo  # type: ignore[return-value]


def remove_worktree(
    path: Path, repo: str | None = None, *, git: Callable[..., Any] = subprocess.run
) -> None:
    """Remove a branch worktree and forget it. Never raises. ``repo`` is the
    git directory the parent recorded; the worktree's own ``.git`` pointer is
    never asked, because the child can rewrite it."""
    if repo:
        try:
            git(
                [
                    "git",
                    *branch_git.GUARD,
                    f"--git-dir={repo}",
                    "worktree",
                    "remove",
                    "--force",
                    str(path),
                ],
                capture_output=True,
            )
            git(
                ["git", *branch_git.GUARD, f"--git-dir={repo}", "worktree", "prune"],
                capture_output=True,
            )
        except OSError:
            pass
    shutil.rmtree(path, ignore_errors=True)


def discard_worktree(path: Path, repo: str, *, git: Callable[..., Any] = subprocess.run) -> None:
    """Remove a branch worktree whose fold failed, and its admin directory
    in ``repo`` whatever the child did to it: the parent takes its rights
    back and drops a lock first, then asks git, then removes what is left of
    ``<repo>/worktrees/<name>`` itself. Never raises."""
    admin = os.path.join(repo, "worktrees", Path(path).name)
    branch_git.reclaim(admin)
    branch_git._drop(os.path.join(admin, "locked"))
    remove_worktree(path, repo, git=git)
    branch_git.reclaim(admin)
    branch_git._drop(admin)
    try:
        git(
            ["git", *branch_git.GUARD, f"--git-dir={repo}", "worktree", "prune"],
            capture_output=True,
        )
    except OSError:
        pass


def _rmdir_if_empty(path: Path) -> None:
    try:
        path.rmdir()
    except OSError:
        pass


def _diff(worktree: Path, base: str, repo: str, git: Callable[..., Any]) -> bytes:
    return branch_git.diff(worktree, base, repo, git) or b""


def branch_hash(
    worktree: Path, base: str, repo: str, answer: str, *, git: Callable[..., Any] = subprocess.run
) -> str:
    """``sha256:`` over the worktree's diff against its base, a NUL byte, and
    the child's answer (W3K-10)."""
    h = hashlib.sha256()
    h.update(_diff(worktree, base, repo, git))
    h.update(b"\x00")
    h.update(answer.encode("utf-8"))
    return "sha256:" + h.hexdigest()


def diff_lines(
    worktree: Path, base: str, repo: str, git: Callable[..., Any] = subprocess.run
) -> int:
    text = _diff(worktree, base, repo, git).decode("utf-8", "replace")
    n = 0
    for line in text.splitlines():
        if line.startswith(("+", "-")) and not line.startswith(("+++", "---")):
            n += 1
    return n


_SCRUB_KEEP = ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TERM")


def scrubbed(environ: dict[str, str]) -> dict[str, str]:
    """The environment a check runs in: no provider key, nothing the child
    was kept from (W3K-11)."""
    return {
        k: v for k, v in environ.items() if k in _SCRUB_KEEP and not agent_profiles.is_key_name(k)
    }


def run_checks(
    checks: list[dict[str, Any]],
    worktree: Path,
    env: dict[str, str],
    timeout_s: int,
    *,
    ran: bool = True,
) -> list[dict[str, Any]]:
    """Run each hard check in the worktree: exit 0 is ``pass``, anything
    else, a timeout and a start failure are ``fail``. When the branch did not
    run, no check runs and each is ``not_run``."""
    out = []
    for c in checks:
        res = "not_run"
        if ran:
            try:
                p = subprocess.run(
                    list(c["argv"]),
                    cwd=str(worktree),
                    env=env,
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    timeout=timeout_s,
                    check=False,
                )
                res = "pass" if p.returncode == 0 else "fail"
            except (OSError, subprocess.SubprocessError):
                res = "fail"
        out.append({"name": c["name"], "required": bool(c.get("required", True)), "result": res})
    return out


@dataclass
class BranchRun:
    status: str  # ran, failed or refused
    leader_session: str | None = None
    answer: str | None = None
    choice_id: str | None = None
    ranking: dict[str, Any] | None = None
    branches: list[dict[str, Any]] = field(default_factory=list)
    reasons: list[str] = field(default_factory=list)
    ranking_id: str | None = None
    ask_id: str | None = None
    usage: bool = False  # the fence is wrong for a branch: exit 2

    def doc(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "session": self.leader_session,
            "answer": self.answer,
            "reasons": self.reasons,
            "branches": self.branches,
            "leader": self.leader_session,
            "choice_id": self.choice_id,
            "ranking": self.ranking,
            "ranking_id": self.ranking_id,
            "ask_id": self.ask_id,
        }


WEAK_IPC = "--allow-weak-ipc"
WEAK_IPC_TEXT = (
    "A branch run cannot take --allow-weak-ipc: without the socket fence a child can ask "
    "the session bus to start a process the run cannot stop. Remove the flag, or run the "
    "branches without a fence. Nothing was started."
)


NO_FENCE = "--no-fence"
NO_FENCE_TEXT = (
    "A branch fence cannot take --no-fence: the run would treat the branches as fenced while "
    "nothing confines them. Remove the flag, or run the branches without a fence. "
    "Nothing was started."
)


def reftable_text(repo: str, fmt: str) -> str:
    return (
        f"{repo} keeps its refs in the {fmt} format, and a fenced branch run can fold a "
        "child's refs only in the files format. Run the branches without a fence, or convert "
        "the repository with git refs migrate --ref-format=files. Nothing was started."
    )


def _refuse(reasons: list[str], usage: bool = False) -> BranchRun:
    return BranchRun("refused", reasons=reasons, usage=usage)


def _children() -> set[tuple[int, bytes]]:
    """This process's children now, as (pid, start time) pairs, so a pid
    used again is not taken for one seen before."""
    me = os.getpid()
    out: set[tuple[int, bytes]] = set()
    try:
        names = os.listdir("/proc")
    except OSError:
        return out
    for n in names:
        if not n.isdigit():
            continue
        try:
            with open(f"/proc/{n}/stat", "rb") as f:
                data = f.read()
        except OSError:
            continue
        rest = data[data.rfind(b")") + 2 :].split()
        if len(rest) > 19 and rest[1] == str(me).encode():
            out.add((int(n), rest[19]))
    return out


class _Sweep:
    """Stop every process a branch child leaves behind, before the parent
    reads a worktree. The child's own process group is killed when it exits,
    but a process that made a new session is not in that group. So the tree
    run makes itself the child subreaper while the children run: every
    process they leave, at any depth, becomes a child of this process when
    its parent ends. ``clear`` stops each child this process did not have
    before, waits for it, and goes on until none is left. Linux only, as the
    fence is."""

    def __init__(self) -> None:
        self.libc: Any = None
        self.was = 0
        self.before: set[tuple[int, bytes]] = set()
        if not sys.platform.startswith("linux"):
            return
        try:
            libc = ctypes.CDLL(None, use_errno=True)
            was = ctypes.c_int(0)
            libc.prctl(_PR_GET_CHILD_SUBREAPER, ctypes.byref(was), 0, 0, 0)
            if libc.prctl(_PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
                return
        except (OSError, AttributeError):
            return
        self.libc, self.was = libc, was.value
        self.before = _children()

    def clear(self) -> bool:
        """Stop and reap what the children left: SIGTERM first, so a
        server (a resident warden the child's fence started) can shut down
        cleanly, then SIGKILL after KILL_GRACE_S. False when something is
        still there after that and SWEEP_S more."""
        if self.libc is None:
            return True
        now = time.monotonic()
        term_until = now + KILL_GRACE_S
        end = term_until + SWEEP_S
        termed: set[int] = set()
        ok = False
        while True:
            left = [pid for pid, _ in _children() - self.before]
            if not left:
                ok = True
                break
            now = time.monotonic()
            if now > end:
                break
            for pid in left:
                if now < term_until and pid in termed:
                    continue
                try:
                    os.kill(pid, signal.SIGTERM if now < term_until else signal.SIGKILL)
                except OSError:
                    pass
                termed.add(pid)
            for pid in left:
                try:
                    os.waitpid(pid, os.WNOHANG)
                except OSError:
                    pass
            time.sleep(0.01)
        self.libc.prctl(_PR_SET_CHILD_SUBREAPER, self.was, 0, 0, 0)
        return ok


def run_branches(
    data_dir: Path,
    warden_root: Path,
    *,
    parent: str,
    envelope: Envelope,
    profiles: agent_profiles.Profiles | None,
    profile_name: str | None,
    model: str | None,
    tokens: int | None,
    turns: int | None,
    parent_tokens_used: int | None,
    parent_turns_used: int | None,
    task: str,
    tools: list[str],
    max_turns: int | None,
    cwd: str,
    timeout_s: int,
    sprig_binary: str,
    environ: dict[str, str],
    attempts: int,
    why: str,
    parallel: int | None,
    checks: list[dict[str, Any]],
    judge: str | None,
    fence: list[str] | None,
    start: Callable[..., Any] | None = None,
    timeout_ms: int = tree.DEFAULT_TIMEOUT_MS,
    git: Callable[..., Any] = subprocess.run,
) -> BranchRun:
    start = start or tree_run.run_node
    parallel = max(1, min(attempts, parallel if parallel else DEFAULT_PARALLEL))
    try:
        top = _git_top(cwd, git)
        base, repo = repo_of(top, git)
    except BranchError as e:
        return _refuse([str(e)])
    # The sessions, minted in order from the parent's proposals so far.
    t = tree.read_tree(data_dir)
    n0 = t.parent_rows(parent)
    sessions: list[str] = []
    k = n0
    while len(sessions) < attempts:
        sid = tree.mint_session(parent, k)
        k += 1
        if sid in t.nodes or tree._envelope_file(warden_root, sid).exists():
            continue
        sessions.append(sid)
    if fence is not None:
        words = fence[1:]
        if any(w == WEAK_IPC or w.startswith(WEAK_IPC + "=") for w in words):
            return _refuse([WEAK_IPC_TEXT], usage=True)
        if any(w == NO_FENCE or w.startswith(NO_FENCE + "=") for w in words):
            return _refuse([NO_FENCE_TEXT], usage=True)
        fmt = branch_git.ref_format(repo, git)
        if fmt != "files":
            return _refuse([reftable_text(repo, fmt)], usage=True)
        env_file = Path(warden_root) / "envelopes" / "default.json"
        for i, w in enumerate(words):
            if w == "--envelope" and i + 1 < len(words):
                env_file = Path(words[i + 1])
            elif w.startswith("--envelope="):
                env_file = Path(w.partition("=")[2])
        # The fence reads a relative grant against its workspace, which for a
        # branch child is the child's worktree.
        for sid in sessions:
            wt = str(branches_dir(data_dir) / sid)
            grant = tree_run.fence_covers(words, env_file, repo, environ.get("HOME", ""), wt)
            if grant is not None:
                return _refuse(
                    [
                        f"A branch child needs the repository's git directory read only, but "
                        f"the fence also grants {grant} read and write, which covers {repo}. "
                        "Nothing was started."
                    ],
                    usage=True,
                )
    ranking_id = (
        "br_" + hashlib.sha256(rank.canonical([parent, task, sessions]).encode()).hexdigest()[:12]
    )
    made: list[Path] = []
    try:
        for sid in sessions:
            made.append(make_worktree(top, data_dir, sid, base=base, git=git))
    except BranchError as e:
        for w in made:
            remove_worktree(w, repo, git=git)
        _rmdir_if_empty(branches_dir(data_dir))
        return _refuse([str(e)])

    def undo_worktrees() -> None:
        for w in made:
            remove_worktree(w, repo, git=git)
        _rmdir_if_empty(branches_dir(data_dir))

    prof = profile_name if profile_name not in (None, agent_profiles.DEFAULT) else None
    sp = tree.spawn_branches(
        data_dir,
        warden_root,
        envelope,
        parent=parent,
        sessions=sessions,
        tokens=tokens,
        turns=turns,
        timeout_ms=timeout_ms,
        profile=prof,
        model=model,
        parent_tokens_used=parent_tokens_used,
        parent_turns_used=parent_turns_used,
        branch_of=ranking_id,
        why=why,
        base=base,
        repo=repo,
    )
    if sp.status != "started":
        undo_worktrees()
        out = _refuse(sp.reasons)
        if sp.status == "asked":
            out.status, out.ask_id = "asked", sp.ask_id
        return out

    def one(i: int) -> tuple[Any, int]:
        t0 = time.monotonic()
        try:
            nr = start(
                data_dir,
                warden_root,
                parent=parent,
                session=sessions[i],
                envelope=envelope,
                profiles=profiles,
                profile_name=profile_name,
                model=model,
                tokens=tokens,
                turns=turns,
                parent_tokens_used=parent_tokens_used,
                parent_turns_used=parent_turns_used,
                task=task,
                tools=tools,
                max_turns=max_turns,
                cwd=str(made[i]),
                timeout_s=timeout_s,
                sprig_binary=sprig_binary,
                environ=environ,
                fence=fence,
                reserved=True,
            )
        except Exception as e:  # a branch that raises is a failed branch
            nr = tree_run.NodeRun("failed", sessions[i], reasons=[f"{type(e).__name__}: {e}"])
        return nr, int((time.monotonic() - t0) * 1000)

    sweep = _Sweep()
    with ThreadPoolExecutor(max_workers=parallel) as pool:
        results = list(pool.map(one, range(attempts)))
    swept = sweep.clear()

    # End every branch, in branch order, with its own use (W3K-13).
    for sid, (nr, _) in zip(sessions, results, strict=True):
        tree.end_subtree(data_dir, warden_root, sid, include_self=False)
        try:
            tree.end(
                data_dir, warden_root, sid, tokens_used=nr.tokens_used, turns_used=nr.turns_used
            )
        except tree.TreeError:
            pass  # already ended by a starter that ended its own node
    if not swept:
        return _refuse([SWEEP_TEXT])
    # A fenced child wrote its objects to a store of its own. Fold it away
    # before any read. In a fenced run every worktree is folded, whatever the
    # child left: it may have deleted its store, or taken the parent's rights
    # on its admin directory. A branch that does not fold is out, and its
    # worktree is removed, so the repository names nothing of the child's.
    folded: list[bool] = []
    for wt in made:
        ok = (fence is None and not os.path.lexists(branch_git.store(repo, wt))) or (
            branch_git.fold(repo, wt, base, git)
        )
        if not ok:
            discard_worktree(wt, repo, git=git)
        folded.append(ok)

    env = scrubbed(environ)
    prof_obj = profiles.lookup(profile_name)[0] if profiles is not None else None
    author = model or (prof_obj.model if prof_obj is not None else None) or "haiku"
    atts: list[dict[str, Any]] = []
    branches: list[dict[str, Any]] = []
    answers: dict[str, str] = {}
    for i, (sid, (nr, wall_ms)) in enumerate(zip(sessions, results, strict=True)):
        ran = nr.status in ("started", "ran") and folded[i]
        answer = nr.answer or ""
        answers[sid] = answer
        wt = made[i]
        tests = [{"name": "the child ran", "required": True, "result": "pass" if ran else "fail"}]
        tests += run_checks(checks, wt, env, timeout_s, ran=ran)
        cost: dict[str, int] = {"wall_ms": wall_ms, "diff_lines": diff_lines(wt, base, repo, git)}
        if nr.tokens_used is not None:
            cost["tokens"] = nr.tokens_used
        first = answer.strip().splitlines()[0][:80] if answer.strip() else ""
        att: dict[str, Any] = {
            "id": sid,
            "content_hash": branch_hash(wt, base, repo, answer, git=git),
            "author": author,
            "edge_proof": "ok",
            "started_by_parent": True,
            "tests": tests,
            "cost": cost,
            "where": {"kind": "worktree", "path": str(wt), "base": base, "repo": repo},
        }
        if first:
            att["previews"] = {"summary": first}
        atts.append(att)
        branches.append(
            {
                "session": sid,
                "status": "ran" if ran else "failed",
                "worktree": str(wt),
                "tokens_used": nr.tokens_used,
                "turns_used": nr.turns_used,
                "tests": tests,
            }
        )
    doc: dict[str, Any] = {
        "ranking_id": ranking_id,
        "task": task,
        "project": top,
        "attempts": atts,
    }
    if judge:
        doc["policy"] = {"judges": [judge]}
    r, res = rank_judge.fit_doc(doc, data_dir)
    if judge and profiles is not None:
        while res["next"] is not None:
            out = rank_judge.judge_once(
                doc, data_dir, profiles=profiles, env=environ, judge=None, now=rank.now()
            )
            if not out["recorded"]:
                break
            res = out["fit"]
    if res["status"] == "none_survived":
        return BranchRun(
            "failed",
            branches=branches,
            ranking=res,
            ranking_id=ranking_id,
            reasons=[f"{e['id']}: {'; '.join(e['reasons'])}" for e in res["eliminated"]],
        )
    choice_id = None
    if len(res["order"]) > 1:
        row = rank.opened_row(
            r,
            res,
            now=rank.now(),
            run={"run_id": ranking_id, "downstream": []},
            adopt=f"daisugi tree adopt {res['leader']}",
        )
        choice_id = row["choice_id"]
        if all(c.id != choice_id for c in rank.read_cards(data_dir)):
            rank.append_rows(data_dir, [row])
    leader = res["leader"]
    return BranchRun(
        "ran",
        leader_session=leader,
        answer=answers.get(leader),
        choice_id=choice_id,
        ranking=res,
        branches=branches,
        ranking_id=ranking_id,
    )


def prune_branches(
    data_dir: Path, *, git: Callable[..., Any] = subprocess.run, now: float | None = None
) -> list[str]:
    """Remove the worktrees of branches whose card is closed, and those of
    ended branches that have no card once they are older than the card decay
    (14 days). A running branch and a branch of an open card stay, and so
    does a lone survivor that no card holds: ``tree adopt`` is the only way
    its edits reach the parent. Returns the sessions removed, in name order."""
    base = branches_dir(data_dir)
    if not base.is_dir():
        return []
    now = rank.now() if now is None else now
    t = tree.read_tree(data_dir)
    cards = {c.opened["ranking_id"]: c for c in rank.read_cards(data_dir)}
    removed = []
    for d in sorted(base.iterdir()):
        node = t.nodes.get(d.name)
        if node is None or node.state != "ended":
            continue
        card = cards.get(node.branch_of or "")
        if card is None:
            aged = node.ended_at is not None and now - node.ended_at > rank.DECAY_SECONDS
            if not aged:
                continue
        elif card.close is None:
            continue
        # A child may have taken the parent's rights on its admin dir, and a
        # sweep refusal skipped the fold: take them back first (R5-1).
        if node.repo:
            discard_worktree(d, node.repo, git=git)
        else:
            remove_worktree(d, node.repo, git=git)
        removed.append(d.name)
    _rmdir_if_empty(base)
    return removed


@dataclass
class Adopt:
    status: str  # adopted, nothing or refused
    session: str
    files: list[str] = field(default_factory=list)
    why: str = ""

    def doc(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "session": self.session,
            "files": self.files,
            "why": self.why,
        }


def _run(git: Callable[..., Any], argv: list[str], **kw: Any) -> Any:
    return git(argv, capture_output=True, **kw)


def adopt(
    data_dir: Path,
    session: str,
    *,
    cwd: str | None = None,
    git: Callable[..., Any] = subprocess.run,
) -> Adopt:
    """Apply a branch's edits to the parent's working tree (``git apply
    --3way`` of the branch's diff against its base commit).

    Refuses, and writes nothing, when the branch has no worktree, when a
    file the patch touches has uncommitted changes in the parent's tree, or
    when the patch does not apply cleanly. Nothing is copied by any other
    path: the owner chooses this."""
    wt = branches_dir(data_dir) / session
    try:
        tree._check_session(session, "")
    except tree.TreeError:
        return Adopt("refused", session, why=f"No branch worktree for {session}.")
    if not wt.is_dir():
        return Adopt("refused", session, why=f"No branch worktree for {session}.")
    ref = branch_ref(data_dir, session)
    if ref is None:
        return Adopt(
            "refused", session, why=f"The ledger has no recorded base for {session}; not adopted."
        )
    base, repo = ref
    if not branch_git.owns(wt, repo, git):
        return Adopt("refused", session, why=f"{branch_git.lost(session, repo)}; nothing was read.")
    if cwd is None:
        cwd = str(Path(repo).parent)
    elif not branch_git.belongs(cwd, repo, git):
        return Adopt("refused", session, why=f"{cwd} is not a work tree of {repo}.")
    top_r = _run(git, ["git", *branch_git.GUARD, "-C", cwd, "rev-parse", "--show-toplevel"])
    if top_r.returncode != 0:
        return Adopt("refused", session, why=f"{cwd} is not a git work tree.")
    top = top_r.stdout.decode("utf-8", "replace").strip()
    branch_git.intent(wt, git)
    patch = branch_git.diff(wt, base, repo, git, extra=("--no-renames",), mark_new=False) or b""
    names = (
        branch_git.diff(
            wt, base, repo, git, extra=("--name-only", "--no-renames", "-z"), mark_new=False
        )
        or b""
    )
    files = sorted(f for f in names.decode("utf-8", "replace").split("\0") if f)
    if not patch or not files:
        return Adopt("nothing", session, why=f"{session} changed no file.")
    st = _run(
        git, ["git", *branch_git.GUARD, "-C", top, "status", "--porcelain", "-z", "--", *files]
    )
    dirty = sorted({e[3:] for e in st.stdout.decode("utf-8", "replace").split("\0") if len(e) > 3})
    if dirty:
        return Adopt(
            "refused",
            session,
            files=files,
            why=f"Refused: {dirty[0]} has uncommitted changes in {top}; commit or stash first.",
        )
    tracked = set(
        _run(
            git,
            [
                "git",
                *branch_git.GUARD,
                "-C",
                top,
                "ls-tree",
                "-r",
                "--name-only",
                "-z",
                "HEAD",
                "--",
                *files,
            ],
        )
        .stdout.decode("utf-8", "replace")
        .split("\0")
    )
    r = git(
        ["git", *branch_git.GUARD, "-C", top, "apply", "--3way", "--whitespace=nowarn", "-"],
        input=patch,
        capture_output=True,
    )
    if r.returncode != 0:
        # Put every touched path back as it was: they were all clean.
        _run(git, ["git", *branch_git.GUARD, "-C", top, "reset", "-q", "--", *files])
        for f in files:
            if f in tracked:
                _run(git, ["git", *branch_git.GUARD, "-C", top, "checkout", "HEAD", "--", f])
            else:
                try:
                    (Path(top) / f).unlink()
                except OSError:
                    pass
        err = (r.stderr or b"").decode("utf-8", "replace").strip().splitlines()
        return Adopt(
            "refused",
            session,
            files=files,
            why=f"Refused: the edits of {session} do not apply cleanly to {top}"
            + (f" ({err[-1]})." if err else "."),
        )
    return Adopt("adopted", session, files=files, why=f"Applied {len(files)} files to {top}.")
