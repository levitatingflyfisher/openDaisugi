"""PaneStateEvent (master spec §3.1) — one JSON object per line.

Owns the canonical §3.1 rules: state/source enums, a manifest source may
never claim 'done', the ask sub-object is only valid when
state == 'blocked', and detail is capped at 200 characters.
opendaisugi._state_report has its OWN, independently-implemented, narrower
validator for the one path that cannot import this package
(`daisugi hook report`, reachable through gate.sock — gate_server.py is a
layer module and may not import opendaisugi.floor, which owns this
contract). tests/floor/test_events.py::test_layer_validator_and_dataclass_agree
pins the two to the same verdicts; this module does not import the layer's
copy, on purpose — floor owns this contract, not the layer.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, replace

STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")  # precedence, highest first

# The coppice-server wire's closed error-code enum. Master spec section 3.3
# names it; the full list lives in spec-02. server_closed is what
# pane.create answers once coppice-server's Close has started shutting the
# process down. It is distinct from pane_closed, which always names one
# particular pane. Keep this set level with the validCodes map in
# harness/coppice/internal/proto/proto.go;
# test_error_codes_match_gos_validcodes_enum reads that file and fails if
# the two drift apart.
ERROR_CODES = (
    "bad_request",
    "no_such_pane",
    "no_such_workspace",
    "no_such_tab",
    "pane_closed",
    "server_closed",
    "not_attached",
    "adapter_error",
    "spawn_failed",
    "timeout",
    "unauthorized",
    "internal",
)

_DETAIL_MAX = 200
_PRECEDENCE = {name: i for i, name in enumerate(reversed(SOURCES))}


TIERS = ("undoable", "permanent")


def normal_tier(tier: object) -> str:
    """``undoable`` for exactly that string, ``permanent`` for anything else.
    Every reader treats a missing or unknown tier as permanent."""
    return "undoable" if tier == "undoable" else "permanent"


@dataclass(frozen=True)
class Ask:
    id: str
    tool: str
    summary: str
    deadline: float
    tier: str = "permanent"


@dataclass(frozen=True)
class PaneStateEvent:
    session_id: str
    harness: str
    state: str
    source: str
    ts: float
    harness_session_id: str | None = None
    pane: str | None = None
    ask: Ask | None = None
    detail: str = ""
    # who names the person behind an operator report, set by the server
    # from the connection's name. It is None on every other event.
    who: str | None = None
    v: int = 1

    def __post_init__(self) -> None:
        if self.state not in STATES:
            raise ValueError(f"unknown state {self.state!r}")
        if self.source not in SOURCES:
            raise ValueError(f"unknown source {self.source!r}")
        if self.state == "done" and self.source not in ("process", "headless"):
            raise ValueError("state 'done' may only come from source 'process' or 'headless'")
        if len(self.detail) > _DETAIL_MAX:
            raise ValueError(f"detail exceeds {_DETAIL_MAX} characters")
        if self.ask is not None and self.state != "blocked":
            raise ValueError("ask is only valid when state == 'blocked'")
        if self.source == "gate" and self.state == "blocked" and self.ask is None:
            raise ValueError("a gate blocked event needs an ask")
        if self.who is not None and self.source != "operator":
            raise ValueError("who is only valid when source == 'operator'")
        if self.v != 1:
            raise ValueError(f"unsupported event schema version {self.v!r}")

    def to_json(self) -> str:
        row: dict[str, object] = {
            "v": self.v,
            "ts": self.ts,
            "session_id": self.session_id,
            "harness_session_id": self.harness_session_id,
            "harness": self.harness,
            "pane": self.pane,
            "state": self.state,
            "source": self.source,
            "detail": self.detail,
        }
        if self.ask is not None:
            row["ask"] = {
                "id": self.ask.id,
                "tool": self.ask.tool,
                "summary": self.ask.summary,
                "deadline": self.ask.deadline,
                "tier": normal_tier(self.ask.tier),
            }
        if self.who is not None:
            row["who"] = self.who
        return json.dumps(row)

    @classmethod
    def from_json(cls, line: str) -> "PaneStateEvent":
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"not valid JSON: {exc}") from exc
        if not isinstance(row, dict):
            raise ValueError("event must be a JSON object")
        required = ("session_id", "harness", "state", "source", "ts")
        missing = [k for k in required if k not in row]
        if missing:
            raise ValueError(f"missing required field(s): {', '.join(missing)}")
        for key in ("session_id", "harness", "state", "source"):
            if not isinstance(row[key], str):
                raise ValueError(f"{key} must be a string")
        ts_raw = row["ts"]
        if isinstance(ts_raw, bool) or not isinstance(ts_raw, (int, float)):
            raise ValueError("ts must be a number")
        detail_raw = row.get("detail", "")
        if not isinstance(detail_raw, str):
            raise ValueError("detail must be a string")
        v_raw = row.get("v", 1)
        if isinstance(v_raw, bool) or not isinstance(v_raw, int):
            raise ValueError(f"invalid schema version {v_raw!r}")
        # harness_session_id and pane are optional. Master spec 3.1 says
        # "when known, else null", so absent or JSON null both mean "not
        # reported". A PRESENT non-string value is still rejected, the same
        # as the required string fields above. Go's ParseStateEvent applies
        # the same rule to these two fields through its own
        # optionalStringFromRow helper.
        harness_session_id_raw = row.get("harness_session_id")
        if harness_session_id_raw is not None and not isinstance(harness_session_id_raw, str):
            raise ValueError("harness_session_id must be a string")
        pane_raw = row.get("pane")
        if pane_raw is not None and not isinstance(pane_raw, str):
            raise ValueError("pane must be a string")
        who_raw = row.get("who")
        if who_raw is not None and not isinstance(who_raw, str):
            raise ValueError("who must be a string")
        ask_row = row.get("ask")
        ask_obj: Ask | None = None
        if ask_row is not None:
            if not isinstance(ask_row, dict):
                raise ValueError("ask must be a JSON object")
            ask_missing = [k for k in ("id", "tool", "summary", "deadline") if k not in ask_row]
            if ask_missing:
                raise ValueError(f"ask missing field(s): {', '.join(ask_missing)}")
            # Type-checked, not coerced (Important 1): a wrong-typed ask field
            # must raise ValueError here, the same as the layer validator
            # (_state_report._validate_hook_report_row) — no str()/float()
            # papering over a malformed ask.id or a non-numeric deadline.
            for key in ("id", "tool", "summary"):
                if not isinstance(ask_row[key], str):
                    raise ValueError(f"ask {key} must be a string")
            ask_deadline = ask_row["deadline"]
            if isinstance(ask_deadline, bool) or not isinstance(ask_deadline, (int, float)):
                raise ValueError("ask deadline must be a number")
            # tier is optional. Absent or null reads as permanent. A present
            # value that is not a string is refused.
            ask_tier = ask_row.get("tier")
            if ask_tier is not None and not isinstance(ask_tier, str):
                raise ValueError("ask tier must be a string")
            ask_obj = Ask(
                id=ask_row["id"],
                tool=ask_row["tool"],
                summary=ask_row["summary"],
                # Go's JSON encoder drops the decimal point on a whole
                # number float, so a deadline of 100.0 arrives as the JSON
                # integer 100. Coerced to float here so every reader sees
                # one type, whatever second the deadline lands on.
                deadline=float(ask_deadline),
                tier=normal_tier(ask_tier),
            )
        try:
            return cls(
                session_id=row["session_id"],
                harness=row["harness"],
                state=row["state"],
                source=row["source"],
                ts=float(ts_raw),
                harness_session_id=harness_session_id_raw,
                pane=pane_raw,
                ask=ask_obj,
                detail=detail_raw,
                who=who_raw,
                v=v_raw,
            )
        except (TypeError, ValueError) as exc:
            raise ValueError(f"malformed event: {exc}") from exc


def merge(
    current: PaneStateEvent | None, incoming: PaneStateEvent, *, now: float
) -> PaneStateEvent:
    """Resolve two ALREADY-VALID events by §3.1 source precedence.

    Checked right after the `current is None` case: once current's own
    state is already 'done', it absorbs everything after it. A dead
    process or an ended session never comes back. merge() returns current
    unchanged here, whatever incoming reports and however long after
    current it arrives. Go's twin lives in
    harness/coppice/internal/state/state.go and enforces the same guard as
    its own rule 2. Its test TestDoneIsTerminal names the case this guards
    against.

    A terminal 'done' whose source is 'process' or 'headless' also wins at
    once on the incoming side, ahead of the gate hold and ahead of the 2 s
    precedence window, checked right after the absorbing guard above.
    Nothing ever re-sends a 'done': the process has already exited, or the
    headless session has already ended, by the time this event exists. If
    a 'done' lost to a higher-precedence 'working'/'blocked' hold here,
    even briefly, even only within the 2 s window, the pane would show
    that stale state forever. No later event will ever arrive to correct
    it.

    Once that terminal check has passed, a gate 'blocked' event holds
    until a gate event explicitly clears it, or its ask's deadline passes.
    Expiry only RELEASES the hold here — it does NOT synthesize a
    'working' event. The gate always sends its own 'working' event the
    moment `_maybe_ask`'s wait resolves (gate.py, task 3), by answer or by
    timeout, which arrives at (or a hair after) the same deadline; this
    function just stops holding the door shut once that deadline has
    passed, so that event — or, failing that, whatever the next real event
    is — gets through instead of being silently discarded by precedence. A
    reader holding only a stale cached event, with no fresh `incoming` to
    merge against at all, calls :func:`effective_state` instead — the same
    expiry rule applied without a second event.

    The 2 s precedence window (whole-branch review, Important 2) holds
    back ONLY an incoming `manifest` event — `manifest` is the fallback
    source with no real signal of its own, so it alone may lose a race to
    a fresher higher-precedence event. Every other source (`headless`,
    `process`, `operator`, `gate`) is a FACT about what actually happened
    and always applies at once; holding one of those back — e.g. a real
    headless 'idle' arriving a heartbeat after a gate 'working' — would
    swallow a true state change and show a stale pane. The gate-blocked
    hold above is the only thing still allowed to delay a fact, and only
    until it yields to a gate event, the ask's deadline, or a terminal
    'done'.

    Divergence from Go, named here on purpose: Go's twin measures the 2 s
    window against the coppice server's own receive clock, so a hook with
    a skewed system clock cannot extend its own hold. Go's own
    TestTheHoldWindowIgnoresASkewedEventTimestamp pins that. This function
    measures the window against current.ts instead, the event's own
    self-reported timestamp. The Python floor has no separate receive
    clock at this layer to compare against. A resident gate process that
    later gains one, by timestamping arrivals itself, should pass that
    clock's reading as `now`, not current.ts, to close this gap. Until
    then this is a known, accepted difference between the two floors, not
    a bug. test_the_hold_window_uses_current_ts_not_a_receive_clock pins
    the divergent outcome on Go's own skewed-clock inputs: every other
    precedence test in this file sets current.ts to a value a receive
    clock would also hold, so only that one test can tell the two clocks
    apart.
    """
    if current is None:
        return incoming
    if current.state == "done":
        return current
    if incoming.state == "done" and incoming.source in ("process", "headless"):
        return incoming
    holding = current.source == "gate" and current.state == "blocked"
    if holding:
        deadline = current.ask.deadline if current.ask else None
        expired = deadline is not None and now >= deadline
        if incoming.source == "gate":
            return incoming
        if not expired:
            return current
        # expired: the hold has lapsed; fall through to ordinary precedence
    # A screen read of blocked outranks a process idle. A question that
    # waits for a key draws nothing more, and that quiet is not an answer.
    if (
        current.state == "blocked"
        and current.source == "manifest"
        and incoming.source == "process"
        and incoming.state == "idle"
    ):
        return current
    if (
        incoming.source == "manifest"
        and now - current.ts < 2.0
        and _PRECEDENCE[incoming.source] < _PRECEDENCE[current.source]
    ):
        return current
    return incoming


def effective_state(current: PaneStateEvent | None, *, now: float) -> PaneStateEvent | None:
    """Read-time counterpart to merge()'s expiry rule.

    merge() only releases a gate 'blocked' hold when a FRESH incoming
    event actually arrives. Two paths never send one at all:
    gate_and_contract's is_disarmed early return emits no report, and any
    crash of the gate process between post_ask and its own resolving
    'working' report leaves the last stored event 'blocked' forever. A
    reader that has only a cached event (no fresh one to merge against —
    a coppice-server reading the last known state, a cockpit roster view)
    calls this instead: returns ``current`` unchanged unless it is a
    ``blocked`` event whose ``ask.deadline`` has passed, in which case it
    returns a synthesized 'working' event (ask cleared, detail explains
    why) so a stale blocked hold never displays forever.

    Whole-branch review, Important 2: this expiry is no longer restricted
    to ``source == "gate"``. ANY ``blocked`` event carrying an ask — gate
    or otherwise — expires the same way once its deadline passes; a
    headless- or process-sourced block with a stale ask is just as stale
    as a gate one, and there is no reason a reader should keep showing it
    forever. A ``blocked`` event with no ask (``ask is None``) never
    expires here, matching merge()'s own hold.
    """
    if current is None:
        return current
    if current.state != "blocked":
        return current
    deadline = current.ask.deadline if current.ask else None
    if deadline is None or now < deadline:
        return current
    return replace(current, state="working", ask=None, detail="ask deadline passed")
