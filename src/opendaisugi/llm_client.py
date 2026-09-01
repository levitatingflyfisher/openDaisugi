"""Our own model client: the one place a model is called over HTTP.

Two HTTP wires, chosen by the model name:

- the Anthropic Messages API, for ``anthropic/<model>`` and a bare name
  that starts with ``claude``;
- OpenAI-compatible chat completions, for ``openai/<model>`` (OpenAI,
  llamafile, llama.cpp, LM Studio, any local server at a ``base_url``) and
  ``ollama/<model>`` or ``ollama_chat/<model>`` (Ollama's ``/v1`` path).

The third backend, the local ``claude -p`` CLI, lives in
``claude_code_llm``; ``llm.get_model_client`` picks between the two.

Structured output keeps the texts and the re-ask loop the call sites used
through instructor's JSON mode: the same schema text appended to the system
message, ``max_retries + 1`` attempts, a reply validated with
``model_validate_json`` and ``non_finite_error``, and on a schema error the
reply plus "Correct your JSON ONLY RESPONSE ..." sent back.

The request bytes are defined here. The Go and Rust ports send the same
bytes (rulings LLM-1 to LLM-14 in clients/ADJUDICATIONS.md). Nothing here
prints. httpx is imported only when a call is made, so importing this
module opens no socket.
"""

from __future__ import annotations

import json
import math
import os
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from textwrap import dedent
from typing import Any

from pydantic import BaseModel, ValidationError

from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.models import non_finite_error

DEFAULT_MAX_TOKENS = 8192
DEFAULT_TIMEOUT_S = 600.0
TIMEOUT_ENV = "OPENDAISUGI_LLM_TIMEOUT"
ANTHROPIC_VERSION = "2023-06-01"
USER_AGENT = "opendaisugi"

CUT_TEXT = "The output is incomplete due to a max_tokens length limit."
NO_KEY_TEXT = "no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set"
REASK_TEXT = "Correct your JSON ONLY RESPONSE, based on the following errors:\n"

_KEY_RE = re.compile(r"(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})")
_USERINFO_RE = re.compile(r"://[^/@]*@")


class ModelCallError(EnvelopeGenerationError):
    """A model call failed. The text names the cause and never holds a key."""


def clean(text: str) -> str:
    """``text`` with API-key-shaped tokens shortened and URL credentials dropped."""
    return _KEY_RE.sub(r"\1...\2", _USERINFO_RE.sub("://", text))


def _fail(text: str) -> ModelCallError:
    return ModelCallError(clean(text))


# ---------------------------------------------------------------------------
# Which wire, which URL, which credential
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Wire:
    """Where one call goes: ``kind`` is ``messages`` or ``chat``."""

    kind: str
    model: str
    url: str
    headers: tuple[tuple[str, str], ...]


def _first(env: Mapping[str, str], *names: str) -> str | None:
    for n in names:
        v = env.get(n)
        if v:
            return v
    return None


def _join(base: str, suffix: str) -> str:
    base = base.rstrip("/")
    return base if base.endswith(suffix) else base + suffix


def _ollama_url(base: str) -> str:
    base = base.rstrip("/")
    if base.endswith("/chat/completions"):
        return base
    if base.endswith("/v1"):
        return base + "/chat/completions"
    return base + "/v1/chat/completions"


_COMMON_HEADERS = (
    ("accept", "application/json"),
    ("accept-encoding", "identity"),
    ("content-type", "application/json"),
    ("user-agent", USER_AGENT),
)


