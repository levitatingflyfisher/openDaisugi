"""The chrome every cockpit screen shares: the mode header, the status line,
and the ``:`` command line — composed on the SCREEN, not the App.

B1 (adversarial review, 2026-08-27): ``App.query()`` resolves only against the
App's ``_default`` screen, and a pushed ``Screen`` is opaque to it. Composing the
header / gate-mode indicator / command line onto the App would (a) hide the
always-visible gate-mode indicator behind the opaque pushed screen — a *safety*
failure, not cosmetics — and (b) make every ``app.query("#roster"/"#hdr"/…)``
return ``NoMatches``. So the chrome lives here, on a base ``Screen`` inherited by
``SessionsScreen`` / ``TreeScreen`` / ``WiringScreen``, and every test queries
``app.screen.query_one(…)``.

Each screen sets its own ``name`` via ``super().__init__(name=…)``: Textual
8.2.8's ``SCREENS`` registry does NOT name a screen it instantiates by key
(verified — ``app.screen.name`` is ``None`` without this), yet the view switch is
keyed on ``app.screen.name``.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.screen import Screen
from textual.widgets import Footer, Header, Input, Static

_CMD_PLACEHOLDER = "sessions · tree <id> · wiring · gate enforce|shadow"
_STATUS_DEFAULT = "Tab next view · : command · ? keys · q quit"


class CockpitScreen(Screen):
    """Header (#hdr) + status (#status) + command line (#cmd), plus a body.

    Subclasses implement :meth:`compose_body`; the shared widgets are yielded
    around it here so the mode indicator is present on every screen.
    """

    def compose(self) -> ComposeResult:
        yield Header(show_clock=True)
        yield Static("", id="hdr")
        yield from self.compose_body()
        yield Input(placeholder=_CMD_PLACEHOLDER, id="cmd")
        yield Static(_STATUS_DEFAULT, id="status")
        yield Footer()

    def compose_body(self) -> ComposeResult:
        return iter(())

    # --- lifecycle: the header refreshes on first mount AND on every re-show,
    # so switching to any screen shows the current gate mode. One-time setup
    # (columns, timers) belongs in a subclass ``on_mount`` that calls
    # ``super().on_mount()``; repeat work in ``on_screen_resume`` that calls
    # ``super().on_screen_resume()``. We never alias the two — a subclass that
    # did would shadow this refresh and drop the mode indicator (B1's failure
    # returning by the back door).
    def on_mount(self) -> None:
        self.refresh_header()

    def on_screen_resume(self) -> None:
        self.refresh_header()

    def refresh_header(self, roster=None) -> None:
        from opendaisugi.cockpit import build_roster, header_state

        try:
            hdr = self.query_one("#hdr", Static)
        except Exception:  # noqa: BLE001 — the header must never crash a screen
            return
        roster = roster if roster is not None else build_roster(self.app.data_dir)
        h = header_state(self.app.data_dir, roster=roster)
        if h.cache_hit_rate is None:
            cache = "cache —"
        else:
            # S5: a gateway-derived (blended) rate is an estimate and says so;
            # a live per-session rate from this run's own transcripts does not.
            mark = " ?" if h.cache_is_estimate else ""
            cache = f"cache {h.cache_hit_rate:.0%} hit{mark} ({h.cache_source})"
        name = self.name or "sessions"
        hdr.update(
            f" daisugi · {name:<9} gate: {h.gate_mode} ({h.gate_mode_source})   "
            f"{cache}   {h.session_count} sessions   ?=keys"
        )

    # --- the shared command line ------------------------------------------
    def on_cmd_submitted(self, value: str) -> bool:
        """Screen-specific command handling (an edit/label/steer prefill, or a
        wiring swap). Return ``True`` when consumed; ``False`` lets the App's
        global command parser (view switch / gate / quit) take it."""
        return False

    def clear_prefill(self) -> None:
        """Drop any pending prefill marker (edit/label/steer). Called when the
        operator CANCELS the command line so the next ``:`` command is not
        swallowed by a stale handler."""
