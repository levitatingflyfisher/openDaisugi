"""The wiring screen: today's live floor with click-to-swap, moved onto the
shared base (B2). Same buttons, same ``[live]/[cfg]/[planned]`` effect tags,
same honest ``do_swap`` — reachable by ``:wiring`` or Tab, never on start.

The status goes through ``self.app.set_status`` (the base's status line), and a
swap returns the HONEST confirmation string (``swap_confirmation``), never
``apply_swap``'s ``Config`` — a typed command or a click can't imply an effect a
``cfg``/``planned`` stage does not have.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, VerticalScroll
from textual.widgets import Button, Static

from opendaisugi.config import load_config
from opendaisugi.dashboard import collect_metrics
from opendaisugi.swap import (
    EFFECT_TAG,
    SWAP_KNOBS,
    apply_swap,
    effect_of,
    is_live,
    is_swappable,
    resolve_command,
    selected_label,
)
from opendaisugi.tui_base import CockpitScreen

_STATE_GLYPH = {"active": "●", "available": "○", "possible": "·"}
_EFFECT_COLOR = {"live": "#7fd08a", "cfg": "#e5b567", "planned": "#8a93a6"}

# A short, specific "how to make it take effect" line per non-live stage.
_EFFECT_HINT: dict[str, str] = {
    "gate": "run `daisugi install --enforce` to apply",
    "backend": "set OPENDAISUGI_LLM_BACKEND, then restart",
    "envelope": "pick a backend first, then restart",
    "harness": "change by running `daisugi install` in your host",
    "router": "start routing with `daisugi gateway`",
    "verifier": "not wired yet — python always enforces",
    "matcher": "not wired yet — MiniLM runs regardless",
    "stores": "git store: run `daisugi registry init`",
}


def swap_confirmation(stage_key: str, label: str, *, config_path) -> str:
    """Apply the swap and return an HONEST one-line confirmation.

    Widget-free (touches no Buttons), so the App can use it for a ``:gate``
    command from ANY screen without querying the wiring screen's buttons — and
    it never returns the ``Config`` ``apply_swap`` hands back (that would print a
    ``Config`` repr and lose the honest "needs a restart/reinstall" tail)."""
    apply_swap(stage_key, label, config_path=config_path)
    tail = "" if is_live(stage_key) else f"  ({_EFFECT_HINT.get(stage_key, 'not live yet')})"
    return f"set {stage_key} → {label}{tail}"


def _gauge_line(gauges) -> str:
    parts = []
    for g in gauges:
        detail = f"  ({g.detail})" if g.detail else ""
        parts.append(f"▸ {g.label} {g.value}{detail}")
    return "   ".join(parts)


class WiringScreen(CockpitScreen):
    """The module map and swap knobs — the expert live floor."""

    CSS = """
    .stage-title { padding: 1 1 0 2; text-style: bold; }
    .stage-role  { padding: 0 2 0 4; color: $text-muted; }
    .optrow      { height: 1; padding: 0 2 0 4; }
    Button.swap  { border: none; height: 1; min-width: 22; margin: 0 1 0 0; padding: 0 1; }
    Button.swap:focus { background: $accent 40%; text-style: bold; }
    Button.swap:hover  { background: $boost; }
    .optdesc     { color: $text-muted; }
    .effnote     { padding: 0 2 0 4; color: $text-muted; text-style: italic; }
    .chips       { padding: 0 2 0 4; }
    .gauge       { padding: 0 2 1 4; color: $accent; }
    """
    BINDINGS = [
        Binding("down,j", "next", "▼ move", show=True),
        Binding("up,k", "prev", "▲ move", show=True),
        Binding("enter,space", "select", "set", show=True),
        Binding("r", "refresh", "↻ refresh", show=True),
    ]

    def __init__(self) -> None:
        super().__init__(name="wiring")
        self._button_map: dict[str, tuple[str, str]] = {}

    def _title_line(self, st) -> str:
        eff = effect_of(st.key)
        if not eff:
            return st.title
        color = _EFFECT_COLOR.get(eff, "#8a93a6")
        return f"{st.title}   ·  [{color}][{eff}][/] [dim]{EFFECT_TAG.get(eff, '')}[/]"

    def compose_body(self) -> ComposeResult:
        cfg = load_config(self.app.config_path)
        metrics = collect_metrics(self.app.data_dir)
        with VerticalScroll(id="floor"):
            for st in self.app._stages:
                yield Static(self._title_line(st), classes="stage-title")
                yield Static(st.role, classes="stage-role")
                if is_swappable(st.key):
                    sel = selected_label(cfg, st.key)
                    knob = SWAP_KNOBS[st.key]
                    for i, opt in enumerate(knob.options):
                        bid = f"swap-{st.key}-{i}"
                        self._button_map[bid] = (st.key, opt.label)
                        cost = " [yellow]· $ costs money[/]" if opt.cost else ""
                        with Horizontal(classes="optrow"):
                            yield Button(
                                opt.label,
                                id=bid,
                                variant="success" if opt.label == sel else "default",
                                classes="swap",
                            )
                            yield Static(f"{opt.desc}{cost}", classes="optdesc")
                else:
                    chips = "   ".join(
                        f"{_STATE_GLYPH.get(m.state, '·')} {m.name}" for m in st.modules
                    )
                    yield Static(chips, classes="chips")
                if not is_live(st.key):
                    yield Static(
                        f"↳ {_EFFECT_HINT.get(st.key, 'not live yet')}",
                        classes="effnote",
                        id=f"effnote-{st.key}",
                    )
                yield Static(
                    _gauge_line(metrics.get(st.key, [])),
                    id=f"gauge-{st.key}",
                    classes="gauge",
                )

    def on_mount(self) -> None:
        super().on_mount()  # the shared header
        self.set_interval(self.app.interval, self.action_refresh)
        first = self.query(".swap").first()
        if first is not None:
            first.focus()  # keyboard nav has a starting point

    # --- navigation & refresh ---------------------------------------------
    def action_next(self) -> None:
        self.focus_next()

    def action_prev(self) -> None:
        self.focus_previous()

    def action_select(self) -> None:
        focused = self.app.focused
        if isinstance(focused, Button):
            focused.press()

    def action_refresh(self) -> None:
        metrics = collect_metrics(self.app.data_dir)
        for st in self.app._stages:
            try:
                self.query_one(f"#gauge-{st.key}", Static).update(
                    _gauge_line(metrics.get(st.key, []))
                )
            except Exception:  # noqa: BLE001
                continue

    # --- the honest swap (command line + click share it) ------------------
    def on_cmd_submitted(self, value: str) -> bool:
        """A ``:`` command on the wiring screen is a swap (``shell reject``,
        ``gate enforce``). If it does not parse as one, hand it back so the App's
        global parser can switch views or quit."""
        try:
            stage, label = resolve_command(value)
        except ValueError:
            return False
        self.app.set_status(self.do_swap(stage, label))
        return True

    def do_swap(self, stage_key: str, label: str) -> str:
        """Apply a swap, re-highlight its stage, and return the HONEST status."""
        status = swap_confirmation(stage_key, label, config_path=self.app.config_path)
        cfg = load_config(self.app.config_path)
        knob = SWAP_KNOBS[stage_key]
        sel = selected_label(cfg, stage_key)
        for i, opt in enumerate(knob.options):
            try:
                btn = self.query_one(f"#swap-{stage_key}-{i}", Button)
                btn.variant = "success" if opt.label == sel else "default"
            except Exception:  # noqa: BLE001
                continue
        return status

    def on_button_pressed(self, event: "Button.Pressed") -> None:
        info = self._button_map.get(event.button.id or "")
        if info is None:
            return
        self.app.set_status(self.do_swap(*info))
