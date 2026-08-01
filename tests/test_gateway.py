"""Turn-level model routing for the token-saving gateway (MVP).

The gateway forwards whole agent *turns* (Anthropic Messages requests) and, before
forwarding, picks the cheapest capable model — it only ever *downgrades* an easy turn
off the frontier, never overriding the model the harness asked for on a hard turn.
These tests pin that decision. The HTTP transport (OAuth-Bearer passthrough, SSE
streaming) is proven separately by examples/gateway-spike and is not under test here.
"""

from __future__ import annotations

from opendaisugi.gateway import (
    RouteDecision,
    TurnSaving,
    measure_turn,
    route_turn,
)


def _req(model: str, user_text: str, *, system: str | None = None) -> dict:
    body: dict = {
        "model": model,
        "max_tokens": 1024,
        "messages": [{"role": "user", "content": user_text}],
    }
    if system is not None:
        body["system"] = system
    return body


def test_route_turn_downgrades_an_easy_turn() -> None:
    d = route_turn(_req("claude-opus-4-8", "say hi to the user"))
    assert isinstance(d, RouteDecision)
    assert d.tier == "tier1-cheap"
    assert d.model != "claude-opus-4-8"  # routed off the frontier
    assert d.downgraded is True
    assert d.requested_model == "claude-opus-4-8"


def test_route_turn_keeps_the_frontier_for_a_hard_turn() -> None:
    d = route_turn(
        _req(
            "claude-opus-4-8",
            "refactor the executor to fix the deadlock race condition under concurrency",
        )
    )
    assert d.tier == "tier2-frontier"
    assert d.model == "claude-opus-4-8"  # never override the requested model on a hard turn
    assert d.downgraded is False


def test_route_turn_reads_the_latest_user_message_from_content_blocks() -> None:
    # Anthropic content can be a list of typed blocks, not only a string; the routing
    # signal is the *latest* user turn, so a hard signal there must escalate.
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [
            {"role": "user", "content": "an earlier easy question"},
            {"role": "assistant", "content": "ok"},
            {
                "role": "user",
                "content": [{"type": "text", "text": "prove the schema migration is thread-safe"}],
            },
        ],
    }
    d = route_turn(body)
    assert d.tier == "tier2-frontier"


def test_route_turn_does_not_downgrade_when_the_request_is_already_cheap() -> None:
    # An easy turn already aimed at the cheap model is left alone — nothing to save.
    d = route_turn(_req("claude-haiku-4-5", "say hi"))
    assert d.tier == "tier1-cheap"
    assert d.model == "claude-haiku-4-5"
    assert d.downgraded is False


# ---- tool-loop continuation turns: no human text is NOT an easy turn ----


def _tool_result_turn(*content_blocks: dict) -> dict:
    """A continuation turn: the harness returns tool output as a user message whose
    content is tool_result blocks — no text block at all."""
    return {"type": "tool_result", "tool_use_id": "t1", "content": "…output…"}


def test_route_turn_does_not_downgrade_a_tool_result_only_turn() -> None:
    # A user turn carrying only a tool_result (no human text) has NO routing signal.
    # Downgrading on an empty signal would invert the never-override invariant on the
    # majority of an agentic session, so an absent signal must keep the requested model.
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [{"role": "user", "content": [_tool_result_turn()]}],
    }
    d = route_turn(body)
    assert d.downgraded is False
    assert d.model == "claude-opus-4-8"


def test_route_turn_walks_back_to_an_easy_human_ask_through_a_tool_loop() -> None:
    # Continuation turn after an easy ask → route by that ask (session affinity) → downgrade.
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [
            {"role": "user", "content": "add a docstring to this function"},
            {"role": "assistant", "content": [{"type": "text", "text": "reading…"}]},
            {"role": "user", "content": [_tool_result_turn()]},
        ],
    }
    d = route_turn(body)
    assert d.downgraded is True
    assert d.tier == "tier1-cheap"


def test_route_turn_walks_back_to_a_hard_human_ask_through_a_tool_loop() -> None:
    # Continuation turn after a hard ask → stay on the frontier for the whole loop.
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [
            {
                "role": "user",
                "content": "refactor to fix the deadlock race condition under concurrency",
            },
            {"role": "assistant", "content": [{"type": "text", "text": "working…"}]},
            {"role": "user", "content": [_tool_result_turn()]},
        ],
    }
    d = route_turn(body)
    assert d.downgraded is False
    assert d.model == "claude-opus-4-8"


def test_route_turn_reroutes_on_the_latest_human_ask_not_an_earlier_one() -> None:
    # A new hard human ask later in the session must override the earlier easy one.
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [
            {"role": "user", "content": "say hi"},
            {"role": "assistant", "content": [{"type": "text", "text": "hi"}]},
            {
                "role": "user",
                "content": "now prove the migration is thread-safe under concurrent writes",
            },
        ],
    }
    d = route_turn(body)
    assert d.tier == "tier2-frontier"
    assert d.downgraded is False


