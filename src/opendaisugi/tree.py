"""The delegation tree: the edge rule, the tree ledger, and the asks.

A parent agent may start a child agent. The child's envelope must fit
inside the parent's before the child runs: ``edge_ok`` proves that. Each
edge is checked against its own parent, so by induction every node is
inside the root, and the root is the envelope the operator registered.

The proof is strict at every edge and fails closed: a missing envelope, a
part that does not fit, and a proof that does not finish are all refusals.
Every failing part is reported, in a fixed order, with a reason that names
no solver model: the reasons must read the same in every client.

The tree ledger keeps the budgets that are estimates (tokens and turns);
the envelope keeps the deadline, which is exact. The starter reserves a
child's budget from its own when it starts the child and gets the unspent
part back when the child ends. The ledger is an append-only JSONL file
under the data directory. After three refused proposals from one parent,
the next refused one goes to the operator as an ask.
"""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import re
import time
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from opendaisugi._invariant_types import RECOGNIZED_OPAQUE_TYPES
from opendaisugi.models import SHELL_INTERPRETERS, Envelope

DEFAULT_TIMEOUT_MS = 2000
#: Refused proposals from one parent before the next refused one is an ask.
ASK_AFTER = 3
MAX_COUNT = 2**53

_STAKES_RANK = {"low": 0, "medium": 1, "high": 2, "physical": 3}
# Tighter first: strict refuses an interpreter, surface flags it, allow
# says nothing.
_POLICY_RANK = {"strict": 0, "surface": 1, "allow": 2}
# subsumption._encode_shell_admission's metacharacters and substrings.
_METACHARS = (";", "|", "&", "`", "<", ">", "\n", "\r", "$(")
_SESSION = re.compile(r"[A-Za-z0-9_-](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?")


def _q(v: Any) -> str:
    return json.dumps(v)


# ---------------------------------------------------------------------------
# The edge rule
# ---------------------------------------------------------------------------


@dataclass
class EdgeResult:
    holds: bool
    reasons: list[str] = field(default_factory=list)
    # The child as it registers: the parent's deadline filled in when the
    # child names none.
    child: Envelope | None = None
    deadline_inherited: bool = False
    # Python only, for a parent model that reads it: the step the child
    # admits and the parent does not, when Z3 found one. No client output
    # holds it, since another solver may pick another step.
    counterexample: Any = None

    def doc(self) -> dict[str, Any]:
        return {
            "holds": self.holds,
            "reasons": self.reasons,
            "deadline": self.child.deadline if self.child is not None else None,
            "deadline_inherited": self.deadline_inherited,
        }


def _canon(item: Any) -> str:
    return json.dumps(item.model_dump(mode="json"), sort_keys=True)


def _robot_reason(parent: Envelope, child: Envelope) -> str | None:
    o, i = parent.permissions, child.permissions
    if o.workspace_bounds is not None:
        if i.workspace_bounds is None or any(
            i.workspace_bounds[0][k] < o.workspace_bounds[0][k]
            or i.workspace_bounds[1][k] > o.workspace_bounds[1][k]
            for k in range(3)
        ):
            return "robot: the child's workspace_bounds are not inside the parent's"
    for axis in ("velocity_limit", "torque_limit"):
        o_lim = getattr(o, axis)
        if o_lim is not None:
            i_lim = getattr(i, axis)
            if i_lim is None or i_lim > o_lim:
                return f"robot: the child's {axis} is not within the parent's"
    for joint, (o_lo, o_hi) in o.joint_limits.items():
        if joint not in i.joint_limits:
            return f"robot: the child's range for joint {_q(joint)} is not within the parent's"
        i_lo, i_hi = i.joint_limits[joint]
        if i_lo < o_lo or i_hi > o_hi:
            return f"robot: the child's range for joint {_q(joint)} is not within the parent's"

    def freeze(boxes: Any) -> set[Any]:
        return {(tuple(lo), tuple(hi)) for lo, hi in boxes}

    missing = freeze(o.obstacles) - freeze(i.obstacles)
    if missing:
        return f"robot: the child drops {len(missing)} of the parent's obstacles"
    return None


