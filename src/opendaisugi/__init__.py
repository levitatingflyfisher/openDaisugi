"""OpenDaisugi: runtime assurance library for agent actions.

Public API surface for v0.0.1: data models, the sync verify() function,
async envelope generation, and the Daisugi facade class. The journal
arrives in Week 3.

Every public name below is a PEP 562 lazy export (see ``__getattr__``): this
module does zero submodule imports at import time. ``import opendaisugi``
used to eagerly pull 54 submodules (~530 ms, including Z3 and networkx)
before any command ran; now it is a directory of names, and a name's owning
module loads only on first access (ADR-0017).
"""

from __future__ import annotations

import importlib
import logging
import sys as _sys
import types as _types
from pathlib import Path

# Silent-by-default library idiom: attach a NullHandler to the top-level
# logger so importing opendaisugi never emits records unless the host
# application explicitly configures logging. All submodules log to
# "opendaisugi.<subsys>" — hosts can route by prefix.
logging.getLogger("opendaisugi").addHandler(logging.NullHandler())

# The default on-disk home for envelope cache, journal, and pathway store when a
# caller passes no ``data_dir``. Kept a module attribute (not an inline literal)
# so the test suite can redirect it to a tmp dir via one autouse fixture — a bare
# ``Daisugi()`` in a test must never touch the user's real ``~/.opendaisugi``.
DEFAULT_DATA_DIR = Path.home() / ".opendaisugi"