def resolve_wire(
    model: str,
    *,
    base_url: str | None = None,
    api_key: str | None = None,
    env: Mapping[str, str] | None = None,
) -> Wire:
    """The wire, URL and headers for ``model``. Raises ModelCallError, before
    any request, for a name no wire serves or a Messages call with no key."""
    env = os.environ if env is None else env
    headers = list(_COMMON_HEADERS)
    if model.startswith("anthropic/") or model.startswith("claude"):
        name = model.removeprefix("anthropic/")
        base = base_url or _first(env, "ANTHROPIC_API_BASE", "ANTHROPIC_BASE_URL")
        url = _join(base or "https://api.anthropic.com", "/v1/messages")
        headers.append(("anthropic-version", ANTHROPIC_VERSION))
        key = api_key or _first(env, "ANTHROPIC_API_KEY")
        if key:
            headers.append(("x-api-key", key))
        else:
            token = _first(env, "ANTHROPIC_AUTH_TOKEN")
            if not token:
                raise _fail(NO_KEY_TEXT)
            headers.append(("authorization", "Bearer " + token))
        return Wire("messages", name, url, tuple(headers))
    if model.startswith("openai/"):
        base = base_url or _first(env, "OPENAI_API_BASE", "OPENAI_BASE_URL")
        url = _join(base or "https://api.openai.com/v1", "/chat/completions")
        key = api_key or _first(env, "OPENAI_API_KEY")
        if key:
            headers.append(("authorization", "Bearer " + key))
        return Wire("chat", model.removeprefix("openai/"), url, tuple(headers))
    for prefix in ("ollama/", "ollama_chat/"):
        if model.startswith(prefix):
            base = base_url or _first(env, "OLLAMA_API_BASE")
            url = _ollama_url(base or "http://localhost:11434")
            if api_key:
                headers.append(("authorization", "Bearer " + api_key))
            return Wire("chat", model.removeprefix(prefix), url, tuple(headers))
    raise _fail(
        f"no model wire for {model!r}: name it anthropic/<model>, openai/<model> or ollama/<model>"
    )


def resolve_timeout(timeout: float | None, env: Mapping[str, str] | None = None) -> float:
    """The call's timeout, else ``OPENDAISUGI_LLM_TIMEOUT``, else 600 seconds.
    A value that is not a finite number above zero is ignored."""
    if timeout is not None:
        return float(timeout)
    env = os.environ if env is None else env
    raw = env.get(TIMEOUT_ENV)
    if raw is not None:
        try:
            t = float(raw)
        except ValueError:
            t = math.nan
        if math.isfinite(t) and t > 0:
            return t
    return DEFAULT_TIMEOUT_S


# ---------------------------------------------------------------------------
# The request body
# ---------------------------------------------------------------------------


def _text(content: Any) -> str:
    if not isinstance(content, str):
        raise TypeError(f"a message's content must be a str, not {type(content).__name__}")
    return content


def request_body(
    wire: Wire,
    messages: Sequence[Mapping[str, Any]],
    *,
    max_tokens: int | None = None,
    temperature: float | None = None,
    json_object: bool = False,
    thinking: Mapping[str, Any] | None = None,
    reasoning_effort: str | None = None,
) -> bytes:
    """The exact bytes one call sends: compact JSON, every non-ASCII
    character escaped."""
    if wire.kind == "messages":
        system = [_text(m.get("content")) for m in messages if m.get("role") == "system"]
        rest = [
            {"role": m.get("role", "user"), "content": _text(m.get("content"))}
            for m in messages
            if m.get("role") != "system"
        ]
        if max_tokens is None:
            max_tokens = DEFAULT_MAX_TOKENS
            if thinking and isinstance(thinking.get("budget_tokens"), int):
                max_tokens += thinking["budget_tokens"]
        body: dict[str, Any] = {"model": wire.model, "max_tokens": max_tokens}
        if system:
            body["system"] = "\n\n".join(system)
        body["messages"] = rest
        if temperature is not None:
            body["temperature"] = temperature
        if thinking:
            body["thinking"] = dict(thinking)
    else:
        body = {
            "model": wire.model,
            "messages": [
                {"role": m.get("role", "user"), "content": _text(m.get("content"))}
                for m in messages
            ],
        }
        if max_tokens is not None:
            body["max_tokens"] = max_tokens
        if temperature is not None:
            body["temperature"] = temperature
        if json_object:
            body["response_format"] = {"type": "json_object"}
        if reasoning_effort is not None:
            body["reasoning_effort"] = reasoning_effort
    return json.dumps(body, separators=(",", ":"), ensure_ascii=True).encode("ascii")


# ---------------------------------------------------------------------------
# Sending and reading
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Reply:
    """A model's answer: its text, whether it was cut at the token limit, and
    the tokens the server reports (None when it reports none)."""

    text: str
    cut: bool = False
    tokens: int | None = None
    input_tokens: int | None = None
    output_tokens: int | None = None


def _unreadable(text: str) -> ModelCallError:
    return _fail(f"the model reply could not be read: {text[:200]}")


def _int(v: Any) -> int | None:
    return v if isinstance(v, int) and not isinstance(v, bool) else None