def _pattern_fits(pattern: str, outer: list[str], timeout_ms: int) -> bool | None:
    """True when every value ``pattern`` admits, ``outer`` admits too; False
    when Z3 finds one it does not; None when the proof did not finish."""
    import z3

    from opendaisugi import z3_checks
    from opendaisugi.subsumption import _glob_to_z3

    if not outer:
        return False
    with z3_checks.Z3_SOLVE_LOCK:
        solver = z3.Solver()
        solver.set("timeout", timeout_ms)
        v = z3.String("v")
        solver.add(_glob_to_z3(v, pattern), z3.Not(z3.Or(*[_glob_to_z3(v, g) for g in outer])))
        r = solver.check()
    if r == z3.unsat:
        return True
    if r == z3.sat:
        return False
    return None


def _scope_reasons(parent: Envelope, child: Envelope, timeout_ms: int) -> list[str]:
    from opendaisugi.subsumption import _glob_unsupported

    out: list[str] = []
    o, i = parent.permissions, child.permissions
    if i.shell_allow_decomposition and not o.shell_allow_decomposition:
        out.append(
            "shell_allow_decomposition: the child allows compound commands; the parent does not"
        )
    for label in ("file_read", "file_write", "mcp_allowlist"):
        inner, outer = getattr(i, label), getattr(o, label)
        if not inner:
            continue
        bad = next((g for g in outer if _glob_unsupported(g)), None)
        if bad is not None:
            out.append(f"{label}: the parent's pattern {_q(bad)} has a shape the proof cannot read")
            continue
        for p in inner:
            fits = _pattern_fits(p, outer, timeout_ms)
            if fits is None:
                out.append(f"{label}: the proof for the child's {_q(p)} did not finish")
                break
            if not fits:
                out.append(f"{label}: the child's {_q(p)} is not inside the parent's")
                break
    if i.network:
        if not o.network:
            out.append("network: the child uses the network; the parent does not")
        elif o.network_hosts:
            if not i.network_hosts:
                out.append(
                    "network_hosts: the child allows any host; the parent allows only "
                    + _q(o.network_hosts)
                )
            else:
                known = {h.lower() for h in o.network_hosts}
                extra = [h for h in i.network_hosts if h.lower() not in known]
                if extra:
                    out.append(f"network_hosts: the child adds {_q(extra)}")
    if i.shell:
        if not o.shell:
            out.append("shell: the child runs shell commands; the parent does not")
        else:
            # The Z3 encoding admits a command when it is a head or starts
            # with a head and a space, and holds no metacharacter. So a child
            # head fits when it is a parent head or starts with one and a
            # space; a head that holds a metacharacter admits nothing.
            heads = o.shell_allowlist
            extra = [
                h
                for h in i.shell_allowlist
                if not any(m in h for m in _METACHARS)
                and not any(h == p or h.startswith(p + " ") for p in heads)
            ]
            if extra:
                out.append(f"shell_allowlist: the child adds {_q(extra)}")
        if parent.shell_interpreter_policy == "strict":
            names = sorted({n for n in i.shell_allowlist if n in SHELL_INTERPRETERS})
            if names:
                out.append(
                    "shell_interpreter_policy: the parent is strict and the child allows "
                    f"the interpreters {_q(names)}"
                )
    opaque = [
        inv.type
        for inv in child.invariants
        if inv.enforce and inv.expr is None and inv.type not in RECOGNIZED_OPAQUE_TYPES
    ]
    if opaque:
        out.append(f"invariants: the child's {_q(opaque)} have no expr, so they cannot be proved")
    return out


