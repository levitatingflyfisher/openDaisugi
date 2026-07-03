"""Tests for opendaisugi.parsers — models, protocol, registry."""

import json as _json

import pytest
from pydantic import ValidationError

from opendaisugi.models import ShellStep
from opendaisugi.parsers import Episode, ParseResult, get_parser

# ----- Episode model -----


def test_episode_requires_id_task_steps_source_range():
    with pytest.raises(ValidationError):
        Episode()


def test_episode_minimal():
    ep = Episode(
        id="ep_00",
        task="Do something",
        steps=[ShellStep(id="s0", command="echo hi")],
        source_range={"first_message": 0, "last_message": 2},
    )
    assert ep.id == "ep_00"
    assert ep.task == "Do something"
    assert ep.context is None
    assert len(ep.steps) == 1
    assert ep.source_range["first_message"] == 0


def test_episode_with_context():
    ep = Episode(
        id="ep_01",
        task="Fix auth",
        context="JWT token was expired",
        steps=[],
        source_range={"first_message": 3, "last_message": 5},
    )
    assert ep.context == "JWT token was expired"


# ----- ParseResult model -----


def test_parse_result_minimal():
    pr = ParseResult(
        source="claude-code",
        source_file="/tmp/transcript.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=[],
    )
    assert pr.source == "claude-code"
    assert pr.episodes == []


def test_parse_result_with_episodes():
    ep = Episode(
        id="ep_00",
        task="t",
        steps=[ShellStep(id="s0", command="echo")],
        source_range={"first_message": 0, "last_message": 1},
    )
    pr = ParseResult(
        source="claude-code",
        source_file="/tmp/t.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=[ep],
    )
    assert len(pr.episodes) == 1
    assert pr.episodes[0].id == "ep_00"


# ----- get_parser registry -----


def test_get_parser_unknown_format_raises():
    with pytest.raises(ValueError, match="Unknown parser format"):
        get_parser("nonexistent")


def test_get_parser_returns_claude_code_parser():
    parser = get_parser("claude-code")
    assert hasattr(parser, "parse")


# ----- ClaudeCodeParser -----

from pathlib import Path

from opendaisugi.parsers.claude_code import _TOOL_TYPE_MAP, ClaudeCodeParser, _extract_step

FIXTURE = Path(__file__).parent / "fixtures" / "sample_transcript.jsonl"


def test_tool_type_map_covers_spec_tools():
    assert _TOOL_TYPE_MAP["Edit"] == "file_write"
    assert _TOOL_TYPE_MAP["Write"] == "file_write"
    assert _TOOL_TYPE_MAP["Read"] == "file_read"
    assert _TOOL_TYPE_MAP["Bash"] == "shell"
    assert _TOOL_TYPE_MAP["Glob"] == "file_read"
    assert _TOOL_TYPE_MAP["Grep"] == "file_read"
    assert _TOOL_TYPE_MAP["WebFetch"] == "network"
    assert _TOOL_TYPE_MAP["WebSearch"] == "network"


def test_extract_step_edit():
    tool_use = {"name": "Edit", "id": "t1", "input": {"file_path": "src/app.py", "old_string": "a", "new_string": "b"}}
    step = _extract_step(tool_use)
    assert step is not None
    assert step["type"] == "file_write"
    assert step["path"] == "src/app.py"


def test_extract_step_bash():
    tool_use = {"name": "Bash", "id": "t2", "input": {"command": "pytest -v"}}
    step = _extract_step(tool_use)
    assert step is not None
    assert step["type"] == "shell"
    assert step["command"] == "pytest -v"


def test_extract_step_unknown_tool_returns_none():
    tool_use = {"name": "Agent", "id": "t3", "input": {"prompt": "do stuff"}}
    assert _extract_step(tool_use) is None


from opendaisugi.parsers.claude_code import _is_real_user_message


def test_real_user_message_string_content():
    assert _is_real_user_message({"role": "user", "content": "do something"}) is True


def test_real_user_message_human_role():
    assert _is_real_user_message({"role": "human", "content": "do something"}) is True


def test_tool_result_is_not_real_user_message():
    msg = {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]}
    assert _is_real_user_message(msg) is False


def test_assistant_is_not_real_user_message():
    assert _is_real_user_message({"role": "assistant", "content": "text"}) is False


def test_structured_text_content_is_real_user_message():
    msg = {
        "role": "user",
        "content": [{"type": "text", "text": "please refactor foo"}],
    }
    assert _is_real_user_message(msg) is True


def test_structured_text_content_task_is_extracted():
    from opendaisugi.parsers.claude_code import _user_text

    msg = {
        "role": "user",
        "content": [{"type": "text", "text": "please refactor foo"}],
    }
    assert _user_text(msg) == "please refactor foo"


def test_parse_fixture_produces_three_episodes():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    assert result.source == "claude-code"
    assert len(result.episodes) == 3


MODERN_FIXTURE = Path(__file__).parent / "fixtures" / "sample_transcript_modern.jsonl"


