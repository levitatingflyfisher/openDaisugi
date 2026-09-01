"""The multi-session view's model: what is on screen, computed from the stores.

No Textual here. Rows are grouped by what the operator must do (NEEDS YOU,
WORKING, PARKED, DONE), never by id. Numbers say where they come from.
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from opendaisugi import ask as _ask
from opendaisugi.claude_transcript import last_model, read_turns, usage_totals
from opendaisugi.session_tree import SessionIndex, SessionTree

GROUPS = ("NEEDS YOU", "WORKING", "PARKED", "DONE")
WORKING_S = 60.0
PARKED_S = 3600.0
_DEFAULT_ALERTS = {
    "deny": "log",
    "deny_destructive": "pause",
    "ask": "pause",
    "budget": "pause",
    "cache_miss": "log",
}
# Word-boundary, case-folded matching (not bare substrings): "rm " previously matched
# inside "confirm "/"perform "/"transform ", and a bare substring check missed
# "DROP TABLE". Matched against `row.action.casefold()`, so these patterns are
# themselves lowercase. "> /dev/" excludes the harmless "> /dev/null".
#
# This list is defence-in-depth and honesty for the alerts strip; it can never be
# exhaustive over "every destructive command". The real guard is at the point of
# use (a raw-shell tool is treated as high-blast regardless of what it matches —
# see SHELL_TOOL_NAMES and the screen's arm-allow). Over-matching here is safe
# (it only strengthens a confirmation); a false negative is the danger, so the set
# leans inclusive.
_DESTRUCTIVE_RE = tuple(
    re.compile(p)
    for p in (
        r"\brm\b",
        r"\bgit\s+reset\s+--hard\b",
        r"\bgit\s+push\s+--force\b",
        r"\bgit\s+clean\b",  # -fdx deletes untracked files
        r"\bdrop\s+table\b",
        r">\s*/dev/(?!null\b)",  # redirect over a device (not /dev/null)
        r"\bdd\b[^\n]*\bof=",  # dd of=/dev/sda — raw disk write
        r"\bmkfs\b",  # mkfs / mkfs.ext4 — format a filesystem
        r"\bchmod\s+-\w*r",  # chmod -R … — recursive permission change
        r"\bchown\s+-\w*r",  # chown -R … — recursive owner change
        r"\bfind\b[^\n]*\s-delete\b",  # find / -delete
        r"\b(?:shutdown|reboot|halt|poweroff)\b",  # power-state changes
        # a TRUNCATING redirect (single >, not >>) over a sensitive file/dir
        r"(?<!>)>\s*(?:~|[^\s>]*\.ssh/|/etc/|/boot/|/root/|[^\s>]*authorized_keys)",
    )
)

# Tool names that mean "run an arbitrary shell command" across the harnesses the
# gate recognizes (claude `Bash`; codex `shell`/`exec_command`/`container.exec`/
# `local_shell_call`, which it normalizes to `Bash`; a generic `run_shell`). A raw
# shell call is inherently high-blast — its blast radius is "whatever the string
# does" — so the screen requires the strong (typed-token) confirm for ALL of these,
# not just the ones a pattern above happens to catch. Compared case-folded.
SHELL_TOOL_NAMES = frozenset(
    {"bash", "shell", "run_shell", "exec_command", "container.exec", "local_shell_call"}
)

# The alert-policy file (<data_dir>/alerts.yaml) maps an alert class to log/pause/modal.
AlertPolicy = dict[str, str]


@dataclass(frozen=True)
class SessionRow:
    session_id: str
    group: str
    agent: str
    action: str
    verdict: str
    clause: str
    steps: int
    fresh: int
    cache_read: int
    cache_write: int
    age_s: float
    pending_ask: dict[str, Any] | None
    harness: str
    cwd: str
    transcript_path: str | None
    last_verdict: dict[str, Any] | None = None
    last_tool_call: dict[str, Any] | None = None
    # The harness's OWN session id (e.g. the real Claude uuid), when it differs
    # from ``session_id`` — under ``daisugi start`` the tree is keyed by a per-cwd
    # PIN, so the real uuid lives here and the attach command must use it, not the
    # pin key (which is not a valid ``claude --resume`` argument).
    harness_session_id: str | None = None


@dataclass(frozen=True)
class Roster:
    rows: list[SessionRow]
    counts: dict[str, int]


@dataclass(frozen=True)
class HeaderState:
    gate_mode: str  # ENFORCE | SHADOW | DISARMED
    gate_mode_source: str  # hook | config
    cache_hit_rate: float | None
    cache_source: str  # transcripts | gateway | none
    cache_is_estimate: bool  # True when cache_hit_rate is a gateway-blended estimate,
    # not a live measurement from this run's own transcripts (S5 honesty correction)
    session_count: int


@dataclass(frozen=True)
class Alert:
    klass: str
    count: int
    policy: str
    sessions: list[str] = field(default_factory=list)


def _tree_usage(entries) -> dict[str, int]:
    total = {"fresh": 0, "cacheRead": 0, "cacheWrite": 0, "out": 0}
    for e in entries:
        if e.type == "assistant":
            u = e.data.get("usage") or {}
            for k in total:
                total[k] += int(u.get(k) or 0)
    return total


def _tree_model(entries) -> str | None:
    for e in reversed(entries):
        if e.type == "assistant" and e.data.get("model"):
            return str(e.data["model"])
    return None


def build_roster(data_dir: Path, *, now: float | None = None) -> Roster:
    now = time.time() if now is None else now
    root = data_dir / "gate"
    asks_by_session: dict[str, dict[str, Any]] = {}
    for a in _ask.pending_asks(root, now=now):
        sid = str(a.get("sessionId") or "")
        if sid:  # an ask with no sessionId is anonymous — never attach it to any row
            asks_by_session.setdefault(sid, a)
    rows: list[SessionRow] = []
    for s in SessionIndex(data_dir / "sessions").list():
        entries = SessionTree.open(data_dir / "sessions", s.session_id).entries()
        # A session with no harness_session_id (e.g. every sprig session) must not fall
        # back to an empty-string bucket — that bucket no longer exists, but the lookup
        # order still matters: only match a real harness_session_id, else the session's
        # own id. Never cross-attach one ask to every id-less session.
        pending = None
        if s.harness_session_id:
            pending = asks_by_session.get(s.harness_session_id)
        if pending is None:
            pending = asks_by_session.get(s.session_id)
        age = max(0.0, now - s.last_ts)
        if s.transcript_path and Path(s.transcript_path).exists():
            turns = read_turns(Path(s.transcript_path))
            usage, agent = usage_totals(turns), (last_model(turns) or s.harness)
        else:
            usage, agent = _tree_usage(entries), (_tree_model(entries) or s.harness)
        call = s.last_tool_call or {}
        verdict = s.last_verdict or {}
        group = (
            "NEEDS YOU"
            if pending
            else "WORKING"
            if age < WORKING_S
            else "PARKED"
            if age < PARKED_S
            else "DONE"
        )
        rows.append(
            SessionRow(
                session_id=s.session_id,
                group=group,
                agent=agent,
                action=" ".join(x for x in (call.get("name"), call.get("detail")) if x)[:60],
                verdict=str(verdict.get("decision", "")).upper(),
                clause=str(verdict.get("clause", "")),
                steps=sum(1 for e in entries if e.type == "tool_call"),
                fresh=usage["fresh"],
                cache_read=usage["cacheRead"],
                cache_write=usage["cacheWrite"],
                age_s=age,
                pending_ask=pending,
                harness=s.harness,
                cwd=s.cwd,
                transcript_path=s.transcript_path,
                last_verdict=verdict or None,
                last_tool_call=call or None,
                harness_session_id=s.harness_session_id,
            )
        )
    order = {g: i for i, g in enumerate(GROUPS)}
    rows.sort(key=lambda r: (order[r.group], r.age_s))
    counts = {g: sum(1 for r in rows if r.group == g) for g in GROUPS}
    return Roster(rows=rows, counts=counts)


def header_state(
    data_dir: Path,
    *,
    home: Path | None = None,
    cwd: Path | None = None,
    roster: Roster | None = None,
) -> HeaderState:
    from opendaisugi.config import effective_hook_mode, hook_source_label
    from opendaisugi.gate import is_disarmed, resolve_gate_mode

    home = home or Path.home()
    # The TUI's own launch directory (wherever the operator ran `daisugi
    # dashboard --tui` from) — the header reflects THAT context plus any
    # machine-global hook, so it never shows a safer mode than an active
    # `daisugi start --enforce` hook over the session the operator is
    # actually watching. Path.cwd() raises if the launch directory was
    # deleted out from under a still-running process; the header must never
    # crash a screen over that, so fall back to reporting the global-only
    # view (== home, so the cwd-hook check below is a no-op) rather than
    # propagate.
    if cwd is None:
        try:
            cwd = Path.cwd()
        except OSError:
            cwd = home
    root = data_dir / "gate"
    eff = effective_hook_mode(home=home, cwd=cwd)
    if eff.mode:
        mode, source = eff.mode, hook_source_label(eff)
    else:
        mode = resolve_gate_mode(None, root=root)
        source = "config"
    if is_disarmed(root):
        mode, source = "disarmed", "marker"  # the DISARMED file overrides hook/config; say so
    roster = roster if roster is not None else build_roster(data_dir)
    live = [r for r in roster.rows if r.group in ("NEEDS YOU", "WORKING")]
    total = sum(r.fresh + r.cache_read + r.cache_write for r in live)
    is_estimate = False
    if total > 0:
        rate, cache_source = sum(r.cache_read for r in live) / total, "transcripts"
    else:
        rate, cache_source = None, "none"
        try:
            from opendaisugi.gateway_journal import GatewayJournal

            summ = GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").summary()
            if summ.turns:
                rate, cache_source, is_estimate = summ.cache_hit_rate, "gateway", True
        except Exception:  # noqa: BLE001 — the header must never fail on a store
            pass
    return HeaderState(
        gate_mode=mode.upper(),
        gate_mode_source=source,
        cache_hit_rate=rate,
        cache_source=cache_source,
        cache_is_estimate=is_estimate,
        session_count=len(roster.rows),
    )


def load_alert_policy(data_dir: Path) -> AlertPolicy:
    pol = dict(_DEFAULT_ALERTS)
    p = data_dir / "alerts.yaml"
    if p.exists():
        try:
            raw = yaml.safe_load(p.read_text(encoding="utf-8")) or {}
            if isinstance(raw, dict):  # a scalar/list YAML document has no .items(); fall back
                pol.update(
                    {str(k): str(v) for k, v in raw.items() if str(v) in ("log", "pause", "modal")}
                )
        except (yaml.YAMLError, OSError):  # unparsable or unreadable (e.g. a directory) — defaults
            pass
    return pol


def is_destructive_action(action: str) -> bool:
    """True when ``action`` matches a destructive class (rm, hard reset, force
    push, DROP TABLE, redirect over a device). Word-boundary, case-folded.

    Public so a screen can classify the guard it shows for an allow. S2: the
    caller builds ``action`` from the PENDING ask (``toolName`` + the values of
    ``toolInput``), not from ``row.action`` — the latter is the last *recorded*
    tool call, which can lag the ask still awaiting an answer, so classifying it
    would hand a destructive pending call the low-effort two-step by mistake.
    """
    low = action.casefold()
    return any(p.search(low) for p in _DESTRUCTIVE_RE)


# Back-compat name kept for internal callers; the public spelling is above.
_is_destructive = is_destructive_action


def alerts_for(roster: Roster, *, policy: AlertPolicy | None = None) -> list[Alert]:
    # A merge, not an `or`: an explicit but partial policy (e.g. {"deny": "modal"}) must
    # not KeyError on the classes it left unset, and an explicit {} must not be silently
    # replaced wholesale by `{} or defaults` (falsy-dict trap).
    policy = {**_DEFAULT_ALERTS, **(policy or {})}
    denies = [r for r in roster.rows if r.verdict == "DENY"]
    destructive = [r for r in denies if _is_destructive(r.action)]
    asks = [r for r in roster.rows if r.pending_ask]
    out = []
    if denies:
        out.append(Alert("deny", len(denies), policy["deny"], [r.session_id for r in denies]))
    if destructive:
        out.append(
            Alert(
                "deny_destructive",
                len(destructive),
                policy["deny_destructive"],
                [r.session_id for r in destructive],
            )
        )
    if asks:
        out.append(Alert("ask", len(asks), policy["ask"], [r.session_id for r in asks]))
    return out
