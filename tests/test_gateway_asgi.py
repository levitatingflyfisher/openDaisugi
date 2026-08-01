"""The gateway's async proxy — a bare ASGI app that forwards Anthropic Messages turns.

Everything here runs over httpx transports, no sockets: the app is driven by
``httpx.ASGITransport`` and the upstream is an ``httpx.MockTransport`` that captures what the
proxy sends and returns canned responses. What must hold: the OAuth Bearer is forwarded
intact, an easy turn goes upstream on the cheap model (a hard turn untouched), SSE bytes
stream through byte-for-byte, usage is sniffed and journalled, a 4xx on the swapped body
fails open by retrying the original model, and an unparseable body is proxied verbatim.
"""

from __future__ import annotations

import json

import httpx

from opendaisugi.gateway_answers import AnswerStore
from opendaisugi.gateway_asgi import _UsageSniffer, make_gateway_app
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.gateway_pipeline import Gateway

_SSE = (
    b"event: message_start\n"
    b'data: {"type":"message_start","message":{"usage":{"input_tokens":1000,'
    b'"cache_read_input_tokens":40000,"cache_creation_input_tokens":0,"output_tokens":1}}}\n\n'
    b"event: content_block_delta\n"
    b'data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}\n\n'
    b"event: message_delta\n"
    b'data: {"type":"message_delta","usage":{"output_tokens":250}}\n\n'
    b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
)


def _stream_body(model: str, text: str) -> dict:
    return {
        "model": model,
        "max_tokens": 1024,
        "stream": True,
        "messages": [{"role": "user", "content": text}],
    }


def _mock_upstream(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def _call(app, body, *, headers=None, raw=None):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        content = raw if raw is not None else json.dumps(body).encode()
        return await client.post(
            "/v1/messages",
            content=content,
            headers=headers
            or {"authorization": "Bearer sk-oauth-XYZ", "content-type": "application/json"},
        )


async def test_forwards_bearer_and_swaps_model_on_an_easy_turn() -> None:
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("authorization")
        seen["model"] = json.loads(request.content)["model"]
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert resp.status_code == 200
    assert seen["auth"] == "Bearer sk-oauth-XYZ"  # OAuth forwarded intact
    assert seen["model"] == "claude-haiku-4-5"  # easy turn downgraded on the wire


async def test_keeps_the_model_on_a_hard_turn() -> None:
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["model"] = json.loads(request.content)["model"]
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler))
    await _call(
        app, _stream_body("claude-opus-4-8", "fix the deadlock race condition under concurrency")
    )
    assert seen["model"] == "claude-opus-4-8"


async def test_streams_sse_bytes_through_unchanged() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert resp.content == _SSE


