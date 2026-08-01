"""Ingest pipeline: generate envelopes, verify, and log traces for parsed episodes."""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field

from opendaisugi.hook import infer_envelope
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan
from opendaisugi.parsers import Episode, ParseResult
from opendaisugi.verify import verify


@dataclass
class EpisodeResult:
    """Per-episode outcome reported by the ingest pipeline."""

    episode_id: str
    task: str
    status: str  # "OK", "FAIL", "SKIP", "TOO-LARGE", "ERROR" (dry-run previews OK/FAIL, logs nothing)
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
    preview_skipped: int = 0  # dry-run only: too large to preview without the splitter
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


def _is_already_ingested(journal: Journal, source_file: str, episode_id: str) -> bool:
    """Check if an episode was already ingested by looking for its trace."""
    try:
        journal.load_trace(_trace_id_for(source_file, episode_id))
        return True
    except FileNotFoundError:
        return False


def _records_for_steps(steps: list) -> list[dict]:
    """Adapt typed episode steps to ``infer_envelope``'s capture-record shape.

    Step kinds without an evidence record (task, agentic, skill, custom)
    contribute nothing here; verification decides what they need.
    """
    records: list[dict] = []
    for step in steps:
        kind = getattr(step, "type", None)
        if kind == "shell":
            records.append({"step_type": "shell", "command": step.command})
        elif kind == "file_read":
            records.append({"step_type": "file_read", "path": step.path})
        elif kind == "file_write":
            records.append({"step_type": "file_write", "path": step.path})
        elif kind == "network":
            records.append({"step_type": "network", "url": step.url})
        elif kind == "mcp":
            records.append(
                {"step_type": "mcp", "mcp_server": step.server, "mcp_tool": step.tool}
            )
    return records


async def _process_episode(
    episode: Episode,
    journal: Journal,
    source_file: str,
    dry_run: bool,
    allow_shell_decomposition: bool = False,
    preview_max_steps: int | None = None,
) -> EpisodeResult:
    """Process a single episode: infer envelope from its steps, verify, log.

    A ``dry_run`` runs the SAME deterministic infer+verify (ADR-0016 — zero LLM,
    zero embedder) so the operator sees what would pass or fail; it just never
    logs to the journal. An episode with more than ``preview_max_steps`` steps
    would be LLM-split into sub-episodes in a real run — a dry-run makes no model
    calls and so cannot split it, and verifying the un-split whole would give a
    misleading verdict — so it is skipped from the preview rather than reported
    wrong.
    """
    n_steps = len(episode.steps)

    if _is_already_ingested(journal, source_file, episode.id):
        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="SKIP",
            steps=n_steps,
        )

    if dry_run and preview_max_steps is not None and n_steps > preview_max_steps:
        return EpisodeResult(
            episode_id=episode.id,
            task=episode.task,
            status="TOO-LARGE",  # would be LLM-split in a real run; not previewable
            steps=n_steps,
        )

    try:
        # The episode IS the evidence: infer the envelope from its observed
        # steps (every head through decomposition, every path, every URL)
        # instead of asking an LLM to guess an allowlist from task text.
        # Deterministic, zero tokens, and exactly what the verifier checks —
        # while staying fail-closed: non-literal constructs ($CMD heads,
        # $VAR redirect targets) are uninferable, so those episodes still
        # land FAIL and are journaled as distillation data. ADR-0016.
        envelope = infer_envelope(
            _records_for_steps(episode.steps),
            task=episode.task,
            allow_shell_decomposition=allow_shell_decomposition,
        )

        plan = ActionPlan(
            source="claude-code-import",
            task=episode.task,
            steps=episode.steps,
        )

        result = verify(plan, envelope)

        if not dry_run:  # a dry-run previews the verdict but writes no trace
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
    dry_run: bool = False,
    allow_shell_decomposition: bool = False,
    preview_max_steps: int | None = None,
) -> IngestSummary:
    """Ingest parsed episodes into the journal.

    For each episode: check idempotency, infer the envelope from the episode's
    own observed steps (ADR-0016 — deterministic, zero LLM calls), verify the
    plan against it, and log the trace. Failed verifications are still logged
    (they are valuable data for the compilation loop).

    Under ``dry_run`` the same deterministic infer+verify runs so the caller gets
    a real would-pass/would-fail preview, but nothing is logged and (when set)
    ``preview_max_steps`` skips episodes too large to preview without the LLM
    splitter. Returns an ``IngestSummary`` with per-episode results.

    ``allow_shell_decomposition`` opts every inferred envelope into ADR-0010
    and widens head collection to match (see ``infer_envelope``): with both
    halves, an episode fails only when it genuinely cannot be authorized —
    non-literal heads or redirect targets, parse errors, opaque wrappers —
    never because an LLM-guessed allowlist missed the plumbing.
    """
    source_file = parse_result.source_file
    # A dry-run writes nothing — including the conformance corpus, which
    # ``decompose_command`` appends to when OPENDAISUGI_CONFORMANCE_RECORD is set.
    # Suppress it for the preview's infer+verify so "dry" stays dry. NOTE: this
    # mutates process-global env for the duration; safe because onboarding ingests
    # transcripts sequentially — a concurrent ingest in the same process would
    # transiently lose its recording flag.
    prev_record = os.environ.pop("OPENDAISUGI_CONFORMANCE_RECORD", None) if dry_run else None
    try:
        results = [
            await _process_episode(
                episode,
                journal,
                source_file,
                dry_run,
                allow_shell_decomposition=allow_shell_decomposition,
                preview_max_steps=preview_max_steps,
            )
            for episode in parse_result.episodes
        ]
    finally:
        if prev_record is not None:
            os.environ["OPENDAISUGI_CONFORMANCE_RECORD"] = prev_record

    summary = IngestSummary(total=len(results))
    for r in results:
        summary.episodes.append(r)
        if r.status == "OK":
            summary.passed += 1
        elif r.status == "FAIL":
            summary.failed += 1
        elif r.status == "SKIP":
            summary.skipped += 1
        elif r.status == "TOO-LARGE":
            summary.preview_skipped += 1
        elif r.status == "ERROR":
            summary.errored += 1

    return summary
