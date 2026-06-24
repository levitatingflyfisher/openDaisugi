"""Token-tier accounting over the journal (v0.4.0).

The journal already records each trace's envelope, and since v0.3.0+v0.4.0
every envelope carries a ``generated_by`` tag that identifies its source:

- ``"compiled-pathway:<id>"`` — served from Tier 0 (PathwayStore)
- ``"tier1:<provider-name>"`` — produced by a Tier-1 adapter
- anything else (``"anthropic/..."``, ``"openai/..."``, ``"distilled"``, etc.) — Tier-2

This module classifies traces into those buckets and aggregates counts +
rough token estimates. It intentionally avoids writing new columns to the
journal — classification lives in ``generated_by`` and is already there for
every v0.3.x trace too.

Estimated tokens are deliberately rough (per-family ceilings). A precise
billing-grade meter is out of scope.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Literal

Tier = Literal["tier0", "tier1", "tier2"]

# Rough ceilings per envelope — envelope + system prompt is usually ~2-3k
# input tokens and ~500 output. These are conservative and intended for
# order-of-magnitude ROI comparison, not billing.
_ESTIMATED_TOKENS_PER_CALL = {
    "tier0": 0,       # deterministic pathway hit, no LLM round-trip
    "tier1": 2000,    # small model, short prompt
    "tier2": 4500,    # frontier model, full envelope prompt + examples
}


def classify_tier(generated_by: str) -> Tier:
    """Bucket a trace's ``generated_by`` string into a tier."""
    if generated_by.startswith("compiled-pathway:"):
        return "tier0"
    if generated_by.startswith("tier1:"):
        return "tier1"
    return "tier2"


def tier1_provider_name(generated_by: str) -> str | None:
    """Return the Tier-1 provider name, or None if not a Tier-1 trace."""
    if generated_by.startswith("tier1:"):
        return generated_by[len("tier1:"):]
    return None


@dataclass
class TierStats:
    """Aggregate counts + token estimates grouped by tier."""
    window_days: int
    total: int = 0
    by_tier: dict[str, int] = field(default_factory=dict)
    by_tier1_provider: dict[str, int] = field(default_factory=dict)
    estimated_tokens: dict[str, int] = field(default_factory=dict)
    estimated_tokens_total: int = 0
    # Pathway hit rate = tier0 / total (0.0 when total == 0).
    pathway_hit_rate: float = 0.0

    @classmethod
    def empty(cls, window_days: int) -> "TierStats":
        return cls(
            window_days=window_days,
            by_tier={"tier0": 0, "tier1": 0, "tier2": 0},
            estimated_tokens={"tier0": 0, "tier1": 0, "tier2": 0},
        )


def tier_stats(journal, *, window_days: int = 30) -> TierStats:
    """Scan the journal and bucket traces into tiers.

    ``journal`` is duck-typed: anything exposing ``list_successful_traces``
    (since v0.3.0) OR ``list_traces`` (pre-v0.3.0 fallback) works. We iterate
    the broadest available listing method, then classify via ``generated_by``
    loaded from each trace's YAML body.
    """
    cutoff = time.time() - window_days * 86400
    stats = TierStats.empty(window_days)

    # Prefer the richer list_successful_traces (v0.3.0+); fall back to list_recent.
    if hasattr(journal, "list_successful_traces"):
        rows = journal.list_successful_traces(since=cutoff)
    elif hasattr(journal, "list_recent"):
        rows = journal.list_recent(limit=10_000)
    else:
        return stats

    for row in rows:
        # row is a DistillableTrace or Trace — both have .trace_id / .id
        trace_id = getattr(row, "trace_id", None) or getattr(row, "id", None)
        if trace_id is None:
            continue
        try:
            record = journal.load_trace(trace_id)
        except Exception:
            continue
        ts = _row_ts(row)
        if ts is not None and ts < cutoff:
            continue
        tier = classify_tier(record.envelope.generated_by)
        stats.by_tier[tier] = stats.by_tier.get(tier, 0) + 1
        stats.total += 1
        if tier == "tier1":
            name = tier1_provider_name(record.envelope.generated_by) or "unknown"
            stats.by_tier1_provider[name] = stats.by_tier1_provider.get(name, 0) + 1

    for tier, count in stats.by_tier.items():
        est = count * _ESTIMATED_TOKENS_PER_CALL.get(tier, 0)
        stats.estimated_tokens[tier] = est
        stats.estimated_tokens_total += est
    if stats.total > 0:
        stats.pathway_hit_rate = stats.by_tier.get("tier0", 0) / stats.total
    return stats


def _row_ts(row) -> float | None:
    """Best-effort timestamp extraction from a DistillableTrace or Trace."""
    raw = getattr(row, "created_at", None)
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    if isinstance(raw, str):
        # ISO-8601 "2026-04-17T12:34:56Z" form — convert via datetime.
        from datetime import datetime
        try:
            if raw.endswith("Z"):
                raw = raw[:-1] + "+00:00"
            return datetime.fromisoformat(raw).timestamp()
        except ValueError:
            return None
    return None
