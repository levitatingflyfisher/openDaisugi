"""Bind a typed pathway's holes for a new task, then re-verify (ADR-0008, B4).

A distilled *typed skill* carries parameters (holes) found by diffing its cluster.
At reuse we fill those holes for the specific task with one small, schema-
constrained LLM call, then run the bound plan through the SAME fail-closed verify
the orchestrator already applies. Safety rests on two independent checks, not on
the model's goodwill:

1. ``apply_bindings`` rejects any value that changes a capability head (a bound
   value may change data, never the program / directory / host — data-slots-only).
2. ``verify`` proves the concrete bound plan stays inside the caller's envelope.

Any failure — no client, LLM error, head change, or verify failure — falls back
to the frozen template, so a caller always gets a plan that satisfies the envelope.
"""

from __future__ import annotations

import logging
from typing import Any

from pydantic import BaseModel

from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_params import apply_bindings
from opendaisugi.verify import verify as _verify

_log = logging.getLogger("opendaisugi.pathway_bind")


class Bindings(BaseModel):
    """LLM response: a value for each hole, keyed by parameter name."""

    values: dict[str, str]


_SYSTEM = (
    "You fill typed data holes in a reusable plan for a new task. Change only the "
    "data — never the leading program, directory, or host. Return a value for "
    "every hole."
)


def _user_prompt(pathway: CompiledPathway, task: str) -> str:
    holes = "\n".join(
        f"- {p.name}: fills the `{p.field}` of a step whose head is `{p.head}`; "
        f"past values: {p.observed}"
        for p in pathway.parameters
    )
    return (
        f"Task: {task}\n\n"
        f"Holes to fill (keep each head `{'`, `'.join(sorted({p.head for p in pathway.parameters}))}` "
        f"exactly):\n{holes}\n\n"
        f"Return {{name: concrete value}} for every hole."
    )


async def bind_parameters(
    pathway: CompiledPathway,
    task: str,
    *,
    envelope: Envelope,
    model: str,
    z3_timeout_ms: int,
    client: Any | None = None,
    backend: str | None = None,
) -> ActionPlan:
    """Return a verified concrete plan for ``task``, or the frozen template.

    Frozen pathways (no parameters) return their template unchanged — the existing
    0-token reuse path. Typed pathways bind their holes and re-verify; on any
    failure they fall back to the template.
    """
    template = pathway.plan_template.model_copy(deep=True)
    if not pathway.parameters:
        return template

    if client is None:
        from opendaisugi.llm import get_instructor_client

        try:
            client = get_instructor_client(model=model, backend=backend)
        except Exception as exc:  # noqa: BLE001 — no client ⇒ frozen fallback
            _log.info("bind.no_client", extra={"error": str(exc)})
            return template

    try:
        resp = await client.chat.completions.create(
            model=model,
            response_model=Bindings,
            messages=[
                {"role": "system", "content": _SYSTEM},
                {"role": "user", "content": _user_prompt(pathway, task)},
            ],
            max_retries=2,
        )
    except Exception as exc:  # noqa: BLE001 — LLM failure ⇒ frozen fallback
        _log.warning("bind.llm_failed", extra={"error": str(exc)})
        return template

    # Apply + verify are also wrapped: "fall back on ANY failure" must hold even
    # if apply_bindings or _verify raises (e.g. a malformed response object or a
    # Z3 blow-up) — never let a bind concern crash orchestration.
    try:
        values = getattr(resp, "values", None)
        if not isinstance(values, dict):
            return template
        bound = apply_bindings(template, pathway.parameters, values)
        if bound is None:
            _log.info("bind.rejected")  # missing hole or capability-head change
            return template
        # Verify against the CALLER's envelope, never the pathway's own (possibly
        # broader) distillation envelope — the envelope is the authorization
        # ceiling (VISION invariant #3).
        result = _verify(bound, envelope, z3_timeout_ms=z3_timeout_ms)
    except Exception as exc:  # noqa: BLE001 — any failure ⇒ frozen fallback
        _log.warning("bind.apply_or_verify_failed", extra={"error": str(exc)})
        return template
    if not result.ok:
        _log.info("bind.verify_failed", extra={"violations": len(result.violations)})
        return template
    return bound


__all__ = ["Bindings", "bind_parameters"]
