"""The one starter of the delegation tree: start a child node, wait, end it.

``run_node`` is what every starter runs: the agentic executor for its
depth-1 child, ``daisugi tree run`` for a child that sprig's spawn tool
asks for, and later the steward. It proves and reserves through
``tree.spawn``, starts sprig on the child's profile with ``--max-turns``
and ``--max-tokens`` from the reservation, waits, and ends the node with
its own use; ``tree.end`` adds the recorded totals of its ended children.

The child gets its key on descriptor 3 and, when its wall holds spawn,
the bundle of resolved profiles on descriptor 4. It starts with the
parent-death signal in its own process group; when it fails or runs past
its time the group is killed, and every node under it still running is
ended from the leaves up with no report, so each keeps its whole
reservation. No node is left running.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from opendaisugi import agent_profiles, tree
from opendaisugi.models import Envelope

#: The most branches one request may ask for, and the reasons a branch may
#: be started for (W3K-8).
MAX_ATTEMPTS = 8
WHYS = ("failed_verify", "failed_tests", "low_confidence", "operator")


@dataclass
class NodeRun:
    """What happened to one child."""

    #: started (it ran and answered), refused, asked or failed.
    status: str
    session: str | None = None
    answer: str | None = None
    tokens_used: int | None = None
    turns_used: int | None = None
    reasons: list[str] = field(default_factory=list)
    ask_id: str | None = None
    #: The child's --json reply, when it answered.
    obj: dict[str, Any] = field(default_factory=dict)

    def doc(self) -> dict[str, Any]:
        return {
            "status": "ran" if self.status == "started" else self.status,
            "session": self.session,
            "answer": self.answer,
            "tokens_used": self.tokens_used,
            "turns_used": self.turns_used,
            "reasons": self.reasons,
            "ask_id": self.ask_id,
        }


def _own_use(obj: dict[str, Any]) -> tuple[int | None, int | None]:
    """The node's own use from sprig's --json: the four usage counts
    added (SX-R-5), capped at 2**53, and its model turns. A negative count
    makes the token report invalid, so the node is charged as one with no
    report (MX-R-7)."""
    turns = obj.get("model_turns")
    turns = turns if isinstance(turns, int) and not isinstance(turns, bool) else None
    tokens = tree.reported_tokens(obj.get("usage"))
    if turns is not None:
        turns = min(max(turns, 0), tree.MAX_COUNT)
    return tokens, turns


def branch_fence(words: list[str], worktree: str, session: str, repo: str | None) -> list[str]:
    """The fence words for a branch child. Its workspace is its own worktree,
    not the project. The repository's git directory is read only, except
    this branch's own admin directory, so the child can use git but cannot
    write the shared config, the hooks, the shared object store (which hold
    the operator's history) or a sibling's git directory. The child writes
    objects to a store of its own in its admin directory (``branch_env``)."""
    out: list[str] = []
    i = 0
    swapped = False
    while i < len(words):
        if words[i] == "--workspace" and i + 1 < len(words):
            out += ["--workspace", worktree]
            swapped = True
            i += 2
        else:
            out.append(words[i])
            i += 1
    if not swapped:
        out += ["--workspace", worktree]
    if repo:
        out += ["--allow", f"{repo}:ro", "--allow", f"{repo}/worktrees/{session}:rw"]
    return out


def branch_env(environ: dict[str, str], session: str, repo: str | None) -> dict[str, str]:
    """The environment of a fenced branch child: git writes its objects to a
    store in the branch's own admin directory and reads the shared store as
    an alternate. The parent makes the directory; the child cannot make it
    outside its grant."""
    if not repo or ":" in repo:
        return environ
    mine = os.path.join(repo, "worktrees", session, "objects")
    try:
        os.makedirs(mine, mode=0o700, exist_ok=True)
    except OSError:
        return environ
    out = dict(environ)
    out["GIT_OBJECT_DIRECTORY"] = mine
    out["GIT_ALTERNATE_OBJECT_DIRECTORIES"] = os.path.join(repo, "objects")
    return out


def fence_covers(
    words: list[str], envelope_file: Path | None, repo: str, home: str, base: str
) -> str | None:
    """The first read-and-write grant of a fence prefix that reaches the
    repository's git directory or its root, or None. Such a grant would let
    the branch child write the shared config and hooks; ``branch_fence``
    narrows only what it adds itself. A grant counts when it holds, or lies
    inside, the git directory or the repository root.

    Each grant is read as the fence reads it: ``~`` is ``home``, a relative
    path is joined to ``base`` (the fence's workspace, the child's
    worktree), and symbolic links are resolved, because Landlock grants the
    place a link points to."""
    from opendaisugi import fence

    real_repo = os.path.realpath(repo)

    def hits(path: str) -> bool:
        # The git directory sits inside the root, so a grant on the root, or
        # above it, holds the git directory too.
        p = os.path.realpath(path)
        return fence.covers(p, real_repo) or fence.covers(real_repo, p)

    i = 0
    given: list[str] = []
    while i < len(words):
        w = words[i]
        name, eq, val = w.partition("=")
        if name in ("--allow", "--workdir") and not eq and i + 1 < len(words):
            val, i = words[i + 1], i + 1
        elif name not in ("--allow", "--workdir"):
            i += 1
            continue
        if name == "--workdir" or val.endswith(":rw"):
            given.append(val[:-3] if val.endswith(":rw") else val)
        i += 1
    for g in given:
        if hits(fence.expand(g, home, base)):
            return g
    if envelope_file is not None:
        try:
            doc = json.loads(envelope_file.read_text())
            pats = doc.get("permissions", doc).get("file_write", [])
        except (OSError, ValueError, AttributeError):
            pats = []
        for pat in pats if isinstance(pats, list) else []:
            if isinstance(pat, str) and pat and hits(fence.glob_prefix(pat, home, base)):
                return pat
    return None


def run_node(
    data_dir: Path,
    warden_root: Path,
    *,
    parent: str,
    session: str | None,
    envelope: Envelope,
    profiles: agent_profiles.Profiles,
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
    default_model: str = "haiku",
    capture: bool = True,
    timeout_ms: int = tree.DEFAULT_TIMEOUT_MS,
    fence: list[str] | None = None,
    reserved: bool = False,
) -> NodeRun:
    """Start one child under ``parent`` and wait for it (see the module
    text). Raises tree.TreeError when the tree refuses the command itself
    (a parent that is not running, a session in use).

    ``fence``, when set, is the ``daisugi fence ...`` prefix (no ``--``)
    sprig starts under (DAISUGI_SPRIG_FENCE, FN-R-25). The tree run itself
    stays outside the fence and keeps writing its warden root; only sprig and
    its tools are fenced, and the warden root is no rw grant of theirs.

    ``reserved`` means a branch starter already proved the edge and reserved
    this node's budget (``tree.spawn_branches``): the node is not spawned
    here, and it is not ended here either. The caller ends it, in branch
    order, with the use this returns (the node's own, W3K-13)."""
    from opendaisugi import agentic_executor as ax

    def early(reasons: list[str]) -> NodeRun:
        # A reserved node that cannot start keeps its whole reservation: the
        # caller ends it with no report.
        return NodeRun("failed", session, reasons=reasons)

    profile, why = profiles.lookup(profile_name)
    if profile is None:
        return early([f"agentic sub-agent failed: {why}"])
    model = model or profile.model or default_model
    wall = ax.sprig_wall(tools, envelope)
    if not wall:
        return early(
            [
                f"no requested tool is backed by the envelope (requested {tools!r}); "
                "nothing to delegate"
            ]
        )
    if ax.has_go_space(ax.sprig_warden_words(spawn="spawn" in wall)):
        return early([ax.SPRIG_SPACE_TEXT])
    if "spawn" in wall and fence is not None:
        return early([ax.SPAWN_FENCE_TEXT])
    if not reserved:
        sp = tree.spawn(
            data_dir,
            warden_root,
            envelope,
            parent=parent,
            session=session,
            tokens=tokens,
            turns=turns,
            timeout_ms=timeout_ms,
            profile=profile_name if profile_name not in (None, agent_profiles.DEFAULT) else None,
            model=model,
            parent_tokens_used=parent_tokens_used,
            parent_turns_used=parent_turns_used,
        )
        if sp.status != "started":
            return NodeRun(sp.status, sp.session, reasons=sp.reasons, ask_id=sp.ask_id)
        session = sp.session
    assert session is not None
    run_turns = turns
    if max_turns is not None:
        run_turns = max_turns if run_turns is None else min(run_turns, max_turns)
    sess_dir = warden_root / "sessions"
    if fence is not None:
        sess_dir = data_dir / ax.FENCE_SESSIONS_DIR
        sess_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    argv, fds, env = ax.sprig_start(
        session_dir=sess_dir,
        sprig_binary=sprig_binary,
        wall=wall,
        model=model,
        warden_root=warden_root,
        data_dir=data_dir,
        session=session,
        profile=profile,
        profiled=profile_name not in (None, agent_profiles.DEFAULT),
        profiles=profiles,
        environ=environ,
        capture=capture,
        max_turns=run_turns,
        max_tokens=tokens,
    )
    if fence is not None:
        # sprig under the fence: its session dir read and write, its key
        # pipes kept, the warden root left read only (FN-R-25). A sprig that
        # is not found starts nothing, as Go's lookup does.
        found = _look_path(sprig_binary, (env if env is not None else environ).get("PATH", ""))
        if found is None:
            out = ax.SprigOutcome(ax._sprig_start_text(sprig_binary))
            return _ended(data_dir, warden_root, session, out, end_node=not reserved)
        wrapped = [*fence[1:], "--allow", f"{sess_dir}:rw"]
        if reserved:
            node = tree.read_tree(data_dir).nodes.get(session)
            repo = node.repo if node else None
            env = branch_env(env if env is not None else environ, session, repo)
            wrapped = branch_fence(wrapped, cwd, session, repo)
        for fd in sorted(fds):
            wrapped += ["--keep-fd", str(fd)]
        argv = [fence[0], *wrapped, "--", found, *argv[1:]]
    out = ax.run_sprig_child(
        argv,
        name=sprig_binary,
        task=task,
        cwd=cwd,
        timeout_s=timeout_s,
        env=env,
        fds=fds,
        on_start=lambda pid: tree.record_group(data_dir, session, pid),
        branch=lambda: tree.read_tree(data_dir).branch_groups(session),
    )
    return _ended(data_dir, warden_root, session, out, end_node=not reserved)


def _look_path(name: str, path: str) -> str | None:
    """The sprig binary as the child starts it: a name with a slash as it
    is, else the first match on PATH."""
    if "/" in name:
        return name
    import shutil

    return shutil.which(name, path=path)


def _ended(
    data_dir: Path, warden_root: Path, session: str, out: Any, *, end_node: bool = True
) -> NodeRun:
    """End the node after its sprig ran (or failed to start). With
    ``end_node`` False (a branch) its descendants are ended but the node is
    left to the caller, and the use returned is the node's own."""
    own_tokens = own_turns = None
    if out.error is None:
        own_tokens, own_turns = _own_use(out.obj)
    # Nothing under it may outlive it: end its running descendants with no
    # report, leaves first, then the node itself with its own use.
    tree.end_subtree(data_dir, warden_root, session, include_self=False)
    if end_node:
        node, _ = tree.end(
            data_dir, warden_root, session, tokens_used=own_tokens, turns_used=own_turns
        )
        used = (node.tokens_used, node.turns_used)
    else:
        used = (own_tokens, own_turns)
    if out.error is not None:
        return NodeRun(
            "failed",
            session,
            tokens_used=used[0],
            turns_used=used[1],
            reasons=[out.error],
        )
    return NodeRun(
        "started",
        session,
        answer=out.answer,
        tokens_used=used[0],
        turns_used=used[1],
        obj=out.obj,
    )


def read_request(data: bytes) -> dict[str, Any]:
    """`tree run`'s stdin: {task, envelope, tools, max_turns, attempts, why}. Raises
    ValueError with a line that says what is wrong."""
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as e:
        raise ValueError("It could not be read as UTF-8 text.") from e
    try:
        doc = json.loads(text)
    except (ValueError, RecursionError) as e:
        raise ValueError(f"It did not parse: {str(e).splitlines()[0]}") from e
    if not isinstance(doc, dict):
        raise ValueError("It is not a JSON object.")
    if not isinstance(doc.get("task"), str) or not doc["task"].strip():
        raise ValueError("task must be text.")
    if not isinstance(doc.get("envelope"), dict):
        raise ValueError("envelope must be an object.")
    tools = doc.get("tools")
    if not isinstance(tools, list) or not all(isinstance(t, str) for t in tools):
        raise ValueError("tools must be a list of text.")
    mt = doc.get("max_turns")
    if mt is not None and (not isinstance(mt, int) or isinstance(mt, bool) or mt < 1):
        raise ValueError("max_turns must be a whole number of 1 or more.")
    at = doc.get("attempts")
    if at is not None and (not isinstance(at, int) or isinstance(at, bool) or at != 1):
        if not isinstance(at, int) or isinstance(at, bool) or not 2 <= at <= MAX_ATTEMPTS:
            raise ValueError("attempts must be a whole number from 2 to 8.")
        if doc.get("why") not in WHYS:
            raise ValueError(
                "why must be one of failed_verify, failed_tests, low_confidence, operator."
            )
    return doc
