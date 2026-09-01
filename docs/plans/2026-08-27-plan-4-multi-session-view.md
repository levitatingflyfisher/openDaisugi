# Plan 4: The Multi-Session View (W8 + W5 + W7) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The TUI opens on the live work: every session, its current action, the verdict and why, the cost of the step, and the operator's next move. Wiring is one command away, never first. The tree of one session is one key away.

**Architecture:** A pure-Python model (`cockpit.py`) builds the roster and the header from the session trees, the ask files, the transcripts, and the gate config; the Textual app (`tui.py` + three screen modules) only renders and dispatches keys. Refresh is an mtime poll with in-place cell updates.

**Tech Stack:** Textual `>=8.0` (the `[tui]` extra), Python 3.12, pytest with `app.run_test()` and `pilot.press`.

**Spec:** `docs/plans/2026-08-27-cockpit-spec.md` §7, §9, and §11 (cross-plan rules).
Design source: `docs/research/cockpit-gate-garden-research-2026-08-27.md` §4.2 and §4.3.

**Requires:** plans 1–3 shipped (`config.installed_hook_mode`; `session_tree`, `ask`,
`claude_transcript`, `checkpoints`). Do not build before them.

## Corrections from adversarial review (2026-08-27 — NEEDS AN ARCHITECTURE REVISION FIRST)

The `cockpit.py` model/view split is right and its grouping/counts logic matches its tests;
the two-step allow is misfire-safe; Task 0 (`uv sync`, `importorskip`) is correct; plan 4 is
consistent with plan 3's *real* interfaces given build order. But the screen architecture is
un-buildable as written and two safety controls need hardening. **Do the B1 revision before
any task-by-task build** — it reshapes Tasks 2–6 and most tests.

- **BLOCKER B1 — the `compose`-chrome + `push_screen` architecture breaks the suite and hides
  the mode indicator.** Verified against Textual 8.2.8: `app.query()` resolves against the
  `_default` screen only (`app.py:941-956`, `dom.py:1411`), which is fixed at compose time and
  never re-points to a pushed screen. So (a) every `app.query("#roster"/"#peek"/"#tree"/…)` in
  the tests returns `NoMatches` — the suite cannot go green — and (b) the header /
  always-visible gate-mode indicator / `:` command line composed onto `_default` are hidden
  behind the opaque pushed screen and can't take focus. The mode indicator being invisible is a
  **safety** failure, not cosmetics. **Fix:** a shared base `Screen` subclass that composes the
  header + status + command-line, inherited by `SessionsScreen`/`TreeScreen`/`WiringScreen`;
  real screen switching keyed on `app.screen.name`; every test queries `app.screen.query_one(…)`,
  not `app.query(…)`.
- **BLOCKER B2 — the moved wiring swap tests can't pass, and `apply_swap` returns a `Config`.**
  Spec §7.5 requires the existing swap tests to pass on the wiring screen, but they drive
  `on_input_submitted` / read `_last_status`, which the new App router drops. Give `WiringScreen`
  its own command line + status (via the shared base), route status through `self.app.set_status`,
  and keep the `do_swap` path that turns a swap into the honest confirmation string — do **not**
  `set_status(apply_swap(...))`, which would print a `Config` repr.
- **SHOULD-FIX S1 — "move `compose()` unchanged" is not unchanged.** On a `Screen`, the moved
  body's `self.query_one("#status"/"#cmd")` and `self._last_status`/`self.data_dir` references
  raise `NoMatches`/`AttributeError`. Rework them to `self.app.…` / the shared base.
- **SHOULD-FIX S2 (safety) — the two-step allow degrades to a rubber stamp under habituation.**
  A fixed `a`→`Enter` every time becomes an automatic reflex (Raskin: a confirmation that always
  elicits the same response goes invisible; tool-interface law 7: match the guard to the blast
  radius). For **destructive** classes require a *varying* token (type the session id or the tool
  name); keep the fixed two-step only for low-blast allows.
- **SHOULD-FIX S3 (fail-closed) — `e` edit-input is fail-dangerous.** It writes
  `answer(decision="allow", updated_input=…)`, but §10 says `updatedInput` is unverified against
  Claude Code. If the harness ignores it, the un-narrowed command runs under an `allow` while the
  operator believes they narrowed it. On edit, prefer **deny-with-reason** (bounce it back to the
  agent to re-issue), and mark the edit path unverified in the peek/status.
- **SHOULD-FIX S4/S5/S6 — honesty and reflow.** `s` (steer) is shown "live" in the peek but never
  bound — bind+implement+test it or drop it from the peek (law 10). Estimated cache numbers must
  carry `?` (spec §7.1). Reconcile §7.2's "never rebuilt": cell values update in place, but a row
  crossing a group boundary (WORKING→PARKED) legitimately rebuilds — relax the spec wording to
  "membership/group changes rebuild; values update in place," and make sure `_refresh_ages` sets
  `self._roster_at` (moved out of the prose aside so it isn't an `AttributeError`).

## Global Constraints

- `uv run pytest -q` green and `uv run ruff check .` clean before each commit (line length 100).
- Commit messages: `type(scope): why`. No attribution trailers. Never push.
- Every TUI test file starts with `pytest.importorskip("textual")`. After Task 0 the tests must run, not skip: `uv run pytest tests/test_tui*.py -q` shows no `s`.
- The model never imports Textual. The screens never read files directly; they call `cockpit` and `ask`.
- Keys never move: `a d e r s t` and `Enter` mean the same thing on every screen where they exist. Allow is two-step. The status line says the next key.
- Honesty: a control that does nothing on this harness path says so in place (`not on this path`), it is never dressed as live.
- Do not add symbols named `Session`, `SessionRecord`, or `list_sessions`.

---

### Task 0: Make the `[tui]` extra real

**Files:**
- Modify: `uv.lock` (regenerated)

- [ ] **Step 1: Lock and sync**

Run: `uv lock && uv sync --extra dev`
Then: `uv run python -c "import textual, textual_serve; print(textual.__version__)"`.
Expected: a version `>= 8.0` prints.

- [ ] **Step 2: Confirm the existing TUI tests run**

Run: `uv run pytest tests/test_tui.py -q`
Expected: 6 passed, 0 skipped.

- [ ] **Step 3: Commit**

```bash
git add uv.lock
git commit -m "build: lock the tui extra so the Textual tests run instead of skipping"
```

---

### Task 1: `cockpit.py`: the roster and the header, without Textual

**Files:**
- Create: `src/opendaisugi/cockpit.py`
- Test: `tests/test_cockpit.py` (new)

**Interfaces:**
- Consumes: `session_tree.SessionIndex/SessionTree`, `ask.pending_asks`, `claude_transcript.read_turns/usage_totals/last_model`, `config.installed_hook_mode`, `gate.resolve_gate_mode/is_disarmed`, `gateway_journal.GatewayJournal`.
- Produces: `SessionRow`, `Roster`, `build_roster(data_dir, *, now=None) -> Roster`, `HeaderState`, `header_state(data_dir, *, home=None, roster=None) -> HeaderState`, `GROUPS`, `AlertPolicy`, `load_alert_policy(data_dir) -> dict[str, str]`, `alerts_for(roster, *, recent=50) -> list[Alert]`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cockpit.py
"""The roster: grouped by what the operator must do; the header: honest about mode and cache."""

from __future__ import annotations

import json
import os
from pathlib import Path

from opendaisugi import ask
from opendaisugi.cockpit import GROUPS, build_roster, header_state, load_alert_policy, alerts_for
from opendaisugi.session_tree import SessionTree
from tests.test_claude_transcript import ROWS

NOW = 1_000_000.0


def _claude_session(
    data_dir: Path, sid: str, *, last_ts: float, tool_use_id: str, decision: str
) -> None:
    t = data_dir / f"{sid}.transcript.jsonl"
    t.write_text("\n".join(json.dumps(r) for r in ROWS) + "\n")
    tree = SessionTree.create(
        data_dir / "sessions",
        session_id=sid,
        harness="claude-code",
        cwd="/w",
        harness_session_id=sid,
        transcript_path=str(t),
        clock=lambda: last_ts - 2,
    )
    c = tree.append(
        "tool_call",
        {"toolUseId": tool_use_id, "name": "Bash", "detail": "rm -rf build/"},
        clock=lambda: last_ts - 1,
    )
    tree.append(
        "verdict",
        {
            "toolUseId": tool_use_id,
            "decision": decision,
            "mode": "enforce",
            "clause": "shell: no delete outside tmp",
            "reason": "x",
        },
        parent_id=c.id,
        clock=lambda: last_ts,
    )


def _sprig_session(data_dir: Path, sid: str, *, last_ts: float) -> None:
    tree = SessionTree.create(
        data_dir / "sessions", session_id=sid, harness="sprig", cwd="/e", clock=lambda: last_ts - 3
    )
    tree.append("prompt", {"text": "hi"}, clock=lambda: last_ts - 2)
    tree.append(
        "assistant",
        {
            "model": "claude-sonnet-4",
            "text": "ok",
            "usage": {"fresh": 100, "cacheRead": 900, "cacheWrite": 0, "out": 10},
        },
        clock=lambda: last_ts,
    )


def test_groups_and_order(tmp_path: Path):
    _claude_session(tmp_path, "needs", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "needs", "toolName": "Bash"},
        deadline=NOW + 60,
    )
    _claude_session(tmp_path, "working", last_ts=NOW - 10, tool_use_id="t2", decision="allow")
    _sprig_session(tmp_path, "parked", last_ts=NOW - 600)
    _sprig_session(tmp_path, "done", last_ts=NOW - 7200)
    roster = build_roster(tmp_path, now=NOW)
    assert [r.session_id for r in roster.rows] == ["needs", "working", "parked", "done"]
    assert [r.group for r in roster.rows] == list(GROUPS)
    assert roster.counts == {"NEEDS YOU": 1, "WORKING": 1, "PARKED": 1, "DONE": 1}


