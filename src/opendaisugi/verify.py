"""Verification pipeline for opendaisugi.

The `verify()` function orchestrates three stages: permissions, Z3
constraints, and DAG structure. This module also hosts the permission-stage
helpers because they share Envelope/ActionPlan structure and are small
enough that a separate file adds friction without clarity.
"""

from __future__ import annotations

import fnmatch
import logging
import posixpath
import re
import time
from pathlib import PurePosixPath
from urllib.parse import urlparse

from opendaisugi._invariant_types import (
    RECOGNIZED_OPAQUE_TYPES,
    RECOGNIZED_STAGE2_POSTCONDITION_TYPES,
)
from opendaisugi.aliases import AliasRegistry, UnknownAliasError
from opendaisugi.dag import check_dag
from opendaisugi.exceptions import VerificationTimeout
from opendaisugi.interpreter_parse import parse_interpreter
from opendaisugi.models import ActionPlan, Envelope, Permission, VerificationResult, Violation
from opendaisugi.predicate import AliasRef, parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate
from opendaisugi.z3_checks import (
    check_envelope_self_consistency,
    check_plan_against_envelope,
    check_plan_invariants,
)

_log = logging.getLogger("opendaisugi.verify")

_SHELL_METACHAR_RE = re.compile(r"[;|&`<>\n\r]|\$\(")

_STRICT_STAKES = frozenset({"high", "physical"})


def resolve_strict(strict: bool | None, envelope: Envelope) -> bool:
    """Resolve effective strict mode. Explicit bool wins; None defaults to
    True for high/physical stakes, False otherwise (v0.27.0)."""
    if strict is not None:
        return strict
    return envelope.stakes in _STRICT_STAKES
_MAX_INTERPRETER_DEPTH = 4
_GLOB_CHARS_RE = re.compile(r"[*?\[]")
_ENV_ASSIGN_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")


def _extract_shell_head(stripped: str) -> str | None:
    """Pull the real command head out of a shell line.

    Returns None for lines that don't execute anything (blank, comment-only).
    Otherwise skips leading POSIX env-var assignments (``FOO=1 BAR=2 cmd …``)
    and returns the first real token. Bare env-only lines like ``FOO=1``
    return None (no command).

    **Safety invariant:** this classifier only decides *which string to use
    as the allowlist key*. It does not decide whether the raw command is
    safe. The metachar gate still runs on the unmodified command string,
    so ``A=$(rm -rf /) cmd`` is still rejected for the ``$(`` substring
    even if this function returns ``cmd``.
    """
    if not stripped:
        return None
    if stripped.startswith("#"):
        return None
    tokens = stripped.split()
    for tok in tokens:
        if _ENV_ASSIGN_RE.match(tok):
            continue
        return tok
    return None


def _build_decomposition_remediation(step_id: str, command: str) -> str | None:
    """Produce a suggested-remediation string for a metachar-gate rejection
    when the command decomposes cleanly into atomic pieces joined by
    &&/||/;. Returns None when the command carries $( or backticks (no
    safe decomposition). v0.18.0+.
    """
    # Lazy import to avoid circular dependency with parsers.
    from opendaisugi.parsers.claude_code import _split_compound_shell
    parts = _split_compound_shell(command)
    if len(parts) <= 1:
        return None
    if any(("$(" in p or "`" in p) for p in parts):
        return None
    lines = []
    for i, p in enumerate(parts):
        new_id = f"{step_id}_d{i}"
        depends = f", depends_on=['{step_id}_d{i-1}']" if i > 0 else ""
        lines.append(f"  ShellStep(id='{new_id}', command={p!r}{depends})")
    return "Decompose into sequential ShellSteps:\n" + "\n".join(lines)