def test_parse_modern_format_unwraps_message_field():
    """Modern Claude Code jsonl rows wrap the real message under ``message`` and
    emit many metadata row types (system, file-history-snapshot, etc.) that must
    be skipped — not just naively json.loads'd into the message stream.
    """
    parser = ClaudeCodeParser(min_tools=1, max_tools=30)
    result = parser.parse(MODERN_FIXTURE)
    assert len(result.episodes) == 1
    ep = result.episodes[0]
    assert ep.task == "Refactor the config loader"
    # Exactly the 3 tool_use blocks from the assistant turn, nothing from
    # metadata rows, nothing from the tool_result row.
    assert [s.type for s in ep.steps] == ["file_read", "shell", "file_write"]


def test_read_messages_drops_metadata_rows():
    """Only user/assistant rows should survive; metadata rows like system,
    custom-title, file-history-snapshot, agent-name are dropped.
    """
    parser = ClaudeCodeParser(min_tools=1, max_tools=30)
    msgs = parser._read_messages(MODERN_FIXTURE)
    roles = [m["role"] for m in msgs]
    assert roles == ["user", "assistant", "user"]


def test_parse_fixture_episode_ids_sequential():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    assert [ep.id for ep in result.episodes] == ["ep_00", "ep_01", "ep_02"]


def test_parse_fixture_first_episode_has_merged_tools():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    ep0 = result.episodes[0]
    # Turn 1 (Read+Write+Bash=3) merged with Turn 2 (Bash=1) = 4 tools
    assert len(ep0.steps) == 4
    assert ep0.task == "Create a Python config module that reads settings from a YAML file"


def test_parse_fixture_source_ranges():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    # Episode 0 spans from message 0 (first user msg) through message 7 (last tool_result of merged turn)
    assert result.episodes[0].source_range["first_message"] == 0
    assert result.episodes[0].source_range["last_message"] == 7
    # Episode 1 starts at message 8
    assert result.episodes[1].source_range["first_message"] == 8


def test_parse_fixture_step_types():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    ep0_types = [s.type for s in result.episodes[0].steps]
    # Turn 1: Read(file_read), Write(file_write), Bash(shell) + Turn 2: Bash(shell)
    assert ep0_types == ["file_read", "file_write", "shell", "shell"]


def test_parse_no_merge_when_min_tools_is_one():
    parser = ClaudeCodeParser(min_tools=1, max_tools=30)
    result = parser.parse(FIXTURE)
    # With min_tools=1 nothing merges: 4 raw turns = 4 episodes
    assert len(result.episodes) == 4


def test_parse_source_file_recorded():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    assert result.source_file == str(FIXTURE)


def test_parse_parsed_at_is_iso_timestamp():
    parser = ClaudeCodeParser(min_tools=3, max_tools=30)
    result = parser.parse(FIXTURE)
    # Should be ISO 8601 format
    assert "T" in result.parsed_at
    assert result.parsed_at.endswith("Z")


from unittest.mock import MagicMock, patch


