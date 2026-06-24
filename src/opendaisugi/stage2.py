"""Stage 2 output verification (v0.9.0).

Runs envelope postconditions against an execution-completed step before
the effect commits to the outside world. Same predicate DSL as Stage 1;
different evaluation point - the step's metadata is now populated with
LLM-generated fields (body, output, generated content).
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

from opendaisugi.aliases import AliasRegistry, UnknownAliasError
from opendaisugi.models import ActionPlan, Envelope, Postcondition, Violation
from opendaisugi.predicate import AliasRef, parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate
from opendaisugi.verify import resolve_strict


def _normalize_expr(raw):
    if raw is None:
        return None
    if isinstance(raw, dict):
        return parse_expression(raw)
    return raw


# v0.28.3: opaque postconditions the envelope-gen few-shot prompt teaches
# the LLM to author. Pre-v0.28.3, these had no handler — at strict they
# raised "no verifiable expr", at non-strict they passed silently. Each
# handler returns ``(ok, detail)``: ``ok=True`` discharges the
# postcondition; ``ok=False`` produces a Violation with ``detail``
# attached.

def _check_exit_code(
    pc: Postcondition, step: Any
) -> tuple[bool, dict[str, Any]]:
    metadata = getattr(step, "metadata", {}) or {}
    rc = metadata.get("rc")
    if rc is None:
        return False, {"reason": "step metadata missing rc"}
    if pc.expected is None:
        return False, {"reason": "exit_code postcondition missing `expected`"}
    return rc == pc.expected, {"observed_rc": rc, "expected": pc.expected}


def _check_file_exists(
    pc: Postcondition, step: Any
) -> tuple[bool, dict[str, Any]]:
    if not pc.path:
        return False, {"reason": "file_exists postcondition missing `path`"}
    exists = Path(pc.path).exists()
    return exists, {"path": pc.path, "exists": exists}


def _check_file_size_range(
    pc: Postcondition, step: Any
) -> tuple[bool, dict[str, Any]]:
    if not pc.path:
        return False, {"reason": "file_size_range postcondition missing `path`"}
    # v0.28.3-followup: require at least one bound. Pre-fix defaulted both
    # to (0, inf), silently accepting any size — strictly LOOSER than the
    # pre-v0.28.3 strict-mode rejection of "no verifiable expr". A
    # bound-less postcondition constrains nothing; treat as misconfig.
    if pc.min is None and pc.max is None:
        return False, {
            "reason": "file_size_range postcondition needs at least one of "
                      "`min` / `max`; otherwise it constrains nothing",
        }
    p = Path(pc.path)
    if not p.exists():
        return False, {"path": pc.path, "reason": "file does not exist"}
    size = p.stat().st_size
    lo = pc.min if pc.min is not None else 0
    hi = pc.max if pc.max is not None else float("inf")
    return lo <= size <= hi, {"path": pc.path, "size": size, "min": lo, "max": hi}


_OPAQUE_POSTCONDITION_HANDLERS = {
    "exit_code": _check_exit_code,
    "file_exists": _check_file_exists,
    "file_size_range": _check_file_size_range,
}

# Keep _OPAQUE_POSTCONDITION_HANDLERS and
# _invariant_types.RECOGNIZED_STAGE2_POSTCONDITION_TYPES in sync — verify.py
# consults the latter for its strict-mode carve-out.
from opendaisugi._invariant_types import RECOGNIZED_STAGE2_POSTCONDITION_TYPES  # noqa: E402

assert set(_OPAQUE_POSTCONDITION_HANDLERS) == RECOGNIZED_STAGE2_POSTCONDITION_TYPES, (
    "stage2 handler set and RECOGNIZED_STAGE2_POSTCONDITION_TYPES diverged: "
    f"{set(_OPAQUE_POSTCONDITION_HANDLERS) ^ RECOGNIZED_STAGE2_POSTCONDITION_TYPES}"
)


def verify_completed_step(
    step, envelope: Envelope, *, strict: bool | None = None,
    aliases: AliasRegistry | None = None,
) -> list[Violation]:
    """Run envelope postconditions over a completed step; return any violations.

    The single completed step is wrapped in a synthetic ActionPlan so the
    predicate evaluator's quantifiers (forall_steps, exists_step) work
    uniformly over the Stage 1 and Stage 2 evaluation points.

    Stage 2 is the last gate before an effect commits externally. Under strict
    mode — default-on for high/physical stakes (v0.27.0) — an opaque enforced
    postcondition (no verifiable ``expr``) is a loud rejection rather than a
    silent pass: nothing discharges it here, so it cannot be trusted on faith.
    """
    effective_strict = resolve_strict(strict, envelope)
    pseudo_plan = ActionPlan(source="stage2", task=envelope.task, steps=[step])
    violations: list[Violation] = []
    for pc in envelope.postconditions:
        if not pc.enforce:
            continue
        expr = _normalize_expr(pc.expr)
        if expr is None:
            # v0.28.3: opaque postconditions with a recognized type get a
            # concrete handler. The strict-mode "no verifiable expr"
            # rejection still fires for unrecognized opaque types.
            handler = _OPAQUE_POSTCONDITION_HANDLERS.get(pc.type)
            if handler is not None:
                try:
                    ok, handler_detail = handler(pc, step)
                except Exception as e:
                    violations.append(Violation(
                        stage="stage2",
                        message=f"postcondition '{pc.type}' handler error: {e}",
                        detail={"postcondition": pc.type, "step_id": step.id},
                    ))
                    continue
                if not ok:
                    violations.append(Violation(
                        stage="stage2",
                        message=f"postcondition '{pc.type}' violated on completed step {step.id}",
                        detail={
                            "postcondition": pc.type,
                            "step_id": step.id,
                            **handler_detail,
                        },
                    ))
                continue
            if effective_strict:
                violations.append(Violation(
                    stage="stage2",
                    message=f"postcondition '{pc.type}' declares a safety property with no "
                            f"verifiable expr; cannot be discharged under strict mode",
                    detail={"postcondition": pc.type, "reason": "opaque_unrecognized",
                            "step_id": step.id,
                            "suggested_remediation": "add an `expr` to make it verifiable, "
                                                     "or set enforce=False to keep it as documentation"},
                ))
            continue
        # Resolve alias references through the registry (if provided) before
        # evaluation. Without a registry an AliasRef falls through and fails
        # closed at evaluate_predicate — never a silent pass.
        if isinstance(expr, AliasRef) and aliases is not None:
            try:
                expr = aliases.resolve(expr)
            except UnknownAliasError as e:
                missing = e.args[0] if e.args else expr.name
                violations.append(Violation(
                    stage="stage2",
                    message=f"postcondition '{pc.type}' references unresolved alias '{missing}'",
                    detail={"postcondition": pc.type, "reason": "unresolved_alias",
                            "alias": missing, "step_id": step.id},
                ))
                continue
        try:
            ok = evaluate_predicate(expr, pseudo_plan, envelope)
        except Exception as e:
            violations.append(Violation(
                stage="stage2",
                message=f"postcondition '{pc.type}' evaluation error: {e}",
                detail={"postcondition": pc.type, "step_id": step.id},
            ))
            continue
        if not ok:
            violations.append(Violation(
                stage="stage2",
                message=f"postcondition '{pc.type}' violated on completed step {step.id}",
                detail={
                    "postcondition": pc.type,
                    "description": pc.description,
                    "step_id": step.id,
                },
            ))
    return violations


__all__ = ["verify_completed_step"]