def _head_allowed(head: str, allowlist: list[str]) -> bool:
    """True if ``head`` matches any entry in ``allowlist``.

    Literal entries (no ``*``/``?``/``[``) require exact string equality.
    Glob entries are matched segment-by-segment (both split on ``/``) with
    the segment counts required to be equal, which left-anchors the match:

    - ``.venv/bin/*`` matches ``.venv/bin/python`` and ``.venv/bin/pytest``
    - ``.venv/bin/*`` does NOT match ``.venv/bin/subdir/python`` (segment count)
    - ``.venv/bin/*`` does NOT match ``/abs/.venv/bin/python`` (segment count)
    - ``/usr/bin/*`` matches ``/usr/bin/python`` (leading empty segment aligns)

    ``PurePosixPath.match`` alone is NOT sufficient here: it is right-anchored,
    so a relative pattern like ``.venv/bin/*`` would happily match the suffix
    of an unrelated absolute path, defeating the point of the allowlist.
    """
    for pat in allowlist:
        if head == pat:
            return True
        if not _GLOB_CHARS_RE.search(pat):
            continue
        head_segs = head.split("/")
        pat_segs = pat.split("/")
        if len(head_segs) != len(pat_segs):
            continue
        if all(fnmatch.fnmatchcase(h, p) for h, p in zip(head_segs, pat_segs, strict=True)):
            return True
    return False


def _check_shell_command(
    command: str,
    step_id: str,
    perms: Permission,
    policy: str,
    depth: int = 0,
) -> list[Violation]:
    """Check a shell command string against the envelope, recursing into
    tractable interpreter payloads (``sh -c`` / ``xargs`` / ``find -exec``
    / ``env``).

    v0.14+ closes the interpreter-escape attack by parsing such payloads
    and verifying the embedded command against the same allowlist. Opaque
    interpreters (python/perl/ruby/node/awk/sed/make) cannot be parsed
    as shell — under ``strict`` policy they become violations; under
    ``surface``/``allow`` they pass (v0.13's subsumption-time surfacing
    is already in place for those).
    """
    violations: list[Violation] = []
    if depth > _MAX_INTERPRETER_DEPTH:
        return [Violation(
            stage="permissions",
            message=(
                f"Step '{step_id}' interpreter recursion exceeded "
                f"max depth {_MAX_INTERPRETER_DEPTH}"
            ),
            detail={"step": step_id, "command": command, "depth": depth},
        )]
    stripped = command.strip()
    if not stripped:
        return violations
    # INVARIANT: metachar gate runs on the raw command FIRST. Head-classifier
    # decisions below cannot soften it — env-prefix skipping, glob allowlist,
    # and comment detection all run after the raw command has been cleared
    # of ;, |, &, `, <, >, $(, newlines. This is the guarantee the tests
    # cover and the reason the supervisor can treat ShellStep as
    # "single-command" under the envelope. Redirection and newline coverage
    # added in v0.28.2 — see CHANGELOG.
    if _SHELL_METACHAR_RE.search(command):
        # v0.18: when the command composes with && / || / ; only (no $( or
        # backticks), offer a decomposed form as suggested_remediation so an
        # agent reading the rejection can emit the corrected plan directly.
        remediation = _build_decomposition_remediation(step_id, command)
        violations.append(Violation(
            stage="permissions",
            message=(
                f"Step '{step_id}' shell command contains dangerous "
                f"metacharacters (;, |, &, `, <, >, $(, newline)"
                + (f" (inside interpreter at depth {depth})" if depth else "")
            ),
            detail={"step": step_id, "command": command, "depth": depth},
            suggested_remediation=remediation,
        ))
        return violations
    head = _extract_shell_head(stripped)
    if head is None:
        # Comment-only line or bare env-assignment — nothing executes.
        return violations
    if not _head_allowed(head, perms.shell_allowlist):
        violations.append(Violation(
            stage="permissions",
            message=(
                f"Step '{step_id}' shell command '{head}' "
                f"not in allowlist {perms.shell_allowlist}"
                + (f" (inside interpreter at depth {depth})" if depth else "")
            ),
            detail={"step": step_id, "command_head": head, "depth": depth},
        ))
        return violations
    payload = parse_interpreter(command)
    if payload is None:
        return violations
    if payload.opaque:
        if policy == "strict":
            violations.append(Violation(
                stage="permissions",
                message=(
                    f"Step '{step_id}' invokes opaque interpreter "
                    f"'{payload.head}' whose payload cannot be recursively "
                    f"verified (strict shell_interpreter_policy rejects)"
                ),
                detail={"step": step_id, "interpreter": payload.head},
            ))
        return violations
    for inner in payload.inner_commands:
        violations.extend(_check_shell_command(
            inner, step_id, perms, policy, depth + 1,
        ))
    return violations


