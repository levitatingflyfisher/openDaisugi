"""Smoke tests for the wiring screen (`daisugi dashboard --tui`, `:wiring`).

Gated on the [tui] extra: where Textual is absent these skip, and the pure swap
logic (test_swap.py) plus the stdlib view (test_dashboard.py) still cover the
behaviour. B2/B1: the swap floor moved onto ``WiringScreen`` over the shared
base, so we reach it with ``:wiring`` and query ``app.screen`` (never
``app.query``). A swap through the screen writes real config, re-highlights the
chosen option, and confirms HONESTLY (never a ``Config`` repr).
"""

import asyncio

import pytest

pytest.importorskip("textual")

from textual.widgets import Button  # noqa: E402

from opendaisugi.config import load_config  # noqa: E402
from opendaisugi.tui import DashboardApp  # noqa: E402  (one-release alias for DaisugiApp)


async def _to_wiring(pilot):
    """Open on sessions, then switch to the wiring screen and settle."""
    await pilot.pause()
    pilot.app.switch_screen("wiring")
    await pilot.pause()


def test_app_boots_over_the_real_data_layer(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            # every stage renders a gauge widget keyed by its stage key
            assert app.screen.query("#gauge-matcher"), "matcher stage did not render"
            assert app.screen.query("#gauge-verifier"), "verifier stage did not render"
            # the swappable shell stage rendered its two option buttons
            assert app.screen.query_one("#swap-shell-0", Button)
            assert app.screen.query_one("#swap-shell-1", Button)

    asyncio.run(scenario())


def test_swap_through_the_app_writes_config_and_highlights(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            status = app.screen.do_swap("shell", "reject-compound")
            await pilot.pause()
            assert "set shell" in status and "reject-compound" in status
            # the picked option is highlighted, the other is not
            assert app.screen.query_one("#swap-shell-1", Button).variant == "success"
            assert app.screen.query_one("#swap-shell-0", Button).variant == "default"
        # and the config on disk actually changed
        assert load_config(tmp_path / "config.yaml").shell_allow_decomposition is False

    asyncio.run(scenario())


def test_preference_stage_shows_a_not_yet_enforced_note(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            # verifier is a preference knob: option buttons render AND a note shows
            assert app.screen.query_one("#swap-verifier-0", Button)
            assert app.screen.query("#effnote-verifier")
            # shell is an enforced swap: no preference note
            assert not app.screen.query("#effnote-shell")

    asyncio.run(scenario())


def test_a_swap_button_is_focused_on_mount_for_keyboard_nav(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            assert isinstance(app.focused, Button), "keyboard nav needs an initial focus"
            assert (app.focused.id or "").startswith("swap-")

    asyncio.run(scenario())


def test_command_line_applies_a_swap_honestly(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            handled = app.screen.on_cmd_submitted("shell reject-compound")
            await pilot.pause()
            assert handled is True
            assert "set shell" in app.status_text and "reject-compound" in app.status_text
        assert load_config(tmp_path / "config.yaml").shell_allow_decomposition is False

    asyncio.run(scenario())


def test_command_line_on_a_cfg_stage_confirms_the_gap_not_a_fake_effect(tmp_path):
    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            app.screen.on_cmd_submitted("gate enforce")
            await pilot.pause()
            # honest: names the install step, never implies it took effect
            assert "set gate" in app.status_text and "install --enforce" in app.status_text

    asyncio.run(scenario())


def test_wiring_swap_never_prints_a_config_repr(tmp_path):
    """B2: the confirmation is the honest string, never ``apply_swap``'s Config."""

    async def scenario():
        app = DashboardApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _to_wiring(pilot)
            app.screen.on_cmd_submitted("gate shadow")
            await pilot.pause()
            assert "Config" not in app.status_text and "gate_mode=" not in app.status_text

    asyncio.run(scenario())
