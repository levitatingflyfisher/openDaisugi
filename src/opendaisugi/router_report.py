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
"""

from __future__ import annotations

import json
import math
import re
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
        r["estimated"] = r["turns_estimated"] > 0 or r["delegate_ok"] > 0
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
        verb = "go to" if rule["state"] == "active" else "would go to (audit: not denied)"
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