def _match_double_star(path: str, glob: str) -> bool:
    # Handle "prefix/**" — match anything under that prefix.
    if glob.endswith("/**"):
        prefix = glob[:-3]
        normalized = posixpath.normpath(path)
        return normalized.startswith(prefix + "/") or normalized == prefix
    return False


def _path_matches_any(path: str, globs: list[str]) -> bool:
    normalized = posixpath.normpath(path)
    p = PurePosixPath(normalized)
    return any(_match_glob(p, normalized, g) for g in globs)


def _match_glob(p: PurePosixPath, path: str, glob: str) -> bool:
    # 1. Trailing /** — our custom handler (prefix match)
    if _match_double_star(path, glob):
        return True
    # 2. PurePosixPath.match respects / boundaries and supports **
    try:
        return p.match(glob)
    except ValueError:
        return False


# Built-in step types that HAVE a verification story: a permission-stage case
# below, a dedicated Z3/robotics handler (joint_move/cartesian_move/gripper/
# sim_reset/vla via z3_checks + robot-capability subsumption), or gating
# elsewhere (task = contained leaf; skill = check_skill_delegations). Any type
# NOT here is a custom @step_type with no verification story — an unknown effect.
_KNOWN_STEP_TYPES: frozenset[str] = frozenset({
    "shell", "network", "file_read", "file_write", "mcp", "task", "skill",
    "joint_move", "cartesian_move", "gripper", "sim_reset", "vla",
})


def check_permissions(
    plan: ActionPlan, envelope: Envelope, *, strict: bool = False
) -> list[Violation]:
    """Check that every step in the plan is permitted by the envelope.

    Stage 1 of the verification pipeline. Uses simple set/string/glob
    operations — no Z3 required. Returns an empty list if all steps
    are permitted.

    Under ``strict`` mode (default-on for high/physical stakes) an UNKNOWN step
    type — a custom ``@step_type`` with no permission surface or handler — is
    rejected: an unverifiable effect cannot be admitted in a high-stakes plan.
    """
    violations: list[Violation] = []
    perms = envelope.permissions

    for step in plan.steps:
        match step.type:
            case "shell":
                if not perms.shell:
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=f"Step '{step.id}' requires shell but envelope forbids it",
                            detail={"step": step.id},
                        )
                    )
                    continue
                violations.extend(_check_shell_command(
                    step.command, step.id, perms, envelope.shell_interpreter_policy,
                ))
            case "network":
                if not perms.network:
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=f"Step '{step.id}' requires network but envelope forbids it",
                            detail={"step": step.id},
                        )
                    )
                    continue
                # Scheme allowlist — urllib honors file://, ftp://, data: too, so
                # a NetworkStep(url='file:///etc/passwd') would read a local file,
                # bypassing file_read permissions. Only http(s) is a network fetch.
                scheme = (urlparse(step.url).scheme or "").lower()
                if scheme not in ("http", "https"):
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=(
                                f"Step '{step.id}' network URL scheme '{scheme}' not allowed "
                                f"(only http/https); got {step.url!r}"
                            ),
                            detail={"step": step.id, "scheme": scheme, "url": step.url},
                        )
                    )
                    continue
                if perms.network_hosts:
                    host = urlparse(step.url).hostname or ""
                    if host not in {h.lower() for h in perms.network_hosts}:
                        violations.append(
                            Violation(
                                stage="permissions",
                                message=(
                                    f"Step '{step.id}' network host '{host}' not in "
                                    f"network_hosts allowlist {perms.network_hosts}"
                                ),
                                detail={"step": step.id, "host": host, "url": step.url},
                            )
                        )
            case "file_read":
                if not _path_matches_any(step.path, perms.file_read):
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=(
                                f"Step '{step.id}' file_read path '{step.path}' "
                                f"not permitted by file_read {perms.file_read}"
                            ),
                            detail={"step": step.id, "path": step.path},
                        )
                    )
            case "file_write":
                if not _path_matches_any(step.path, perms.file_write):
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=(
                                f"Step '{step.id}' file_write path '{step.path}' "
                                f"not permitted by file_write {perms.file_write}"
                            ),
                            detail={"step": step.id, "path": step.path},
                        )
                    )
            case "mcp":
                # v0.32: MCP tool call. Deny-by-default — the ``server/tool`` key
                # must match an entry in ``mcp_allowlist`` (literal or glob). An
                # empty allowlist admits nothing, so an MCPStep never verifies
                # vacuously against an envelope that didn't name its tool.
                key = f"{step.server}/{step.tool}"
                if not _head_allowed(key, perms.mcp_allowlist):
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=(
                                f"Step '{step.id}' MCP tool '{key}' "
                                f"not in mcp_allowlist {perms.mcp_allowlist}"
                            ),
                            detail={"step": step.id, "mcp_tool": key},
                        )
                    )
            # task/skill steps carry no permission-stage surface here: a TaskStep
            # is a contained pure-reasoning leaf (gated by _check_delegation_safety),
            # and a SkillStep's surface is the subsumption stage
            # (check_skill_delegations), not this per-step permission match.
            case _:
                # Unknown custom @step_type: no permission surface, no handler.
                # Fail closed under strict mode (an unverifiable effect can't run
                # in a high-stakes plan); pass under non-strict (the trust mode).
                if strict and step.type not in _KNOWN_STEP_TYPES:
                    violations.append(
                        Violation(
                            stage="permissions",
                            message=(
                                f"Step '{step.id}' has unverifiable step type "
                                f"'{step.type}' (no permission surface or handler); "
                                f"rejected under strict mode"
                            ),
                            detail={"step": step.id, "type": step.type},
                        )
                    )

    return violations


