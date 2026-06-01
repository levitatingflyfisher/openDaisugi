"""Tests for opendaisugi.ingest — episode ingest pipeline."""

import pytest
from unittest.mock import AsyncMock, patch

from opendaisugi.ingest import (
    ingest_episodes,
    IngestSummary,
    EpisodeResult,
    _trace_id_for,
)
from opendaisugi.journal import Journal
from opendaisugi.models import ShellStep, Envelope, Permission
from opendaisugi.parsers import Episode, ParseResult


def _make_episode(ep_id: str, task: str, num_steps: int = 2) -> Episode:
    return Episode(
        id=ep_id,
        task=task,
        steps=[
            ShellStep(id=f"s{i}", command=f"echo step{i}")
            for i in range(num_steps)
        ],
        source_range={"first_message": 0, "last_message": 5},
    )


def _make_parse_result(*episodes: Episode) -> ParseResult:
    return ParseResult(
        source="claude-code",
        source_file="/tmp/test.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=list(episodes),
    )


def _fake_envelope(task: str, **kwargs) -> Envelope:
    return Envelope(
        generated_by="test",
        task=task,
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


@pytest.fixture
def journal(tmp_path):
    return Journal(data_dir=tmp_path)


async def test_ingest_happy_path(journal):
    pr = _make_parse_result(
        _make_episode("ep_00", "Task one"),
        _make_episode("ep_01", "Task two"),
    )
    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_envelope(task)
        summary = await ingest_episodes(pr, journal)

    assert isinstance(summary, IngestSummary)
    assert summary.total == 2
    assert summary.passed + summary.failed == 2
    assert summary.skipped == 0
    assert summary.errored == 0
    # Journal should have 2 traces
    assert journal.stats().total == 2


async def test_ingest_idempotent_skips_existing(journal):
    pr = _make_parse_result(_make_episode("ep_00", "Task one"))

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_envelope(task)
        await ingest_episodes(pr, journal)
        # Second run should skip
        summary = await ingest_episodes(pr, journal)

    assert summary.total == 1
    assert summary.skipped == 1
    assert summary.passed == 0
    assert mock_gen.call_count == 1  # Only called once (first run)


async def test_ingest_dry_run_writes_nothing(journal):
    pr = _make_parse_result(_make_episode("ep_00", "Task one"))

    summary = await ingest_episodes(pr, journal, dry_run=True)

    assert summary.total == 1
    assert journal.stats().total == 0
    # Each episode reported as DRY-RUN
    assert summary.episodes[0].status == "DRY-RUN"


async def test_ingest_partial_failure_continues(journal):
    pr = _make_parse_result(
        _make_episode("ep_00", "Good task"),
        _make_episode("ep_01", "Bad task"),
        _make_episode("ep_02", "Another good task"),
    )

    async def flaky_gen(task, **kw):
        if "Bad" in task:
            raise RuntimeError("LLM timeout")
        return _fake_envelope(task)

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock, side_effect=flaky_gen):
        summary = await ingest_episodes(pr, journal)

    assert summary.total == 3
    assert summary.errored == 1
    assert summary.passed + summary.failed == 2
    # Error episode recorded with error message
    error_ep = [e for e in summary.episodes if e.status == "ERROR"][0]
    assert "LLM timeout" in error_ep.error


async def test_ingest_uses_claude_code_import_source(journal):
    pr = _make_parse_result(_make_episode("ep_00", "Task"))

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_envelope(task)
        await ingest_episodes(pr, journal)

    record = journal.load_trace(_trace_id_for("/tmp/test.jsonl", "ep_00"))
    assert record.plan.source == "claude-code-import"


async def test_trace_ids_are_source_scoped(journal):
    """Two transcripts with colliding episode ids must not share trace ids."""
    pr_a = ParseResult(
        source="claude-code",
        source_file="/tmp/a.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=[_make_episode("ep_00", "Task from A")],
    )
    pr_b = ParseResult(
        source="claude-code",
        source_file="/tmp/b.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=[_make_episode("ep_00", "Task from B")],
    )

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_envelope(task)
        sa = await ingest_episodes(pr_a, journal)
        sb = await ingest_episodes(pr_b, journal)

    # Both episodes ingested (no silent skip from colliding trace ids).
    assert sa.skipped == 0
    assert sb.skipped == 0
    assert journal.stats().total == 2
    # And they produced distinct trace ids.
    id_a = _trace_id_for("/tmp/a.jsonl", "ep_00")
    id_b = _trace_id_for("/tmp/b.jsonl", "ep_00")
    assert id_a != id_b
    assert journal.load_trace(id_a).task == "Task from A"
    assert journal.load_trace(id_b).task == "Task from B"


async def test_ingest_concurrency_parameter(journal):
    """The semaphore bounds simultaneous envelope generations to `concurrency`."""
    episodes = [_make_episode(f"ep_{i:02d}", f"Task {i}") for i in range(6)]
    pr = _make_parse_result(*episodes)

    import asyncio as _asyncio
    in_flight = 0
    max_in_flight = 0
    lock = _asyncio.Lock()

    async def tracked_gen(task, **kw):
        nonlocal in_flight, max_in_flight
        async with lock:
            in_flight += 1
            if in_flight > max_in_flight:
                max_in_flight = in_flight
        # Yield to the event loop so other coroutines can enter.
        await _asyncio.sleep(0.01)
        async with lock:
            in_flight -= 1
        return _fake_envelope(task)

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock, side_effect=tracked_gen):
        summary = await ingest_episodes(pr, journal, concurrency=2)

    assert summary.total == 6
    assert summary.passed + summary.failed == 6
    # With concurrency=2, at most 2 generate_envelope calls should run at once.
    assert max_in_flight == 2
