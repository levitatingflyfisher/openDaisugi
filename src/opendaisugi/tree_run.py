"""The one starter of the delegation tree: start a child node, wait, end it.

``run_node`` is what every starter runs: the agentic executor for its
depth-1 child, ``daisugi tree run`` for a child that sprig's spawn tool
asks for, and later the foreman. It proves and reserves through
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
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from opendaisugi import agent_profiles, tree
from opendaisugi.models import Envelope


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


def run_node(
    data_dir: Path,
    gate_root: Path,
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
) -> NodeRun:
    """Start one child under ``parent`` and wait for it (see the module
    text). Raises tree.TreeError when the tree refuses the command itself
    (a parent that is not running, a session in use).

    ``fence``, when set, is the ``daisugi fence ...`` prefix (no ``--``)
    sprig starts under (DAISUGI_SPRIG_FENCE, FN-R-25). The tree run itself
    stays outside the fence and keeps writing its gate root; only sprig and
    its tools are fenced, and the gate root is no rw grant of theirs."""
    from opendaisugi import agentic_executor as ax

    profile, why = profiles.lookup(profile_name)
    if profile is None:
        return NodeRun("failed", session, reasons=[f"agentic sub-agent failed: {why}"])
    model = model or profile.model or default_model
    wall = ax.sprig_wall(tools, envelope)
    if not wall:
        return NodeRun(
            "failed",
            session,
            reasons=[
                f"no requested tool is backed by the envelope (requested {tools!r}); "
                "nothing to delegate"
            ],
        )
    if ax.has_go_space(ax.sprig_gate_words(spawn="spawn" in wall)):
        return NodeRun("failed", session, reasons=[ax.SPRIG_SPACE_TEXT])
    if "spawn" in wall and fence is not None:
        return NodeRun("failed", session, reasons=[ax.SPAWN_FENCE_TEXT])
    sp = tree.spawn(
        data_dir,
        gate_root,
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
    sess_dir = gate_root / "sessions"
    if fence is not None:
        sess_dir = data_dir / ax.FENCE_SESSIONS_DIR
        sess_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    argv, fds, env = ax.sprig_start(
        session_dir=sess_dir,
        sprig_binary=sprig_binary,
        wall=wall,
        model=model,
        gate_root=gate_root,
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
        # pipes kept, the gate root left read only (FN-R-25). A sprig that
        # is not found starts nothing, as Go's lookup does.
        found = _look_path(sprig_binary, (env if env is not None else environ).get("PATH", ""))
        if found is None:
            out = ax.SprigOutcome(ax._sprig_start_text(sprig_binary))
            return _ended(data_dir, gate_root, session, out)
        wrapped = [*fence[1:], "--allow", f"{sess_dir}:rw"]
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
    return _ended(data_dir, gate_root, session, out)


def _look_path(name: str, path: str) -> str | None:
    """The sprig binary as the child starts it: a name with a slash as it
    is, else the first match on PATH."""
    if "/" in name:
        return name
    import shutil

    return shutil.which(name, path=path)


def _ended(data_dir: Path, gate_root: Path, session: str, out: Any) -> NodeRun:
    """End the node after its sprig ran (or failed to start)."""
    own_tokens = own_turns = None
    if out.error is None:
        own_tokens, own_turns = _own_use(out.obj)
    # Nothing under it may outlive it: end its running descendants with no
    # report, leaves first, then the node itself with its own use.
    tree.end_subtree(data_dir, gate_root, session, include_self=False)
    node, _ = tree.end(data_dir, gate_root, session, tokens_used=own_tokens, turns_used=own_turns)
    if out.error is not None:
        return NodeRun(
            "failed",
            session,
            tokens_used=node.tokens_used,
            turns_used=node.turns_used,
            reasons=[out.error],
        )
    return NodeRun(
        "started",
        session,
        answer=out.answer,
        tokens_used=node.tokens_used,
        turns_used=node.turns_used,
        obj=out.obj,
    )


def read_request(data: bytes) -> dict[str, Any]:
    """`tree run`'s stdin: {task, envelope, tools, max_turns}. Raises
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
    return doc