def test_row_fields_for_a_claude_session(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.harness == "claude-code" and row.agent == "claude-sonnet-4"
    assert row.action == "Bash rm -rf build/"
    assert row.verdict == "DENY" and row.clause == "shell: no delete outside tmp"
    assert row.steps == 1
    assert (row.fresh, row.cache_read, row.cache_write) == (17, 2100, 50)  # from the transcript
    assert row.age_s == 5 and row.pending_ask is None


def test_row_fields_for_a_sprig_session_use_tree_usage(tmp_path: Path):
    _sprig_session(tmp_path, "e1", last_ts=NOW - 1)
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.harness == "sprig" and row.agent == "claude-sonnet-4"
    assert (row.fresh, row.cache_read) == (100, 900)
    assert row.verdict == "" and row.action == ""


def test_pending_ask_is_attached_to_its_row(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Bash"},
        deadline=NOW + 60,
    )
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.group == "NEEDS YOU" and row.pending_ask["toolUseId"] == "t1"


def test_header_mode_sources(tmp_path: Path):
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    h = header_state(tmp_path, home=home)
    assert h.gate_mode == "SHADOW" and h.gate_mode_source == "config"
    (home / ".claude" / "settings.json").write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "py -m opendaisugi.gate_client --mode enforce",
                                }
                            ]
                        }
                    ]
                }
            }
        )
    )
    h = header_state(tmp_path, home=home)
    assert h.gate_mode == "ENFORCE" and h.gate_mode_source == "hook"
    (tmp_path / "gate").mkdir(exist_ok=True)
    (tmp_path / "gate" / "DISARMED").write_text("")
    assert header_state(tmp_path, home=home).gate_mode == "DISARMED"


def test_header_cache_from_live_transcripts_else_gateway(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="allow")
    h = header_state(tmp_path, home=tmp_path, roster=build_roster(tmp_path, now=NOW))
    assert h.cache_source == "transcripts" and round(h.cache_hit_rate, 2) == round(
        2100 / (17 + 2100 + 50), 2
    )
    assert h.session_count == 1
    empty = header_state(tmp_path / "none", home=tmp_path)
    assert empty.cache_hit_rate is None and empty.cache_source == "none"


def test_alert_policy_defaults_and_file(tmp_path: Path):
    pol = load_alert_policy(tmp_path)
    assert pol["deny"] == "log" and pol["ask"] == "pause" and pol["budget"] == "pause"
    (tmp_path / "alerts.yaml").write_text("deny: pause\n")
    assert load_alert_policy(tmp_path)["deny"] == "pause"


