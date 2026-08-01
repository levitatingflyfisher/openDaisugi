"""The token-saving gateway's async transport — a bare ASGI proxy for Anthropic Messages.

Point any harness at this process with ``base_url`` and it forwards each whole turn to the
real provider, first swapping an easy turn onto the cheap model. It carries the credential it
is handed straight through — the transport probe showed Claude Code authenticates with
``Authorization: Bearer`` (OAuth/subscription), so this proxy never needs an API key of its
own; it forwards whatever the harness sent.

The design's one hard rule lives here: **fail open, and decide before the upstream call
opens.** If the body can't be parsed or routed, the original bytes go upstream untouched. If
the *cheap* model rejects the swapped body with a 4xx, the turn is retried once on the
original model — a pre-stream decision, so it never violates "no mid-stream recovery." Either
way the client gets a real answer; only the saving is lost.

Streaming is proxied byte-for-byte while a lightweight sniffer reads the ``usage`` off the
SSE events in passing (never buffering the stream to parse it), so the meter and journal see
real token counts without touching the bytes the client receives.

Requires the ``[gateway]`` extra (httpx). The routing/meter/journal layer works without it;
only :func:`make_gateway_app` imports httpx.
"""

from __future__ import annotations

import json
import logging
from dataclasses import replace
from pathlib import Path
from typing import TYPE_CHECKING

import httpx

from opendaisugi.gateway_openai import (
    OpenAIUsageSniffer,
    extract_openai_text,
    is_openai_wire,
    normalize_openai_usage,
)

if TYPE_CHECKING:
    from opendaisugi.gateway_pipeline import Gateway, PreparedTurn

_log = logging.getLogger("opendaisugi.gateway_asgi")

# Hop-by-hop headers never forwarded either way; plus framing headers we recompute.
_DROP_REQUEST = {
    b"host",
    b"content-length",
    b"connection",
    b"keep-alive",
    b"transfer-encoding",
    b"upgrade",
    b"te",
    b"trailers",
    b"proxy-connection",
}
# On the response we also drop content-encoding: httpx has already decoded the body via
# aiter_bytes, so passing the header through would tell the client to decode a second time.
_DROP_RESPONSE = {
    "content-length",
    "connection",
    "keep-alive",
    "transfer-encoding",
    "upgrade",
    "te",
    "trailers",
    "proxy-connection",
    "content-encoding",
}


class _UsageSniffer:
    """Pulls token counts and the answer text out of an SSE stream incrementally, line by line.

    ``message_start`` carries the input buckets (fresh + cache) in ``message.usage``;
    ``message_delta`` carries the final ``output_tokens``. Later values overwrite earlier
    ones, so a plain merge yields the correct end-of-turn totals. ``content_block_delta``
    events of type ``text_delta`` carry the streamed answer text, concatenated in order into
    ``self.text``. Anything unparseable is ignored — measurement is best-effort and must
    never disturb the proxied bytes.
    """

    def __init__(self) -> None:
        self.usage: dict[str, int] = {}
        self.text: str = ""
        self._buf = ""

    def feed(self, chunk: bytes) -> None:
        self._buf += chunk.decode("utf-8", "replace")
        while "\n" in self._buf:
            line, self._buf = self._buf.split("\n", 1)
            line = line.strip()
            if not line.startswith("data:"):
                continue
            payload = line[len("data:") :].strip()
            if not payload or payload == "[DONE]":
                continue
            try:
                obj = json.loads(payload)
            except json.JSONDecodeError:
                continue
            try:
                u = None
                if obj.get("type") == "message_start":
                    u = obj.get("message", {}).get("usage")
                elif obj.get("type") == "message_delta":
                    u = obj.get("usage")
                elif obj.get("type") == "content_block_delta":
                    delta = obj.get("delta")
                    if isinstance(delta, dict) and delta.get("type") == "text_delta":
                        text = delta.get("text")
                        if isinstance(text, str):
                            self.text += text
                if isinstance(u, dict):
                    self.usage.update({k: v for k, v in u.items() if isinstance(v, int)})
            except Exception:  # best-effort: a malformed event must never break the stream
                continue


async def _read_request_body(receive) -> bytes:
    chunks: list[bytes] = []
    while True:
        msg = await receive()
        if msg["type"] == "http.request":
            chunks.append(msg.get("body", b""))
            if not msg.get("more_body"):
                break
        elif msg["type"] == "http.disconnect":
            break
    return b"".join(chunks)


def _prepare(gateway: "Gateway", body_bytes: bytes):
    """Route the turn, fail-open. Returns (outbound_bytes, prepared_or_None, wants_stream)."""
    try:
        body = json.loads(body_bytes)
        prepared = gateway.prepare(body)
        return json.dumps(prepared.outbound_body).encode(), prepared, body.get("stream") is True
    except Exception:  # unparseable / unroutable → forward the original bytes untouched
        return body_bytes, None, False


