"""The smart end of the router slot: a model that picks, and a model that grades.

The fake upstream is an httpx MockTransport. No real model, no network.
"""

from __future__ import annotations

import asyncio
import json

import httpx
import numpy as np

from opendaisugi import gateway_asgi
from opendaisugi import route_knn as rk
from opendaisugi.gateway_asgi import make_gateway_app
from opendaisugi.gateway_pipeline import Gateway

OPUS, HAIKU = "claude-opus-4-8", "claude-haiku-4-5"
ROUTER, GRADER = "router-m", "grader-m"
VEC = np.array([1.0, 0.0], dtype="<f4")


def _gateway(tmp_path, **kw) -> Gateway:
    store = rk.OutcomeStore(tmp_path / "routing.db")
    knn = rk.KnnRouter(store, lambda text: (VEC, "fake"), 0.25)
    return Gateway(knn=knn, **kw)


def _reply(text: str, stop: str = "end_turn") -> httpx.Response:
    body = {
        "content": [{"type": "text", "text": text}],
        "stop_reason": stop,
        "usage": {"input_tokens": 1, "output_tokens": 1},
    }
    return httpx.Response(200, json=body)


async def _turn(
    gw: Gateway,
    answers: dict[str, str | int],
    messages=None,
    stop: str = "end_turn",
    prompts: list[str] | None = None,
):
    """Send one turn; `answers` maps a model name to its reply text (or a status code)."""
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        sent = json.loads(request.content)
        model = sent["model"]
        seen.append(model)
        if prompts is not None:
            prompts.append(sent["messages"][0]["content"])
        a = answers.get(model, "ok")
        return httpx.Response(a) if isinstance(a, int) else _reply(a, stop)

    app = make_gateway_app(
        gw,
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    body = {
        "model": OPUS,
        "max_tokens": 9,
        "messages": messages or [{"role": "user", "content": "rename a helper"}],
    }
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://gw") as c:
        await c.post("/v1/messages", content=json.dumps(body).encode())
    await asyncio.gather(*list(gateway_asgi._PENDING))
    return seen


async def test_the_router_model_picks_an_allowed_name(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    seen = await _turn(gw, {ROUTER: HAIKU})
    assert seen == [ROUTER, HAIKU]  # asked first, then the turn on the picked model
    assert gw.knn.store.recent("fake")[0].model == HAIKU


async def test_a_name_that_is_not_allowed_falls_back_to_rules(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    seen = await _turn(gw, {ROUTER: "claude-opus-9"})
    assert seen[0] == ROUTER and len(seen) == 2
    assert "claude-opus-9" not in seen[1:]  # the turn was not served on the unlisted name
    # The reason line, from the fallback choice the proxy hands to prepare().
    body = {"model": OPUS, "messages": [{"role": "user", "content": "rename a helper"}]}
    why = "the router model named a model that is not allowed"
    prepared = gw.prepare(body, (None, why))
    assert prepared.decision.reason.startswith("model: the router model named a model")
    assert "rules router: " in prepared.decision.reason


async def test_a_router_model_error_falls_back_to_rules(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    seen = await _turn(gw, {ROUTER: 500})
    assert seen[0] == ROUTER and len(seen) == 2  # the turn still went through
    assert "claude-opus-9" not in seen


async def test_the_grader_writes_a_grade_after_the_reply(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="knn", grader_model=GRADER)
    seen = await _turn(gw, {GRADER: "bad"})
    assert seen[-1] == GRADER
    row = gw.knn.store.recent("fake")[0]
    assert (row.outcome, row.source) == (rk.BAD, "grader")


async def test_an_odd_grader_answer_writes_nothing(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="knn", grader_model=GRADER)
    await _turn(gw, {GRADER: "maybe"})
    row = gw.knn.store.recent("fake")[0]
    assert (row.outcome, row.source) == (rk.UNKNOWN, "auto")


def test_human_beats_grader_and_grader_beats_auto(tmp_path) -> None:
    store = rk.OutcomeStore(tmp_path / "r.db")
    tid = store.add(
        conversation="c",
        ts=1.0,
        task_sha="s",
        embedder="e",
        vec=VEC,
        model=HAIKU,
        cost=0.0,
        latency_ms=None,
    )
    assert store.set_outcome(tid, rk.GOOD, "grader")
    assert not store.set_outcome(tid, rk.BAD, "auto")
    assert store.set_outcome(tid, rk.BAD, "human")
    assert not store.set_outcome(tid, rk.GOOD, "grader")
    row = store.recent("e")[0]
    assert (row.outcome, row.source) == (rk.BAD, "human")


async def test_a_tool_call_step_is_not_graded(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="knn", grader_model=GRADER)
    seen = await _turn(gw, {GRADER: "bad"}, stop="tool_use")
    assert GRADER not in seen
    assert gw.knn.store.recent("fake")[0].outcome == rk.UNKNOWN


async def test_a_continuation_does_not_ask_the_router_model(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    steps = [
        {"role": "user", "content": "rename a helper"},
        {
            "role": "assistant",
            "content": [{"type": "tool_use", "id": "a", "name": "x", "input": {}}],
        },
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "a", "content": "ok"}]},
    ]
    seen = await _turn(gw, {ROUTER: HAIKU}, messages=steps)
    assert ROUTER not in seen and len(seen) == 1


def test_a_pick_is_applied_when_the_embedder_fails(tmp_path) -> None:
    def broken(text):
        raise RuntimeError("no embedder")

    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    gw.knn.embed = broken
    body = {"model": OPUS, "messages": [{"role": "user", "content": "rename a helper"}]}
    prepared = gw.prepare(body, (HAIKU, None))
    assert prepared.outbound_body["model"] == HAIKU and prepared.knn_vec is None
    gw.finish(prepared, {"input_tokens": 1, "output_tokens": 1})
    assert gw.knn.store.count() == 0  # a turn with no vector is not booked


def test_a_pick_outside_the_list_falls_back_to_rules(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="model", router_model=ROUTER)
    body = {"model": OPUS, "messages": [{"role": "user", "content": "rename a helper"}]}
    prepared = gw.prepare(body, ("claude-opus-9", None))
    assert prepared.outbound_body["model"] != "claude-opus-9"
    assert "rules router: " in prepared.decision.reason


def test_harness_reminders_are_not_the_users_words() -> None:
    from opendaisugi.gateway import _latest_user_text

    reminder = "<system-reminder>\ntoday is monday\n</system-reminder>"
    body = {
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": reminder},
                    {"type": "text", "text": "fix the bug"},
                ],
            }
        ]
    }
    assert _latest_user_text(body) == "fix the bug"


