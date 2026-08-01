"""The calibration report — prove the blended multiplier on a recorded day.

Two very different numbers get blended in Phase-3 reporting, and this module's whole job is
to keep them honestly separate:

* **Realized routing (measured).** Straight from :func:`~opendaisugi.gateway_journal.
  summarize` — what the gateway's cheap/frontier routing decision *actually* saved on turns
  already run. This is fact, read off the journal.
* **Potential reuse (a ceiling).** Mined from repeat clusters via
  :func:`~opendaisugi.gateway_distill.rank_reuse_candidates`. For each repeat cluster, the
  first occurrence must still run — every occurrence *after* the first COULD have been
  served from a cache instead. This is a best case, not a measurement: it assumes every
  repeat-after-first reuses perfectly and freshly, which nothing in this codebase yet does.
* **Combined.** Realized routing with the reuse ceiling layered on top — also a ceiling,
  since one of its two ingredients is.

Nothing here mints a pathway or touches a cache; this is a read-only report over journal
history. Kept importable without the ``[search]`` extra at import time — the embedder is
only ever reached through ``rank_reuse_candidates``, mirroring ``gateway_distill``'s own
discipline. Tokens are the headline metric project-wide; dollars ride alongside.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

from opendaisugi.gateway_cluster import EmbedFn
from opendaisugi.gateway_distill import rank_reuse_candidates
from opendaisugi.gateway_journal import GatewayTurnRecord, summarize

_EPSILON = 1e-9


@dataclass(frozen=True)
class CalibrationReport:
    """A day of routed turns, calibrated: realized routing + potential reuse, kept separate.

    Tokens first, dollars alongside, in every group.
    """

    # Realized routing (measured) — from gateway_journal.summarize().
    turns: int
    downgraded_turns: int
    routing_frontier_tokens_saved: int
    routing_dollars_saved: float
    routing_multiplier: float
    # Cache visibility (ADR-0015) — measured, straight from provider usage reports.
    cache_read_tokens: int
    cache_creation_tokens: int
    cache_hit_rate: float
    local_turns: int

    # Potential reuse (a CEILING) — from repeat clusters, assuming perfect fresh reuse of
    # every occurrence after a cluster's first.
    repeat_clusters: int
    reuse_recoverable_tokens: int
    reuse_recoverable_dollars: float

    # Combined (a CEILING, since it includes the reuse ceiling) — realized routing with the
    # reuse ceiling layered on top. Note ``combined_frontier_tokens_saved`` sums two
    # different pools, not one: frontier tokens routing kept off the frontier (the turn
    # still ran, just on a cheaper model) plus tokens on whatever model actually served
    # them that perfect reuse would have avoided spending at all (the turn wouldn't have
    # run again). It is not a single-baseline "vs. always frontier, never reused" total —
    # a turn that is both downgraded and a repeat is counted in both addends.
    combined_frontier_tokens_saved: int
    combined_multiplier: float


def build_report(
    records: Iterable[GatewayTurnRecord],
    *,
    embed: EmbedFn | None = None,
) -> CalibrationReport:
    """Build a :class:`CalibrationReport` from a run of gateway turn records.

    Pure/deterministic given a fixed ``embed`` (only reached when there is at least one
    signed — i.e. non-continuation — record; see ``rank_reuse_candidates``). Empty
    ``records`` returns an all-zeros report with both multipliers at 1.0, never a
    divide-by-zero.
    """
    records = list(records)

    routing = summarize(records)

    candidates = rank_reuse_candidates(records, embed=embed)
    reuse_recoverable_tokens = 0
    reuse_recoverable_dollars = 0.0
    for candidate in candidates:
        count = candidate.cluster.count
        if count <= 1:
            # A singleton is not a repeat; nothing to recover. (rank_reuse_candidates
            # already excludes these, but the guard costs nothing and keeps this function
            # correct even if that upstream invariant ever loosens.)
            continue
        reuse_recoverable_tokens += int(candidate.total_tokens * (count - 1) / count)
        reuse_recoverable_dollars += candidate.total_dollars * (count - 1) / count

    total_actual = sum(r.actual_dollars for r in records)
    total_counterfactual = sum(r.counterfactual_dollars for r in records)

    combined_frontier_tokens_saved = routing.frontier_tokens_saved + reuse_recoverable_tokens
    if total_actual <= 0.0:
        combined_multiplier = 1.0
    else:
        combined_multiplier = total_counterfactual / max(
            total_actual - reuse_recoverable_dollars, _EPSILON
        )

    return CalibrationReport(
        turns=routing.turns,
        downgraded_turns=routing.downgraded_turns,
        routing_frontier_tokens_saved=routing.frontier_tokens_saved,
        routing_dollars_saved=routing.dollars_saved,
        routing_multiplier=routing.blended_multiplier,
        cache_read_tokens=routing.cache_read_tokens,
        cache_creation_tokens=routing.cache_creation_tokens,
        cache_hit_rate=routing.cache_hit_rate,
        local_turns=routing.local_turns,
        repeat_clusters=len(candidates),
        reuse_recoverable_tokens=reuse_recoverable_tokens,
        reuse_recoverable_dollars=reuse_recoverable_dollars,
        combined_frontier_tokens_saved=combined_frontier_tokens_saved,
        combined_multiplier=combined_multiplier,
    )