def _forward_request_headers(scope) -> list[tuple[str, str]]:
    return [
        (k.decode("latin-1"), v.decode("latin-1"))
        for k, v in scope.get("headers", [])
        if k.lower() not in _DROP_REQUEST
    ]


def _response_headers(resp: httpx.Response) -> list[tuple[bytes, bytes]]:
    return [
        (k.encode("latin-1"), v.encode("latin-1"))
        for k, v in resp.headers.items()
        if k.lower() not in _DROP_RESPONSE
    ]


def make_gateway_app(
    gateway: "Gateway",
    *,
    upstream_base_url: str = "https://api.anthropic.com",
    openai_gateway: "Gateway | None" = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    client: httpx.AsyncClient | None = None,
):
    """Build the ASGI app. ``client`` is injectable so tests can supply a mock upstream;
    in production it is created per-process with no timeout (agent turns run long).

    Two wires share the proxy, selected per-request by path: Anthropic Messages
    (default) and OpenAI Chat Completions (``…/chat/completions`` — Codex and
    every OpenAI-wire agent). Each wire has its own upstream and its own
    Gateway instance, because a downgrade must land on a model the upstream
    serves — a Claude name would 404 on api.openai.com and vice versa. With no
    ``openai_gateway`` configured, OpenAI-wire requests pass through unrouted
    and unmetered."""
    base = upstream_base_url.rstrip("/")
    openai_base = openai_upstream_base_url.rstrip("/")

    async def app(scope, receive, send) -> None:
        if scope["type"] != "http":
            return

        wire = "openai" if is_openai_wire(scope["path"]) else "anthropic"
        active_gateway = openai_gateway if wire == "openai" else gateway
        body_bytes = await _read_request_body(receive)
        if active_gateway is not None:
            outbound_bytes, prepared, wants_stream = _prepare(active_gateway, body_bytes)
        else:
            outbound_bytes, prepared = body_bytes, None
            try:
                wants_stream = json.loads(body_bytes).get("stream") is True
            except Exception:
                wants_stream = False
        headers = _forward_request_headers(scope)
        url = (openai_base if wire == "openai" else base) + scope["path"]
        if scope.get("query_string"):
            url += "?" + scope["query_string"].decode("latin-1")

        # Attempts: the (possibly downgraded) body first; on a downgrade, the original body is
        # the fail-open retry if the cheap model 4xxs. Only ever one retry, decided pre-stream.
        attempts: list[tuple[bytes, bool]] = [(outbound_bytes, False)]
        if prepared is not None and prepared.decision.downgraded:
            attempts.append((body_bytes, True))

        owns_client = client is None
        cl = client or httpx.AsyncClient(timeout=None)
        try:
            if wants_stream:
                await _dispatch_streaming(
                    cl, url, headers, attempts, send, active_gateway, prepared, wire
                )
            else:
                await _dispatch_buffered(
                    cl, url, headers, attempts, send, active_gateway, prepared, wire
                )
        finally:
            if owns_client:
                await cl.aclose()

    return app


def _record(
    gateway: "Gateway",
    prepared: "PreparedTurn | None",
    usage: dict,
    used_original: bool,
    answer_text: str | None = None,
):
    """Measure and journal the turn — best-effort; a meter failure never breaks the proxy.

    ``used_original`` means the fail-open retry ran: the turn actually served on the requested
    model, so it must be booked as *not* downgraded — no phantom saving. ``answer_text`` is
    threaded straight into :meth:`Gateway.finish`, which decides (opt-in, default-off)
    whether it's worth capturing — this function only carries it, never gates it."""
    if prepared is None:
        return
    try:
        eff = prepared
        if used_original:
            reverted = replace(
                prepared.decision,
                model=prepared.decision.requested_model,
                tier="tier2-frontier",
                downgraded=False,
                reason="downgrade rejected by upstream 4xx; served the original model",
            )
            eff = replace(prepared, decision=reverted)
        gateway.finish(eff, usage, answer_text=answer_text)
    except Exception as exc:  # pragma: no cover - defensive
        _log.warning("gateway meter/journal failed (turn still served): %s", exc)


async def _dispatch_streaming(
    cl, url, headers, attempts, send, gateway, prepared, wire="anthropic"
) -> None:
    sniffer = OpenAIUsageSniffer() if wire == "openai" else _UsageSniffer()
    for i, (content, used_original) in enumerate(attempts):
        is_last = i == len(attempts) - 1
        async with cl.stream("POST", url, content=content, headers=headers) as resp:
            if resp.status_code >= 400 and not is_last:
                continue  # fail open: retry on the original model before touching the client
            await send(
                {
                    "type": "http.response.start",
                    "status": resp.status_code,
                    "headers": _response_headers(resp),
                }
            )
            async for chunk in resp.aiter_bytes():
                sniffer.feed(chunk)
                await send({"type": "http.response.body", "body": chunk, "more_body": True})
            await send({"type": "http.response.body", "body": b"", "more_body": False})
            _record(gateway, prepared, sniffer.usage, used_original, answer_text=sniffer.text)
            return


