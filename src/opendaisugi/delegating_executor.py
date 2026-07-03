"""DelegatingExecutor — runs a step by prompting a configurable LLM (v0.19).

Sits at the bottom of the v0.18 reproduction substrate's selection signal:
trustworthy receipts only matter if runs differ in cost, and runs only
differ in cost if some steps execute against a cheap model. This executor
makes that real.

Design constraints:
- Reuses ``opendaisugi.llm.get_instructor_client`` so the same backend
  switch (``OPENDAISUGI_LLM_BACKEND``) governs delegation as governs
  envelope generation.
- Honors ``step.preferred_model`` over the executor's ``default_model``,
  so the agent (under the Checklist skill) can author per-step preferences.
- Records the model used on the executor instance so the supervisor's
  ``_write_step_receipt`` can stamp it into ``Receipt.model_id`` (L6).
- Does not gate on stakes; the verifier's ``_check_delegation_safety``
  (L4) refuses physical-stakes envelopes before any executor is invoked.
"""

from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass
from typing import Any, Callable

from opendaisugi.executor import ExecutorResult
from opendaisugi.llm import translate_llm_error
from opendaisugi.models import StepBase

_log = logging.getLogger("opendaisugi.delegating_executor")


@dataclass
class _LastInvocation:
    model: str | None = None
    attempts: int = 0
    # v0.32: total tokens reported by the backend for the last call, when it
    # exposes usage (litellm ``result.usage.total_tokens`` or claude-code
    # ``--output-format json`` usage). None when unavailable or mocked.
    # The budget-aware executor reads this to record actual spend.
    tokens: int | None = None
    # v0.33.2: measured dollar cost, exact, from the claude-code backend's
    # ``total_cost_usd`` (Claude Code's own accounting; works on a subscription).
    # None on backends that don't report a cost (litellm → estimated elsewhere).
    cost_usd: float | None = None


def _extract_total_tokens(result: object) -> int | None:
    """Pull ``usage.total_tokens`` off a litellm result, tolerant of shape."""
    usage = getattr(result, "usage", None)
    if usage is None:
        return None
    total = getattr(usage, "total_tokens", None)
    if total is None and isinstance(usage, dict):
        total = usage.get("total_tokens")
    try:
        return int(total) if total is not None else None
    except (TypeError, ValueError):
        return None


