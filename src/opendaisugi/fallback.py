"""Simplex fallback handlers (v0.2.0).

``FallbackHandler`` is a runtime-checkable protocol mirroring
``StepExecutor`` — Supervisor holds one, calls it on rejection, reacts to
the outcome. Injection at construction enables testing with fakes.

``HaltHandler`` (default) unconditionally halts the run.
``RecomputeHandler`` asks the LLM to re-plan the rejected step.
"""

from __future__ import annotations

import json
import logging
from typing import Protocol, runtime_checkable

from pydantic import BaseModel

from opendaisugi import llm as _llm
from opendaisugi.models import ActionStep, Envelope, VerificationResult
from opendaisugi.verify import verify

_log = logging.getLogger("opendaisugi.fallback")

_RECOMPUTE_SYSTEM_PROMPT = """\
You are a step-repair agent. A step in an action plan was rejected by a \
safety verifier. Your job is to produce a SINGLE replacement step that \
accomplishes the same goal while respecting the envelope's constraints.

Output a JSON object matching the step schema. Do NOT output an envelope \
or a full plan — just one step.
"""


class FallbackOutcome(BaseModel):
    """What the handler decided to do."""

    action: str  # "halted" or "recomputed"
    replacement_step: ActionStep | None = None
    replacement_result: VerificationResult | None = None


@runtime_checkable
class FallbackHandler(Protocol):
    async def handle(
        self,
        step: ActionStep,
        result: VerificationResult,
        envelope: Envelope,
    ) -> FallbackOutcome: ...


class HaltHandler:
    """Unconditionally halts the run. No dependencies, no I/O."""

    async def handle(
        self,
        step: ActionStep,
        result: VerificationResult,
        envelope: Envelope,
    ) -> FallbackOutcome:
        return FallbackOutcome(action="halted")


def _get_recompute_client(model: str):
    """Thin wrapper so tests can patch the client injection point."""
    return _llm.get_instructor_client(model=model)


class RecomputeHandler:
    """Asks the LLM to re-plan a rejected step; verifies the replacement.

    One shot — if the replacement also fails verification, returns halted.
    """

    def __init__(self, *, model: str, z3_timeout_ms: int = 500) -> None:
        self._model = model
        self._z3_timeout_ms = z3_timeout_ms

    async def handle(
        self,
        step: ActionStep,
        result: VerificationResult,
        envelope: Envelope,
    ) -> FallbackOutcome:
        try:
            user_content = self._build_prompt(step, result, envelope)
            client = _get_recompute_client(self._model)
            replacement = await client.chat.completions.create(
                model=self._model,
                max_retries=2,
                response_model=type(step),
                messages=[
                    {"role": "system", "content": _RECOMPUTE_SYSTEM_PROMPT},
                    {"role": "user", "content": user_content},
                ],
            )
        except Exception as exc:
            _log.warning("RecomputeHandler LLM call failed: %s", exc)
            return FallbackOutcome(action="halted")

        from opendaisugi.models import ActionPlan
        singleton_plan = ActionPlan(source="recompute", task=envelope.task, steps=[replacement])
        vr = verify(singleton_plan, envelope, z3_timeout_ms=self._z3_timeout_ms)
        if vr.ok:
            return FallbackOutcome(
                action="recomputed",
                replacement_step=replacement,
                replacement_result=vr,
            )
        _log.info("RecomputeHandler: replacement also failed verification")
        return FallbackOutcome(action="halted")

    @staticmethod
    def _build_prompt(
        step: ActionStep,
        result: VerificationResult,
        envelope: Envelope,
    ) -> str:
        parts = [
            f"Rejected step:\n{json.dumps(step.model_dump(mode='json'), indent=2)}",
            f"\nEnvelope permissions:\n{json.dumps(envelope.permissions.model_dump(mode='json'), indent=2)}",
        ]
        if envelope.fallback.include_refinement and result.violations:
            violation_strs = [f"- [{v.stage}] {v.message}" for v in result.violations]
            parts.append("\nViolations:\n" + "\n".join(violation_strs))
        parts.append("\nProduce a replacement step that accomplishes the same goal within these permissions.")
        return "\n".join(parts)
