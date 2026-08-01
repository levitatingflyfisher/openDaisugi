"""The gateway turn journal — an append-only record of routed turns and what they saved.

This is deliberately *not* the assurance :class:`~opendaisugi.journal.Journal`. That store
holds an envelope + plan + replayable :func:`~opendaisugi.verify.verify` result for every
entry, so ``replay()`` can re-verify a trace against current code. A raw agent *turn* — an
Anthropic Messages request and its response — has no plan to decompose and nothing to
re-verify, so forcing it through ``Journal.log`` would mean fabricating a trivially-true
envelope and plan and polluting the assurance store with non-replayable pseudo-traces. It
gets its own lightweight store instead.

Two jobs, both immediate:

* **Prove the blended saving** over a real day of work — sum the frontier tokens preserved
  and the dollars saved across every turn (see :func:`summarize`).
* **Count repeated asks** — the same task, seen again, is exactly the signal a later
  reuse tool (a harness-opt-in MCP tool, ADR-0004) will mine. v1 only records and counts;
  promoting a repeat into a reusable skill is deferred, not faked here.

Tokens are the headline (the frontier quota is what rate-limits a subscription); dollars
ride alongside to show how cheap those tokens are. The record layer is pure; persistence is
a thin append-only JSONL wrapper with no schema migrations to carry.
"""

from __future__ import annotations

import hashlib
import json
import re
from collections import Counter
from collections.abc import Iterable
from dataclasses import asdict, dataclass, fields
from datetime import datetime, timezone
from pathlib import Path

from opendaisugi.gateway import RouteDecision, TurnSaving

_WHITESPACE = re.compile(r"\s+")


def turn_signature(task: str) -> str:
    """A content-addressed id for an ask, stable across trivial formatting.

    Case and surrounding/internal whitespace are normalized away so "Say hi" and
    "  say  hi " collapse to one signature — that is what makes a *repeat* detectable.
    """
    normalized = _WHITESPACE.sub(" ", task.strip().lower())
    return hashlib.sha256(normalized.encode("utf-8")).hexdigest()[:16]


@dataclass(frozen=True)
class GatewayTurnRecord:
    """One routed turn: what was asked, where it went, and what that saved.

    ``actual_dollars`` / ``counterfactual_dollars`` are kept (rather than a single
    pre-subtracted ``dollars_saved``) so a pooled multiplier can be blended honestly across
    many turns — sum the numerators and denominators, don't average per-turn ratios.
    """

    created_at: str
    signature: str
    task: str
    tier: str
    requested_model: str
    model: str
    difficulty: float
    downgraded: bool
    estimated: bool
    input_tokens: int
    output_tokens: int
    frontier_tokens_saved: int
    actual_dollars: float
    counterfactual_dollars: float
    cache_read_tokens: int = 0
    cache_creation_tokens: int = 0

    @property
    def dollars_saved(self) -> float:
        return self.counterfactual_dollars - self.actual_dollars


def record_turn(
    decision: RouteDecision,
    saving: TurnSaving,
    *,
    task: str,
    ask: str | None = None,
    created_at: str | None = None,
) -> GatewayTurnRecord:
    """Build a turn record from a routing decision and its measured saving.

    ``task`` is the governing ask, kept for readability. ``ask`` is *this turn's own* new
    human text — the signature keys on it, so a tool-loop continuation (empty ``ask``) gets
    an empty signature and is not mistaken for a repeat of the ask that governs its loop.
    ``ask`` defaults to ``task`` (a plain single-turn ask is its own new text).
    ``created_at`` is an injection point for deterministic tests; callers normally omit it
    and let the journal stamp the current UTC time (mirroring ``Journal.log``).
    """
    if created_at is None:
        created_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if ask is None:
        ask = task
    signature = turn_signature(ask) if ask.strip() else ""
    return GatewayTurnRecord(
        created_at=created_at,
        signature=signature,
        task=task,
        tier=decision.tier,
        requested_model=decision.requested_model,
        model=decision.model,
        difficulty=decision.difficulty,
        downgraded=decision.downgraded,
        estimated=saving.estimated,
        input_tokens=saving.actual.input_tokens,
        output_tokens=saving.actual.output_tokens,
        frontier_tokens_saved=saving.frontier_tokens_saved,
        actual_dollars=saving.actual.dollars,
        counterfactual_dollars=saving.counterfactual.dollars,
        cache_read_tokens=saving.actual.cache_read_tokens,
        cache_creation_tokens=saving.actual.cache_creation_tokens,
    )