class DelegatingExecutor:
    """StepExecutor that runs a step by prompting an LLM.

    Construction:
        DelegatingExecutor(
            default_model="haiku",
            prompt_template=lambda step: f"Execute: {step.model_dump_json()}",
            response_schema=None,            # optional Pydantic class
            max_retries=2,
        )

    On run(step):
      1. Resolve model = step.preferred_model OR self.default_model
      2. Render prompt via prompt_template(step)
      3. Call the resolved LLM client
      4. If response_schema is set, validate; on validation error retry up to max_retries
      5. Return ExecutorResult(rc=0, stdout=<JSON of validated content>, ...)
         OR ExecutorResult(rc=1, ...) on terminal failure

    Stamps ``self.last`` with the model used and number of attempts so the
    supervisor can populate Receipt.model_id afterwards.
    """

    def __init__(
        self,
        *,
        default_model: str = "haiku",
        prompt_template: Callable[[StepBase], str] | None = None,
        response_schema: type | None = None,
        max_retries: int = 2,
        backend: str | None = None,
        json_mode: bool = True,
        endpoint_overrides: "dict[str, dict[str, Any]] | None" = None,
    ) -> None:
        self.default_model = default_model
        self.prompt_template = prompt_template or self._default_prompt
        self.response_schema = response_schema
        self.max_retries = max_retries
        self.backend = backend
        # v0.32: per-model litellm kwargs (api_base/api_key) so a local rung's
        # model actually reaches its endpoint. Applied only to the matching model
        # id, so cloud rungs in the same executor are untouched.
        self.endpoint_overrides = dict(endpoint_overrides or {})
        # v0.32: evidence steps (v0.19) want JSON; a natural-language TaskStep
        # wants free text. json_mode=False drops the response_format so the model
        # answers the subtask in prose instead of being forced into a JSON object.
        self.json_mode = json_mode
        self.last = _LastInvocation()
        # Set by the backend call as a side channel; folded into self.last after
        # each call so a patched _call (returning a bare str) leaves them None.
        self._last_usage: int | None = None
        self._last_cost: float | None = None

    @staticmethod
    def _default_prompt(step: StepBase) -> str:
        """Stringify the step's fields as a prompt. Kits override for richer prompts."""
        return (
            "Execute the following step. Respond with a single JSON object whose "
            "keys are the structured-evidence fields the step's postcondition "
            "requires. Step: " + step.model_dump_json()
        )

    def _resolve_model(self, step: StepBase) -> str:
        return getattr(step, "preferred_model", None) or self.default_model

    def _call_litellm_sync(
        self, model: str, prompt: str,
        *, timeout_s: int, max_tokens: int,
    ) -> str:
        """Synchronous wrapper around the async litellm call. Honors timeout
        and a max-tokens cap derived from the supervisor's max_output_bytes.
        """
        from litellm import completion
        # Direct litellm call (not instructor) — we want the raw text content;
        # response_schema validation runs in our retry loop, not via instructor.
        extra = {"response_format": {"type": "json_object"}} if self.json_mode else {}
        extra.update(self.endpoint_overrides.get(model, {}))
        result = completion(
            model=model,
            messages=[{"role": "user", "content": prompt}],
            timeout=timeout_s,
            max_tokens=max_tokens,
            **extra,
        )
        self._last_usage = _extract_total_tokens(result)
        return result.choices[0].message.content or ""

    def _call_claude_code_sync(
        self, model: str, prompt: str, *, timeout_s: int,
    ) -> str:
        # Honor json_mode on the claude-code backend too: a prose TaskStep
        # (json_mode=False) must get raw text, not be forced through JSON
        # extraction — which raises on a prose answer and fails the step. The
        # metered variant also captures Claude Code's exact usage + cost.
        if not self.json_mode:
            from opendaisugi.claude_code_llm import call_claude_p_metered
            text, meter = call_claude_p_metered(prompt, timeout_s=float(timeout_s), model=model)
            self._last_usage = meter.get("tokens")
            self._last_cost = meter.get("cost_usd")
            return text
        from opendaisugi.claude_code_llm import call_claude_p_json_sync
        body = call_claude_p_json_sync(prompt, timeout_s=float(timeout_s), model=model)
        return json.dumps(body)

    def _call(
        self, model: str, prompt: str,
        *, timeout_s: int, max_tokens: int,
    ) -> str:
        from opendaisugi.llm import resolve_backend
        if resolve_backend(self.backend) == "claude-code":
            return self._call_claude_code_sync(model, prompt, timeout_s=timeout_s)
        return self._call_litellm_sync(
            model, prompt, timeout_s=timeout_s, max_tokens=max_tokens,
        )

    def run(
        self,
        step: StepBase,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        # Roughly 4 bytes/token; floor at 256 so even tiny budgets don't
        # produce zero-length completions.
        max_tokens = max(256, max_output_bytes // 4)
        started = time.time()
        model = self._resolve_model(step)
        prompt = self.prompt_template(step)
        last_error: str | None = None
        last_content: str = ""

        for attempt in range(1, self.max_retries + 2):
            self.last = _LastInvocation(model=model, attempts=attempt)
            self._last_usage = None
            self._last_cost = None
            try:
                content = self._call(
                    model, prompt,
                    timeout_s=timeout_s, max_tokens=max_tokens,
                )
                self.last = _LastInvocation(
                    model=model, attempts=attempt,
                    tokens=self._last_usage, cost_usd=self._last_cost,
                )
            except Exception as exc:  # noqa: BLE001 — translate at boundary
                last_error = str(translate_llm_error(exc))
                _log.warning(
                    "delegate.call_error",
                    extra={"step_id": step.id, "model": model, "attempt": attempt, "error": last_error},
                )
                continue
            last_content = content
            if self.response_schema is None:
                break
            try:
                self.response_schema.model_validate_json(content)
                break
            except Exception as exc:  # validation failure
                last_error = f"schema validation failed: {exc}"
                _log.info(
                    "delegate.schema_retry",
                    extra={"step_id": step.id, "model": model, "attempt": attempt},
                )
                continue
        else:
            return ExecutorResult(
                rc=1,
                stdout=f"delegating_executor: exhausted retries: {last_error}",
                duration_ms=(time.time() - started) * 1000.0,
                timed_out=False,
            )

        return ExecutorResult(
            rc=0,
            stdout=last_content,
            duration_ms=(time.time() - started) * 1000.0,
            timed_out=False,
        )
