"""Envelope subsumption — the skills-as-contracts primitive (v0.11.0).

``envelope_subsumes(outer, inner)`` proves, via Z3, that every ActionStep the
``inner`` envelope admits is also admitted by ``outer``. When the proof
fails, Z3 returns a concrete counterexample step: the specific shell
command / file path / network URL the callee could legally emit that the
caller's envelope forbids.

The encoding:

    outer ⊨ inner
        ⟺ ∀ step. admit_inner(step) → admit_outer(step)
        ⟺ UNSAT of: admit_inner(step) ∧ ¬ admit_outer(step)

Permission allowlists become Z3 string predicates (literal equality,
``PrefixOf`` for shell command heads and path globs, ``SuffixOf`` for
suffix globs). Invariants with a predicate-algebra ``expr`` are compiled
via ``predicate_z3.compile_to_z3`` over the symbolic step and added to
the admission formula. Invariants without an ``expr`` are opaque — we
surface them as ``unverified_invariants`` rather than silently approving
the delegation.

This module is where Z3 earns its keep: the question isn't "is this
concrete plan OK" (Python handles that fine) but "is there *any* plan
the callee could produce that's unsafe for me." That is a genuine
symbolic entailment problem.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any

import z3

from opendaisugi._invariant_types import RECOGNIZED_OPAQUE_TYPES
from opendaisugi.exceptions import VerificationTimeout
from opendaisugi.models import (
    SHELL_INTERPRETERS,
    ActionStep,
    Envelope,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.predicate import ExistsStep, ForallSteps, parse_expression
from opendaisugi.predicate_z3 import _Scope, _compile_scalar


@dataclass
class Counterexample:
    """A concrete ActionStep the inner envelope allows but the outer forbids."""

    step: ActionStep
    outer_violation: str
    inner_justification: str
    raw_model: dict[str, Any] = field(default_factory=dict)


@dataclass
class SubsumptionResult:
    holds: bool
    counterexample: Counterexample | None
    unverified_invariants: list[str] = field(default_factory=list)
    reasons: list[str] = field(default_factory=list)
    duration_ms: float = 0.0


def _glob_to_z3(path_var: z3.ExprRef, glob: str) -> z3.BoolRef:
    """Encode a permission glob as a Z3 String predicate.

    Supported shapes (matches ``verify._path_matches_any``'s behaviors):
        ``/tmp/**``          → prefix match on "/tmp/" or equality with "/tmp"
        ``*.log``            → suffix match on ".log"
        ``/var/log/*.log``   → prefix "/var/log/" AND suffix ".log"
        ``/absolute/path``   → exact equality
        ``**``               → True (anything)
    """
    if glob == "**":
        return z3.BoolVal(True)
    if glob.endswith("/**"):
        prefix = glob[:-3]
        return z3.Or(
            z3.PrefixOf(z3.StringVal(prefix + "/"), path_var),
            path_var == z3.StringVal(prefix),
        )
    if "*" not in glob:
        return path_var == z3.StringVal(glob)
    # Handle at most one leading-star and one trailing-star glob like ``*.log``
    # or ``/var/log/*.log``. More exotic globs fall back to a free Bool.
    if glob.startswith("*") and "*" not in glob[1:]:
        suffix = glob[1:]
        return z3.SuffixOf(z3.StringVal(suffix), path_var)
    if "*" in glob and glob.count("*") == 1:
        prefix, suffix = glob.split("*", 1)
        return z3.And(
            z3.PrefixOf(z3.StringVal(prefix), path_var),
            z3.SuffixOf(z3.StringVal(suffix), path_var),
        )
    # Unsupported glob pattern — approximate as "true" to avoid false
    # subsumption proofs; the caller sees unverified glob shapes.
    return z3.BoolVal(True)


def _shell_head_in_allowlist(cmd_var: z3.ExprRef, allowlist: list[str]) -> z3.BoolRef:
    """``head(cmd) ∈ allowlist`` encoded without string tokenization.

    ``head(cmd)`` matches the allowlist iff either ``cmd == head`` (single-
    word command) or ``cmd`` starts with ``head + " "`` (command with args).
    Matches the Python splitting rule used in ``verify.check_permissions``.
    """
    if not allowlist:
        return z3.BoolVal(False)
    pieces: list[z3.BoolRef] = []
    for head in allowlist:
        pieces.append(cmd_var == z3.StringVal(head))
        pieces.append(z3.PrefixOf(z3.StringVal(head + " "), cmd_var))
    return z3.Or(*pieces)


def _encode_shell_admission(
    perms: Permission, cmd_var: z3.ExprRef
) -> z3.BoolRef:
    """Step is an admissible ShellStep under ``perms``."""
    if not perms.shell:
        return z3.BoolVal(False)
    head_ok = _shell_head_in_allowlist(cmd_var, perms.shell_allowlist)
    # Forbid dangerous metachars anywhere in the command string. Matches
    # verify._SHELL_METACHAR_RE for the concrete-check pathway. The
    # redirect characters and newline coverage was added in v0.28.2; the
    # $( command-substitution substring was added in v0.28.3 (verify has
    # always caught it, subsumption lagged). The two lists MUST stay in
    # sync — subsumption permissiveness here above the verify gate
    # produces unsound delegation proofs.
    metachars = [";", "|", "&", "`", "<", ">", "\n", "\r"]
    substrings = ["$("]
    no_meta = z3.And(
        *[z3.Not(z3.Contains(cmd_var, z3.StringVal(ch))) for ch in metachars],
        *[z3.Not(z3.Contains(cmd_var, z3.StringVal(s))) for s in substrings],
    )
    return z3.And(head_ok, no_meta)


def _patterns_subsume(
    inner_patterns: list[str], outer_patterns: list[str], *, label: str, timeout_ms: int
) -> str | None:
    """None if every value inner admits, outer admits too; else a reason string.

    For glob-list permissions (file_read/file_write/mcp_allowlist) with
    deny-by-default (empty list ⟹ admits nothing). Uses Z3 to search for a
    witness value inner admits but outer forbids. **Fail-closed**: an empty
    inner is trivially subsumed, but a solver ``unknown`` (timeout) or an
    exotic outer glob that can't be encoded soundly is treated as NOT proven
    → a violation, never an optimistic pass.
    """
    if not inner_patterns:
        return None  # inner admits nothing on this axis
    # An exotic outer glob would encode as True (permissive) — that would be a
    # fail-open. Refuse to rely on it: if any outer pattern isn't soundly
    # encodable, we can't prove containment → fail closed.
    if any(_glob_unsupported(g) for g in outer_patterns):
        return (f"{label}: outer declares a glob shape that cannot be soundly "
                f"encoded ({[g for g in outer_patterns if _glob_unsupported(g)]}); "
                f"cannot prove subsumption → denied")
    solver = z3.Solver()
    solver.set("timeout", timeout_ms)
    v = z3.String("v")
    inner_ok = z3.Or(*[_glob_to_z3(v, g) for g in inner_patterns])
    outer_ok = (z3.Or(*[_glob_to_z3(v, g) for g in outer_patterns])
                if outer_patterns else z3.BoolVal(False))
    solver.add(inner_ok, z3.Not(outer_ok))
    result = solver.check()
    if result == z3.sat:
        witness = solver.model()[v]
        return f"{label}: inner admits {witness} which outer forbids"
    if result != z3.unsat:  # unknown / timeout → can't prove → deny
        return f"{label}: could not prove subsumption (solver {result}) → denied"
    return None


def _glob_unsupported(glob: str) -> bool:
    """True if ``_glob_to_z3`` would fall back to its permissive True encoding."""
    if glob == "**" or glob.endswith("/**") or "*" not in glob:
        return False
    if glob.startswith("*") and "*" not in glob[1:]:
        return False
    return glob.count("*") != 1


def _network_scope_violation(outer: Permission, inner: Permission) -> str | None:
    """None if inner's network scope is within outer's, else a reason.

    Host matching is exact (mirrors ``verify.check_permissions``). Empty
    ``network_hosts`` with ``network=True`` means 'any host'.
    """
    if not inner.network:
        return None  # inner uses no network
    if not outer.network:
        return "network: inner uses network but outer forbids it"
    if not outer.network_hosts:
        return None  # outer admits any host → any inner scope is within it
    # outer is restricted to a host set
    if not inner.network_hosts:
        return ("network: inner admits any host but outer restricts to "
                f"{outer.network_hosts}")
    outer_set = {h.lower() for h in outer.network_hosts}
    extra = [h for h in inner.network_hosts if h.lower() not in outer_set]
    if extra:
        return f"network: inner hosts {extra} not in outer allowlist {outer.network_hosts}"
    return None


def _permission_scope_violation(
    outer: Permission, inner: Permission, *, timeout_ms: int
) -> str | None:
    """Fail-closed subsumption of file/network/mcp permissions (v0.33.3).

    ``envelope_subsumes`` historically encoded only shell + invariants, so an
    inner envelope permitting broader file_read/file_write/network/mcp scope than
    the outer was silently 'subsumed' — the core delegation-safety hole. This
    proves each of those axes is contained too.
    """
    for label, inner_p, outer_p in (
        ("file_read", inner.file_read, outer.file_read),
        ("file_write", inner.file_write, outer.file_write),
        ("mcp_allowlist", inner.mcp_allowlist, outer.mcp_allowlist),
    ):
        reason = _patterns_subsume(inner_p, outer_p, label=label, timeout_ms=timeout_ms)
        if reason is not None:
            return reason
    return _network_scope_violation(outer, inner)


def _compile_invariants(
    invariants: list[Invariant],
    scope: _Scope,
    soft: list[str],
    *,
    strict: bool = False,
) -> tuple[z3.BoolRef, list[str], list[str]]:
    """Conjunction of compiled invariants + list of opaque (no expr) ones + strict-blocking types.

    Returns (term, opaque_list, strict_blocking_list). Under strict mode, any
    opaque invariant whose type is not in RECOGNIZED_OPAQUE_TYPES is collected
    into strict_blocking_list rather than opaque — the caller uses this to
    short-circuit subsumption to holds=False (v0.27.0).
    """
    opaque: list[str] = []
    strict_blocking: list[str] = []
    terms: list[z3.BoolRef] = []
    for inv in invariants:
        if not inv.enforce:
            continue
        if inv.expr is None:
            if strict and inv.type not in RECOGNIZED_OPAQUE_TYPES:
                strict_blocking.append(inv.type)
            else:
                opaque.append(inv.type)
            continue
        expr = inv.expr
        if isinstance(expr, dict):
            expr = parse_expression(expr)
        # ForallSteps / ExistsStep at the outer level collapse to their
        # child predicate when the "plan" we reason over is a single
        # symbolic step (subsumption's max_steps=1 default).
        if isinstance(expr, ForallSteps):
            expr = expr.pred
        elif isinstance(expr, ExistsStep):
            expr = expr.pred
        terms.append(_compile_scalar(expr, scope, soft, scope.prefix))
    if not terms:
        return z3.BoolVal(True), opaque, strict_blocking
    if len(terms) == 1:
        return terms[0], opaque, strict_blocking
    return z3.And(*terms), opaque, strict_blocking


def _build_shell_step_from_model(
    model: z3.ModelRef, cmd_var: z3.ExprRef, scope_vars: dict[str, z3.ExprRef]
) -> tuple[ShellStep, dict[str, Any]]:
    """Decode a Z3 model into a ShellStep + raw var dump for display."""
    raw: dict[str, Any] = {}
    command = ""
    cmd_val = model[cmd_var]
    if cmd_val is not None and z3.is_string_value(cmd_val):
        command = cmd_val.as_string()
    metadata: dict[str, Any] = {}
    for name, var in scope_vars.items():
        val = model[var]
        if val is None:
            continue
        if z3.is_string_value(val):
            decoded = val.as_string()
        elif z3.is_int_value(val):
            decoded = val.as_long()
        else:
            decoded = str(val)
        raw[name] = decoded
        # Populate metadata from scope vars of shape "<scope>__metadata__<key>".
        marker = "__metadata__"
        if marker in name:
            meta_key = name.split(marker, 1)[1].replace("__", ".")
            metadata[meta_key] = decoded
    step = ShellStep(id="ctx_0", command=command or "<symbolic>", metadata=metadata)
    return step, raw


def _detect_interpreters(perms: Permission) -> list[str]:
    """Return shell allowlist entries that are names in SHELL_INTERPRETERS.

    An interpreter in the allowlist matches syntactically (e.g. ``find`` head
    OK) but its arguments carry semantics opaque to the envelope algebra
    (``find -exec rm {}``, ``python -c "…"``, ``xargs …``). Subsumption
    surfaces these so the caller knows the proof is syntactic, not semantic.
    """
    if not perms.shell:
        return []
    return sorted({name for name in perms.shell_allowlist if name in SHELL_INTERPRETERS})


def _robot_capability_violation(outer: Permission, inner: Permission) -> str | None:
    """Fail-closed comparison of declared robot capabilities (PLAN-LEVEL only).

    Returns a reason string if the inner (callee) envelope's declared physical
    capabilities are NOT contained by the outer (caller) envelope — i.e. the
    delegation cannot be proven safe at the plan level — else ``None``.

    Fail-closed: when the OUTER declares a bound and the inner exceeds it OR
    leaves it undeclared (undeclared = unbounded), that is a violation. When the
    outer declares no bound on an axis, that axis is unconstrained (the caller
    accepted it) and never triggers a violation — so this is a no-op for
    non-robot envelopes. This is NOT trajectory reachability and NOT a robot
    safety system; it closes the "robot bounds fail open" subsumption hole only.
    """
    # workspace_bounds: ((min_x,min_y,min_z),(max_x,max_y,max_z)).
    if outer.workspace_bounds is not None:
        if inner.workspace_bounds is None:
            return ("inner declares no workspace_bounds but outer constrains the "
                    "workspace (undeclared = unbounded → denied)")
        (o_min, o_max) = outer.workspace_bounds
        (i_min, i_max) = inner.workspace_bounds
        if any(i_min[k] < o_min[k] or i_max[k] > o_max[k] for k in range(3)):
            return (f"inner workspace_bounds {inner.workspace_bounds} exceed outer "
                    f"{outer.workspace_bounds}")

    for axis in ("velocity_limit", "torque_limit"):
        o_lim = getattr(outer, axis)
        if o_lim is not None:
            i_lim = getattr(inner, axis)
            if i_lim is None:
                return f"inner declares no {axis} but outer caps it (undeclared → denied)"
            if i_lim > o_lim:
                return f"inner {axis} {i_lim} exceeds outer {o_lim}"

    for joint, (o_lo, o_hi) in outer.joint_limits.items():
        if joint not in inner.joint_limits:
            return f"inner does not bound joint {joint!r} that outer limits (undeclared → denied)"
        i_lo, i_hi = inner.joint_limits[joint]
        if i_lo < o_lo or i_hi > o_hi:
            return f"inner joint {joint!r} range ({i_lo},{i_hi}) exceeds outer ({o_lo},{o_hi})"

    def _freeze(boxes):
        return {(tuple(lo), tuple(hi)) for lo, hi in boxes}

    missing = _freeze(outer.obstacles) - _freeze(inner.obstacles)
    if missing:
        return (f"inner omits {len(missing)} obstacle region(s) the outer forbids "
                f"(undeclared forbidden region → denied)")

    return None


def envelope_subsumes(
    outer: Envelope,
    inner: Envelope,
    *,
    timeout_ms: int = 2000,
    strict: bool = False,
) -> SubsumptionResult:
    """Prove ``outer ⊨ inner`` — every step inner admits, outer admits too.

    v0.11.0 scope: ShellStep-shaped symbolic steps. File and network steps
    are admitted identically by both envelopes if their allowlists match;
    this is verified structurally by ``_shell_head_in_allowlist`` and
    ``_glob_to_z3`` abstractions over free Z3 strings.

    v0.13.0: ``outer.shell_interpreter_policy`` governs what happens when
    either envelope's allowlist contains a shell interpreter (sh, bash,
    xargs, find, python, make, …). See ``SHELL_INTERPRETERS``. ``"surface"``
    flags the interpreter in ``unverified_invariants``; ``"strict"`` causes
    inner-admitted interpreters to short-circuit subsumption to holds=False;
    ``"allow"`` silences both.
    """
    t0 = time.monotonic()

    # Fail-closed robot-capability subsumption (v0.31). PLAN-LEVEL only — NOT a
    # robot safety system. When the outer declares a physical bound the inner
    # exceeds or leaves undeclared, the delegation cannot be proven safe. No-op
    # for non-robot envelopes (outer declares no robot bounds).
    robot_violation = _robot_capability_violation(outer.permissions, inner.permissions)
    if robot_violation is not None:
        return SubsumptionResult(
            holds=False,
            counterexample=None,
            reasons=[f"robot capability subsumption failed (fail-closed): {robot_violation}"],
            duration_ms=(time.monotonic() - t0) * 1000,
        )

    # File / network / MCP subsumption (v0.33.3). The Z3 admission formula below
    # encodes only shell + invariants, so these axes must be checked here or an
    # inner envelope with broader file/network/mcp scope than the outer would be
    # silently 'subsumed' — the core delegation-safety hole. Fail-closed.
    scope_violation = _permission_scope_violation(
        outer.permissions, inner.permissions, timeout_ms=timeout_ms
    )
    if scope_violation is not None:
        return SubsumptionResult(
            holds=False,
            counterexample=None,
            reasons=[f"permission scope subsumption failed (fail-closed): {scope_violation}"],
            duration_ms=(time.monotonic() - t0) * 1000,
        )

    # Interpreter policy check runs before Z3. Strict mode short-circuits
    # when inner admits an interpreter — Z3 can't reason about interpreter
    # arguments anyway, so there's no value in running the SAT query.
    policy = outer.shell_interpreter_policy
    inner_interpreters = _detect_interpreters(inner.permissions)
    outer_interpreters = _detect_interpreters(outer.permissions)
    if policy == "strict" and inner_interpreters:
        counter = Counterexample(
            step=ShellStep(
                id="ctx_0",
                command=f"{inner_interpreters[0]} <unverified interpreter>",
            ),
            outer_violation="shell_interpreter_policy",
            inner_justification="shell_allowlist",
        )
        return SubsumptionResult(
            holds=False,
            counterexample=counter,
            unverified_invariants=sorted(
                {f"shell_interpreter:{name}" for name in inner_interpreters}
                | {f"shell_interpreter:{name}" for name in outer_interpreters}
            ),
            duration_ms=(time.monotonic() - t0) * 1000,
        )

    solver = z3.Solver()
    solver.set("timeout", timeout_ms)

    cmd = z3.String("ctx_command")
    soft_inner: list[str] = []
    soft_outer: list[str] = []

    # Symbolic step scope — no concrete binding, string fields stay free.
    # We seed scope.vars with the command variable so invariants referencing
    # ``command`` share it with the permission check.
    scope_inner = _Scope(prefix="ctx", concrete=None)
    scope_inner.vars["ctx__command"] = cmd
    scope_outer = _Scope(prefix="ctx", concrete=None)
    scope_outer.vars["ctx__command"] = cmd

    inner_shell = _encode_shell_admission(inner.permissions, cmd)
    outer_shell = _encode_shell_admission(outer.permissions, cmd)

    inner_inv, inner_opaque, inner_strict_blocking = _compile_invariants(
        inner.invariants, scope_inner, soft_inner, strict=strict
    )
    outer_inv, outer_opaque, outer_strict_blocking = _compile_invariants(
        outer.invariants, scope_outer, soft_outer, strict=strict
    )

    # Strict mode: opaque non-recognized inner invariants can't be verified
    # symbolically — delegation cannot be proven safe. Short-circuit before Z3
    # (mirrors the shell_interpreter_policy == "strict" precedent at `:227-232`).
    if strict and inner_strict_blocking:
        reasons = [
            f"inner invariant '{t}' is opaque and unrecognized; "
            f"delegation cannot be proven safe under strict mode — "
            f"suggested_remediation: add an `expr` to make it verifiable, "
            f"or set enforce=False to keep it as documentation"
            for t in sorted(inner_strict_blocking)
        ]
        return SubsumptionResult(
            holds=False,
            counterexample=None,
            # Even on this early refusal, surface the outer envelope's opaque
            # constraints so the caller's visibility matches the normal path.
            unverified_invariants=sorted(set(outer_opaque) | set(outer_strict_blocking)),
            reasons=reasons,
            duration_ms=(time.monotonic() - t0) * 1000,
        )

    inner_admits = z3.And(inner_shell, inner_inv)
    outer_admits = z3.And(outer_shell, outer_inv)

    # Share any metadata vars that exist on both scopes (same Z3 name → same var).
    shared_vars: dict[str, z3.ExprRef] = {}
    for name, var in scope_inner.vars.items():
        shared_vars[name] = var
    for name, var in scope_outer.vars.items():
        shared_vars.setdefault(name, var)

    # Soft-node polarity. Soft nodes come from LLMCheck and regex-fallback
    # predicates that can't be compiled symbolically. For the "try to
    # disprove subsumption" stance we want:
    #   - inner soft = True   (optimistic — inner is maximally permissive,
    #                          admits anything its soft predicate might allow)
    #   - outer soft = False  (pessimistic — outer rejects what we can't
    #                          prove it admits)
    # Soft-node names are position-keyed within a scope (see
    # predicate_z3._compile_scalar), so when inner and outer declare the
    # SAME soft predicate at the same position they share a Z3 Bool — in
    # that case the inner binding (True) wins and the shared check behaves
    # as "both sides agree," which is what identical envelopes need.
    #
    # Fixed in v0.13.0 (Attack C from the v0.11 audit). The v0.11/v0.12
    # code optimistically bound outer to True, which silently approved
    # subsumption when outer had a stricter LLMCheck than inner.
    inner_soft_set = set(soft_inner)
    outer_soft_unique_names = [n for n in soft_outer if n not in inner_soft_set]
    # Fail-closed on outer-only soft constraints. The old approach pinned them to
    # False ("pessimistic"), but that was fail-OPEN under negation: an outer
    # deny-rule using an unsupported regex (NotMatches r'\bsudo\b') compiles to
    # Not(soft); pinning soft=False makes the term True and SILENTLY DROPS the
    # deny-rule → holds=True. An outer constraint the inner doesn't share means
    # inner may be broader in either polarity, so subsumption can't be proven.
    if outer_soft_unique_names:
        return SubsumptionResult(
            holds=False,
            counterexample=None,
            unverified_invariants=sorted(
                set(inner_opaque) | set(outer_opaque) | set(outer_strict_blocking)
                | {f"soft:{n}" for n in outer_soft_unique_names}
            ),
            reasons=[
                f"outer has unverifiable soft constraints not shared by inner "
                f"({sorted(outer_soft_unique_names)}); cannot prove subsumption (fail-closed)"
            ],
            duration_ms=(time.monotonic() - t0) * 1000,
        )
    for name in soft_inner:
        solver.add(z3.Bool(name) == z3.BoolVal(True))

    solver.add(inner_admits)
    solver.add(z3.Not(outer_admits))

    result = solver.check()
    duration_ms = (time.monotonic() - t0) * 1000

    # Surface outer soft nodes (LLMCheck, regex fallback) as unverified so
    # the caller sees what couldn't be proven statically. The "soft:" prefix
    # distinguishes them from opaque invariants (those without an expr at all).
    outer_soft_unique = [n for n in soft_outer if n not in inner_soft_set]
    interpreter_surface: set[str] = set()
    if policy != "allow":
        interpreter_surface = {
            f"shell_interpreter:{name}"
            for name in inner_interpreters + outer_interpreters
        }
    # outer_strict_blocking holds outer opaque non-recognized invariants under
    # strict mode. Unlike inner_strict_blocking (which hard-fails delegation),
    # an OUTER opaque constraint doesn't make delegation unsafe — it's the
    # caller's own under-specified guard. Surface it so strict visibility is
    # never worse than non-strict (where it appears in outer_opaque).
    unverified = sorted(
        set(inner_opaque)
        | set(outer_opaque)
        | set(outer_strict_blocking)
        | {f"soft:{n}" for n in outer_soft_unique}
        | interpreter_surface
    )

    if result == z3.unknown:
        raise VerificationTimeout(
            f"Z3 subsumption check exceeded {timeout_ms}ms"
        )
    if result == z3.unsat:
        return SubsumptionResult(
            holds=True,
            counterexample=None,
            unverified_invariants=unverified,
            duration_ms=duration_ms,
        )
    model = solver.model()
    step, raw = _build_shell_step_from_model(model, cmd, shared_vars)
    # Diagnose which outer rule failed. Narrow to Z3Exception so genuine
    # programming errors (malformed substitutions, etc.) surface to the
    # auditor as tracebacks rather than being swallowed as "unknown".
    try:
        outer_shell_val = z3.simplify(z3.substitute(outer_shell, *[
            (v, model[v]) for v in shared_vars.values() if model[v] is not None
        ]))
        outer_inv_val = z3.simplify(z3.substitute(outer_inv, *[
            (v, model[v]) for v in shared_vars.values() if model[v] is not None
        ]))
    except z3.Z3Exception:
        outer_shell_val = z3.BoolVal(False)
        outer_inv_val = z3.BoolVal(False)
    if z3.is_false(outer_shell_val):
        outer_violation = "shell_allowlist"
    elif z3.is_false(outer_inv_val):
        outer_violation = "invariant"
    else:
        outer_violation = "unknown"
    counter = Counterexample(
        step=step,
        outer_violation=outer_violation,
        inner_justification="shell_allowlist",
        raw_model=raw,
    )
    return SubsumptionResult(
        holds=False,
        counterexample=counter,
        unverified_invariants=unverified,
        duration_ms=duration_ms,
    )


__all__ = ["Counterexample", "SubsumptionResult", "envelope_subsumes"]
