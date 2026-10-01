"""The gateway in knn mode: route a turn by past outcomes, then learn from the turn.

A fake embedder maps a few words to fixed unit vectors. No model and no network.
"""

from __future__ import annotations

import json

import numpy as np
import pytest

from opendaisugi import route_knn as rk
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.gateway_pipeline import ROUTER_MODES, Gateway

OPUS = "claude-opus-4-8"
HAIKU = "claude-haiku-4-5"
USAGE = {"input_tokens": 1000, "output_tokens": 100}

VECS = {
    "alpha": np.array([1.0, 0.0], dtype="<f4"),
    "alpha again": np.array([0.999, 0.0447], dtype="<f4"),
    "omega": np.array([0.0, 1.0], dtype="<f4"),
}


def _embed(text: str):
    for key in sorted(VECS, key=len, reverse=True):
        if text.startswith(key):
            return VECS[key], "fake"
    raise RuntimeError("no vector for " + text)


def _req(text: str, model: str = OPUS, extra: list | None = None) -> dict:
    msgs = [{"role": "user", "content": text}]
    return {"model": model, "max_tokens": 64, "messages": msgs + (extra or [])}


def _gateway(tmp_path, rows=(), **kw) -> Gateway:
    store = rk.OutcomeStore(tmp_path / "gateway" / "routing.db")
    for model, outcome in rows:
        store.add(
            conversation="old",
            ts=0.0,
            task_sha="s",
            embedder="fake",
            vec=VECS["alpha"],
            model=model,
            cost=0.0,
            latency_ms=None,
            outcome=outcome,
        )
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    router = rk.KnnRouter(store, _embed, 0.25)
    return Gateway(router_mode="knn", knn=router, journal=journal, **kw)


GOOD_CHEAP = [(HAIKU, rk.GOOD)] * 4 + [(OPUS, rk.GOOD)]


def test_knn_is_a_router_mode() -> None:
    assert "knn" in ROUTER_MODES


def test_knn_mode_needs_a_router_object() -> None:
    with pytest.raises(ValueError, match="knn"):
        Gateway(router_mode="knn")


def test_a_cheaper_model_that_did_well_on_similar_tasks_is_used(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("alpha design the schema for a distributed lock"))
    assert prepared.decision.model == HAIKU
    assert prepared.decision.downgraded is True
    assert prepared.outbound_body["model"] == HAIKU
    assert prepared.decision.reason.startswith("knn: claude-haiku-4-5; 5 neighbours, 5 known;")


def test_a_task_far_from_every_past_row_uses_the_rules_router(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("omega say hi"))
    assert "0 of 0 neighbours have a known outcome (need 3); rules router: " in (
        prepared.decision.reason
    )
    assert prepared.decision.model == HAIKU  # the rules router sends an easy turn down


def test_a_hard_task_far_from_every_past_row_keeps_the_requested_model(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("omega fix the deadlock race condition under concurrency"))
    assert prepared.decision.model == OPUS
    assert prepared.decision.reason.startswith("knn: 0 of 0 neighbours")


def test_knn_never_upgrades_a_cheap_request(tmp_path) -> None:
    gw = _gateway(tmp_path, [(OPUS, rk.GOOD)] * 5)
    prepared = gw.prepare(_req("alpha anything", model=HAIKU))
    assert prepared.decision.model == HAIKU
    assert prepared.decision.downgraded is False


