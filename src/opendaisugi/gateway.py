"""Turn-level model routing for the token-saving gateway.

The gateway forwards whole agent *turns* — Anthropic Messages API requests — and, before
forwarding, picks the cheapest capable model. Its one rule is conservative on purpose: it
only ever **downgrades** an easy turn off the frontier. It never upgrades, and it never
overrides the model the harness asked for on a hard turn — so the worst case of a bad
routing call is "you paid what you would have paid anyway", never "your hard task ran on a
model too weak for it".

Difficulty is the same transparent heuristic the in-plan router uses
(:func:`opendaisugi.routing.estimate_difficulty`) — one source of truth, shared with the
step sizer. The routing signal is the *latest user turn*; earlier history is context, not
the ask.

This module is pure and unit-testable. The HTTP transport (OAuth-Bearer passthrough, SSE
streaming, fail-open-before-dispatch) lives in the ASGI layer and is proven separately by
``examples/gateway-spike``; nothing here opens a socket.
"""

from __future__ import annotations

from dataclasses import dataclass

from opendaisugi.routing import (
    _DEFAULT_CHEAP_MODEL,
    _HARD_THRESHOLD,
    estimate_difficulty,
)


@dataclass
class RouteDecision:
    """Which model a single turn should actually be sent to, and why."""

    tier: str  # "tier1-local" | "tier1-cheap" | "tier2-frontier"
    model: str  # the model to actually call
    requested_model: str  # what the harness asked for
    difficulty: float
    downgraded: bool  # True iff we chose a cheaper model than requested
    reason: str


# ---------------------------------------------------------------------------
# Cache-aware stickiness (ADR-0015).
#
# The provider prompt cache is keyed to the model, so downgrading one easy turn
# out of a frontier conversation forfeits the ~0.1x reads on its whole cached
# prefix and pays a fresh cache write on the cheap model — then a re-write at
# ~1.25x when the conversation returns to the frontier. With the default price
# table (frontier 15/75, cheap 1/5 per MTok) the forfeit on a prefix of P tokens
# is ≈ P × 1.15 × 15/1M ≈ P × $17/1M, while a typical easy turn's downgrade
# saves ≈ 500 output tokens × $70/1M ≈ $0.035 — break-even near P ≈ 2,000
# tokens. The default threshold is set at 2× that (prefix estimation by chars/4
# is rough), overridable per call. Two rungs are exempt because they pay no
# cloud cache economics: a configured LOCAL model (zero quota — strictly the
# better home for an easy turn), and a conversation that already lives on the
# cheap model (its warm cache IS the cheap one).
# ---------------------------------------------------------------------------

STICKY_PREFIX_THRESHOLD_TOKENS = 4096


def estimate_prefix_tokens(body: dict) -> int:
    """Rough size of the cacheable prefix, in tokens (chars/4).

    Counts the system prompt and every message's text — including tool_result
    content, which dominates real agentic prefixes. An estimate for a threshold
    comparison, not an exact count; ±25% changes nothing about the decision."""

    def _block_chars(block: object) -> int:
        if isinstance(block, str):
            return len(block)
        if isinstance(block, dict):
            chars = len(block.get("text", "") or "")
            inner = block.get("content")
            if isinstance(inner, str):
                chars += len(inner)
            elif isinstance(inner, list):
                chars += sum(_block_chars(b) for b in inner)
            return chars
        return 0

    chars = 0
    system = body.get("system", "")
    if isinstance(system, str):
        chars += len(system)
    elif isinstance(system, list):
        chars += sum(_block_chars(b) for b in system)
    for msg in body.get("messages", []):
        content = msg.get("content", "")
        if isinstance(content, str):
            chars += len(content)
        elif isinstance(content, list):
            chars += sum(_block_chars(b) for b in content)
    return chars // 4


