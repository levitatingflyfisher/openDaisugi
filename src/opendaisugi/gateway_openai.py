"""OpenAI-wire adaptation for the gateway (Codex and every OpenAI-wire agent).

The routing pipeline is wire-neutral; what differs per wire is the upstream
host, the usage vocabulary, and the SSE event grammar. This module holds the
OpenAI side of each: path detection for ``/chat/completions``, usage
normalization into the meter's Anthropic-keyed buckets (OpenAI's
``prompt_tokens`` INCLUDES ``cached_tokens``, so fresh input is the
difference; OpenAI cache writes are automatic and unpriced, so the
cache-creation bucket is always zero), buffered text extraction from
``choices``, and an SSE sniffer for chunk deltas plus the final
``stream_options: include_usage`` usage chunk. A stream without that final
chunk yields empty usage — the meter records an unmeasured turn rather than
inventing numbers.
"""

from __future__ import annotations

import json


def is_openai_wire(path: str) -> bool:
    """True for OpenAI Chat Completions paths (``…/chat/completions``)."""
    return path.rstrip("/").endswith("/chat/completions")


def normalize_openai_usage(usage: object) -> dict[str, int]:
    """Map OpenAI usage onto the meter's Anthropic-keyed buckets."""
    if not isinstance(usage, dict):
        return {}
    out: dict[str, int] = {}
    prompt = usage.get("prompt_tokens")
    completion = usage.get("completion_tokens")
    details = usage.get("prompt_tokens_details")
    cached = details.get("cached_tokens", 0) if isinstance(details, dict) else 0
    cached = cached if isinstance(cached, int) else 0
    if isinstance(prompt, int):
        out["input_tokens"] = max(0, prompt - cached)
        out["cache_read_input_tokens"] = cached
        out["cache_creation_input_tokens"] = 0
    if isinstance(completion, int):
        out["output_tokens"] = completion
    return out


def extract_openai_text(data: dict) -> str:
    """Best-effort answer text of a buffered chat completion."""
    try:
        choices = data.get("choices") or []
        if not choices:
            return ""
        content = (choices[0].get("message") or {}).get("content")
        return content if isinstance(content, str) else ""
    except Exception:
        return ""


class OpenAIUsageSniffer:
    """Incremental SSE reader: chat-chunk text deltas + the final usage chunk.

    Same feed contract as the Anthropic ``_UsageSniffer`` — ``feed(bytes)``
    accumulates ``self.text`` and ``self.usage`` (already normalized) — so the
    dispatch layer can hold either sniffer without caring which wire it is.
    Anything unparseable is ignored: measurement must never disturb the
    proxied bytes.
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
                for choice in obj.get("choices") or []:
                    delta = choice.get("delta")
                    if isinstance(delta, dict):
                        text = delta.get("content")
                        if isinstance(text, str):
                            self.text += text
                normalized = normalize_openai_usage(obj.get("usage"))
                if normalized:
                    self.usage.update(normalized)
            except Exception:  # best-effort — a malformed event must never break the stream
                continue
