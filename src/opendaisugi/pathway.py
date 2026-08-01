"""v0.3.0 compiled-pathway data types.

A CompiledPathway is a distilled (envelope + plan template) pair produced
by the Distiller from clusters of successful journal traces. Pathways are
served to future matching tasks by the PathwayStore.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from opendaisugi.models import ActionPlan, Envelope


class PathwayParameter(BaseModel):
    """A typed data-hole in a distilled pathway's plan (ADR-0008, Phase B3).

    Names a field that varied across the cluster's successful plans while its
    *capability head* (a shell program, a path directory, a URL host) stayed
    fixed — so binding a new value can only change data, never the capability.
    ``observed`` records the values seen, as evidence for binding and display.
    """

    name: str
    # Topological position of the step in the plan — the authoritative locator,
    # stable across the id-renaming the distiller's generalization does. step_id
    # is a human-readable hint remapped to the stored template.
    step_index: int
    step_id: str
    field: str
    head: str
    observed: list[str] = Field(default_factory=list)


class CompiledPathway(BaseModel):
    """A distilled (envelope + plan template) pair."""

    id: str
    task_description: str
    task_embedding: list[float]
    # Defaults empty so rows distilled before v0.3.1 still load; fresh
    # distillations always stamp both.
    embedding_model: str = ""
    embedding_model_version: str = ""
    envelope: Envelope
    plan_template: ActionPlan
    source_trace_ids: list[str]
    version: int = 1
    hit_count: int = 0
    distilled_at: float
    # Gardener lifecycle fields. Defaults make existing pathway-store rows
    # load cleanly without migration. failure_count + activation_count
    # together give the Gardener a fitness ratio for selection / pruning.
    # Mutated by ``gardener.outcomes.record_run_outcome``.
    last_activation_at: float = 0.0
    failure_count: int = 0
    activation_count: int = 0
    # v0.24+: canonical step-type sequence derived from plan_template at
    # distillation time. Lets the pathway store do a fast structural
    # prefilter before falling back to embedding similarity. None on
    # v0.23 rows (no migration; the Distiller backfills on next tend).
    structure_signature: str | None = None
    # ADR-0008 (Phase B3): typed data-holes found by diffing the cluster's
    # plans. Empty = a frozen pathway (served verbatim, 0-token reuse). A
    # non-empty list makes this a *typed skill* whose holes are bound then
    # re-verified at reuse (Phase B4). Defaults empty so pre-B3 rows load frozen.
    parameters: list[PathwayParameter] = Field(default_factory=list)


class PathwayMatch(BaseModel):
    """Result of matching a task against compiled pathways."""

    pathway: CompiledPathway
    similarity: float = Field(..., ge=0.0, le=1.0)
    adapted_plan: ActionPlan | None = None
