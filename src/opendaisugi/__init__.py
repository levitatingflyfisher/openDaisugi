"""OpenDaisugi: runtime assurance library for agent actions.

Public API surface for v0.0.1: data models, the sync verify() function,
async envelope generation, and the Daisugi facade class. The journal
arrives in Week 3.
"""

from __future__ import annotations

import logging
import os
from pathlib import Path
from typing import Literal

# Silent-by-default library idiom: attach a NullHandler to the top-level
# logger so importing opendaisugi never emits records unless the host
# application explicitly configures logging. All submodules log to
# "opendaisugi.<subsys>" — hosts can route by prefix.
logging.getLogger("opendaisugi").addHandler(logging.NullHandler())
_log = logging.getLogger("opendaisugi.facade")

from opendaisugi.exceptions import (
    EnvelopeGenerationError,
    LowStakesNotConfigured,
    ModelLadderExhausted,
    OpenDaisugiError,
    StakesInheritanceWarning,
    TaskTooLongError,
    VerificationTimeout,
)
from opendaisugi.defaults import DEFAULT_LOW_STAKES_ENVELOPE
from opendaisugi.thinking import ThinkingBudget
from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD, PathwayStore
from opendaisugi.pathway_bundle import (
    PathwayBundle, pathway_to_bundle, bundle_to_pathway,
    UntrustedSignerError, InvalidSignatureError, UnsignedBundleError,
)
from opendaisugi.distiller import Distiller, TendReport
from opendaisugi.pathway import CompiledPathway, PathwayMatch
from opendaisugi.accounting import TierStats, classify_tier, tier_stats
from opendaisugi.gardener import (
    ABResult,
    GardenerConfig,
    GardenerReport,
    MergeConfig,
    MergeReport,
    PruneConfig,
    PruneReport,
    RegressionAlert,
    ab_test,
    merge,
    prune,
    regression_check,
    run_gardener,
)
from opendaisugi.tier1 import (
    ClaudeCodeTier1Provider,
    LiteLLMTier1Provider,
    OllamaTier1Provider,
    Tier1Provider,
)
from opendaisugi.lora import (
    DatasetStats,
    TrainingExample,
    emit_jsonl,
    iter_training_examples,
)
from opendaisugi import integrations
from opendaisugi.portability import (
    BUNDLE_SCHEMA_VERSION,
    ImportResult,
    PathwayImportError,
    export as export_pathway,
    import_pathway,
    parse_bundle,
)
from opendaisugi.inheritance import EnvelopeInheritanceError, verify_inheritance
from opendaisugi.models import (
    ActionPlan,
    ActionStep,
    CartesianMoveStep,
    VLAStep,
    Envelope,
    FallbackStrategy,
    FileReadStep,
    FileWriteStep,
    GripperStep,
    Invariant,
    JointMoveStep,
    NetworkStep,
    Permission,
    Postcondition,
    ShellStep,
    SimulationResetStep,
    Trace,
    VerificationResult,
    Violation,
)
from opendaisugi.config import Config, load_config, save_config
from opendaisugi.verify import verify
from opendaisugi.verify import verify as _verify
from opendaisugi.envelope import (
    CalibrationReport,
    ENVELOPE_PROMPT_VERSION,
    generate_envelope,
    run_calibration,
)
from opendaisugi.envelope import generate_envelope as _generate_envelope
from opendaisugi.journal import (
    Journal,
    JournalStats,
    ReplayResult,
    TraceRecord,
)
from opendaisugi.parsers import Episode, ParseResult
from opendaisugi.approval import ApprovalDecision, ApprovalStrategy
from opendaisugi.executor import (
    DryRunExecutor,
    ExecutorResult,
    FakeExecutor,
    StepExecutor,
    SubprocessExecutor,
)
from opendaisugi.run_session import RunSession, RunStatus, StepOutcome
from opendaisugi.supervisor import Supervisor
from opendaisugi.refinement import RefinementLog, RefinementRecord
from opendaisugi.fallback import (
    FallbackHandler,
    FallbackOutcome,
    HaltHandler,
    RecomputeHandler,
)

