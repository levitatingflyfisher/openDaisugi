"""No control looks live when it is not; the mode is on screen at all times."""

import asyncio
import json
import time

import pytest

pytest.importorskip("textual")

from textual.widgets import Static  # noqa: E402

from opendaisugi.session_tree import SessionTree  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session, _sprig_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def _hdr(app) -> str:
    return str(app.screen.query_one("#hdr", Static).render())


def _peek(app) -> str:
    return str(app.screen.query_one("#peek", Static).render())


def test_header_shows_enforce_from_the_installed_hook(tmp_path, monkeypatch):
    home = tmp_path / "home"
    cwd = tmp_path / "cwd"  # no hook here — this is the machine-global-only case
    cwd.mkdir()
    (home / ".claude").mkdir(parents=True)
    (home / ".claude" / "settings.json").write_text(json.dumps({"hooks": {"PreToolUse": [{"hooks": [
        {"type": "command", "command": "py -m opendaisugi.gate_client --mode enforce"}]}]}}))
    monkeypatch.setattr("pathlib.Path.home", lambda: home)
    monkeypatch.chdir(cwd)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            # A machine-global-only hook reads as "global" — plan 5's fix so the
            # header can also distinguish "project" and "project+global" once a
            # daisugi start hook is in the mix, not just "a hook exists somewhere".
            assert "gate: ENFORCE (global)" in _hdr(app)

    _run(scenario)


def test_header_shows_enforce_from_a_cwd_scoped_start_hook(tmp_path, monkeypatch):
    """The header must not show a safer mode than an active `daisugi start
    --enforce` hook over the TUI's own launch directory, even with no
    machine-global hook installed."""
    home = tmp_path / "home"
    cwd = tmp_path / "cwd"
    (cwd / ".claude").mkdir(parents=True)
    (cwd / ".claude" / "settings.json").write_text(json.dumps({"hooks": {"PreToolUse": [{"hooks": [
        {"type": "command", "command": "py -m opendaisugi.gate_client --mode enforce"}]}]}}))
    monkeypatch.setattr("pathlib.Path.home", lambda: home)
    monkeypatch.chdir(cwd)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert "gate: ENFORCE (project)" in _hdr(app)

    _run(scenario)


def test_header_shows_disarmed(tmp_path):
    (tmp_path / "gate").mkdir()
    (tmp_path / "gate" / "DISARMED").write_text("")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert "gate: DISARMED" in _hdr(app)

    _run(scenario)


def test_header_marks_a_gateway_cache_rate_as_an_estimate(tmp_path):
    # S5: a gateway-derived (blended) cache rate carries a `?`; a live per-session
    # rate does not. With no live sessions, the header falls back to the gateway.
    from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord

    rec = GatewayTurnRecord(
        created_at="2026-08-27T00:00:00Z", signature="", task="x", tier="tier1-cloud",
        requested_model="claude-sonnet-4", model="claude-sonnet-4", difficulty=0.1,
        downgraded=False, estimated=False, input_tokens=100, output_tokens=10,
        frontier_tokens_saved=0, actual_dollars=0.01, counterfactual_dollars=0.02,
        cache_read_tokens=900, cache_creation_tokens=0,
    )
    GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl").append(rec)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            h = _hdr(app)
            assert "gateway" in h and "hit ?" in h  # estimate marked with ?

    _run(scenario)


def test_attach_on_claude_path_shows_the_resume_command(tmp_path):
    _claude_session(tmp_path, "abc-123", last_ts=time.time() - 1, tool_use_id="t1", decision="allow")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("enter")
            await pilot.pause()
            assert "claude --resume abc-123" in app.status_text

    _run(scenario)


def test_attach_uses_the_real_uuid_under_a_start_pin_not_the_pin_key(tmp_path):
    # `daisugi start` keys the tree by a per-cwd PIN, so session_id is the pin key
    # (a dead `claude --resume` argument); the real uuid lives on harnessSessionId.
    # Attach must resume by the uuid. Red against the pre-fix code (used session_id).
    pin = "myproj-ab12cd34"
    uuid = "9f3c1e77-real-claude-uuid"
    tree = SessionTree.create(tmp_path / "sessions", session_id=pin, harness="claude-code",
                              cwd="/proj", harness_session_id=uuid, clock=lambda: time.time() - 2)
    tree.append("tool_call", {"toolUseId": "tt", "name": "Read", "detail": "x.py"},
                clock=lambda: time.time() - 1)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("enter")
            await pilot.pause()
            assert f"claude --resume {uuid}" in app.status_text
            assert f"claude --resume {pin}" not in app.status_text  # never the dead pin key

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
            assert "s steer (not on this path)" in _peek(app)
            await pilot.press("j")
            await pilot.pause()
            peek = _peek(app)
            assert "s steer" in peek and "not on this path" not in peek

    _run(scenario)


def test_steer_is_inert_on_claude_but_writes_a_note_on_sprig(tmp_path):
    # S4: the s control is only bound to a real effect where it works.
    _claude_session(tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="t1", decision="allow")
    _sprig_session(tmp_path, "e1", last_ts=time.time() - 2)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")  # onto c1 (claude)
            await pilot.press("s")
            await pilot.pause()
            assert "not on this path" in app.status_text
            assert not app.screen.query_one("#cmd").display  # no input opened
            await pilot.press("j")  # onto e1 (sprig)
            await pilot.press("s")
            await pilot.pause()
            assert app.screen.query_one("#cmd").display
            app.screen.query_one("#cmd").value = "try the other file"
            await pilot.press("enter")
            await pilot.pause()
            notes = [e for e in SessionTree.open(tmp_path / "sessions", "e1").entries()
                     if e.type == "note"]
            assert notes and notes[-1].data == {"from": "operator", "text": "try the other file"}

    _run(scenario)


def test_wiring_keeps_its_effect_tags(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            app.switch_screen("wiring")
            await pilot.pause()
            assert app.screen.query("#effnote-verifier"), "planned stages must still say so"

    _run(scenario)