def test_parse_splits_large_episode_with_llm():
    """With max_tools=2, turns with 3+ tools trigger an LLM split.

    The mock returns a single subtask covering every tool in the incoming
    batch so no steps are silently dropped.
    """
    def fake_completion(*args, **kwargs):
        user_msg = kwargs["messages"][1]["content"]
        # "Tool calls (N total):" appears in the prompt; parse N
        n = int(user_msg.split("Tool calls (")[1].split(" total")[0])
        content = (
            '{"subtasks": ['
            '{"start_index": 0, "end_index": ' + str(n // 2) + ', "task": "first half"},'
            '{"start_index": ' + str(n // 2 + 1) + ', "end_index": ' + str(n - 1) + ', "task": "second half"}'
            ']}'
        )
        fake_response = MagicMock()
        fake_response.choices = [MagicMock(message=MagicMock(content=content))]
        return fake_response

    with patch("opendaisugi.parsers.claude_code.litellm") as mock_litellm:
        mock_litellm.completion.side_effect = fake_completion
        parser = ClaudeCodeParser(min_tools=1, max_tools=2)
        result = parser.parse(FIXTURE)

    assert mock_litellm.completion.called
    # All 11 tool calls must survive the split — no data loss.
    total_steps = sum(len(ep.steps) for ep in result.episodes)
    assert total_steps == 11


def test_parse_keeps_episode_when_llm_returns_empty_subtasks():
    """If the LLM returns no subtasks, the original episode is kept intact."""
    fake_response = MagicMock()
    fake_response.choices = [MagicMock(message=MagicMock(content='{"subtasks": []}'))]
    with patch("opendaisugi.parsers.claude_code.litellm") as mock_litellm:
        mock_litellm.completion.return_value = fake_response
        parser = ClaudeCodeParser(min_tools=1, max_tools=2)
        result = parser.parse(FIXTURE)

    # All 11 tool calls from the fixture must still be present.
    total_steps = sum(len(ep.steps) for ep in result.episodes)
    assert total_steps == 11


from opendaisugi.parsers.claude_code import _validate_boundaries


def test_validate_boundaries_rejects_gapped():
    boundaries = [
        {"start_index": 0, "end_index": 1, "task": "a"},
        {"start_index": 4, "end_index": 5, "task": "b"},
    ]
    assert _validate_boundaries(boundaries, 6) is False


def test_validate_boundaries_rejects_overlapping():
    boundaries = [
        {"start_index": 0, "end_index": 3, "task": "a"},
        {"start_index": 2, "end_index": 5, "task": "b"},
    ]
    assert _validate_boundaries(boundaries, 6) is False


def test_validate_boundaries_rejects_out_of_bounds():
    boundaries = [
        {"start_index": 0, "end_index": 10, "task": "a"},
    ]
    assert _validate_boundaries(boundaries, 6) is False


def test_validate_boundaries_rejects_negative_index():
    boundaries = [
        {"start_index": -1, "end_index": 5, "task": "a"},
    ]
    assert _validate_boundaries(boundaries, 6) is False


def test_validate_boundaries_accepts_valid_contiguous():
    boundaries = [
        {"start_index": 0, "end_index": 2, "task": "a"},
        {"start_index": 3, "end_index": 5, "task": "b"},
    ]
    assert _validate_boundaries(boundaries, 6) is True


def test_validate_boundaries_rejects_missing_keys():
    boundaries = [{"task": "a"}]
    assert _validate_boundaries(boundaries, 6) is False


def test_parse_falls_back_on_gapped_llm_boundaries():
    """LLM returns non-contiguous boundaries -> episode kept unsplit, no data loss."""
    def fake_completion(*args, **kwargs):
        content = _json.dumps({"subtasks": [
            {"start_index": 0, "end_index": 0, "task": "first"},
            {"start_index": 3, "end_index": 4, "task": "skipped middle"},
        ]})
        fake_response = MagicMock()
        fake_response.choices = [MagicMock(message=MagicMock(content=content))]
        return fake_response

    with patch("opendaisugi.parsers.claude_code.litellm") as mock_litellm:
        mock_litellm.completion.side_effect = fake_completion
        parser = ClaudeCodeParser(min_tools=1, max_tools=2)
        result = parser.parse(FIXTURE)

    total_steps = sum(len(ep.steps) for ep in result.episodes)
    assert total_steps == 11


def test_split_sub_episodes_have_distinct_source_ranges():
    """After LLM split, each sub-episode carries step_start/step_end."""
    def fake_completion(*args, **kwargs):
        user_msg = kwargs["messages"][1]["content"]
        n = int(user_msg.split("Tool calls (")[1].split(" total")[0])
        mid = n // 2
        content = _json.dumps({"subtasks": [
            {"start_index": 0, "end_index": mid, "task": "first half"},
            {"start_index": mid + 1, "end_index": n - 1, "task": "second half"},
        ]})
        fake_response = MagicMock()
        fake_response.choices = [MagicMock(message=MagicMock(content=content))]
        return fake_response

    with patch("opendaisugi.parsers.claude_code.litellm") as mock_litellm:
        mock_litellm.completion.side_effect = fake_completion
        parser = ClaudeCodeParser(min_tools=1, max_tools=2)
        result = parser.parse(FIXTURE)

    split_eps = [ep for ep in result.episodes if "step_start" in ep.source_range]
    assert len(split_eps) > 0
    for ep in split_eps:
        assert isinstance(ep.source_range["step_start"], int)
        assert isinstance(ep.source_range["step_end"], int)
        assert ep.source_range["step_start"] <= ep.source_range["step_end"]

    non_split = [ep for ep in result.episodes if "step_start" not in ep.source_range]
    for ep in non_split:
        assert "first_message" in ep.source_range
        assert "step_start" not in ep.source_range


def test_parse_handles_utf8_transcript(tmp_path):
    """Transcripts with non-ASCII content parse correctly."""
    transcript = tmp_path / "utf8.jsonl"
    transcript.write_text(
        '{"role": "user", "content": "Ajouter la gestion des accents"}\n'
        '{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "caf\\u00e9.py"}}, {"type": "tool_use", "id": "t2", "name": "Edit", "input": {"file_path": "caf\\u00e9.py", "old_string": "a", "new_string": "b"}}, {"type": "tool_use", "id": "t3", "name": "Bash", "input": {"command": "echo h\\u00e9llo"}}]}\n'
        '{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]}\n',
        encoding="utf-8",
    )
    parser = ClaudeCodeParser(min_tools=1, max_tools=30)
    result = parser.parse(transcript)
    assert len(result.episodes) == 1
    assert result.episodes[0].task == "Ajouter la gestion des accents"


# ----- Public API exports -----


def test_episode_importable_from_top_level():
    from opendaisugi import Episode
    assert Episode is not None


def test_parse_result_importable_from_top_level():
    from opendaisugi import ParseResult
    assert ParseResult is not None
