"""The OpenAI-wire adapter: Codex (and every OpenAI-wire agent) meets the gateway.

The proxy's routing pipeline is wire-neutral — messages in, model out — but
three things are wire-specific: the upstream host, the usage vocabulary
(prompt/completion/cached_tokens vs input/output/cache buckets), and the SSE
event grammar (chat-completion chunks vs message_start/message_delta). This
module pins all three: ``/v1/chat/completions`` routes through its own
Gateway instance (a downgrade must land on an OpenAI-family model — a Claude
name would 404 on api.openai.com), usage is normalized into the meter's
Anthropic-keyed buckets (prompt_tokens INCLUDES cached tokens upstream, so
input = prompt - cached), and the sniffer reads chunk deltas + the final
``stream_options: include_usage`` usage chunk.
"""

from __future__ import annotations

import json

import httpx
import pytest

from opendaisugi.gateway_asgi import make_gateway_app
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.gateway_openai import (
    OpenAIUsageSniffer,
    extract_openai_text,
    is_openai_wire,
    normalize_openai_usage,
)
from opendaisugi.gateway_pipeline import Gateway

pytestmark = pytest.mark.asyncio

_OPENAI_SSE = (
    b'data: {"choices":[{"delta":{"role":"assistant"},"index":0}]}\n\n'
    b'data: {"choices":[{"delta":{"content":"Hello"},"index":0}]}\n\n'
    b'data: {"choices":[{"delta":{"content":" there"},"index":0}]}\n\n'
    b'data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":5,'
    b'"prompt_tokens_details":{"cached_tokens":800}}}\n\n'
    b"data: [DONE]\n\n"
)


def _chat_body(model: str, text: str, *, stream: bool = True) -> dict:
    return {
        "model": model,
        "messages": [
            {"role": "system", "content": "You are Codex."},
            {"role": "user", "content": text},
        ],
        "stream": stream,
    }


async def _call(app, body, path="/v1/chat/completions"):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        return await client.post(
            path,
            content=json.dumps(body).encode(),
            headers={"authorization": "Bearer sk-oai-XYZ", "content-type": "application/json"},
        )


# --- pure pieces -----------------------------------------------------------------


def test_wire_detection():
    assert is_openai_wire("/v1/chat/completions")
    assert is_openai_wire("/openai/v1/chat/completions")
    assert not is_openai_wire("/v1/messages")


def test_normalize_openai_usage_separates_cached_reads():
    u = normalize_openai_usage(
        {
            "prompt_tokens": 1000,
            "completion_tokens": 5,
            "prompt_tokens_details": {"cached_tokens": 800},
        }
    )
    assert u == {
        "input_tokens": 200,
        "output_tokens": 5,
        "cache_read_input_tokens": 800,
        "cache_creation_input_tokens": 0,
    }


def test_normalize_openai_usage_without_details():
    u = normalize_openai_usage({"prompt_tokens": 50, "completion_tokens": 7})
    assert u["input_tokens"] == 50 and u["cache_read_input_tokens"] == 0


def test_extract_openai_text_buffered():
    data = {"choices": [{"message": {"role": "assistant", "content": "The answer."}}]}
    assert extract_openai_text(data) == "The answer."
    assert extract_openai_text({"choices": []}) == ""


def test_sniffer_reads_chunks_and_final_usage():
    s = OpenAIUsageSniffer()
    for i in range(0, len(_OPENAI_SSE), 17):  # ragged chunking on purpose
        s.feed(_OPENAI_SSE[i : i + 17])
    assert s.text == "Hello there"
    assert s.usage["input_tokens"] == 200
    assert s.usage["cache_read_input_tokens"] == 800
    assert s.usage["output_tokens"] == 5


# --- through the proxy -----------------------------------------------------------


def _mock(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_easy_turn_downgrades_to_openai_cheap_model():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["host"] = str(request.url).rsplit("/v1", 1)[0]
        seen["model"] = json.loads(request.content)["model"]
        seen["auth"] = request.headers.get("authorization")
        return httpx.Response(
            200, headers={"content-type": "text/event-stream"}, content=_OPENAI_SSE
        )

    app = make_gateway_app(
        Gateway(),
        upstream_base_url="http://anthropic-up",
        openai_gateway=Gateway(cheap_model="gpt-5-mini"),
        openai_upstream_base_url="http://openai-up",
        client=_mock(handler),
    )
    resp = await _call(app, _chat_body("gpt-5.3-codex", "say hi please"))
    assert resp.status_code == 200
    assert seen["host"] == "http://openai-up"
    assert seen["model"] == "gpt-5-mini"
    assert seen["auth"] == "Bearer sk-oai-XYZ"


async def test_hard_turn_keeps_the_requested_model():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["model"] = json.loads(request.content)["model"]
        return httpx.Response(
            200, headers={"content-type": "text/event-stream"}, content=_OPENAI_SSE
        )

    app = make_gateway_app(
        Gateway(),
        openai_gateway=Gateway(cheap_model="gpt-5-mini"),
        openai_upstream_base_url="http://openai-up",
        client=_mock(handler),
    )
    await _call(
        app, _chat_body("gpt-5.3-codex", "debug the deadlock race condition in the scheduler")
    )
    assert seen["model"] == "gpt-5.3-codex"


async def test_openai_turn_is_journaled_with_normalized_usage(tmp_path):
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, headers={"content-type": "text/event-stream"}, content=_OPENAI_SSE
        )

    app = make_gateway_app(
        Gateway(),
        openai_gateway=Gateway(cheap_model="gpt-5-mini", journal=journal),
        openai_upstream_base_url="http://openai-up",
        client=_mock(handler),
    )
    await _call(app, _chat_body("gpt-5.3-codex", "say hi please"))
    records = journal.load()
    assert len(records) == 1
    assert records[0].downgraded is True
    assert records[0].model == "gpt-5-mini"


async def test_without_openai_gateway_the_path_is_a_pure_passthrough():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["model"] = json.loads(request.content)["model"]
        return httpx.Response(200, json={"choices": []})

    app = make_gateway_app(
        Gateway(),
        openai_upstream_base_url="http://openai-up",
        client=_mock(handler),
    )
    await _call(app, _chat_body("gpt-5.3-codex", "say hi please", stream=False))
    assert seen["model"] == "gpt-5.3-codex"  # no openai gateway → no routing, no rewrite


async def test_anthropic_path_still_routes_to_anthropic_upstream():
    seen = {}
    _SSE = (
        b'data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}\n\n'
        b'data: {"type":"message_delta","usage":{"output_tokens":2}}\n\n'
    )

    def handler(request: httpx.Request) -> httpx.Response:
        seen["host"] = str(request.url).rsplit("/v1", 1)[0]
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_SSE)

    app = make_gateway_app(
        Gateway(),
        upstream_base_url="http://anthropic-up",
        openai_gateway=Gateway(cheap_model="gpt-5-mini"),
        openai_upstream_base_url="http://openai-up",
        client=_mock(handler),
    )
    body = {
        "model": "claude-opus-4-8",
        "messages": [{"role": "user", "content": "say hi please"}],
        "stream": True,
    }
    await _call(app, body, path="/v1/messages")
    assert seen["host"] == "http://anthropic-up"
