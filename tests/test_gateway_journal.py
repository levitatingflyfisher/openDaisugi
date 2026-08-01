"""The gateway turn journal — an append-only record of routed turns and what they saved.

This is deliberately NOT the assurance Journal (envelope + plan + replayable verification):
a raw agent turn has no plan to verify or replay, so it gets its own lightweight store. The
journal's two immediate jobs: aggregate the blended saving over a real day of work, and count
repeated asks (the substrate a Phase-2 reuse tool will mine). Tokens are the headline
constraint; dollars ride alongside.
"""

from __future__ import annotations

from opendaisugi.gateway import measure_turn, route_turn
from opendaisugi.gateway_journal import (
    GatewayJournal,
    GatewayTurnRecord,
    record_turn,
    summarize,
    turn_signature,
)


def _req(model: str, user_text: str) -> dict:
    return {
        "model": model,
        "max_tokens": 1024,
        "messages": [{"role": "user", "content": user_text}],
    }


_USAGE = {"input_tokens": 50_000, "output_tokens": 800}


def _downgraded_record(task: str = "say hi", *, created_at: str = "2026-08-01T00:00:00Z"):
    body = _req("claude-opus-4-8", task)
    decision = route_turn(body)
    saving = measure_turn(decision, _USAGE)
    return record_turn(decision, saving, task=task, created_at=created_at)


def _frontier_record(task: str, *, created_at: str = "2026-08-01T00:00:00Z"):
    body = _req("claude-opus-4-8", task)
    decision = route_turn(body)
    saving = measure_turn(decision, _USAGE)
    return record_turn(decision, saving, task=task, created_at=created_at)


# ---- the turn signature: content-addressed so repeats collapse ----


def test_turn_signature_is_stable_across_trivial_formatting() -> None:
    # The same ask, spelled with different surrounding whitespace/case, is one signature.
    assert turn_signature("Say hi to the user") == turn_signature("  say hi to the user  ")


def test_turn_signature_separates_different_asks() -> None:
    assert turn_signature("summarize the readme") != turn_signature("delete the readme")


# ---- one record captures both currencies ----


def test_record_turn_captures_tokens_and_dollars() -> None:
    rec = _downgraded_record()
    assert isinstance(rec, GatewayTurnRecord)
    assert rec.downgraded is True
    assert rec.frontier_tokens_saved == _USAGE["input_tokens"] + _USAGE["output_tokens"]
    assert rec.counterfactual_dollars > rec.actual_dollars  # cheaper in money too
    assert rec.signature == turn_signature("say hi")


# ---- the summary blends a day of turns, tokens-first, dollars alongside ----


def test_summarize_blends_both_currencies() -> None:
    records = [
        _downgraded_record("say hi"),
        _downgraded_record("list the files"),
        _frontier_record("fix the deadlock race condition under concurrency"),
    ]
    s = summarize(records)
    assert s.turns == 3
    assert s.downgraded_turns == 2
    # tokens: two downgraded turns, each preserving the whole turn on the frontier pool
    assert s.frontier_tokens_saved == 2 * (_USAGE["input_tokens"] + _USAGE["output_tokens"])
    # dollars: strictly positive, and the blended multiplier reflects the pooled spend
    assert s.dollars_saved > 0.0
    assert s.blended_multiplier > 1.0


def test_summarize_surfaces_repeated_asks() -> None:
    # The same ask twice + a unique one: only the repeated group is surfaced (count > 1).
    records = [
        _downgraded_record("run the tests"),
        _downgraded_record("run the tests"),
        _downgraded_record("write the changelog"),
    ]
    s = summarize(records)
    assert len(s.repeats) == 1
    top = s.repeats[0]
    assert top.count == 2
    assert top.signature == turn_signature("run the tests")


# ---- persistence: append-only JSONL that round-trips ----


