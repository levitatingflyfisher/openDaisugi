"""The roster: grouped, with a peek pane, refreshed in place (B1: query the screen)."""

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
            rows = _cells(app.screen.query_one("#roster", DataTable))
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
            peek = str(app.screen.query_one("#peek", Static).render())
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
            table = app.screen.query_one("#roster", DataTable)
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


def test_refresh_ages_survives_a_roster_keys_disagreement(tmp_path):
    # S6: _refresh_ages must set _roster_at (not AttributeError) and skip an id
    # missing from _keys (not KeyError) — a re-entrant timer tick must be a no-op.
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 5, tool_use_id="t1", decision="allow")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            screen = app.screen
            screen._keys.pop("s1", None)  # force the disagreement
            screen._refresh_ages()  # must not raise
            await pilot.pause()

    _run(scenario)
