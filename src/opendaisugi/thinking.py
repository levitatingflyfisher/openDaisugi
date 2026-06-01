"""Per-provider thinking-budget mapping (v0.1.3).

The library exposes one knob — ``thinking_budget: Literal["light","standard","deep"]``
— and translates it into provider-specific completion kwargs. Unsupported providers
pass through silently (with a one-time WARNING log per model).
"""
from __future__ import annotations

import logging
import re
from typing import Any, Literal

ThinkingBudget = Literal["light", "standard", "deep"]

_log = logging.getLogger("opendaisugi.thinking")
_LOGGED_UNSUPPORTED: set[str] = set()

_OPENAI_REASONING_PATTERN = re.compile(r"^(openai/)?(o[1-9]|o4)")
_OPENAI_EFFORT = {"light": "low", "standard": "medium", "deep": "high"}


def _log_unsupported_once(model: str, budget: ThinkingBudget) -> None:
    if model in _LOGGED_UNSUPPORTED:
        return
    _LOGGED_UNSUPPORTED.add(model)
    _log.warning(
        "thinking_budget=%r has no effect for model %r; passing through",
        budget, model,
    )


def thinking_kwargs(model: str, budget: ThinkingBudget) -> dict[str, Any]:
    """Return provider-specific kwargs to merge into the chat-completions call.

    Supported providers:
      - Anthropic Claude (``anthropic/...`` or ``claude-...``) — only ``deep``
        enables extended thinking; ``light`` and ``standard`` are no-ops.
      - OpenAI reasoning (``o1``/``o3``/``o4`` family, with or without ``openai/``
        prefix) — budget maps to ``reasoning_effort`` low/medium/high.
      - Gemini thinking-capable (model string contains ``gemini`` and either
        ``thinking`` or ``2.5-pro``) — budget maps to a ``thinking_config`` dict.

    Unsupported providers return ``{}`` and log WARNING once per model.
    """
    m = model.lower()

    if m.startswith("anthropic/") or m.startswith("claude-"):
        if budget == "deep":
            return {"thinking": {"type": "enabled", "budget_tokens": 16000}}
        return {}

    if _OPENAI_REASONING_PATTERN.match(m):
        return {"reasoning_effort": _OPENAI_EFFORT[budget]}

    if "gemini" in m and ("thinking" in m or "2.5-pro" in m):
        if budget == "light":
            return {"thinking_config": {"thinking_budget": 0}}
        if budget == "deep":
            return {"thinking_config": {"thinking_budget": 16000, "include_thoughts": True}}
        return {"thinking_config": {"thinking_budget": 4000}}

    _log_unsupported_once(model, budget)
    return {}