_LAZY: dict[str, tuple[str, str | None]] = {
    "ABResult": ("opendaisugi.gardener", "ABResult"),
    "ActionPlan": ("opendaisugi.models", "ActionPlan"),
    "ActionStep": ("opendaisugi.models", "ActionStep"),
    "AgenticExecutor": ("opendaisugi.agentic_executor", "AgenticExecutor"),
    "AgenticStep": ("opendaisugi.models", "AgenticStep"),
    "Alias": ("opendaisugi.aliases", "Alias"),
    "AliasRegistry": ("opendaisugi.aliases", "AliasRegistry"),
    "AnswerStore": ("opendaisugi.gateway_answers", "AnswerStore"),
    "ApprovalDecision": ("opendaisugi.approval", "ApprovalDecision"),
    "ApprovalStrategy": ("opendaisugi.approval", "ApprovalStrategy"),
    "BUNDLE_SCHEMA_VERSION": ("opendaisugi.portability", "BUNDLE_SCHEMA_VERSION"),
    "BatchClassification": ("opendaisugi.batch", "BatchClassification"),
    "BatchDeclaration": ("opendaisugi.batch", "BatchDeclaration"),
    "BatchResult": ("opendaisugi.batch", "BatchResult"),
    "BudgetAwareDelegatingExecutor": ("opendaisugi.orchestrator", "BudgetAwareDelegatingExecutor"),
    "BudgetExceeded": ("opendaisugi.budget", "BudgetExceeded"),
    "BudgetReport": ("opendaisugi.budget", "BudgetReport"),
    "BudgetTracker": ("opendaisugi.budget", "BudgetTracker"),
    "CalibrationReport": ("opendaisugi.envelope", "CalibrationReport"),
    "CartesianMoveStep": ("opendaisugi.models", "CartesianMoveStep"),
    "ClaudeCodeTier1Provider": ("opendaisugi.tier1", "ClaudeCodeTier1Provider"),
    "CompiledPathway": ("opendaisugi.pathway", "CompiledPathway"),
    "CompiledPredicate": ("opendaisugi.predicate_z3", "CompiledPredicate"),
    "Config": ("opendaisugi.config", "Config"),
    "Contract": ("opendaisugi.contracts", "Contract"),
    "Counterexample": ("opendaisugi.subsumption", "Counterexample"),
    "DEFAULT_LADDER": ("opendaisugi.model_sizer", "DEFAULT_LADDER"),
    "DEFAULT_LOW_STAKES_ENVELOPE": ("opendaisugi.defaults", "DEFAULT_LOW_STAKES_ENVELOPE"),
    "DEFAULT_PATHWAY_THRESHOLD": ("opendaisugi.pathway_store", "DEFAULT_PATHWAY_THRESHOLD"),
    "Daisugi": ("opendaisugi.facade", "Daisugi"),
    "DatasetStats": ("opendaisugi.lora", "DatasetStats"),
    "DecomposedPlan": ("opendaisugi.decomposer", "DecomposedPlan"),
    "DecomposedStep": ("opendaisugi.decomposer", "DecomposedStep"),
    "DecompositionError": ("opendaisugi.decomposer", "DecompositionError"),
    "DelegationDecision": ("opendaisugi.contracts", "DelegationDecision"),
    "DelegationDenied": ("opendaisugi.subagent", "DelegationDenied"),
    "Distiller": ("opendaisugi.distiller", "Distiller"),
    "DryRunExecutor": ("opendaisugi.executor", "DryRunExecutor"),
    "ENVELOPE_PROMPT_VERSION": ("opendaisugi.envelope", "ENVELOPE_PROMPT_VERSION"),
    "Envelope": ("opendaisugi.models", "Envelope"),
    "EnvelopeCache": ("opendaisugi.envelope_cache", "EnvelopeCache"),
    "EnvelopeGenerationError": ("opendaisugi.exceptions", "EnvelopeGenerationError"),
    "EnvelopeInheritanceError": ("opendaisugi.inheritance", "EnvelopeInheritanceError"),
    "Episode": ("opendaisugi.parsers", "Episode"),
    "ExecutorResult": ("opendaisugi.executor", "ExecutorResult"),
    "Expression": ("opendaisugi.predicate", "Expression"),
    "FakeExecutor": ("opendaisugi.executor", "FakeExecutor"),
    "FallbackHandler": ("opendaisugi.fallback", "FallbackHandler"),
    "FallbackOutcome": ("opendaisugi.fallback", "FallbackOutcome"),
    "FallbackStrategy": ("opendaisugi.models", "FallbackStrategy"),
    "FileReadStep": ("opendaisugi.models", "FileReadStep"),
    "FileWriteStep": ("opendaisugi.models", "FileWriteStep"),
    "FootprintProof": ("opendaisugi.batch", "FootprintProof"),
    "GardenerConfig": ("opendaisugi.gardener", "GardenerConfig"),
    "GardenerReport": ("opendaisugi.gardener", "GardenerReport"),
    "Gateway": ("opendaisugi.gateway_pipeline", "Gateway"),
    "GatewayJournal": ("opendaisugi.gateway_journal", "GatewayJournal"),
    "GatewaySummary": ("opendaisugi.gateway_journal", "GatewaySummary"),
    "GatewayTurnRecord": ("opendaisugi.gateway_journal", "GatewayTurnRecord"),
    "GripperStep": ("opendaisugi.models", "GripperStep"),
    "HaltHandler": ("opendaisugi.fallback", "HaltHandler"),
    "ImportResult": ("opendaisugi.portability", "ImportResult"),
    "InvalidSignatureError": ("opendaisugi.pathway_bundle", "InvalidSignatureError"),
    "Invariant": ("opendaisugi.models", "Invariant"),
    "JointMoveStep": ("opendaisugi.models", "JointMoveStep"),
    "Journal": ("opendaisugi.journal", "Journal"),
    "JournalStats": ("opendaisugi.journal", "JournalStats"),
    "LengthRange": ("opendaisugi.predicate", "LengthRange"),
    "LiteLLMTier1Provider": ("opendaisugi.tier1", "LiteLLMTier1Provider"),
    "LowStakesNotConfigured": ("opendaisugi.exceptions", "LowStakesNotConfigured"),
    "MCPExecutor": ("opendaisugi.orchestration_executors", "MCPExecutor"),
    "MCPStep": ("opendaisugi.models", "MCPStep"),
    "MCPTransport": ("opendaisugi.orchestration_executors", "MCPTransport"),
    "MergeConfig": ("opendaisugi.gardener", "MergeConfig"),
    "MergeReport": ("opendaisugi.gardener", "MergeReport"),
    "ModelLadder": ("opendaisugi.model_sizer", "ModelLadder"),
    "ModelLadderExhausted": ("opendaisugi.exceptions", "ModelLadderExhausted"),
    "ModelRung": ("opendaisugi.model_sizer", "ModelRung"),
    "MuJoCoExecutor": ("opendaisugi.executor_mujoco", "MuJoCoExecutor"),
    "NetTokenLedger": ("opendaisugi.batch", "NetTokenLedger"),
    "NetworkStep": ("opendaisugi.models", "NetworkStep"),
    "OllamaTier1Provider": ("opendaisugi.tier1", "OllamaTier1Provider"),
    "OpenDaisugiError": ("opendaisugi.exceptions", "OpenDaisugiError"),
    "OrchestrationResult": ("opendaisugi.orchestrator", "OrchestrationResult"),
    "Orchestrator": ("opendaisugi.orchestrator", "Orchestrator"),
    "PairedResult": ("opendaisugi.benchmark", "PairedResult"),
    "ParseResult": ("opendaisugi.parsers", "ParseResult"),
    "PathState": ("opendaisugi.deeds", "PathState"),
    "PathwayBundle": ("opendaisugi.pathway_bundle", "PathwayBundle"),
    "PathwayImportError": ("opendaisugi.portability", "PathwayImportError"),
    "PathwayMatch": ("opendaisugi.pathway", "PathwayMatch"),
    "PathwayStore": ("opendaisugi.pathway_store", "PathwayStore"),
    "Permission": ("opendaisugi.models", "Permission"),
    "Postcondition": ("opendaisugi.models", "Postcondition"),
    "PreparedTurn": ("opendaisugi.gateway_pipeline", "PreparedTurn"),
    "PromotionResult": ("opendaisugi.strata", "PromotionResult"),
    "PruneConfig": ("opendaisugi.gardener", "PruneConfig"),
    "PruneReport": ("opendaisugi.gardener", "PruneReport"),
    "RecomputeHandler": ("opendaisugi.fallback", "RecomputeHandler"),
    "ReconstructedContext": ("opendaisugi.strata", "ReconstructedContext"),
    "RederivationLedger": ("opendaisugi.strata", "RederivationLedger"),
    "RefinementLog": ("opendaisugi.refinement", "RefinementLog"),
    "RefinementRecord": ("opendaisugi.refinement", "RefinementRecord"),
    "RegressionAlert": ("opendaisugi.gardener", "RegressionAlert"),
    "RepeatGroup": ("opendaisugi.gateway_journal", "RepeatGroup"),
    "ReplayResult": ("opendaisugi.journal", "ReplayResult"),
    "ReversalHandle": ("opendaisugi.models", "ReversalHandle"),
    "RollbackReport": ("opendaisugi.deeds", "RollbackReport"),
    "RouteDecision": ("opendaisugi.gateway", "RouteDecision"),
    "RunMetric": ("opendaisugi.benchmark", "RunMetric"),
    "RunSession": ("opendaisugi.run_session", "RunSession"),
    "RunStatus": ("opendaisugi.run_session", "RunStatus"),
    "SafeSubagent": ("opendaisugi.subagent", "SafeSubagent"),
    "ShellStep": ("opendaisugi.models", "ShellStep"),
    "SigningUnavailable": ("opendaisugi.signing", "SigningUnavailable"),
    "SimulationResetStep": ("opendaisugi.models", "SimulationResetStep"),
    "SkillExecutor": ("opendaisugi.orchestration_executors", "SkillExecutor"),
    "SkillHandler": ("opendaisugi.orchestration_executors", "SkillHandler"),
    "SkillStep": ("opendaisugi.models", "SkillStep"),
    "StakesInheritanceWarning": ("opendaisugi.exceptions", "StakesInheritanceWarning"),
    "StepCost": ("opendaisugi.budget", "StepCost"),
    "StepExecutor": ("opendaisugi.executor", "StepExecutor"),
    "StepOutcome": ("opendaisugi.run_session", "StepOutcome"),
    "StepOutput": ("opendaisugi.synthesizer", "StepOutput"),
    "StepSizing": ("opendaisugi.model_sizer", "StepSizing"),
    "StrataStore": ("opendaisugi.strata", "StrataStore"),
    "Stratum": ("opendaisugi.strata", "Stratum"),
    "SubprocessExecutor": ("opendaisugi.executor", "SubprocessExecutor"),
    "SubsumptionResult": ("opendaisugi.subsumption", "SubsumptionResult"),
    "Supervisor": ("opendaisugi.supervisor", "Supervisor"),
    "SwarmConflict": ("opendaisugi.swarm", "SwarmConflict"),
    "SwarmVerdict": ("opendaisugi.swarm", "SwarmVerdict"),
    "SynthesisResult": ("opendaisugi.synthesizer", "SynthesisResult"),
    "TaskStep": ("opendaisugi.models", "TaskStep"),
    "TaskTooLongError": ("opendaisugi.exceptions", "TaskTooLongError"),
    "TendReport": ("opendaisugi.distiller", "TendReport"),
    "ThinkingBudget": ("opendaisugi.thinking", "ThinkingBudget"),
    "Tier1Provider": ("opendaisugi.tier1", "Tier1Provider"),
    "TierStats": ("opendaisugi.accounting", "TierStats"),
    "Trace": ("opendaisugi.models", "Trace"),
    "TraceRecord": ("opendaisugi.journal", "TraceRecord"),
    "TrainingExample": ("opendaisugi.lora", "TrainingExample"),
    "TrustedSignerRegistry": ("opendaisugi.signing", "TrustedSignerRegistry"),
    "TurnCost": ("opendaisugi.gateway", "TurnCost"),
    "TurnSaving": ("opendaisugi.gateway", "TurnSaving"),
    "TwoLedgerReport": ("opendaisugi.batch", "TwoLedgerReport"),
    "UnsignedBundleError": ("opendaisugi.pathway_bundle", "UnsignedBundleError"),
    "UnsupportedRegexError": ("opendaisugi.regex_to_z3", "UnsupportedRegexError"),
    "UntrustedSignerError": ("opendaisugi.pathway_bundle", "UntrustedSignerError"),
    "VLAStep": ("opendaisugi.models", "VLAStep"),
    "VerificationResult": ("opendaisugi.models", "VerificationResult"),
    "VerificationTimeout": ("opendaisugi.exceptions", "VerificationTimeout"),
    "Violation": ("opendaisugi.models", "Violation"),
    "aabb_disjoint": ("opendaisugi.swarm", "aabb_disjoint"),
    "aabb_intersection": ("opendaisugi.swarm", "aabb_intersection"),
    "ab_test": ("opendaisugi.gardener", "ab_test"),
    "apply_reversal": ("opendaisugi.deeds", "apply_reversal"),
    "bundle_to_pathway": ("opendaisugi.pathway_bundle", "bundle_to_pathway"),
    "canonicalize_contract": ("opendaisugi.signing", "canonicalize_contract"),
    "classify_declaration": ("opendaisugi.batch", "classify_declaration"),
    "classify_tier": ("opendaisugi.accounting", "classify_tier"),
    "collect_outputs": ("opendaisugi.synthesizer", "collect_outputs"),
    "compile_to_z3": ("opendaisugi.predicate_z3", "compile_to_z3"),
    "decompose": ("opendaisugi.decomposer", "decompose"),
    "default_registry_path": ("opendaisugi.signing", "default_registry_path"),
    "emit_jsonl": ("opendaisugi.lora", "emit_jsonl"),
    "envelope_subsumes": ("opendaisugi.subsumption", "envelope_subsumes"),
    "estimate_step_difficulty": ("opendaisugi.model_sizer", "estimate_step_difficulty"),
    "evaluate_predicate": ("opendaisugi.predicate_z3", "evaluate_predicate"),
    "export_pathway": ("opendaisugi.portability", "export"),
    "generate_envelope": ("opendaisugi.envelope", "generate_envelope"),
    "generate_keypair": ("opendaisugi.signing", "generate_keypair"),
    "import_pathway": ("opendaisugi.portability", "import_pathway"),
    # NOT ("opendaisugi", "integrations") — that would make __getattr__ import
    # "opendaisugi" (itself) and re-enter __getattr__("integrations") forever.
    "integrations": ("opendaisugi.integrations", None),
    "is_batchable_type": ("opendaisugi.batch", "is_batchable_type"),
    "iter_training_examples": ("opendaisugi.lora", "iter_training_examples"),
    "load_config": ("opendaisugi.config", "load_config"),
    "load_system_aliases": ("opendaisugi.system_aliases", "load_system_aliases"),
    "make_cache_key": ("opendaisugi.envelope_cache", "make_cache_key"),
    "measure_turn": ("opendaisugi.gateway", "measure_turn"),
    "meets_stage4_bar": ("opendaisugi.benchmark", "meets_stage4_bar"),
    "merge": ("opendaisugi.gardener", "merge"),
    "parse_bundle": ("opendaisugi.portability", "parse_bundle"),
    "parse_expression": ("opendaisugi.predicate", "parse_expression"),
    "partition_airspace": ("opendaisugi.swarm", "partition_airspace"),
    "partition_and_assign": ("opendaisugi.swarm", "partition_and_assign"),
    "pathway_to_bundle": ("opendaisugi.pathway_bundle", "pathway_to_bundle"),
    "price_turn": ("opendaisugi.gateway", "price_turn"),
    "promote_constraint": ("opendaisugi.strata", "promote_constraint"),
    "prove_footprint": ("opendaisugi.batch", "prove_footprint"),
    "prune": ("opendaisugi.gardener", "prune"),
    "record_turn": ("opendaisugi.gateway_journal", "record_turn"),
    "regression_check": ("opendaisugi.gardener", "regression_check"),
    "rollback_result": ("opendaisugi.batch", "rollback_result"),
    "rollback_run": ("opendaisugi.deeds", "rollback_run"),
    "route_turn": ("opendaisugi.gateway", "route_turn"),
    "run_batch": ("opendaisugi.batch", "run_batch"),
    "run_calibration": ("opendaisugi.envelope", "run_calibration"),
    "run_gardener": ("opendaisugi.gardener", "run_gardener"),
    "run_paired_benchmark": ("opendaisugi.benchmark", "run_paired_benchmark"),
    "save_config": ("opendaisugi.config", "save_config"),
    "sign_contract": ("opendaisugi.signing", "sign_contract"),
    "size_plan": ("opendaisugi.model_sizer", "size_plan"),
    "size_step": ("opendaisugi.model_sizer", "size_step"),
    "summarize": ("opendaisugi.benchmark", "summarize"),
    "synthesize": ("opendaisugi.synthesizer", "synthesize"),
    "tasks_hash": ("opendaisugi.benchmark", "tasks_hash"),
    "tier_stats": ("opendaisugi.accounting", "tier_stats"),
    "touched_files": ("opendaisugi.deeds", "touched_files"),
    "two_ledger_report": ("opendaisugi.batch", "two_ledger_report"),
    "verify": ("opendaisugi.verify", "verify"),
    "verify_completed_step": ("opendaisugi.stage2", "verify_completed_step"),
    "verify_delegation": ("opendaisugi.contracts", "verify_delegation"),
    "verify_inheritance": ("opendaisugi.inheritance", "verify_inheritance"),
    "verify_predicate_z3": ("opendaisugi.predicate_z3", "verify_predicate_z3"),
    "verify_signature_raw": ("opendaisugi.signing", "verify_signature_raw"),
    "verify_swarm_tasking": ("opendaisugi.swarm", "verify_swarm_tasking"),
    "would_be_reversible": ("opendaisugi.batch", "would_be_reversible"),
}