LOOP = [
    {"role": "user", "content": "rename a helper"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "a", "name": "x", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "a", "content": "ok"}]},
]


async def test_the_final_step_of_a_loop_is_graded_against_the_governing_ask(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="knn", grader_model=GRADER)
    await _turn(gw, {GRADER: "bad"}, stop="tool_use")  # the booked turn: not graded
    prompts: list[str] = []
    seen = await _turn(gw, {GRADER: "bad"}, messages=LOOP, prompts=prompts)
    assert seen[-1] == GRADER
    assert "Task:\nrename a helper\n" in prompts[-1]
    row = gw.knn.store.recent("fake")[0]
    assert (row.outcome, row.source) == (rk.BAD, "grader")


async def test_a_loop_step_with_no_booked_row_is_not_graded(tmp_path) -> None:
    gw = _gateway(tmp_path, router_mode="knn", grader_model=GRADER)
    seen = await _turn(gw, {GRADER: "bad"}, messages=LOOP)
    assert GRADER not in seen


def test_openai_finish_reasons_map_to_the_grader_words() -> None:
    from opendaisugi.gateway_openai import OpenAIUsageSniffer

    for finish, want in (("stop", "end_turn"), ("tool_calls", "tool_use")):
        sn = OpenAIUsageSniffer()
        sn.feed(f'data: {{"choices":[{{"delta":{{}},"finish_reason":"{finish}"}}]}}\n'.encode())
        assert sn.stop_reason == want
