"""Call-time tool gate — the enforce-mode counterpart of the passive hook.

Where :mod:`opendaisugi.hook` observes a host harness's tool calls and fails
open (correct for capture, wrong for protection), this module takes each
intercepted call, synthesizes it into a one-step plan, and proves it inside
the session's registered envelope *before it runs* (ADR-0007). The two share
a code path, not a failure policy:

- **enforce** mode is fail-closed: unknown tool, unparseable input, internal
  exception, or a slow verifier all DENY. The gate owns an inner timeout that
  itself denies, because every known host's *outer* hook timeout fails open.
  A Z3 check that timed out inside ``verify()`` also denies here, even when
  ``verify()`` itself only warned about it (its lenient-mode default). The
  gate must never let a call through on an unfinished check.
- **shadow** mode (the default) observes: every call is evaluated and the
  would-have-denied verdict recorded, but the host always gets its allow
  contract. Shadow mode is observation, not protection.

The full :func:`opendaisugi.verify.verify` pipeline runs per call — not the
``verify_step`` hot path — so plan-level strict-mode checks are never skipped
at the boundary, and strictness is resolved from the envelope's stakes
(``strict=None``), never relaxed at the gate.
"""

from __future__ import annotations

import json
import os
import threading
import time
import warnings
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    import argparse

    from opendaisugi.session_tree import SessionTree

from opendaisugi.effects import (
    PERMANENT,
    SILENT,
    UNDOABLE,
    Workspace,
    effect_class,
    tier_for,
    workspace_root,
)
from opendaisugi.floor_config import OPENCODE_PLUGIN_REFUSAL, floor_config_hit, opencode_plugin_hit
from opendaisugi.floor_config import REFUSAL as FLOOR_REFUSAL
from opendaisugi.hook import (
    APPLY_PATCH_TOOL,
    EXIT_CODE_FORMATS,
    STDOUT_BLOCK_FORMATS,
    _payload_to_record,
    _records_to_steps,
    _safe_session_id,
    join_keys,
    parse_apply_patch,
    stdout_for_format,
)
from opendaisugi.models import ActionPlan, Envelope, Permission
from opendaisugi.pane_rule import REFUSAL as PANE_REFUSAL
from opendaisugi.pane_rule import pane_rule_hit
from opendaisugi.search_rule import SEARCH_REFUSAL, search_above_secret_hit
from opendaisugi.verify import is_z3_timeout_warning, verify

DEFAULT_GATE_ROOT = Path.home() / ".opendaisugi" / "gate"

# re warns (FutureWarning) on a character class a future Python may read
# differently, such as a nested set "[[" in a predicate regex. The warning
# names this install's source path and line, and it would land on the
# hook's stderr, the host's deny channel, so the gate's output would depend
# on where it is installed. The regex is read exactly as before; only the
# warning is dropped. Installed at import, before any verifier thread.
warnings.filterwarnings(
    "ignore",
    message=r"Possible (nested set|set (difference|intersection|union|symmetric difference)) at position \d+",
    category=FutureWarning,
)
# The project directory the harness names in the hook's environment. The
# agent can move the payload's cwd with cd, but not this, so a write counts
# as inside the workspace only when it is inside both.
_PROJECT_DIR_ENV = "CLAUDE_PROJECT_DIR"
_DISARM_FILENAME = "DISARMED"
_DEFAULT_VERIFY_TIMEOUT_S = 10.0

# Reading a payload or a shell command costs time that grows with its size
# (CPython's shlex is quadratic), so past some size the verdict would depend
# on the verify budget and the speed of the box. The gate denies such input
# before reading it: fixed caps, the same in every client, far above any
# real hook payload or command.
MAX_PAYLOAD_BYTES = 16 * 1024 * 1024
MAX_SHELL_COMMAND_CHARS = 256 * 1024


def resolve_gate_mode(explicit: str | None, *, root: Path = DEFAULT_GATE_ROOT) -> str:
    """Resolve the gate verdict mode. An explicit ``--mode`` ALWAYS wins; config
    is only the fallback when the flag is absent.

    This precedence is load-bearing for safety, not a convenience: the installed
    hook command passes ``--mode`` (the one thing the agent cannot rewrite), and
    ``config.yaml`` is user-writable — so config must never be able to override
    the flag, especially to flip an installed ``enforce`` down to ``shadow``.
    The config path is ``root.parent / config.yaml`` (the gate root lives under
    the data dir). Any read error or unrecognized value falls back to ``shadow``.
    """
    if explicit is not None:
        return explicit
    try:
        from opendaisugi.config import load_config

        mode = load_config(root.parent / "config.yaml").gate_mode
    except Exception:
        return "shadow"
    return mode if mode in ("shadow", "enforce") else "shadow"


@dataclass
class GateDecision:
    """The gate's verdict on one intercepted tool call.

    ``allow`` is what the host is told in the current mode; ``would_deny``
    is what enforce mode would have done — in shadow mode the pair diverges
    by design, and the shadow report is built from ``would_deny``.
    """

    allow: bool
    would_deny: bool
    reason: str
    mode: str
    tool_name: str | None = None
    step_type: str | None = None
    detail: str = ""
    elapsed_ms: float = 0.0
    violations: list[dict[str, Any]] = field(default_factory=list)
    envelope_id: str | None = None
    plan_id: str | None = None
    ask: bool = False  # an operator answered (plan 3, Task 8)
    updated_input: dict[str, Any] | None = None
    # True when the pane rule denied the call. No mode and no operator ask
    # can turn that deny into an allow.
    pane_rule: bool = False
    # How careful an operator's answer must be: silent, undoable or
    # permanent. A decision that never set it is permanent.
    tier: str = PERMANENT
    # Who answered the ask and how that name is known. Set only when an
    # operator answered, so ask is True.
    answered_by: str | None = None
    who_from: str | None = None

    @property
    def clause(self) -> str:
        """The envelope clause that decided it: ``<stage>: <message>`` of the first
        violation, or the reason when there is none (allow, internal error)."""
        if self.violations:
            v = self.violations[0]
            return f"{v.get('stage', '?')}: {v.get('message', '')}"
        return self.reason

    @property
    def counterexample(self) -> dict[str, Any]:
        return dict(self.violations[0].get("detail") or {}) if self.violations else {}


def _deny(
    mode: str,
    reason: str,
    *,
    tool_name: str | None = None,
    step_type: str | None = None,
    detail: str = "",
    t0: float,
    violations: list[dict[str, Any]] | None = None,
    envelope_id: str | None = None,
    plan_id: str | None = None,
) -> GateDecision:
    return GateDecision(
        allow=(mode == "shadow"),
        would_deny=True,
        reason=reason,
        mode=mode,
        tool_name=tool_name,
        step_type=step_type,
        detail=detail,
        elapsed_ms=(time.monotonic() - t0) * 1000,
        violations=violations or [],
        envelope_id=envelope_id,
        plan_id=plan_id,
    )


def _maybe_ask(
    root: Path,
    payload: dict[str, Any],
    decision: GateDecision,
    *,
    timeout_s: float,
    session_id: str | None = None,
    fmt: str = "claude",
    sleep: Callable[[float], None] = time.sleep,
    clock: Callable[[], float] = time.monotonic,
) -> GateDecision:
    """Hand a would-deny to a present operator for at most ``timeout_s``.

    No operator (or no ``tool_use_id`` to key the ask on) → the deny stands,
    nothing is written. Operator allow → allow, marked ``ask``, carrying any
    ``updatedInput`` the operator supplied. Operator deny, a timeout, or a
    rejected (mismatched-nonce, stale, malformed) answer → deny with the
    reason. This function is pure file-polling I/O — it never touches Z3 or
    ``z3_checks.Z3_SOLVE_LOCK``: a would-deny is already fully decided by the
    time this runs, and a 90 s ask must never be able to block another
    session's (millisecond) Z3 solve on the shared resident gate server.
    """
    from dataclasses import replace

    from opendaisugi import ask as _ask

    tool_use_id = payload.get("tool_use_id")
    if not tool_use_id or not _ask.operator_present(root):
        return decision
    tid = str(tool_use_id)
    deadline = time.time() + timeout_s
    _ask.post_ask(
        root,
        tool_use_id=tid,
        question={
            "sessionId": payload.get("session_id"),
            "toolName": decision.tool_name,
            "detail": decision.detail,
            "reason": decision.reason,
            "clause": decision.clause,
            "counterexample": decision.counterexample,
            "toolInput": payload.get("tool_input"),
            "tier": decision.tier,
        },
        deadline=deadline,
    )
    try:
        _report_blocked(
            root,
            payload,
            decision,
            session_id=session_id,
            fmt=fmt,
            tool_use_id=tid,
            deadline=deadline,
        )
    except Exception:  # noqa: BLE001 — must never skip wait_answer below
        pass
    # wait_answer starts its OWN clock here, not when `deadline` above was
    # computed — post_ask's file write and _report_blocked's own (bounded)
    # network call both cost real wall-clock time first. Deriving the
    # wait's timeout from the SAME `deadline` (as a remaining duration,
    # measured right before the call) keeps the floor's already-reported
    # ask.deadline consistent with when the operator's window actually
    # closes, instead of advertising a deadline the real wait outlives (S6).
    remaining = max(0.0, deadline - time.time())
    reply = _ask.wait_answer(root, tool_use_id=tid, timeout_s=remaining, sleep=sleep, clock=clock)
    by, who_from = _ask.who_of(reply.get("by"), reply.get("whoFrom")) if reply else (None, None)
    if reply and reply.get("decision") == "allow":
        why = reply.get("reason") or "no reason given"
        if reply.get("scope") == "task" and decision.tier == UNDOABLE:
            _propose_for_task(root, payload, decision, tid)
        return replace(
            decision,
            allow=True,
            ask=True,
            reason=f"allowed by operator: {why}",
            updated_input=reply.get("updatedInput") or None,
            answered_by=by,
            who_from=who_from,
        )
    why = "operator denied" if reply else f"operator did not answer within {int(timeout_s)} s"
    return replace(
        decision,
        ask=bool(reply),
        reason=f"{decision.reason} ({why})",
        answered_by=by,
        who_from=who_from,
    )