def test_alerts_count_by_class(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    _claude_session(tmp_path, "s2", last_ts=NOW - 6, tool_use_id="t2", decision="deny")
    ask.post_ask(
        tmp_path / "gate", tool_use_id="t2", question={"sessionId": "s2"}, deadline=NOW + 60
    )
    alerts = alerts_for(build_roster(tmp_path, now=NOW))
    by_class = {a.klass: a for a in alerts}
    assert by_class["deny"].count == 2 and set(by_class["deny"].sessions) == {"s1", "s2"}
    assert by_class["ask"].count == 1 and by_class["ask"].policy == "pause"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cockpit.py -q`
Expected: FAIL with `ModuleNotFoundError: opendaisugi.cockpit`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/cockpit.py
"""The multi-session view's model: what is on screen, computed from the stores.

No Textual here. Rows are grouped by what the operator must do (NEEDS YOU,
WORKING, PARKED, DONE), never by id. Numbers say where they come from.
"""

from __future__ import annotations

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
_DESTRUCTIVE = ("rm ", "rm -", "git reset --hard", "git push --force", "drop table", "> /dev/")


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
        asks_by_session.setdefault(sid, a)
    rows: list[SessionRow] = []
    for s in SessionIndex(data_dir / "sessions").list():
        entries = SessionTree.open(data_dir / "sessions", s.session_id).entries()
        pending = asks_by_session.get(s.harness_session_id or "") or asks_by_session.get(
            s.session_id
        )
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
            )
        )
    order = {g: i for i, g in enumerate(GROUPS)}
    rows.sort(key=lambda r: (order[r.group], r.age_s))
    counts = {g: sum(1 for r in rows if r.group == g) for g in GROUPS}
    return Roster(rows=rows, counts=counts)


def header_state(
    data_dir: Path, *, home: Path | None = None, roster: Roster | None = None
) -> HeaderState:
    from opendaisugi.config import installed_hook_mode
    from opendaisugi.gate import is_disarmed, resolve_gate_mode

    home = home or Path.home()
    root = data_dir / "gate"
    hook_mode = installed_hook_mode(home / ".claude" / "settings.json")
    mode = hook_mode or resolve_gate_mode(None, root=root)
    source = "hook" if hook_mode else "config"
    if is_disarmed(root):
        mode = "disarmed"
    roster = roster if roster is not None else build_roster(data_dir)
    live = [r for r in roster.rows if r.group in ("NEEDS YOU", "WORKING")]
    total = sum(r.fresh + r.cache_read + r.cache_write for r in live)
    if total > 0:
        rate, cache_source = sum(r.cache_read for r in live) / total, "transcripts"
    else:
        rate, cache_source = None, "none"
        try:
            from opendaisugi.gateway_journal import GatewayJournal

            summ = GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").summary()
            if summ.turns:
                rate, cache_source = summ.cache_hit_rate, "gateway"
        except Exception:  # noqa: BLE001 — the header must never fail on a store
            pass
    return HeaderState(
        gate_mode=mode.upper(),
        gate_mode_source=source,
        cache_hit_rate=rate,
        cache_source=cache_source,
        session_count=len(roster.rows),
    )


def load_alert_policy(data_dir: Path) -> dict[str, str]:
    pol = dict(_DEFAULT_ALERTS)
    p = data_dir / "alerts.yaml"
    if p.exists():
        try:
            raw = yaml.safe_load(p.read_text(encoding="utf-8")) or {}
            pol.update(
                {str(k): str(v) for k, v in raw.items() if str(v) in ("log", "pause", "modal")}
            )
        except yaml.YAMLError:
            pass
    return pol


def alerts_for(roster: Roster, *, policy: dict[str, str] | None = None) -> list[Alert]:
    policy = policy or dict(_DEFAULT_ALERTS)
    denies = [r for r in roster.rows if r.verdict == "DENY"]
    destructive = [r for r in denies if any(m in r.action for m in _DESTRUCTIVE)]
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cockpit.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cockpit.py tests/test_cockpit.py
git commit -m "feat(cockpit): the roster and header model, grouped by what the operator must do"
```

---

### Task 2: The app has three screens; the wiring screen keeps today's behaviour

**Files:**
- Modify: `src/opendaisugi/tui.py` (becomes `DaisugiApp` + screen registry + `run_tui`/`serve`)
- Create: `src/opendaisugi/tui_wiring.py` (today's `compose`/actions moved into `WiringScreen`)
- Create: `src/opendaisugi/tui_sessions.py` (`SessionsScreen`, a placeholder table this task; filled in Task 3)
- Create: `src/opendaisugi/tui_tree.py` (`TreeScreen`, a placeholder this task; filled in Task 6)
- Test: `tests/test_tui.py` (adapt), `tests/test_tui_app.py` (new)

**Interfaces:**
- Produces: `DaisugiApp(data_dir, interval=2.0)`, `DaisugiApp.SCREENS = {"sessions", "wiring", "tree"}`, `action_cycle` (Tab), `action_cmd` (`:`), commands `sessions`, `wiring`, `tree [<id>]`, `gate <mode>` (delegates to the wiring swap), `q`. `DashboardApp = DaisugiApp` stays as an alias for one release.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tui_app.py
"""The instrument opens on the live work; wiring is a command away."""

import asyncio

import pytest

pytest.importorskip("textual")

from opendaisugi.tui import DaisugiApp  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def test_app_opens_on_the_sessions_screen(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert app.screen.name == "sessions"
            assert app.query("#roster"), "roster table did not render"
            assert "gate:" in app.query_one("#hdr").renderable.__str__()

    _run(scenario)


def test_colon_wiring_and_tab_cycle(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("colon")
            await pilot.press(*"wiring")
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name == "wiring"
            assert app.query("#gauge-matcher"), "wiring did not render its gauges"
            await pilot.press("tab")
            await pilot.pause()
            assert app.screen.name == "tree"
            await pilot.press("tab")
            await pilot.pause()
            assert app.screen.name == "sessions"

    _run(scenario)


def test_unknown_command_says_so(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("colon")
            await pilot.press(*"banana")
            await pilot.press("enter")
            await pilot.pause()
            assert "unknown command" in app.status_text

    _run(scenario)
```

Adapt `tests/test_tui.py`: replace `DashboardApp` with `DaisugiApp`, and in each scenario
add `await app.push_screen("wiring")` (or `app.switch_screen("wiring")`) followed by
`await pilot.pause()` before the existing assertions and `do_swap` calls. `do_swap` moves
to `WiringScreen`; call `app.screen.do_swap(...)`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_tui_app.py tests/test_tui.py -q`
Expected: FAIL with `ImportError: DaisugiApp`.

- [ ] **Step 3: Write minimal implementation**

`tui_wiring.py`: move everything from today's `DashboardApp` except `BINDINGS` for
`tab`/`colon`/`q` into `class WiringScreen(Screen)`. Its `compose()` is today's
`compose()` minus `Header`, `Input#cmd`, `Static#status`, `Footer` (those belong to the
app). `on_mount`, `action_next/prev/select/refresh`, `do_swap`, `on_button_pressed`,
`_title_line` move unchanged; `self.data_dir`, `self.config_path`, `self._stages` come
from `self.app`. `BINDINGS` on the screen: `j/k/down/up`, `enter/space`, `r`.

`tui_sessions.py` (this task, minimal):

```python
from __future__ import annotations

from textual.app import ComposeResult
from textual.binding import Binding
from textual.screen import Screen
from textual.widgets import DataTable, Static


class SessionsScreen(Screen):
    BINDINGS = [
        Binding("down,j", "cursor_down", "▼", show=True),
        Binding("up,k", "cursor_up", "▲", show=True),
    ]

    def compose(self) -> ComposeResult:
        table = DataTable(id="roster", cursor_type="row", zebra_stripes=False)
        table.add_columns(
            "session", "action", "verdict", "clause", "steps", "↑fresh", "⟳read", "✎write", "age"
        )
        yield table
        yield Static("", id="peek")

    def action_cursor_down(self) -> None:
        self.query_one("#roster", DataTable).action_cursor_down()

    def action_cursor_up(self) -> None:
        self.query_one("#roster", DataTable).action_cursor_up()
```

`tui_tree.py` (this task, minimal): `class TreeScreen(Screen)` composing a
`Static("no session selected · press t on a row", id="tree-empty")`.

`tui.py`:

```python
"""The operator's instrument: three screens over one store.

sessions (default): every session, its action, verdict, cost, and what the
operator must do. tree: one session's prompt tree. wiring: the module map and
swap knobs (today's dashboard), reachable by `:wiring` or Tab, never first.
"""

from __future__ import annotations

import shlex
import sys
from pathlib import Path

from opendaisugi.modules import detect_stages

try:
    from textual.app import App, ComposeResult
    from textual.binding import Binding
    from textual.widgets import Footer, Header, Input, Static

    _HAVE_TEXTUAL = True
except ImportError:  # pragma: no cover
    _HAVE_TEXTUAL = False


class TextualNotInstalled(RuntimeError):
    """Raised when the GUI is requested without the [tui] extra."""


if _HAVE_TEXTUAL:
    from opendaisugi.tui_sessions import SessionsScreen
    from opendaisugi.tui_tree import TreeScreen
    from opendaisugi.tui_wiring import WiringScreen

    _ORDER = ("sessions", "tree", "wiring")

    class DaisugiApp(App):
        TITLE = "daisugi"
        SCREENS = {"sessions": SessionsScreen, "tree": TreeScreen, "wiring": WiringScreen}
        CSS = """
        #hdr { dock: top; height: 1; padding: 0 1; background: $panel; }
        #status { dock: bottom; height: 1; padding: 0 1; background: $panel; }
        #cmd { dock: bottom; display: none; border: tall $accent; }
        """
        BINDINGS = [
            Binding("tab", "cycle", "next view", show=True),
            Binding("colon", "cmd", ": command", show=True),
            Binding("question_mark", "help", "? keys", show=True),
            Binding("q", "quit", "quit", show=True),
            Binding("escape", "hide_cmd", "cancel", show=False),
        ]

        def __init__(self, *, data_dir: Path, interval: float = 2.0) -> None:
            super().__init__()
            self.data_dir = Path(data_dir)
            self.interval = interval
            self.config_path = self.data_dir / "config.yaml"
            self._stages = detect_stages(self.data_dir)
            self.status_text = ""
            self.selected_session: str | None = None

        def compose(self) -> ComposeResult:
            yield Header(show_clock=True)
            yield Static("", id="hdr")
            yield Input(placeholder="sessions · tree <id> · wiring · gate enforce|shadow", id="cmd")
            yield Static("Tab next view · : command · ? keys · q quit", id="status")
            yield Footer()

        def on_mount(self) -> None:
            self.push_screen("sessions")
            self.refresh_header()

        # --- header ---------------------------------------------------------
        def refresh_header(self) -> None:
            from opendaisugi.cockpit import build_roster, header_state

            roster = build_roster(self.data_dir)
            h = header_state(self.data_dir, roster=roster)
            cache = (
                "cache —"
                if h.cache_hit_rate is None
                else f"cache {h.cache_hit_rate:.0%} hit ({h.cache_source})"
            )
            name = self.screen.name if self.screen is not None else "sessions"
            self.query_one("#hdr", Static).update(
                f" daisugi · {name:<9} gate: {h.gate_mode} ({h.gate_mode_source})   {cache}   {h.session_count} sessions"
            )

        # --- views ----------------------------------------------------------
        def action_cycle(self) -> None:
            cur = self.screen.name or "sessions"
            nxt = _ORDER[(_ORDER.index(cur) + 1) % len(_ORDER)] if cur in _ORDER else "sessions"
            self.switch_screen(nxt)
            self.refresh_header()

        def action_cmd(self) -> None:
            box = self.query_one("#cmd", Input)
            box.display = True
            box.value = ""
            box.focus()

        def action_hide_cmd(self) -> None:
            self.query_one("#cmd", Input).display = False

        def action_help(self) -> None:
            keys = ", ".join(
                f"{b.key}={b.description}" for b in self.screen.BINDINGS if getattr(b, "show", True)
            )
            self.set_status(f"keys: {keys} · Tab next view · : command · q quit")

        def on_input_submitted(self, event: "Input.Submitted") -> None:
            self.action_hide_cmd()
            self.run_command(event.value.strip())

        def run_command(self, line: str) -> None:
            parts = shlex.split(line) if line else []
            if not parts:
                return
            verb, args = parts[0], parts[1:]
            if verb in _ORDER:
                if verb == "tree" and args:
                    self.selected_session = args[0]
                self.switch_screen(verb)
                self.refresh_header()
                self.set_status(f"view: {verb}")
            elif verb == "gate" and args and args[0] in ("enforce", "shadow"):
                from opendaisugi.swap import apply_swap

                self.set_status(apply_swap("gate", args[0], config_path=self.config_path))
                self.refresh_header()
            elif verb in ("q", "quit"):
                self.exit()
            else:
                self.set_status(
                    f"unknown command: {line!r} · try sessions, tree <id>, wiring, gate enforce"
                )

        def set_status(self, text: str) -> None:
            self.status_text = text
            self.query_one("#status", Static).update(text)

    DashboardApp = DaisugiApp  # one-release alias


def run_tui(data_dir: Path, *, interval: float = 2.0) -> None:
    if not _HAVE_TEXTUAL:
        raise TextualNotInstalled("The GUI needs the tui extra: uv pip install 'opendaisugi[tui]'")
    DaisugiApp(data_dir=data_dir, interval=interval).run()


def serve(
    data_dir: Path, *, host: str = "127.0.0.1", port: int = 8000, interval: float = 2.0
) -> None:
    if not _HAVE_TEXTUAL:
        raise TextualNotInstalled("The GUI needs the tui extra: uv pip install 'opendaisugi[tui]'")
    from textual_serve.server import Server

    command = (
        f"{shlex.quote(sys.executable)} -m opendaisugi.cli dashboard --tui "
        f"--data-dir {shlex.quote(str(data_dir))} --interval {interval}"
    )
    Server(command, host=host, port=port, title="daisugi").serve()
```

If a Textual name differs in the installed version (`switch_screen`, `Screen.name`,
`Binding` fields), check `uv run python -c "import textual; print(textual.__version__)"`
against the Textual docs for that version and keep the behaviour, not the name.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_tui_app.py tests/test_tui.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tui.py src/opendaisugi/tui_wiring.py src/opendaisugi/tui_sessions.py src/opendaisugi/tui_tree.py tests/test_tui.py tests/test_tui_app.py
git commit -m "feat(tui): open on the sessions view; wiring is :wiring or Tab, never first"
```

---

### Task 3: The sessions screen: grouped roster, peek pane, in-place refresh

**Files:**
- Modify: `src/opendaisugi/tui_sessions.py`
- Test: `tests/test_tui_sessions.py` (new)

**Interfaces:**
- Produces: `SessionsScreen.reload()`, `SessionsScreen.selected_row -> SessionRow | None`, `SessionsScreen.render_peek(row)`, 1 s mtime poll (`_poll`), row keys `"row:<session_id>"` and `"group:<name>"`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tui_sessions.py
"""The roster: grouped, with a peek pane, refreshed in place."""

import asyncio
import time

import pytest

pytest.importorskip("textual")

from textual.widgets import DataTable, Static  # noqa: E402

from opendaisugi.session_tree import SessionTree  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session, _sprig_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def _cells(table: DataTable) -> list[list[str]]:
    return [[str(c) for c in table.get_row_at(i)] for i in range(table.row_count)]


def test_roster_shows_groups_and_rows(tmp_path):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 5, tool_use_id="t1", decision="deny")
    _sprig_session(tmp_path, "e1", last_ts=now - 600)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            rows = _cells(app.query_one("#roster", DataTable))
            first_col = [r[0] for r in rows]
            assert first_col[0].startswith("WORKING (1)")
            assert "s1" in first_col[1]
            assert any(c.startswith("PARKED (1)") for c in first_col)
            assert "DENY" in rows[1][2] and "rm -rf build/" in rows[1][1]

    _run(scenario)


def test_peek_shows_verdict_clause_and_keys(tmp_path):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 5, tool_use_id="t1", decision="deny")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")  # off the group header, onto the row
            await pilot.pause()
            peek = str(app.query_one("#peek", Static).renderable)
            assert "s1" in peek and "DENY" in peek and "shell: no delete outside tmp" in peek
            assert "a allow" in peek and "d deny" in peek and "t tree" in peek

    _run(scenario)


def test_cell_updates_are_in_place_and_new_sessions_rebuild_once(tmp_path):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 5, tool_use_id="t1", decision="allow")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            table = app.query_one("#roster", DataTable)
            gen = app.screen._table_generation
            # a new verdict on the same session: cells update, no rebuild
            tree = SessionTree.open(tmp_path / "sessions", "s1")
            c = tree.append("tool_call", {"toolUseId": "t9", "name": "Read", "detail": "x.py"})
            tree.append(
                "verdict",
                {"toolUseId": "t9", "decision": "deny", "clause": "files: no"},
                parent_id=c.id,
            )
            app.screen._poll()
            await pilot.pause()
            assert app.screen._table_generation == gen
            assert "DENY" in [str(x) for x in table.get_row_at(1)][2]
            # a new session in a new group: the table is rebuilt once
            before = table.row_count
            _sprig_session(tmp_path, "e1", last_ts=time.time() - 600)
            app.screen._poll()
            await pilot.pause()
            assert table.row_count == before + 2  # the PARKED header + the row
            assert app.screen._table_generation == gen + 1

    _run(scenario)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_tui_sessions.py -q`
Expected: FAIL (the table has no rows; no `_poll`).

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/tui_sessions.py
"""The sessions screen: every session's current action and verdict, grouped by
what the operator must do. Rows update in place; nothing reflows under the eye."""

from __future__ import annotations

import time

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.screen import Screen
from textual.widgets import DataTable, Static

from opendaisugi.cockpit import GROUPS, PARKED_S, WORKING_S, Roster, SessionRow, build_roster
from opendaisugi.session_tree import SessionIndex

_COLS = ("session", "action", "verdict", "clause", "steps", "↑fresh", "⟳read", "✎write", "age")


def _age(s: float) -> str:
    return f"{int(s)}s" if s < 60 else f"{int(s // 60)}m" if s < 3600 else f"{s / 3600:.1f}h"


def _k(n: int) -> str:
    return f"{n / 1000:.1f}k" if n >= 1000 else str(n)


def _cells(r: SessionRow) -> list[str]:
    return [
        f"  {r.session_id[:14]}",
        r.action[:36],
        r.verdict,
        r.clause[:30],
        str(r.steps),
        _k(r.fresh),
        _k(r.cache_read),
        _k(r.cache_write),
        _age(r.age_s),
    ]


class SessionsScreen(Screen):
    BINDINGS = [
        Binding("down,j", "cursor_down", "▼", show=True),
        Binding("up,k", "cursor_up", "▲", show=True),
        Binding("t", "tree", "t tree", show=True),
        Binding("enter", "confirm", "⏎ confirm/attach", show=True),
    ]

    def __init__(self) -> None:
        super().__init__()
        self._roster: Roster | None = None
        self._rows: dict[str, SessionRow] = {}
        self._keys: dict[str, object] = {}  # session_id -> row key
        self._col_keys: list[object] = []
        self._last_mtime = -1.0
        self._table_generation = 0
        self._armed: str | None = None

    def compose(self) -> ComposeResult:
        with Vertical():
            table = DataTable(id="roster", cursor_type="row", zebra_stripes=False)
            yield table
            yield Static("", id="alerts")
            yield Static("", id="peek")

    def on_mount(self) -> None:
        table = self.query_one("#roster", DataTable)
        self._col_keys = list(table.add_columns(*_COLS))
        self.reload(force=True)
        self.set_interval(1.0, self._poll)

    # --- data ---------------------------------------------------------------
    def _poll(self) -> None:
        idx = SessionIndex(self.app.data_dir / "sessions")
        asks = self.app.data_dir / "gate" / "asks"
        m = max(idx.mtime(), asks.stat().st_mtime if asks.exists() else 0.0)
        if m != self._last_mtime:
            self._last_mtime = m
            self.reload()
        else:
            self._refresh_ages()

    def reload(self, *, force: bool = False) -> None:
        roster = build_roster(self.app.data_dir)
        table = self.query_one("#roster", DataTable)
        new_ids = [r.session_id for r in roster.rows]
        old_ids = [r.session_id for r in (self._roster.rows if self._roster else [])]
        groups_changed = [r.group for r in roster.rows] != [
            self._rows[i].group for i in old_ids if i in self._rows
        ]
        if force or new_ids != old_ids or groups_changed:
            self._rebuild(table, roster)
        else:
            for r in roster.rows:
                key = self._keys[r.session_id]
                for col, val in zip(self._col_keys, _cells(r), strict=True):
                    table.update_cell(key, col, val)
        self._roster, self._rows = roster, {r.session_id: r for r in roster.rows}
        self._render_alerts()
        self.render_peek(self.selected_row)
        self.app.refresh_header()

    def _rebuild(self, table: DataTable, roster: Roster) -> None:
        # Only when the set or the grouping of sessions changed; cell edits are in place.
        if self._roster is not None:
            self._table_generation += 1
        table.clear()
        self._keys.clear()
        for g in GROUPS:
            rows = [r for r in roster.rows if r.group == g]
            if not rows:
                continue
            table.add_row(f"{g} ({len(rows)})", *[""] * (len(_COLS) - 1), key=f"group:{g}")
            for r in rows:
                self._keys[r.session_id] = table.add_row(*_cells(r), key=f"row:{r.session_id}")
        if self._roster is None:
            self._table_generation = 0

    def _refresh_ages(self) -> None:
        if not self._roster:
            return
        table = self.query_one("#roster", DataTable)
        delta = time.time() - self._roster_at
        for r in self._roster.rows:
            age = r.age_s + delta
            crossed = (r.group == "WORKING" and age >= WORKING_S) or (
                r.group == "PARKED" and age >= PARKED_S
            )
            if crossed and not r.pending_ask:
                self.reload()  # a row changed group with no file change: regroup once
                return
            table.update_cell(self._keys[r.session_id], self._col_keys[-1], _age(age))

    # --- selection ----------------------------------------------------------
    @property
    def selected_row(self) -> SessionRow | None:
        table = self.query_one("#roster", DataTable)
        if table.row_count == 0:
            return None
        key = table.coordinate_to_cell_key(table.cursor_coordinate).row_key.value
        if not key or not key.startswith("row:"):
            return None
        return self._rows.get(key[4:])

    def on_data_table_row_highlighted(self, _event) -> None:
        self._armed = None
        self.render_peek(self.selected_row)

    def render_peek(self, row: SessionRow | None) -> None:
        peek = self.query_one("#peek", Static)
        if row is None:
            peek.update("select a session row · j/k move")
            return
        v = row.last_verdict or {}
        lines = [
            f"{row.session_id} · {row.agent} · {row.harness} · {row.cwd}",
            f"proposes {row.action or '(nothing yet)'}",
            f"verdict {row.verdict or '—'} · clause {row.clause or '—'} · "
            f"envelope {v.get('envelopeId') or '—'} · {v.get('latencyMs', '—')} ms",
        ]
        if v.get("counterexample"):
            lines.append(f"counterexample: {v['counterexample']}")
        if row.pending_ask:
            left = int(float(row.pending_ask.get("deadline", 0)) - time.time())
            lines.append(
                f"ASK pending · {max(left, 0)}s left · reason: {row.pending_ask.get('reason', '')}"
            )
        steer = "s steer" if row.harness == "sprig" else "s steer (not on this path)"
        lines.append(
            f"a allow (then ⏎)  d deny  e edit input  r remember…  {steer}  t tree  ⏎ attach"
        )
        peek.update("\n".join(lines))

    def _render_alerts(self) -> None:
        from opendaisugi.cockpit import alerts_for, load_alert_policy

        alerts = (
            alerts_for(self._roster, policy=load_alert_policy(self.app.data_dir))
            if self._roster
            else []
        )
        text = "   ".join(f"{a.count}× {a.klass} ({', '.join(a.sessions[:3])})" for a in alerts)
        self.query_one("#alerts", Static).update(f"ALERTS  {text}" if text else "ALERTS  none")

    def action_cursor_down(self) -> None:
        self.query_one("#roster", DataTable).action_cursor_down()

    def action_cursor_up(self) -> None:
        self.query_one("#roster", DataTable).action_cursor_up()

    def action_tree(self) -> None:
        row = self.selected_row
        if row is None:
            self.app.set_status("no session selected")
            return
        self.app.selected_session = row.session_id
        self.app.switch_screen("tree")
        self.app.refresh_header()

    def action_confirm(
        self,
    ) -> None:  # Enter: filled in by Task 4 (allow confirm) and Task 5 (attach)
        self.app.set_status("nothing armed · a then ⏎ to allow")
```

Set `self._roster_at = time.time()` in `reload` next to `self._roster = roster`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_tui_sessions.py tests/test_tui_app.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tui_sessions.py tests/test_tui_sessions.py
git commit -m "feat(tui): the grouped roster with a peek pane, refreshed in place"
```

---

### Task 4: Answering an ask: `a` then Enter, `d`, `e`, `r`; the presence heartbeat

**Files:**
- Modify: `src/opendaisugi/tui_sessions.py`, `src/opendaisugi/tui.py` (heartbeat on the app)
- Test: `tests/test_tui_ask.py` (new)

**Interfaces:**
- Consumes: `ask.answer`, `ask.write_presence`, `ask.clear_presence`, `ask.propose`.
- Produces: bindings `a`, `d`, `e`, `r` on `SessionsScreen`; `action_confirm` completes an armed allow; `DaisugiApp._heartbeat()` every 5 s; presence cleared in `on_unmount`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tui_ask.py
"""Allow is two keys, deny is one, and the operator's presence is a heartbeat file."""

import asyncio
import json
import time

import pytest

pytest.importorskip("textual")

from opendaisugi import ask  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def _seed(tmp_path):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 2, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Bash", "toolInput": {"command": "rm -rf build/"}},
        deadline=now + 60,
    )


def test_allow_needs_a_then_enter(tmp_path):
    _seed(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("a")
            await pilot.pause()
            assert "⏎ to confirm allow" in app.status_text
            assert not (tmp_path / "gate" / "answers" / "t1.json").exists()
            await pilot.press("k")  # moving disarms
            await pilot.press("j")
            await pilot.press("enter")
            await pilot.pause()
            assert not (tmp_path / "gate" / "answers" / "t1.json").exists()
            await pilot.press("a")
            await pilot.press("enter")
            await pilot.pause()
            body = json.loads((tmp_path / "gate" / "answers" / "t1.json").read_text())
            assert body["decision"] == "allow"
            assert "allowed t1" in app.status_text

    _run(scenario)


def test_deny_is_one_key(tmp_path):
    _seed(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("d")
            await pilot.pause()
            body = json.loads((tmp_path / "gate" / "answers" / "t1.json").read_text())
            assert body["decision"] == "deny"

    _run(scenario)


def test_edit_input_writes_updated_input(tmp_path):
    _seed(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("e")
            await pilot.pause()
            box = app.query_one("#cmd")
            assert box.display and "rm -rf build/" in box.value
            box.value = "rm -rf build/tmp"
            await pilot.press("enter")
            await pilot.pause()
            body = json.loads((tmp_path / "gate" / "answers" / "t1.json").read_text())
            assert body["decision"] == "allow" and body["updatedInput"] == {
                "command": "rm -rf build/tmp"
            }

    _run(scenario)


def test_remember_writes_a_proposal_not_an_envelope(tmp_path):
    _seed(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("r")
            await pilot.pause()
            await pilot.press("p")  # scope: project
            await pilot.pause()
            files = list((tmp_path / "gate" / "proposals").glob("*.json"))
            assert len(files) == 1
            body = json.loads(files[0].read_text())
            assert body["scope"] == "project" and body["kind"] == "allow-pattern"
            assert not list((tmp_path / "gate" / "envelopes").glob("*.json"))  # nothing applied

    _run(scenario)


def test_presence_while_running_and_gone_after(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert ask.operator_present(tmp_path / "gate")
        assert not (tmp_path / "gate" / "operator.json").exists()

    _run(scenario)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_tui_ask.py -q`
Expected: FAIL (no answer files; no presence).

- [ ] **Step 3: Write minimal implementation**

In `SessionsScreen.BINDINGS` add:

```python
(Binding("a", "arm_allow", "a allow", show=True),)
(Binding("d", "deny", "d deny", show=True),)
(Binding("e", "edit_input", "e edit", show=True),)
(Binding("r", "remember", "r remember", show=True),)
```

Methods:

```python
def _pending(self) -> tuple[SessionRow, str] | None:
    row = self.selected_row
    if row is None or not row.pending_ask:
        self.app.set_status("no pending ask on this row")
        return None
    return row, str(row.pending_ask["toolUseId"])


def action_arm_allow(self) -> None:
    p = self._pending()
    if p:
        self._armed = p[1]
        self.app.set_status(f"allow {p[1]}? ⏎ to confirm allow · move the cursor to cancel")


def action_confirm(self) -> None:
    if (
        self._armed
        and self.selected_row
        and self.selected_row.pending_ask
        and str(self.selected_row.pending_ask["toolUseId"]) == self._armed
    ):
        from opendaisugi import ask as _ask

        _ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=self._armed,
            decision="allow",
            reason="operator allowed from the sessions view",
        )
        self.app.set_status(f"allowed {self._armed}")
        self._armed = None
        self.reload()
        return
    self._armed = None
    self.action_attach()  # Task 5


def action_deny(self) -> None:
    p = self._pending()
    if p:
        from opendaisugi import ask as _ask

        _ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=p[1],
            decision="deny",
            reason="operator denied from the sessions view",
        )
        self.app.set_status(f"denied {p[1]}")
        self.reload()


def action_edit_input(self) -> None:
    p = self._pending()
    if not p:
        return
    row, tid = p
    tool_input = row.pending_ask.get("toolInput") or {}
    key = next((k for k in ("command", "file_path", "path", "url") if k in tool_input), None)
    if key is None:
        self.app.set_status("this input has no editable field")
        return
    self._editing = (tid, key)
    self.app.open_cmd(prefill=str(tool_input[key]))


def on_cmd_submitted(self, value: str) -> bool:
    """The app calls this before its own command parser; True means handled."""
    editing = getattr(self, "_editing", None)
    if editing:
        from opendaisugi import ask as _ask

        tid, key = editing
        self._editing = None
        _ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=tid,
            decision="allow",
            reason="operator edited the input",
            updated_input={key: value},
        )
        self.app.set_status(f"allowed {tid} with edited {key}")
        self.reload()
        return True
    return False


def action_remember(self) -> None:
    p = self._pending()
    if p:
        row, tid = p
        self.app.push_screen(RememberScope(), callback=lambda scope: self._propose(row, tid, scope))


def _propose(self, row: SessionRow, tid: str, scope: str | None) -> None:
    if scope is None:
        self.app.set_status("remember cancelled")
        return
    from opendaisugi import ask as _ask

    _ask.propose(
        self.app.data_dir / "gate",
        kind="allow-pattern",
        scope=scope,
        expires_at=time.time() + 30 * 86400,
        body={
            "sessionId": row.session_id,
            "toolUseId": tid,
            "toolInput": (row.pending_ask or {}).get("toolInput"),
            "clause": row.clause,
        },
    )
    self.app.set_status(
        f"proposal written (scope {scope}) · apply it with `daisugi gate proposals`"
    )
```

The scope picker is a modal so the keys cannot leak into the table (and the two-step
allow is disarmed by cursor movement, never by a key handler):

```python
from textual.screen import ModalScreen


class RememberScope(ModalScreen[str | None]):
    BINDINGS = [
        Binding("o", "pick('once')", "o once"),
        Binding("s", "pick('session')", "s session"),
        Binding("p", "pick('project')", "p project"),
        Binding("g", "pick('global')", "g global"),
        Binding("escape", "pick_none", "Esc cancel"),
    ]

    def compose(self) -> ComposeResult:
        yield Static(
            "remember this allow for:  o once   s session   p project   g global   Esc cancel",
            id="scope",
        )

    def action_pick(self, scope: str) -> None:
        self.dismiss(scope)

    def action_pick_none(self) -> None:
        self.dismiss(None)
```

In `DaisugiApp`: `open_cmd(prefill="")` shows the input with a value;
`on_input_submitted` first asks `self.screen.on_cmd_submitted(value)` when the screen
has it and returns if handled. `on_mount` adds
`self.set_interval(5.0, self._heartbeat)` and calls `self._heartbeat()` once;
`_heartbeat` = `ask.write_presence(self.data_dir / "gate")`; `on_unmount` =
`ask.clear_presence(self.data_dir / "gate")`. Wrap both in `try/except OSError`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_tui_ask.py tests/test_tui_sessions.py tests/test_ask.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tui_sessions.py src/opendaisugi/tui.py tests/test_tui_ask.py
git commit -m "feat(tui): answer an ask with a+Enter, d, e, or r; presence is a heartbeat file"
```

---

### Task 5: Honesty: mode in the header, attach and steer on path D, wiring tags survive

**Files:**
- Modify: `src/opendaisugi/tui_sessions.py` (`action_attach`), `src/opendaisugi/tui.py` (header text already), `src/opendaisugi/tui_wiring.py` (nothing to change; test it)
- Test: `tests/test_tui_honesty.py` (new)

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tui_honesty.py
"""No control looks live when it is not; the mode is on screen at all times."""

import asyncio
import json
import time

import pytest

pytest.importorskip("textual")

from textual.widgets import Static  # noqa: E402

from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session, _sprig_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def test_header_shows_enforce_from_the_installed_hook(tmp_path, monkeypatch):
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    (home / ".claude" / "settings.json").write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "py -m opendaisugi.gate_client --mode enforce",
                                }
                            ]
                        }
                    ]
                }
            }
        )
    )
    monkeypatch.setattr("pathlib.Path.home", lambda: home)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert "gate: ENFORCE (hook)" in str(app.query_one("#hdr", Static).renderable)

    _run(scenario)