def check_skill_delegations(
    plan: ActionPlan,
    envelope: Envelope,
    *,
    strict: bool,
    timeout_ms: int = 2000,
    warnings_out: list[str] | None = None,
) -> list[Violation]:
    """Prove every SkillStep's contract is subsumed by the caller's envelope.

    A SkillStep names a skill and (optionally) carries the skill's published
    ``contract_envelope``. The delegation is safe iff the caller's envelope
    subsumes that contract — every action the skill can legally take is already
    admissible for the caller. Z3 answers this symbolically and, on failure,
    hands back the concrete step the skill could emit that violates policy.

    An opaque SkillStep (no ``contract_envelope``) declares no surface: under
    strict mode that is a hard rejection (a high-stakes caller refuses to
    delegate to something it cannot bound); under lenient mode it is surfaced as
    a warning and allowed. Mirrors the opaque-invariant policy in
    ``_check_predicate_item``.
    """
    skill_steps = [s for s in plan.steps if getattr(s, "type", None) == "skill"]
    if not skill_steps:
        return []
    # Local imports avoid the contracts.py↔models.py↔verify.py cycle. A SkillStep
    # IS a delegation, so its non-opaque check is exactly verify_delegation — reuse
    # it rather than re-deriving subsumption + counterexample formatting (and get
    # its signature / unverified-invariant handling for free).
    from opendaisugi.contracts import Contract, verify_delegation

    violations: list[Violation] = []
    for step in skill_steps:
        contract_env = getattr(step, "contract_envelope", None)
        if contract_env is None:
            msg = (
                f"Step '{step.id}' invokes opaque skill '{step.skill_id}' with no "
                f"contract_envelope; delegation cannot be proved subsumed"
            )
            if strict:
                violations.append(Violation(
                    stage="delegation",
                    message=msg + " (strict mode rejects)",
                    detail={"step": step.id, "skill_id": step.skill_id, "reason": "opaque_skill"},
                    suggested_remediation=(
                        "attach the skill's published contract_envelope so subsumption "
                        "can be proved, or lower stakes below high to allow it"
                    ),
                ))
            elif warnings_out is not None:
                warnings_out.append(msg + " (allowed under lenient mode)")
            continue
        contract = Contract(
            contract_id=f"skillstep:{step.id}",
            skill_id=step.skill_id,
            envelope=contract_env,
        )
        decision = verify_delegation(
            envelope, contract, strict=strict, timeout_ms=timeout_ms
        )
        if not decision.allowed:
            violations.append(Violation(
                stage="delegation",
                message=(
                    f"Step '{step.id}' skill '{step.skill_id}' delegation refused: "
                    f"{decision.reason}"
                ),
                detail={"step": step.id, "skill_id": step.skill_id, "reason": "not_subsumed"},
            ))
        elif decision.unverified_invariants and warnings_out is not None:
            warnings_out.append(
                f"Step '{step.id}' skill '{step.skill_id}' has unverified invariants: "
                f"{sorted(decision.unverified_invariants)}"
            )
    return violations