# How long a proposal an operator makes with "allow for this task" stays
# listed before it expires. The same span the session view uses.
_TASK_PROPOSAL_TTL_S = 30 * 86400


def _propose_for_task(
    root: Path, payload: dict[str, Any], decision: GateDecision, tool_use_id: str
) -> None:
    """Record an operator's "allow for this task" as a proposed envelope
    edit. Nothing applies it. Best-effort: a failed write changes no
    verdict."""
    from opendaisugi import ask as _ask

    try:
        _ask.propose(
            root,
            kind="allow-pattern",
            scope="task",
            expires_at=time.time() + _TASK_PROPOSAL_TTL_S,
            body={
                "sessionId": payload.get("session_id"),
                "toolUseId": tool_use_id,
                "toolInput": payload.get("tool_input"),
                "clause": decision.clause,
                "tier": decision.tier,
            },
        )
    except Exception:  # noqa: BLE001 - a proposal is best-effort by contract
        pass


_PANE_ENV_VARS: tuple[str, ...] = ("COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE", "TMUX_PANE")


def _string_harness_session_id(payload: dict[str, Any]) -> str | None:
    """The payload's own session_id, kept only when it is actually a string.

    A hook payload is untrusted input: session_id can arrive as anything
    JSON allows. harness_session_id ends up in a PaneStateEvent and in the
    session tree's own header, both of which expect a string or nothing,
    so a non-string value is dropped here rather than carried through.
    """
    raw = payload.get("session_id")
    return raw if isinstance(raw, str) else None


def _pane_from_env() -> str | None:
    """The running pane's id, read straight from the environment.

    Checked in order: COPPICE_PANE, then HERDR_PANE_ID, then HERDR_PANE,
    then TMUX_PANE. None when none of the four is set. Reads os.environ
    only, no injected mapping: this module is part of the layer and may
    not import opendaisugi.floor, which owns the PaneStateEvent contract
    this value ends up in.
    """
    for name in _PANE_ENV_VARS:
        value = os.environ.get(name)
        if value:
            return value
    return None


# Set on a resident gate server's handler thread for the length of one
# call. See resident_call.
_resident = threading.local()

# gate_server.py's serve() drops these four from a resident server's own
# os.environ at start, so os.environ inside that process is already
# pane-free by the time any call runs. _pane_free_env() below filters them
# out again anyway rather than trust that invariant blindly: a resident
# report must never carry pane identity from this process's own
# environment, whatever else changes.
_RESIDENT_PANE_ENV_KEYS = ("COPPICE_SOCK", "COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE")


def _pane_free_env() -> dict[str, str]:
    """A copy of this process's own environment with the four coppice/Herdr
    pane variables removed, and everything else (PATH, HOME,
    XDG_RUNTIME_DIR, ...) kept. Used for a resident report's ``env=``
    argument: an empty ``{}`` would starve the Herdr CLI subprocess of its
    own environment (it needs more than PATH to find its daemon), and a
    resident report must not use this process's environment for pane
    identity either way. This gives both at once.
    """
    return {k: v for k, v in os.environ.items() if k not in _RESIDENT_PANE_ENV_KEYS}


@contextmanager
def resident_call(
    *,
    sock: str | None = None,
    pane: str | None = None,
    herdr_pane: str | None = None,
    peer_pid: int | None = None,
    data_dir: str | None = None,
) -> Iterator[None]:
    """Mark the calls made inside this block as served by the resident gate.

    The resident gate serves every session from one process. Its own
    environment names no pane at all (it drops COPPICE_SOCK, COPPICE_PANE,
    HERDR_PANE_ID and HERDR_PANE at start; see gate_server.py's serve()),
    so a resident call never names a transcript path or a harness session
    id to coppice: either would let a wrongly-attributed pane read another
    session's transcript or resume another session. The session tree keeps
    both.

    ``sock``/``pane``/``herdr_pane`` are the CALLER's own pane identity, as
    the request that reached the resident server named it explicitly
    (gate_server.py reads them off the wire request, never off this
    process's environment). ``peer_pid`` is different in kind: it is the
    real OS pid of whatever process connected to gate.sock for this call,
    read with SO_PEERCRED by gate_server.py's handler, never taken from
    the request body a caller controls. ``_report_and_append_state`` reads
    all four back out through ``_resident.caller`` and passes them
    straight to ``report_state``, the one place that still validates and
    delivers them, so the live coppice report lands on the caller's own
    pane, never on whichever pane the resident server happened to start
    in, never on nothing just because this process's environment was
    cleared, and (with ``peer_pid`` naming a real pid) never on a pane
    some OTHER caller merely claimed to be. The flag and the caller
    identity are both per thread, so the resident server's other threads
    and an in-process gate are not touched.

    ``data_dir`` is the caller's COPPICE_DATA_DIR, as its request named
    it. The hard-deny rules guard that directory's secrets and files the
    same as the default data directory's. It can only add a guarded
    directory, never take the default ones away.
    """
    prev_on = getattr(_resident, "on", False)
    prev_caller = getattr(_resident, "caller", None)
    _resident.on = True
    _resident.caller = {
        "sock": sock,
        "pane": pane,
        "herdr_pane": herdr_pane,
        "peer_pid": peer_pid,
        "data_dir": data_dir,
    }
    try:
        yield
    finally:
        _resident.on = prev_on
        _resident.caller = prev_caller


def _report_and_append_state(
    root: Path,
    *,
    session_id: str,
    harness: str,
    cwd: str,
    harness_session_id: str | None,
    transcript_path: str | None,
    state: str,
    detail: str,
    ask: dict[str, Any] | None = None,
    tree: "SessionTree | None" = None,
    verdict: dict[str, Any] | None = None,
    mode: str | None = None,
) -> None:
    """Build one PaneStateEvent, deliver it, and mirror it into the session
    tree — three INDEPENDENT best-effort steps (spec-01): a socket failure
    must not also swallow the purely-local tree write, and neither may
    ever change a verdict.

    Shared by the live 'blocked' report (posted inside _maybe_ask, before
    the wait — there is no already-open tree to reuse there) and the final
    'working' report (posted once gate_and_contract's pipeline is done —
    _log_tree already opened one). ``tree``, when given, is reused instead
    of reopening the session tree file and rescanning it for its head a
    second time on the SAME gate call (S4, spec-01) — SessionTree.append's
    only expensive path (session_tree.py's head()/_compute_head) is a full
    ``path.read_text()``.

    ALL of the exception boundaries below matter: this is called (directly,
    for the blocked report; via _maybe_report_state, for the final report)
    from inside _maybe_ask/gate_and_contract's flow, where an escape must
    never propagate — for the blocked report specifically, it sits between
    post_ask and wait_answer, and an escape there would skip the wait
    entirely, leaving an operator's ask posted but never honored.
    """
    try:
        from opendaisugi._state_report import (
            _valid_coppice_pane_id,
            build_event,
            report_transcript_path,
        )

        resident = getattr(_resident, "on", False)
        # The caller's own pane identity, as resident_call() was entered
        # with it. Never this process's own environment, which the
        # resident server drops at start (gate_server.py's serve()).
        caller = (getattr(_resident, "caller", None) or {}) if resident else {}
        # The claimed pane is only a string the wire request carried; it is
        # authenticated against the caller's real peer_pid inside
        # report_state's own coppice leg below, but that authentication
        # happens on the LIVE coppice connection, not here. This event
        # body is written into the local session tree regardless of what
        # coppice ends up doing with the report, so an unshaped claim
        # (never a real coppice pane id in the first place) must not be
        # carried into it either: validate it here too, the same shape
        # check report_state itself applies, or drop it.
        claimed_pane = caller.get("pane") if resident else None
        resident_pane = claimed_pane if _valid_coppice_pane_id(claimed_pane) else None
        ev_json = build_event(
            session_id=session_id,
            harness=harness,
            state=state,
            source="gate",
            harness_session_id=None if resident else harness_session_id,
            pane=resident_pane if resident else _pane_from_env(),
            detail=detail,
            ask=ask,
            transcript_path=None if resident else report_transcript_path(transcript_path),
            verdict=verdict,
            mode=mode,
        )
    except Exception:  # noqa: BLE001 — best-effort by contract
        return
    try:
        from opendaisugi._state_report import report_state

        if resident:
            # A pane-free copy of this process's real environment, not {}.
            # report_state already ignores env for IDENTITY once any of
            # sock/pane/herdr_pane is given, but the Herdr leg still uses
            # env for the herdr subprocess's own environment (PATH to find
            # the binary, HOME/XDG_RUNTIME_DIR for it to run at all). An
            # empty env starves that subprocess, not just the identity it
            # must never read from here.
            report_state(
                ev_json,
                env=_pane_free_env(),
                sock=caller.get("sock"),
                pane=caller.get("pane"),
                herdr_pane=caller.get("herdr_pane"),
                peer_pid=caller.get("peer_pid"),
            )
        else:
            report_state(ev_json)
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    try:
        from opendaisugi.session_tree import SessionTree

        t = tree
        if t is None:
            t = SessionTree.open_or_create(
                root.parent / "sessions",
                session_id=session_id,
                harness=harness,
                cwd=cwd,
                harness_session_id=harness_session_id,
                transcript_path=transcript_path,
            )
        t.append("state", json.loads(ev_json))
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass


