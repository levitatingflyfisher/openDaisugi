"""Per-step model sizing — the cheapest model that can handle each step, budgeted.

``routing.estimate_difficulty`` is a per-*task* seed heuristic that feeds the
task-level ``RouteAdvisor`` but was never connected to actual per-step model
selection. This module makes it per-*step* and less blunt, then connects it to a
configurable model ladder and the live :class:`~opendaisugi.budget.BudgetTracker`:

1. **Per-step difficulty** — reuses the text heuristic for reasoning steps
   (``TaskStep.prompt``), adds a step-type base (mechanical shell/file/mcp steps
   are cheap; skill reuse is cheapest) and a dependency-fan-in bump (a step
   integrating many upstream outputs is harder to get right).
2. **Ladder** — an ordered cheap→strong list of model rungs; the sizer picks the
   cheapest rung *capable* of the difficulty (sizing).
3. **Budget** — if that rung can't be afforded from what remains, downgrade to
   the cheapest affordable rung (budgeting). If nothing fits, it's flagged
   ``affordable=False`` so the orchestrator can stop cleanly.
"""

from __future__ import annotations

from dataclasses import dataclass

from opendaisugi.routing import (
    _DEFAULT_CHEAP_MODEL,
    _DEFAULT_FRONTIER_MODEL,
    estimate_difficulty,
)

# Placeholder id for a locally-hosted Tier-1 model. Deployments override the
# ladder (or just this rung) with the concrete local model from ``local_setup``.
DEFAULT_LOCAL_MODEL = "openai/local-model"

# Step-type difficulty floors. Mechanical actions don't need a smart model;
# skill reuse is the cheapest thing there is; a reasoning task starts mid and is
# refined upward by its prompt text.
_STEP_TYPE_BASE: dict[str, float] = {
    "task": 0.3,
    "shell": 0.1,
    "file_read": 0.05,
    "file_write": 0.1,
    "network": 0.1,
    "skill": 0.0,
    "mcp": 0.1,
}


@dataclass(frozen=True)
class ModelRung:
    """One step on the model ladder: a model and the max difficulty it handles."""

    name: str
    model: str
    max_difficulty: float
    est_tokens: int


@dataclass(frozen=True)
class StepSizing:
    """The sizing decision for one step."""

    step_id: str
    difficulty: float
    tier: str
    model: str
    est_tokens: int
    downgraded: bool = False
    affordable: bool = True


class ModelLadder:
    """An ordered cheap→strong list of :class:`ModelRung`."""

    def __init__(self, rungs: list[ModelRung]) -> None:
        if not rungs:
            raise ValueError("ModelLadder needs at least one rung")
        self.rungs = list(rungs)

    def rung_for_difficulty(self, difficulty: float) -> ModelRung:
        """Cheapest rung capable of ``difficulty`` (strongest if none suffices)."""
        for rung in self.rungs:
            if difficulty <= rung.max_difficulty:
                return rung
        return self.rungs[-1]

    def rung_for_model(self, model: str) -> ModelRung | None:
        """The rung whose model id is ``model``, or None if it's off this ladder."""
        for rung in self.rungs:
            if rung.model == model:
                return rung
        return None

    def cheapest_affordable(self, remaining: float) -> ModelRung | None:
        """Cheapest rung whose estimate fits ``remaining`` tokens, or None."""
        for rung in self.rungs:
            if rung.est_tokens <= remaining:
                return rung
        return None


# Default 3-rung ladder: local (cheapest) → cheap cloud → frontier. Token
# estimates mirror accounting._ESTIMATED_TOKENS_PER_CALL's order of magnitude.
DEFAULT_LADDER = ModelLadder([
    ModelRung(name="local", model=DEFAULT_LOCAL_MODEL, max_difficulty=0.2, est_tokens=1200),
    ModelRung(name="cheap", model=_DEFAULT_CHEAP_MODEL, max_difficulty=0.5, est_tokens=2000),
    ModelRung(name="frontier", model=_DEFAULT_FRONTIER_MODEL, max_difficulty=1.0, est_tokens=4500),
])


def estimate_step_difficulty(step) -> float:
    """Per-step difficulty in [0, 1] — the connected, less-dumb heuristic.

    Deliberately legible (not a model). A journal-calibrated estimator can
    replace it without changing the interface, exactly as ``estimate_difficulty``
    documents.
    """
    step_type = getattr(step, "type", "")
    base = _STEP_TYPE_BASE.get(step_type, 0.3)
    if step_type == "task":
        base = max(base, estimate_difficulty(getattr(step, "prompt", "") or ""))
    fan_in = len(getattr(step, "depends_on", []) or [])
    base += min(fan_in * 0.05, 0.2)
    return min(base, 1.0)


def size_step(
    step,
    *,
    ladder: ModelLadder = DEFAULT_LADDER,
    budget: "object | None" = None,
    target_model: str | None = None,
) -> StepSizing:
    """Size one step: cheapest capable model, downgraded if the budget is tight.

    ``budget`` is any object exposing ``remaining() -> float`` (a
    :class:`~opendaisugi.budget.BudgetTracker`); pass the live tracker to gate on
    what remains *now*, or ``None`` for pure capability-based sizing.

    ``target_model`` honors an explicit choice (e.g. a step's ``preferred_model``)
    as the starting rung; budget downgrade still applies on top. Falls back to
    difficulty-based selection when the model isn't on the ladder.
    """
    difficulty = estimate_step_difficulty(step)
    chosen = (
        (ladder.rung_for_model(target_model) if target_model else None)
        or ladder.rung_for_difficulty(difficulty)
    )
    downgraded = False
    affordable = True
    if budget is not None:
        remaining = budget.remaining()
        if chosen.est_tokens > remaining:
            cheaper = ladder.cheapest_affordable(remaining)
            if cheaper is not None:
                chosen = cheaper
                downgraded = True
            else:
                # Nothing fits — keep the cheapest rung but flag it so the caller
                # can stop rather than silently overspend.
                cheapest = ladder.rungs[0]
                downgraded = chosen is not cheapest
                chosen = cheapest
                affordable = False
    return StepSizing(
        step_id=getattr(step, "id", "?"),
        difficulty=difficulty,
        tier=chosen.name,
        model=chosen.model,
        est_tokens=chosen.est_tokens,
        downgraded=downgraded,
        affordable=affordable,
    )


def size_plan(
    plan,
    *,
    ladder: ModelLadder = DEFAULT_LADDER,
    budget: "object | None" = None,
) -> list[StepSizing]:
    """Size every step in ``plan`` (static, up-front). One :class:`StepSizing` each.

    Note: static sizing sets each step's initial model. LIVE budget gating during
    the run happens in the orchestrator's budget-aware executor, which re-sizes
    per step against the tracker's *current* remaining budget.
    """
    return [size_step(s, ladder=ladder, budget=budget) for s in plan.steps]


__all__ = [
    "DEFAULT_LADDER",
    "DEFAULT_LOCAL_MODEL",
    "ModelLadder",
    "ModelRung",
    "StepSizing",
    "estimate_step_difficulty",
    "size_plan",
    "size_step",
]
