"""One session's tree: fold, filter, label, fork, rewind. Says what it will not restore.

Positioning uses ``Tree.move_cursor`` (which does NOT post NodeSelected) so a
programmatic setup can't trip the rewind; ``enter`` (the Tree's own key) posts
NodeSelected and opens the menu. Every test asserts the cursor landed before
acting — a silent move_cursor no-op would otherwise pass or fail for the wrong
reason.
"""

import asyncio
import time

import pytest

pytest.importorskip("textual")

from textual.widgets import Tree  # noqa: E402

from opendaisugi.session_tree import SessionIndex, SessionTree  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402
from tests.test_cockpit import _claude_session  # noqa: E402


def _run(coro_fn):
    asyncio.run(coro_fn())


def _sprig_tree(tmp_path):
    t = SessionTree.create(tmp_path / "sessions", session_id="e1", harness="sprig", cwd=str(tmp_path))
    p = t.append("prompt", {"text": "write hello"})
    a = t.append("assistant", {"text": "ok", "model": "m",
                               "usage": {"fresh": 1, "cacheRead": 0, "cacheWrite": 0, "out": 1}})
    c = t.append("tool_call", {"toolUseId": "x1", "name": "write", "detail": "hi.txt"}, parent_id=a.id)
    t.append("verdict", {"toolUseId": "x1", "decision": "allow", "clause": "ok"}, parent_id=c.id)
    return t, p, a, c


def _labels(tree: Tree) -> list[str]:
    out = []

    def walk(node):
        out.append(str(node.label))
        for ch in node.children:
            walk(ch)

    walk(tree.root)
    return out


async def _open_tree(app, pilot, sid):
    app.selected_session = sid
    await pilot.pause()
    app.switch_screen("tree")
    await pilot.pause()


def test_tree_renders_a_sprig_session_and_filters(tmp_path):
    _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "e1")
            labels = _labels(app.screen.query_one("#tree", Tree))
            assert any("write hello" in x for x in labels)
            assert any("write hi.txt" in x for x in labels)
            await pilot.press("ctrl+o")  # no tools
            await pilot.pause()
            labels = _labels(app.screen.query_one("#tree", Tree))
            assert not any("write hi.txt" in x for x in labels)
            assert "filter: no tools" in app.status_text

    _run(scenario)


def test_tree_renders_a_claude_session_from_its_transcript(tmp_path):
    _claude_session(tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="toolu_01", decision="allow")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "c1")
            labels = _labels(app.screen.query_one("#tree", Tree))
            assert any("list files" in x for x in labels)
            assert any("try again" in x for x in labels)  # the branch from a1
            assert any("allow" in x.lower() and "toolu_01" in x for x in labels)  # joined verdict

    _run(scenario)


def test_label_writes_a_label_entry(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "e1")
            tree = app.screen.query_one("#tree", Tree)
            tree.move_cursor(tree.root.children[0])  # the prompt
            await pilot.pause()
            assert tree.cursor_node.data.id == p.id  # the cursor really landed
            await pilot.press("L")
            await pilot.pause()
            app.screen.query_one("#cmd").value = "good start"
            await pilot.press("enter")
            await pilot.pause()
            labels = [e for e in t.entries() if e.type == "label"]
            assert labels and labels[0].data == {"target": p.id, "label": "good start"}

    _run(scenario)


def test_fork_here_creates_a_child_session(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "e1")
            tree = app.screen.query_one("#tree", Tree)
            tree.move_cursor(tree.root.children[0].children[0])  # the assistant turn
            await pilot.pause()
            assert tree.cursor_node.data.id == a.id
            await pilot.press("enter")
            await pilot.pause()
            assert app.screen.name != "tree"  # the rewind menu is up
            assert "Restore workspace" not in str(app.screen.query_one("#menu").render())  # no checkpoint
            await pilot.press("f")
            await pilot.pause()
            ids = [s.session_id for s in SessionIndex(tmp_path / "sessions").list()]
            child = next(i for i in ids if i.startswith("e1-"))
            assert SessionTree.open(tmp_path / "sessions", child).meta()["parentEntry"] == a.id
            assert "forked" in app.status_text and child in app.status_text

    _run(scenario)


def test_restore_conversation_moves_the_head(tmp_path):
    t, p, a, c = _sprig_tree(tmp_path)

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "e1")
            tree = app.screen.query_one("#tree", Tree)
            tree.move_cursor(tree.root.children[0])  # the prompt
            await pilot.pause()
            assert tree.cursor_node.data.id == p.id
            await pilot.press("enter")
            await pilot.pause()
            await pilot.press("c")  # Restore conversation
            await pilot.pause()
            # re-open: the app moved the head via its own SessionTree instance; the
            # builder ``t`` still holds its stale in-memory head cache.
            assert SessionTree.open(tmp_path / "sessions", "e1").head() == p.id
            assert "workspace not restored" in app.status_text

    _run(scenario)


def test_rewind_menu_on_claude_session_offers_fork_command(tmp_path):
    _claude_session(tmp_path, "c1", last_ts=time.time() - 1, tool_use_id="toolu_01", decision="allow")

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            await _open_tree(app, pilot, "c1")
            tree = app.screen.query_one("#tree", Tree)
            tree.move_cursor(tree.root.children[0])
            await pilot.pause()
            assert tree.cursor_node.data is not None  # the cursor landed on a real node
            await pilot.press("enter")
            await pilot.pause()
            await pilot.press("f")
            await pilot.pause()
            assert "claude --resume c1 --fork-session" in app.status_text
            assert "rewind inside Claude with Esc Esc" in app.status_text

    _run(scenario)