async def test_sniffs_usage_and_journals_the_turn(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(
        Gateway(journal=journal), upstream_base_url="http://up", client=_mock_upstream(handler)
    )
    await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    records = journal.load()
    assert len(records) == 1
    rec = records[0]
    assert rec.downgraded is True
    # input(1000) + cache_read(40000) + output(250) all kept off the frontier pool
    assert rec.frontier_tokens_saved == 1000 + 40000 + 250


async def test_fail_open_retries_the_original_model_on_a_4xx(tmp_path) -> None:
    calls = []
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        model = json.loads(request.content)["model"]
        calls.append(model)
        if model == "claude-haiku-4-5":  # cheap model rejects the swapped body
            return httpx.Response(400, json={"error": {"message": "max_tokens too high"}})
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(
        Gateway(journal=journal), upstream_base_url="http://up", client=_mock_upstream(handler)
    )
    resp = await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert resp.status_code == 200  # the client never sees the 400
    assert calls == ["claude-haiku-4-5", "claude-opus-4-8"]  # retried on the original model
    # The turn actually ran on the original model, so it must NOT be booked as a saving.
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_fail_open_retries_when_the_4xx_body_is_itself_streamed() -> None:
    # The retry must fire even when the cheap model's 400 comes back as a streaming body
    # (not pre-materialized) — the branch reads status at context entry, before the body.
    calls = []

    async def _streamed_400_body():
        yield b'{"error":'
        yield b'"rejected"}'

    def handler(request: httpx.Request) -> httpx.Response:
        model = json.loads(request.content)["model"]
        calls.append(model)
        if model == "claude-haiku-4-5":
            return httpx.Response(400, content=_streamed_400_body())  # genuinely streamed 400
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert resp.status_code == 200
    assert resp.content == _SSE
    assert calls == ["claude-haiku-4-5", "claude-opus-4-8"]


async def test_fail_open_proxies_an_unparseable_body_verbatim() -> None:
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["content"] = request.content
        return httpx.Response(200, json={"ok": True})

    app = make_gateway_app(Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, None, raw=b"this is not json{{{")
    assert resp.status_code == 200
    assert seen["content"] == b"this is not json{{{"  # forwarded exactly, no crash


def _continuation_body(model: str) -> dict:
    """A tool-loop continuation: the only user content is a tool_result, not a fresh ask."""
    return {
        "model": model,
        "max_tokens": 1024,
        "stream": True,
        "messages": [
            {"role": "user", "content": "add a docstring to this function"},
            {"role": "assistant", "content": [{"type": "text", "text": "reading…"}]},
            {
                "role": "user",
                "content": [{"type": "tool_result", "tool_use_id": "t", "content": "ok"}],
            },
        ],
    }


class _BoomStore:
    """A fake answer store whose ``append`` always raises — proves fail-open capture."""

    def append(self, entry) -> None:
        raise RuntimeError("boom")


def test_usage_sniffer_accumulates_text() -> None:
    sniffer = _UsageSniffer()
    sniffer.feed(_SSE)
    assert sniffer.text == "hi"
    assert sniffer.usage.get("output_tokens") == 250  # usage parsing untouched


async def test_streaming_capture_records_the_answer_and_keeps_bytes_intact(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    gw = Gateway(answer_store=store, capture_answers=True)
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, _stream_body("claude-opus-4-8", "what does this function do?"))
    assert resp.content == _SSE  # byte-for-byte passthrough intact
    entries = store.load()
    assert len(entries) == 1
    assert entries[0].answer == "hi"
    assert entries[0].task == "what does this function do?"


async def test_streaming_capture_is_opt_in(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    gw = Gateway(answer_store=store, capture_answers=False)  # opted out
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert store.load() == []


async def test_streaming_capture_skips_a_tool_loop_continuation(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    gw = Gateway(answer_store=store, capture_answers=True)
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    await _call(app, _continuation_body("claude-opus-4-8"))
    assert store.load() == []


async def test_buffered_capture_records_the_answer(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "id": "msg_1",
                "content": [{"type": "text", "text": "the answer"}],
                "usage": {"input_tokens": 10, "output_tokens": 5},
            },
        )

    gw = Gateway(answer_store=store, capture_answers=True)
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    body = _stream_body("claude-opus-4-8", "what does this function do?")
    del body["stream"]  # non-streaming request
    resp = await _call(app, body)
    assert resp.status_code == 200
    entries = store.load()
    assert len(entries) == 1
    assert entries[0].answer == "the answer"


async def test_buffered_capture_tolerates_a_non_json_body(tmp_path) -> None:
    # The buffered path's usage/answer extraction must not disturb a non-JSON response —
    # the client still gets it verbatim, and no exception escapes the dispatch.
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"not json")

    gw = Gateway(answer_store=store, capture_answers=True)
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    body = _stream_body("claude-opus-4-8", "say hi")
    del body["stream"]
    resp = await _call(app, body)
    assert resp.status_code == 200
    assert resp.content == b"not json"
    assert store.load() == []


async def test_capture_failure_never_breaks_the_turn() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    gw = Gateway(answer_store=_BoomStore(), capture_answers=True)
    app = make_gateway_app(gw, upstream_base_url="http://up", client=_mock_upstream(handler))
    resp = await _call(app, _stream_body("claude-opus-4-8", "say hi"))
    assert resp.status_code == 200
    assert resp.content == _SSE
