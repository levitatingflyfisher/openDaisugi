"""The router's weekly measure, for ``daisugi router status``.

Three sources, all local and append-only:

- the gateway's turn journal (``<data dir>/gateway/turns.jsonl``): each
  routed turn with its tokens, its billed cost and the frontier counterfactual;
- the delegation journal (``<data dir>/router/delegations.jsonl``): each
  delegate call, refused or not, with the worker, its tokens and cost, the
  time, the quotes kept and dropped, and the frontier tokens it kept off the
  context;
- the gate's audit logs (``<data dir>/gate/audit/*.jsonl``): each read a graft
  rule matched, redirected or not.

A week is an ISO week in UTC. Every counterfactual is an estimate and is
shown as one: the frontier tokens a delegation kept off the context, and the
dollars they would have cost. No escalation path is built, so no
turn escalates yet. A task's outcome is unknown until something labels it;
unknown is shown as unknown.

The promotion meter (GR-6, GR-7) reads three more: the graft arm each
session was put in (the gate's audit records), the operator's labels
(``<data dir>/router/labels.jsonl``, written by ``daisugi router label``)
and each session's Claude Code transcript, priced at the gateway's list
prices. It prints what promotion would do and changes nothing.
"""

from __future__ import annotations

import json
import math
import os
import re
import stat
from datetime import date, datetime
from pathlib import Path
from typing import Any

from opendaisugi import delegate

MAX_WEEKS = 8


# An integer field counts only when its size is at most 2**53: a port
# sums it in 64 bits, and a larger count is no real journal's.
_INT_LIMIT = 2**53


def _int(v: Any) -> int:
    if isinstance(v, int) and not isinstance(v, bool) and -_INT_LIMIT <= v <= _INT_LIMIT:
        return v
    return 0


def _num(v: Any) -> float | None:
    """A JSON number as a finite float, or None (a bool, a string, NaN, an
    infinity, or an integer too large for a float)."""
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return None
    try:
        f = float(v)
    except OverflowError:
        return None
    return f if math.isfinite(f) else None


def _float(v: Any) -> float:
    f = _num(v)
    return 0.0 if f is None else f


_ISO_STAMP = re.compile(r"([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})Z")
# date(1970, 1, 1).toordinal(), and the Unix times of 0001-01-01 and
# 9999-12-31T23:59:59, the range a date holds.
_EPOCH_ORDINAL = 719163
_FIRST_S = -62135596800
_LAST_S = 253402300799


def _iso_week(d: date) -> str:
    y, w, _ = d.isocalendar()
    return f"{y:04d}-W{w:02d}"


def week_of_iso(s: Any) -> str | None:
    """The ISO week of a ``YYYY-MM-DDTHH:MM:SSZ`` stamp (ASCII digits, a
    real date and time), or None."""
    if not isinstance(s, str):
        return None
    m = _ISO_STAMP.fullmatch(s)
    if m is None:
        return None
    try:
        dt = datetime(*(int(g) for g in m.groups()))
    except ValueError:
        return None
    return _iso_week(dt.date())


