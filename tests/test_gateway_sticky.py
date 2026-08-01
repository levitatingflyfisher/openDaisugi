"""Cache-aware sticky routing and the local rung (ADR-0015).

The provider prompt cache is keyed to the model, so every downgrade of a
conversation with a deep frontier prefix forfeits ~0.9x of that prefix's read
cost and pays a fresh cache write on the cheap model — and a re-write when the
conversation returns to the frontier. Past a threshold prefix depth the forfeit
exceeds a typical easy turn's savings, so the router goes sticky. Two rungs are
exempt because they pay no cloud cache economics at all: a configured *local*
model (zero quota — always the better home for an easy turn) and a conversation
that already lives on the cheap model (its warm cache is the cheap one).
"""

from __future__ import annotations

from opendaisugi.gateway import (
    STICKY_PREFIX_THRESHOLD_TOKENS,
    conversation_key,
    estimate_prefix_tokens,
    route_turn,
)
from opendaisugi.gateway_pipeline import Gateway

_EASY = "say hi please"
_HARD = (
    "architect a distributed consensus algorithm with security proofs and "
    "deadlock-free concurrency for the scheduler refactor; debug the race condition"
)


def _body(text: str, *, model: str = "claude-opus-4-8", pad_chars: int = 0) -> dict:
    messages = []
    if pad_chars:
        messages.append({"role": "assistant", "content": "x" * pad_chars})
    messages.append({"role": "user", "content": text})
    return {"model": model, "messages": messages}


# --- prefix estimation and conversation identity --------------------------------


def test_estimate_prefix_tokens_counts_system_and_messages():
    body = {
        "system": "s" * 400,
        "messages": [
            {"role": "user", "content": "u" * 400},
            {"role": "assistant", "content": [{"type": "text", "text": "a" * 400}]},
        ],
    }
    est = estimate_prefix_tokens(body)
    assert 250 <= est <= 350  # 1200 chars / 4 = 300


def test_conversation_key_stable_as_messages_append():
    b1 = {"messages": [{"role": "user", "content": "fix the flaky test in ci"}]}
    b2 = {
        "messages": [
            {"role": "user", "content": "fix the flaky test in ci"},
            {"role": "assistant", "content": "done"},
            {"role": "user", "content": "now run it"},
        ]
    }
    assert conversation_key(b1) == conversation_key(b2)
    b3 = {"messages": [{"role": "user", "content": "a totally different session"}]}
    assert conversation_key(b1) != conversation_key(b3)


# --- the local rung: an easy turn goes local, stickiness never blocks it --------


def test_easy_turn_routes_to_local_model_when_configured():
    d = route_turn(_body(_EASY), local_model="qwen2.5-7b-local")
    assert d.tier == "tier1-local"
    assert d.model == "qwen2.5-7b-local"
    assert d.downgraded


def test_local_wins_even_with_a_deep_frontier_prefix():
    body = _body(_EASY, pad_chars=STICKY_PREFIX_THRESHOLD_TOKENS * 8)
    d = route_turn(body, local_model="qwen2.5-7b-local")
    assert d.model == "qwen2.5-7b-local"


def test_hard_turn_never_routes_local():
    d = route_turn(_body(_HARD), local_model="qwen2.5-7b-local")
    assert d.tier == "tier2-frontier"
    assert d.model == "claude-opus-4-8"


# --- cache-aware stickiness on the cloud path -----------------------------------


def test_easy_turn_with_shallow_prefix_still_downgrades():
    d = route_turn(_body(_EASY))
    assert d.tier == "tier1-cheap"
    assert d.downgraded


def test_easy_turn_with_deep_frontier_prefix_stays_sticky():
    body = _body(_EASY, pad_chars=STICKY_PREFIX_THRESHOLD_TOKENS * 8)
    d = route_turn(body)
    assert d.tier == "tier2-frontier"
    assert d.model == "claude-opus-4-8"
    assert not d.downgraded
    assert "cache" in d.reason or "sticky" in d.reason


def test_sticky_to_cheap_conversation_keeps_the_cheap_cache_warm():
    # The conversation already lives on the cheap model: its warm cache is the
    # cheap one, so a deep prefix must NOT bounce it back to the frontier.
    body = _body(_EASY, pad_chars=STICKY_PREFIX_THRESHOLD_TOKENS * 8)
    d = route_turn(body, sticky_model="claude-haiku-4-5")
    assert d.tier == "tier1-cheap"
    assert d.model == "claude-haiku-4-5"


# --- pipeline: stickiness is remembered per conversation ------------------------


def test_pipeline_remembers_the_cheap_conversation():
    gw = Gateway()
    first = gw.prepare(_body(_EASY))
    assert first.decision.model == "claude-haiku-4-5"
    # Same conversation, now with a deep prefix: without memory the deep prefix
    # would trigger frontier stickiness; with it, the cheap cache stays warm.
    grown = {
        "model": "claude-opus-4-8",
        "messages": [
            {"role": "user", "content": _EASY},
            {"role": "assistant", "content": "x" * (STICKY_PREFIX_THRESHOLD_TOKENS * 8)},
            {"role": "user", "content": "thanks, again please"},
        ],
    }
    second = gw.prepare(grown)
    assert second.decision.model == "claude-haiku-4-5"


def test_pipeline_local_model_prices_as_zero():
    gw = Gateway(local_model="qwen2.5-7b-local")
    prepared = gw.prepare(_body(_EASY))
    assert prepared.decision.model == "qwen2.5-7b-local"
    saving, record = gw.finish(prepared, {"input_tokens": 1000, "output_tokens": 500})
    assert saving.actual.dollars == 0.0
    assert saving.frontier_tokens_saved == 1500
    assert record.model == "qwen2.5-7b-local"