# v0.9.0 meta-DSL exports
from opendaisugi.predicate import Expression, LengthRange, parse_expression
from opendaisugi.aliases import Alias, AliasRegistry
from opendaisugi.system_aliases import load_system_aliases
from opendaisugi.stage2 import verify_completed_step

# v0.11.0: real Z3 compilation + skills-as-contracts
from opendaisugi.predicate_z3 import (
    CompiledPredicate,
    compile_to_z3,
    evaluate_predicate,
    verify_predicate_z3,
)
from opendaisugi.regex_to_z3 import UnsupportedRegexError
from opendaisugi.subsumption import (
    Counterexample,
    SubsumptionResult,
    envelope_subsumes,
)
from opendaisugi.contracts import (
    Contract,
    DelegationDecision,
    verify_delegation,
)
from opendaisugi.subagent import DelegationDenied, SafeSubagent

# v0.32.0: forward-looking orchestration layer
from opendaisugi.models import MCPStep, SkillStep, TaskStep
from opendaisugi.budget import BudgetExceeded, BudgetReport, BudgetTracker, StepCost
from opendaisugi.model_sizer import (
    DEFAULT_LADDER,
    ModelLadder,
    ModelRung,
    StepSizing,
    estimate_step_difficulty,
    size_plan,
    size_step,
)
from opendaisugi.decomposer import (
    DecomposedPlan,
    DecomposedStep,
    DecompositionError,
    decompose,
)
from opendaisugi.synthesizer import (
    StepOutput,
    SynthesisResult,
    collect_outputs,
    synthesize,
)
from opendaisugi.orchestration_executors import (
    MCPExecutor,
    MCPTransport,
    SkillExecutor,
    SkillHandler,
)
from opendaisugi.orchestrator import (
    BudgetAwareDelegatingExecutor,
    OrchestrationResult,
    Orchestrator,
)

# v0.33.0: verified swarm tasking (airspace deconfliction via envelope algebra)
from opendaisugi.swarm import (
    SwarmConflict,
    SwarmVerdict,
    aabb_disjoint,
    aabb_intersection,
    partition_airspace,
    partition_and_assign,
    verify_swarm_tasking,
)

# v0.15.0: real ed25519 signing (optional, requires [sign] extra)
try:
    from opendaisugi.signing import (
        SigningUnavailable,
        TrustedSignerRegistry,
        canonicalize_contract,
        default_registry_path,
        generate_keypair,
        sign_contract,
        verify_signature_raw,
    )
except ImportError:
    # cryptography not installed — only signing.py itself raises at use time.
    pass