def verify(
    plan: ActionPlan,
    envelope: Envelope,
    *,
    z3_timeout_ms: int = 500,
    strict: bool | None = None,
    aliases: AliasRegistry | None = None,
) -> VerificationResult:
    """Run the full verification pipeline: permissions → Z3 → DAG.

    The first failing stage short-circuits the pipeline. Z3 timeouts are
    added as warnings (not violations) so verification is not blocked by
    transient Z3 budget exhaustion on complex envelopes.

    All checks are sync and pure — no I/O.
    """
    t0 = time.monotonic()
    effective_strict = resolve_strict(strict, envelope)
    violations: list[Violation] = []
    warnings: list[str] = []
    _log.debug(
        "verify.start",
        extra={
            "task": plan.task,
            "steps": len(plan.steps),
            "stakes": getattr(envelope, "stakes", None),
            "z3_timeout_ms": z3_timeout_ms,
        },
    )

    # Stage 0.5 (v0.19): delegation safety. Refuse to delegate physical-
    # stakes plans to LLM-backed executors. Steps that opt into delegation
    # (preferred_model is set) and whose envelope is stakes='physical' are
    # rejected before any other check — robotic motions cannot be delegated
    # to a model whose arguments static verification can't ground.
    violations.extend(_check_delegation_safety(plan, envelope))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Stage 1: permissions
    violations.extend(check_permissions(plan, envelope, strict=effective_strict))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Stage 1b (v0.32): skill-delegation subsumption. Each SkillStep's contract
    # envelope must be subsumed by the caller's — proved via Z3, opaque skills
    # gated by strict. Its own stage so check_permissions stays Z3-free.
    violations.extend(check_skill_delegations(
        plan, envelope, strict=effective_strict,
        timeout_ms=z3_timeout_ms, warnings_out=warnings,
    ))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Stage 2: Z3 self-consistency + plan-vs-envelope
    try:
        violations.extend(check_envelope_self_consistency(envelope, timeout_ms=z3_timeout_ms))
    except VerificationTimeout as e:
        warnings.append(str(e))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    try:
        violations.extend(check_plan_against_envelope(plan, envelope, timeout_ms=z3_timeout_ms))
    except VerificationTimeout as e:
        warnings.append(str(e))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Stage 2b: predicate-algebra invariants + postconditions
    violations.extend(_check_predicate_invariants(plan, envelope, strict=effective_strict, aliases=aliases, warnings_out=warnings))
    # Stage 2c: numerical trajectory checks (robotics Z3 — not expressible as predicates)
    try:
        violations.extend(check_plan_invariants(plan, envelope, timeout_ms=z3_timeout_ms))
    except VerificationTimeout as e:
        warnings.append(str(e))

    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Stage 3: DAG
    violations.extend(check_dag(plan))

    return _result(plan, envelope, violations, warnings, t0)


