"""Promotion is one line: the pane reads the foreman page the coppice binary
prints, and becomes the foreman."""

from __future__ import annotations

import re
import time
from pathlib import Path

import pytest

from opendaisugi.floor import PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.promote import PROMOTION, PromotionNotSent, promote
from opendaisugi.floor.registry import prompt_pane

REPO = Path(__file__).resolve().parents[2]


class Recorder:
    """A backend whose one pane w1:p1 is in the given state."""

    name = "fake"

    def __init__(self, state="idle"):
        self.state = state
        self.calls: list[tuple[str, str, bool]] = []

    def send_text(self, pane, text, *, enter=True):
        self.calls.append((pane.id, text, enter))

    def list(self):
        ev = PaneStateEvent(
            session_id="s",
            harness="claude",
            state=self.state,
            source="process",
            ts=time.time(),
            pane="w1:p1",
        )
        ref = PaneRef("fake", "w1:p1")
        return [PaneInfo(ref=ref, label="floor", cwd="/", cmd=["claude"], kind="pty", state=ev)]


def test_promote_sends_exactly_one_line_that_names_the_page():
    b = Recorder()
    got = promote(b, PaneRef("fake", "w1:p1"))
    assert b.calls == [("w1:p1", PROMOTION, True)]
    assert got == PROMOTION
    assert "coppice skill foreman" in PROMOTION
    assert PROMOTION.endswith("Say ready.")


def test_the_line_is_the_one_the_floor_sends():
    src = (REPO / "harness/coppice/skills/skills.go").read_text()
    m = re.search(r'const FloorLine = "([^"]*)"', src)
    assert m is not None
    assert m.group(1) == PROMOTION


def test_prompt_pane_as_foreman_promotes_first():
    b = Recorder()
    got = prompt_pane(b, PaneRef("fake", "w1:p1"), "tidy the docs", kind="pty", foreman=True)
    assert got == "typed"
    assert [c[1] for c in b.calls] == [PROMOTION, "tidy the docs"]


def test_prompt_pane_as_foreman_with_no_text_only_promotes():
    b = Recorder()
    prompt_pane(b, PaneRef("fake", "w1:p1"), "", kind="pty", foreman=True)
    assert [c[1] for c in b.calls] == [PROMOTION]


@pytest.mark.parametrize("state", ["blocked", "working"])
def test_promote_sends_nothing_to_a_pane_that_is_not_idle(state):
    b = Recorder(state=state)
    with pytest.raises(PromotionNotSent) as e:
        promote(b, PaneRef("fake", "w1:p1"), timeout_s=0.3)
    assert b.calls == []
    assert "idle" in str(e.value)


def test_prompt_pane_as_foreman_sends_no_text_when_the_page_was_not_sent():
    b = Recorder(state="blocked")
    with pytest.raises(PromotionNotSent):
        prompt_pane(b, PaneRef("fake", "w1:p1"), "tidy", kind="pty", foreman=True, timeout_s=0.3)
    assert b.calls == []
