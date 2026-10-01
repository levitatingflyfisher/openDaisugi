"""The smart end of the router slot: a model that picks, and a model that grades.

Both go through the gateway's own upstream path (the same client, URL and headers
as the user's turn), so they add no client and no dependency. Both prompts are
fixed text: the Go and Rust gateways send the same bytes, and the fake upstream of
the compare cases is keyed on them.
"""

from __future__ import annotations

import json
from collections.abc import Sequence

TEXT_LIMIT = 2000
ASK_TIMEOUT_S = 15.0


def _body(model: str, prompt: str, max_tokens: int) -> bytes:
    body = {
        "model": model,
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }
    return json.dumps(body).encode("utf-8")


def picker_body(router_model: str, task: str, candidates: Sequence[str]) -> bytes:
    prompt = (
        f"Pick the model for this task. Allowed models: {', '.join(candidates)}.\n"
        f"Reply with one name from the list and nothing else.\n\n"
        f"Task:\n{task[:TEXT_LIMIT]}"
    )
    return _body(router_model, prompt, 20)


def parse_pick(text: str | None, candidates: Sequence[str]) -> str | None:
    """The allowed name the model answered, or None."""
    if text is None:
        return None
    answer = text.strip().strip("`'\".")
    return answer if answer in candidates else None


def grader_body(grader_model: str, task: str, answer: str) -> bytes:
    prompt = (
        f"Task:\n{task[:TEXT_LIMIT]}\n\nAnswer:\n{answer[:TEXT_LIMIT]}\n\n"
        f"Did the answer do the task? Reply with one word: good or bad."
    )
    return _body(grader_model, prompt, 5)


def parse_grade(text: str | None) -> str | None:
    """good or bad from the grader's reply, or None for anything else."""
    if text is None:
        return None
    word = text.strip().strip("`'\".").lower()
    return word if word in ("good", "bad") else None


def reply_text(data: object) -> str | None:
    """The text of a Messages reply, or None when it has none."""
    if not isinstance(data, dict):
        return None
    content = data.get("content")
    if not isinstance(content, list):
        return None
    parts = [b.get("text") for b in content if isinstance(b, dict) and b.get("type") == "text"]
    parts = [p for p in parts if isinstance(p, str)]
    return "".join(parts) if parts else None


def off(value: str | None) -> str | None:
    """A config value for the grader or router model: None when unset or ``off``."""
    if value is None or not value.strip() or value.strip().lower() == "off":
        return None
    return value.strip()
