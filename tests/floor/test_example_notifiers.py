"""The three example notifiers, each driven by a fake coppice socket.

Each notifier hears one pane go blocked and sends exactly one message with
the pane's label and the ask's summary. The ntfy and voice targets are
loopback HTTP servers the test starts. No test reaches a real ntfy or a
real voice engine.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from tests.floor.plugin_fakes import PLUGINS, FakeFloor, assert_ended, run_plugin, state

ASK = {
    "id": "toolu_1",
    "tool": "Bash",
    "summary": "rm -rf build",
    "deadline": 9e9,
    "tier": "undoable",
    "gate": {"verdict": "deny", "rule": 4},
}


class Sink:
    """A loopback HTTP server that records every POST."""

    def __init__(self) -> None:
        self.posts: list[dict] = []
        sink = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                n = int(self.headers.get("Content-Length") or 0)
                sink.posts.append(
                    {
                        "path": self.path,
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                        "body": self.rfile.read(n).decode(),
                    }
                )
                self.send_response(200)
                self.end_headers()

            def log_message(self, *args):
                pass

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self) -> None:
        self.srv.shutdown()
        self.srv.server_close()


@pytest.fixture
def sink():
    s = Sink()
    yield s
    s.close()


@pytest.fixture
def sock(tmp_path: Path) -> Path:
    return tmp_path / "f.sock"


def panes(req: dict) -> dict:
    return {"panes": [{"id": "w1:p1", "label": "auth fix"}, {"id": "w2:p1", "label": ""}]}


EVENTS = [
    state("w1:p1", "working", 1.0),
    state("w1:p1", "blocked", 2.0, ask=ASK),
    state("w1:p1", "blocked", 3.0, ask=ASK),
    state("w2:p1", "working", 4.0),
]


def manifest(plugin: str) -> dict:
    return json.loads((PLUGINS / plugin / "manifest.json").read_text())


def drive(plugin: str, sock: Path, config: dict) -> tuple[FakeFloor, object]:
    floor = FakeFloor(sock, answers={"pane.list": panes}, events=EVENTS)
    proc = run_plugin(plugin, plugin + ".py", sock, config=config)
    floor.close()
    return floor, proc


def test_notifiers_hold_no_acting_verb():
    for plugin in ("notify-ntfy", "notify-lockscreen", "notify-voice"):
        m = manifest(plugin)
        assert m["kind"] == "policy" and m["listens"] == ["state"]
        assert set(m["needs"]) <= {"pane.list", "floor.note"}, m["needs"]
        first = (PLUGINS / plugin / m["run"]).read_text().splitlines()[:5]
        assert first[0] == "#!/usr/bin/env python3"
        assert "# dependencies = []" in first


def test_ntfy_posts_one_message_with_the_label_and_the_summary(sock, sink):
    floor, proc = drive(
        "notify-ntfy",
        sock,
        {"url": sink.url, "topic": "floor", "click_base": "https://box.example"},
    )
    assert_ended(proc)
    assert len(sink.posts) == 1, sink.posts
    post = sink.posts[0]
    assert post["path"] == "/floor"
    assert post["headers"]["title"] == "auth fix needs you."
    assert post["body"] == "Wants rm -rf build. Gate says no, rule 4."
    assert post["headers"]["click"] == "https://box.example/#/pane/w1:p1"
    assert "actions" not in post["headers"]
    assert {r["cmd"] for r in floor.requests} <= {"hello", "events.subscribe", "pane.list"}


def test_ntfy_sends_the_token_from_its_named_variable(sock, sink, monkeypatch):
    floor = FakeFloor(sock, answers={"pane.list": panes}, events=EVENTS)
    proc = run_plugin(
        "notify-ntfy",
        "notify-ntfy.py",
        sock,
        config={"url": sink.url, "topic": "floor", "token_env": "NTFY_TOK"},
        extra_env={"NTFY_TOK": "sekrit"},
    )
    floor.close()
    assert_ended(proc)
    assert sink.posts[0]["headers"]["authorization"] == "Bearer sekrit"


def test_ntfy_with_no_settings_sends_nothing_and_finishes(sock, sink):
    floor, proc = drive("notify-ntfy", sock, manifest("notify-ntfy")["config"])
    assert proc.returncode == 0, proc.stderr
    assert sink.posts == []
    assert "not set up" in proc.stderr
    assert floor.requests == []


def test_lockscreen_posts_one_card_with_look_and_no_deny(sock, sink):
    floor, proc = drive(
        "notify-lockscreen",
        sock,
        {"url": sink.url, "topic": "floor", "click_base": "https://box.example"},
    )
    assert_ended(proc)
    assert len(sink.posts) == 1, sink.posts
    post = sink.posts[0]
    assert post["headers"]["title"] == "auth fix needs you."
    assert post["body"] == "Wants rm -rf build. Gate says no, rule 4."
    assert post["headers"]["actions"] == "view, Look, https://box.example/#/pane/w1:p1"
    assert "deny" not in post["headers"]["actions"].lower()


def test_voice_speaks_one_line_with_the_label_and_the_summary(sock, sink):
    floor, proc = drive("notify-voice", sock, {"speak_url": sink.url + "/speak"})
    assert_ended(proc)
    assert len(sink.posts) == 1, sink.posts
    assert sink.posts[0]["path"] == "/speak"
    assert json.loads(sink.posts[0]["body"]) == {"text": "auth fix needs you. Wants rm -rf build."}
    assert floor.sent("floor.note") == []


def test_voice_with_no_speak_url_leaves_one_note(sock, sink):
    floor, proc = drive("notify-voice", sock, manifest("notify-voice")["config"])
    assert_ended(proc)
    assert sink.posts == []
    notes = floor.sent("floor.note")
    assert len(notes) == 1
    assert notes[0]["text"] == (
        "auth fix needs you. Wants rm -rf build. Voice is not set up. "
        "Set speak_url in [plugin.notify-voice] in coppice.toml."
    )


def test_a_label_falls_back_to_the_pane_id(sock, sink):
    events = [state("w2:p1", "blocked", 1.0, ask=ASK)]
    floor = FakeFloor(sock, answers={"pane.list": panes}, events=events)
    proc = run_plugin("notify-ntfy", "notify-ntfy.py", sock, config={"url": sink.url, "topic": "t"})
    floor.close()
    assert_ended(proc)
    assert sink.posts[0]["headers"]["title"] == "w2:p1 needs you."


def test_a_refused_label_lookup_does_not_stop_the_notifier(sock, sink):
    calls = []

    def flaky(req):
        calls.append(req)
        if len(calls) == 1:
            return {"error": {"code": "internal", "message": "busy"}}
        return panes(req)

    events = [state("w1:p1", "blocked", 1.0, ask=ASK), state("w2:p1", "blocked", 2.0, ask=ASK)]
    floor = FakeFloor(sock, answers={"pane.list": flaky}, events=events)
    proc = run_plugin("notify-ntfy", "notify-ntfy.py", sock, config={"url": sink.url, "topic": "t"})
    floor.close()
    assert_ended(proc)
    assert [p["headers"]["title"] for p in sink.posts] == ["w1:p1 needs you.", "w2:p1 needs you."]


HELD = dict(state("w1:p1", "blocked", 2.0, ask=ASK), held={"by": "w1:p9", "task": "t1"})


@pytest.mark.parametrize(
    ("events", "posts"),
    [
        ([state("w1:p1", "working", 1.0), HELD, dict(HELD, ts=2.5)], 0),
        ([state("w1:p1", "working", 1.0), HELD, state("w1:p1", "blocked", 3.0, ask=ASK)], 1),
    ],
    ids=["while-held", "when-the-hold-ends"],
)
def test_a_held_ask_notifies_only_when_the_hold_ends(sock, sink, events, posts):
    floor = FakeFloor(sock, answers={"pane.list": panes}, events=events)
    proc = run_plugin(
        "notify-ntfy",
        "notify-ntfy.py",
        sock,
        config={"url": sink.url, "topic": "floor", "click_base": "https://box.example"},
    )
    floor.close()
    assert_ended(proc)
    assert len(sink.posts) == posts, sink.posts
