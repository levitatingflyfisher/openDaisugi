"""Pydantic data models for opendaisugi."""

from __future__ import annotations

import hashlib
import json
from typing import Annotated, Any, Literal
from uuid import uuid4

from pydantic import BaseModel, Field


def compute_evidence_hash(evidence: dict[str, Any]) -> str:
    """Content-addressed sha256 of an evidence dict.

    Canonical JSON (sorted keys, stable separators, str-fallback) so identical
    content hashes identically regardless of authoring order. Used by Receipt
    to content-address the evidence of a step's execution. v0.18.0+.
    """
    canonical = json.dumps(evidence, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()

# v0.13.0: names treated as shell interpreters for policy purposes. A command
# whose head matches one of these can carry its dangerous action in an argument
# (``sh -c "rm -rf /"``, ``find / -delete``, ``xargs rm``, ``python -c "..."``)
# that static verification cannot prove safe — the interpreter's semantics are
# outside the envelope algebra's scope (deferred to v0.14+ semantic recursion).
# Envelope.shell_interpreter_policy governs what the tool does when one of
# these appears in shell_allowlist.
SHELL_INTERPRETERS: frozenset[str] = frozenset({
    "sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh",
    "xargs", "find",
    "python", "python3", "python2",
    "perl", "ruby", "node", "deno",
    "make", "awk", "gawk", "sed",
    "eval", "exec", "source",
    "env",  # env VAR=val CMD — indirect invocation
})


class Permission(BaseModel):
    """What actions are permitted for a verified plan."""

    file_read: list[str] = Field(default_factory=list, description="Glob patterns of readable paths")
    file_write: list[str] = Field(default_factory=list, description="Glob patterns of writable paths")
    network: bool = False
    network_hosts: list[str] = Field(default_factory=list, description="If non-empty, restrict NetworkStep URLs to these hosts. Empty list = any host (when network=True).")
    shell: bool = False
    shell_allowlist: list[str] = Field(default_factory=list, description="Allowed shell commands when shell=True")
    # v0.32: MCP tool allowlist for MCPStep. Entries are ``server/tool`` (glob-able,
    # e.g. ``github/*``). Deny-by-default: an empty list admits NO MCP tool, so an
    # MCPStep only verifies against an envelope that explicitly names its tool.
    mcp_allowlist: list[str] = Field(
        default_factory=list,
        description="Allowed MCP tools as 'server/tool' (glob-able) for MCPStep. Empty = none permitted.",
    )
    max_execution_time_s: int = 30
    max_output_size_mb: int = 10

    # v0.8.0: Robotics permissions. All optional — default preserves non-robot envelopes.
    workspace_bounds: tuple[tuple[float, float, float], tuple[float, float, float]] | None = Field(
        default=None,
        description="(xyz_min, xyz_max) AABB constraining end-effector position in world frame.",
    )
    obstacles: list[tuple[tuple[float, float, float], tuple[float, float, float]]] = Field(
        default_factory=list,
        description="List of (xyz_min, xyz_max) AABBs the trajectory must not penetrate.",
    )
    velocity_limit: float | None = Field(
        default=None,
        description="Per-joint maximum velocity in rad/s. None = no velocity constraint.",
    )
    joint_limits: dict[str, tuple[float, float]] = Field(
        default_factory=dict,
        description="joint_name -> (min_rad, max_rad). Empty dict = defer to MJCF-declared limits.",
    )
    torque_limit: float | None = Field(
        default=None,
        description="Per-joint torque bound in Nm. Verified at simulator rollout time, not by Z3.",
    )


class Invariant(BaseModel):
    """A property that must hold throughout execution."""

    type: str  # e.g. "file_unchanged", "no_side_effects"
    target: str | None = None
    scope: str | None = None
    description: str
    expr: Any | None = None
    enforce: bool = True


class Postcondition(BaseModel):
    """A property that must hold on plan output."""

    type: str
    path: str | None = None
    expected: int | None = None
    min: int | None = None
    max: int | None = None
    description: str | None = None
    expr: Any | None = None
    enforce: bool = True


class FallbackStrategy(BaseModel):
    """What to do if verification fails at runtime (v0.1+ enforces this)."""

    strategy: str = "tier2_recompute"
    model: str = "anthropic/claude-sonnet-4-20250514"
    include_refinement: bool = True


class Envelope(BaseModel):
    """A checkable safety specification generated per-task.

    Constrains what actions an action-proposing system is allowed to propose
    for a given task. Verified against proposed action plans before execution.

    Note: `parent_envelope` and `tightening_only` are carried for forward-compat
    with v0.1 envelope inheritance. v0.0.1 does not enforce inheritance.
    `summary` is set when the caller passes `summarize=True` to
    `generate_envelope` (v0.1.2+); otherwise it remains None.
    """

    id: str = Field(default_factory=lambda: f"env_{uuid4().hex[:8]}")
    generated_by: str
    task: str
    permissions: Permission
    invariants: list[Invariant] = Field(default_factory=list)
    postconditions: list[Postcondition] = Field(default_factory=list)
    fallback: FallbackStrategy = Field(default_factory=FallbackStrategy)
    parent_envelope: str | None = None
    tightening_only: bool = True
    summary: str | None = Field(
        default=None,
        max_length=80,
        description="Optional one-line human-readable summary (v0.1.2+).",
    )
    cache_key: str | None = Field(
        default=None,
        description="Stamped by generate_envelope() at creation time (v0.2.1+). "
                    "None for hand-built envelopes.",
    )
    stakes: Literal["low", "medium", "high", "physical"] = Field(
        default="low",
        description=(
            "Stakes level (v0.9.0+); 'physical' locks out probabilistic primitives "
            "like llm_check so robotics envelopes rely on sound primitives only."
        ),
    )
    shell_interpreter_policy: Literal["surface", "strict", "allow"] = Field(
        default="surface",
        description=(
            "v0.13.0+: what to do when shell_allowlist contains a name in "
            "SHELL_INTERPRETERS (sh/bash/xargs/find/python/make/...). "
            "'surface' (default) flags the interpreter in "
            "SubsumptionResult.unverified_invariants so the caller knows "
            "static verification can't prove interpreter-argument safety. "
            "'strict' (outer only) causes subsumption to fail when inner's "
            "allowlist admits an interpreter — use for high-trust delegation "
            "where the caller refuses unverified interpreter paths. 'allow' "
            "suppresses surfacing — the user has considered the interpreter "
            "and accepts the residual risk."
        ),
    )


class StepBase(BaseModel):
    id: str
    depends_on: list[str] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(
        default_factory=dict,
        description="Open bag for agent-authored fields (email body, signature, etc.). v0.9.0+",
    )
    postcondition: "Postcondition | None" = Field(
        default=None,
        description=(
            "Optional per-step postcondition evaluated after execution. The "
            "supervisor feeds execution evidence to the check; the boolean "
            "result lands in the step's Receipt.verify_result. None means "
            "'execution-happened-is-enough' — the receipt is still written so "
            "the run-end integrity check sees it. v0.18.0+"
        ),
    )
    preferred_model: str | None = Field(
        default=None,
        description=(
            "Optional hint to the supervisor: when this step is delegated to "
            "an LLM-backed executor, prefer this model (e.g. 'haiku', "
            "'sonnet', 'opus'). Ignored by non-LLM executors. Honored by "
            "DelegatingExecutor; physical-stakes envelopes refuse delegation "
            "outright (see _check_delegation_safety in verify.py). v0.19+"
        ),
    )


STEP_TYPE_REGISTRY: "dict[str, type[StepBase]]" = {}


def step_type(cls=None, *, override: bool = False):
    """Register a step class so the verifier/supervisor/parser understand it.

    The class's ``type`` field default is the registry key. Enables agent-
    authored Pydantic step subclasses to participate in verification and
    execution without in-tree edits.

    Usage:
        @step_type                       # standard registration
        class DraftEmail(StepBase):
            type: Literal["draft_email"] = "draft_email"

        @step_type(override=True)        # explicit override of an existing key
        class CustomShell(StepBase):
            type: Literal["shell"] = "shell"

    Raises ValueError if the discriminator is already registered to a
    different class and ``override=True`` was not passed. This prevents
    a misbehaving or adversarial third-party kit from silently shadowing
    a built-in step type. Re-registering the same class is idempotent.
    """
    def _do_register(c):
        default = c.model_fields["type"].default
        existing = STEP_TYPE_REGISTRY.get(default)
        if existing is not None and existing is not c and not override:
            raise ValueError(
                f"step_type collision: '{default}' is already registered to "
                f"{existing.__module__}.{existing.__name__}; pass "
                f"@step_type(override=True) to replace it deliberately."
            )
        STEP_TYPE_REGISTRY[default] = c
        return c
    if cls is None:
        # Called with kwargs: @step_type(override=True)
        return _do_register
    # Called bare: @step_type
    return _do_register(cls)


def get_step_type_registry() -> "dict[str, type[StepBase]]":
    """Return a copy of the registered step-type mapping."""
    return dict(STEP_TYPE_REGISTRY)


def coerce_step(v):
    """Hand-dispatch a step-shaped input to the right ``StepBase`` subclass.

    Used by ``ActionPlan._dispatch_steps`` and ``RefinementRecord._dispatch_step``
    so agent-authored step types round-trip through JSON serialization without
    losing their concrete class. Already-instantiated ``StepBase`` subclasses
    pass through unchanged.
    """
    if v is None or isinstance(v, StepBase):
        return v
    if isinstance(v, dict) and "type" in v:
        subclass = STEP_TYPE_REGISTRY.get(v["type"])
        if subclass is not None:
            return subclass.model_validate(v)
    return v


@step_type
class ShellStep(StepBase):
    """A shell command step."""

    type: Literal["shell"] = "shell"
    command: str


@step_type
class FileReadStep(StepBase):
    """A file read step."""

    type: Literal["file_read"] = "file_read"
    path: str


@step_type
class FileWriteStep(StepBase):
    """A file write step."""

    type: Literal["file_write"] = "file_write"
    path: str
    content: str


@step_type
class NetworkStep(StepBase):
    """An HTTP network step."""

    type: Literal["network"] = "network"
    url: str
    method: Literal["GET"] = "GET"
    headers: dict[str, str] = Field(default_factory=dict)


@step_type
class JointMoveStep(StepBase):
    """Move specified joints to target positions. v0.8+ robotics."""

    type: Literal["joint_move"] = "joint_move"
    joint_targets: dict[str, float]
    duration_s: float = 1.0
    velocity_scale: float = Field(default=1.0, ge=0.0, le=1.0)


@step_type
class CartesianMoveStep(StepBase):
    """Move end-effector to a target pose in world frame. v0.8+ robotics."""

    type: Literal["cartesian_move"] = "cartesian_move"
    target_position: tuple[float, float, float]
    target_orientation: tuple[float, float, float, float] | None = None
    duration_s: float = 1.0
    velocity_scale: float = Field(default=1.0, ge=0.0, le=1.0)


@step_type
class GripperStep(StepBase):
    """Binary gripper open/close. v0.8+ robotics."""

    type: Literal["gripper"] = "gripper"
    action: Literal["open", "close"]
    hold_s: float = 0.2


@step_type
class SimulationResetStep(StepBase):
    """Reset the simulator to initial conditions. v0.8+ robotics."""

    type: Literal["sim_reset"] = "sim_reset"
    seed: int | None = None


@step_type
class VLAStep(StepBase):
    """A skill executed by a Vision-Language-Action policy. v0.26+

    Treats the VLA (Physical Intelligence π0/π0.5, an LeRobot policy, any
    visuomotor controller) as an opaque motor primitive. Individual actions
    inside the rollout aren't visible to the verifier — what's verified is
    the envelope around the rollout: workspace bounds (via the v0.8 Z3
    trajectory check on ``target_pose``), max action count, final-pose
    postconditions, and the v0.18 integrity guarantee that the VLA actually
    produced a receipt.

    Two reasons for the opaque framing:
    1. VLAs run at 30Hz; per-action verify would 50× the gate latency.
    2. The verifier doesn't have anything useful to say at the per-action
       level — the controller already enforces joint limits.

    The receipt's evidence carries the rollout summary (action count,
    final pose, contact summary) so post-hoc analysis (and the Gardener's
    selection signal) still operates on what the skill produced.
    """

    type: Literal["vla"] = "vla"
    task: str
    target_pose: tuple[float, float, float] | None = None
    max_actions: int = 50
    timeout_s: float = 5.0


@step_type
class TaskStep(StepBase):
    """A natural-language subtask delegated to an LLM. v0.32 orchestration.

    The workhorse of a decomposed prompt: the orchestrator sizes each TaskStep
    to a model (via ``preferred_model``) and runs it through an LLM-backed
    executor. It is a **pure-reasoning leaf** — it carries no capability field
    (no command/path/url), so it structurally cannot touch the shell, disk, or
    network. Its output is consumed only by the synthesizer; openDaisugi never
    splices a step's output into a downstream command string, which removes the
    prompt-injection → privileged-execution path by construction. Physical-stakes
    envelopes refuse delegation outright (verify._check_delegation_safety).
    """

    type: Literal["task"] = "task"
    prompt: str


@step_type
class SkillStep(StepBase):
    """Invoke a named skill / distilled pathway. v0.32 orchestration.

    The "repeated prompts via skills" half. ``skill_id`` names the skill; the
    orchestrator resolves it to a distilled pathway or contract at run time.
    ``contract_envelope`` is the skill's published envelope: when present, verify
    proves ``envelope_subsumes(current, contract_envelope)`` so the skill can
    only do what the caller's envelope already permits (delegation, proved). When
    absent the skill is opaque — strict mode rejects it, non-strict surfaces it.
    Typed as :class:`Envelope` (not :class:`~opendaisugi.contracts.Contract`) to
    avoid the contracts.py↔models.py import cycle; the envelope is the part
    subsumption reasons about.
    """

    type: Literal["skill"] = "skill"
    skill_id: str
    skill_input: dict[str, Any] = Field(default_factory=dict)
    contract_envelope: "Envelope | None" = None


@step_type
class MCPStep(StepBase):
    """Invoke a tool on an MCP server. v0.32 orchestration.

    ``server``/``tool`` name the call; verify checks ``f"{server}/{tool}"`` against
    ``Permission.mcp_allowlist`` (glob-able, deny-by-default). Execution is via a
    pluggable transport so no live MCP client is a hard dependency.
    """

    type: Literal["mcp"] = "mcp"
    server: str
    tool: str
    arguments: dict[str, Any] = Field(default_factory=dict)


# ActionStep is a type alias (discriminated union) — not a class.
# Pydantic dispatches to the right subclass based on the ``type`` field.
ActionStep = Annotated[
    ShellStep | FileReadStep | FileWriteStep | NetworkStep
    | JointMoveStep | CartesianMoveStep | GripperStep | SimulationResetStep
    | VLAStep | TaskStep | SkillStep | MCPStep,
    Field(discriminator="type"),
]


from pydantic import field_validator


class ActionPlan(BaseModel):
    """A proposed sequence of actions for a task, produced by any source."""

    id: str = Field(default_factory=lambda: f"plan_{uuid4().hex[:8]}")
    source: str  # "vanilla-llm", "hermes", "openclaw", "script", etc.
    task: str
    # v0.18: typed as ``list[Any]`` so the field validator can hand-dispatch
    # dicts via STEP_TYPE_REGISTRY and preserve subclass identity of already-
    # instantiated StepBase children. A plain ``list[StepBase]`` would let
    # Pydantic silently coerce subclasses back to the base class during round-
    # tripping, breaking ShellStep.command / FileReadStep.path access.
    steps: list[Any]

    @field_validator("steps", mode="before")
    @classmethod
    def _dispatch_steps(cls, v):
        """Accept already-instantiated ``StepBase`` subclasses OR dicts
        from JSON deserialization. Each item is dispatched via ``coerce_step``
        which looks up the concrete subclass in ``STEP_TYPE_REGISTRY``.
        """
        if not isinstance(v, list):
            return v
        return [coerce_step(item) for item in v]

    @field_validator("steps", mode="after")
    @classmethod
    def _check_all_steps_are_stepbase(cls, v):
        for s in v:
            if not isinstance(s, StepBase):
                raise ValueError(
                    f"ActionPlan.steps item {s!r} is not a StepBase subclass. "
                    f"If authoring a custom step type, register it with "
                    f"@opendaisugi.step_type."
                )
        return v


class Violation(BaseModel):
    """A single verification failure, attributed to a pipeline stage."""

    stage: str  # "permissions", "z3", "dag"
    message: str
    detail: dict = Field(default_factory=dict)
    suggested_remediation: str | None = Field(
        default=None,
        description=(
            "When the verifier can see a clean decomposition that would pass, "
            "a ready-to-use plan-level fix string. Agents can read this from "
            "a rejected verification and emit the corrected plan. v0.18.0+."
        ),
    )


class VerificationResult(BaseModel):
    """Result of running the verification pipeline on a plan+envelope pair."""

    ok: bool
    violations: list[Violation] = Field(default_factory=list)
    warnings: list[str] = Field(default_factory=list)
    envelope_id: str
    plan_id: str
    duration_ms: float


class Receipt(BaseModel):
    """Evidence that a step executed, produced by the supervisor after each step.

    Domain-agnostic: ``evidence`` is a free-form dict so shell, email, robotic
    joint-readings, and file-op receipts all use the same machinery. The Gardener
    reads ``verify_result`` across a run to build its selection signal.
    ``evidence_hash`` is content-addressed (see ``compute_evidence_hash``) so
    receipts are comparable across runs without leaking raw evidence. v0.18.0+.
    """
    step_id: str
    run_id: str
    timestamp: float
    evidence: dict[str, Any] = Field(default_factory=dict)
    evidence_hash: str
    verify_result: bool
    verify_details: str = ""
    # v0.19: when an LLM-backed executor (DelegatingExecutor) produced the
    # evidence, its model identity lands here. None for shell, file, network,
    # robotic-motion, or any non-LLM step. Gardener uses this to attribute
    # success/failure to specific models (e.g. "Haiku failure rate on
    # DraftEmail steps").
    model_id: str | None = None


class Trace(BaseModel):
    """A journal trace entry. v0.0.1 YAML files embed full envelope+plan bodies
    (see spec); this model represents the metadata view surfaced by the journal."""

    id: str
    created_at: str
    task: str
    plan_id: str
    envelope_id: str
    ok: bool
    duration_ms: float
    violations: list[Violation] = Field(default_factory=list)
