"""v0.3.0 compiled-pathway data types.

A CompiledPathway is a distilled (envelope + plan template) pair produced
by the Distiller from clusters of successful journal traces. Pathways are
served to future matching tasks by the PathwayStore.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from opendaisugi.models import ActionPlan, Envelope


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


class PathwayMatch(BaseModel):
    """Result of matching a task against compiled pathways."""

    pathway: CompiledPathway
    similarity: float = Field(..., ge=0.0, le=1.0)
    adapted_plan: ActionPlan | None = None
