"""Live token-budget accounting that gates orchestration during a run (v0.32).

``accounting.tier_stats`` reports token spend *after the fact* by scanning the
journal. The orchestrator needs the opposite: a running total consulted *before*
each step so routing can downgrade (or, in strict mode, stop) when the budget is
tight. ``BudgetTracker`` is that running total.

It is deliberately small and legible — token counts are rough estimates (see
``accounting`` and ``model_sizer``), not billing-grade. The value is the gate:
the difference between "we spent 40k tokens" discovered afterward and "this step
would blow the budget" decided in time to route around it.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field


class BudgetExceeded(Exception):
    """A strict-mode budget was exceeded by a recorded spend."""


@dataclass(frozen=True)
class StepCost:
    """One recorded spend: which step, which model, how many tokens."""

    step_id: str
    model: str
    tokens: int


@dataclass(frozen=True)
class BudgetReport:
    """Immutable snapshot of a tracker's state for an OrchestrationResult."""

    total: int | None
    spent: int
    remaining: float
    step_count: int
    by_model: dict[str, int]


@dataclass
class BudgetTracker:
    """Running token budget for one orchestration.

    ``total_tokens=None`` means unlimited (``remaining()`` is +inf, every spend
    is affordable). ``strict=True`` makes a spend that would push the running
    total past ``total_tokens`` raise :class:`BudgetExceeded` instead of silently
    overshooting — used when the caller wants a hard ceiling rather than
    best-effort downgrade.
    """

    total_tokens: int | None = None
    strict: bool = False
    _spent: int = field(default=0, init=False)
    _costs: list[StepCost] = field(default_factory=list, init=False)

    @property
    def total(self) -> int | None:
        return self.total_tokens

    def spent(self) -> int:
        return self._spent

    def remaining(self) -> float:
        if self.total_tokens is None:
            return math.inf
        return max(0, self.total_tokens - self._spent)

    def can_afford(self, estimate: int) -> bool:
        """True if ``estimate`` tokens fit in what remains."""
        return estimate <= self.remaining()

    def exhausted(self) -> bool:
        return self.remaining() <= 0 and self.total_tokens is not None

    def fraction_used(self) -> float:
        """Spent / total in [0, ∞); 0.0 when unlimited."""
        if not self.total_tokens:
            return 0.0
        return self._spent / self.total_tokens

    def record(self, *, step_id: str, model: str, tokens: int) -> None:
        """Add ``tokens`` to the running total, attributed to ``step_id``/``model``.

        Raises :class:`ValueError` on a negative count and, in strict mode,
        :class:`BudgetExceeded` when the spend crosses ``total_tokens``.
        """
        if tokens < 0:
            raise ValueError(f"token count must be non-negative, got {tokens}")
        if (
            self.strict
            and self.total_tokens is not None
            and self._spent + tokens > self.total_tokens
        ):
            raise BudgetExceeded(
                f"recording {tokens} tokens for step {step_id!r} would push spend to "
                f"{self._spent + tokens} > budget {self.total_tokens}"
            )
        self._spent += tokens
        self._costs.append(StepCost(step_id=step_id, model=model, tokens=tokens))

    def costs(self) -> list[StepCost]:
        return list(self._costs)

    def by_model(self) -> dict[str, int]:
        out: dict[str, int] = {}
        for c in self._costs:
            out[c.model] = out.get(c.model, 0) + c.tokens
        return out

    def report(self) -> BudgetReport:
        return BudgetReport(
            total=self.total_tokens,
            spent=self._spent,
            remaining=self.remaining(),
            step_count=len(self._costs),
            by_model=self.by_model(),
        )


__all__ = ["BudgetExceeded", "BudgetReport", "BudgetTracker", "StepCost"]