def week_of_epoch(t: Any) -> str | None:
    """The ISO week (UTC) of a Unix time: its day, from the whole seconds
    (the floor), or None outside the years 1 to 9999."""
    if not isinstance(t, (int, float)) or isinstance(t, bool):
        return None
    if isinstance(t, float) and not math.isfinite(t):
        return None
    sec = math.floor(t)
    if not _FIRST_S <= sec <= _LAST_S:
        return None
    return _iso_week(date.fromordinal(_EPOCH_ORDINAL + sec // 86400))


def _empty(week: str) -> dict[str, Any]:
    return {
        "week": week,
        "turns": 0,
        "turns_downgraded": 0,
        "turn_tokens_saved": 0,
        "turn_dollars_saved": 0.0,
        "turns_estimated": 0,
        "turn_elapsed_ms": 0.0,
        "escalations": 0,
        "redirects": 0,
        "not_redirected": 0,
        "delegations": 0,
        "delegate_ok": 0,
        "delegate_elapsed_ms": 0.0,
        "quotes": 0,
        "dropped": 0,
        "worker_tokens": 0,
        "worker_dollars": 0.0,
        "worker_unpriced": 0,
        "delegate_tokens_kept": 0,
        "delegate_dollars_kept": 0.0,
        "task_pass": 0,
        "task_fail": 0,
        "task_unknown": 0,
        "delegate_estimated": 0,
        "tokens_saved": 0,
        "dollars_saved": 0.0,
        "estimated": False,
    }


def read_turns(data_dir: Path) -> list[dict[str, Any]]:
    """The turn journal's rows that read as JSON objects."""
    return _read_jsonl(data_dir / "gateway" / "turns.jsonl")


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    try:
        raw = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return []
    out = []
    for line in raw.split("\n"):
        if not line.strip():
            continue
        try:
            obj = json.loads(line)
        except (ValueError, RecursionError):
            continue
        if isinstance(obj, dict):
            out.append(obj)
    return out


def read_graft_events(data_dir: Path) -> list[dict[str, Any]]:
    """Every audit record that carries a graft, from every audit log, by
    file name then line."""
    d = data_dir / "gate" / "audit"
    try:
        names = sorted(p.name for p in d.iterdir() if p.name.endswith(".jsonl"))
    except OSError:
        return []
    out = []
    for name in names:
        for rec in _read_jsonl(d / name):
            if isinstance(rec.get("graft"), dict):
                out.append(rec)
    return out


def weekly(data_dir: Path) -> list[dict[str, Any]]:
    """One row per ISO week with any record, newest first, at most
    ``MAX_WEEKS``."""
    weeks: dict[str, dict[str, Any]] = {}

    def row(week: str) -> dict[str, Any]:
        if week not in weeks:
            weeks[week] = _empty(week)
        return weeks[week]

    for t in read_turns(data_dir):
        week = week_of_iso(t.get("created_at"))
        if week is None:
            continue
        r = row(week)
        r["turns"] += 1
        if t.get("downgraded") is True:
            r["turns_downgraded"] += 1
        r["turn_tokens_saved"] += _int(t.get("frontier_tokens_saved"))
        r["turn_dollars_saved"] += _float(t.get("counterfactual_dollars")) - _float(
            t.get("actual_dollars")
        )
        if t.get("estimated") is True:
            r["turns_estimated"] += 1
        r["turn_elapsed_ms"] += _float(t.get("elapsed_ms"))
    for e in read_graft_events(data_dir):
        week = week_of_epoch(e.get("at"))
        if week is None:
            continue
        r = row(week)
        if e["graft"].get("applied") is True:
            r["redirects"] += 1
        else:
            r["not_redirected"] += 1
    for d in delegate.load_records(data_dir):
        week = week_of_iso(d.get("at"))
        if week is None:
            continue
        r = row(week)
        r["delegations"] += 1
        if d.get("ok") is True:
            r["delegate_ok"] += 1
            r["delegate_elapsed_ms"] += _float(d.get("elapsed_ms"))
        r["quotes"] += _int(d.get("quotes"))
        r["dropped"] += _int(d.get("dropped"))
        r["worker_tokens"] += _int(d.get("worker_input_tokens")) + _int(
            d.get("worker_output_tokens")
        )
        if d.get("ok") is True and d.get("estimated") is True:
            r["delegate_estimated"] += 1
        if d.get("ok") is True:
            wd = _num(d.get("worker_dollars"))
            if wd is None:
                r["worker_unpriced"] += 1
            else:
                r["worker_dollars"] += wd
        r["delegate_tokens_kept"] += _int(d.get("frontier_tokens_kept"))
        r["delegate_dollars_kept"] += _float(d.get("frontier_dollars_kept"))
        ok = d.get("task_ok")
        if ok is True:
            r["task_pass"] += 1
        elif ok is False:
            r["task_fail"] += 1
        else:
            r["task_unknown"] += 1
    out = []
    for week in sorted(weeks, reverse=True)[:MAX_WEEKS]:
        r = weeks[week]
        r["tokens_saved"] = r["turn_tokens_saved"] + r["delegate_tokens_kept"]
        r["dollars_saved"] = (
            r["turn_dollars_saved"] + r["delegate_dollars_kept"] - r["worker_dollars"]
        )
        r["estimated"] = r["turns_estimated"] > 0 or r["delegate_estimated"] > 0
        out.append(r)
    return out


def week_line(r: dict[str, Any]) -> str:
    """One week as ``router status`` prints it."""
    est = " (estimated)" if r["estimated"] else ""
    refused = r["delegations"] - r["delegate_ok"]
    unpriced = f", {r['worker_unpriced']} unpriced" if r["worker_unpriced"] else ""
    each = (
        f", {r['delegate_elapsed_ms'] / 1000 / r['delegate_ok']:.1f}s each"
        if r["delegate_ok"]
        else ""
    )
    return (
        f"{r['week']}: tokens saved {r['tokens_saved']:,}{est}, "
        f"billed saved ${r['dollars_saved']:.4f}{est}; "
        f"turns {r['turns']} ({r['turns_downgraded']} routed cheaper), "
        f"escalations {r['escalations']}; "
        f"delegations {r['delegations']} ({r['delegate_ok']} ok{each}, {refused} refused), "
        f"quotes {r['quotes']} kept, {r['dropped']} dropped, "
        f"worker {r['worker_tokens']:,} tokens ${r['worker_dollars']:.4f}{unpriced}; "
        f"reads redirected {r['redirects']}, not redirected {r['not_redirected']}; "
        f"tasks {r['task_pass']} passed, {r['task_fail']} failed, {r['task_unknown']} unknown"
    )


def delegate_state(data_dir: Path, env: Any = None) -> dict[str, Any]:
    """The rule in force and the worker the router would pick now."""
    root = data_dir / "gate"
    rules, bad = delegate.load_rules(root)
    rule = next((r for r in rules if r.state in delegate.RULE_STATES_ACTING), None)
    envelope = None
    envelope_error = None
    try:
        from opendaisugi.gate import load_envelope

        envelope = load_envelope(None, root=root)
    except Exception:  # noqa: BLE001 - shown, never raised
        envelope_error = "the gate's default envelope cannot be read"
    if envelope_error is not None:
        route = delegate.Route(False, f"no worker: {envelope_error}")
    else:
        route = delegate.route_delegate(
            data_dir, envelope, allow_remote=rule.allow_remote if rule else False, env=env
        )
    return {
        "rule": None
        if rule is None
        else {
            "file": rule.file,
            "id": rule.id,
            "version": rule.version,
            "state": rule.state,
            "file_lines_over": rule.min_lines,
            "allow_remote": rule.allow_remote,
        },
        "unused_rules": [
            {
                "file": r.file,
                "id": r.id,
                "state": r.state,
                "why": f"its state is {r.state}"
                if r.state not in delegate.RULE_STATES_ACTING
                else "an earlier rule file is in force",
            }
            for r in rules
            if r is not rule
        ],
        "bad_rules": [{"file": f, "why": why} for f, why in bad],
        "worker": route.as_dict() if route.ok else None,
        "worker_reason": route.reason,
    }


def delegate_lines(state: dict[str, Any]) -> list[str]:
    """The delegate section as ``router status`` prints it."""
    out = ["delegate (large reads):"]
    rule = state["rule"]
    if rule is None:
        out.append("  rule: none. Reads are not redirected (a rule file in <gate root>/grafts).")
    else:
        verb = {
            "active": "go to",
            "trial": "go to, in the trial's graft arm only,",
        }.get(rule["state"], "would go to (audit: not denied)")
        out.append(
            f"  rule: {rule['id']} v{rule['version']} ({rule['file']}), {rule['state']}: "
            f"reads over {rule['file_lines_over']} lines {verb} the delegate tool"
        )
    for r in state["unused_rules"]:
        out.append(f"  rule {r['id']} ({r['file']}) is not in force: {r['why']}")
    for b in state["bad_rules"]:
        out.append(f"  rule file {b['file']} is not used: {b['why']}")
    w = state["worker"]
    if w is not None:
        out.append(f"  worker: {w['model']} ({w['tier']}, {w['host']})")
    else:
        out.append(f"  worker: none. {state['worker_reason']}.")
        if rule is not None:
            out.append("  With no worker, reads over the threshold go through the normal gate.")
    return out


# ---------------------------------------------------------------------------
# The promotion meter (GR-6, GR-7)
# ---------------------------------------------------------------------------

#: Each arm needs this many labeled sessions before promotion can be judged.
MIN_LABELED = 3
#: The largest transcript read, in bytes.
MAX_TRANSCRIPT_BYTES = 64 * 1024 * 1024
#: The longest label note.
MAX_NOTE_CHARS = 500
OUTCOMES = ("pass", "fail")


def labels_path(data_dir: Path) -> Path:
    return data_dir / "router" / "labels.jsonl"


def session_ok(session: Any) -> bool:
    """True when ``session`` is a session id as the gate names its audit
    files: equal to its own safe form."""
    from opendaisugi.hook import _safe_session_id

    return isinstance(session, str) and _safe_session_id(session) == session


def write_label(data_dir: Path, session: str, outcome: str, note: str | None) -> dict[str, Any]:
    """Append one label row; raises OSError when it cannot be written."""
    row = {"at": delegate.now_iso(), "session": session, "outcome": outcome, "note": note}
    path = labels_path(data_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    new = not path.exists()
    with path.open("a", encoding="utf-8") as f:
        f.write(json.dumps(row) + "\n")
    if new:
        os.chmod(path, 0o600)
    return row


def read_labels(data_dir: Path) -> dict[str, str]:
    """The outcome of each labeled session: the last good row wins."""
    out: dict[str, str] = {}
    for row in _read_jsonl(labels_path(data_dir)):
        session, outcome = row.get("session"), row.get("outcome")
        if session_ok(session) and outcome in OUTCOMES:
            out[session] = outcome
    return out


def _count(v: Any) -> int:
    if isinstance(v, int) and not isinstance(v, bool) and 0 <= v <= _INT_LIMIT:
        return v
    return 0


def _read_capped(path: str) -> bytes | None:
    """The bytes of a regular file of at most ``MAX_TRANSCRIPT_BYTES``,
    opened without blocking and checked on the open descriptor, or None."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
    except (OSError, ValueError):
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_TRANSCRIPT_BYTES:
            return None
        chunks = []
        total = 0
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_TRANSCRIPT_BYTES:
                return None
            chunks.append(chunk)
    except OSError:
        return None
    finally:
        os.close(fd)
    return b"".join(chunks)


def session_cost(path: Any) -> dict[str, Any] | None:
    """A session's billed cost from its Claude Code transcript, or None when
    the transcript cannot be read (SW-14)."""
    from opendaisugi.gateway import _CACHE_READ_MULT, _CACHE_WRITE_MULT, _FALLBACK_PRICE
    from opendaisugi.gateway import _PRICES_PER_MTOK as prices

    if not isinstance(path, str) or not os.path.isabs(path):
        return None
    data = _read_capped(path)
    if data is None:
        return None
    messages: dict[Any, tuple[str, dict[str, Any]]] = {}
    for n, line in enumerate(data.decode("utf-8", "replace").split("\n")):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except (ValueError, RecursionError):
            continue
        if not isinstance(row, dict) or row.get("type") != "assistant":
            continue
        msg = row.get("message")
        if not isinstance(msg, dict) or not isinstance(msg.get("usage"), dict):
            continue
        mid = msg.get("id")
        key: Any = ("id", mid) if isinstance(mid, str) else ("line", n)
        model = msg.get("model")
        messages[key] = (model if isinstance(model, str) else "", msg["usage"])
    dollars = 0.0
    quota = 0
    estimated = False
    for model, u in messages.values():
        fresh = _count(u.get("input_tokens"))
        read = _count(u.get("cache_read_input_tokens"))
        write = _count(u.get("cache_creation_input_tokens"))
        out = _count(u.get("output_tokens"))
        price = prices.get(model)
        if price is None:
            price = _FALLBACK_PRICE
            estimated = True
        dollars += (
            fresh * price[0]
            + read * price[0] * _CACHE_READ_MULT
            + write * price[0] * _CACHE_WRITE_MULT
            + out * price[1]
        ) / 1_000_000
        quota += fresh + read + write + out
    return {"dollars": dollars, "quota_tokens": quota, "estimated": estimated}


def _read_audit(data_dir: Path) -> list[dict[str, Any]]:
    """Every audit record, from every audit log, by file name then line."""
    d = data_dir / "gate" / "audit"
    try:
        names = sorted(p.name for p in d.iterdir() if p.name.endswith(".jsonl"))
    except OSError:
        return []
    out = []
    for name in names:
        out.extend(_read_jsonl(d / name))
    return out


def _arm_row() -> dict[str, Any]:
    return {
        "sessions": 0,
        "labeled": 0,
        "passed": 0,
        "failed": 0,
        "unlabeled": 0,
        "cost_unknown": 0,
        "billed_dollars": 0.0,
        "quota_tokens": 0,
        "estimated": False,
        "success_rate": None,
        "dollars_per_success": None,
        "quota_per_success": None,
    }


def trial_state(data_dir: Path) -> dict[str, Any] | None:
    """The graft trial of the rule in force (SW-15), or None when no rule in
    audit or trial is in force."""
    rules, _ = delegate.load_rules(data_dir / "gate")
    rule = next((r for r in rules if r.state in delegate.RULE_STATES_ACTING), None)
    if rule is None or rule.state not in ("audit", "trial"):
        return None
    arms: dict[str, set[str]] = {}
    transcripts: dict[str, str] = {}
    for rec in _read_audit(data_dir):
        session = rec.get("session_id")
        if not isinstance(session, str):
            continue
        tp = rec.get("transcript_path")
        if isinstance(tp, str) and os.path.isabs(tp):
            transcripts[session] = tp
        g = rec.get("graft")
        if (
            isinstance(g, dict)
            and g.get("rule_id") == rule.id
            and _count(g.get("version")) == rule.version
            and g.get("arm") in ("graft", "control")
        ):
            arms.setdefault(session, set()).add(g["arm"])
    labels = read_labels(data_dir)
    rows = {"graft": _arm_row(), "control": _arm_row()}
    conflicting = 0
    for session in sorted(arms):
        if len(arms[session]) > 1:
            conflicting += 1
            continue
        r = rows[next(iter(arms[session]))]
        r["sessions"] += 1
        outcome = labels.get(session)
        if outcome is None:
            r["unlabeled"] += 1
            continue
        r["labeled"] += 1
        r["passed" if outcome == "pass" else "failed"] += 1
        cost = session_cost(transcripts.get(session))
        if cost is None:
            r["cost_unknown"] += 1
            continue
        r["billed_dollars"] += cost["dollars"]
        r["quota_tokens"] += cost["quota_tokens"]
        if cost["estimated"]:
            r["estimated"] = True
    if rule.allow_remote:
        # A remote worker's cost is not in any session's cost.
        rows["graft"]["estimated"] = True
    for r in rows.values():
        if r["labeled"]:
            r["success_rate"] = r["passed"] / r["labeled"]
        if r["passed"] and not r["cost_unknown"]:
            r["dollars_per_success"] = r["billed_dollars"] / r["passed"]
            r["quota_per_success"] = r["quota_tokens"] / r["passed"]
    verdict, text = _verdict(rule, rows["graft"], rows["control"])
    return {
        "rule": rule.id,
        "version": rule.version,
        "state": rule.state,
        "seed": rule.seed,
        "min_labeled": MIN_LABELED,
        "arms": rows,
        "conflicting": conflicting,
        "verdict": verdict,
        "verdict_text": text,
    }


def _verdict(rule: delegate.Rule, g: dict[str, Any], c: dict[str, Any]) -> tuple[str, str]:
    if rule.state == "audit":
        return (
            "none",
            "nothing: the rule is in audit, so both arms only record. "
            "Set its state to trial to compare them.",
        )
    if g["labeled"] < MIN_LABELED or c["labeled"] < MIN_LABELED:
        return (
            "wait",
            f"wait: each arm needs {MIN_LABELED} labeled sessions "
            f"(graft {g['labeled']}, control {c['labeled']}).",
        )
    if g["success_rate"] < c["success_rate"]:
        return (
            "retire",
            f"retire rule {rule.id}: the graft arm's success rate {g['success_rate']:.2f} "
            f"is below the control arm's {c['success_rate']:.2f}.",
        )
    gd, cd = g["dollars_per_success"], c["dollars_per_success"]
    if gd is None or cd is None:
        return (
            "wait",
            "wait: a labeled session's billed cost is unknown, or an arm has no success.",
        )
    if gd < cd:
        return (
            "promote",
            f"promote: set rule {rule.id} to active (billed cost per success "
            f"${gd:.4f} against ${cd:.4f}).",
        )
    return (
        "keep",
        f"keep the trial: billed cost per success ${gd:.4f} is not below ${cd:.4f}.",
    )


def _arm_line(name: str, r: dict[str, Any]) -> str:
    est = " (estimated)" if r["estimated"] else ""
    if r["dollars_per_success"] is None:
        per = "per success: unknown"
    else:
        per = (
            f"per success ${r['dollars_per_success']:.4f}{est}, "
            f"{r['quota_per_success']:.0f} quota tokens"
        )
    unknown = f", {r['cost_unknown']} with no cost" if r["cost_unknown"] else ""
    return (
        f"  {name} arm: {r['sessions']} sessions, {r['labeled']} labeled "
        f"({r['passed']} passed, {r['failed']} failed), {r['unlabeled']} unlabeled{unknown}; "
        f"billed ${r['billed_dollars']:.4f}{est}, {r['quota_tokens']:,} quota tokens; {per}"
    )


def trial_lines(t: dict[str, Any]) -> list[str]:
    """The trial section as ``router status`` prints it."""
    out = [
        f"trial (rule {t['rule']} v{t['version']}, {t['state']}, seed {t['seed']}): "
        "promotion is the operator's; nothing is changed here."
    ]
    out.append(_arm_line("graft", t["arms"]["graft"]))
    out.append(_arm_line("control", t["arms"]["control"]))
    if t["conflicting"]:
        out.append(f"  {t['conflicting']} sessions with both arms recorded are left out.")
    out.append(f"  promotion would: {t['verdict_text']}")
    return out
