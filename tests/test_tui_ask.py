"""Allow is two keys (varying-token for a destructive would-deny), deny is one,
edit bounces back fail-closed, and the operator's presence is a heartbeat file.
"""

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


def _seed_lowblast(tmp_path):
    """The tree's last recorded call is a destructive Bash, but the PENDING ask
    is a harmless Read — S2 must classify off the ask, so this is low-blast."""
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 2, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Read", "toolInput": {"file_path": "x.py"}},
        deadline=now + 60,
    )


def _seed_destructive(tmp_path):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 2, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Bash", "toolInput": {"command": "rm -rf build/"}},
        deadline=now + 60,
    )


def _answer(tmp_path):
    return tmp_path / "gate" / "answers" / "t1.json"


def test_allow_needs_a_then_enter(tmp_path):
    _seed_lowblast(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("a")
            await pilot.pause()
            assert "⏎ to confirm allow" in app.status_text
            assert not _answer(tmp_path).exists()
            await pilot.press("a")  # a repeated a still needs ⏎ — never a one-key allow
            await pilot.pause()
            assert not _answer(tmp_path).exists()
            await pilot.press("k")  # moving disarms
            await pilot.press("j")
            await pilot.press("enter")
            await pilot.pause()
            assert not _answer(tmp_path).exists()
            await pilot.press("a")
            await pilot.press("enter")
            await pilot.pause()
            body = json.loads(_answer(tmp_path).read_text())
            assert body["decision"] == "allow"
            assert "allowed t1" in app.status_text

    _run(scenario)


def test_destructive_allow_requires_a_typed_token(tmp_path):
    _seed_destructive(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("a")
            await pilot.pause()
            # a fixed a->Enter is NOT enough for a high-blast would-deny, and the
            # token VARIES per row (the session id 's1', not the constant 'Bash')
            assert "HIGH-BLAST" in app.status_text and "'s1'" in app.status_text
            assert "'Bash'" not in app.status_text  # the constant tool name is never the token
            box = app.screen.query_one("#cmd")
            box.value = "nope"
            await pilot.press("enter")
            await pilot.pause()
            assert not _answer(tmp_path).exists()
            assert "did not match" in app.status_text
            # the right token (the session id) confirms
            await pilot.press("a")
            await pilot.pause()
            app.screen.query_one("#cmd").value = "s1"
            await pilot.press("enter")
            await pilot.pause()
            body = json.loads(_answer(tmp_path).read_text())
            assert body["decision"] == "allow" and "token confirmed" in app.status_text

    _run(scenario)


def _seed_shell(tmp_path, command):
    now = time.time()
    _claude_session(tmp_path, "s1", last_ts=now - 2, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Bash", "toolInput": {"command": command}},
        deadline=now + 60,
    )


def test_a_non_rm_destructive_shell_command_demands_the_token(tmp_path):
    # The typed-token guard must cover destructive shell beyond `rm` (e.g. the
    # review's `dd of=/dev/sda`). This uses `truncate`, which NO destructive
    # pattern matches — so the guard here comes purely from the raw-shell rule
    # (a Bash call is high-blast regardless of the string). Removing that rule
    # makes this test go red.
    _seed_shell(tmp_path, "truncate -s 0 /srv/prod.db")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("a")
            await pilot.pause()
            assert "HIGH-BLAST" in app.status_text  # not the low-effort two-step
            await pilot.press("enter")  # a bare Enter (the habituated reflex)
            await pilot.pause()
            assert not _answer(tmp_path).exists()  # cannot be allowed by a+Enter
            # only the varying token confirms
            await pilot.press("a")
            await pilot.pause()
            app.screen.query_one("#cmd").value = "s1"
            await pilot.press("enter")
            await pilot.pause()
            assert json.loads(_answer(tmp_path).read_text())["decision"] == "allow"

    _run(scenario)


def test_bare_enter_reflex_cannot_allow_a_destructive_action(tmp_path):
    # The exact habituated two-key reflex (a, then Enter with no typing) must NOT
    # allow a destructive action — that reflex is what the varying token defeats.
    _seed_destructive(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("a")
            await pilot.press("enter")  # bare Enter — no token typed
            await pilot.pause()
            assert not _answer(tmp_path).exists()

    _run(scenario)


def test_confirm_rechecks_the_armed_id_against_the_selected_row(tmp_path):
    # S2 (misfire-safe): an arm that survives a re-sort must NOT fire on whatever
    # row now sits under the cursor. This fails if the armed-id == selected-row
    # re-check in action_confirm is removed.
    now = time.time()
    _claude_session(tmp_path, "sa", last_ts=now - 1, tool_use_id="ta", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="ta",
        question={"sessionId": "sa", "toolName": "Read", "toolInput": {"file_path": "a.py"}},
        deadline=now + 60,
    )
    _claude_session(tmp_path, "sb", last_ts=now - 5, tool_use_id="tb", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="tb",
        question={"sessionId": "sb", "toolName": "Read", "toolInput": {"file_path": "b.py"}},
        deadline=now + 60,
    )

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")  # onto sa (age 1, sorts above sb)
            await pilot.press("a")  # arm ta
            await pilot.pause()
            assert app.screen._armed == "ta"
            await pilot.press("j")  # onto sb — the cursor move disarms
            await pilot.pause()
            app.screen._armed = "ta"  # simulate a stale arm surviving a re-sort
            await pilot.press("enter")  # confirm while sb is selected
            await pilot.pause()
            # neither ta (not the row under the cursor) nor tb (not armed) is allowed
            assert not (tmp_path / "gate" / "answers" / "ta.json").exists()
            assert not (tmp_path / "gate" / "answers" / "tb.json").exists()

    _run(scenario)


def test_deny_is_one_key(tmp_path):
    _seed_destructive(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("d")
            await pilot.pause()
            body = json.loads(_answer(tmp_path).read_text())
            assert body["decision"] == "deny"

    _run(scenario)


def test_edit_bounces_back_as_deny_with_reason(tmp_path):
    # S3: the harness's honoring of updatedInput is UNVERIFIED, so an edit never
    # becomes a silent allow of the original — it denies and re-asks the agent.
    _seed_destructive(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("e")
            await pilot.pause()
            box = app.screen.query_one("#cmd")
            assert box.display and "rm -rf build/" in box.value
            box.value = "rm -rf build/tmp"
            await pilot.press("enter")
            await pilot.pause()
            body = json.loads(_answer(tmp_path).read_text())
            assert body["decision"] == "deny"
            assert "rm -rf build/tmp" in body["reason"]
            assert "unverified" in app.status_text

    _run(scenario)


def test_remember_writes_a_proposal_not_an_envelope(tmp_path):
    _seed_destructive(tmp_path)

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


def test_cancel_clears_the_prefill_marker(tmp_path):
    # Advisor: if _cmd_mode survives a cancel, the operator's NEXT command is
    # eaten by the edit handler and answers a tool they weren't looking at.
    _seed_destructive(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await pilot.press("j")
            await pilot.press("e")  # open the edit line
            await pilot.pause()
            await pilot.press("escape")  # cancel
            await pilot.pause()
            await pilot.press("colon")
            await pilot.press(*"wiring")
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name == "wiring"  # the command ran, was not eaten
            assert not _answer(tmp_path).exists()  # no stray answer written

    _run(scenario)


def test_presence_while_running_and_gone_after(tmp_path):
    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert ask.operator_present(tmp_path / "gate")
        assert not (tmp_path / "gate" / "operator.json").exists()

    _run(scenario)
