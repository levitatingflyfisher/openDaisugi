"""The instrument opens on the live work; wiring is a command (or Tab) away.

B1: every query goes through ``app.screen.query_one(...)``. A pushed screen is
opaque to ``App.query``, and the gate-mode indicator must be visible on EVERY
screen. This is a safety requirement, not chrome.
"""

import asyncio

import pytest

pytest.importorskip("textual")

from textual.widgets import Static  # noqa: E402

from opendaisugi.tui import DaisugiApp  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def test_app_opens_on_the_sessions_screen(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert app.screen.name == "sessions"
            assert app.screen.query("#roster"), "roster table did not render"
            assert "gate:" in str(app.screen.query_one("#hdr", Static).render())

    _run(scenario)


def test_colon_wiring_reaches_the_wiring_screen(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("colon")
            await pilot.press(*"wiring")
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name == "wiring"
            assert app.screen.query("#gauge-matcher"), "wiring did not render its gauges"

    _run(scenario)


def test_tab_cycles_sessions_tree_wiring(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert app.screen.name == "sessions"
            await pilot.press("tab")
            await pilot.pause()
            assert app.screen.name == "tree"
            await pilot.press("tab")
            await pilot.pause()
            assert app.screen.name == "wiring"
            await pilot.press("tab")
            await pilot.pause()
            assert app.screen.name == "sessions"

    _run(scenario)


def test_bare_colon_floor_names_the_coppice_binary(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("colon")
            await pilot.press(*"floor")
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name == "sessions"
            assert app.status_text == (
                "The floor lives in the coppice binary. Run: daisugi coppice floor"
            )

    _run(scenario)


def test_gate_mode_indicator_is_on_every_screen(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            for name in ("sessions", "tree", "wiring"):
                app.switch_screen(name)
                await pilot.pause()
                assert app.screen.name == name
                assert "gate:" in str(app.screen.query_one("#hdr", Static).render()), (
                    f"the gate-mode indicator is missing on {name}"
                )

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


def test_the_sessions_poll_stops_while_another_screen_shows(tmp_path, monkeypatch):
    """A screen switched away from is suspended, not unmounted, so on_unmount
    never stopped its 1 Hz poll. The poll then ran on a hidden screen, and a
    tick at app exit, after the screen's widgets were gone, raised NoMatches
    for #roster (CI, 2026-10-14). The poll runs only while the screen shows.
    """
    from opendaisugi import tui_sessions

    hidden_polls = []
    real_poll = tui_sessions.SessionsScreen._poll

    def spy(self):
        if not self.is_current:
            hidden_polls.append(True)
        return real_poll(self)

    monkeypatch.setattr(tui_sessions.SessionsScreen, "_poll", spy)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            sessions = app.screen
            app.switch_screen("wiring")
            await pilot.pause()
            assert sessions._poll_timer is None, "the poll still runs on a hidden screen"
            await pilot.pause(1.5)
            app.switch_screen("sessions")
            await pilot.pause()
            assert sessions._poll_timer is not None, "the poll did not come back"
        assert hidden_polls == []

    _run(scenario)