def edge_ok(
    parent: Envelope | None, child: Envelope | None, *, timeout_ms: int = DEFAULT_TIMEOUT_MS
) -> EdgeResult:
    """Prove that ``child`` fits inside ``parent``: every part below holds.

    The parts, each reported when it fails, in this order: both envelopes
    are there; stakes; custom step types; the per-step budgets; the
    deadline; the interpreter policy; the parent's enforced invariants and
    postconditions; the robot bounds; the scope of each permission; and,
    when all of those hold, the Z3 proof (``envelope_subsumes``, strict).
    A child with no deadline under a parent with one takes the parent's.
    """
    if parent is None:
        return EdgeResult(False, ["envelope: the parent has no envelope"])
    if child is None:
        return EdgeResult(False, ["envelope: the child declares no envelope"])
    reasons: list[str] = []
    o_rank, i_rank = _STAKES_RANK[parent.stakes], _STAKES_RANK[child.stakes]
    if i_rank < o_rank:
        reasons.append(
            f"stakes: the child's {_q(child.stakes)} is lower than the parent's {_q(parent.stakes)}"
        )
    known = set(parent.permissions.custom_step_allowlist)
    extra = sorted(set(child.permissions.custom_step_allowlist) - known)
    if extra:
        reasons.append(f"custom_step_allowlist: the child adds {_q(extra)}")
    for name in ("max_execution_time_s", "max_output_size_mb"):
        o_val, i_val = getattr(parent.permissions, name), getattr(child.permissions, name)
        if i_val > o_val:
            reasons.append(f"{name}: the child's {i_val} is more than the parent's {o_val}")
    inherited = False
    effective = child
    if parent.deadline is not None:
        if child.deadline is None:
            effective = child.model_copy(update={"deadline": parent.deadline})
            inherited = True
        elif child.deadline > parent.deadline:
            reasons.append(
                f"deadline: the child's {_q(child.deadline)} is after the parent's "
                f"{_q(parent.deadline)}"
            )
    if _POLICY_RANK[child.shell_interpreter_policy] > _POLICY_RANK[parent.shell_interpreter_policy]:
        reasons.append(
            f"shell_interpreter_policy: the child's {_q(child.shell_interpreter_policy)} is "
            f"looser than the parent's {_q(parent.shell_interpreter_policy)}"
        )
    for label in ("invariants", "postconditions"):
        have = {_canon(x) for x in getattr(child, label)}
        missing = [x.type for x in getattr(parent, label) if x.enforce and _canon(x) not in have]
        if missing:
            reasons.append(f"{label}: the child drops the parent's {_q(missing)}")
    robot = _robot_reason(parent, child)
    if robot is not None:
        reasons.append(robot)
    reasons += _scope_reasons(parent, child, timeout_ms)
    counter = None
    # With no enforced invariant that has an expr on either side, the Z3
    # query over one shell step reduces to the head rule above, which
    # decides it exactly. Z3 runs only when an invariant can change it: the
    # string solver can take seconds on a head of two words.
    exprs = [x for x in [*parent.invariants, *child.invariants] if x.enforce and x.expr is not None]
    if not reasons and exprs:
        from opendaisugi.subsumption import envelope_subsumes

        res = envelope_subsumes(parent, effective, timeout_ms=timeout_ms, strict=True)
        if res.timed_out:
            reasons.append(f"proof: the proof did not finish in {timeout_ms} ms")
        elif not res.holds:
            reasons.append("proof: the child admits a step the parent does not")
            counter = res.counterexample
    return EdgeResult(
        holds=not reasons,
        reasons=reasons,
        child=effective,
        deadline_inherited=inherited,
        counterexample=counter,
    )


def edge_text(res: EdgeResult) -> str:
    if res.holds:
        lines = ["edge ok: the child fits inside the parent."]
        if res.deadline_inherited and res.child is not None:
            lines.append(f"The child takes the parent's deadline {_q(res.child.deadline)}.")
    else:
        lines = ["edge refused:"] + [f"  {r}" for r in res.reasons]
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# The tree ledger
# ---------------------------------------------------------------------------


