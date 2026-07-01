"""Ingest pipeline: generate envelopes, verify, and log traces for parsed episodes."""

from __future__ import annotations

import asyncio
import hashlib
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from opendaisugi.envelope import generate_envelope
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan
from opendaisugi.parsers import Episode, ParseResult
from opendaisugi.verify import verify

if TYPE_CHECKING:
    from opendaisugi.tier1 import Tier1Provider


@dataclass
class EpisodeResult:
    """Per-episode outcome reported by the ingest pipeline."""

    episode_id: str
    task: str
    status: str  # "OK", "FAIL", "SKIP", "DRY-RUN", "ERROR"
    steps: int = 0
    violations: int = 0
    error: str | None = None


@dataclass
class IngestSummary:
    """Aggregate results from an ingest run."""

    total: int = 0
    passed: int = 0
    failed: int = 0
    skipped: int = 0
    errored: int = 0
    episodes: list[EpisodeResult] = field(default_factory=list)


def _source_prefix(source_file: str) -> str:
    """8-char stable hash of the source transcript path.

    Why: episode ids are per-transcript (ep_00, ep_01, ...) so without a
    source-scoped prefix, two transcripts produce colliding trace ids and
    the second ingest silently skips everything as already-imported.
    """
    return hashlib.sha256(source_file.encode()).hexdigest()[:8]


def _trace_id_for(source_file: str, episode_id: str) -> str:
    """Deterministic trace ID for idempotent ingest."""
    return f"import-{_source_prefix(source_file)}-{episode_id}"


def _is_already_ingested(
    journal: Journal, source_file: str, episode_id: str
) -> bool:
    """Check if an episode was already ingested by looking for its trace."""
    try:
        journal.load_trace(_trace_id_for(source_file, episode_id))
        return True
    except FileNotFoundError:
        return False


async def _process_episode(
    episode: Episode,
    journal: Journal,
    source_file: str,
    model: str,
    dry_run: bool,
    tier1: "Tier1Provider | None" = None,
) -> EpisodeResult:
    """Process a single episode: generate envelope, verify, log."""
    n_steps = len(episode.steps)

    if _is_already_ingested(journal, source_file, episode.id):
        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="SKIP",
            steps=n_steps,
        )

    if dry_run:
        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="DRY-RUN",
            steps=n_steps,
        )

    try:
        envelope = await generate_envelope(
            task=episode.task,
            context=episode.context,
            model=model,
            tier1=tier1,
        )

        plan = ActionPlan(
            source="claude-code-import",
            task=episode.task,
            steps=episode.steps,
        )

        result = verify(plan, envelope)

        journal.log(
            task=episode.task,
            envelope=envelope,
            plan=plan,
            result=result,
            trace_id=_trace_id_for(source_file, episode.id),
        )

        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="OK" if result.ok else "FAIL",
            steps=n_steps,
            violations=len(result.violations),
        )
    except Exception as exc:
        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="ERROR",
            steps=n_steps,
            error=str(exc),
        )


async def ingest_episodes(
    parse_result: ParseResult,
    journal: Journal,
    *,
    concurrency: int = 5,
    model: str = "anthropic/claude-sonnet-4-20250514",
    dry_run: bool = False,
    tier1: "Tier1Provider | None" = None,
) -> IngestSummary:
    """Ingest parsed episodes into the journal.

    For each episode: check idempotency, generate envelope, verify plan,
    and log trace. Failed verifications are still logged (they are valuable
    data for the compilation loop).

    Returns an ``IngestSummary`` with per-episode results.
    """
    sem = asyncio.Semaphore(concurrency)
    source_file = parse_result.source_file

    async def bounded(episode: Episode) -> EpisodeResult:
        async with sem:
            return await _process_episode(
                episode, journal, source_file, model, dry_run, tier1
            )

    tasks = [bounded(ep) for ep in parse_result.episodes]
    results = await asyncio.gather(*tasks)

    summary = IngestSummary(total=len(results))
    for r in results:
        summary.episodes.append(r)
        if r.status == "OK":
            summary.passed += 1
        elif r.status == "FAIL":
            summary.failed += 1
        elif r.status == "SKIP":
            summary.skipped += 1
        elif r.status == "ERROR":
            summary.errored += 1

    return summary