def _floor_mode(mode: str) -> str:
    """The mode as the floor names it: ``enforce`` is ``enforcing``, and
    every other value, ``shadow`` included, is ``watching``. A mode the
    gate does not know never reads as enforcing."""
    return "enforcing" if mode == "enforce" else "watching"


def _report_blocked(
    root: Path,
    payload: dict[str, Any],
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
    tool_use_id: str,
    deadline: float,
) -> None:
    """Live-report 'blocked' the moment an ask is posted to a present
    operator — before the wait, not after. That is the only point this
    state is actually true, and the only point a floor watching gate.sock
    can show a countdown against the ask's deadline.

    The whole body is wrapped, matching _log_tree/_maybe_checkpoint's own
    pattern: this sits BETWEEN post_ask and wait_answer (_maybe_ask), so
    an escape here must never propagate on its own — it would skip
    wait_answer entirely, leaving a posted ask nobody ever waits on.
    _maybe_ask ALSO wraps its call to this function as a second line of
    defense (spec-01).
    """
    try:
        sid = _safe_session_id(session_id or payload.get("session_id"))
        _report_and_append_state(
            root,
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=_string_harness_session_id(payload),
            transcript_path=payload.get("transcript_path"),
            state="blocked",
            detail=f"awaiting operator: {decision.reason}"[:200],
            ask={
                "id": tool_use_id,
                "tool": str(decision.tool_name or "unknown"),
                "summary": decision.detail,
                "deadline": deadline,
                "tier": decision.tier,
            },
            verdict={
                "decision": "ask",
                "tool": str(decision.tool_name or "unknown"),
                "clause": decision.clause,
            },
            mode=_floor_mode(decision.mode),
        )
    except Exception:  # noqa: BLE001 — best-effort by contract; the wait must still happen
        pass


def _maybe_report_state(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
    tree: "SessionTree | None",
) -> None:
    """Best-effort: tell whatever floor is listening the call resolved.

    Runs once the verdict, the tree write, and any checkpoint are all final
    (spec-01) — mirrors _log_tree's own placement and contract. Any ask
    this call triggered is already resolved by the time this runs
    (_maybe_ask blocks synchronously), so the state reported here is
    always 'working': allow vs deny shows up in DETAIL, not in floor
    STATE — 'blocked' means waiting on a human, never 'was denied'.

    ``tree`` is the SessionTree `_log_tree` already opened for this same
    call (or `None` if that failed) — passed through so the state entry
    lands in the SAME tree object right after the verdict entry (S4/S5:
    no second file open+scan, and the file order is guaranteed
    tool_call → verdict → state by construction). The whole body is
    wrapped so a failure in this preamble (session-id sanitizing, the
    detail format string, the harness lookup) cannot escape either — its
    call site in gate_and_contract wraps it again, matching the pattern
    _report_blocked/`_maybe_ask` use (B4, spec-01).

    A non-dict ``payload`` (unparseable hook stdin — no tool call ever
    happened) reports nothing, mirroring ``_log_tree``'s own guard: there
    is no real call to say "resolved" about, and reporting one would create
    a session-tree directory for a session_id that only exists because a
    fallback filled it in (regression: ``test_no_payload_writes_nothing``).
    """
    if not isinstance(payload, dict):
        return
    try:
        p = payload
        sid = _safe_session_id(session_id or p.get("session_id"))
        detail = "verdict=allow" if decision.allow else f"verdict=deny clause={decision.clause}"
        _report_and_append_state(
            root,
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(p.get("cwd") or ""),
            harness_session_id=_string_harness_session_id(p),
            transcript_path=p.get("transcript_path"),
            state="working",
            detail=detail[:200],
            tree=tree,
            verdict={
                "decision": "allow" if decision.allow else "deny",
                "tool": str(decision.tool_name or "unknown"),
                # An allow names a clause only when an enforcing gate would
                # have denied the call, which in watching mode is the news.
                "clause": decision.clause if decision.would_deny else "",
            },
            mode=_floor_mode(decision.mode),
        )
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass


# A dispatched verification gets this fraction of the gate's inner budget as
# the bound on the whole verify_via call, oracle included. The rest is slack
# so the join in _verify_with_timeout always sees a verdict. Without the
# split one hung client turns every tool call into a permanent "gate internal
# error" deny with no clue which client did it.
_DISPATCH_BUDGET_FRACTION = 0.5


def _gate_root(root: Path | None) -> Path:
    """The gate root in force: the one given, else the data dir's ``gate``."""
    from opendaisugi import DEFAULT_DATA_DIR

    return Path(root) if root is not None else DEFAULT_DATA_DIR / "gate"


def _configured_verifier_client(root: Path | None = None) -> tuple[Path, str]:
    """The gate root and the verifier_client named beside it, read now.

    The config file is ``root.parent / "config.yaml"``, the same file
    ``resolve_gate_mode`` reads, so a hook run with ``--root /srv/x/gate``
    takes both its mode and its verifier from ``/srv/x/config.yaml``. A
    config that cannot be read names python, so verification never skips.
    """
    from opendaisugi.config import load_config

    gate_root = _gate_root(root)
    try:
        client = load_config(gate_root.parent / "config.yaml").verifier_client
    except Exception:  # noqa: BLE001 - a broken config must not skip verification
        client = "python"
    return gate_root, str(client or "python")


def _dispatch_verify(
    plan: ActionPlan, envelope: Envelope, *, timeout_s: float, root: Path | None = None
):
    """Verify with the client this gate's config names, right now.

    The choice is read per call, which is what makes the verifier stage live:
    the resident gate is long-lived, so a cached choice would need a restart.
    verify_via runs the oracle too and returns the conjunction, so a dispatch
    failure costs a warning and never an allow; anything that still escapes is
    caught by evaluate_record's fail-closed except.

    The choice comes from ``root.parent / "config.yaml"`` and the dispatch
    record is written under ``root``, so ``daisugi modules`` for that data dir
    sees the dispatch. With no root the gate lives under the data dir, so a
    test that points the data dir at a temp directory never writes to the
    operator's home.
    """
    gate_root, client = _configured_verifier_client(root)
    if client == "python":
        return verify(plan, envelope, strict=None)
    from opendaisugi.bench import options
    from opendaisugi.verifier_dispatch import verify_via

    return verify_via(
        client,
        plan,
        envelope,
        timeout_s=timeout_s * _DISPATCH_BUDGET_FRACTION,
        root=gate_root,
        locate=options.gate_client_argv,
    )


def _verify_with_timeout(
    plan: ActionPlan, envelope: Envelope, timeout_s: float, root: Path | None = None
):
    """Run the configured verifier in a worker thread with an inner deny-on-timeout.

    Returns the VerificationResult, or raises TimeoutError when the verifier
    outlives the budget. The worker is a daemon thread — a hung Z3 query
    cannot pin the gate process open past its own deadline.
    """
    box: list[Any] = []
    err: list[BaseException] = []

    def _run() -> None:
        try:
            box.append(_dispatch_verify(plan, envelope, timeout_s=timeout_s, root=root))
        except BaseException as exc:  # noqa: BLE001 — re-raised on the caller side
            err.append(exc)

    worker = threading.Thread(target=_run, daemon=True)
    worker.start()
    worker.join(timeout_s)
    if worker.is_alive():
        raise TimeoutError(f"verifier exceeded the gate's inner time budget ({timeout_s}s)")
    if err:
        raise err[0]
    return box[0]


