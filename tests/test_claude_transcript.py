"""Claude Code's own JSONL: usage counters and the parentUuid tree, read-only."""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.claude_transcript import (
    last_model,
    last_prompt_uuid,
    leaf_uuids,
    read_turns,
    usage_totals,
)

ROWS = [
    {"type": "summary", "summary": "x"},
    {
        "type": "user",
        "uuid": "u1",
        "parentUuid": None,
        "sessionId": "s",
        "timestamp": "2026-08-27T10:00:00Z",
        "message": {"role": "user", "content": "list files"},
    },
    {
        "type": "assistant",
        "uuid": "a1",
        "parentUuid": "u1",
        "timestamp": "2026-08-27T10:00:01Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-4",
            "content": [
                {"type": "text", "text": "Sure."},
                {"type": "tool_use", "id": "toolu_01", "name": "Bash", "input": {"command": "ls"}},
            ],
            "usage": {
                "input_tokens": 12,
                "cache_read_input_tokens": 1000,
                "cache_creation_input_tokens": 50,
                "output_tokens": 30,
            },
        },
    },
    {
        "type": "user",
        "uuid": "u2",
        "parentUuid": "a1",
        "timestamp": "2026-08-27T10:00:02Z",
        "message": {
            "role": "user",
            "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": "a b"}],
        },
    },
    {
        "type": "assistant",
        "uuid": "a2",
        "parentUuid": "u2",
        "timestamp": "2026-08-27T10:00:03Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-4",
            "content": [{"type": "text", "text": "Done."}],
            "usage": {
                "input_tokens": 5,
                "cache_read_input_tokens": 1100,
                "cache_creation_input_tokens": 0,
                "output_tokens": 4,
            },
        },
    },
    {
        "type": "user",
        "uuid": "u3",
        "parentUuid": "a1",
        "timestamp": "2026-08-27T10:05:00Z",
        "message": {"role": "user", "content": "try again"},
    },  # a branch from a1
]


def _write(tmp_path: Path) -> Path:
    p = tmp_path / "t.jsonl"
    p.write_text("\n".join(json.dumps(r) for r in ROWS) + "\ntorn{", encoding="utf-8")
    return p


def test_read_turns_keeps_ids_kinds_and_usage(tmp_path):
    turns = read_turns(_write(tmp_path))
    assert [t.uuid for t in turns] == ["u1", "a1", "u2", "a2", "u3"]
    assert turns[1].parent_uuid == "u1" and turns[1].kind == "assistant"
    assert turns[1].usage == {"fresh": 12, "cacheRead": 1000, "cacheWrite": 50, "out": 30}
    assert turns[1].tool_uses == [{"id": "toolu_01", "name": "Bash"}]
    assert turns[2].tool_result_ids == ["toolu_01"]
    assert turns[0].text == "list files" and turns[1].text.startswith("Sure.")
    assert turns[1].model == "claude-sonnet-4"


def test_usage_totals_sum_assistant_turns(tmp_path):
    assert usage_totals(read_turns(_write(tmp_path))) == {
        "fresh": 17,
        "cacheRead": 2100,
        "cacheWrite": 50,
        "out": 34,
    }


def test_last_prompt_is_the_last_real_user_message(tmp_path):
    assert last_prompt_uuid(read_turns(_write(tmp_path))) == "u3"


def test_leaves_and_model(tmp_path):
    turns = read_turns(_write(tmp_path))
    assert sorted(leaf_uuids(turns)) == ["a2", "u3"]
    assert last_model(turns) == "claude-sonnet-4"


def test_missing_file_is_empty(tmp_path):
    assert read_turns(tmp_path / "nope.jsonl") == []
    assert usage_totals([]) == {"fresh": 0, "cacheRead": 0, "cacheWrite": 0, "out": 0}