def read_reply(wire: Wire, status: int, text: str) -> Reply:
    """Read a response: a status other than 2xx, or a body that is not the
    wire's reply, raises ModelCallError."""
    if not 200 <= status <= 299:
        raise _fail(f"the model server answered HTTP {status}: {text[:500]}")
    try:
        obj = json.loads(text)
    except (ValueError, RecursionError):
        raise _unreadable(text) from None
    if not isinstance(obj, dict):
        raise _unreadable(text)
    usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
    if wire.kind == "messages":
        blocks = obj.get("content")
        if not isinstance(blocks, list):
            raise _unreadable(text)
        parts = [
            b["text"]
            for b in blocks
            if isinstance(b, dict) and b.get("type") == "text" and isinstance(b.get("text"), str)
        ]
        i, o = _int(usage.get("input_tokens")), _int(usage.get("output_tokens"))
        tokens = i + o if i is not None and o is not None else None
        return Reply("".join(parts), obj.get("stop_reason") == "max_tokens", tokens, i, o)
    choices = obj.get("choices")
    if not isinstance(choices, list) or not choices or not isinstance(choices[0], dict):
        raise _unreadable(text)
    message = choices[0].get("message")
    if not isinstance(message, dict):
        raise _unreadable(text)
    content = message.get("content")
    if content is None:
        content = ""
    if not isinstance(content, str):
        raise _unreadable(text)
    return Reply(
        content,
        choices[0].get("finish_reason") == "length",
        _int(usage.get("total_tokens")),
        _int(usage.get("prompt_tokens")),
        _int(usage.get("completion_tokens")),
    )


def _httpx() -> Any:
    try:
        import httpx
    except ImportError as exc:
        raise ImportError(
            "the model client needs the 'generate' extra (httpx): "
            "uv add 'opendaisugi[generate]'  (or: pip install 'opendaisugi[generate]')"
        ) from exc
    return httpx


def _transport_error(exc: BaseException, url: str) -> ModelCallError:
    httpx = _httpx()

    if isinstance(exc, httpx.TimeoutException):
        return _fail(f"the model call to {url} timed out")
    if isinstance(exc, httpx.ProxyError):
        return _fail(f"the proxy refused the model call: {exc}")
    return _fail(f"could not reach the model server at {url}")


def post(wire: Wire, body: bytes, timeout: float) -> tuple[int, str]:
    """One POST. httpx reads the proxy settings from the environment."""
    httpx = _httpx()

    try:
        client = httpx.Client(timeout=timeout, trust_env=True, follow_redirects=False)
    except (ImportError, ValueError) as exc:
        # httpx cannot build a client for this proxy setting (a SOCKS or an
        # unknown scheme).
        raise _fail(f"the model call could not start: {exc}") from None
    try:
        with client:
            r = client.post(wire.url, content=body, headers=list(wire.headers))
            return r.status_code, r.content.decode("utf-8", "replace")
    except (httpx.HTTPError, httpx.InvalidURL) as exc:
        raise _transport_error(exc, wire.url) from None


async def apost(wire: Wire, body: bytes, timeout: float) -> tuple[int, str]:
    """``post``, without blocking the event loop."""
    httpx = _httpx()

    try:
        client = httpx.AsyncClient(timeout=timeout, trust_env=True, follow_redirects=False)
    except (ImportError, ValueError) as exc:
        raise _fail(f"the model call could not start: {exc}") from None
    try:
        async with client:
            r = await client.post(wire.url, content=body, headers=list(wire.headers))
            return r.status_code, r.content.decode("utf-8", "replace")
    except (httpx.HTTPError, httpx.InvalidURL) as exc:
        raise _transport_error(exc, wire.url) from None


def complete(
    model: str,
    messages: Sequence[Mapping[str, Any]],
    *,
    max_tokens: int | None = None,
    temperature: float | None = None,
    json_object: bool = False,
    base_url: str | None = None,
    api_key: str | None = None,
    timeout: float | None = None,
    env: Mapping[str, str] | None = None,
) -> Reply:
    """One plain call, blocking: the reply's text, unvalidated."""
    wire = resolve_wire(model, base_url=base_url, api_key=api_key, env=env)
    body = request_body(
        wire, messages, max_tokens=max_tokens, temperature=temperature, json_object=json_object
    )
    status, text = post(wire, body, resolve_timeout(timeout, env))
    return read_reply(wire, status, text)


# ---------------------------------------------------------------------------
# Structured output
# ---------------------------------------------------------------------------


def json_mode_system(response_model: type[BaseModel]) -> str:
    """The schema text appended to the system message, as instructor's JSON
    mode writes it (dedent of a template whose schema lines are not
    indented, so the template's own indent stays)."""
    schema = response_model.model_json_schema()
    return dedent(
        f"""
            As a genius expert, your task is to understand the content and provide
            the parsed objects in json that match the following json_schema:\n

            {json.dumps(schema, indent=2, ensure_ascii=False)}

            Make sure to return an instance of the JSON, not the schema itself
            """
    )