def test_header_shows_disarmed(tmp_path):
    (tmp_path / "gate").mkdir()
    (tmp_path / "gate" / "DISARMED").write_text("")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert "gate: DISARMED" in str(app.query_one("#hdr", Static).renderable)

    _run(scenario)


def test_attach_on_claude_path_shows_the_resume_command(tmp_path):
    _claude_session(
        tmp_path, "abc-123", last_ts=time.time() - 1, tool_use_id="t1", decision="allow"
    )

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("enter")
            await pilot.pause()
            assert "claude --resume abc-123" in app.status_text

    _run(scenario)


def test_steer_is_marked_not_on_this_path_for_claude_and_live_for_sprig(tmp_path):
    _claude_session(tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="t1", decision="allow")
    _sprig_session(tmp_path, "e1", last_ts=time.time() - 2)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.pause()
            assert "s steer (not on this path)" in str(app.query_one("#peek", Static).renderable)
            await pilot.press("j")
            await pilot.pause()
            peek = str(app.query_one("#peek", Static).renderable)
            assert "s steer" in peek and "not on this path" not in peek

    _run(scenario)


def test_wiring_keeps_its_effect_tags(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("wiring")
            await pilot.pause()
            assert app.query("#effnote-verifier"), "planned stages must still say so"

    _run(scenario)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_tui_honesty.py -q`
Expected: FAIL on `attach` (status unchanged).

- [ ] **Step 3: Write minimal implementation**

In `SessionsScreen`:

```python
def action_attach(self) -> None:
    row = self.selected_row
    if row is None:
        self.app.set_status("no session selected")
        return
    if row.harness == "claude-code":
        sid = row.session_id
        cmd = f"claude --resume {sid}"
        self.app.copy_to_clipboard(cmd)
        self.app.set_status(f"attach: run `{cmd}` in {row.cwd or 'its directory'} (copied)")
    elif row.harness == "sprig":
        self.app.set_status(
            f"attach: `sprig --resume {row.session_id} --session-dir {self.app.data_dir / 'sessions'}`"
        )
    else:
        self.app.set_status(f"attach: not on this path ({row.harness})")
```

`s` (steer) on a sprig row writes a `note` entry `{"from": "operator", "text": ...}` through
the command box (reuse `open_cmd`/`on_cmd_submitted` with a `_steering` marker); on a
Claude row it only sets the status `steer: not on this path`. If
`App.copy_to_clipboard` is absent in the installed Textual, print the command in the
status only.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_tui_honesty.py tests/test_tui.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tui_sessions.py src/opendaisugi/tui.py tests/test_tui_honesty.py
git commit -m "feat(tui): the mode is always on screen; attach and steer say what they can do on each path"
```

---

### Task 6: The tree screen: navigate, label, fork, rewind

**Files:**
- Modify: `src/opendaisugi/tui_tree.py`
- Test: `tests/test_tui_tree.py` (new)

**Interfaces:**
- Consumes: `SessionTree` (sprig sessions), `claude_transcript.read_turns` (Claude sessions), `checkpoints.restore`, `SessionTree.fork`, `SessionTree.set_head`.
- Produces: `TreeScreen` with `Tree#tree`, filters cycled by `ctrl+o` (`all`, `no tools`, `prompts`, `labeled`, `verdicts`), `L` label, `Enter` → `RewindMenu(ModalScreen)` with options `Restore conversation / Restore workspace / Restore both / Fork here / Never mind` (workspace rows only when a checkpoint exists on the path).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tui_tree.py
"""One session's tree: fold, filter, label, fork, rewind. Says what it will not restore."""

import asyncio
import time

import pytest

pytest.importorskip("textual")

from textual.widgets import Tree  # noqa: E402

from opendaisugi.session_tree import SessionIndex, SessionTree  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def _sprig_tree(tmp_path):
    t = SessionTree.create(
        tmp_path / "sessions", session_id="e1", harness="sprig", cwd=str(tmp_path)
    )
    p = t.append("prompt", {"text": "write hello"})
    a = t.append(
        "assistant",
        {
            "text": "ok",
            "model": "m",
            "usage": {"fresh": 1, "cacheRead": 0, "cacheWrite": 0, "out": 1},
        },
    )
    c = t.append(
        "tool_call", {"toolUseId": "x1", "name": "write", "detail": "hi.txt"}, parent_id=a.id
    )
    t.append("verdict", {"toolUseId": "x1", "decision": "allow", "clause": "ok"}, parent_id=c.id)
    return t, p, a, c


def _labels(tree: Tree) -> list[str]:
    out = []

    def walk(node):
        out.append(str(node.label))
        for ch in node.children:
            walk(ch)

    walk(tree.root)
    return out


def test_tree_renders_a_sprig_session_and_filters(tmp_path):
    _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "e1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            labels = _labels(app.query_one("#tree", Tree))
            assert any("write hello" in x for x in labels)
            assert any("write hi.txt" in x for x in labels)
            await pilot.press("ctrl+o")  # no tools
            await pilot.pause()
            labels = _labels(app.query_one("#tree", Tree))
            assert not any("write hi.txt" in x for x in labels)
            assert "filter: no tools" in app.status_text

    _run(scenario)


def test_tree_renders_a_claude_session_from_its_transcript(tmp_path):
    _claude_session(
        tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="toolu_01", decision="allow"
    )

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "c1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            labels = _labels(app.query_one("#tree", Tree))
            assert any("list files" in x for x in labels)
            assert any("try again" in x for x in labels)  # the branch from a1
            assert any("allow" in x.lower() and "toolu_01" in x for x in labels)  # joined verdict

    _run(scenario)


def test_label_writes_a_label_entry(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "e1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            tree = app.query_one("#tree", Tree)
            tree.select_node(tree.root.children[0])  # the prompt
            await pilot.press("L")
            await pilot.pause()
            app.query_one("#cmd").value = "good start"
            await pilot.press("enter")
            await pilot.pause()
            labels = [e for e in t.entries() if e.type == "label"]
            assert labels and labels[0].data == {"target": p.id, "label": "good start"}

    _run(scenario)


def test_fork_here_creates_a_child_session(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "e1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            tree = app.query_one("#tree", Tree)
            tree.select_node(tree.root.children[0].children[0])  # the assistant turn
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name != "tree"  # the rewind menu is up
            assert "Restore workspace" not in str(
                app.screen.query_one("#menu").renderable
            )  # no checkpoint
            await pilot.press("f")
            await pilot.pause()
            ids = [s.session_id for s in SessionIndex(tmp_path / "sessions").list()]
            child = next(i for i in ids if i.startswith("e1-"))
            assert SessionTree.open(tmp_path / "sessions", child).meta()["parentEntry"] == a.id
            assert "forked" in app.status_text and child in app.status_text

    _run(scenario)


def test_restore_conversation_moves_the_head(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "e1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            tree = app.query_one("#tree", Tree)
            tree.select_node(tree.root.children[0])  # the prompt
            await pilot.press("enter")
            await pilot.pause()
            await pilot.press("c")  # Restore conversation
            await pilot.pause()
            assert t.head() == p.id
            assert "workspace not restored" in app.status_text

    _run(scenario)


def test_rewind_menu_on_claude_session_offers_fork_command(tmp_path):
    _claude_session(
        tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="toolu_01", decision="allow"
    )

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        app.selected_session = "c1"
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("tree")
            await pilot.pause()
            tree = app.query_one("#tree", Tree)
            tree.select_node(tree.root.children[0])
            await pilot.press("enter")
            await pilot.pause()
            await pilot.press("f")
            await pilot.pause()
            assert "claude --resume c1 --fork-session" in app.status_text
            assert "rewind inside Claude with Esc Esc" in app.status_text

    _run(scenario)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_tui_tree.py -q`
Expected: FAIL (`#tree` not found).

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/tui_tree.py
"""One session's prompt tree. Rewind has two axes and says what it will not restore."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from textual.app import ComposeResult
from textual.binding import Binding
from textual.screen import ModalScreen, Screen
from textual.widgets import Static, Tree

from opendaisugi.claude_transcript import read_turns
from opendaisugi.session_tree import SessionTree

FILTERS = ("all", "no tools", "prompts", "labeled", "verdicts")
_TOOL_TYPES = {"tool_call", "verdict", "tool_result"}


@dataclass(frozen=True)
class Node:
    id: str
    parent: str | None
    kind: str
    label: str
    has_checkpoint: bool = False


def nodes_for_sprig(tree: SessionTree) -> list[Node]:
    labels = {
        e.data.get("target"): e.data.get("label") for e in tree.entries() if e.type == "label"
    }
    cps = {e.parent_id for e in tree.entries() if e.type == "checkpoint"}
    out = []
    for e in tree.entries():
        if not e.id or e.type in ("session", "label", "head", "checkpoint"):
            continue
        text = e.data.get("text") or " ".join(
            str(e.data.get(k, "")) for k in ("name", "detail") if e.data.get(k)
        )
        if e.type == "verdict":
            text = f"{e.data.get('decision', '')} {e.data.get('toolUseId', '')} · {e.data.get('clause', '')}"
        tag = f" [{labels[e.id]}]" if e.id in labels else ""
        out.append(Node(e.id, e.parent_id, e.type, f"{e.type}: {text[:60]}{tag}", e.id in cps))
    return out


def nodes_for_claude(transcript: Path, tree: SessionTree | None) -> list[Node]:
    verdicts = {}
    if tree is not None:
        for e in tree.entries():
            if e.type == "verdict" and e.data.get("toolUseId"):
                verdicts[e.data["toolUseId"]] = e.data
    out = []
    for t in read_turns(transcript):
        kind = "prompt" if (t.kind == "user" and not t.tool_result_ids) else t.kind
        out.append(Node(t.uuid, t.parent_uuid, kind, f"{kind}: {t.text[:60]}"))
        for u in t.tool_uses:
            v = verdicts.get(u["id"])
            vtxt = (
                f"{v.get('decision')} {u['id']} · {v.get('clause', '')}"
                if v
                else "(no verdict recorded)"
            )
            out.append(Node(f"{t.uuid}/{u['id']}", t.uuid, "verdict", f"{u['name']} → {vtxt}"))
    return out


class RewindMenu(ModalScreen[str]):
    BINDINGS = [
        Binding("c", "pick('conversation')", "c conversation"),
        Binding("w", "pick('workspace')", "w workspace"),
        Binding("b", "pick('both')", "b both"),
        Binding("f", "pick('fork')", "f fork"),
        Binding("escape,n", "pick('never')", "n never mind"),
    ]

    def __init__(self, node: Node, *, workspace_ok: bool, skipped: list[str]) -> None:
        super().__init__()
        self.node, self.workspace_ok, self.skipped = node, workspace_ok, skipped

    def compose(self) -> ComposeResult:
        rows = [f"Rewind to: {self.node.label}", "", "c  Restore conversation (move the head here)"]
        if self.workspace_ok:
            rows += ["w  Restore workspace (a rollback ref is taken first)", "b  Restore both"]
            if self.skipped:
                rows.append(f"   will NOT restore: {', '.join(self.skipped[:5])}")
        rows += ["f  Fork here (new session, this path copied)", "n  Never mind"]
        yield Static("\n".join(rows), id="menu")

    def action_pick(self, choice: str) -> None:
        self.dismiss(choice)


class TreeScreen(Screen):
    BINDINGS = [
        Binding("ctrl+o", "cycle_filter", "^O filter", show=True),
        Binding("L", "label", "L label", show=True),
        Binding("enter", "rewind", "⏎ rewind/fork", show=True),
    ]

    def __init__(self) -> None:
        super().__init__()
        self._filter = 0
        self._nodes: list[Node] = []

    def compose(self) -> ComposeResult:
        yield Static("", id="tree-title")
        yield Tree("session", id="tree")

    def on_screen_resume(self) -> None:
        self.reload()

    on_mount = on_screen_resume

    # --- data ---------------------------------------------------------------
    def _session(self) -> SessionTree | None:
        sid = self.app.selected_session
        if not sid:
            return None
        try:
            return SessionTree.open(self.app.data_dir / "sessions", sid)
        except FileNotFoundError:
            return None

    def reload(self) -> None:
        tree_w = self.query_one("#tree", Tree)
        tree_w.clear()
        st = self._session()
        if st is None:
            self.query_one("#tree-title", Static).update("no session selected · press t on a row")
            return
        meta = st.meta()
        self.query_one("#tree-title", Static).update(
            f"{st.session_id} · {meta.get('harness')} · filter: {FILTERS[self._filter]}"
        )
        if meta.get("harness") == "claude-code" and meta.get("transcriptPath"):
            self._nodes = nodes_for_claude(Path(meta["transcriptPath"]), st)
        else:
            self._nodes = nodes_for_sprig(st)
        shown = [n for n in self._nodes if self._visible(n)]
        by_parent: dict[str | None, list[Node]] = {}
        for n in shown:
            by_parent.setdefault(
                n.parent if any(m.id == n.parent for m in shown) else None, []
            ).append(n)

        def add(parent_widget, pid):
            for n in by_parent.get(pid, []):
                w = parent_widget.add(n.label, data=n, expand=True)
                add(w, n.id)

        add(tree_w.root, None)
        tree_w.root.expand()

    def _visible(self, n: Node) -> bool:
        f = FILTERS[self._filter]
        return (
            f == "all"
            or (f == "no tools" and n.kind not in _TOOL_TYPES)
            or (f == "prompts" and n.kind == "prompt")
            or (f == "labeled" and n.label.endswith("]"))
            or (f == "verdicts" and n.kind == "verdict")
        )

    # --- actions ------------------------------------------------------------
    def action_cycle_filter(self) -> None:
        self._filter = (self._filter + 1) % len(FILTERS)
        self.reload()
        self.app.set_status(f"filter: {FILTERS[self._filter]}")

    def _selected(self) -> Node | None:
        node = self.query_one("#tree", Tree).cursor_node
        return node.data if node is not None and isinstance(node.data, Node) else None

    def action_label(self) -> None:
        n = self._selected()
        if n is None:
            self.app.set_status("select an entry to label")
            return
        self._labeling = n
        self.app.open_cmd(prefill="")

    def on_cmd_submitted(self, value: str) -> bool:
        n = getattr(self, "_labeling", None)
        if not n:
            return False
        self._labeling = None
        st = self._session()
        if st is not None and "/" not in n.id:
            st.append("label", {"target": n.id, "label": value})
            self.app.set_status(f"labeled {n.id}: {value}")
            self.reload()
        else:
            self.app.set_status(
                "labels on Claude transcript rows are kept in daisugi only (not yet)"
            )
        return True

    def action_rewind(self) -> None:
        n = self._selected()
        st = self._session()
        if n is None or st is None:
            return
        cps = [e for e in st.entries() if e.type == "checkpoint"]
        on_path = (
            [e for e in cps if "/" not in n.id and e.parent_id in {x.id for x in st.path_to(n.id)}]
            if st.meta().get("harness") != "claude-code"
            else cps
        )
        cp = on_path[-1] if on_path else None
        self.app.push_screen(
            RewindMenu(
                n,
                workspace_ok=cp is not None,
                skipped=list((cp.data.get("skipped") if cp else []) or []),
            ),
            callback=lambda choice: self._apply(choice, n, cp),
        )

    def _apply(self, choice: str, n: Node, cp) -> None:
        st = self._session()
        if st is None or choice == "never":
            return
        meta = st.meta()
        if meta.get("harness") == "claude-code":
            if choice == "fork":
                head = st.head()
                child = (
                    st.fork(head) if head else None
                )  # daisugi's own entries; Claude owns the prompts
                self.app.set_status(
                    f"fork: run `claude --resume {st.session_id} --fork-session` "
                    f"(daisugi registered {child.session_id if child else 'no child'}); "
                    "rewind inside Claude with Esc Esc"
                )
            else:
                self.app.set_status(
                    "rewind inside Claude with Esc Esc; daisugi sees the new head on the next tool call"
                )
            return
        if choice in ("conversation", "both"):
            st.set_head(n.id)
        if choice in ("workspace", "both") and cp is not None:
            from opendaisugi.checkpoints import restore

            rb = restore(
                Path(meta["cwd"]), ref=cp.data["ref"], session_id=st.session_id, entry_id=n.id
            )
            self.app.set_status(f"workspace restored to {cp.data['ref']} · rollback at {rb.ref}")
        if choice == "conversation":
            self.app.set_status(f"head moved to {n.id} · workspace not restored")
        if choice == "fork":
            child = st.fork(n.id)
            self.app.set_status(
                f"forked {st.session_id} at {n.id} → {child.session_id} · "
                f"resume with `sprig --resume {child.session_id}`"
            )
        self.reload()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_tui_tree.py tests/test_tui_app.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tui_tree.py tests/test_tui_tree.py
git commit -m "feat(tui): the tree screen with filters, labels, fork, and a two-axis rewind that says what it skips"
```

---

### Task 7: `--serve` forwards the interval; docs and the plan file

**Files:**
- Modify: `src/opendaisugi/cli.py:2833-2889` (`dashboard --serve` passes `interval`)
- Modify: `docs/reference/module-wiring.md` (or wherever the TUI is documented: `grep -rl "dashboard --tui" docs`)
- Modify: `docs/plans/2026-08-26-daisugi-interface-two-lenses.md` (W8, W5, W7 shipped notes)

- [ ] **Step 1: Forward the interval and update the docs**

`serve_gui(data_dir, host=host, port=port, interval=interval)`. In the docs, describe
the three views, the key table (`a d e r s t ⏎ Tab : ? q`, `ctrl+o L` on the tree), the
grouping rule, the header fields and their sources, the ask flow and its deadline, and
the per-path capability table from the spec §7.

- [ ] **Step 2: Mark W8, W5, W7 shipped in the plan file**

Under W8: "**Shipped (plan 4):** the TUI opens on the multi-session view; wiring is
`:wiring`/Tab; tree view with fork and two-axis rewind." Under W5: "the header shows
`gate: MODE (source)` at all times; `gate status` prints it." Under W7: "the `[live]/
[cfg]/[planned]` tags survive on the wiring screen (tested)."

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/cli.py docs/ tests/
git commit -m "docs(tui): describe the three views and their keys; --serve keeps the interval"
```

---

## Self-review

- Spec coverage (§7): screens and header (T2), roster + peek + in-place refresh (T3), keys and presence (T4), honesty per path and wiring tags (T5), tree with filters/labels/fork/rewind and the skipped list (T6), serve/docs (T7). Alerts strip and policy file (T1 + T3). Tag-then-verb (`Space` tags, then `d`/`a` on the set) is not built; it is a follow-on once single-row answering has been used for real.
- Placeholders: none; every test has code and every step names its file and lines.
- Types: `SessionRow.pending_ask["toolUseId"]` matches `ask.post_ask`'s body. `app.open_cmd(prefill)` and `screen.on_cmd_submitted(value) -> bool` are used the same way in T4 and T6. `RewindMenu` dismisses with one of `conversation|workspace|both|fork|never`, and `_apply` handles exactly those.
