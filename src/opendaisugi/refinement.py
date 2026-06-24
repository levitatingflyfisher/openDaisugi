"""CEGAR refinement data types (v0.2.0).

Pure data — no behavior. ``RefinementRecord`` captures a single step-rejection
event (what failed, why, and what the system did about it).
``RefinementLog`` groups records by session for journal persistence.
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field, field_validator

from opendaisugi.models import (
    VerificationResult,
    Violation,
    StepBase,
    coerce_step,
)


class RefinementRecord(BaseModel):
    """Single rejection event + what the system did about it.

    ``step`` and ``recomputed_step`` accept any ``StepBase`` subclass —
    built-in (ShellStep, etc.) or agent-authored via ``@step_type`` —
    with validator-based dispatch via :func:`opendaisugi.models.coerce_step`
    that preserves subclass identity across JSON round-trips.
    """

    step: Any
    violations: list[Violation]
    z3_counterexample: dict[str, Any] | None
    envelope_id: str
    fallback_action: Literal["halted", "recomputed"]
    recomputed_step: Any = None
    recomputed_verification: VerificationResult | None = None
    timestamp: float
    cache_key: str | None = None  # envelope cache key, for refinement lookup

    @field_validator("step", "recomputed_step", mode="before")
    @classmethod
    def _dispatch_step(cls, v):
        return coerce_step(v)

    @field_validator("step", "recomputed_step", mode="after")
    @classmethod
    def _require_stepbase(cls, v):
        if v is not None and not isinstance(v, StepBase):
            raise ValueError(
                f"RefinementRecord step must be a StepBase subclass, "
                f"got {type(v).__name__}"
            )
        return v


class RefinementLog(BaseModel):
    """Ordered list of refinement records for a single run session."""

    session_id: str
    records: list[RefinementRecord] = Field(default_factory=list)
