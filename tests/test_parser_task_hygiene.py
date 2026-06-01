"""Dogfooding fix: episode tasks must stay short enough that envelope generation
(max_task_chars=4000) doesn't hard-error on real transcripts, where a user turn
can carry thousands of chars of injected skill/system text.
"""
from __future__ import annotations

from opendaisugi.parsers.claude_code import _user_text


def test_user_text_caps_runaway_task_length():
    msg = {"role": "user", "content": "do the thing. " + "x" * 9000}
    t = _user_text(msg)
    assert t is not None
    assert len(t) <= 2000, "task must be capped so ingest's envelope-gen won't hard-error"
    assert t.startswith("do the thing.")  # keeps the meaningful head


def test_user_text_caps_list_content_too():
    msg = {"role": "user", "content": [{"type": "text", "text": "y" * 9000}]}
    t = _user_text(msg)
    assert t is not None and len(t) <= 2000


def test_user_text_short_message_unchanged():
    msg = {"role": "user", "content": "fix the bug"}
    assert _user_text(msg) == "fix the bug"


# --- v0.28.1: clean injected boilerplate out of episode task labels -----------

def test_skill_injection_becomes_skill_label():
    msg = {"role": "user", "content":
           "Base directory for this skill: /home/x/.claude/plugins/cache/iss-skills/skills/sgcm\n\n"
           "# SGCM: Scout · Generator · Critic · Mediator\n\nA meta-cognitive framework..."}
    t = _user_text(msg)
    assert t == "skill: sgcm"  # the skill name, not the injected body


def test_continuation_banner_becomes_short_label():
    msg = {"role": "user", "content":
           "This session is being continued from a previous conversation that ran out of context.\n\n"
           "Summary:\n" + "blah " * 2000}
    t = _user_text(msg)
    assert t == "session continuation"
    assert len(t) < 100


def test_system_reminder_stripped_real_text_kept():
    msg = {"role": "user", "content":
           "refactor the auth module\n<system-reminder>You have superpowers. Always use skills.</system-reminder>"}
    t = _user_text(msg)
    assert "superpowers" not in t
    assert "refactor the auth module" in t


def test_normal_task_unchanged_by_cleaner():
    assert _user_text({"role": "user", "content": "add a retry decorator to the client"}) \
        == "add a retry decorator to the client"