class TreeError(Exception):
    """A command that cannot go on: what it tried, why not, and the fix."""

    def __init__(self, what: str, why: str, fix: str, code: int = 1) -> None:
        super().__init__(why)
        self.what, self.why, self.fix, self.code = what, why, fix, code


def ledger_path(data_dir: Path) -> Path:
    return Path(data_dir) / "tree" / "ledger.jsonl"


def valid_session(s: str) -> bool:
    return bool(_SESSION.fullmatch(s)) and s not in ("default", "no-session")


def _count(v: Any) -> bool:
    return v is None or (isinstance(v, int) and not isinstance(v, bool) and 0 <= v <= MAX_COUNT)


def _finite(v: Any) -> bool:
    import math

    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return False
    try:
        return math.isfinite(float(v))
    except OverflowError:
        return False


def _strs(v: Any) -> bool:
    return isinstance(v, list) and all(isinstance(x, str) for x in v)


def _fits(row: dict[str, Any]) -> bool:
    """A row the fold can read: its event's keys, of their types."""
    ev = row.get("event")
    if not _finite(row.get("ts")):
        return False
    s = lambda k: isinstance(row.get(k), str)  # noqa: E731
    if ev == "root":
        return (
            s("session")
            and s("envelope_id")
            and _count(row.get("tokens"))
            and _count(row.get("turns"))
            and (row.get("deadline") is None or _finite(row.get("deadline")))
        )
    if ev == "spawn":
        return (
            s("parent")
            and s("session")
            and s("envelope_id")
            and _count(row.get("tokens"))
            and _count(row.get("turns"))
            and (row.get("deadline") is None or _finite(row.get("deadline")))
            and isinstance(row.get("proved"), bool)
        )
    if ev == "refused":
        return s("parent") and s("session") and _strs(row.get("reasons"))
    if ev == "ask":
        return (
            s("ask_id")
            and s("parent")
            and s("session")
            and _count(row.get("tokens"))
            and _count(row.get("turns"))
            and isinstance(row.get("envelope"), dict)
            and _strs(row.get("reasons"))
        )
    if ev == "answer":
        return s("ask_id") and row.get("answer") in ("allow", "deny")
    if ev == "end":
        return s("session") and _count(row.get("tokens_used")) and _count(row.get("turns_used"))
    return False


@dataclass
class Node:
    session: str
    parent: str | None
    envelope_id: str
    tokens: int | None
    turns: int | None
    deadline: float | None
    proved: bool
    state: str = "running"
    tokens_used: int | None = None
    turns_used: int | None = None
    children: list[str] = field(default_factory=list)
    refused: int = 0


@dataclass
class Tree:
    nodes: dict[str, Node] = field(default_factory=dict)
    roots: list[str] = field(default_factory=list)
    asks: dict[str, dict[str, Any]] = field(default_factory=dict)
    answers: dict[str, str] = field(default_factory=dict)
    skipped: int = 0

    def running(self, session: str) -> Node | None:
        n = self.nodes.get(session)
        return n if n is not None and n.state == "running" else None

    def left(self, node: Node, axis: str) -> int | None:
        """What ``node`` may still reserve for children on ``axis``."""
        budget = getattr(node, axis)
        if budget is None:
            return None
        spent = 0
        for c in node.children:
            child = self.nodes[c]
            reserved = getattr(child, axis) or 0
            used = getattr(child, axis + "_used")
            if child.state == "ended" and used is not None:
                reserved = min(used, reserved)
            spent += reserved
        return budget - spent

    def open_asks(self) -> list[dict[str, Any]]:
        return [a for k, a in self.asks.items() if k not in self.answers]


