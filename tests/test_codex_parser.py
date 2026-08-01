"""The Codex rollout parser — Codex transcripts become onboardable episodes.

Codex persists sessions as rollout JSONL under ``~/.codex/sessions/YYYY/MM/DD``:
each line wraps one item (``session_meta``, ``response_item``, ``event_msg``,
``turn_context``, …). The parser translates response items into the flat
message shape the shared episode pipeline already understands — shell calls
(both ``function_call name=shell`` with its JSON-string arguments and the
older ``local_shell_call`` argv form) become Bash tool_use blocks with the
``bash -lc`` wrapper unwrapped, ``apply_patch`` becomes Write blocks (one per
file named in the patch), messages become user/assistant turns — so episode
boundaries, min-tools merging, compound-shell decomposition, and step typing
are all inherited rather than reimplemented.
"""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.models import FileWriteStep, ShellStep
from opendaisugi.parsers import get_parser

FIXTURE = Path(__file__).parent / "fixtures" / "codex" / "rollout-sample.jsonl"


def _parse(path: Path = FIXTURE, **kw):
    kw.setdefault("min_tools", 1)
    return get_parser("codex", **kw).parse(path)


def test_codex_parser_is_registered():
    parser = get_parser("codex", min_tools=1)
    assert parser is not None


def test_parse_result_source_is_codex():
    result = _parse()
    assert result.source == "codex"
    assert result.source_file.endswith("rollout-sample.jsonl")


def test_episodes_split_at_user_messages():
    result = _parse()
    assert len(result.episodes) == 2
    assert result.episodes[0].task == "set up the project and run the tests"
    assert result.episodes[1].task == "now deploy it"


def test_shell_function_call_unwraps_bash_lc():
    ep = _parse().episodes[0]
    commands = [s.command for s in ep.steps if isinstance(s, ShellStep)]
    # 'cd /repo && pytest -q' may arrive whole or pre-decomposed by the
    # inherited compound splitter — either way both halves must be present.
    joined = " && ".join(commands)
    assert "pytest -q" in joined and "cd /repo" in joined
    assert not any("bash -lc" in c or "'-lc'" in c for c in commands)


def test_local_shell_call_argv_becomes_shell_step():
    ep = _parse().episodes[0]
    commands = [s.command for s in ep.steps if isinstance(s, ShellStep)]
    assert "git status" in commands


def test_apply_patch_becomes_file_writes():
    result = _parse()
    ep1_writes = [s.path for s in result.episodes[0].steps if isinstance(s, FileWriteStep)]
    ep2_writes = [s.path for s in result.episodes[1].steps if isinstance(s, FileWriteStep)]
    assert ep1_writes == ["src/app.py"]
    assert ep2_writes == ["notes.md"]


def test_unknown_tools_are_dropped_not_fatal():
    ep = _parse().episodes[1]
    # update_plan is bookkeeping — no step, no crash.
    kinds = [s.type for s in ep.steps]
    assert kinds.count("shell") == 1
    assert kinds.count("file_write") == 1


def test_event_msg_user_message_does_not_double_boundary():
    # The fixture carries the same first task as BOTH an event_msg
    # user_message and a response_item message — one episode, not two.
    result = _parse()
    assert len(result.episodes) == 2


def test_item_wrapped_lines_parse_identically(tmp_path):
    # Tolerate the {"timestamp", "item": {...}} wrapping some builds emit.
    rewrapped = tmp_path / "rollout-rewrapped.jsonl"
    with open(FIXTURE) as f, open(rewrapped, "w") as out:
        for line in f:
            d = json.loads(line)
            out.write(
                json.dumps(
                    {
                        "timestamp": d["timestamp"],
                        "item": {"type": d["type"], "payload": d["payload"]},
                    }
                )
                + "\n"
            )
    a, b = _parse(), _parse(rewrapped)
    assert [e.task for e in a.episodes] == [e.task for e in b.episodes]
    assert [len(e.steps) for e in a.episodes] == [len(e.steps) for e in b.episodes]


def test_codex_episode_ingests_end_to_end(tmp_path):
    """The point of it all: a Codex session round-trips through real ingest."""
    import asyncio

    from opendaisugi.ingest import ingest_episodes
    from opendaisugi.journal import Journal

    result = _parse()
    journal = Journal(data_dir=tmp_path)
    summary = asyncio.run(ingest_episodes(result, journal, allow_shell_decomposition=True))
    assert summary.total == 2
    assert summary.passed == 2, [e.error or e.status for e in summary.episodes]
