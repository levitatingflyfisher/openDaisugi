"""Tests for the LLM episode-split cache (avoids re-billing splits on re-onboard)."""

from __future__ import annotations

from pathlib import Path
from unittest.mock import MagicMock, patch

from opendaisugi.parsers.claude_code import ClaudeCodeParser
from opendaisugi.split_cache import SplitCache


def _big_episode_transcript(tmp_path: Path, n: int = 6) -> Path:
    tools = ",".join(
        '{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"echo %d"}}' % (i, i)
        for i in range(n)
    )
    t = tmp_path / "s.jsonl"
    t.write_text(
        '{"role":"user","content":"a big task"}\n'
        '{"role":"assistant","content":[' + tools + "]}\n",
        encoding="utf-8",
    )
    return t


# ---- store ----


def test_split_cache_roundtrip(tmp_path):
    c = SplitCache(tmp_path / "s.db", prompt_version="v1")
    assert c.get(model="haiku", content="abc") is None
    c.put([{"start_index": 0, "end_index": 2, "task": "x"}], model="haiku", content="abc")
    got = c.get(model="haiku", content="abc")
    assert got == [{"start_index": 0, "end_index": 2, "task": "x"}]


def test_split_cache_empty_list_is_a_hit_not_a_miss(tmp_path):
    c = SplitCache(tmp_path / "s.db", prompt_version="v1")
    c.put([], model="haiku", content="abc")
    assert c.get(model="haiku", content="abc") == []  # not None


def test_split_cache_evicts_on_prompt_version_change(tmp_path):
    db = tmp_path / "s.db"
    SplitCache(db, prompt_version="v1").put([], model="m", content="abc")
    c2 = SplitCache(db, prompt_version="v2")  # different prompt version
    assert c2.get(model="m", content="abc") is None
    assert c2.stats()["evicted_on_init"] == 1


# ---- parser integration ----


def _fake_completion_factory(counter):
    def fake(*a, **k):
        counter["n"] += 1
        content = '{"subtasks":[{"start_index":0,"end_index":5,"task":"all"}]}'
        r = MagicMock()
        r.choices = [MagicMock(message=MagicMock(content=content))]
        return r

    return fake


def test_second_parse_hits_cache_across_instances(tmp_path):
    """A fresh SplitCache pointed at the same db (a later `onboard` run) must
    reuse the split — no second model call."""
    t = _big_episode_transcript(tmp_path)
    counter = {"n": 0}
    db = tmp_path / "split.db"

    with patch("opendaisugi.parsers.claude_code.litellm") as ml:
        ml.completion.side_effect = _fake_completion_factory(counter)

        cache1 = SplitCache(db, prompt_version="v1")
        ClaudeCodeParser(min_tools=1, max_tools=2, split_cache=cache1).parse(t)
        assert counter["n"] == 1  # first run calls the splitter

        cache2 = SplitCache(db, prompt_version="v1")  # simulates a later run
        ClaudeCodeParser(min_tools=1, max_tools=2, split_cache=cache2).parse(t)
        assert counter["n"] == 1, "second run must hit the split cache, not re-call the model"


def test_no_cache_means_every_parse_calls_the_splitter(tmp_path):
    t = _big_episode_transcript(tmp_path)
    counter = {"n": 0}
    with patch("opendaisugi.parsers.claude_code.litellm") as ml:
        ml.completion.side_effect = _fake_completion_factory(counter)
        ClaudeCodeParser(min_tools=1, max_tools=2).parse(t)
        ClaudeCodeParser(min_tools=1, max_tools=2).parse(t)
    assert counter["n"] == 2  # no cache -> both parses call the splitter


def test_split_failure_is_not_cached(tmp_path, monkeypatch):
    """A failed split (model unavailable) must not be cached as 'no split'."""
    t = _big_episode_transcript(tmp_path)
    cache = SplitCache(tmp_path / "split.db", prompt_version="v1")
    # Simulate the splitter failing (returns None -> keep episode unsplit).
    monkeypatch.setattr(ClaudeCodeParser, "_call_split_llm", lambda self, content: None)
    ClaudeCodeParser(min_tools=1, max_tools=2, split_cache=cache).parse(t)
    assert cache.stats()["entries"] == 0
