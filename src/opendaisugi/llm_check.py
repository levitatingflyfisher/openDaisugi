"""LLM-as-verifier primitive helper (v0.9.0).

Isolated so tests can monkeypatch the network call without touching the
main predicate evaluator. Used by LLMCheck primitives in the predicate
algebra. Stakes-gated: the evaluator rejects LLMCheck expressions in
physical-stakes envelopes before this module is ever called.

v0.12.0: routes through ``OPENDAISUGI_LLM_BACKEND=claude-code`` when set,
so subscription-credits users get LLMCheck without an API key.

v0.27.0: ``run_llm_check`` is the preferred entry point — it wraps
``_invoke_model`` and fails CLOSED on any exception (network error,
timeout, rate-limit). A failed probabilistic check never silently approves.
"""

from __future__ import annotations

import json
import logging
import os
from dataclasses import dataclass, field
from typing import Any

from opendaisugi.llm import resolve_backend

logger = logging.getLogger(__name__)


@dataclass
class LLMCheckResult:
    """Structured result from an LLM verifier call.

    ``satisfied`` is the verdict. ``reason`` is a short human-readable
    rationale. ``errored`` is True when the call itself failed (network,
    timeout, rate-limit) — in that case ``satisfied`` is always False
    (fail-closed).
    """

    satisfied: bool
    reason: str = ""
    errored: bool = False


def _invoke_model(rule: str, payload: dict[str, Any]) -> tuple[bool, str]:
    """Low-level model call — isolated so tests can monkeypatch it.

    Returns (satisfied, rationale). May raise any exception (network,
    timeout, rate-limit, JSON parse failure). Callers should use
    ``run_llm_check`` which wraps this with fail-closed error handling.
    """
    model = os.environ.get(
        "OPENDAISUGI_LLM_CHECK_MODEL", "anthropic/claude-haiku-4-5-20251001"
    )
    system = (
        "You are a strict verifier. Answer in strict JSON: "
        '{"satisfied": true|false, "rationale": "short reason"}. '
        "No prose outside the JSON."
    )
    user = (
        f"Rule:\n{rule}\n\n"
        f"Plan payload (JSON):\n{json.dumps(payload, default=str)[:4000]}\n\n"
        "Does the plan payload satisfy the rule?"
    )

    if resolve_backend() == "claude-code":
        from opendaisugi.claude_code_llm import call_claude_p_json_sync
        from opendaisugi.exceptions import EnvelopeGenerationError

        prompt = f"[system]\n{system}\n\n[user]\n{user}"
        try:
            parsed = call_claude_p_json_sync(prompt, timeout_s=60.0, model="haiku")
        except EnvelopeGenerationError as exc:
            return False, f"llm-check failed: {exc}"
        return bool(parsed.get("satisfied", False)), str(parsed.get("rationale", ""))

    import litellm
    response = litellm.completion(
        model=model,
        messages=[
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
        temperature=0,
        max_tokens=200,
    )
    content = response.choices[0].message.content or ""
    parsed = json.loads(content)
    return bool(parsed.get("satisfied", False)), str(parsed.get("rationale", ""))


def run_llm_check(rule: str, context: dict[str, Any]) -> LLMCheckResult:
    """Call the LLM verifier and fail CLOSED on any error (v0.27.0).

    A raised exception (network, timeout, rate-limit, parse failure) is
    caught and converted into ``LLMCheckResult(satisfied=False, errored=True)``
    — never a silent approval. Use this instead of ``call_llm_check`` for
    any safety-critical evaluation path.
    """
    try:
        satisfied, reason = _invoke_model(rule, context)
        return LLMCheckResult(satisfied=satisfied, reason=reason, errored=False)
    except Exception as exc:  # noqa: BLE001
        logger.warning("llm_check call failed (fail-closed): %s", exc)
        return LLMCheckResult(
            satisfied=False,
            reason=f"error: llm_check call failed: {exc}",
            errored=True,
        )


def call_llm_check(rule: str, payload: dict[str, Any]) -> tuple[bool, str]:
    """Call a small LLM to evaluate a natural-language rule against a payload.

    Returns (satisfied, rationale). Defaults to a cheap Haiku-class option
    via litellm; routes through ``claude -p`` when
    ``OPENDAISUGI_LLM_BACKEND=claude-code``.

    .. deprecated::
        Prefer ``run_llm_check`` which returns a structured ``LLMCheckResult``
        and fails CLOSED on any exception (v0.27.0).
    """
    try:
        return _invoke_model(rule, payload)
    except json.JSONDecodeError as exc:
        return False, f"parser error: {exc}"


__all__ = ["LLMCheckResult", "call_llm_check", "run_llm_check"]