def read_tree(data_dir: Path) -> Tree:
    """Fold the ledger, rows in file order. A row that does not read, or
    does not fit the tree as it stands, is skipped and counted."""
    t = Tree()
    try:
        text = ledger_path(data_dir).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return t
    # read_text turns \r\n and \r into \n, as rank._read_rows reads.
    for line in text.split("\n"):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except (ValueError, RecursionError):
            t.skipped += 1
            continue
        if not isinstance(row, dict) or not _fits(row):
            t.skipped += 1
            continue
        ev = row["event"]
        if ev in ("root", "spawn"):
            sid = row["session"]
            parent = row.get("parent") if ev == "spawn" else None
            if sid in t.nodes or (ev == "spawn" and t.running(parent) is None):
                t.skipped += 1
                continue
            t.nodes[sid] = Node(
                session=sid,
                parent=parent,
                envelope_id=row["envelope_id"],
                tokens=row.get("tokens"),
                turns=row.get("turns"),
                deadline=float(row["deadline"]) if row.get("deadline") is not None else None,
                proved=bool(row.get("proved", True)),
            )
            if parent is None:
                t.roots.append(sid)
            else:
                t.nodes[parent].children.append(sid)
                t.nodes[parent].refused = 0
        elif ev == "refused":
            p = t.nodes.get(row["parent"])
            if p is None:
                t.skipped += 1
                continue
            p.refused += 1
        elif ev == "ask":
            if row["ask_id"] in t.asks or row["parent"] not in t.nodes:
                t.skipped += 1
                continue
            t.asks[row["ask_id"]] = row
        elif ev == "answer":
            a = t.asks.get(row["ask_id"])
            if a is None or row["ask_id"] in t.answers:
                t.skipped += 1
                continue
            t.answers[row["ask_id"]] = row["answer"]
            t.nodes[a["parent"]].refused = 0
        elif ev == "end":
            n = t.running(row["session"])
            if n is None:
                t.skipped += 1
                continue
            n.state = "ended"
            n.tokens_used = row.get("tokens_used")
            n.turns_used = row.get("turns_used")
    return t


@contextlib.contextmanager
def _locked(data_dir: Path) -> Iterator[None]:
    """One writer at a time: the fold, the check and the append happen
    under one lock, so two starters cannot both reserve the same budget."""
    d = ledger_path(data_dir).parent
    d.mkdir(parents=True, exist_ok=True)
    with open(d / "ledger.lock", "a", encoding="utf-8") as fh:
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(fh.fileno(), fcntl.LOCK_UN)