def evaluate_record(
    record: dict[str, Any],
    envelope: Envelope,
    *,
    mode: str = "shadow",
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    root: Path | None = None,
    cwd: Workspace | str | None = None,
    call_cwd: str | None = None,
) -> GateDecision:
    """Decide one already-normalized capture record against an envelope.

    Deny-by-default: every failure path inside this function resolves to a
    deny decision, never an exception to the caller. ``root`` is the gate
    root whose config names the verifier client; None means the data dir's.
    ``cwd`` is the workspace the tier reads: a ``Workspace``, or one
    directory that is both the working directory and the root. Without it,
    no write counts as inside the workspace.
    """
    decision = _evaluate_record(
        record, envelope, mode=mode, verify_timeout_s=verify_timeout_s, root=root
    )
    decision.tier = _tier(record, decision, cwd, call_cwd)
    return decision


def _tier(
    record: dict[str, Any] | None,
    decision: GateDecision,
    cwd: Workspace | str | None,
    call_cwd: str | None = None,
) -> str:
    """The tier of one decision. An allowed call is silent. A deny with no
    envelope clause behind it, such as an internal error or a timeout, is
    permanent. Any other deny takes the tier of the call's effect class.
    Any error here is permanent."""
    try:
        if not decision.would_deny:
            return SILENT
        if not decision.violations:
            return PERMANENT
        return tier_for(effect_class(record, cwd, call_cwd))
    except Exception:  # noqa: BLE001 - an unplaced call is permanent
        return PERMANENT


def _evaluate_record(
    record: dict[str, Any],
    envelope: Envelope,
    *,
    mode: str,
    verify_timeout_s: float,
    root: Path | None,
) -> GateDecision:
    t0 = time.monotonic()
    tool_name = record.get("tool_name")
    step_type = record.get("step_type")
    detail = str(record.get("command") or record.get("path") or record.get("url") or "")
    try:
        steps = _records_to_steps([record])
        if not steps:
            return _deny(
                mode,
                f"could not synthesize a step for tool {tool_name!r}",
                tool_name=tool_name,
                step_type=step_type,
                detail=detail,
                t0=t0,
            )
        plan = ActionPlan(source="call-time-gate", task=envelope.task, steps=steps)
        result = _verify_with_timeout(plan, envelope, verify_timeout_s, root)
        if result.ok:
            timeout_warnings = [w for w in result.warnings if is_z3_timeout_warning(w)]
            if timeout_warnings:
                # verify() kept this Z3 `unknown` as a warning (lenient
                # library default), but the gate decides whether to let a
                # call run right now. It must never allow one whose check
                # could not finish, whatever the envelope's stakes. Shadow
                # mode still logs this as a would-deny, same as any other
                # deny (see _log_shadow's caller, gate_and_contract).
                return _deny(
                    mode,
                    "the verifier could not finish a check in time; denied ("
                    + "; ".join(timeout_warnings)
                    + ")",
                    tool_name=tool_name,
                    step_type=step_type,
                    detail=detail,
                    t0=t0,
                    envelope_id=result.envelope_id,
                    plan_id=result.plan_id,
                )
            return GateDecision(
                allow=True,
                would_deny=False,
                reason="verified in envelope",
                mode=mode,
                tool_name=tool_name,
                step_type=step_type,
                detail=detail,
                elapsed_ms=(time.monotonic() - t0) * 1000,
                violations=[],
                envelope_id=result.envelope_id,
                plan_id=result.plan_id,
            )
        summary = (
            "; ".join(f"{v.stage}: {v.message}" for v in result.violations) or "verification failed"
        )
        return _deny(
            mode,
            summary,
            tool_name=tool_name,
            step_type=step_type,
            detail=detail,
            t0=t0,
            violations=[v.model_dump(mode="json") for v in result.violations],
            envelope_id=result.envelope_id,
            plan_id=result.plan_id,
        )
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        try:
            chosen = _configured_verifier_client(root)[1]
        except Exception:  # noqa: BLE001 - the deny must not depend on the config
            chosen = "python"
        via = "" if chosen == "python" else f" with verifier_client={chosen}"
        hint = (
            ""
            if chosen == "python"
            else " Set verifier_client: python in config.yaml to rule out the client."
        )
        return _deny(
            mode,
            f"gate internal error{via} (denied fail-closed): {exc}.{hint}",
            tool_name=tool_name,
            step_type=step_type,
            detail=detail,
            t0=t0,
        )


def evaluate_call(
    payload: Any,
    envelope: Envelope,
    *,
    mode: str = "shadow",
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    fmt: str = "claude",
    root: Path | None = None,
) -> GateDecision:
    """Decide one raw hook payload against an envelope. Deny-by-default.

    Never raises: malformed payloads, unknown tools, verifier errors, and
    verifier timeouts all come back as deny decisions (allowed-but-flagged
    in shadow mode). ``fmt`` selects the host-specific tool-name
    classification in hook's ``_payload_to_record``. It does not change
    verification itself.
    """
    t0 = time.monotonic()
    try:
        if not isinstance(payload, dict):
            return _deny(mode, "hook payload is not a JSON object", t0=t0)
        tool_name = payload.get("tool_name") or payload.get("tool") or payload.get("name")
        if not tool_name:
            return _deny(mode, "no tool name in hook payload", t0=t0)
        args = (envelope, mode, verify_timeout_s, root, t0)
        if tool_name == APPLY_PATCH_TOOL:
            return _decide_patch(payload, str(tool_name), *args)
        record = _payload_to_record(payload, fmt=fmt)
        return _decide(payload, record, str(tool_name), *args)
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}", t0=t0)


_TIER_RANK = {SILENT: 0, UNDOABLE: 1, PERMANENT: 2}


def _decide(
    payload: dict[str, Any],
    record: dict[str, Any] | None,
    tool_name: str,
    envelope: Envelope,
    mode: str,
    verify_timeout_s: float,
    root: Path | None,
    t0: float,
) -> GateDecision:
    """Decide one normalized record of a call: the pane rule and the two
    hard-deny rules first, then the envelope."""
    command = record.get("command") if record else None
    if record and record.get("step_type") == "shell" and isinstance(command, str):
        if len(command) > MAX_SHELL_COMMAND_CHARS:
            return _deny(
                mode,
                f"shell command is longer than {MAX_SHELL_COMMAND_CHARS} characters; the gate does not read it",
                tool_name=tool_name,
                step_type="shell",
                detail=command,
                t0=t0,
            )
    if pane_rule_hit(payload, record, root):
        return GateDecision(
            allow=False,
            would_deny=True,
            reason=PANE_REFUSAL,
            mode=mode,
            tool_name=tool_name,
            step_type=record.get("step_type") if record else None,
            detail=str((record or {}).get("command") or (record or {}).get("path") or ""),
            elapsed_ms=(time.monotonic() - t0) * 1000,
            pane_rule=True,
        )
    for hit, refusal in (
        (floor_config_hit, FLOOR_REFUSAL),
        (opencode_plugin_hit, OPENCODE_PLUGIN_REFUSAL),
        (search_above_secret_hit, SEARCH_REFUSAL),
    ):
        if hit(payload, record):
            return GateDecision(
                allow=False,
                would_deny=True,
                reason=refusal,
                mode=mode,
                tool_name=tool_name,
                step_type=record.get("step_type") if record else None,
                detail=str((record or {}).get("command") or (record or {}).get("path") or ""),
                elapsed_ms=(time.monotonic() - t0) * 1000,
                pane_rule=True,
            )
    if record is None:
        return _deny(
            mode,
            f"unrecognized tool {tool_name!r} — not in the gate's "
            "classification map, denied by default",
            tool_name=tool_name,
            t0=t0,
        )
    return evaluate_record(
        record,
        envelope,
        mode=mode,
        verify_timeout_s=verify_timeout_s,
        root=root,
        cwd=workspace_root(payload.get("cwd"), os.environ.get(_PROJECT_DIR_ENV)),
        call_cwd=payload.get("cwd") if isinstance(payload.get("cwd"), str) else None,
    )