def conversation_key(body: dict) -> str:
    """A stable identity for one conversation: the first user message's text.

    Messages only append within a session, so the first user text survives every
    turn; a compaction that rewrites it simply starts a new sticky history,
    which is the safe direction (stickiness is an optimization, never policy).
    """
    import hashlib

    for msg in body.get("messages", []):
        if msg.get("role") != "user":
            continue
        text = _message_text(msg.get("content", ""))
        if text.strip():
            return hashlib.sha256(text.strip().encode("utf-8")).hexdigest()[:16]
    return "no-user-text"


def _message_text(content: object) -> str:
    """Human text of one message's content — the string form, or its text blocks joined.
    tool_result / tool_use / image blocks contribute nothing."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [
            b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"
        ]
        return " ".join(p for p in parts if p)
    return ""


def _latest_user_text(body: dict) -> str:
    """The routing signal: the most recent user message that carries human text.

    A tool-loop continuation turn is a user message whose content is only ``tool_result``
    blocks — it has no human text. Routing such a turn by its (empty) text would read as
    difficulty 0 and downgrade the *majority* of an agentic session. Instead we walk back
    past continuations to the ask that governs the loop, so the whole session routes by
    session affinity (which also keeps it on one model, sparing the model-keyed prompt
    cache from thrash). Returns "" only when no user message carries any text.
    """
    for msg in reversed(body.get("messages", [])):
        if msg.get("role") != "user":
            continue
        text = _message_text(msg.get("content", ""))
        if text.strip():
            return text
    return ""


def _new_user_text(body: dict) -> str:
    """This turn's *own* new human ask — the text of the last user message only, "" if it
    is a pure tool-result continuation. Distinct from the routing signal: a continuation
    routes by the governing ask (walk-back) but is not itself a new ask, so it must not be
    counted as a repeat of that ask in the turn journal."""
    for msg in reversed(body.get("messages", [])):
        if msg.get("role") != "user":
            continue
        return _message_text(msg.get("content", ""))
    return ""


def route_turn(
    body: dict,
    *,
    cheap_model: str = _DEFAULT_CHEAP_MODEL,
    local_model: str | None = None,
    sticky_model: str | None = None,
    prefix_tokens: int | None = None,
    sticky_prefix_threshold: int = STICKY_PREFIX_THRESHOLD_TOKENS,
) -> RouteDecision:
    """Pick the cheapest capable model for one Anthropic Messages turn.

    Hard turn → the model the harness requested, untouched. No routing signal (a
    pure tool-result continuation with no governing human text anywhere) → keep
    the requested model: under the locked "fail-open loses savings, never
    safety" rule, an absent signal must never trigger a downgrade.

    An easy turn falls down the routing ladder (ADR-0015):

    1. ``local_model`` configured → the local rung. Zero quota, no cloud cache
       economics — stickiness never blocks this fall.
    2. ``sticky_model == cheap_model`` (the conversation already lives on the
       cheap model) → stay cheap; its warm cache is the cheap one.
    3. Estimated prefix ≥ ``sticky_prefix_threshold`` → stay on the requested
       model: forfeiting the frontier prefix's 0.1x cached reads (plus the
       re-write when the conversation returns) costs more than the easy turn's
       downgrade saves.
    4. Otherwise → the cheap cloud model, as always.
    """
    requested = body.get("model", "")
    text = _latest_user_text(body)

    if not text.strip():
        return RouteDecision(
            tier="tier2-frontier",
            model=requested,
            requested_model=requested,
            difficulty=0.0,
            downgraded=False,
            reason="no routing signal; keep the requested model",
        )

    difficulty = estimate_difficulty(text)

    if difficulty >= _HARD_THRESHOLD:
        return RouteDecision(
            tier="tier2-frontier",
            model=requested,
            requested_model=requested,
            difficulty=difficulty,
            downgraded=False,
            reason=f"hard turn ({difficulty:.2f}); keep the requested model",
        )

    if local_model:
        return RouteDecision(
            tier="tier1-local",
            model=local_model,
            requested_model=requested,
            difficulty=difficulty,
            downgraded=local_model != requested,
            reason=(
                f"easy turn ({difficulty:.2f}); route to the local model — "
                f"zero quota, cache stickiness never blocks the local rung"
            ),
        )

    if sticky_model == cheap_model:
        return RouteDecision(
            tier="tier1-cheap",
            model=cheap_model,
            requested_model=requested,
            difficulty=difficulty,
            downgraded=cheap_model != requested,
            reason=(
                f"easy turn ({difficulty:.2f}); sticky to the cheap model — "
                f"this conversation's warm cache is the cheap one"
            ),
        )

    if prefix_tokens is None:
        prefix_tokens = estimate_prefix_tokens(body)
    if prefix_tokens >= sticky_prefix_threshold:
        return RouteDecision(
            tier="tier2-frontier",
            model=requested,
            requested_model=requested,
            difficulty=difficulty,
            downgraded=False,
            reason=(
                f"easy turn ({difficulty:.2f}) but sticky: ~{prefix_tokens} prefix "
                f"tokens cached against the requested model — forfeiting 0.1x reads "
                f"(and re-writing on return) costs more than the downgrade saves"
            ),
        )

    return RouteDecision(
        tier="tier1-cheap",
        model=cheap_model,
        requested_model=requested,
        difficulty=difficulty,
        downgraded=cheap_model != requested,
        reason=f"easy turn ({difficulty:.2f}); route to the cheap model",
    )


# ---------------------------------------------------------------------------
# The meter — the routing multiplier, reported honestly.
#
# What we actually spent is exact: the tokens come straight from the model's own
# usage report. What the frontier *would* have cost is an estimate — we never ran
# it, so we assume it would have produced the same token counts and price it at the
# requested model's rate. Every TurnSaving that involved a downgrade carries
# ``estimated=True`` so the multiplier is never mistaken for a measured A/B result.
#
# Prices are USD per million tokens, (input, output). They are editable defaults,
# not an authoritative price list — override ``prices`` for your plan and the day.
# ---------------------------------------------------------------------------

_PRICES_PER_MTOK: dict[str, tuple[float, float]] = {
    "claude-opus-4-8": (15.0, 75.0),
    "claude-sonnet-5": (3.0, 15.0),
    "claude-haiku-4-5": (1.0, 5.0),
}
_FALLBACK_PRICE: tuple[float, float] = (3.0, 15.0)

# Prompt-cache multipliers on the base input rate (Anthropic): a cache *read* is ~0.1x,
# a cache *write* (creation) ~1.25x. Claude Code caches aggressively, so a turn's usage
# splits input across three buckets — pricing only ``input_tokens`` mis-measures every
# cached turn. NOTE the cache is keyed to the model: a prefix cached against the frontier
# is unreadable by the cheap model, so the frontier counterfactual (same token split) does
# not model the frontier's warm cache — it is an estimate, flagged as one.
_CACHE_READ_MULT = 0.1
_CACHE_WRITE_MULT = 1.25


@dataclass
class TurnCost:
    """The cost of one turn on one model, priced from token counts (cache buckets included)."""

    model: str
    input_tokens: int
    output_tokens: int
    dollars: float
    cache_read_tokens: int = 0
    cache_creation_tokens: int = 0

    @property
    def total_input_tokens(self) -> int:
        """Every input token the model processed — fresh + cache read + cache creation."""
        return self.input_tokens + self.cache_read_tokens + self.cache_creation_tokens


@dataclass
class TurnSaving:
    """Actual (exact) vs counterfactual (estimated when a downgrade happened) cost.

    Two currencies, and they answer different questions. **Tokens are the binding
    constraint** on a subscription/OAuth plan: you run out of *frontier quota* (the pool
    that returns 429), so the resource actually conserved by a downgrade is the frontier
    tokens the turn did not spend — reported by ``frontier_tokens_saved``. **Dollars** show
    how cheap those tokens are on an API-key plan — reported by ``dollars_saved`` /
    ``multiplier``. Both are first-class; neither is derived from the other.
    """

    actual: TurnCost
    counterfactual: TurnCost
    estimated: bool

    @property
    def _downgraded(self) -> bool:
        # measure_turn sets counterfactual == actual (same model) iff no downgrade happened.
        return self.actual.model != self.counterfactual.model

    # -- tokens: the constraint that 429s ------------------------------------------------

    @property
    def frontier_input_tokens_saved(self) -> int:
        """Input tokens kept off the frontier quota — every bucket (fresh + cache read +
        cache creation). 0 unless the turn was downgraded."""
        return self.actual.total_input_tokens if self._downgraded else 0

    @property
    def frontier_output_tokens_saved(self) -> int:
        """Output tokens kept off the frontier quota (0 unless the turn was downgraded)."""
        return self.actual.output_tokens if self._downgraded else 0

    @property
    def frontier_tokens_saved(self) -> int:
        """Total tokens the frontier pool did not have to spend on this turn.

        A downgraded turn runs entirely on the cheap model, so the frontier quota — the
        one that actually rate-limits you — is spared the whole turn. The counts are the
        cheap run's real usage, taken as the estimate of what the frontier would have used.
        """
        return self.frontier_input_tokens_saved + self.frontier_output_tokens_saved

    # -- dollars: how cheap those tokens are ---------------------------------------------

    @property
    def dollars_saved(self) -> float:
        return self.counterfactual.dollars - self.actual.dollars

    @property
    def multiplier(self) -> float:
        """counterfactual / actual — how many times cheaper this turn came out."""
        if self.actual.dollars <= 0.0:
            return 1.0
        return self.counterfactual.dollars / self.actual.dollars


def price_turn(
    model: str,
    input_tokens: int,
    output_tokens: int,
    *,
    cache_read_tokens: int = 0,
    cache_creation_tokens: int = 0,
    prices: dict[str, tuple[float, float]] = _PRICES_PER_MTOK,
) -> TurnCost:
    """Price a turn on one model across all three input buckets plus output. Exact for the
    model actually run; the caller marks any counterfactual pricing as estimated."""
    per_in, per_out = prices.get(model, _FALLBACK_PRICE)
    dollars = (
        input_tokens * per_in
        + cache_read_tokens * per_in * _CACHE_READ_MULT
        + cache_creation_tokens * per_in * _CACHE_WRITE_MULT
        + output_tokens * per_out
    ) / 1_000_000
    return TurnCost(
        model=model,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        dollars=dollars,
        cache_read_tokens=cache_read_tokens,
        cache_creation_tokens=cache_creation_tokens,
    )


def measure_turn(
    decision: RouteDecision,
    usage: dict,
    *,
    prices: dict[str, tuple[float, float]] = _PRICES_PER_MTOK,
) -> TurnSaving:
    """Turn a routing decision plus the model's own usage report into a saving.

    ``usage`` is the Anthropic response ``usage`` block: ``input_tokens`` /
    ``output_tokens`` plus the cache buckets ``cache_read_input_tokens`` /
    ``cache_creation_input_tokens`` (absent fields default to 0). When the turn was not
    downgraded, actual == counterfactual and nothing is estimated.
    """
    in_tok = int(usage.get("input_tokens", 0))
    out_tok = int(usage.get("output_tokens", 0))
    cache_read = int(usage.get("cache_read_input_tokens", 0))
    cache_creation = int(usage.get("cache_creation_input_tokens", 0))

    def _price(model: str) -> TurnCost:
        return price_turn(
            model,
            in_tok,
            out_tok,
            cache_read_tokens=cache_read,
            cache_creation_tokens=cache_creation,
            prices=prices,
        )

    actual = _price(decision.model)
    if not decision.downgraded:
        return TurnSaving(actual=actual, counterfactual=actual, estimated=False)

    return TurnSaving(
        actual=actual, counterfactual=_price(decision.requested_model), estimated=True
    )