def test_gateway_journal_round_trips_through_jsonl(tmp_path) -> None:
    path = tmp_path / "gw" / "turns.jsonl"  # parent dir does not exist yet
    j = GatewayJournal(path=path)
    j.append(_downgraded_record("say hi"))
    j.append(_downgraded_record("list the files"))

    reopened = GatewayJournal(path=path)
    loaded = reopened.load()
    assert len(loaded) == 2
    assert loaded[0].task == "say hi"
    assert reopened.summary().frontier_tokens_saved > 0


def test_gateway_journal_tolerates_blank_lines(tmp_path) -> None:
    path = tmp_path / "turns.jsonl"
    j = GatewayJournal(path=path)
    j.append(_downgraded_record("say hi"))
    path.write_text(path.read_text() + "\n\n")  # trailing blank lines
    assert len(GatewayJournal(path=path).load()) == 1


def test_gateway_journal_skips_a_partial_line_from_a_crash(tmp_path) -> None:
    # A process killed mid-append leaves a truncated JSON line. That must not brick every
    # future load() — the saving math survives on a best-effort basis.
    path = tmp_path / "turns.jsonl"
    j = GatewayJournal(path=path)
    j.append(_downgraded_record("say hi"))
    with path.open("a", encoding="utf-8") as fh:
        fh.write('{"created_at": "2026-08-01T00:00:00Z", "signature": "abc"')  # truncated
    loaded = GatewayJournal(path=path).load()
    assert len(loaded) == 1
    assert loaded[0].task == "say hi"


def test_gateway_journal_skips_a_line_missing_fields(tmp_path) -> None:
    # Valid JSON but not a turn record → skipped, not a hard error.
    path = tmp_path / "turns.jsonl"
    j = GatewayJournal(path=path)
    j.append(_downgraded_record("say hi"))
    with path.open("a", encoding="utf-8") as fh:
        fh.write('{"unrelated": "blob"}\n')
    assert len(GatewayJournal(path=path).load()) == 1


# --- ADR-0015: cache visibility in the summary ----------------------------------


def test_summarize_exposes_cache_buckets_and_hit_rate():
    from opendaisugi.gateway import RouteDecision, TurnSaving, price_turn
    from opendaisugi.gateway_journal import record_turn, summarize

    def _rec(model, requested, downgraded, usage, tier="tier1-cheap"):
        decision = RouteDecision(
            tier=tier,
            model=model,
            requested_model=requested,
            difficulty=0.1,
            downgraded=downgraded,
            reason="t",
        )
        actual = price_turn(
            model,
            usage.get("input_tokens", 0),
            usage.get("output_tokens", 0),
            cache_read_tokens=usage.get("cache_read_input_tokens", 0),
            cache_creation_tokens=usage.get("cache_creation_input_tokens", 0),
        )
        saving = TurnSaving(actual=actual, counterfactual=actual, estimated=False)
        return record_turn(decision, saving, task="t", created_at="2026-08-20T00:00:00Z")

    records = [
        _rec(
            "claude-opus-4-8",
            "claude-opus-4-8",
            False,
            {
                "input_tokens": 100,
                "cache_read_input_tokens": 700,
                "cache_creation_input_tokens": 200,
            },
            tier="tier2-frontier",
        ),
        _rec(
            "claude-haiku-4-5", "claude-opus-4-8", True, {"input_tokens": 500, "output_tokens": 50}
        ),
        _rec("local-q", "claude-opus-4-8", True, {"input_tokens": 10}, tier="tier1-local"),
    ]
    s = summarize(records)
    assert s.cache_read_tokens == 700
    assert s.cache_creation_tokens == 200
    # 700 reads / (700 + 200 + 610 fresh) input tokens
    assert abs(s.cache_hit_rate - 700 / 1510) < 1e-9
    assert s.local_turns == 1


def test_summarize_cache_hit_rate_zero_when_no_input():
    from opendaisugi.gateway_journal import summarize

    s = summarize([])
    assert s.cache_hit_rate == 0.0
    assert s.local_turns == 0