class Daisugi:
    """Composition root for opendaisugi.

    Holds per-instance config (model, char budget, Z3 timeout, data dir)
    and dispatches to ``generate_envelope`` and ``verify``. Contains no
    logic of its own — it exists so callers can construct one object and
    reuse config rather than passing five kwargs to every call.
    """

    def __init__(
        self,
        *,
        model: str = "anthropic/claude-sonnet-4-20250514",
        max_task_chars: int = 4000,
        z3_timeout_ms: int = 500,
        data_dir: str | os.PathLike[str] | None = None,
        cache: bool | EnvelopeCache = True,
        pathway_store: bool | PathwayStore = True,
        pathway_threshold: float = DEFAULT_PATHWAY_THRESHOLD,
        low_stakes_envelope: Envelope | None = None,
        tier1: Tier1Provider | None = None,
        tend_after: int | None = None,
        strict: bool | None = None,
    ) -> None:
        self.model = model
        self.max_task_chars = max_task_chars
        self.z3_timeout_ms = z3_timeout_ms
        self._pathway_threshold = pathway_threshold
        # v0.28.3: facade-level strict override. None preserves verify()'s
        # stake-based default. Setting True opts low/medium-stakes envelopes
        # into strict mode through this facade — previously unreachable.
        self._strict = strict
        self.data_dir = Path(data_dir) if data_dir is not None else Path.home() / ".opendaisugi"
        self._tier1 = tier1
        if isinstance(cache, EnvelopeCache):
            self._cache: EnvelopeCache | None = cache
        elif cache is True:
            self._cache = EnvelopeCache(
                self.data_dir / "envelope_cache.db",
                prompt_version=ENVELOPE_PROMPT_VERSION,
            )
        else:
            self._cache = None
        if isinstance(pathway_store, PathwayStore):
            self._pathway_store: PathwayStore | None = pathway_store
        elif pathway_store is True:
            # Lazy — create on first access via the property.
            self._pathway_store = None
            self._pathway_store_auto = True
        else:
            self._pathway_store = None
            self._pathway_store_auto = False
        self._low_stakes_envelope = low_stakes_envelope
        self._tend_after = tend_after
        self._runs_since_tend: int = 0

    @classmethod
    def with_default_low_stakes(cls, **kwargs) -> "Daisugi":
        """Construct a Daisugi instance wired with the shipped low-stakes default.

        Equivalent to ``Daisugi(low_stakes_envelope=DEFAULT_LOW_STAKES_ENVELOPE, **kwargs)``.
        Separate classmethod exists so ``Daisugi()`` stays opt-out-of-permissive
        by default — callers must name this explicitly.
        """
        return cls(low_stakes_envelope=DEFAULT_LOW_STAKES_ENVELOPE, **kwargs)

    @property
    def cache(self) -> EnvelopeCache | None:
        """The EnvelopeCache this facade threads into generate_envelope calls.

        ``None`` when the facade was constructed with ``cache=False``. Exposed
        as read-only so callers can introspect (``d.cache.stats()``) or manage
        (``d.cache.clear()``) the shared cache without reaching into privates.
        """
        return self._cache

    async def generate_envelope(
        self,
        task: str,
        *,
        context: str | None = None,
        parent: Envelope | None = None,
        summarize: bool = False,
        stakes: Literal["low", "medium", "high"] = "medium",
        model: str | list[str] | None = None,
        thinking_budget: ThinkingBudget = "standard",
    ) -> Envelope:
        """Generate an envelope for ``task`` using this facade's config.

        When ``stakes='low'``, the facade's configured ``low_stakes_envelope``
        is used (set via ``Daisugi(low_stakes_envelope=...)`` or
        ``Daisugi.with_default_low_stakes()``). Callers can override on a
        per-call basis by passing ``stakes`` directly.

        Since v0.2.1, the facade's journal is threaded into generation so
        past refinement records for this task+model are injected as hints.
        """
        return await _generate_envelope(
            task=task,
            context=context,
            parent=parent,
            summarize=summarize,
            cache=self._cache,
            pathway_store=self.pathway_store,
            pathway_threshold=self._pathway_threshold,
            journal=self.journal,
            stakes=stakes,
            low_stakes_envelope=self._low_stakes_envelope,
            model=model if model is not None else self.model,
            thinking_budget=thinking_budget,
            tier1=self._tier1,
            max_task_chars=self.max_task_chars,
        )

    async def run(
        self,
        plan: ActionPlan,
        envelope: Envelope,
        *,
        aliases: "AliasRegistry | None" = None,
        strict: bool | None = None,
    ) -> "RunSession":
        """Execute ``plan`` against ``envelope`` via a managed Supervisor.

        Convenience wrapper over ``Supervisor.run`` that keeps the facade as the
        single object callers need to hold.  When ``tend_after=N`` was passed to
        the constructor, every N successful runs automatically trigger
        :meth:`tend` so the pathway store stays warm without manual scheduling.

        ``strict`` (v0.28.3) forces strict verification — overrides both the
        constructor-level ``strict`` and verify()'s stake-based default.
        """
        sup = Supervisor(
            journal=self.journal,
            z3_timeout_ms=self.z3_timeout_ms,
            aliases=aliases,
            strict=strict if strict is not None else self._strict,
        )
        session = await sup.run(plan, envelope)
        if session.status == RunStatus.SUCCEEDED and self._tend_after is not None:
            self._runs_since_tend += 1
            if self._runs_since_tend >= self._tend_after:
                self._runs_since_tend = 0
                # v0.28.4: distillation failure is non-fatal to the run.
                # Pre-v0.28.4 a tend() exception (LLM call, embedder
                # unavailable, sqlite locked) would make a successful
                # supervised run appear to fail at the user-facing call
                # site, despite the run being fully journaled.
                try:
                    await self.tend()
                except Exception as e:
                    _log.warning(
                        "auto-tend after run %s raised %s: %s — "
                        "run already succeeded and journaled, swallowing",
                        session.id, type(e).__name__, e,
                    )
        return session

    async def tend(self, **kwargs) -> "TendReport":
        """Run the Distiller against this facade's journal and pathway store.

        Keyword arguments are forwarded to :class:`Distiller` — e.g. ``min_traces=5``.
        Raises RuntimeError if the facade was constructed with ``pathway_store=False``.
        """
        if self.pathway_store is None:
            raise RuntimeError("Daisugi was constructed with pathway_store=False; cannot tend.")
        distiller = Distiller(
            journal=self.journal,
            pathway_store=self.pathway_store,
            model=self.model,
            **kwargs,
        )
        return await distiller.tend()

    async def orchestrate(
        self,
        prompt: str,
        *,
        envelope: Envelope | None = None,
        budget_tokens: int | None = None,
        stakes: Literal["low", "medium", "high"] = "medium",
        skill_handlers: "dict | None" = None,
        mcp_transport=None,
        ladder: "ModelLadder | None" = None,
        strict: bool | None = None,
        strict_budget: bool = False,
    ) -> "OrchestrationResult":
        """Run ``prompt`` end to end: decompose → size → execute → synthesize.

        The forward-looking counterpart to :meth:`tend`. When ``envelope`` is
        None one is generated for the prompt (the authorization boundary the
        decomposed plan must verify against) at the given ``stakes``. The
        orchestrator reuses this facade's pathway store (Tier-0 reuse for repeat
        prompts) and journal, and routes each executed step to the cheapest
        capable model under ``budget_tokens`` (None = unbudgeted; the decompose
        and synthesize calls are overhead, not drawn from it).
        """
        from opendaisugi.orchestrator import Orchestrator
        from opendaisugi.model_sizer import build_ladder

        if envelope is None:
            envelope = await self.generate_envelope(prompt, stakes=stakes)

        # Thread a configured local Tier-1 model into the ladder's local rung so
        # easy reasoning routes to it (token saving); its endpoint is passed to the
        # task executor so the call actually reaches the local server. Absent a
        # local model, the ladder has no local rung and easy tasks fall back to the
        # cheapest cloud model — never a placeholder.
        endpoint_overrides: dict = {}
        if ladder is not None:
            resolved_ladder = ladder
        else:
            local_model = getattr(self._tier1, "model", None)
            resolved_ladder = build_ladder(local_model)
            base_url = getattr(self._tier1, "base_url", None)
            if local_model and base_url:
                override = {"api_base": base_url}
                api_key = getattr(self._tier1, "api_key", None)
                if api_key:
                    override["api_key"] = api_key
                endpoint_overrides[local_model] = override

        orch = Orchestrator(
            ladder=resolved_ladder,
            skill_handlers=skill_handlers,
            mcp_transport=mcp_transport,
            pathway_store=self.pathway_store,
            journal=self.journal,
            decompose_model=self.model,
            z3_timeout_ms=self.z3_timeout_ms,
            pathway_threshold=self._pathway_threshold,
            endpoint_overrides=endpoint_overrides,
        )
        return await orch.orchestrate(
            prompt,
            envelope=envelope,
            budget_tokens=budget_tokens,
            strict=strict if strict is not None else self._strict,
            strict_budget=strict_budget,
        )

    async def find_pathway(
        self, task: str, *, threshold: float | None = None
    ) -> "PathwayMatch | None":
        """Check the pathway store for a matching compiled pathway.

        Returns None if pathway_store is disabled or no match is above the
        threshold. ``threshold`` defaults to this facade's ``pathway_threshold``
        (``Daisugi(pathway_threshold=...)``); pass it explicitly to override per
        call. The underlying ``PathwayStore.find`` is fully synchronous (SQLite +
        numpy + sentence-transformers), so we offload to a worker thread to
        avoid blocking the event loop when called from async code.
        """
        if self.pathway_store is None:
            return None
        import asyncio
        eff = threshold if threshold is not None else self._pathway_threshold
        return await asyncio.to_thread(
            lambda: self.pathway_store.find(task, threshold=eff)
        )

    async def adapt_plan(
        self,
        match: "PathwayMatch",
        task: str,
        *,
        model: str | None = None,
    ) -> ActionPlan:
        """Adapt a pathway's plan template to a specific task via LLM.

        Falls back to the unmodified template if the LLM call fails or the
        adapted plan doesn't verify against the pathway envelope.
        """
        from opendaisugi.distiller import adapt_plan as _adapt_plan

        return await _adapt_plan(
            match, task,
            model=model if model is not None else self.model,
            z3_timeout_ms=self.z3_timeout_ms,
        )

    def verify(
        self,
        plan: ActionPlan,
        envelope: Envelope,
        *,
        strict: bool | None = None,
        aliases: "AliasRegistry | None" = None,
    ) -> VerificationResult:
        """Verify ``plan`` against ``envelope`` using this facade's Z3 timeout.

        ``strict`` overrides the default stake-based resolution (v0.27.0).
        Precedence (v0.28.3): method kwarg > constructor ``strict=`` >
        stake-based default. Pre-v0.28.3 patch this method ignored the
        constructor strict, contradicting ``run``'s behavior.
        ``aliases`` supplies an :class:`AliasRegistry` so alias-referenced
        invariant expressions are resolved before evaluation (v0.27.0).
        """
        return _verify(
            plan,
            envelope,
            z3_timeout_ms=self.z3_timeout_ms,
            strict=strict if strict is not None else self._strict,
            aliases=aliases,
        )

    @property
    def pathway_store(self) -> PathwayStore | None:
        """Lazy PathwayStore rooted at ``data_dir / pathways.db``.

        Returns None if the facade was constructed with ``pathway_store=False``.
        Auto-constructs the SQLite file on first access when ``pathway_store=True``.
        """
        if self._pathway_store is not None:
            return self._pathway_store
        if getattr(self, "_pathway_store_auto", False):
            self._pathway_store = PathwayStore(self.data_dir / "pathways.db")
            return self._pathway_store
        return None

    @property
    def journal(self) -> Journal:
        """Lazy Journal instance rooted at ``self.data_dir``.

        Created on first access and cached. Avoids doing filesystem I/O
        in ``__init__`` — constructing a Daisugi should never create
        directories unless the caller actually uses the journal.
        """
        if not hasattr(self, "_journal"):
            self._journal = Journal(
                data_dir=self.data_dir,
                z3_timeout_ms=self.z3_timeout_ms,
            )
        return self._journal