# ---- the meter: the multiplier, honestly (actual exact, counterfactual estimated) ----

_USAGE = {"input_tokens": 50_000, "output_tokens": 800}


def test_measure_turn_reports_a_routing_multiplier_when_downgraded() -> None:
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    assert decision.downgraded is True
    s = measure_turn(decision, _USAGE)
    assert isinstance(s, TurnSaving)
    assert s.multiplier > 1.0
    assert s.dollars_saved > 0.0


def test_measure_turn_prices_the_model_actually_run_from_real_usage() -> None:
    decision = route_turn(_req("claude-opus-4-8", "say hi"))  # -> cheap
    s = measure_turn(decision, _USAGE)
    assert s.actual.model == decision.model  # the cheap model we really called
    assert s.actual.input_tokens == _USAGE["input_tokens"]
    assert s.actual.output_tokens == _USAGE["output_tokens"]


def test_measure_turn_flags_the_counterfactual_as_estimated() -> None:
    # We never ran the frontier, so its cost is an estimate, never claimed as measured.
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    s = measure_turn(decision, _USAGE)
    assert s.estimated is True
    assert s.counterfactual.model == "claude-opus-4-8"


def test_measure_turn_is_flat_when_the_turn_was_not_downgraded() -> None:
    # A hard turn kept on the requested model saves nothing — multiplier 1.0, no estimate.
    decision = route_turn(
        _req("claude-opus-4-8", "fix the deadlock race condition in the concurrent refactor")
    )
    assert decision.downgraded is False
    s = measure_turn(decision, _USAGE)
    assert s.multiplier == 1.0
    assert s.dollars_saved == 0.0
    assert s.estimated is False


# ---- tokens are the actual constraint; dollars show how cheap those tokens are ----


def test_measure_turn_reports_frontier_tokens_preserved_when_downgraded() -> None:
    # The binding constraint on a subscription is the FRONTIER quota (that is the pool
    # that 429s). A downgraded turn spends zero frontier tokens — the whole turn's tokens
    # are preserved on the frontier pool.
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    s = measure_turn(decision, _USAGE)
    assert s.frontier_input_tokens_saved == _USAGE["input_tokens"]
    assert s.frontier_output_tokens_saved == _USAGE["output_tokens"]
    assert s.frontier_tokens_saved == _USAGE["input_tokens"] + _USAGE["output_tokens"]


def test_measure_turn_preserves_no_frontier_tokens_when_not_downgraded() -> None:
    # A hard turn ran on the frontier, so nothing was kept off that pool.
    decision = route_turn(
        _req("claude-opus-4-8", "fix the deadlock race condition in the concurrent refactor")
    )
    s = measure_turn(decision, _USAGE)
    assert s.frontier_tokens_saved == 0
    assert s.frontier_input_tokens_saved == 0
    assert s.frontier_output_tokens_saved == 0


def test_measure_turn_reports_both_currencies_on_a_downgrade() -> None:
    # Both must work: tokens are the constraint, dollars are the cheapness of those tokens.
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    s = measure_turn(decision, _USAGE)
    assert s.frontier_tokens_saved > 0  # the constraint eased
    assert s.dollars_saved > 0.0  # and it was cheaper in money terms too


# ---- prompt caching: Claude Code sends cached prefixes; the meter must read those fields ----


def test_measure_turn_prices_cache_reads_cheaper_than_fresh_but_not_free() -> None:
    # The same input volume costs less as a cache read (~0.1x input) — but NOT zero. Ignoring
    # the field entirely would price it at zero, so assert it lands strictly between.
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    fresh = measure_turn(decision, {"input_tokens": 100_000, "output_tokens": 100})
    output_only = measure_turn(decision, {"input_tokens": 0, "output_tokens": 100})
    cached = measure_turn(
        decision, {"input_tokens": 0, "cache_read_input_tokens": 100_000, "output_tokens": 100}
    )
    assert output_only.actual.dollars < cached.actual.dollars < fresh.actual.dollars


def test_measure_turn_counts_every_input_category_in_frontier_tokens_saved() -> None:
    # Fresh + cache-read + cache-creation input and output were all kept off the frontier pool.
    decision = route_turn(_req("claude-opus-4-8", "say hi"))
    s = measure_turn(
        decision,
        {
            "input_tokens": 1_000,
            "cache_read_input_tokens": 40_000,
            "cache_creation_input_tokens": 9_000,
            "output_tokens": 500,
        },
    )
    assert s.frontier_tokens_saved == 1_000 + 40_000 + 9_000 + 500