def _decide_patch(
    payload: dict[str, Any],
    tool_name: str,
    envelope: Envelope,
    mode: str,
    verify_timeout_s: float,
    root: Path | None,
    t0: float,
) -> GateDecision:
    """Decide an OpenCode apply_patch as one file write per path it names,
    each placed from the call's working directory.

    A patch the gate cannot read, or a call with no absolute working
    directory, is denied. A hard-deny hit on any path
    decides the call. Else the call is allowed only when every path is, and
    a deny carries the worst tier among its paths.
    """
    inp = payload.get("tool_input") or payload.get("args") or payload.get("input") or {}
    paths = parse_apply_patch(inp.get("patchText") if isinstance(inp, dict) else None)
    if paths is None:
        return _deny(
            mode,
            "apply_patch: the gate cannot read this patch, so it denies it. A patch "
            "needs Begin Patch and End Patch lines and at least one file header.",
            tool_name=tool_name,
            t0=t0,
        )
    cwd = payload.get("cwd")
    if not isinstance(cwd, str) or not os.path.isabs(cwd):
        return _deny(
            mode,
            "apply_patch: the call names no absolute working directory, so the gate "
            "cannot place the patch's paths. It denies the patch.",
            tool_name=tool_name,
            t0=t0,
        )
    decisions: list[GateDecision] = []
    for raw in paths:
        # OpenCode resolves each patch path from the session directory,
        # so the gate checks the path it will write, not the text.
        path = os.path.normpath(os.path.join(cwd, raw))
        record: dict[str, Any] = {
            "captured_at": time.time(),
            "session_id": _safe_session_id(payload.get("session_id")),
            "tool_name": "Write",
            "step_type": "file_write",
            "path": path,
            "content_len": 0,
        }
        record.update(join_keys(payload))
        d = _decide(payload, record, tool_name, envelope, mode, verify_timeout_s, root, t0)
        if d.pane_rule:
            return d
        decisions.append(d)
    denies = [d for d in decisions if d.would_deny]
    chosen = decisions[-1]
    if denies:
        chosen = max(denies, key=lambda d: _TIER_RANK.get(d.tier, 2))
    chosen.tool_name = tool_name
    return chosen


# ---------------------------------------------------------------------------
# Envelope registration channel + disarm switch + host contract (I/O layer)
# ---------------------------------------------------------------------------


def _envelopes_dir(root: Path) -> Path:
    return root / "envelopes"


def _shadow_dir(root: Path) -> Path:
    return root / "shadow"


def _mkdir_private(d: Path) -> None:
    d.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(d, 0o700)
    except OSError:
        pass


# A conservative starting shell allowlist for a generated envelope: read-
# oriented inspection plus version control and test runners. It is
# deliberately NOT a blanket ``*`` and omits the interpreters (``bash``,
# ``sh``) and anything that trivially shells out, because the operator is
# meant to *review and tighten or widen* this before trusting enforce mode.
# Shell allowlisting matches only the command head, and shadow mode is where
# you discover what your real session actually needs.
_STARTER_SHELL_ALLOWLIST: tuple[str, ...] = (
    "cat",
    "cd",
    "echo",
    "find",
    "git",
    "grep",
    "head",
    "ls",
    "npm",
    "cargo",
    "printf",
    "pwd",
    "pytest",
    "python",
    "python3",
    "rg",
    "sort",
    "tail",
    "uniq",
    "wc",
    "which",
)


def starter_envelope(
    workspace: Path, *, stakes: str = "medium", allow_shell_decomposition: bool = False
) -> Envelope:
    """Generate a reviewable starter envelope for an existing session.

    This is the drafted-then-reviewed answer to "where does the envelope come
    from?" for an operator's *own* running session (roadmap Stage 1's open
    sub-problem; the onboarding funnel of Stage 6). It grants read/write
    within one workspace, a conservative shell head allowlist, and NO network
    — a sane, tight default that the operator edits before enforcing. It is
    not a security guarantee on its own; it is a starting point that shadow
    mode and `daisugi gate report` help tune.

    ``allow_shell_decomposition`` carries ADR-0010's opt-in through to the
    registered envelope: with it on, a compound command (``a && b``, a pipe) is
    parsed by a real bash grammar and EVERY head is checked against the
    allowlist, instead of the blanket metacharacter rejection. Default off —
    the verdict stays a pure function of (plan, envelope), and the opt-in needs
    ``opendaisugi[shell]`` to be present or the step fails closed.
    """
    ws = str(Path(workspace).resolve())
    return Envelope(
        generated_by="opendaisugi.gate.starter_envelope",
        task=f"session in {ws}",
        permissions=Permission(
            file_read=[f"{ws}/**"],
            file_write=[f"{ws}/**"],
            shell=True,
            shell_allowlist=sorted(_STARTER_SHELL_ALLOWLIST),
            shell_allow_decomposition=allow_shell_decomposition,
            network=False,
            max_execution_time_s=60,
            max_output_size_mb=20,
        ),
        stakes=stakes,
    )


def register_envelope(
    envelope: Envelope, *, session_id: str | None = None, root: Path = DEFAULT_GATE_ROOT
) -> Path:
    """Register an envelope for the gate to check calls against.

    With a ``session_id`` the envelope binds to that session; without one it
    becomes the ``default`` envelope every unmatched session falls back to.
    Files are private (0700 dir / 0600 file) — envelopes reveal what a
    session is allowed to touch.
    """
    d = _envelopes_dir(root)
    _mkdir_private(d)
    name = _safe_session_id(session_id) if session_id else "default"
    path = d / f"{name}.json"
    path.write_text(envelope.model_dump_json(indent=2), encoding="utf-8")
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass
    return path


def load_envelope(session_id: str | None, *, root: Path = DEFAULT_GATE_ROOT) -> Envelope | None:
    """Load the envelope for a session: exact match first, then ``default``."""
    candidates = []
    if session_id:
        candidates.append(_safe_session_id(session_id))
    candidates.append("default")
    for name in candidates:
        path = _envelopes_dir(root) / f"{name}.json"
        if path.exists():
            return Envelope.model_validate_json(path.read_text(encoding="utf-8"))
    return None


def disarm(root: Path = DEFAULT_GATE_ROOT) -> Path:
    """One-command kill switch: an armed gate allows everything while the
    marker exists. Deliberately requires no allowed tool call — the operator
    runs it from any shell, outside the gated agent."""
    _mkdir_private(root)
    marker = root / _DISARM_FILENAME
    marker.write_text("disarmed by operator\n", encoding="utf-8")
    return marker


def arm(root: Path = DEFAULT_GATE_ROOT) -> None:
    """Remove the disarm marker; the gate resumes evaluating calls."""
    marker = root / _DISARM_FILENAME
    if marker.exists():
        marker.unlink()


def is_disarmed(root: Path = DEFAULT_GATE_ROOT) -> bool:
    return (root / _DISARM_FILENAME).exists()


@dataclass
class GateOutcome:
    """What the gate process should emit to the host: stdout, stderr, exit
    code — plus the decision for logging/inspection."""

    stdout: str
    stderr: str
    exit_code: int
    decision: GateDecision


def _outcome(decision: GateDecision, fmt: str) -> GateOutcome:
    # ``not decision.allow`` rather than ``mode == "enforce" and would_deny``:
    # today the two are equivalent for every decision constructor (`_deny`
    # sets ``allow = (mode == "shadow")``, so mode selects allow directly),
    # but an operator-allowed decision (Task 8, `_maybe_ask`) is the first
    # case where they diverge — ``would_deny`` stays True (the report must
    # still show what enforce would have denied) while ``allow`` is True.
    # ``allow`` is the one field that must drive the host contract.
    deny_now = not decision.allow
    if fmt in EXIT_CODE_FORMATS:
        if deny_now:
            return GateOutcome(
                stdout="",
                stderr=f"openDaisugi gate: DENIED — {decision.reason}",
                exit_code=2,
                decision=decision,
            )
        if decision.updated_input:
            if fmt == "claude":
                # The operator edited the call before allowing it. Claude
                # Code's PreToolUse contract for a modified-but-allowed call
                # is hookSpecificOutput.updatedInput, not {"continue":true}.
                stdout = json.dumps(
                    {
                        "hookSpecificOutput": {
                            "hookEventName": "PreToolUse",
                            "permissionDecision": "allow",
                            "permissionDecisionReason": decision.reason,
                            "updatedInput": decision.updated_input,
                        }
                    }
                )
                return GateOutcome(stdout=stdout, stderr="", exit_code=0, decision=decision)
            # An exit-code-only host has no channel that carries an operator
            # edit. Deny fail-closed rather than run the original input,
            # which was already denied.
            return GateOutcome(
                stdout="",
                stderr=(
                    f"openDaisugi gate: DENIED: {decision.reason}. The operator edit "
                    f"cannot be carried on the {fmt!r} format. It has no updatedInput "
                    "channel. Denied fail-closed rather than running the original input."
                ),
                exit_code=2,
                decision=decision,
            )
        return GateOutcome(
            stdout=stdout_for_format(fmt, block=False),
            stderr="",
            exit_code=0,
            decision=decision,
        )
    if fmt in STDOUT_BLOCK_FORMATS:
        if decision.updated_input:
            # An operator edited the call before allowing it, but only the
            # claude contract above has a channel that carries an edit to
            # the host. A plain allow here would drop the edit and run the
            # original, denied input. Deny fail-closed instead.
            return GateOutcome(
                stdout=stdout_for_format(
                    fmt,
                    block=True,
                    reason=f"{decision.reason}. The operator edit cannot be carried on the "
                    f"{fmt!r} format. It has no updatedInput channel. Denied fail-closed "
                    "rather than running the original input",
                ),
                stderr="",
                exit_code=0,
                decision=decision,
            )
        return GateOutcome(
            stdout=stdout_for_format(fmt, block=deny_now, reason=decision.reason),
            stderr="",
            exit_code=0,
            decision=decision,
        )
    # fmt is in neither set. --format is an unrestricted string, so a typo or
    # a host with no wired contract reaches here. stdout_for_format's default
    # body is an allow at exit 0, which could carry a real deny. Fail closed
    # instead of guessing how an unknown host reads a body it never learned.
    return GateOutcome(
        stdout="",
        stderr=(
            f"openDaisugi gate: DENIED: unknown host format {fmt!r}. "
            "Use --format claude, pi, opencode, hermes, or openclaw."
        ),
        exit_code=2,
        decision=decision,
    )


