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
from opendaisugi.executor import ExecutorResult
from opendaisugi.decomposer import _DEFAULT_MODEL as _DEFAULT_DECOMPOSE_MODEL, decompose
from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.executor import default_executors
from opendaisugi.model_sizer import DEFAULT_LADDER, ModelLadder, StepSizing, build_ladder, size_plan, size_step
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.orchestration_executors import MCPExecutor, MCPTransport, SkillExecutor, SkillHandler
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD
from opendaisugi.supervisor import Supervisor
from opendaisugi.synthesizer import _DEFAULT_MODEL as _DEFAULT_SYNTH_MODEL, SynthesisResult, synthesize

_log = logging.getLogger("opendaisugi.orchestrator")


def _task_step_prompt(step) -> str:
    """Prompt a TaskStep with its own subtask (free-form answer).

    Each task step answers its subtask independently; the synthesizer integrates
    all step outputs into the final answer. (Threading a step's upstream outputs
    into its prompt is a future enhancement — it needs the Supervisor to pass
    prior receipts; deferred to keep the injection boundary simple.)
    """
    prompt = getattr(step, "prompt", None)
    if prompt:
        return (
            f"{prompt}\n\n"
            "Complete this subtask and respond with a direct, complete answer."
        )
    return step.model_dump_json()


class BudgetAwareDelegatingExecutor(DelegatingExecutor):
    """A DelegatingExecutor whose model choice is gated by a live budget.

    This is where the token budget is enforced *during* a run. On each step:

    1. The step is sized against the tracker's *current* remaining budget (not a
       static up-front decision), honoring an explicit ``preferred_model`` as the
       starting rung and downgrading it when the budget is tight — the routing gate.
    2. If the tracker is in strict-budget mode and even the cheapest rung is
       unaffordable, the step fails *without* an LLM call — a clean stop rather
       than a silent overspend.
    3. Otherwise the call runs and its spend is recorded: the backend's actual
       ``usage`` when available, else the chosen rung's estimate, so the running
       total advances and later steps see the pressure.

    Each step's realized :class:`StepSizing` is appended to ``live_sizings`` so the
    orchestrator can report what actually ran (including downgrades), not the
    pre-run estimate.
    """

    def __init__(
        self,
        *,
        tracker: BudgetTracker,
        ladder: ModelLadder = DEFAULT_LADDER,
        prompt_template=None,
        json_mode: bool = False,
        **kwargs: Any,
    ) -> None:
        # A TaskStep is a natural-language subtask: prompt with the subtask text
        # (free-form answer), not the DelegatingExecutor evidence-JSON default.
        super().__init__(
            prompt_template=prompt_template or _task_step_prompt,
            json_mode=json_mode,
            **kwargs,
        )
        self.tracker = tracker
        self.ladder = ladder
        self._pending_sizing: StepSizing | None = None
        self.live_sizings: list[StepSizing] = []

    def _resolve_model(self, step):
        # Use the sizing computed once in run(); recompute only if called stand-alone.
        if self._pending_sizing is not None:
            return self._pending_sizing.model
        return size_step(
            step, ladder=self.ladder, budget=self.tracker,
            target_model=getattr(step, "preferred_model", None),
        ).model

    def run(self, step, *, timeout_s: int, max_output_bytes: int):
        sizing = size_step(
            step, ladder=self.ladder, budget=self.tracker,
            target_model=getattr(step, "preferred_model", None),
        )
        self._pending_sizing = sizing
        self.live_sizings.append(sizing)

        if self.tracker.strict and not sizing.affordable:
            return ExecutorResult(
                rc=1,
                stdout=f"budget exhausted: cannot afford step {step.id!r} at any tier",
                duration_ms=0.0,
                timed_out=False,
            )

        result = super().run(step, timeout_s=timeout_s, max_output_bytes=max_output_bytes)
        tokens = self.last.tokens if self.last.tokens is not None else sizing.est_tokens
        self.tracker.record(step_id=step.id, model=self.last.model or sizing.model, tokens=tokens)
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
        endpoint_overrides: "dict[str, dict[str, Any]] | None" = None,
        step_timeout_s: int = 180,
    ) -> None:
        self.ladder = ladder
        # LLM-backed task steps need far longer than the shell-oriented 30s
        # Supervisor default — a frontier model can take a minute-plus per step.
        self.step_timeout_s = step_timeout_s
        self.skill_handlers = dict(skill_handlers or {})
        self.mcp_transport = mcp_transport
        self.pathway_store = pathway_store
        self.journal = journal
        self.decompose_model = decompose_model
        self.synth_model = synth_model
        self.backend = backend
        self.z3_timeout_ms = z3_timeout_ms
        self.pathway_threshold = pathway_threshold
        # Per-model litellm kwargs (api_base/api_key) threaded into the task
        # executor so a local rung's model reaches its endpoint.
        self.endpoint_overrides = dict(endpoint_overrides or {})

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
        strict_budget: bool = False,
        decompose_client: Any | None = None,
        synth_client: Any | None = None,
        approval: ApprovalStrategy | None = None,
    ) -> OrchestrationResult:
        """Run ``prompt`` to a final answer within an optional token budget.

        ``envelope`` is the authorization boundary — the decomposed plan must
        verify against it and each step is re-verified before execution. Steps
        are auto-approved by default (the verified envelope is the authorization);
        pass ``approval=`` to gate execution differently.

        ``budget_tokens`` bounds the *mid-plan step routing*: each executed step is
        sized against what remains and downgraded when tight. The decompose and
        synthesize calls that bracket the run are orchestration overhead and are
        not drawn from this budget. ``strict_budget=True`` makes a step whose
        cheapest tier is still unaffordable fail cleanly instead of overspending
        (default is graceful downgrade). ``strict`` controls *verification*
        strictness, a separate axis.
        """
        tracker = BudgetTracker(total_tokens=budget_tokens, strict=strict_budget)

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

        # Static, budget-free sizing = the capability plan (difficulty → cheapest
        # capable model). It sets each task step's preferred_model. The live,
        # budget-gated routing happens in the executor and is reported back below.
        planned = size_plan(plan, ladder=self.ladder)
        _apply_preferred_models(plan, planned)

        task_executor = BudgetAwareDelegatingExecutor(
            tracker=tracker, ladder=self.ladder, backend=self.backend,
            endpoint_overrides=self.endpoint_overrides,
        )
        executors = default_executors()
        executors["task"] = task_executor
        executors["skill"] = SkillExecutor(handlers=self.skill_handlers)
        executors["mcp"] = MCPExecutor(transport=self.mcp_transport)

        supervisor = Supervisor(
            executors=executors,
            approval=approval or CallbackStrategy(lambda step, env: True),
            journal=self.journal,
            z3_timeout_ms=self.z3_timeout_ms,
            step_timeout_s=self.step_timeout_s,
            strict=strict,
        )
        session = await supervisor.run(plan, run_envelope)

        # Report what actually ran: overlay each task step's realized sizing
        # (incl. any budget downgrade) over the capability plan.
        realized = {s.step_id: s for s in task_executor.live_sizings}
        sizings = [realized.get(s.step_id, s) for s in planned]

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