def _append(data_dir: Path, rows: list[dict[str, Any]]) -> None:
    with ledger_path(data_dir).open("a", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


def now() -> float:
    return time.time()


def _check_session(s: str, what: str) -> None:
    if not valid_session(s):
        raise TreeError(
            what,
            f"The session id {_q(s)} is not 1 to 64 of A-Z a-z 0-9 . _ - "
            "(no dot at either end), or it is default.",
            "Pick another session id.",
            code=2,
        )


def _check_counts(**counts: int | None) -> None:
    for name, v in counts.items():
        if v is not None and not 0 <= v <= MAX_COUNT:
            raise TreeError(
                "Tried to read the budget.",
                f"--{name} must be a whole number from 0 to 2**53.",
                "Fix the option and run again.",
                code=2,
            )


def load_registered(session: str, gate_root: Path) -> Envelope | None:
    """The envelope registered for ``session``, or None when there is none
    or it does not read as one: a parent the tree cannot read has no
    envelope, and its child does not start."""
    from pydantic import ValidationError

    from opendaisugi.gate import load_envelope

    try:
        return load_envelope(session, root=gate_root)
    except (ValidationError, ValueError, OSError):
        return None


def _envelope_file(gate_root: Path, session: str) -> Path:
    from opendaisugi.gate import _envelopes_dir

    return _envelopes_dir(gate_root) / f"{session}.json"


def make_root(
    data_dir: Path,
    gate_root: Path,
    envelope: Envelope,
    session: str,
    *,
    tokens: int | None,
    turns: int | None,
) -> Path:
    """Register the operator's root envelope for ``session`` and open its
    budget. Only the operator runs this."""
    from opendaisugi.gate import register_envelope

    what = f"Tried to make session {session} a root of the tree."
    _check_session(session, what)
    _check_counts(tokens=tokens, turns=turns)
    with _locked(data_dir):
        t = read_tree(data_dir)
        if session in t.nodes or _envelope_file(gate_root, session).exists():
            raise TreeError(
                what, f"Session {session} is in use already.", "Pick another session id."
            )
        _append(
            data_dir,
            [
                {
                    "event": "root",
                    "session": session,
                    "envelope_id": envelope.id,
                    "tokens": tokens,
                    "turns": turns,
                    "deadline": envelope.deadline,
                    "ts": now(),
                }
            ],
        )
        return register_envelope(envelope, session_id=session, root=gate_root)


@dataclass
class Spawn:
    status: str  # started, refused or asked
    reasons: list[str] = field(default_factory=list)
    path: Path | None = None
    ask_id: str | None = None
    inherited: float | None = None


def _ask_id(parent: str, session: str, envelope: dict[str, Any], n: int) -> str:
    text = json.dumps([parent, session, envelope, n], sort_keys=True, separators=(",", ":"))
    return "ask_" + hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


def _budget_reasons(t: Tree, parent: Node, tokens: int | None, turns: int | None) -> list[str]:
    out = []
    for axis, want in (("tokens", tokens), ("turns", turns)):
        left = t.left(parent, axis)
        if left is None:
            continue
        if want is None:
            out.append(f"{axis}: the parent has a budget, so the child must ask for an amount")
        elif want > left:
            out.append(f"{axis}: the child asks for {want}; the parent has {left} left")
    return out


def spawn(
    data_dir: Path,
    gate_root: Path,
    envelope: Envelope,
    *,
    parent: str,
    session: str,
    tokens: int | None,
    turns: int | None,
    timeout_ms: int = DEFAULT_TIMEOUT_MS,
) -> Spawn:
    """Prove the edge from ``parent`` to a child, reserve its budget, and
    register its envelope for ``session``. The starter runs this, and the
    starter picks the session, not the child."""
    from opendaisugi.gate import register_envelope

    what = f"Tried to start session {session} under {parent}."
    _check_session(session, what)
    _check_session(parent, what)
    _check_counts(tokens=tokens, turns=turns)
    with _locked(data_dir):
        t = read_tree(data_dir)
        pnode = t.running(parent)
        if pnode is None:
            raise TreeError(
                what,
                f"{parent} is not a running node of the tree.",
                "Start the child under a running node; daisugi tree status lists them.",
            )
        if session in t.nodes or _envelope_file(gate_root, session).exists():
            raise TreeError(
                what, f"Session {session} is in use already.", "Pick another session id."
            )
        if any(a["session"] == session for a in t.open_asks()):
            raise TreeError(
                what,
                f"An ask for session {session} is open.",
                "Wait for the operator's answer, or pick another session id.",
            )
        penv = load_registered(parent, gate_root)
        res: EdgeResult | None = None
        if penv is not None and penv.stakes == "physical":
            reasons = ["stakes: the parent's stakes are physical, so it cannot start an agent"]
        else:
            res = edge_ok(penv, envelope, timeout_ms=timeout_ms)
            reasons = list(res.reasons)
        reasons += _budget_reasons(t, pnode, tokens, turns)
        ts = now()
        if reasons:
            if pnode.refused >= ASK_AFTER:
                dump = envelope.model_dump(mode="json")
                aid = _ask_id(parent, session, dump, len(t.asks))
                _append(
                    data_dir,
                    [
                        {
                            "event": "ask",
                            "ask_id": aid,
                            "parent": parent,
                            "session": session,
                            "tokens": tokens,
                            "turns": turns,
                            "envelope": dump,
                            "reasons": reasons,
                            "ts": ts,
                        }
                    ],
                )
                return Spawn("asked", reasons, ask_id=aid)
            _append(
                data_dir,
                [
                    {
                        "event": "refused",
                        "parent": parent,
                        "session": session,
                        "reasons": reasons,
                        "ts": ts,
                    }
                ],
            )
            return Spawn("refused", reasons)
        assert res is not None and res.child is not None and penv is not None
        child = res.child.model_copy(update={"parent_envelope": penv.id})
        _append(
            data_dir,
            [
                {
                    "event": "spawn",
                    "parent": parent,
                    "session": session,
                    "envelope_id": child.id,
                    "tokens": tokens,
                    "turns": turns,
                    "deadline": child.deadline,
                    "proved": True,
                    "ts": ts,
                }
            ],
        )
        path = register_envelope(child, session_id=session, root=gate_root)
        return Spawn(
            "started", path=path, inherited=child.deadline if res.deadline_inherited else None
        )


def end(
    data_dir: Path,
    gate_root: Path,
    session: str,
    *,
    tokens_used: int | None,
    turns_used: int | None,
) -> tuple[Node, Tree]:
    """Record that ``session`` ended and what it used; its parent gets the
    unspent part back. Its envelope is unregistered, so the gate denies
    any later call of it."""
    what = f"Tried to end session {session}."
    _check_session(session, what)
    _check_counts(**{"tokens-used": tokens_used, "turns-used": turns_used})
    with _locked(data_dir):
        t = read_tree(data_dir)
        node = t.running(session)
        if node is None:
            raise TreeError(
                what,
                f"{session} is not a running node of the tree.",
                "daisugi tree status lists the running nodes.",
            )
        busy = [c for c in node.children if t.nodes[c].state == "running"]
        if busy:
            raise TreeError(what, f"It has running children: {', '.join(busy)}.", "End them first.")
        _append(
            data_dir,
            [
                {
                    "event": "end",
                    "session": session,
                    "tokens_used": tokens_used,
                    "turns_used": turns_used,
                    "ts": now(),
                }
            ],
        )
        with contextlib.suppress(FileNotFoundError):
            _envelope_file(gate_root, session).unlink()
        node.state = "ended"
        node.tokens_used, node.turns_used = tokens_used, turns_used
        return node, t


def answer(
    data_dir: Path, gate_root: Path, ask_id: str, verdict: str
) -> tuple[dict[str, Any], Path | None]:
    """The operator's answer to an ask. An allow starts the child as
    proposed, not proved against its parent and marked so; it must still
    fit inside the root of its branch, and the budget must still fit."""
    from opendaisugi.gate import register_envelope

    what = f"Tried to answer ask {ask_id}."
    if verdict not in ("allow", "deny"):
        raise TreeError(
            what, "The answer must be allow or deny.", "Run it again with allow or deny.", code=2
        )
    with _locked(data_dir):
        t = read_tree(data_dir)
        ask = t.asks.get(ask_id)
        if ask is None:
            raise TreeError(
                what, f"There is no ask {ask_id}.", "daisugi tree status lists the open asks."
            )
        if ask_id in t.answers:
            raise TreeError(
                what, f"It is answered already: {t.answers[ask_id]}.", "An answer stands."
            )
        row = {"event": "answer", "ask_id": ask_id, "answer": verdict, "ts": now()}
        if verdict == "deny":
            _append(data_dir, [row])
            return ask, None
        parent, session = ask["parent"], ask["session"]
        pnode = t.running(parent)
        if pnode is None:
            raise TreeError(what, f"{parent} is not a running node of the tree.", "Deny the ask.")
        if session in t.nodes or _envelope_file(gate_root, session).exists():
            raise TreeError(what, f"Session {session} is in use already.", "Deny the ask.")
        over = [r for r in _budget_reasons(t, pnode, ask["tokens"], ask["turns"])]
        if over:
            raise TreeError(what, f"The budget does not fit: {'; '.join(over)}.", "Deny the ask.")
        # AT-5 holds under an allow too: a physical parent starts no agent.
        ppar = load_registered(parent, gate_root)
        if ppar is not None and ppar.stakes == "physical":
            raise TreeError(
                what,
                "stakes: the parent's stakes are physical, so it cannot start an agent.",
                "Deny the ask.",
            )
        # An allow is the operator's, but it never widens the tree past its
        # root: the child must fit inside the root of its branch, proved as
        # an edge, so every node stays inside the root. "Not proved" means
        # not proved against its parent.
        root = parent
        while t.nodes[root].parent is not None:
            root = t.nodes[root].parent  # type: ignore[assignment]
        edge = edge_ok(load_registered(root, gate_root), Envelope.model_validate(ask["envelope"]))
        if not edge.holds or edge.child is None:
            raise TreeError(
                what,
                f"The child does not fit inside the root {root}: {'; '.join(edge.reasons)}.",
                "Deny the ask, or register the child as a root of its own.",
            )
        child = edge.child
        penv = load_registered(parent, gate_root)
        if penv is not None:
            child = child.model_copy(update={"parent_envelope": penv.id})
        _append(
            data_dir,
            [
                row,
                {
                    "event": "spawn",
                    "parent": parent,
                    "session": session,
                    "envelope_id": child.id,
                    "tokens": ask["tokens"],
                    "turns": ask["turns"],
                    "deadline": child.deadline,
                    "proved": False,
                    "ts": row["ts"],
                },
            ],
        )
        return ask, register_envelope(child, session_id=session, root=gate_root)


def _axis(node: Node, t: Tree, axis: str) -> dict[str, Any]:
    return {
        "budget": getattr(node, axis),
        "used": getattr(node, axis + "_used"),
        "left": t.left(node, axis),
    }


def status_doc(data_dir: Path) -> dict[str, Any]:
    t = read_tree(data_dir)
    nodes: list[dict[str, Any]] = []

    def walk(sid: str) -> None:
        n = t.nodes[sid]
        nodes.append(
            {
                "session": n.session,
                "parent": n.parent,
                "state": n.state,
                "proved": n.proved,
                "envelope_id": n.envelope_id,
                "deadline": n.deadline,
                "tokens": _axis(n, t, "tokens"),
                "turns": _axis(n, t, "turns"),
                "refused": n.refused,
                "children": list(n.children),
            }
        )
        for c in n.children:
            walk(c)

    for r in t.roots:
        walk(r)
    asks = [
        {
            "ask_id": a["ask_id"],
            "parent": a["parent"],
            "session": a["session"],
            "tokens": a["tokens"],
            "turns": a["turns"],
            "reasons": a["reasons"],
        }
        for a in t.open_asks()
    ]
    return {"nodes": nodes, "asks": asks, "skipped": t.skipped}


def _amount(a: dict[str, Any], state: str) -> str:
    if a["budget"] is None:
        return "-"
    if state == "ended":
        used = "?" if a["used"] is None else str(a["used"])
        return f"{a['budget']} (used {used})"
    return f"{a['budget']} (left {a['left']})"


def status_text(doc: dict[str, Any]) -> str:
    lines: list[str] = []
    depth: dict[str, int] = {}
    for n in doc["nodes"]:
        d = 0 if n["parent"] is None else depth[n["parent"]] + 1
        depth[n["session"]] = d
        state = n["state"] + ("" if n["proved"] else ", operator allow, not proved")
        line = (
            f"{'  ' * d}{n['session']} ({state}) tokens {_amount(n['tokens'], n['state'])}, "
            f"turns {_amount(n['turns'], n['state'])}"
        )
        if n["deadline"] is not None:
            line += f", deadline {_q(n['deadline'])}"
        if n["refused"]:
            line += f", {n['refused']} refused"
        lines.append(line)
    if not lines:
        lines.append("The tree is empty.")
    if doc["asks"]:
        lines.append("Open asks:")
        for a in doc["asks"]:
            lines.append(
                f"  {a['ask_id']}: {a['parent']} asks to start {a['session']}: {a['reasons'][0]}"
            )
        lines.append("Answer one with: daisugi tree answer ASK allow|deny")
    if doc["skipped"]:
        lines.append(f"{doc['skipped']} ledger rows did not read or fit; skipped.")
    return "\n".join(lines) + "\n"