def _log_shadow(
    root: Path,
    session_id: str | None,
    decision: GateDecision,
    payload_session_id: str | None = None,
    *,
    join: dict[str, Any] | None = None,
) -> None:
    """Best-effort JSONL decision log — the raw material of the shadow
    report. Never raises; a logging failure must not change a verdict."""
    try:
        d = _shadow_dir(root)
        _mkdir_private(d)
        path = d / f"{_safe_session_id(session_id)}.jsonl"
        newly_created = not path.exists()
        rec = {
            "at": time.time(),
            "session_id": _safe_session_id(session_id),
            # What the payload claimed, kept even when the envelope was pinned
            # to something else — so a report can show a mismatch.
            "payload_session_id": payload_session_id,
            "tool_name": decision.tool_name,
            "step_type": decision.step_type,
            "detail": decision.detail,
            "mode": decision.mode,
            "allow": decision.allow,
            "would_deny": decision.would_deny,
            "reason": decision.reason,
            "elapsed_ms": round(decision.elapsed_ms, 3),
            "clause": decision.clause,
            "violations": decision.violations,
            "envelope_id": decision.envelope_id,
            "plan_id": decision.plan_id,
            "ask": decision.ask,
            "tier": decision.tier,
        }
        if decision.ask:
            # An answered ask always names who gave it. No name is local.
            key = "allowed_by" if decision.allow else "denied_by"
            rec[key] = decision.answered_by or "local"
            rec["who_from"] = decision.who_from or "none"
        rec.update(join or {})
        with path.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec) + "\n")
        if newly_created:
            try:
                os.chmod(path, 0o600)
            except OSError:
                pass
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        pass


_HARNESS_BY_FMT = {
    "claude": "claude-code",
    "codex": "codex",
    "hermes": "hermes",
    "openclaw": "openclaw",
    "pi": "pi",
}


def _log_tree(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
) -> "SessionTree | None":
    """Best-effort mirror of the call and its verdict into the session tree.

    The multi-session view reads this. Never raises; a store failure must
    not change a verdict (same contract as ``_log_shadow``). Returns the
    ``SessionTree`` it opened on success, ``None`` on any failure or a
    non-dict payload — the caller (``_maybe_report_state``, via
    ``gate_and_contract``) reuses this SAME tree for the final 'working'
    state entry instead of reopening the file and rescanning it for its
    head a second time (S4, spec-01); every prior caller already ignored
    the return value, so this is additive.
    """
    if not isinstance(payload, dict):
        return None
    try:
        from opendaisugi.session_tree import SessionTree

        sid = _safe_session_id(session_id or payload.get("session_id"))
        tree = SessionTree.open_or_create(
            root.parent / "sessions",
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=_string_harness_session_id(payload),
            transcript_path=payload.get("transcript_path"),
        )
        tool_use_id = payload.get("tool_use_id")
        call = tree.append(
            "tool_call",
            {
                "toolUseId": tool_use_id,
                "name": decision.tool_name or payload.get("tool_name"),
                "stepType": decision.step_type,
                "detail": decision.detail,
                "agentId": payload.get("agent_id"),
                "agentType": payload.get("agent_type"),
            },
        )
        tree.append(
            "verdict",
            {
                "toolUseId": tool_use_id,
                "decision": "allow" if decision.allow else "deny",
                "wouldDeny": decision.would_deny,
                "mode": decision.mode,
                "reason": decision.reason,
                "clause": decision.clause,
                "counterexample": decision.counterexample,
                "envelopeId": decision.envelope_id,
                "planId": decision.plan_id,
                "latencyMs": round(decision.elapsed_ms, 3),
                "answeredBy": "operator" if decision.ask else None,
                "tier": decision.tier,
            },
            parent_id=call.id,
        )
        return tree
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        return None


_SKIPPED_INLINE_BYTE_BUDGET = 1500  # keeps the WHOLE checkpoint line comfortably under PIPE_BUF


def _cap_skipped_for_line(
    skipped: list[str], *, budget: int = _SKIPPED_INLINE_BYTE_BUDGET
) -> list[str]:
    """Keep the leading paths whose JSON-encoded size stays under ``budget`` bytes.

    A raw COUNT cap (e.g. "first 20") is not enough: skipped path length
    varies with how deep the workspace nests (a node_modules tree routinely
    puts single paths past 200 chars), so 20 such names alone can still push
    the whole checkpoint line past ``PIPE_BUF`` (4096) — reintroducing
    exactly the unbounded-line problem ``coversCount`` exists to avoid for
    ``covers``. Budgeting bytes instead keeps the guarantee regardless of
    how long any individual skipped path happens to be.
    """
    out: list[str] = []
    used = 0
    for name in skipped:
        cost = len(json.dumps(name)) + 1  # +1 for the list's separating comma
        if used + cost > budget:
            break
        out.append(name)
        used += cost
    return out


def _maybe_checkpoint(root: Path, payload: dict[str, Any], *, session_id: str | None) -> None:
    """Snapshot the workspace once per new prompt. Best-effort; never touches the verdict.

    Stores ``coversCount`` and the ref, not every covered path: a session
    with thousands of files would write a multi-KB JSONL line, which
    exceeds ``PIPE_BUF`` and breaks the single-line ``O_APPEND`` atomicity
    that concurrent writers on a shared session id rely on. The full path
    list is still recoverable later via ``git ls-tree <ref>``. ``skipped``
    stays inline (it names what could NOT be captured, which is exactly what
    an operator needs without another git call) but is capped by
    :func:`_cap_skipped_for_line` — a byte budget, not a count — for the
    same reason; ``skippedCount`` carries the true total.
    """
    try:
        from opendaisugi.checkpoints import is_repo, snapshot
        from opendaisugi.claude_transcript import last_prompt_uuid, read_turns
        from opendaisugi.session_tree import SessionTree

        cwd = payload.get("cwd")
        tpath = payload.get("transcript_path")
        if not cwd or not tpath or not is_repo(Path(cwd)):
            return
        prompt = last_prompt_uuid(read_turns(Path(tpath)))
        if not prompt:
            return
        sid = _safe_session_id(session_id or payload.get("session_id"))
        state = root / "checkpoint-state" / f"{sid}.json"
        last = json.loads(state.read_text()).get("prompt") if state.exists() else None
        if last == prompt:
            return
        tree = SessionTree.open(root.parent / "sessions", sid)
        entry_id = tree.head() or "root"
        cp = snapshot(Path(cwd), session_id=sid, entry_id=entry_id)
        tree.append(
            "checkpoint",
            {
                "ref": cp.ref,
                "commit": cp.commit,
                "coversCount": len(cp.covers),
                "skipped": _cap_skipped_for_line(cp.skipped),
                "skippedCount": len(cp.skipped),
                "promptUuid": prompt,
            },
        )
        _mkdir_private(state.parent)
        state.write_text(json.dumps({"prompt": prompt}))
        try:
            os.chmod(state, 0o600)
        except OSError:
            pass
    except Exception:  # noqa: BLE001 — best-effort by contract, same as _log_tree
        pass


