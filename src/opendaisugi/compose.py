"""Compose distilled pathways as callable skills (ADR-0008, Phase B5).

A pathway can appear as a `SkillStep` inside a larger plan — "some free steps and
some LLM calls composed together." This turns each *frozen* distilled pathway into
a `SkillHandler` (``skill_id`` = the pathway id) that runs its plan through the
provided executors and returns the combined output.

Safety is by construction and needs no new machinery: a `SkillStep` referencing a
pathway carries that pathway's envelope as its ``contract_envelope``, and
``verify`` already proves ``envelope_subsumes(caller, contract)`` — so a composed
pathway can only ever do what the caller's envelope already permits. The pathway's
own plan was verified against its envelope at distill time, so its steps stay
within the caller's envelope by transitivity.

Frozen pathways only: a *typed* pathway would need its holes bound (and re-verified)
per invocation, which the composition seam does not yet do — a further extension.
"""

from __future__ import annotations

from opendaisugi.dag import topological_order
from opendaisugi.models import Envelope, SkillStep
from opendaisugi.orchestration_executors import SkillHandler
from opendaisugi.pathway import CompiledPathway


def pathway_skill_handler(
    pathway: CompiledPathway,
    *,
    executors: dict,
    timeout_s: int = 30,
    max_output_bytes: int = 65_536,
) -> SkillHandler:
    """A SkillHandler that runs a frozen pathway's plan as one composed step."""

    def _handler(step: SkillStep) -> str:
        outputs: list[str] = []
        for s in topological_order(pathway.plan_template):
            executor = executors.get(s.type)
            if executor is None:
                raise RuntimeError(f"no executor for composed step type {s.type!r}")
            result = executor.run(s, timeout_s=timeout_s, max_output_bytes=max_output_bytes)
            outputs.append(f"[{s.id}·{s.type}] {result.stdout}")
        return "\n".join(outputs)

    return _handler


def pathway_skill_handlers_for(
    store, skill_ids: set[str], *, executors: dict
) -> dict[str, SkillHandler]:
    """Resolve handlers for only the pathway ids a plan actually references.

    O(referenced skills) single-row ``store.get`` lookups, not an O(all pathways)
    ``list_all`` on every run. Unknown ids and typed pathways are skipped (they
    fall through to the SkillExecutor's "no handler" path or aren't composable yet).
    """
    handlers: dict[str, SkillHandler] = {}
    for skill_id in skill_ids:
        pathway = store.get(skill_id)
        if pathway is None or pathway.parameters:
            continue
        handlers[skill_id] = pathway_skill_handler(pathway, executors=executors)
    return handlers


def pathway_contract_envelopes(store) -> dict[str, Envelope]:
    """The published contract (envelope) for each frozen pathway, so a composing
    `SkillStep` can carry the right ``contract_envelope`` for the subsumption proof."""
    return {p.id: p.envelope for p in store.list_all() if not p.parameters}


__all__ = [
    "pathway_contract_envelopes",
    "pathway_skill_handler",
    "pathway_skill_handlers_for",
]
