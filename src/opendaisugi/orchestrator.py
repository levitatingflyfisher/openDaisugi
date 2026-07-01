"""Orchestrator — run one prompt end to end, safely and within budget (v0.32).

The forward-looking composition root. Where the Gardener/``tend()`` looks
*backward* (successful traces → distilled pathways), the Orchestrator looks
*forward*: a prompt becomes a verified plan becomes a result. It ties together
the five orchestration units and reuses the existing runtime-assurance spine:

    prompt
      → Tier-0 reuse         (a distilled pathway already covers it? reuse it)
      → decompose            (LLM authors a typed-step DAG; verified vs envelope)
      → size                 (per-step difficulty → cheapest capable model)
      → supervised execute   (Supervisor re-verifies each step; budget-aware
                              executor downgrades the model live when the token
                              budget is tight and records actual spend)
      → synthesize           (collect step outputs → final answer)

Every stage that touches an LLM takes an injectable client so the whole pipeline
is unit-testable without a live model. The plan is verified before it runs and
each step is re-verified at execution time — the orchestrator adds routing and
assembly on top of the assurance guarantees, it does not weaken them.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Any

from opendaisugi.approval import ApprovalStrategy, CallbackStrategy
from opendaisugi.budget import BudgetReport, BudgetTracker
from opendaisugi.decomposer import decompose
from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.executor import default_executors
from opendaisugi.model_sizer import DEFAULT_LADDER, ModelLadder, StepSizing, size_plan, size_step
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.orchestration_executors import MCPExecutor, MCPTransport, SkillExecutor, SkillHandler
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD
from opendaisugi.supervisor import Supervisor
from opendaisugi.synthesizer import SynthesisResult, synthesize

_log = logging.getLogger("opendaisugi.orchestrator")

_DEFAULT_DECOMPOSE_MODEL = "anthropic/claude-sonnet-4-20250514"
_DEFAULT_SYNTH_MODEL = "claude-haiku-4-5"


class BudgetAwareDelegatingExecutor(DelegatingExecutor):
    """A DelegatingExecutor whose model choice is gated by a live budget.

    This is where the token budget is enforced *during* a run. On each step:

    1. ``_resolve_model`` re-sizes the step against the tracker's *current*
       remaining budget (not a static up-front decision) and returns the chosen,
       possibly-downgraded, model — the routing gate.
    2. After the call, the spend is recorded into the tracker: the backend's
       actual ``usage`` when available, else the chosen rung's estimate, so the
       running total always advances and later steps see the pressure.
    """

    def __init__(
        self,
        *,
        tracker: BudgetTracker,
        ladder: ModelLadder = DEFAULT_LADDER,
        **kwargs: Any,
    ) -> None:
        super().__init__(**kwargs)
        self.tracker = tracker
        self.ladder = ladder
        self._last_sizing: StepSizing | None = None

    def _resolve_model(self, step):
        sizing = size_step(step, ladder=self.ladder, budget=self.tracker)
        self._last_sizing = sizing
        return sizing.model

    def run(self, step, *, timeout_s: int, max_output_bytes: int):
        result = super().run(step, timeout_s=timeout_s, max_output_bytes=max_output_bytes)
        tokens = self.last.tokens
        if tokens is None:
            tokens = self._last_sizing.est_tokens if self._last_sizing is not None else 0
        self.tracker.record(step_id=step.id, model=self.last.model or "?", tokens=tokens)
        return result


@dataclass
class OrchestrationResult:
    """Everything the orchestration produced, for the caller and the CLI."""

    prompt: str
    plan: ActionPlan
    session: Any               # RunSession
    final_answer: str
    sizings: list[StepSizing]
    budget: BudgetReport
    reused_pathway: bool
    used_llm_synthesis: bool

    @property
    def status(self) -> str:
        return self.session.status.value


class Orchestrator:
    """Runs a prompt end to end via decompose → size → execute → synthesize."""

    def __init__(
        self,
        *,
        ladder: ModelLadder = DEFAULT_LADDER,
        skill_handlers: dict[str, SkillHandler] | None = None,
        mcp_transport: MCPTransport | None = None,
        pathway_store: Any | None = None,
        journal: Any | None = None,
        decompose_model: str = _DEFAULT_DECOMPOSE_MODEL,
        synth_model: str = _DEFAULT_SYNTH_MODEL,
        backend: str | None = None,
        z3_timeout_ms: int = 500,
        pathway_threshold: float = DEFAULT_PATHWAY_THRESHOLD,
    ) -> None:
        self.ladder = ladder
        self.skill_handlers = dict(skill_handlers or {})
        self.mcp_transport = mcp_transport
        self.pathway_store = pathway_store
        self.journal = journal
        self.decompose_model = decompose_model
        self.synth_model = synth_model
        self.backend = backend
        self.z3_timeout_ms = z3_timeout_ms
        self.pathway_threshold = pathway_threshold

    async def _maybe_reuse(
        self, prompt: str, envelope: Envelope
    ) -> tuple[ActionPlan | None, Envelope, bool]:
        """Tier-0: reuse a distilled pathway that already covers the prompt (D6).

        Returns ``(plan, run_envelope, reused)``. On reuse, the plan is the
        pathway's template and the run envelope is the pathway's own (already
        verified) envelope; the Supervisor re-verifies it regardless.
        """
        if self.pathway_store is None:
            return None, envelope, False
        try:
            match = await asyncio.to_thread(
                self.pathway_store.find, prompt, threshold=self.pathway_threshold
            )
        except Exception as exc:  # store/embedder issues must not break orchestration
            _log.warning("orchestrate.pathway_lookup_failed", extra={"error": str(exc)})
            match = None
        if match is None:
            return None, envelope, False
        _log.info("orchestrate.reuse_pathway", extra={"pathway_id": match.pathway.id})
        # Deep-copy the template: sizing mutates preferred_model on steps, and the
        # store's template is shared/cached — mutating it would corrupt it.
        return match.pathway.plan_template.model_copy(deep=True), match.pathway.envelope, True

    async def orchestrate(
        self,
        prompt: str,
        *,
        envelope: Envelope,
        budget_tokens: int | None = None,
        strict: bool | None = None,
        decompose_client: Any | None = None,
        synth_client: Any | None = None,
        approval: ApprovalStrategy | None = None,
    ) -> OrchestrationResult:
        """Run ``prompt`` to a final answer within an optional token budget.

        ``envelope`` is the authorization boundary — the decomposed plan must
        verify against it and each step is re-verified before execution. Steps
        are auto-approved by default (the verified envelope is the authorization);
        pass ``approval=`` to gate execution differently.
        """
        tracker = BudgetTracker(total_tokens=budget_tokens)

        plan, run_envelope, reused = await self._maybe_reuse(prompt, envelope)
        if not reused:
            plan = await decompose(
                prompt,
                model=self.decompose_model,
                client=decompose_client,
                backend=self.backend,
                envelope=envelope,
                z3_timeout_ms=self.z3_timeout_ms,
            )
            run_envelope = envelope

        # Static, up-front sizing — records the plan's shape and sets the initial
        # model hint on delegated (task) steps. Live gating happens in the
        # budget-aware executor at run time.
        sizings = size_plan(plan, ladder=self.ladder, budget=tracker)
        _apply_preferred_models(plan, sizings)

        executors = default_executors()
        executors["task"] = BudgetAwareDelegatingExecutor(
            tracker=tracker, ladder=self.ladder, backend=self.backend,
        )
        executors["skill"] = SkillExecutor(handlers=self.skill_handlers)
        executors["mcp"] = MCPExecutor(transport=self.mcp_transport)

        supervisor = Supervisor(
            executors=executors,
            approval=approval or CallbackStrategy(lambda step, env: True),
            journal=self.journal,
            z3_timeout_ms=self.z3_timeout_ms,
            strict=strict,
        )
        session = await supervisor.run(plan, run_envelope)

        # If the budget is spent, synthesize deterministically rather than
        # spending tokens we don't have on assembly.
        use_llm = not tracker.exhausted()
        synth: SynthesisResult = await synthesize(
            prompt, session, plan,
            model=self.synth_model, client=synth_client,
            backend=self.backend, use_llm=use_llm,
        )

        return OrchestrationResult(
            prompt=prompt,
            plan=plan,
            session=session,
            final_answer=synth.answer,
            sizings=sizings,
            budget=tracker.report(),
            reused_pathway=reused,
            used_llm_synthesis=synth.used_llm,
        )


def _apply_preferred_models(plan: ActionPlan, sizings: list[StepSizing]) -> None:
    """Set ``preferred_model`` on task steps from their sizing.

    Only task steps are delegated to an LLM, so only they carry a model hint;
    other kinds run on their own (non-LLM) executors and ignore it. Keeping the
    hint off non-task steps also avoids tripping the physical-stakes delegation
    check on a future robotics orchestration.
    """
    by_id = {s.step_id: s for s in sizings}
    for step in plan.steps:
        if getattr(step, "type", None) != "task":
            continue
        sizing = by_id.get(step.id)
        if sizing is not None:
            step.preferred_model = sizing.model


__all__ = ["BudgetAwareDelegatingExecutor", "OrchestrationResult", "Orchestrator"]