def gate_and_contract(
    raw: bytes,
    *,
    root: Path = DEFAULT_GATE_ROOT,
    fmt: str = "claude",
    mode: str = "shadow",
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    captures_root: Path | None = None,
    pin_session: str | None = None,
    ask: bool = False,
    ask_timeout_s: float = 90.0,
    checkpoints: bool = False,
) -> GateOutcome:
    """Full gate entry: raw hook stdin → decision → host contract.

    Failure policy is mode-selected (ADR-0007): enforce fails CLOSED (any
    error here denies with exit 2), shadow fails OPEN (observation must
    never break the host). Every decision is appended to the shadow log,
    in both modes — enforce sessions produce the same report material.

    With ``captures_root``, calls the gate *allows* are also mirrored into
    passive-capture format (best-effort) so a gated session feeds the same
    captures → to-trace → journal pipeline distillation already reads.
    Denied calls are never mirrored — they didn't happen.

    ``pin_session`` fixes which registered envelope is used, ignoring the
    session id in the payload. Authorization must not key on input the
    caller can influence: unpinned, a payload claiming another session's id
    is checked against *that* session's envelope, which may be more
    permissive. The hook command supplies the pin from outside anything the
    agent can rewrite — the same principle as the sub-agent gate root living
    outside its workspace.

    ``checkpoints`` (off by default) snapshots the workspace into a private
    git ref at most once per new prompt boundary — best-effort, AFTER the
    verdict is already final and fully wrapped (:func:`_maybe_checkpoint`),
    so a checkpoint failure can never change what the host is told.

    ``ask`` (enforce mode only; off by default) hands a would-deny to a
    present operator for at most ``ask_timeout_s`` before letting the deny
    stand (:func:`_maybe_ask`). No ``--ask`` flag, no operator present, an
    operator deny, a rejected/late answer, or a timeout all fall through to
    the same deny this function already produces without ``ask`` — the flag
    only ever *narrows* what gets denied, never widens it beyond what an
    operator explicitly allowed in time.
    """
    t0 = time.monotonic()
    try:
        if is_disarmed(root):
            decision = GateDecision(
                allow=True,
                would_deny=False,
                reason="gate disarmed by operator (marker file present)",
                mode=mode,
                elapsed_ms=(time.monotonic() - t0) * 1000,
            )
            _log_shadow(root, None, decision)
            return _outcome(decision, fmt)
        too_big = len(raw) > MAX_PAYLOAD_BYTES
        try:
            text = "" if too_big else raw.decode("utf-8", "replace")
            payload = json.loads(text) if text.strip() else None
        except Exception:  # noqa: BLE001 — malformed stdin is a deny, not a crash
            payload = None
        payload_session = payload.get("session_id") if isinstance(payload, dict) else None
        # Pinned wins: the payload's claim is recorded but never authorizes.
        session_id = pin_session or payload_session
        envelope = load_envelope(session_id, root=root)
        if envelope is None:
            decision = _deny(
                mode,
                "no envelope registered for this session — run "
                "`daisugi gate register <envelope.json>` to authorize it, or "
                "`daisugi gate disarm` to switch the gate off",
                t0=t0,
            )
        elif too_big:
            decision = _deny(
                mode,
                f"hook payload is larger than {MAX_PAYLOAD_BYTES} bytes; the gate does not read it",
                t0=t0,
            )
        elif payload is None:
            decision = _deny(mode, "hook payload was not parseable JSON", t0=t0)
        else:
            decision = evaluate_call(
                payload,
                envelope,
                mode=mode,
                verify_timeout_s=verify_timeout_s,
                fmt=fmt,
                root=root,
            )
            if (
                ask
                and mode == "enforce"
                and decision.would_deny
                and not decision.pane_rule
                and isinstance(payload, dict)
            ):
                decision = _maybe_ask(
                    root, payload, decision, timeout_s=ask_timeout_s, session_id=session_id, fmt=fmt
                )
        join = join_keys(payload) if isinstance(payload, dict) else {}
        _log_shadow(root, session_id, decision, payload_session_id=payload_session, join=join)
        tree = _log_tree(root, payload, decision, session_id=session_id, fmt=fmt)
        if checkpoints and decision.allow and isinstance(payload, dict):
            _maybe_checkpoint(root, payload, session_id=session_id)
        if captures_root is not None and decision.allow and isinstance(payload, dict):
            try:
                from opendaisugi.hook import record_call

                record_call(payload, root=captures_root)
            except Exception:  # noqa: BLE001 — mirroring is best-effort
                pass
        try:
            _maybe_report_state(root, payload, decision, session_id=session_id, fmt=fmt, tree=tree)
        except Exception:  # noqa: BLE001 — reporting is best-effort by contract (B4, spec-01)
            pass
        return _outcome(decision, fmt)
    except Exception as exc:  # noqa: BLE001 — mode-selected failure policy
        decision = _deny(mode, f"gate I/O error (denied fail-closed): {exc}", t0=t0)
        if mode != "enforce":
            decision = GateDecision(
                allow=True,
                would_deny=True,
                reason=f"gate I/O error (shadow mode allows): {exc}",
                mode=mode,
                elapsed_ms=(time.monotonic() - t0) * 1000,
            )
        return _outcome(decision, fmt)


# ---------------------------------------------------------------------------
# Shadow report + capture replay
# ---------------------------------------------------------------------------


def _is_false_positive_candidate(reason: str) -> bool:
    """Classify a would-deny as a likely false positive worth operator review.

    Two known classes (the product's false-positive economics, per the
    roadmap): compound-command metachar denials (the command may be benign;
    the gate can't prove it and offers a decomposition instead) and host
    tools the classification map doesn't know (TodoWrite, Task, …) which
    deny-by-default sweeps up wholesale.
    """
    return "metacharacters" in reason or reason.startswith("unrecognized tool")


def _build_report(records: list[dict[str, Any]]) -> dict[str, Any]:
    denied = [r for r in records if r.get("would_deny")]
    reasons: dict[str, int] = {}
    for r in denied:
        key = (r.get("reason") or "")[:120]
        reasons[key] = reasons.get(key, 0) + 1
    return {
        "calls": len(records),
        "allowed": sum(1 for r in records if not r.get("would_deny")),
        "would_deny": len(denied),
        "reasons": reasons,
        "denied": denied,
        "false_positive_candidates": [
            r for r in denied if _is_false_positive_candidate(r.get("reason") or "")
        ],
    }


def shadow_report(
    *, root: Path = DEFAULT_GATE_ROOT, session_id: str | None = None
) -> dict[str, Any]:
    """Summarize the shadow log: what an enforcing gate would have denied.

    Denied records are included verbatim so the operator can adjudicate each
    one; the ``false_positive_candidates`` subset flags the two known
    over-denial classes (compound-command metachars, unrecognized host
    tools). One session, or all sessions when ``session_id`` is None.
    """
    d = _shadow_dir(root)
    files = (
        [d / f"{_safe_session_id(session_id)}.jsonl"]
        if session_id
        else sorted(d.glob("*.jsonl"))
        if d.exists()
        else []
    )
    records: list[dict[str, Any]] = []
    for f in files:
        if not f.exists():
            continue
        for line in f.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(rec, dict):  # a line that is JSON but not a record is skipped too
                records.append(rec)
    return _build_report(records)


def replay_captures(
    captures_jsonl: Path,
    envelope: Envelope,
    *,
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    root: Path | None = None,
) -> dict[str, Any]:
    """Run a passively captured session back through the gate, offline.

    This is how an operator tunes an envelope against a real session before
    trusting enforce mode: every captured call is decided in shadow terms
    against ``envelope`` and summarized like :func:`shadow_report` — false
    positive candidates included. Nothing is executed and nothing is denied;
    the captures are historical.
    """
    records: list[dict[str, Any]] = []
    for line in captures_jsonl.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            cap = json.loads(line)
        except json.JSONDecodeError:
            continue
        decision = evaluate_record(
            cap,
            envelope,
            mode="shadow",
            verify_timeout_s=verify_timeout_s,
            root=root,
        )
        records.append(
            {
                "at": cap.get("captured_at"),
                "session_id": cap.get("session_id"),
                "tool_name": decision.tool_name,
                "step_type": decision.step_type,
                "detail": decision.detail,
                "mode": "shadow",
                "allow": decision.allow,
                "would_deny": decision.would_deny,
                "reason": decision.reason,
                "elapsed_ms": round(decision.elapsed_ms, 3),
            }
        )
    return _build_report(records)


# ---------------------------------------------------------------------------
# Host wiring: settings emitter + lean hook entry
# ---------------------------------------------------------------------------