def verify_step(
    step,
    envelope: Envelope,
    *,
    z3_timeout_ms: int = 500,
) -> VerificationResult:
    """Lightweight per-step verification (v0.22+).

    Skips envelope-only Z3 checks (``check_envelope_self_consistency`` and
    ``check_plan_against_envelope``) — both are pure functions of the
    envelope, so re-running them per step on a singleton plan re-proves
    the same thing the whole-plan ``verify()`` already proved. Keeps the
    per-step gates that *do* depend on the step: delegation safety,
    permissions (the actual safety gate), robotics trajectory checks, and a
    (trivially-singleton) DAG.

    Predicate-algebra invariants/postconditions are plan-level and are NOT
    re-run here — and therefore neither are the v0.27.0 strict-mode checks
    (opaque-invariant rejection, vacuity, alias resolution), which ride that
    same path. They are the whole-plan ``verify()``'s responsibility; the
    caller MUST run ``verify(plan, envelope, strict=...)`` once up front to
    get strict enforcement. ``verify_step`` is the per-step hot-path gate, not
    a substitute for it.

    Drops per-step verify cost from ~3.5ms to ~0.1-0.3ms on a 20-step plan,
    a ~10-30× win on the supervisor's hot path.
    """
    t0 = time.monotonic()
    violations: list[Violation] = []
    warnings: list[str] = []

    plan = ActionPlan(source="per-step-verify", task=envelope.task, steps=[step])

    violations.extend(_check_delegation_safety(plan, envelope))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    violations.extend(check_permissions(plan, envelope, strict=resolve_strict(None, envelope)))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Skill-delegation subsumption is step-local (the SkillStep carries its own
    # contract), so it belongs on the per-step hot path too. strict is resolved
    # from the envelope here since verify_step takes no strict kwarg.
    violations.extend(check_skill_delegations(
        plan, envelope, strict=resolve_strict(None, envelope),
        timeout_ms=z3_timeout_ms, warnings_out=warnings,
    ))
    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # Predicate-algebra invariants (forall_steps / exists_step / quantified
    # claims over the whole plan) are deliberately NOT re-run here. They are
    # plan-level by definition — running them on a singleton plan would
    # report false rejections (e.g. an exists_step claim that holds on the
    # whole plan trivially fails on most singletons). The whole-plan verify
    # already validated them.

    # Robotics trajectory check still matters per-step — joint targets and
    # velocity ceilings are step-local data, not envelope-only.
    try:
        violations.extend(check_plan_invariants(plan, envelope, timeout_ms=z3_timeout_ms))
    except VerificationTimeout as e:
        warnings.append(str(e))

    if violations:
        return _result(plan, envelope, violations, warnings, t0)

    # DAG check on a singleton with depends_on=[] is trivial; included for
    # symmetry with the whole-plan path.
    violations.extend(check_dag(plan))

    return _result(plan, envelope, violations, warnings, t0)


def _robotics_backing_missing(type_name: str, perms: Permission) -> str | None:
    """Return the name of the missing backing Permission for a recognized robotics
    invariant, or None if its data is present (or it's not a robotics invariant).

    The z3 trajectory handler silently no-ops when its backing data is absent, so
    a declared robotics invariant with no backing is an unenforced (vacuous) guard.
    """
    # Only flag the genuinely-vacuous cases: an undefined workspace, or a
    # 'bounded' velocity claim with no bound. obstacles=[] legitimately means
    # 'nothing to avoid' (trivially satisfied) and joint_limits={} means 'defer
    # to the MJCF-declared limits' — neither is an unenforced guard.
    if type_name == "end_effector_in_workspace" and perms.workspace_bounds is None:
        return "workspace_bounds"
    if type_name == "velocity_bounded" and perms.velocity_limit is None:
        return "velocity_limit"
    return None


def _normalize_expr(raw):
    if raw is None:
        return None
    if isinstance(raw, dict):
        return parse_expression(raw)
    return raw


def _check_delegation_safety(
    plan: ActionPlan, envelope: Envelope
) -> list[Violation]:
    """v0.19 L4: refuse to delegate physical-stakes plans.

    A plan whose envelope is stakes='physical' AND any step has a
    preferred_model hint will be rejected. The rationale: LLM-produced
    arguments for joint trajectories or end-effector poses cannot be
    statically grounded; there is no safe way to delegate motion to a
    cheap model. Software stakes (low/medium/high) can delegate freely.
    """
    if getattr(envelope, "stakes", None) != "physical":
        return []
    violations: list[Violation] = []
    for step in plan.steps:
        if getattr(step, "preferred_model", None):
            violations.append(Violation(
                stage="permissions",
                message=(
                    f"Step '{step.id}' requests delegation to "
                    f"'{step.preferred_model}' but envelope stakes='physical'; "
                    f"physical-stakes plans cannot be LLM-delegated"
                ),
                detail={"step": step.id, "stakes": "physical",
                        "preferred_model": step.preferred_model},
            ))
    return violations


def _check_predicate_item(
    *,
    label: str,
    type_name: str,
    raw_expr,
    enforce: bool,
    description,
    plan: ActionPlan,
    envelope: Envelope,
    strict: bool,
    aliases: AliasRegistry | None,
    warnings_out: list[str] | None,
) -> list[Violation]:
    """Run one invariant or postcondition through the full predicate pipeline:
    opaque strict-reject -> alias resolution -> vacuity -> evaluation.

    ``label`` is ``"invariant"`` or ``"postcondition"``; it appears in messages
    and is the key under which the type name is stored in ``Violation.detail`` —
    so both paths produce structurally identical, parallel diagnostics. Sharing
    this body is what keeps the postcondition loop from drifting back into a
    silent fail-open (v0.27.0).
    """
    if not enforce:
        return []
    expr = _normalize_expr(raw_expr)

    # Opaque item: no predicate to evaluate. Recognized robotics types are
    # discharged by z3_checks.check_plan_invariants — but ONLY for invariants;
    # check_plan_invariants iterates envelope.invariants, never postconditions.
    # So the carve-out applies to invariants alone: an opaque postcondition (or
    # any other opaque type) declares a safety property nothing discharges, and
    # under strict mode that must be a loud rejection, not a silent pass.
    if expr is None:
        # A recognized robotics invariant is 'discharged elsewhere' by the z3
        # trajectory handler — but that handler NO-OPS when its backing
        # Permission data is absent (workspace_bounds/velocity_limit/...). So a
        # declared-but-unbacked robotics invariant is silently vacuous even at
        # physical stakes: reject it (fail-closed) rather than trust an unenforced
        # guard.
        backing_reason = _robotics_backing_missing(type_name, envelope.permissions) if label == "invariant" else None
        if backing_reason is not None:
            return [Violation(
                stage="predicate",
                message=(
                    f"invariant '{type_name}' is declared but its backing permission "
                    f"({backing_reason}) is absent; the check no-ops and the invariant "
                    f"is unenforced — add the bound or remove the invariant"
                ),
                detail={label: type_name, "reason": "robotics_invariant_unbacked"},
            )]
        discharged_elsewhere = (
            (label == "invariant" and type_name in RECOGNIZED_OPAQUE_TYPES)
            # v0.28.3: stage2 has concrete handlers for exit_code /
            # file_exists / file_size_range — those postcondition types
            # are discharged at the post-execution gate, not "opaque
            # unverifiable" at Stage 1.
            or (label == "postcondition" and type_name in RECOGNIZED_STAGE2_POSTCONDITION_TYPES)
        )
        if strict and not discharged_elsewhere:
            return [Violation(
                stage="predicate",
                message=f"{label} '{type_name}' declares a safety property with no "
                        f"verifiable expr; cannot be discharged under strict mode",
                detail={label: type_name, "reason": "opaque_unrecognized",
                        "suggested_remediation": "add an `expr` to make it verifiable, "
                                                 "or set enforce=False to keep it as documentation"},
            )]
        return []

    # Resolve alias references through the registry (if provided). An unresolved
    # AliasRef with no registry is a loud Violation, not a silent pass — that
    # would reintroduce the fail-open bug.
    if isinstance(expr, AliasRef):
        if aliases is None:
            return [Violation(
                stage="predicate",
                message=f"{label} '{type_name}' references unresolved alias '{expr.name}'; "
                        f"pass an AliasRegistry via aliases= to verify()",
                detail={label: type_name, "reason": "unresolved_alias", "alias": expr.name,
                        "suggested_remediation": "register the alias in an AliasRegistry and pass aliases= to verify()"},
            )]
        try:
            expr = aliases.resolve(expr)
        except UnknownAliasError as e:
            # The unresolved name may be a NESTED ref discovered during recursive
            # resolution, not the outer AliasRef — report the actual missing name.
            missing = e.args[0] if e.args else expr.name
            return [Violation(
                stage="predicate",
                message=f"{label} '{type_name}' references unresolved alias '{missing}'",
                detail={label: type_name, "reason": "unresolved_alias", "alias": missing,
                        "suggested_remediation": "register the alias in an AliasRegistry and pass aliases= to verify()"},
            )]
        except Exception as e:
            return [Violation(
                stage="predicate",
                message=f"{label} '{type_name}' alias resolution error: {e}",
                detail={label: type_name},
            )]
    elif aliases is not None:
        # Resolve any nested alias refs within the expression tree.
        try:
            expr = aliases.resolve(expr)
        except Exception:
            # If resolution fails on a non-AliasRef expr, fall through to evaluation as-is.
            pass

    # Vacuity check before evaluation.
    # - contradiction: always a hard error (DoS-class bug — envelope can never pass).
    # - tautology: hard error under strict; warning under non-strict.
    try:
        from opendaisugi.vacuity import check_vacuity
        vacuity_verdict = check_vacuity(expr)
    except Exception:
        # Unsupported expr, Z3 unavailable, timeout — skip and fall through to eval.
        vacuity_verdict = "non_trivial"

    if vacuity_verdict == "contradiction":
        return [Violation(
            stage="predicate",
            message=f"{label} '{type_name}' can never be satisfied (unsatisfiable); "
                    f"the envelope can never pass — fix the predicate",
            detail={label: type_name, "reason": "contradiction",
                    "suggested_remediation": f"this {label} is unsatisfiable (always false); "
                                             f"the envelope can never pass — fix the predicate"},
        )]
    if vacuity_verdict == "tautology":
        if strict:
            return [Violation(
                stage="predicate",
                message=f"{label} '{type_name}' is a tautology (constrains nothing); "
                        f"tighten the predicate or remove it",
                detail={label: type_name, "reason": "tautology",
                        "suggested_remediation": f"this {label} constrains nothing; "
                                                 f"tighten the predicate or remove it"},
            )]
        # Non-strict: surface as a result warning (not just a log line), then evaluate.
        _taut_msg = (f"{label} '{type_name}' is a tautology (constrains nothing); "
                     f"tighten the predicate or remove it")
        if warnings_out is not None:
            warnings_out.append(_taut_msg)
        else:
            # No warnings sink (direct caller / default) — keep at least a log floor
            # so the tautology signal is never wholly invisible.
            _log.warning(_taut_msg)

    try:
        ok = evaluate_predicate(expr, plan, envelope)
    except Exception as e:
        return [Violation(
            stage="predicate",
            message=f"{label} '{type_name}' evaluation error: {e}",
            detail={label: type_name},
        )]
    if not ok:
        return [Violation(
            stage="predicate",
            message=f"{label} '{type_name}' violated",
            detail={label: type_name, "description": description},
        )]
    return []


def _check_predicate_invariants(
    plan: ActionPlan, envelope: Envelope, *, strict: bool = False,
    aliases: AliasRegistry | None = None, warnings_out: list[str] | None = None,
) -> list[Violation]:
    """Evaluate predicate-algebra invariants and postconditions against the plan.

    Invariants and postconditions run through the SAME pipeline
    (``_check_predicate_item``) so an opaque/aliased/tautological postcondition
    is treated exactly as the equivalent invariant — no asymmetric fail-open.
    """
    violations: list[Violation] = []
    for inv in envelope.invariants:
        violations.extend(_check_predicate_item(
            label="invariant", type_name=inv.type, raw_expr=inv.expr,
            enforce=inv.enforce, description=inv.description,
            plan=plan, envelope=envelope, strict=strict, aliases=aliases,
            warnings_out=warnings_out,
        ))
    for pc in envelope.postconditions:
        violations.extend(_check_predicate_item(
            label="postcondition", type_name=pc.type, raw_expr=pc.expr,
            enforce=pc.enforce, description=pc.description,
            plan=plan, envelope=envelope, strict=strict, aliases=aliases,
            warnings_out=warnings_out,
        ))
    return violations


def _result(
    plan: ActionPlan,
    envelope: Envelope,
    violations: list[Violation],
    warnings: list[str],
    t0: float,
) -> VerificationResult:
    duration_ms = (time.monotonic() - t0) * 1000
    result = VerificationResult(
        ok=len(violations) == 0,
        violations=violations,
        warnings=warnings,
        envelope_id=envelope.id,
        plan_id=plan.id,
        duration_ms=duration_ms,
    )
    if result.ok:
        _log.info(
            "verify.pass",
            extra={
                "envelope_id": envelope.id,
                "plan_id": plan.id,
                "duration_ms": duration_ms,
                "warnings": len(warnings),
            },
        )
    else:
        _log.warning(
            "verify.fail",
            extra={
                "envelope_id": envelope.id,
                "plan_id": plan.id,
                "duration_ms": duration_ms,
                "violation_count": len(violations),
                "violation_stages": sorted({v.stage for v in violations}),
            },
        )
    return result
