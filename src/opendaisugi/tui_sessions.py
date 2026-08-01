"""The sessions screen: every session's current action and verdict, grouped by
what the operator must do. Rows update in place; nothing reflows under the eye.

Membership/group changes rebuild the table (a row crossing WORKING→PARKED
legitimately regroups); cell values update in place via ``DataTable.update_cell``
(spec §7.2, reconciled by the S6 correction).
"""

from __future__ import annotations

import time

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.screen import ModalScreen
from textual.widgets import DataTable, Static

from opendaisugi.cockpit import (
    GROUPS,
    PARKED_S,
    SHELL_TOOL_NAMES,
    WORKING_S,
    Roster,
    SessionRow,
    build_roster,
    is_destructive_action,
)
from opendaisugi.session_tree import SessionIndex, SessionTree
from opendaisugi.tui_base import CockpitScreen

_COLS = ("session", "action", "verdict", "clause", "steps", "↑fresh", "⟳read", "✎write", "age")


def _age(s: float) -> str:
    return f"{int(s)}s" if s < 60 else f"{int(s // 60)}m" if s < 3600 else f"{s / 3600:.1f}h"


def _k(n: int) -> str:
    return f"{n / 1000:.1f}k" if n >= 1000 else str(n)


def _cells(r: SessionRow) -> list[str]:
    return [f"  {r.session_id[:14]}", r.action[:36], r.verdict, r.clause[:30], str(r.steps),
            _k(r.fresh), _k(r.cache_read), _k(r.cache_write), _age(r.age_s)]