def gate_settings_json(
    *,
    mode: str = "shadow",
    root: Path = DEFAULT_GATE_ROOT,
    fmt: str = "claude",
    hook_timeout_s: int = 30,
    python: str | None = None,
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    captures_root: Path | None = None,
    session: str | None = None,
    ask: bool = False,
    ask_timeout_s: float = 90.0,
) -> str:
    """Return the Claude Code hooks-settings JSON that wires in the gate.

    Usable inline (``claude --settings "$(daisugi gate settings ...)"``) or
    merged into a settings file. The matcher is ``*`` — total by design;
    tool classification happens *inside* the gate so an unmatched tool can
    never be silently allowed. The command uses ``python -m opendaisugi.gate_client``
    (stdlib only, no typer import) — it asks the resident gate server first
    (``daisugi gate serve``, milliseconds) and falls back to the in-process
    ``opendaisugi.gate`` (argparse only, ADR-0007's latency-critical path,
    ~0.7 s) when no server is running or its reply can't be trusted. The
    one-flag flip to protection is ``mode="enforce"``.

    The host-side ``timeout`` is a backstop only: on every known host an
    outer hook timeout fails OPEN, which is why the gate owns an inner
    ``verify_timeout_s`` that denies first.

    ``ask=True`` bakes ``--ask --ask-timeout {ask_timeout_s}`` into the
    command (off by default — Task 8) and widens the host-side ``timeout``
    to ``max(hook_timeout_s, ask_timeout_s + verify_timeout_s + 5)`` so the
    host's own outer timeout — which fails OPEN — can never fire while the
    gate is legitimately waiting on a present operator. This budgets exactly
    one pass through the gate; the resident-server client's own round-trip
    timeout is a separate, narrower window (``gate_client.py``) and is not
    widened here.
    """
    import shlex
    import sys as _sys

    py = python or _sys.executable
    inner = min(verify_timeout_s, max(1.0, hook_timeout_s - 5.0))
    command = (
        f"{shlex.quote(py)} -m opendaisugi.gate_client"
        f" --mode {shlex.quote(mode)}"
        f" --root {shlex.quote(str(root))}"
        f" --format {shlex.quote(fmt)}"
        f" --verify-timeout {inner}"
    )
    if captures_root is not None:
        command += f" --captures-root {shlex.quote(str(captures_root))}"
    # Pin the envelope from OUTSIDE the agent: with --session set, a payload
    # claiming another session's id cannot select that session's (possibly
    # more permissive) envelope. The agent cannot rewrite this command.
    if session is not None:
        command += f" --session {shlex.quote(session)}"
    effective_hook_timeout_s = hook_timeout_s
    if ask:
        command += f" --ask --ask-timeout {int(ask_timeout_s)}"
        effective_hook_timeout_s = max(hook_timeout_s, int(ask_timeout_s + verify_timeout_s + 5))
    # Default-deny at the PROCESS boundary. The host runs `command` through a
    # shell (verified live), and on Claude Code any hook exit that is not 2 is
    # non-blocking — so a crashed gate (exit 1), or a package that fails to
    # import (where main() never runs to trap anything), would silently ALLOW.
    # `... || exit 2` maps every nonzero exit — including import failure, since
    # the whole invocation is the left operand — to a deny, while leaving exit 0
    # an allow and exit 2 a deny. `python -m …` is an external command, so its
    # nonzero exit triggers `||` (unlike a shell `exit` builtin). Live-verified:
    # without this, an exit-1 hook lets the read through; with it, it blocks.
    # ENFORCE only, and only for the claude contract (exit-code deny). In
    # SHADOW mode a crash must stay non-blocking — shadow never breaks the
    # host, so mapping a gate error to exit 2 (deny) would be the exact
    # regression shadow exists to avoid. hermes/openclaw deny via stdout JSON,
    # where this idiom is meaningless anyway (their crash behavior is part of
    # the documented 'unverified' enforcement class).
    if fmt == "claude" and mode == "enforce":
        command = f"{command} || exit 2"
    return json.dumps(
        {
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "*",
                        "hooks": [
                            {
                                "type": "command",
                                "command": command,
                                "timeout": effective_hook_timeout_s,
                            }
                        ],
                    }
                ],
            },
        }
    )


def _build_parser() -> "argparse.ArgumentParser":
    import argparse

    parser = argparse.ArgumentParser(prog="opendaisugi.gate", add_help=True)
    # default=None so we can tell "flag omitted" from an explicit choice, and
    # fall back to config.gate_mode only in the omitted case (resolve_gate_mode).
    parser.add_argument("--mode", choices=("shadow", "enforce"), default=None)
    parser.add_argument("--root", type=Path, default=DEFAULT_GATE_ROOT)
    parser.add_argument("--format", dest="fmt", default="claude")
    parser.add_argument("--verify-timeout", type=float, default=_DEFAULT_VERIFY_TIMEOUT_S)
    parser.add_argument("--captures-root", type=Path, default=None)
    parser.add_argument(
        "--session",
        default=None,
        help="Pin the envelope to this registered session, ignoring the "
        "session id in the payload (authorization must not key on "
        "caller-influenceable input).",
    )
    parser.add_argument(
        "--ask",
        action="store_true",
        help="Enforce mode only: hand a would-deny to a present operator for "
        "up to --ask-timeout seconds before letting the deny stand. Off by "
        "default.",
    )
    parser.add_argument("--ask-timeout", type=float, default=90.0, dest="ask_timeout")
    parser.add_argument(
        "--checkpoints",
        action="store_true",
        help="Snapshot the workspace into a private git ref at most once per "
        "new prompt boundary. Off by default; best-effort and never affects "
        "the verdict.",
    )
    return parser


def _fmt_from_argv(argv: list[str]) -> str:
    """Best-effort --format recovery for the escape path.

    Full argparse parsing has already failed by the time this runs, so
    ``args.fmt`` was never bound. Defaults to "claude" when the flag is
    absent or unreadable.
    """
    for i, a in enumerate(argv):
        if a == "--format" and i + 1 < len(argv):
            return argv[i + 1]
        if a.startswith("--format="):
            return a.split("=", 1)[1]
    return "claude"


def _escape_outcome(mode: str, exc: BaseException, fmt: str = "claude") -> "GateOutcome":
    """Build the fail-closed GateOutcome for an escape from run_argv's try.

    enforce denies; shadow has nothing to protect, so it allows, the same
    posture main()'s own try/except already used.

    ``fmt`` shapes the shadow-mode stdout body, and now the enforce-mode
    body too, for a host whose deny contract is JSON on stdout rather than
    the exit code (:data:`STDOUT_BLOCK_FORMATS`: hermes, openclaw). Before
    this, enforce's body was always empty at exit 2 regardless of ``fmt``.
    Exit 2 carries no signal to those hosts, which read an empty stdout as
    allow: the exact fail-open an argv error (an unrecognized flag, say)
    must not cause. claude/pi/opencode (they read the exit code, never
    stdout) and any format this gate does not know keep the plain exit-2
    deny, empty stdout, unchanged.
    """
    t0 = time.monotonic()
    reason = f"gate escape: {exc}"
    if mode == "enforce":
        decision = _deny("enforce", reason, t0=t0)
        if fmt in STDOUT_BLOCK_FORMATS:
            return _outcome(decision, fmt)
        return GateOutcome(
            stdout="",
            stderr=f"openDaisugi gate: DENIED (fail-closed on error): {exc}",
            exit_code=2,
            decision=decision,
        )
    return GateOutcome(
        stdout=stdout_for_format(fmt, block=False),
        stderr="",
        exit_code=0,
        decision=_deny("shadow", reason, t0=t0),
    )


def run_argv(argv: list[str], raw: bytes) -> GateOutcome:
    """The whole gate for one call, as an outcome: argv + stdin bytes in, verdict out.

    Fail-closed wrapper: ANY escape (a BaseException out of the verify thread,
    a bad argv) denies in enforce mode. Shared by the process entry (main),
    the resident server, and the client's fallback so the three cannot drift.

    ``argparse`` raises ``SystemExit`` both for ``--help`` (code 0 — a normal,
    intentional exit that must propagate untouched) and for a malformed argv
    (code 2 — an escape, since the mode couldn't even be resolved; that case
    denies in the fail-closed default posture below).

    Parsing is its own ``try``, separate from everything after it. Once
    ``parse_args`` returns, ``args.fmt`` is a real, fully-parsed value (the
    flag always has a default), strictly more precise than
    :func:`_fmt_from_argv`'s raw-text guess. So an escape AFTER a
    successful parse (anything ``gate_and_contract`` itself raises) is
    denied in the format the caller actually asked for, while an escape
    from parsing itself falls back to the guess, same as before.
    """
    mode = "enforce"  # until argv proves otherwise, an escape must deny
    try:
        args = _build_parser().parse_args(argv)
    except SystemExit as exc:
        if exc.code == 0:  # --help, --version, etc.: a real, intentional exit
            raise
        return _escape_outcome(mode, exc, fmt=_fmt_from_argv(argv))
    try:
        mode = resolve_gate_mode(args.mode, root=args.root)
        return gate_and_contract(
            raw,
            root=args.root,
            fmt=args.fmt,
            mode=mode,
            verify_timeout_s=args.verify_timeout,
            captures_root=args.captures_root,
            pin_session=args.session,
            ask=args.ask,
            ask_timeout_s=args.ask_timeout,
            checkpoints=args.checkpoints,
        )
    except SystemExit as exc:
        if exc.code == 0:  # --help, --version, etc.: a real, intentional exit
            raise
        return _escape_outcome(mode, exc, fmt=args.fmt)
    except BaseException as exc:  # noqa: BLE001 — deny-by-default on any escape
        return _escape_outcome(mode, exc, fmt=args.fmt)


def main(argv: list[str] | None = None) -> int:
    """Lean hook entry: ``python -m opendaisugi.gate`` (no typer import).

    Reads one hook payload from stdin, emits the host contract, and returns
    the process exit code (2 = deny on the Claude Code path). Kept argparse-
    only because hook round-trip latency is import-dominated; the full
    ``daisugi gate check`` command delegates here. The verdict itself comes
    from :func:`run_argv`, shared with the resident server and its client.
    """
    import sys as _sys

    try:
        raw = _sys.stdin.buffer.read()
    except Exception:  # noqa: BLE001 — a broken stdin still gets a verdict
        raw = b""
    out = run_argv(list(_sys.argv[1:] if argv is None else argv), raw)
    try:
        if out.stdout:
            print(out.stdout)
        if out.stderr:
            print(out.stderr, file=_sys.stderr)
    except Exception:  # noqa: BLE001 — a broken stdout must not un-deny
        pass
    return out.exit_code


if __name__ == "__main__":  # pragma: no cover — exercised via main() tests
    raise SystemExit(main())