def with_schema(
    messages: Sequence[Mapping[str, Any]], response_model: type[BaseModel]
) -> list[dict[str, Any]]:
    """The messages with the schema text in the system message (appended
    after a blank line, or a new system message first), then consecutive
    messages of one role joined by a blank line."""
    suffix = json_mode_system(response_model)
    msgs = [dict(m) for m in messages]
    if msgs and msgs[0].get("role") == "system":
        msgs[0]["content"] = _text(msgs[0].get("content")) + "\n\n" + suffix
    else:
        msgs.insert(0, {"role": "system", "content": suffix})
    merged: list[dict[str, Any]] = []
    for m in msgs:
        role = m.get("role", "user")
        content = _text(m.get("content") if m.get("content") is not None else "")
        if merged and merged[-1]["role"] == role:
            merged[-1]["content"] += "\n\n" + content
        else:
            merged.append({"role": role, "content": content})
    return merged


def validate_reply(response_model: type[BaseModel], text: str) -> BaseModel:
    """The reply as ``response_model``; a number that is not finite is a
    schema error too (GD-16). Raises pydantic's ValidationError."""
    reply = response_model.model_validate_json(text)
    bad = non_finite_error(reply, response_model.__name__)
    if bad is not None:
        raise bad
    return reply


def flatten(first: BaseException, attempts: int) -> str:
    """One line for a structured call that never validated: the first
    attempt's error and the count."""
    lines = [ln for ln in str(first).strip().splitlines() if ln.strip()]
    line = lines[0].strip() if lines else first.__class__.__name__
    return f"{first.__class__.__name__}: {line} ({attempts} attempts)"


class _Completions:
    def __init__(self, parent: "ModelClient") -> None:
        self._parent = parent

    async def create(
        self,
        *,
        model: str,
        messages: Sequence[Mapping[str, Any]],
        response_model: type[BaseModel] | None = None,
        max_retries: int = 3,
        max_tokens: int | None = None,
        temperature: float | None = None,
        timeout: float | None = None,
        base_url: str | None = None,
        api_base: str | None = None,
        api_key: str | None = None,
        thinking: Mapping[str, Any] | None = None,
        reasoning_effort: str | None = None,
        **_ignored: Any,
    ) -> Any:
        env = self._parent.env
        wire = resolve_wire(model, base_url=base_url or api_base, api_key=api_key, env=env)
        limit = resolve_timeout(timeout, env)

        async def send(msgs: list[dict[str, Any]], json_object: bool) -> Reply:
            body = request_body(
                wire,
                msgs,
                max_tokens=max_tokens,
                temperature=temperature,
                json_object=json_object,
                thinking=thinking,
                reasoning_effort=reasoning_effort,
            )
            status, text = await apost(wire, body, limit)
            return read_reply(wire, status, text)

        if response_model is None:
            return (await send([dict(m) for m in messages], False)).text
        msgs = with_schema(messages, response_model)
        attempts = max(max_retries, 0) + 1 if isinstance(max_retries, int) else 1
        first: ValidationError | None = None
        for n in range(1, attempts + 1):
            reply = await send(msgs, True)
            if reply.cut:
                raise _fail(CUT_TEXT)
            try:
                return validate_reply(response_model, reply.text)
            except ValidationError as exc:
                first = first or exc
                if n == attempts:
                    raise _fail(flatten(first, n)) from exc
                msgs = msgs + [
                    {"role": "assistant", "content": reply.text},
                    {"role": "user", "content": REASK_TEXT + str(exc)},
                ]
        raise AssertionError("unreachable")


class _Chat:
    def __init__(self, parent: "ModelClient") -> None:
        self.completions = _Completions(parent)


class ModelClient:
    """The HTTP backend's client: ``.chat.completions.create(model=...,
    messages=..., response_model=..., max_retries=...)``, the call shape every
    structured call site uses (the claude-code backend's client has it too)."""

    def __init__(self, env: Mapping[str, str] | None = None) -> None:
        self.env = env
        self.chat = _Chat(self)


__all__ = [
    "CUT_TEXT",
    "DEFAULT_MAX_TOKENS",
    "ModelCallError",
    "ModelClient",
    "Reply",
    "Wire",
    "clean",
    "complete",
    "json_mode_system",
    "read_reply",
    "request_body",
    "resolve_timeout",
    "resolve_wire",
    "with_schema",
]
