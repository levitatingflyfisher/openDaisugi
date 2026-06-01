"""Day-one onboarding: discover existing harness transcripts to bulk-distill.

A new adopter typically already has months of conversation history sitting in
their agent harness's data directory — Claude Code under ``~/.claude/projects``,
Codex under ``~/.codex/sessions``, and so on. ``discover_transcripts`` finds
those ``.jsonl`` transcripts so ``daisugi onboard`` can replay them into the
journal and distill pathways, delivering token-saving routing from day one
without the user hand-listing thousands of files.

Discovery is read-only and never raises on a missing directory: an absent
harness root simply contributes no transcripts.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Awaitable, Callable

from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

if TYPE_CHECKING:
    from opendaisugi.ingest import IngestSummary
    from opendaisugi.parsers import ParseResult

_log = logging.getLogger("opendaisugi.onboarding")

# Standard per-harness transcript locations, relative to the user's home dir.
# Each maps a harness id to the directory we glob ``**/*.jsonl`` under.
_HARNESS_SUBPATHS: dict[str, tuple[str, ...]] = {
    "claude-code": (".claude", "projects"),
    "codex": (".codex", "sessions"),
}

_ENV_ROOTS = "OPENDAISUGI_TRANSCRIPT_ROOTS"


@dataclass(frozen=True)
class DiscoveredTranscript:
    """One transcript file found on disk, with enough metadata to triage."""

    path: Path
    harness: str
    size: int
    mtime: float


def default_transcript_roots(home: Path | None = None) -> dict[str, Path]:
    """Return the default ``{harness: root_dir}`` map to search.

    Built from the known per-harness home-relative locations, then extended /
    overridden by the ``OPENDAISUGI_TRANSCRIPT_ROOTS`` environment variable
    (colon-separated ``harness=path`` entries; a bare ``path`` is registered
    under harness id ``custom``).
    """
    home = home if home is not None else Path.home()
    roots: dict[str, Path] = {
        harness: home.joinpath(*parts) for harness, parts in _HARNESS_SUBPATHS.items()
    }
    env = os.environ.get(_ENV_ROOTS, "").strip()
    if env:
        for entry in env.split(os.pathsep):
            entry = entry.strip()
            if not entry:
                continue
            if "=" in entry:
                harness, _, path = entry.partition("=")
                roots[harness.strip() or "custom"] = Path(path.strip()).expanduser()
            else:
                roots["custom"] = Path(entry).expanduser()
    return roots


def discover_transcripts(
    roots: dict[str, Path] | None = None,
) -> list[DiscoveredTranscript]:
    """Find all non-empty ``*.jsonl`` transcripts under the given harness roots.

    ``roots`` defaults to :func:`default_transcript_roots`. Missing directories
    are skipped silently. Results are de-duplicated by resolved path and sorted
    newest-first (so ``--limit`` keeps the most recent work).
    """
    if roots is None:
        roots = default_transcript_roots()

    seen: set[Path] = set()
    found: list[DiscoveredTranscript] = []
    for harness, root in roots.items():
        if not root.exists() or not root.is_dir():
            continue
        for path in root.rglob("*.jsonl"):
            if not path.is_file():
                continue
            resolved = path.resolve()
            if resolved in seen:
                continue
            try:
                stat = path.stat()
            except OSError:
                continue
            if stat.st_size == 0:
                continue
            seen.add(resolved)
            found.append(
                DiscoveredTranscript(
                    path=path, harness=harness, size=stat.st_size, mtime=stat.st_mtime
                )
            )

    found.sort(key=lambda t: t.mtime, reverse=True)
    return found


@dataclass
class StatusReport:
    """Day-one readiness snapshot: are token savings live and is trust in place?"""

    data_dir: Path
    search_extra_installed: bool
    pathway_count: int
    pathway_hits: int
    retrieval_threshold: float
    journal_total: int
    journal_passed: int
    journal_failed: int

    @property
    def token_savings_ready(self) -> bool:
        # Pathways only save tokens if the embedder is present AND pathways exist.
        return self.search_extra_installed and self.pathway_count > 0

    @property
    def trust_ready(self) -> bool:
        return self.journal_total > 0


def gather_status(
    data_dir: Path, *, threshold: float = DEFAULT_PATHWAY_THRESHOLD
) -> StatusReport:
    """Read pathway-store + journal state under ``data_dir`` into a StatusReport.

    Read-only and resilient: a missing store or journal reports zeros rather
    than raising, so ``daisugi status`` works before the first ``onboard``.
    """
    import importlib.util

    search_installed = importlib.util.find_spec("sentence_transformers") is not None

    pathway_count = 0
    pathway_hits = 0
    db = data_dir / "pathways.db"
    if db.exists():
        try:
            from opendaisugi.pathway_store import PathwayStore

            stats = PathwayStore(db).stats()
            pathway_count = int(stats.get("count", 0))
            pathway_hits = int(stats.get("total_hits", 0))
        except Exception as exc:  # corrupt/locked store shouldn't break status
            _log.warning("status: could not read pathway store: %s", exc)

    journal_total = journal_passed = journal_failed = 0
    try:
        from opendaisugi.journal import Journal

        jstats = Journal(data_dir=data_dir).stats()
        journal_total, journal_passed, journal_failed = (
            jstats.total, jstats.passed, jstats.failed,
        )
    except Exception as exc:
        _log.warning("status: could not read journal: %s", exc)

    return StatusReport(
        data_dir=data_dir,
        search_extra_installed=search_installed,
        pathway_count=pathway_count,
        pathway_hits=pathway_hits,
        retrieval_threshold=threshold,
        journal_total=journal_total,
        journal_passed=journal_passed,
        journal_failed=journal_failed,
    )


@dataclass
class OnboardReport:
    """Outcome of an ``onboard`` run — what a new adopter got from day one."""

    transcripts_found: int = 0
    transcripts_processed: int = 0
    by_harness: dict[str, int] = field(default_factory=dict)
    episodes_total: int = 0
    traces_passed: int = 0
    traces_failed: int = 0
    traces_skipped: int = 0
    traces_errored: int = 0
    pathways_created: int = 0
    pathways_updated: int = 0
    dry_run: bool = False
    warnings: list[str] = field(default_factory=list)


async def onboard(
    *,
    parse_one: "Callable[[DiscoveredTranscript], ParseResult | None]",
    ingest_one: "Callable[[ParseResult], Awaitable[IngestSummary]]",
    run_tend: "Callable[[], Awaitable[object]]",
    discover: "Callable[..., list[DiscoveredTranscript]]" = discover_transcripts,
    roots: dict[str, Path] | None = None,
    harnesses: list[str] | None = None,
    limit: int | None = None,
    dry_run: bool = False,
    progress: "Callable[[str], None] | None" = None,
) -> OnboardReport:
    """Discover transcripts, replay them into the journal, and distill pathways.

    The heavy lifting (parse, envelope generation, distillation) is supplied as
    injectable callables so this orchestration is testable without LLM calls and
    so the CLI can wire in dry-run / model / concurrency choices. ``run_tend`` is
    skipped under ``dry_run`` (distillation writes pathways and calls the LLM).
    """

    def _say(msg: str) -> None:
        _log.info(msg)
        if progress is not None:
            progress(msg)

    report = OnboardReport(dry_run=dry_run)

    transcripts = discover(roots=roots)
    if harnesses:
        wanted = set(harnesses)
        transcripts = [t for t in transcripts if t.harness in wanted]
    report.transcripts_found = len(transcripts)

    if limit is not None:
        transcripts = transcripts[:limit]

    if not transcripts:
        report.warnings.append(
            "no transcripts discovered — check OPENDAISUGI_TRANSCRIPT_ROOTS or "
            "your harness data directory"
        )
        _say("onboard: no transcripts discovered; nothing to distill")
        return report

    _say(f"onboard: processing {len(transcripts)} transcript(s)")
    for t in transcripts:
        try:
            parsed = parse_one(t)
        except Exception as exc:  # one bad transcript shouldn't abort onboarding
            report.warnings.append(f"parse failed for {t.path}: {exc}")
            _log.warning("onboard: parse failed for %s: %s", t.path, exc)
            continue

        if parsed is None:  # no parser registered for this harness — skip, don't fail
            report.warnings.append(f"no parser for harness {t.harness!r}: {t.path.name}")
            continue

        report.transcripts_processed += 1
        report.by_harness[t.harness] = report.by_harness.get(t.harness, 0) + 1
        report.episodes_total += len(parsed.episodes)

        summary = await ingest_one(parsed)
        report.traces_passed += getattr(summary, "passed", 0)
        report.traces_failed += getattr(summary, "failed", 0)
        report.traces_skipped += getattr(summary, "skipped", 0)
        report.traces_errored += getattr(summary, "errored", 0)
        _say(
            f"onboard: {t.path.name} -> {len(parsed.episodes)} episode(s) "
            f"({getattr(summary, 'passed', 0)} passed)"
        )

    if dry_run:
        _say("onboard: dry-run — skipping distillation (no pathways written)")
        return report

    _say("onboard: distilling pathways (tend)…")
    tend_report = await run_tend()
    report.pathways_created += getattr(tend_report, "created", 0)
    report.pathways_updated += getattr(tend_report, "updated", 0)
    for w in getattr(tend_report, "warnings", []) or []:
        report.warnings.append(w)
    _say(
        f"onboard: distilled {report.pathways_created} new pathway(s), "
        f"{report.pathways_updated} updated"
    )
    return report