def __getattr__(name: str):
    # Lazy: keep mujoco/numpy off the default import path.
    if name == "MuJoCoExecutor":
        from opendaisugi.executor_mujoco import MuJoCoExecutor
        return MuJoCoExecutor
    raise AttributeError(f"module 'opendaisugi' has no attribute {name!r}")


__version__ = "0.34.1"

__all__ = [
    "__version__",
    "integrations",
    # Runtime supervision (v0.1.0)
    "Supervisor",
    "RunSession",
    "RunStatus",
    "StepOutcome",
    "StepExecutor",
    "SubprocessExecutor",
    "DryRunExecutor",
    "FakeExecutor",
    "ExecutorResult",
    "ApprovalStrategy",
    "ApprovalDecision",
    "CalibrationReport",
    "Config",
    "Daisugi",
    "Journal",
    "JournalStats",
    "ReplayResult",
    "TraceRecord",
    "generate_envelope",
    "load_config",
    "run_calibration",
    "save_config",
    "verify",
    # Models
    "ActionPlan",
    "ActionStep",
    "Episode",
    "FileReadStep",
    "FileWriteStep",
    "NetworkStep",
    "ParseResult",
    "ShellStep",
    "Envelope",
    "FallbackStrategy",
    "Invariant",
    "Permission",
    "Postcondition",
    "Trace",
    "VerificationResult",
    "Violation",
    # Exceptions
    "OpenDaisugiError",
    "TaskTooLongError",
    "VerificationTimeout",
    "EnvelopeGenerationError",
    "EnvelopeInheritanceError",
    "LowStakesNotConfigured",
    # v0.1.3: Tiered routing + stakes policy
    "ModelLadderExhausted",
    "StakesInheritanceWarning",
    # Defaults (v0.1.3)
    "DEFAULT_LOW_STAKES_ENVELOPE",
    "ThinkingBudget",
    # Inheritance (v0.1.2)
    "verify_inheritance",
    # Envelope cache (v0.1.2)
    "EnvelopeCache",
    # v0.2.0: Simplex fallback + CEGAR refinement
    "RefinementRecord",
    "RefinementLog",
    "FallbackHandler",
    "FallbackOutcome",
    "HaltHandler",
    "RecomputeHandler",
    # v0.2.1: Refinement-aware envelope generation
    "make_cache_key",
    # v0.3.0: Distillation + compiled pathways
    "CompiledPathway",
    "PathwayMatch",
    "PathwayStore",
    "PathwayBundle",
    "pathway_to_bundle",
    "bundle_to_pathway",
    "UntrustedSignerError",
    "InvalidSignatureError",
    "UnsignedBundleError",
    "Distiller",
    "TendReport",
    # v0.4.0: Tier-1 pluggable local-model routing
    "Tier1Provider",
    "LiteLLMTier1Provider",
    "ClaudeCodeTier1Provider",
    "OllamaTier1Provider",
    # v0.4.0: Token-tier accounting
    "TierStats",
    "tier_stats",
    "classify_tier",
    # v0.4.0: Gardener
    "GardenerConfig",
    "GardenerReport",
    "run_gardener",
    "PruneConfig",
    "PruneReport",
    "prune",
    "MergeConfig",
    "MergeReport",
    "merge",
    "ABResult",
    "ab_test",
    "RegressionAlert",
    "regression_check",
    # v0.5.0: LoRA training-data pipeline
    "DatasetStats",
    "TrainingExample",
    "emit_jsonl",
    "iter_training_examples",
    # v0.7.0: Pathway portability (export/import)
    "BUNDLE_SCHEMA_VERSION",
    "ImportResult",
    "PathwayImportError",
    "export_pathway",
    "import_pathway",
    "parse_bundle",
    # v0.8.0: Robotics step types
    "CartesianMoveStep",
    "VLAStep",
    "GripperStep",
    "JointMoveStep",
    "SimulationResetStep",
    # v0.8.0: MuJoCo-backed executor (lazy — requires `robotics` extra)
    "MuJoCoExecutor",
    # v0.11.0: Real Z3 compilation + skills-as-contracts
    "CompiledPredicate",
    "compile_to_z3",
    "evaluate_predicate",
    "verify_predicate_z3",
    "UnsupportedRegexError",
    "Counterexample",
    "SubsumptionResult",
    "envelope_subsumes",
    "Contract",
    "DelegationDecision",
    "verify_delegation",
    "SafeSubagent",
    "DelegationDenied",
    # v0.15.0: length algebra + ed25519 signing
    "LengthRange",
    "SigningUnavailable",
    "TrustedSignerRegistry",
    "canonicalize_contract",
    "default_registry_path",
    "generate_keypair",
    "sign_contract",
    "verify_signature_raw",
    # v0.32.0: forward-looking orchestration layer
    "TaskStep",
    "SkillStep",
    "MCPStep",
    "BudgetTracker",
    "BudgetReport",
    "BudgetExceeded",
    "StepCost",
    "ModelLadder",
    "ModelRung",
    "StepSizing",
    "DEFAULT_LADDER",
    "estimate_step_difficulty",
    "size_plan",
    "size_step",
    "decompose",
    "DecomposedPlan",
    "DecomposedStep",
    "DecompositionError",
    "synthesize",
    "collect_outputs",
    "SynthesisResult",
    "StepOutput",
    "SkillExecutor",
    "MCPExecutor",
    "SkillHandler",
    "MCPTransport",
    "Orchestrator",
    "OrchestrationResult",
    "BudgetAwareDelegatingExecutor",
    # v0.33.0: verified swarm tasking
    "verify_swarm_tasking",
    "partition_and_assign",
    "partition_airspace",
    "aabb_disjoint",
    "aabb_intersection",
    "SwarmVerdict",
    "SwarmConflict",
]
