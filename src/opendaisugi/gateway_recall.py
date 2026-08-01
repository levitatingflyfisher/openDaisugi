"""daisugi_recall — assured reuse a harness opts into (ADR-0012 §2C).

The token-saving gateway's Phase 2 answers a recurring ask from a distilled
pathway instead of the frontier model, WITHOUT skipping verification. This
module is the bridge to the existing ADR-0008 reuse engine
(``CompiledPathway`` / ``PathwayStore.find`` / ``bind_parameters``), exposed
as a single call a harness makes BEFORE the model:

    match = pathway_store.find(task)          -> miss if no match
    plan  = frozen template | bind_parameters  -> the candidate plan
    verify(plan, caller_envelope)              -> miss on ANY failure
    hit   = {plan, provenance}

The load-bearing safety line is step 3: the reused plan is re-verified
against the CALLER's envelope, never trusted on the strength of the
pathway's own (possibly stale or broader) distillation envelope, and never
on the strength of ``bind_parameters``' internal re-verify alone — that
function may fall back to the frozen template on a binding failure, and
that fallback has not itself been checked against *this* caller's envelope.
On a miss or a verification failure this returns ``hit=False`` so the
harness falls open to the model (ADR-0004: the layer never drives the
harness; recall is a tool it opts into). An unverified plan is never
returned as a hit.

Importable without the ``[search]`` extra: ``PathwayStore.find`` itself
does the heavy embedder import lazily, and ``verify``/``bind_parameters``
never touch it.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.pathway_bind import bind_parameters
from opendaisugi.pathway_store import PathwayStore
from opendaisugi.verify import verify

# Mirrors the facade default (opendaisugi.Daisugi(model=...)) — kept as a
# local constant, same pattern as decomposer._DEFAULT_MODEL /
# synthesizer._DEFAULT_MODEL, so this module has a sane default when called
# outside the facade (e.g. directly from a test or a non-Daisugi harness).
_DEFAULT_MODEL = "anthropic/claude-sonnet-4-20250514"


@dataclass(frozen=True)
class RecallProvenance:
    """Travels with every hit so a caller never treats a stale reuse as fresh fact."""

    pathway_id: str
    similarity: float
    tier: str  # "frozen" | "typed"
    source_trace_count: int
    distilled_at: float
    hit_count: int


@dataclass(frozen=True)
class RecallResult:
    """Either a verified, reusable plan (+ provenance) or a miss."""

    hit: bool
    reason: str | None
    plan: ActionPlan | None
    provenance: RecallProvenance | None


async def recall(
    task: str,
    caller_envelope: Envelope,
    *,
    pathway_store: PathwayStore,
    model: str = _DEFAULT_MODEL,
    z3_timeout_ms: int = 500,
    client: Any | None = None,
    backend: str | None = None,
) -> RecallResult:
    """Find, bind (if typed), and re-verify a reusable plan for ``task``.

    Returns a miss (``hit=False``, ``plan=None``, ``provenance=None``) when
    no pathway matches, or when the candidate plan fails verification
    against ``caller_envelope`` — never an unverified plan as a hit.
    """
    match = pathway_store.find(task)
    if match is None:
        return RecallResult(hit=False, reason="no matching pathway", plan=None, provenance=None)

    frozen = not match.pathway.parameters
    if frozen:
        plan = match.pathway.plan_template.model_copy(deep=True)
    else:
        plan = await bind_parameters(
            match.pathway,
            task,
            envelope=caller_envelope,
            model=model,
            z3_timeout_ms=z3_timeout_ms,
            client=client,
            backend=backend,
        )

    # The safety line: re-verify against the CALLER's envelope, not the
    # pathway's stored one, and not merely trusting bind_parameters' own
    # internal re-verify (whose frozen-template fallback is unverified
    # against THIS caller). Any failure here is a miss, never a hit.
    result = verify(plan, caller_envelope, z3_timeout_ms=z3_timeout_ms)
    if not result.ok:
        return RecallResult(
            hit=False,
            reason="reuse failed verification against your envelope",
            plan=None,
            provenance=None,
        )

    provenance = RecallProvenance(
        pathway_id=match.pathway.id,
        similarity=match.similarity,
        tier="frozen" if frozen else "typed",
        source_trace_count=len(match.pathway.source_trace_ids),
        distilled_at=match.pathway.distilled_at,
        hit_count=match.pathway.hit_count,
    )
    return RecallResult(hit=True, reason=None, plan=plan, provenance=provenance)


__all__ = ["RecallProvenance", "RecallResult", "recall"]
