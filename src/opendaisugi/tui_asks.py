"""Answering a pending ask, as a mixin any cockpit screen with a row can use.

The behaviour is the sessions screen's, unchanged: a low-blast allow is the
two-step (arm, then Enter, disarmed by any cursor move); a high-blast one needs
the session id typed back. It lives here so a second screen inherits it rather
than growing its own drifting copy of a safety guard.

A screen using this mixin supplies `selected_row -> SessionRow | None`, owns
`self._armed` and `self._cmd_mode`, and sets `_ask_origin` to the phrase that lands in
the audit reason, such as "sessions view". The reason string is a safety-audit
record, so it keeps the wording each screen already wrote rather than drifting to one
generic sentence.
"""

from __future__ import annotations

from opendaisugi.cockpit import SHELL_TOOL_NAMES, SessionRow, is_destructive_action


class AskActionsMixin:
    """`a` / `d` / the typed-token confirm, for any screen with a selected row."""

    _armed: str | None
    _cmd_mode: tuple | None
    _ask_origin: str = "cockpit"

    @property
    def selected_row(self) -> SessionRow | None:  # pragma: no cover - supplied by the screen
        raise NotImplementedError

    def _no_row_reason(self) -> str:
        """Why selected_row is None, for a screen that can tell the reason
        apart from a plain "nothing pending". A screen whose rows can lack
        a session id overrides this to say so."""
        return "no pending ask on this row"

    def _pending(self) -> tuple[SessionRow, str] | None:
        row = self.selected_row
        if row is None:
            self.app.set_status(self._no_row_reason())
            return None
        if not row.pending_ask:
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

    def action_confirm_allow(self) -> bool:
        """Answer an armed allow. True when it fired, False when nothing was armed."""
        from opendaisugi import ask

        # S2 (misfire-safe): the armed id must still equal the CURRENTLY selected
        # row's toolUseId — a poll re-sort can never misfire onto another row.
        row = self.selected_row
        if (
            self._armed
            and row
            and row.pending_ask
            and str(row.pending_ask["toolUseId"]) == self._armed
        ):
            ask.answer(
                self.app.data_dir / "gate",
                tool_use_id=self._armed,
                decision="allow",
                reason=f"operator allowed from the {self._ask_origin}",
            )
            self.app.set_status(f"allowed {self._armed}")
            self._armed = None
            self.reload()
            return True
        self._armed = None
        return False

    def action_deny(self) -> None:
        p = self._pending()
        if not p:
            return
        from opendaisugi import ask

        _, tid = p
        ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=tid,
            decision="deny",
            reason=f"operator denied from the {self._ask_origin}",
        )
        self.app.set_status(f"denied {tid}")
        self.reload()

    def handle_allow_token(self, mode: tuple, value: str) -> bool:
        """The `allow_token` branch of a `#cmd` submit. True when consumed."""
        from opendaisugi import ask

        _, tid, token = mode
        row = self.selected_row
        if (
            row is None
            or not row.pending_ask
            or str(row.pending_ask["toolUseId"]) != tid
            or value.strip() != token
        ):
            self.app.set_status(f"allow cancelled — token did not match '{token}'")
            return True
        ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=tid,
            decision="allow",
            reason="operator allowed a destructive action (typed-token confirm)",
        )
        self.app.set_status(f"allowed {tid} (destructive · token confirmed)")
        self.reload()
        return True
