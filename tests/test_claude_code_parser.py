"""The Claude Code parser keeps threading ids (uuid/parentUuid/sessionId)."""

from __future__ import annotations


def test_read_messages_keeps_threading_ids(tmp_path):
    import json

    from opendaisugi.parsers.claude_code import ClaudeCodeParser

    p = tmp_path / "t.jsonl"
    p.write_text(json.dumps({"type": "user", "uuid": "u1", "parentUuid": None, "sessionId": "s9",
                             "message": {"role": "user", "content": "hi"}}) + "\n")
    msgs = ClaudeCodeParser()._read_messages(p)
    assert msgs[0]["uuid"] == "u1" and msgs[0]["parentUuid"] is None and msgs[0]["sessionId"] == "s9"
    assert msgs[0]["role"] == "user" and msgs[0]["content"] == "hi"