class SessionsScreen(CockpitScreen):
    BINDINGS = [
        Binding("down,j", "cursor_down", "▼", show=True),
        Binding("up,k", "cursor_up", "▲", show=True),
        Binding("a", "arm_allow", "a allow", show=True),
        Binding("d", "deny", "d deny", show=True),
        Binding("e", "edit_input", "e edit", show=True),
        Binding("r", "remember", "r remember", show=True),
        Binding("s", "steer", "s steer", show=True),
        Binding("t", "tree", "t tree", show=True),
    ]

    def __init__(self) -> None:
        super().__init__(name="sessions")
        self._roster: Roster | None = None
        self._rows: dict[str, SessionRow] = {}
        self._keys: dict[str, object] = {}  # session_id -> row key
        self._col_keys: list[object] = []
        self._last_mtime = -1.0
        self._table_generation = 0
        self._roster_at = 0.0
        self._armed: str | None = None  # low-blast a->Enter: the armed toolUseId
        # What a #cmd submit means on this screen, or None. One of:
        #   ("edit", tid, key)          — e: narrow-then-deny-and-re-ask (S3)
        #   ("allow_token", tid, token) — a on a destructive would-deny (S2)
        #   ("steer", session_id)       — s on a sprig session (S4)
        self._cmd_mode: tuple | None = None

    def compose_body(self) -> ComposeResult:
        with Vertical():
            yield DataTable(id="roster", cursor_type="row", zebra_stripes=False)
            yield Static("", id="alerts")
            yield Static("", id="peek")

    def on_mount(self) -> None:
        super().on_mount()  # the shared header
        table = self.query_one("#roster", DataTable)
        self._col_keys = list(table.add_columns(*_COLS))
        self.reload(force=True)
        self.set_interval(1.0, self._poll)

    def clear_prefill(self) -> None:
        # Cancelling the command line drops the pending intent AND any low-blast
        # arm, so the next key or command is never swallowed by a stale handler.
        self._cmd_mode = None
        self._armed = None

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
        self._roster_at = time.time()  # S6: set here so _refresh_ages never AttributeErrors
        self._render_alerts()
        self.render_peek(self.selected_row)
        self.refresh_header(roster=roster)

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
            if r.session_id not in self._keys:
                continue  # a _roster/_keys disagreement is a no-op, never a KeyError
            age = r.age_s + delta
            crossed = ((r.group == "WORKING" and age >= WORKING_S)
                       or (r.group == "PARKED" and age >= PARKED_S))
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
        if not key or not str(key).startswith("row:"):
            return None
        return self._rows.get(str(key)[4:])

    def on_data_table_row_highlighted(self, _event) -> None:
        # S2: any cursor move disarms an armed allow — a poll re-sort or a
        # fat-fingered move can never carry an arm onto a different row.
        self._armed = None
        self.render_peek(self.selected_row)

    def render_peek(self, row: SessionRow | None) -> None:
        peek = self.query_one("#peek", Static)
        if row is None:
            peek.update("select a session row · j/k move")
            return
        v = row.last_verdict or {}
        lines = [f"{row.session_id} · {row.agent} · {row.harness} · {row.cwd}",
                 f"proposes {row.action or '(nothing yet)'}",
                 f"verdict {row.verdict or '—'} · clause {row.clause or '—'} · "
                 f"envelope {v.get('envelopeId') or '—'} · {v.get('latencyMs', '—')} ms"]
        if v.get("counterexample"):
            lines.append(f"counterexample: {v['counterexample']}")
        if row.pending_ask:
            left = int(float(row.pending_ask.get("deadline", 0)) - time.time())
            lines.append(f"ASK pending · {max(left, 0)}s left · reason: {row.pending_ask.get('reason', '')}")
        # S4: steer is only bound+implemented on the sprig path; on Claude it is
        # marked, never dressed as live (tool-interface law 10). `e` edit bounces
        # back as a deny-with-reason (S3) — the narrowing is UNVERIFIED against
        # the harness, so it never becomes a silent allow of the original.
        steer = "s steer" if row.harness == "sprig" else "s steer (not on this path)"
        lines.append("a allow (then ⏎)  d deny  e edit→deny&re-ask (narrowing unverified)  "
                     f"r remember…  {steer}  t tree  ⏎ attach")
        peek.update("\n".join(lines))

    def _render_alerts(self) -> None:
        from opendaisugi.cockpit import alerts_for, load_alert_policy

        alerts = (alerts_for(self._roster, policy=load_alert_policy(self.app.data_dir))
                  if self._roster else [])
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

    # --- answering an ask ---------------------------------------------------
    def _pending(self) -> tuple[SessionRow, str] | None:
        row = self.selected_row
        if row is None or not row.pending_ask:
            self.app.set_status("no pending ask on this row")
            return None
        return row, str(row.pending_ask["toolUseId"])

    def _ask_action_text(self, row: SessionRow) -> str:
        """S2: build the string we classify from the PENDING ask (toolName + the
        values of toolInput), not from ``row.action`` — the latter is the last
        *recorded* call and can lag the ask still awaiting an answer, so a
        destructive pending call would otherwise get the low-effort two-step."""
        a = row.pending_ask or {}
        parts = [str(a.get("toolName") or "")]
        ti = a.get("toolInput")
        if isinstance(ti, dict):
            parts += [str(v) for v in ti.values()]
        return " ".join(p for p in parts if p).strip() or row.action

    def _needs_strong_guard(self, row: SessionRow) -> bool:
        """True when this allow must use the typed-token confirm, not a->Enter.

        High-blast is decided at the point of use, not by chasing every command
        string. It is high-blast when ANY of:
        - the tool is a RAW SHELL tool (SHELL_TOOL_NAMES) — its blast radius is
          "whatever the string does", so dd/mkfs/chmod -R/git clean/find -delete/
          a truncating redirect/… all land here whether or not a pattern catches
          them;
        - ``toolInput`` is not a clean dict — we can't inspect the call, so we
          can't call it low-blast (this also closes the minor fail-open where a
          non-dict input was classified on ``toolName`` alone);
        - ``is_destructive_action`` matches the ask's text (defence in depth, and
          it covers a destructive command issued through a NON-shell tool)."""
        a = row.pending_ask or {}
        tool = str(a.get("toolName") or "").strip().casefold()
        if tool in SHELL_TOOL_NAMES:
            return True
        if not isinstance(a.get("toolInput"), dict):
            return True
        return is_destructive_action(self._ask_action_text(row))

    def action_arm_allow(self) -> None:
        p = self._pending()
        if not p:
            return
        row, tid = p
        if self._needs_strong_guard(row):
            # S2 (habituation-resistant): a fixed a->Enter would go invisible on a
            # high-blast would-deny. Match the guard to the blast radius — require
            # a token that VARIES per row (the session id, not the near-constant
            # tool name "Bash") typed to confirm, so the operator's locus of
            # attention lands on THIS specific action and the reflex can't allow.
            token = row.session_id
            self._armed = None
            self._cmd_mode = ("allow_token", tid, token)
            self.app.open_cmd(prefill="")
            self.app.set_status(
                f"HIGH-BLAST allow — type the session id '{token}' then ⏎ to confirm allow of {tid}"
            )
        else:
            # Low-blast: the low-effort two-step. a arms, ⏎ confirms, any cursor
            # move disarms (on_data_table_row_highlighted).
            self._armed = tid
            self._cmd_mode = None
            self.app.set_status(f"allow {tid}? ⏎ to confirm allow · move the cursor to cancel")

    def on_data_table_row_selected(self, _event) -> None:
        # Enter on the focused roster (the table owns the enter key) → confirm an
        # armed allow, else attach.
        self.action_confirm()

    def action_confirm(self) -> None:
        from opendaisugi import ask

        row = self.selected_row
        # S2 (misfire-safe): the armed id must still equal the CURRENTLY selected
        # row's toolUseId — a poll re-sort can never misfire onto another row.
        if (self._armed and row and row.pending_ask
                and str(row.pending_ask["toolUseId"]) == self._armed):
            ask.answer(self.app.data_dir / "gate", tool_use_id=self._armed, decision="allow",
                       reason="operator allowed from the sessions view")
            self.app.set_status(f"allowed {self._armed}")
            self._armed = None
            self.reload()
            return
        self._armed = None
        self.action_attach()

    def action_deny(self) -> None:
        p = self._pending()
        if not p:
            return
        from opendaisugi import ask

        _, tid = p
        ask.answer(self.app.data_dir / "gate", tool_use_id=tid, decision="deny",
                   reason="operator denied from the sessions view")
        self.app.set_status(f"denied {tid}")
        self.reload()

    def action_edit_input(self) -> None:
        p = self._pending()
        if not p:
            return
        row, tid = p
        tool_input = (row.pending_ask or {}).get("toolInput") or {}
        key = next((k for k in ("command", "file_path", "path", "url") if k in tool_input), None)
        if key is None:
            self.app.set_status("this input has no editable field")
            return
        self._cmd_mode = ("edit", tid, key)
        self.app.open_cmd(prefill=str(tool_input[key]))
        self.app.set_status(f"edit {key}, then ⏎ — this DENIES and re-asks (narrowing unverified)")

    def action_remember(self) -> None:
        p = self._pending()
        if not p:
            return
        row, tid = p
        self.app.push_screen(RememberScope(), callback=lambda scope: self._propose(row, tid, scope))

    def _propose(self, row: SessionRow, tid: str, scope: str | None) -> None:
        if scope is None:
            self.app.set_status("remember cancelled")
            return
        from opendaisugi import ask

        ask.propose(self.app.data_dir / "gate", kind="allow-pattern", scope=scope,
                    expires_at=time.time() + 30 * 86400,
                    body={"sessionId": row.session_id, "toolUseId": tid,
                          "toolInput": (row.pending_ask or {}).get("toolInput"), "clause": row.clause})
        self.app.set_status(f"proposal written (scope {scope}) · apply it with `daisugi gate proposals`")

    def action_steer(self) -> None:
        # S4: only bound to a real effect on the sprig path; on Claude it says so
        # in place rather than being dressed as live.
        row = self.selected_row
        if row is None:
            self.app.set_status("no session selected")
            return
        if row.harness != "sprig":
            self.app.set_status(f"steer: not on this path ({row.harness})")
            return
        self._cmd_mode = ("steer", row.session_id)
        self.app.open_cmd(prefill="")
        self.app.set_status(f"steer note for {row.session_id}, then ⏎")

    def action_attach(self) -> None:
        # Honesty per path: on Claude, attach IS the exact resume command (copied,
        # when the terminal supports it); on sprig, the resume invocation; on any
        # other harness, it says plainly that attach is not on that path.
        row = self.selected_row
        if row is None:
            self.app.set_status("no session selected")
            return
        if row.harness == "claude-code":
            # Resume by the REAL Claude uuid when present. Under `daisugi start`
            # the tree is keyed by a per-cwd pin, not the uuid, so `session_id` is
            # the pin key — a dead `claude --resume` argument. `harness_session_id`
            # carries the uuid; fall back to `session_id` for a uuid-keyed session
            # (e.g. `daisugi install --gate`, which has no pin).
            resume_id = row.harness_session_id or row.session_id
            cmd = f"claude --resume {resume_id}"
            copied = ""
            try:
                self.app.copy_to_clipboard(cmd)
                copied = " (copied)"
            except Exception:  # noqa: BLE001 — a terminal without clipboard still shows the command
                pass
            self.app.set_status(f"attach: run `{cmd}` in {row.cwd or 'its directory'}{copied}")
        elif row.harness == "sprig":
            sessions = self.app.data_dir / "sessions"
            self.app.set_status(
                f"attach: `sprig --resume {row.session_id} --session-dir {sessions}`"
            )
        else:
            self.app.set_status(f"attach: not on this path ({row.harness})")

    def on_cmd_submitted(self, value: str) -> bool:
        from opendaisugi import ask

        mode = self._cmd_mode
        if mode is None:
            return False
        self._cmd_mode = None
        kind = mode[0]
        if kind == "edit":
            _, tid, key = mode
            # S3 (fail-closed): the harness's honoring of updatedInput is
            # UNVERIFIED (spec §10). Rather than a silent allow of the original,
            # deny with a reason that hands the narrower call back to the agent.
            ask.answer(self.app.data_dir / "gate", tool_use_id=tid, decision="deny",
                       reason=f"operator asks to narrow {key} to: {value!r} — re-issue it "
                       "(daisugi cannot verify an in-place edit is honored on this harness)")
            self.app.set_status(f"edit → DENIED {tid}; agent asked to re-issue as {value!r} "
                                "(narrowing unverified)")
            self.reload()
            return True
        if kind == "allow_token":
            _, tid, token = mode
            row = self.selected_row
            if (row is None or not row.pending_ask
                    or str(row.pending_ask["toolUseId"]) != tid or value.strip() != token):
                self.app.set_status(f"allow cancelled — token did not match '{token}'")
                return True
            ask.answer(self.app.data_dir / "gate", tool_use_id=tid, decision="allow",
                       reason="operator allowed a destructive action (typed-token confirm)")
            self.app.set_status(f"allowed {tid} (destructive · token confirmed)")
            self.reload()
            return True
        if kind == "steer":
            _, sid = mode
            try:
                SessionTree.open(self.app.data_dir / "sessions", sid).append(
                    "note", {"from": "operator", "text": value})
                self.app.set_status(f"steer note added to {sid}")
                self.reload()
            except FileNotFoundError:
                self.app.set_status(f"steer: session {sid} not found")
            return True
        return False


class RememberScope(ModalScreen[str]):
    """A modal scope picker: its keys can't leak into the roster, and the
    two-step allow is only ever disarmed by cursor movement, never by a key
    handler firing from under the table."""

    BINDINGS = [Binding("o", "pick('once')", "o once"), Binding("s", "pick('session')", "s session"),
                Binding("p", "pick('project')", "p project"), Binding("g", "pick('global')", "g global"),
                Binding("escape", "pick_none", "Esc cancel")]

    def compose(self) -> ComposeResult:
        yield Static("remember this allow for:  o once   s session   p project   g global   Esc cancel",
                     id="scope")

    def action_pick(self, scope: str) -> None:
        self.dismiss(scope)

    def action_pick_none(self) -> None:
        self.dismiss(None)