def __getattr__(name: str):
    """PEP 562: import a public name on first use and cache it.

    ``import opendaisugi`` used to pull 54 submodules (~530 ms, incl. Z3 and
    networkx) before any command ran. Now the package is a directory of names.
    """
    target = _LAZY.get(name)
    if target is None:
        raise AttributeError(f"module 'opendaisugi' has no attribute {name!r}")
    module_name, attr = target
    module = importlib.import_module(module_name)
    value = module if attr is None else getattr(module, attr)
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(_LAZY))


# A name whose target module's basename equals the name itself, but whose
# _LAZY attr isn't the module ("verify": the function opendaisugi.verify.verify,
# not the submodule opendaisugi.verify). Python's import machinery
# unconditionally binds ``opendaisugi.verify = <the submodule>`` the FIRST
# time anything, anywhere in the process, does ``import opendaisugi.verify``
# or ``from opendaisugi.verify import ...`` — including our own facade.py —
# regardless of whether that goes through this file's __getattr__. That
# auto-bind writes straight into this module's __dict__, so it silently wins
# over (or preempts) the lazy function export and __getattr__ is never asked
# again, because normal attribute lookup now "succeeds" with the module.
# __getattribute__ on a swapped-in module subclass is the only hook that
# runs on every access (not just lookup failures), so it's the only place
# that can keep re-resolving these specific names to the right value.
_COLLIDING_NAMES = frozenset(
    name
    for name, (module_name, attr) in _LAZY.items()
    if attr is not None and module_name.rsplit(".", 1)[-1] == name
)


