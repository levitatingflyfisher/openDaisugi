"""Runtime-supervision state types.

``RunSession`` is the mutable lifecycle object passed through ``Supervisor.run()``.
``StepOutcome`` is a frozen per-step record. ``RunStatus`` names the
transitions a session passes through. These types are intentionally small and
free of behavior — the supervisor owns the state machine.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Literal

from opendaisugi.models import VerificationResult


class RunStatus(str, Enum):
    PENDING = "pending"
    REJECTED = "rejected"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    ABORTED = "aborted"
    HALTED_BY_SIMPLEX = "halted_by_simplex"


@dataclass(frozen=True)
class StepOutcome:
    step_id: str
    status: Literal["succeeded", "failed", "skipped", "aborted", "rejected_halted", "rejected_recomputed"]
    approved_by: Literal["allowlist", "tty", "env", "callback", "denied"] | None
    rc: int | None
    stdout: str
    duration_ms: float
    started_at: str
    error: str | None


@dataclass
class RunSession:
    id: str
    envelope_id: str
    plan_id: str
    status: RunStatus
    verification: VerificationResult
    steps: list[StepOutcome] = field(default_factory=list)
    started_at: str = ""
    ended_at: str | None = None
    trace_id: str | None = None
    # v0.18: set by the run-end integrity check. None means "not checked"
    # (e.g. rejected-at-verify runs that never reached execution). True means
    # every step that was supposed to run produced a Receipt; False means at
    # least one expected Receipt is missing (silent step-skipping).
    integrity_passed: bool | None = None
    # v0.18: the id of the step where a halt-on-failure run stopped. None
    # for successful or rejected-at-verify runs. Used by the integrity
    # check to distinguish legitimate partial runs (receipts only up to the
    # failing step) from silent skips (gaps in the middle).
    failed_step_id: str | None = None