async def _dispatch_buffered(
    cl, url, headers, attempts, send, gateway, prepared, wire="anthropic"
) -> None:
    for i, (content, used_original) in enumerate(attempts):
        is_last = i == len(attempts) - 1
        resp = await cl.request("POST", url, content=content, headers=headers)
        if resp.status_code >= 400 and not is_last:
            continue
        await send(
            {
                "type": "http.response.start",
                "status": resp.status_code,
                "headers": _response_headers(resp),
            }
        )
        await send({"type": "http.response.body", "body": resp.content, "more_body": False})
        usage = {}
        answer_text = ""
        try:
            data = resp.json()
        except (json.JSONDecodeError, ValueError):
            data = None
        if isinstance(data, dict):
            if wire == "openai":
                usage = normalize_openai_usage(data.get("usage"))
                answer_text = extract_openai_text(data)
            else:
                usage = data.get("usage", {}) or {}
                answer_text = _extract_buffered_text(data)
        _record(gateway, prepared, usage, used_original, answer_text=answer_text)
        return


def _extract_buffered_text(data: dict) -> str:
    """Concatenate the text of a non-streamed Messages response's ``content`` blocks.

    Best-effort, mirroring ``_UsageSniffer``: any shape surprise (a missing key, a
    block that isn't a dict, ``content`` that isn't a list) degrades to ``""`` rather
    than raising — extraction must never be able to break the response the client
    already received."""
    try:
        content = data.get("content", [])
        if not isinstance(content, list):
            return ""
        return "".join(
            block.get("text", "")
            for block in content
            if isinstance(block, dict) and block.get("type") == "text"
        )
    except Exception:
        return ""


def build_default_gateway(
    *,
    data_dir: Path,
    cheap_model: str | None = None,
    journalling: bool = True,
    capture_answers: bool = False,
    local_model: str | None = None,
):
    """A :class:`~opendaisugi.gateway_pipeline.Gateway` with a turn journal under
    ``<data_dir>/gateway/turns.jsonl`` — the store the meter and any later reuse tool read.

    ``capture_answers`` is opt-in and default-off: persisting raw response text is a
    privacy choice. When on, an :class:`~opendaisugi.gateway_answers.AnswerStore` is
    created under ``<data_dir>/gateway/answers.jsonl``; when off (the default), no
    answer store exists and nothing is captured."""
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.gateway_pipeline import Gateway

    journal = (
        GatewayJournal(path=Path(data_dir) / "gateway" / "turns.jsonl") if journalling else None
    )
    kwargs: dict = {"journal": journal}
    if cheap_model:
        kwargs["cheap_model"] = cheap_model
    if local_model:
        kwargs["local_model"] = local_model
    if capture_answers:
        from opendaisugi.gateway_answers import AnswerStore

        kwargs["answer_store"] = AnswerStore(path=Path(data_dir) / "gateway" / "answers.jsonl")
        kwargs["capture_answers"] = True
    return Gateway(**kwargs)


def serve_gateway(
    *,
    host: str = "127.0.0.1",
    port: int = 8787,
    upstream_base_url: str = "https://api.anthropic.com",
    data_dir: Path | None = None,
    cheap_model: str | None = None,
    gateway: "Gateway | None" = None,
    capture_answers: bool = False,
    local_model: str | None = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    openai_cheap_model: str | None = "gpt-5-mini",
) -> None:
    """Run the gateway under uvicorn. ``upstream_base_url`` is where saved turns go — a raw
    provider (replace) or a NeMo Switchyard endpoint (compose). Requires the ``[gateway]`` extra.

    ``capture_answers`` is forwarded to :func:`build_default_gateway` when no ``gateway`` is
    supplied directly; it is ignored if the caller passes an already-built ``gateway``."""
    try:
        import uvicorn
    except ImportError:  # pragma: no cover - environment-dependent
        raise ImportError(
            "The gateway server needs the [gateway] extra: uv add 'opendaisugi[gateway]' "
            "(or: pip install 'opendaisugi[gateway]')"
        ) from None

    resolved_dir = data_dir or (Path.home() / ".opendaisugi")
    if gateway is None:
        gateway = build_default_gateway(
            data_dir=resolved_dir,
            cheap_model=cheap_model,
            capture_answers=capture_answers,
            local_model=local_model,
        )
    # The OpenAI wire gets its own pipeline: a downgrade must land on a model
    # the upstream serves. Same journal file — the meter aggregates both wires.
    openai_gateway = (
        build_default_gateway(
            data_dir=resolved_dir,
            cheap_model=openai_cheap_model,
            capture_answers=capture_answers,
            local_model=local_model,
        )
        if openai_cheap_model
        else None
    )
    app = make_gateway_app(
        gateway,
        upstream_base_url=upstream_base_url,
        openai_gateway=openai_gateway,
        openai_upstream_base_url=openai_upstream_base_url,
    )
    uvicorn.run(app, host=host, port=port)
