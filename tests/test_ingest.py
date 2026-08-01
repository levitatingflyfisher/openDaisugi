"""Tests for opendaisugi.ingest — episode ingest pipeline.

Envelopes are inferred deterministically from each episode's observed steps
(ADR-0016) — no LLM, so no mocks: these tests run the real pipeline offline.
"""

import pytest

from opendaisugi.ingest import (
    IngestSummary,
    _trace_id_for,
    ingest_episodes,
)
from opendaisugi.journal import Journal
from opendaisugi.models import ShellStep
from opendaisugi.parsers import Episode, ParseResult


def _make_episode(ep_id: str, task: str, num_steps: int = 2) -> Episode:
    return Episode(
        id=ep_id,
        task=task,
        steps=[ShellStep(id=f"s{i}", command=f"echo step{i}") for i in range(num_steps)],
        source_range={"first_message": 0, "last_message": 5},
    )


def _make_parse_result(*episodes: Episode) -> ParseResult:
    return ParseResult(
        source="claude-code",
        source_file="/tmp/test.jsonl",
        parsed_at="2026-04-10T12:00:00Z",
        episodes=list(episodes),
    )


@pytest.fixture
def journal(tmp_path):
    return Journal(data_dir=tmp_path)


async def test_ingest_happy_path(journal):
    pr = _make_parse_result(
        _make_episode("ep_00", "Task one"),
        _make_episode("ep_01", "Task two"),
    )
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

    await ingest_episodes(pr, journal)
    # Second run should skip
    summary = await ingest_episodes(pr, journal)

    assert summary.total == 1
    assert summary.skipped == 1
    assert summary.passed == 0
    assert journal.stats().total == 1  # first run's trace only


async def test_ingest_dry_run_previews_verdict_but_writes_nothing(journal):
    pr = _make_parse_result(_make_episode("ep_00", "Task one"))

    summary = await ingest_episodes(pr, journal, dry_run=True)

    assert summary.total == 1
    assert journal.stats().total == 0  # nothing logged
    # The deterministic infer+verify still runs, so the preview reports a real
    # verdict (OK/FAIL) rather than short-circuiting to a placeholder status.
    assert summary.episodes[0].status in {"OK", "FAIL"}
    assert summary.passed + summary.failed == 1


async def test_ingest_dry_run_skips_episodes_too_large_to_split(journal):
    # An episode with more steps than preview_max_steps would be LLM-split in a
    # real run; a dry-run can't split, so it is skipped (not verified whole).
    big = _make_episode("ep_00", "Huge task", num_steps=5)
    pr = _make_parse_result(big)

    summary = await ingest_episodes(pr, journal, dry_run=True, preview_max_steps=2)

    assert summary.episodes[0].status == "TOO-LARGE"
    assert summary.preview_skipped == 1
    assert summary.passed == 0 and summary.failed == 0
    assert journal.stats().total == 0


async def test_ingest_partial_failure_continues(journal):
    pr = _make_parse_result(
        _make_episode("ep_00", "Good task"),
        _make_episode("ep_01", "Bad task"),
        _make_episode("ep_02", "Another good task"),
    )

    import opendaisugi.ingest as ingest_mod

    real_infer = ingest_mod.infer_envelope

    def flaky_infer(records, *, task="", **kw):
        if "Bad" in task:
            raise RuntimeError("inference exploded")
        return real_infer(records, task=task, **kw)

    monkeypatch = pytest.MonkeyPatch()
    try:
        monkeypatch.setattr(ingest_mod, "infer_envelope", flaky_infer)
        summary = await ingest_episodes(pr, journal)
    finally:
        monkeypatch.undo()

    assert summary.total == 3
    assert summary.errored == 1
    assert summary.passed + summary.failed == 2
    # Error episode recorded with error message
    error_ep = [e for e in summary.episodes if e.status == "ERROR"][0]
    assert "inference exploded" in error_ep.error


async def test_ingest_uses_claude_code_import_source(journal):
    pr = _make_parse_result(_make_episode("ep_00", "Task"))

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


# --- deterministic envelope inference (ADR-0016) --------------------------------


async def test_ingest_infers_envelope_from_observed_steps(journal):
    """Bulk onboarding must not ask an LLM to guess an allowlist from task text.

    The episode IS the evidence: every observed head (through decomposition),
    every path, every URL. No mock here — the test passing offline proves the
    pipeline makes zero LLM calls.
    """
    from opendaisugi.models import FileWriteStep

    ep = Episode(
        id="ep_infer",
        task="count matches and save them",
        steps=[
            ShellStep(id="s1", command="grep -n foo src/*.py | head -5 > out/hits.txt"),
            FileWriteStep(id="s2", path="out/summary.md", content="x", depends_on=["s1"]),
        ],
        source_range={"first_message": 0, "last_message": 5},
    )
    summary = await ingest_episodes(
        _make_parse_result(ep), journal, allow_shell_decomposition=True
    )
    assert summary.passed == 1, [e.error or e.status for e in summary.episodes]
    record = journal.load_trace(_trace_id_for("/tmp/test.jsonl", "ep_infer"))
    assert "infer" in record.envelope.generated_by
    assert "grep" in record.envelope.permissions.shell_allowlist
    assert "head" in record.envelope.permissions.shell_allowlist


async def test_ingest_inference_stays_fail_closed_on_nonliteral(journal):
    """Inference admits what was observed — it cannot admit what cannot be read.

    A ``$CMD`` head is non-literal: no envelope can authorize it, so the
    episode must land FAIL (and still be journaled as distillation data),
    never rubber-stamped OK.
    """
    ep = Episode(
        id="ep_nonlit",
        task="run the configured tool",
        steps=[ShellStep(id="s1", command="$CMD --do-things && ls")],
        source_range={"first_message": 0, "last_message": 5},
    )
    summary = await ingest_episodes(
        _make_parse_result(ep), journal, allow_shell_decomposition=True
    )
    assert summary.failed == 1
    assert summary.passed == 0