class _LazyModule(_types.ModuleType):
    def __getattribute__(self, name: str):
        if name in _COLLIDING_NAMES:
            module_name, attr = _LAZY[name]
            return getattr(importlib.import_module(module_name), attr)
        return super().__getattribute__(name)


_sys.modules[__name__].__class__ = _LazyModule


__version__ = "0.43.0"

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
    # Deed ledger — reversibility (v0.41.0, roadmap Stage 8)
    "ReversalHandle",
    "rollback_run",
    "touched_files",
    "apply_reversal",
    "RollbackReport",
    "PathState",
    # Within-instance batch compilation (v0.42.0, roadmap Stage 9)
    "BatchDeclaration",
    "BatchClassification",
    "FootprintProof",
    "BatchResult",
    "NetTokenLedger",
    "TwoLedgerReport",
    "is_batchable_type",
    "classify_declaration",
    "prove_footprint",
    "would_be_reversible",
    "two_ledger_report",
    "run_batch",
    "rollback_result",
    # Rationale-durability ledger (v0.43.0, roadmap Stage 10)
    "Stratum",
    "StrataStore",
    "ReconstructedContext",
    "RederivationLedger",
    "PromotionResult",
    "promote_constraint",
    # Token-saving gateway — turn-level routing + meter (MVP)
    "route_turn",
    "RouteDecision",
    "measure_turn",
    "TurnSaving",
    "TurnCost",
    "price_turn",
    # Token-saving gateway — turn journal + pipeline (tokens the constraint, dollars alongside)
    "Gateway",
    "PreparedTurn",
    "GatewayJournal",
    "GatewaySummary",
    "GatewayTurnRecord",
    "RepeatGroup",
    "record_turn",
    # Token-saving gateway — the answer store (ADR-0012 §2D)
    "AnswerStore",
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
    # v0.36.0: tool-using delegation inside the envelope (roadmap Stage 2)
    "AgenticStep",
    "AgenticExecutor",
    # v0.39.0: distillation-fidelity benchmark harness (roadmap Stage 4)
    "RunMetric",
    "PairedResult",
    "run_paired_benchmark",
    "summarize",
    "tasks_hash",
    "meets_stage4_bar",
    # v0.33.0: verified swarm tasking
    "verify_swarm_tasking",
    "partition_and_assign",
    "partition_airspace",
    "aabb_disjoint",
    "aabb_intersection",
    "SwarmVerdict",
    "SwarmConflict",
]