def test_cache_stickiness_holds_a_big_conversation_on_the_requested_model(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    big = [{"role": "assistant", "content": "x" * 40_000}, {"role": "user", "content": "alpha"}]
    prepared = gw.prepare(_req("alpha first", extra=big))
    assert prepared.decision.model == OPUS
    assert prepared.decision.downgraded is False
    assert "sticky" in prepared.decision.reason
    assert prepared.decision.reason.startswith("knn: claude-haiku-4-5;")


def test_the_local_rung_is_not_held_back_by_stickiness(tmp_path) -> None:
    gw = _gateway(tmp_path, [("local-m", rk.GOOD)] * 5, local_model="local-m")
    big = [{"role": "assistant", "content": "x" * 40_000}, {"role": "user", "content": "alpha"}]
    prepared = gw.prepare(_req("alpha first", extra=big))
    assert prepared.decision.model == "local-m"
    assert prepared.decision.tier == "tier1-local"


def test_an_embedder_failure_falls_back_to_the_rules_router(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("zzz unknown words"))
    assert prepared.decision.reason.startswith("knn: could not embed the task")
    assert "rules router: " in prepared.decision.reason


def test_finish_books_a_row_and_journals_the_turn_id_and_reason(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("alpha design the schema"))
    _, record = gw.finish(prepared, USAGE)
    assert record.turn_id == "t6"
    assert record.reason.startswith("knn: ")
    line = json.loads((tmp_path / "gateway" / "turns.jsonl").read_text().splitlines()[0])
    assert line["turn_id"] == "t6" and line["reason"].startswith("knn: ")
    row = gw.knn.store.recent("fake")[0]
    assert (row.turn_id, row.model, row.outcome) == ("t6", HAIKU, rk.UNKNOWN)
    assert row.cost > 0.0


def test_a_failed_turn_is_booked_bad(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    prepared = gw.prepare(_req("alpha design the schema"))
    gw.finish(prepared, {}, failed=True)
    assert gw.knn.store.recent("fake")[0].outcome == rk.BAD


def test_the_next_human_turn_labels_the_previous_one(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    first = _req("alpha design the schema")
    reply = {"role": "assistant", "content": "an answer"}
    second = _req(
        "alpha design the schema", extra=[reply, {"role": "user", "content": "alpha again"}]
    )
    third = _req(
        "alpha design the schema",
        extra=[
            reply,
            {"role": "user", "content": "alpha again"},
            reply,
            {"role": "user", "content": "omega now something else"},
        ],
    )
    for body, now in ((first, 1000.0), (second, 1100.0), (third, 1200.0)):
        gw.finish(gw.prepare(body), USAGE, now=now)
    outcomes = {r.turn_id: r.outcome for r in gw.knn.store.recent("fake")}
    assert outcomes["t6"] == rk.BAD  # the user retried it
    assert outcomes["t7"] == rk.GOOD  # the user moved on
    assert outcomes["t8"] == rk.UNKNOWN


def test_a_tool_result_continuation_is_not_booked(tmp_path) -> None:
    gw = _gateway(tmp_path, GOOD_CHEAP)
    body = _req("alpha design the schema")
    body["messages"] += [
        {
            "role": "assistant",
            "content": [{"type": "tool_use", "id": "x", "name": "r", "input": {}}],
        },
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "x", "content": "ok"}]},
    ]
    prepared = gw.prepare(body)
    gw.finish(prepared, USAGE)
    assert gw.knn.store.count() == 5  # only the five seeded rows


def test_other_modes_journal_no_turn_id_or_reason(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    gw = Gateway(journal=journal)
    gw.finish(gw.prepare(_req("say hi")), USAGE)
    line = json.loads((tmp_path / "turns.jsonl").read_text().splitlines()[0])
    assert "turn_id" not in line and "reason" not in line


# ---------------------------------------------------------------------------
# The proxy: an upstream error or a refusal books the turn as bad.
# ---------------------------------------------------------------------------


async def _proxy_turn(tmp_path, status: int, content: bytes, headers: dict, stream: bool):
    import httpx

    from opendaisugi.gateway_asgi import make_gateway_app

    gw = _gateway(tmp_path, GOOD_CHEAP)

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(status, headers=headers, content=content)

    app = make_gateway_app(
        gw,
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    body = {**_req("alpha design the schema"), "stream": stream}
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://gw") as c:
        await c.post("/v1/messages", content=json.dumps(body).encode())
    return gw.knn.store.recent("fake")[0]


async def test_an_upstream_error_books_the_turn_bad(tmp_path) -> None:
    row = await _proxy_turn(tmp_path, 500, b"{}", {"content-type": "application/json"}, False)
    assert row.turn_id == "t6" and row.outcome == rk.BAD


async def test_a_buffered_refusal_books_the_turn_bad(tmp_path) -> None:
    content = json.dumps({"stop_reason": "refusal", "content": [], "usage": USAGE}).encode()
    row = await _proxy_turn(tmp_path, 200, content, {"content-type": "application/json"}, False)
    assert row.outcome == rk.BAD


async def test_a_streamed_refusal_books_the_turn_bad(tmp_path) -> None:
    sse = (
        b'data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}\n\n'
        b'data: {"type":"message_delta","delta":{"stop_reason":"refusal"},'
        b'"usage":{"output_tokens":5}}\n\n'
    )
    row = await _proxy_turn(tmp_path, 200, sse, {"content-type": "text/event-stream"}, True)
    assert row.outcome == rk.BAD


async def test_a_good_answer_books_the_turn_unknown(tmp_path) -> None:
    content = json.dumps({"stop_reason": "end_turn", "content": [], "usage": USAGE}).encode()
    row = await _proxy_turn(tmp_path, 200, content, {"content-type": "application/json"}, False)
    assert row.outcome == rk.UNKNOWN
