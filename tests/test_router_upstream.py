"""The router's external router mode and its off mode.

In external mode an outside chooser, Switchyard, owns the model choice. The
router sends the route id, never rewrites a turn onto its own cheap model,
and meters the turn by the target the response names. In off mode the
router forwards every turn unchanged and only meters it.
"""

from __future__ import annotations

import json

import httpx
import pytest

from opendaisugi.router_asgi import make_router_app
from opendaisugi.router_journal import RouterJournal
from opendaisugi.router_pipeline import ExternalRouterConfig, Router


def _ext(**overrides) -> ExternalRouterConfig:
    base = dict(
        route_id="daisugi",
        capable_target="claude-sonnet-5",
        efficient_target="qwen3-coder:30b",
    )
    base.update(overrides)
    return ExternalRouterConfig(**base)


def test_rules_mode_is_unchanged_by_default():
    gw = Router()
    assert gw.routing_mode == "rules"
    assert gw.external is None
    prepared = gw.prepare(
        {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    )
    assert prepared.outbound_body["model"] == "claude-haiku-4-5"


def test_external_mode_requires_a_config():
    with pytest.raises(ValueError, match="ExternalRouterConfig"):
        Router(routing_mode="external")


def test_an_unknown_routing_mode_is_refused():
    with pytest.raises(ValueError, match="routing_mode"):
        Router(routing_mode="switchyard")


def test_external_prepare_sends_the_route_id_not_the_harness_model():
    gw = Router(routing_mode="external", external=_ext())
    body = {"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}
    prepared = gw.prepare(body)
    assert prepared.outbound_body["model"] == "daisugi"
    assert prepared.decision.tier == "tier-switchyard"
    assert prepared.decision.model == "daisugi"
    assert prepared.decision.requested_model == "claude-sonnet-5"
    assert prepared.decision.downgraded is False
    assert body["model"] == "claude-sonnet-5"


def test_external_prepare_never_downgrades_locally_even_with_a_local_model():
    gw = Router(routing_mode="external", external=_ext(), local_model="qwen2.5-coder-7b")
    prepared = gw.prepare(
        {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    )
    assert prepared.outbound_body["model"] == "daisugi"
    assert prepared.decision.downgraded is False


def test_external_mode_registers_target_prices():
    gw = Router(
        routing_mode="external",
        external=_ext(prices={"qwen3-coder:30b": (0.0, 0.0)}),
    )
    assert gw.prices["qwen3-coder:30b"] == (0.0, 0.0)


def test_off_mode_forwards_the_body_unchanged():
    gw = Router(routing_mode="off")
    body = {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    prepared = gw.prepare(body)
    assert prepared.outbound_body == body
    assert prepared.outbound_body is not body
    assert prepared.decision.tier == "tier-off"
    assert prepared.decision.downgraded is False
    assert prepared.decision.model == "claude-opus-4-8"


def test_off_mode_needs_no_external_config():
    Router(routing_mode="off")


# --- metering by the served target -------------------------------------------

HEADER = "x-model-router-selected-model"


def _mock_upstream(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def _call(app, body, path="/v1/messages"):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        return await client.post(
            path,
            content=json.dumps(body).encode(),
            headers={"authorization": "Bearer sk-oauth-XYZ", "content-type": "application/json"},
        )


def _stream_body(model: str, text: str) -> dict:
    return {
        "model": model,
        "max_tokens": 1024,
        "stream": True,
        "messages": [{"role": "user", "content": text}],
    }


def _sse_served_by(model: str) -> bytes:
    return (
        b"event: message_start\n"
        b'data: {"type":"message_start","message":{"model":"' + model.encode() + b'",'
        b'"usage":{"input_tokens":1000,"cache_read_input_tokens":0,'
        b'"cache_creation_input_tokens":0,"output_tokens":1}}}\n\n'
        b"event: content_block_delta\n"
        b'data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}\n\n'
        b"event: message_delta\n"
        b'data: {"type":"message_delta","usage":{"output_tokens":250}}\n\n'
        b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
    )


def _served(model: str, *, header: str | None = "same", content: bytes | None = None):
    """A streamed Switchyard answer. The header names the same target by default."""
    headers = {"content-type": "text/event-stream"}
    if header == "same":
        headers[HEADER] = model
    elif header is not None:
        headers[HEADER] = header
    return httpx.Response(200, headers=headers, content=content or _sse_served_by(model))


def _router(journal=None) -> Router:
    return Router(
        routing_mode="external",
        external=_ext(prices={"qwen3-coder:30b": (0.0, 0.0)}),
        journal=journal,
    )


def _app(router, handler):
    return make_router_app(router, upstream_base_url="http://up", client=_mock_upstream(handler))


async def test_external_mode_forwards_the_rest_of_the_body_unchanged():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        seen["auth"] = request.headers.get("authorization")
        return _served("claude-sonnet-5")

    body = _stream_body("claude-sonnet-5", "say hi")
    body["metadata"] = {"user_id": "abc"}
    await _call(_app(_router(), handler), body)
    expected = dict(body)
    expected["model"] = "daisugi"
    assert seen["body"] == expected
    assert seen["auth"] == "Bearer sk-oauth-XYZ"


async def test_an_efficient_turn_is_booked_as_a_saving(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        assert json.loads(request.content)["model"] == "daisugi"
        return _served("qwen3-coder:30b")

    resp = await _call(_app(_router(journal), handler), _stream_body("claude-sonnet-5", "hi"))
    assert resp.status_code == 200
    rec = journal.load()[0]
    assert rec.tier == "tier-switchyard"
    assert rec.model == "qwen3-coder:30b"
    assert rec.requested_model == "claude-sonnet-5"
    assert rec.downgraded is True
    assert rec.frontier_tokens_saved == 1250
    assert rec.actual_dollars == 0.0
    assert rec.counterfactual_dollars > 0.0


async def test_a_capable_turn_books_no_saving(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    await _call(
        _app(_router(journal), lambda r: _served("claude-sonnet-5")),
        _stream_body("claude-sonnet-5", "hi"),
    )
    rec = journal.load()[0]
    assert rec.model == "claude-sonnet-5"
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_an_efficient_turn_the_harness_asked_for_is_no_saving(tmp_path):
    # The counterfactual is what the harness asked for. It asked for the
    # efficient model itself, so serving that model saved nothing.
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    await _call(
        _app(_router(journal), lambda r: _served("qwen3-coder:30b")),
        _stream_body("qwen3-coder:30b", "hi"),
    )
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


@pytest.mark.parametrize(
    ("body_model", "header"),
    [
        ("some-other-provider/model-x", "same"),  # a target the router does not know
        ("daisugi", "same"),  # the route id echoed back
        ("qwen3-coder:30b", None),  # no header
        ("qwen3-coder:30b", "claude-sonnet-5"),  # header and body disagree
    ],
)
async def test_an_unknown_target_books_no_saving_and_no_tier_row(tmp_path, body_model, header):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    await _call(
        _app(_router(journal), lambda r: _served(body_model, header=header)),
        _stream_body("claude-sonnet-5", "hi"),
    )
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0
    if body_model not in ("some-other-provider/model-x",):
        assert rec.model == "unknown"


async def test_a_buffered_turn_reads_the_served_target_too(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={HEADER: "qwen3-coder:30b"},
            json={
                "model": "qwen3-coder:30b",
                "content": [{"type": "text", "text": "ok"}],
                "usage": {"input_tokens": 10, "output_tokens": 5},
            },
        )

    body = {"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}
    await _call(_app(_router(journal), handler), body)
    rec = journal.load()[0]
    assert rec.model == "qwen3-coder:30b"
    assert rec.downgraded is True


async def test_an_unknown_route_returns_switchyards_error_with_no_retry():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        calls.append(body["model"])
        return httpx.Response(404, json={"error": {"message": f"unknown route '{body['model']}'"}})

    resp = await _call(_app(_router(), handler), _stream_body("claude-sonnet-5", "hi"))
    assert resp.status_code == 404
    assert "daisugi" in resp.text
    assert calls == ["daisugi"]


async def test_tool_use_blocks_and_stream_deltas_pass_through_unchanged(tmp_path):
    sse = (
        b"event: message_start\n"
        b'data: {"type":"message_start","message":{"model":"qwen3-coder:30b","usage":{'
        b'"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,'
        b'"output_tokens":1}}}\n\n'
        b"event: content_block_start\n"
        b'data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use",'
        b'"id":"toolu_1","name":"Bash","input":{}}}\n\n'
        b"event: content_block_delta\n"
        b'data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta",'
        b'"partial_json":"{\\"command\\": \\"ls\\"}"}}\n\n'
        b"event: message_delta\n"
        b'data: {"type":"message_delta","usage":{"output_tokens":5}}\n\n'
        b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
    )
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    resp = await _call(
        _app(_router(journal), lambda r: _served("qwen3-coder:30b", content=sse)),
        _stream_body("claude-sonnet-5", "run ls"),
    )
    assert resp.content == sse
    assert resp.headers[HEADER] == "qwen3-coder:30b"
    rec = journal.load()[0]
    assert rec.model == "qwen3-coder:30b"
    assert rec.downgraded is True


async def test_count_tokens_goes_to_the_route_and_is_not_a_turn(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["model"] = json.loads(request.content)["model"]
        return httpx.Response(200, json={"input_tokens": 42})

    body = {"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}
    resp = await _call(_app(_router(journal), handler), body, path="/v1/messages/count_tokens")
    assert resp.json() == {"input_tokens": 42}
    assert seen == {"path": "/v1/messages/count_tokens", "model": "daisugi"}
    assert journal.load() == []


async def test_off_mode_forwards_and_meters_with_no_saving(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["model"] = json.loads(request.content)["model"]
        return _served("claude-opus-4-8", header=None)

    app = make_router_app(
        Router(routing_mode="off", journal=journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert seen["model"] == "claude-opus-4-8"
    rec = journal.load()[0]
    assert rec.tier == "tier-off"
    assert rec.downgraded is False


# --- the launch choice survives a config reload -----------------------------


def test_serve_keeps_external_mode_across_a_config_reload(tmp_path, monkeypatch):
    import signal

    import yaml

    import opendaisugi.router_asgi as asgi

    registered = {}
    monkeypatch.setattr(signal, "signal", lambda sig, handler: registered.__setitem__(sig, handler))
    served = {}
    monkeypatch.setattr("uvicorn.run", lambda app, **k: served.setdefault("app", app))
    built = []
    real_build = asgi.build_default_router

    def _spy(**kwargs):
        gw = real_build(**kwargs)
        built.append(gw)
        return gw

    monkeypatch.setattr(asgi, "build_default_router", _spy)
    cfg = tmp_path / "config.yaml"
    cfg.write_text(yaml.safe_dump({"router_local_model": None}))
    ext = _ext()
    asgi.serve_router(
        data_dir=tmp_path,
        port=0,
        upstream_base_url="http://127.0.0.1:4000",
        routing_mode="external",
        external=ext,
        openai_cheap_model=None,
    )
    assert built[-1].routing_mode == "external"
    cfg.write_text(yaml.safe_dump({"router_local_model": "qwen2.5-coder-7b"}))
    registered[signal.SIGHUP](signal.SIGHUP, None)
    assert built[-1].local_model == "qwen2.5-coder-7b"
    assert built[-1].routing_mode == "external", "a reload dropped the launch router mode"
    assert built[-1].external == ext
    prepared = built[-1].prepare(
        {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    )
    assert prepared.outbound_body["model"] == "daisugi"


# --- a saving needs a cheaper, priced served model ----------------------------


def _priced_router(journal, capable, efficient, prices=None) -> Router:
    return Router(
        routing_mode="external",
        external=_ext(capable_target=capable, efficient_target=efficient, prices=prices or {}),
        journal=journal,
    )


async def test_an_upgrade_onto_the_efficient_tier_is_never_a_saving(tmp_path):
    # The reviewer's probe: capable opus, efficient sonnet, and a background
    # call sent as haiku that Switchyard serves on the efficient tier.
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    router = _priced_router(journal, "claude-opus-4-8", "claude-sonnet-5")
    await _call(
        _app(router, lambda r: _served("claude-sonnet-5")),
        _stream_body("claude-haiku-4-5", "hi"),
    )
    rec = journal.load()[0]
    assert rec.model == "claude-sonnet-5"
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0
    assert rec.counterfactual_dollars == rec.actual_dollars


async def test_an_unpriced_requested_model_books_no_saving(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    router = _priced_router(journal, "claude-sonnet-5", "claude-haiku-4-5")
    await _call(
        _app(router, lambda r: _served("claude-haiku-4-5")),
        _stream_body("claude-haiku-4-5-20251001", "hi"),
    )
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_an_unpriced_served_model_books_no_saving(tmp_path):
    # A hand-written config with a local efficient target and no price.
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    router = _priced_router(journal, "claude-sonnet-5", "my-local-model")
    await _call(
        _app(router, lambda r: _served("my-local-model")),
        _stream_body("claude-haiku-4-5", "hi"),
    )
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_a_priced_cheaper_efficient_tier_is_a_saving(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    router = _priced_router(journal, "claude-opus-4-8", "claude-haiku-4-5")
    await _call(
        _app(router, lambda r: _served("claude-haiku-4-5")),
        _stream_body("claude-sonnet-5", "hi"),
    )
    rec = journal.load()[0]
    assert rec.downgraded is True
    assert rec.counterfactual_dollars > rec.actual_dollars


def test_is_cheaper_needs_both_prices_and_both_rates_lower():
    from opendaisugi.router_asgi import _strictly_cheaper

    prices = {"a": (1.0, 5.0), "b": (3.0, 15.0), "c": (1.0, 20.0), "z": (0.0, 0.0)}
    assert _strictly_cheaper("a", "b", prices) is True
    assert _strictly_cheaper("z", "a", prices) is True
    assert _strictly_cheaper("b", "a", prices) is False
    assert _strictly_cheaper("a", "a", prices) is False
    assert _strictly_cheaper("c", "b", prices) is False  # output rate is higher
    assert _strictly_cheaper("a", "missing", prices) is False
    assert _strictly_cheaper("missing", "b", prices) is False


# --- rules mode books a downgrade only when the price table shows one ------------


def test_rules_mode_books_no_downgrade_for_a_dated_id_routed_to_the_same_model(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    gw = Router(journal=journal)
    prepared = gw.prepare(
        {"model": "claude-haiku-4-5-20251001", "messages": [{"role": "user", "content": "say hi"}]}
    )
    # Routing is unchanged: the easy turn still goes to the cheap model.
    assert prepared.outbound_body["model"] == "claude-haiku-4-5"
    gw.finish(prepared, {"input_tokens": 1000, "output_tokens": 200})
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0
    assert rec.counterfactual_dollars == rec.actual_dollars


def test_rules_mode_still_books_a_real_downgrade(tmp_path):
    journal = RouterJournal(path=tmp_path / "turns.jsonl")
    gw = Router(journal=journal)
    prepared = gw.prepare(
        {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    )
    gw.finish(prepared, {"input_tokens": 1000, "output_tokens": 200})
    rec = journal.load()[0]
    assert rec.downgraded is True
    assert rec.frontier_tokens_saved == 1200
