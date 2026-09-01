"""The operator's instrument: three screens over one store.

``sessions`` (default): every session, its action, verdict, cost, and what the
operator must do. ``tree``: one session's prompt tree. ``wiring``: the module
map and swap knobs (yesterday's dashboard), reachable by ``:wiring`` or Tab,
never first. The floor, every pane and its state with a live attach, is the
coppice binary: ``daisugi coppice floor`` execs it.

B1: the header / gate-mode indicator / status / ``:`` command line are composed
on each SCREEN via :class:`~opendaisugi.tui_base.CockpitScreen`, not on the App —
a pushed screen is opaque to ``App.query`` and the always-visible mode indicator
would otherwise hide behind it (a safety failure). The App owns only the screen
registry, the global keys, the command router, and the presence heartbeat.
"""

from __future__ import annotations

import shlex
import sys
from pathlib import Path

from opendaisugi.modules import detect_stages

try:  # Textual is an optional extra; import lazily so the core CLI never needs it.
    from textual.app import App
    from textual.binding import Binding
    from textual.widgets import Input

    _HAVE_TEXTUAL = True
except ImportError:  # pragma: no cover - exercised only where the extra is absent
    _HAVE_TEXTUAL = False


class TextualNotInstalled(RuntimeError):
    """Raised when the GUI is requested without the [tui] extra installed."""