@dataclass(frozen=True)
class RepeatGroup:
    """A task that was asked more than once — a reuse candidate."""

    signature: str
    task: str
    count: int


@dataclass(frozen=True)
class GatewaySummary:
    """A day of routed turns, blended. Tokens first, dollars alongside."""

    turns: int
    downgraded_turns: int
    frontier_input_tokens_saved: int
    frontier_output_tokens_saved: int
    frontier_tokens_saved: int
    dollars_saved: float
    blended_multiplier: float
    repeats: list[RepeatGroup]
    # ADR-0015 cache visibility: the provider's own usage split across every
    # recorded turn, and the share of all input that came from cache reads —
    # the FinOps number the landscape survey says everyone lacks.
    cache_read_tokens: int = 0
    cache_creation_tokens: int = 0
    cache_hit_rate: float = 0.0
    # Turns served by the local rung (tier1-local): zero quota spent.
    local_turns: int = 0


def summarize(records: Iterable[GatewayTurnRecord]) -> GatewaySummary:
    """Blend a run of turn records into one tokens-first, dollars-alongside summary."""
    records = list(records)

    downgraded = [r for r in records if r.downgraded]
    # Input kept off the frontier is every bucket — fresh + cache read + cache creation.
    frontier_in = sum(
        r.input_tokens + r.cache_read_tokens + r.cache_creation_tokens for r in downgraded
    )
    frontier_out = sum(r.output_tokens for r in downgraded)

    total_actual = sum(r.actual_dollars for r in records)
    total_counterfactual = sum(r.counterfactual_dollars for r in records)
    dollars_saved = total_counterfactual - total_actual
    blended = total_counterfactual / total_actual if total_actual > 0.0 else 1.0

    # Continuations carry an empty signature — they route by the governing ask but are not
    # themselves re-asks, so they are excluded from repeat grouping.
    counts = Counter(r.signature for r in records if r.signature)
    task_by_sig = {r.signature: r.task for r in records if r.signature}
    repeats = [
        RepeatGroup(signature=sig, task=task_by_sig[sig], count=n)
        for sig, n in counts.most_common()
        if n > 1
    ]

    cache_read = sum(r.cache_read_tokens for r in records)
    cache_creation = sum(r.cache_creation_tokens for r in records)
    all_input = sum(r.input_tokens + r.cache_read_tokens + r.cache_creation_tokens for r in records)

    return GatewaySummary(
        turns=len(records),
        downgraded_turns=len(downgraded),
        frontier_input_tokens_saved=frontier_in,
        frontier_output_tokens_saved=frontier_out,
        cache_read_tokens=cache_read,
        cache_creation_tokens=cache_creation,
        cache_hit_rate=(cache_read / all_input) if all_input else 0.0,
        local_turns=sum(1 for r in records if r.tier == "tier1-local"),
        frontier_tokens_saved=frontier_in + frontier_out,
        dollars_saved=dollars_saved,
        blended_multiplier=blended,
        repeats=repeats,
    )


_FIELD_NAMES = {f.name for f in fields(GatewayTurnRecord)}


class GatewayJournal:
    """Append-only JSONL store of gateway turn records at ``path``.

    One turn per line. No index, no migrations — the whole point is a store cheap enough to
    write on every proxied turn. Reads are best-effort: blank lines, a truncated line from a
    crash mid-append, or a line that isn't a turn record are skipped (and counted in a warning)
    rather than raising. A saving journal that bricks itself on one partial line would take the
    meter down with it — exactly the fail-closed-on-a-saving-path outcome the design forbids.
    """

    def __init__(self, *, path: Path) -> None:
        self.path = Path(path)

    def append(self, record: GatewayTurnRecord) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.path.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(asdict(record)) + "\n")

    def load(self) -> list[GatewayTurnRecord]:
        if not self.path.exists():
            return []
        out: list[GatewayTurnRecord] = []
        skipped = 0
        for line in self.path.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                raw = json.loads(line)
                out.append(GatewayTurnRecord(**{k: raw[k] for k in _FIELD_NAMES if k in raw}))
            except (json.JSONDecodeError, TypeError):
                # Truncated/partial line (crash mid-append) or valid JSON that isn't a turn
                # record. Skip it; never let one bad line sink the whole saving history.
                skipped += 1
        if skipped:
            import logging

            logging.getLogger("opendaisugi.gateway_journal").warning(
                "gateway journal: skipped %d unreadable line(s) in %s", skipped, self.path
            )
        return out

    def summary(self) -> GatewaySummary:
        return summarize(self.load())