if _HAVE_TEXTUAL:
    from textual.actions import SkipAction

    from opendaisugi.tui_sessions import SessionsScreen
    from opendaisugi.tui_tree import TreeScreen
    from opendaisugi.tui_wiring import WiringScreen, swap_confirmation

    _ORDER = ("sessions", "tree", "wiring")

    # `:floor <option>` is an alias for the `floor_backend` swap stage. `:floor`
    # alone, with no option, names the coppice binary, where the floor lives now.
    # `:gate <option>` keeps its own stage key, unchanged.
    _COMMAND_STAGE_ALIAS = {"gate": "gate", "floor": "floor_backend"}

    FLOOR_MOVED = "The floor lives in the coppice binary. Run: daisugi coppice floor"

    class DaisugiApp(App):
        """The multi-session view: three screens, keyed switching, one store."""

        TITLE = "daisugi"
        SCREENS = {
            "sessions": SessionsScreen,
            "tree": TreeScreen,
            "wiring": WiringScreen,
        }
        CSS = """
        #hdr    { dock: top; height: 1; padding: 0 1; background: $panel; }
        #status { dock: bottom; height: 1; padding: 0 1; background: $panel; }
        #cmd    { dock: bottom; display: none; border: tall $accent; }
        """
        BINDINGS = [
            # priority=True so Tab cycles views instead of being eaten by the
            # focus system (Tab is Textual's default focus-next key).
            Binding("tab", "cycle", "next view", show=True, priority=True),
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
            self._stages = detect_stages(self.data_dir)  # resolve the map once
            self.status_text = ""
            self.selected_session: str | None = None

        # --- lifecycle -----------------------------------------------------
        def on_mount(self) -> None:
            self.push_screen("sessions")
            self._heartbeat()  # presence exists from the first frame
            self.set_interval(5.0, self._heartbeat)

        def on_unmount(self) -> None:
            from opendaisugi import ask

            try:
                ask.clear_presence(self.data_dir / "gate")
            except OSError:
                pass

        def _heartbeat(self) -> None:
            from opendaisugi import ask

            try:
                ask.write_presence(self.data_dir / "gate")
            except OSError:
                pass

        # --- the header (delegated to the active screen) -------------------
        def refresh_header(self, roster=None) -> None:
            screen = self.screen
            if hasattr(screen, "refresh_header"):
                screen.refresh_header(roster=roster)

        # --- view switching ------------------------------------------------
        def _passthrough(self) -> bool:
            """True while a screen owns every key, e.g. full-screen attach.

            The App's `tab` binding is priority=True, so it fires BEFORE the screen's,
            and `App._dispatch_action` counts an action as handled the moment it is
            invoked: a plain `return` still eats the key. Every caller therefore
            raises `SkipAction`, which is the one signal that makes the dispatcher
            report the key unhandled and let it reach the screen. Verified on textual
            8.2.8: with `return`, `AttachScreen.on_key` sees `['q', 'colon',
            'question_mark', 'h']`; with `SkipAction`, it sees `tab` as well.
            """
            return bool(getattr(self.screen, "passthrough", False))

        def action_cycle(self) -> None:
            if self._passthrough():
                raise SkipAction()
            cur = self.screen.name or "sessions"
            nxt = _ORDER[(_ORDER.index(cur) + 1) % len(_ORDER)] if cur in _ORDER else "sessions"
            self.switch_screen(nxt)

        # --- the command line ----------------------------------------------
        def action_cmd(self) -> None:
            if self._passthrough():
                raise SkipAction()
            self.open_cmd()

        def open_cmd(self, prefill: str = "") -> None:
            box = self.screen.query_one("#cmd", Input)
            box.display = True
            box.value = prefill
            box.focus()

        def action_hide_cmd(self) -> None:
            try:
                self.screen.query_one("#cmd", Input).display = False
            except Exception:  # noqa: BLE001
                pass
            # Cancelling must drop any pending prefill marker so the operator's
            # NEXT command isn't swallowed by a stale edit/label/steer handler.
            if hasattr(self.screen, "clear_prefill"):
                self.screen.clear_prefill()

        def action_help(self) -> None:
            if self._passthrough():
                raise SkipAction()
            keys = ", ".join(
                f"{b.key}={b.description}" for b in self.screen.BINDINGS if getattr(b, "show", True)
            )
            self.set_status(f"keys: {keys} · Tab next view · : command · q quit")

        def action_quit(self) -> None:
            if self._passthrough():
                raise SkipAction()
            self.exit()

        def on_input_submitted(self, event: "Input.Submitted") -> None:
            if event.input.id != "cmd":
                return
            value = event.value
            event.input.display = False  # hide, but keep the marker for the handler
            screen = self.screen
            if hasattr(screen, "on_cmd_submitted") and screen.on_cmd_submitted(value):
                return
            self.run_command(value)

        def run_command(self, line: str) -> None:
            parts = shlex.split(line) if line.strip() else []
            if not parts:
                return
            verb, args = parts[0], parts[1:]
            stage_key = _COMMAND_STAGE_ALIAS.get(verb)
            # `floor` is a swap alias with an option, as in floor tmux. Bare,
            # it points at the coppice binary, where the floor screen lives.
            if verb == "floor" and not args:
                self.set_status(FLOOR_MOVED)
            elif stage_key and args:
                from opendaisugi.swap import resolve_command

                try:
                    stage, label = resolve_command(f"{stage_key} {args[0]}")
                    self.set_status(swap_confirmation(stage, label, config_path=self.config_path))
                except ValueError as e:
                    self.set_status(f"✗ {e}")
                self.refresh_header()
            elif verb in _ORDER:
                if verb == "tree" and args:
                    self.selected_session = args[0]
                self.switch_screen(verb)
                self.set_status(f"view: {verb}")
            elif verb in ("q", "quit"):
                self.exit()
            else:
                self.set_status(
                    f"unknown command: {line!r} · try sessions, tree <id>, wiring, gate enforce"
                )

        def set_status(self, text: str) -> None:
            self.status_text = text
            try:
                from textual.widgets import Static

                self.screen.query_one("#status", Static).update(text)
            except Exception:  # noqa: BLE001 — status text is still recorded above
                pass

    DashboardApp = DaisugiApp  # one-release alias for the old name


def run_tui(data_dir: Path, *, interval: float = 2.0) -> None:
    """Run the interactive instrument in the terminal."""
    if not _HAVE_TEXTUAL:
        raise TextualNotInstalled(
            "the interactive GUI needs the [tui] extra — install `opendaisugi[tui]`"
        )
    DaisugiApp(data_dir=Path(data_dir), interval=interval).run()


def serve(
    data_dir: Path, *, host: str = "127.0.0.1", port: int = 8000, interval: float = 2.0
) -> None:
    """Serve the same app to a browser via local textual-serve (no external relay)."""
    if not _HAVE_TEXTUAL:
        raise TextualNotInstalled(
            "the browser GUI needs the [tui] extra — install `opendaisugi[tui]`"
        )
    from textual_serve.server import Server

    command = (
        f"{shlex.quote(sys.executable)} -m opendaisugi.cli dashboard --tui "
        f"--data-dir {shlex.quote(str(data_dir))} --interval {interval}"
    )
    Server(command, host=host, port=port, title="daisugi").serve()
