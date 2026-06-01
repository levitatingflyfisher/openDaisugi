"""Compile predicate expressions to Z3 and evaluate them over an ActionPlan.

v0.11.0 honesty pass: ``compile_to_z3`` now emits real Z3 BoolRef expression
trees — ``InRe`` nodes for ``Matches``, ``String ==`` nodes for ``Equals``,
``Or`` of literal equalities for ``InSet``, real ``And``/``Or``/``Not``/
``Implies`` connectives, and concrete-plan quantifier unrolling into Z3
conjunctions and disjunctions. The Python fast path (``evaluate_predicate``)
is retained for ground evaluation inside ``verify()``; Z3 earns its keep at
subsumption and counterexample generation time.

Soft nodes — ``LLMCheck`` and regex patterns outside the translator's
supported subset — lower to free Z3 ``Bool`` variables and are reported in
``CompiledPredicate.soft_nodes`` so callers can discharge them at Stage 2.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any

import z3

from opendaisugi.exceptions import VerificationTimeout
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.predicate import (
    AliasRef,
    And,
    Before,
    DependsOn,
    Equals,
    Exists,
    ExistsStep,
    ForallOutputs,
    ForallSteps,
    Implies,
    InSet,
    IsEmpty,
    LengthRange,
    LLMCheck,
    Matches,
    Not,
    NotEquals,
    NotInSet,
    NotMatches,
    NumericRange,
    Or,
)
from opendaisugi.regex_to_z3 import UnsupportedRegexError, translate as translate_regex


_MISSING = object()


def _resolve_path(obj: Any, path: str) -> Any:
    cur = obj
    for part in path.split("."):
        if isinstance(cur, dict):
            if part not in cur:
                return _MISSING
            cur = cur[part]
        else:
            attr = getattr(cur, part, _MISSING)
            if attr is _MISSING:
                return _MISSING
            cur = attr
    return cur


def _step_to_dict(step: Any) -> dict[str, Any]:
    if hasattr(step, "model_dump"):
        return step.model_dump()
    return dict(step)


def _eval_scalar(expr: Any, scope: dict[str, Any]) -> bool:
    if isinstance(expr, Equals):
        return _resolve_path(scope, expr.path) == expr.value
    if isinstance(expr, NotEquals):
        val = _resolve_path(scope, expr.path)
        return val is not _MISSING and val != expr.value
    if isinstance(expr, InSet):
        val = _resolve_path(scope, expr.path)
        return val is not _MISSING and val in expr.values
    if isinstance(expr, NotInSet):
        val = _resolve_path(scope, expr.path)
        return val is not _MISSING and val not in expr.values
    if isinstance(expr, Matches):
        val = _resolve_path(scope, expr.path)
        if val is _MISSING or not isinstance(val, str):
            return False
        return re.search(expr.regex, val) is not None
    if isinstance(expr, NotMatches):
        val = _resolve_path(scope, expr.path)
        if val is _MISSING or not isinstance(val, str):
            return True
        return re.search(expr.regex, val) is None
    if isinstance(expr, NumericRange):
        val = _resolve_path(scope, expr.path)
        if val is _MISSING or not isinstance(val, (int, float)):
            return False
        return expr.min <= float(val) <= expr.max
    if isinstance(expr, LengthRange):
        val = _resolve_path(scope, expr.path)
        if val is _MISSING or not hasattr(val, "__len__"):
            return False
        n = len(val)
        if n < expr.min:
            return False
        if expr.max is not None and n > expr.max:
            return False
        return True
    if isinstance(expr, Exists):
        return _resolve_path(scope, expr.path) is not _MISSING
    if isinstance(expr, IsEmpty):
        val = _resolve_path(scope, expr.path)
        if val is _MISSING or val is None:
            return True
        if hasattr(val, "__len__"):
            return len(val) == 0
        return False
    if isinstance(expr, And):
        return all(_eval_scalar(c, scope) for c in expr.children)
    if isinstance(expr, Or):
        return any(_eval_scalar(c, scope) for c in expr.children)
    if isinstance(expr, Not):
        return not _eval_scalar(expr.child, scope)
    if isinstance(expr, Implies):
        return (not _eval_scalar(expr.a, scope)) or _eval_scalar(expr.b, scope)
    if isinstance(expr, LLMCheck):
        raise ValueError(
            "LLMCheck must be evaluated via evaluate_llm_check, not _eval_scalar"
        )
    if isinstance(expr, AliasRef):
        raise ValueError(
            f"unresolved alias reference '{expr.name}'; resolve aliases before evaluation"
        )
    raise ValueError(f"unknown predicate op: {getattr(expr, 'op', type(expr).__name__)!r}")


def evaluate_predicate(expr: Any, plan: ActionPlan, envelope: Envelope) -> bool:
    step_dicts = [_step_to_dict(s) for s in plan.steps]

    def go(e: Any) -> bool:
        if isinstance(e, ForallSteps):
            return all(_eval_scalar(e.pred, s) for s in step_dicts)
        if isinstance(e, ExistsStep):
            return any(_eval_scalar(e.pred, s) for s in step_dicts)
        if isinstance(e, ForallOutputs):
            outputs = [s.get("metadata", {}).get("output") for s in step_dicts]
            outputs = [{"output": o} for o in outputs if o is not None]
            return all(_eval_scalar(e.pred, s) for s in outputs)
        if isinstance(e, DependsOn):
            for s in step_dicts:
                if s.get("id") == e.step_id_a:
                    return e.step_id_b in (s.get("depends_on") or [])
            return False
        if isinstance(e, Before):
            ids = [s.get("id") for s in step_dicts]
            if e.step_id_a not in ids or e.step_id_b not in ids:
                return False
            return ids.index(e.step_id_a) < ids.index(e.step_id_b)
        if isinstance(e, LLMCheck):
            if getattr(envelope, "stakes", "low") == "physical":
                raise ValueError(
                    "llm_check blocked for physical stakes — use sound primitives only"
                )
            from opendaisugi.llm_check import run_llm_check
            payload = {"task": plan.task, "steps": step_dicts}
            res = run_llm_check(e.rule, payload)
            # Fail CLOSED: a failed probabilistic check (network/timeout/
            # rate-limit) must never be read as "satisfied". Raise so the
            # caller records a violation rather than silently approving.
            if res.errored:
                raise ValueError(res.reason)
            return res.satisfied
        if isinstance(e, And):
            return all(go(c) for c in e.children)
        if isinstance(e, Or):
            return any(go(c) for c in e.children)
        if isinstance(e, Not):
            return not go(e.child)
        if isinstance(e, Implies):
            return (not go(e.a)) or go(e.b)
        synthetic = {"steps": step_dicts}
        return _eval_scalar(e, synthetic)

    return go(expr)


# -------------------- v0.11.0 real Z3 compilation --------------------


@dataclass
class CompiledPredicate:
    """The result of symbolic predicate compilation.

    ``term`` is a real Z3 BoolRef expression tree. ``variables`` maps
    human-readable paths (``step_0.metadata.body``) to the Z3 variables
    they were bound to. ``soft_nodes`` lists predicate fragments that
    could not be compiled symbolically (LLMCheck, unsupported regex) —
    each such fragment is represented by a free Z3 Bool whose name is
    in the list.
    """

    term: z3.BoolRef
    variables: dict[str, z3.ExprRef] = field(default_factory=dict)
    soft_nodes: list[str] = field(default_factory=list)
    assumptions: list[z3.BoolRef] = field(default_factory=list)


def _z3_lit(value: Any) -> z3.ExprRef:
    if isinstance(value, bool):
        return z3.BoolVal(value)
    if isinstance(value, int):
        return z3.IntVal(value)
    if isinstance(value, float):
        return z3.RealVal(value)
    return z3.StringVal(str(value))


def _var_name(prefix: str, path: str) -> str:
    return f"{prefix}__{path.replace('.', '__')}"


class _Scope:
    """One symbolic step's variable registry.

    For concrete steps we substitute known values in as Z3 literals.
    For symbolic steps (subsumption), variables stay free.
    """

    def __init__(self, prefix: str, concrete: dict[str, Any] | None):
        self.prefix = prefix
        self.concrete = concrete  # None => fully symbolic
        self.vars: dict[str, z3.ExprRef] = {}
        self.assumptions: list[z3.BoolRef] = []

    def resolve_string(self, path: str) -> tuple[z3.ExprRef, bool]:
        """Returns (z3_var, present_in_concrete).

        For concrete scopes, if the path resolves to a string we bind the
        var to that literal and return present=True. If absent, we return
        a fresh var plus present=False. For symbolic scopes, always fresh
        and present=True (we don't model absence by default)."""
        name = _var_name(self.prefix, path)
        if name in self.vars:
            # Assume repeats share presence state — acceptable for our ops.
            present = True
            if self.concrete is not None:
                present = _resolve_path(self.concrete, path) is not _MISSING
            return self.vars[name], present
        v = z3.String(name)
        self.vars[name] = v
        if self.concrete is not None:
            val = _resolve_path(self.concrete, path)
            if val is _MISSING:
                return v, False
            self.assumptions.append(v == z3.StringVal(str(val)))
        return v, True

    def resolve_numeric(self, path: str) -> tuple[z3.ExprRef, bool]:
        name = _var_name(self.prefix, path)
        key = name + "__real"
        if key in self.vars:
            present = True
            if self.concrete is not None:
                val = _resolve_path(self.concrete, path)
                present = isinstance(val, (int, float))
            return self.vars[key], present
        v = z3.Real(name)
        self.vars[key] = v
        if self.concrete is not None:
            val = _resolve_path(self.concrete, path)
            if not isinstance(val, (int, float)):
                return v, False
            self.assumptions.append(v == z3.RealVal(float(val)))
        return v, True


def _compile_scalar(
    expr: Any,
    scope: _Scope,
    soft: list[str],
    soft_prefix: str,
) -> z3.BoolRef:
    if isinstance(expr, Equals):
        if isinstance(expr.value, (int, float)) and not isinstance(expr.value, bool):
            var, present = scope.resolve_numeric(expr.path)
            if not present:
                return z3.BoolVal(False)
            return var == _z3_lit(expr.value)
        var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        return var == _z3_lit(expr.value)
    if isinstance(expr, NotEquals):
        var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        return var != _z3_lit(expr.value)
    if isinstance(expr, InSet):
        if expr.values and isinstance(expr.values[0], (int, float)) and not isinstance(expr.values[0], bool):
            var, present = scope.resolve_numeric(expr.path)
        else:
            var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        if not expr.values:
            return z3.BoolVal(False)
        return z3.Or(*[var == _z3_lit(v) for v in expr.values])
    if isinstance(expr, NotInSet):
        if expr.values and isinstance(expr.values[0], (int, float)) and not isinstance(expr.values[0], bool):
            var, present = scope.resolve_numeric(expr.path)
        else:
            var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        if not expr.values:
            return z3.BoolVal(True)
        return z3.And(*[var != _z3_lit(v) for v in expr.values])
    if isinstance(expr, Matches):
        var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        try:
            z3_re = translate_regex(expr.regex)
        except UnsupportedRegexError:
            name = f"{soft_prefix}__matches__{len(soft)}"
            soft.append(name)
            return z3.Bool(name)
        return z3.InRe(var, z3_re)
    if isinstance(expr, NotMatches):
        var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(True)
        try:
            z3_re = translate_regex(expr.regex)
        except UnsupportedRegexError:
            name = f"{soft_prefix}__not_matches__{len(soft)}"
            soft.append(name)
            return z3.Not(z3.Bool(name))
        return z3.Not(z3.InRe(var, z3_re))
    if isinstance(expr, NumericRange):
        var, present = scope.resolve_numeric(expr.path)
        if not present:
            return z3.BoolVal(False)
        return z3.And(var >= z3.RealVal(expr.min), var <= z3.RealVal(expr.max))
    if isinstance(expr, LengthRange):
        # Concrete scope with a non-string value: evaluate in Python (lists,
        # dicts, etc. don't have a clean symbolic encoding). Concrete strings
        # AND symbolic scopes compile to z3.Length so subsumption over body
        # lengths stays sound.
        if scope.concrete is not None:
            val = _resolve_path(scope.concrete, expr.path)
            if val is _MISSING or not hasattr(val, "__len__"):
                return z3.BoolVal(False)
            if not isinstance(val, str):
                n = len(val)
                ok = n >= expr.min and (expr.max is None or n <= expr.max)
                return z3.BoolVal(ok)
        var, present = scope.resolve_string(expr.path)
        if not present:
            return z3.BoolVal(False)
        length = z3.Length(var)
        bounds: list[z3.BoolRef] = [length >= z3.IntVal(expr.min)]
        if expr.max is not None:
            bounds.append(length <= z3.IntVal(expr.max))
        return z3.And(*bounds) if len(bounds) > 1 else bounds[0]
    if isinstance(expr, Exists):
        # Concrete: presence test; symbolic: always-present assumption.
        if scope.concrete is None:
            return z3.BoolVal(True)
        return z3.BoolVal(_resolve_path(scope.concrete, expr.path) is not _MISSING)
    if isinstance(expr, IsEmpty):
        # Fall back to Python — length semantics over structured values.
        if scope.concrete is None:
            name = f"{soft_prefix}__is_empty__{len(soft)}"
            soft.append(name)
            return z3.Bool(name)
        val = _resolve_path(scope.concrete, expr.path)
        if val is _MISSING or val is None:
            return z3.BoolVal(True)
        return z3.BoolVal(hasattr(val, "__len__") and len(val) == 0)
    if isinstance(expr, And):
        if not expr.children:
            return z3.BoolVal(True)
        return z3.And(*[_compile_scalar(c, scope, soft, soft_prefix) for c in expr.children])
    if isinstance(expr, Or):
        if not expr.children:
            return z3.BoolVal(False)
        return z3.Or(*[_compile_scalar(c, scope, soft, soft_prefix) for c in expr.children])
    if isinstance(expr, Not):
        return z3.Not(_compile_scalar(expr.child, scope, soft, soft_prefix))
    if isinstance(expr, Implies):
        return z3.Implies(
            _compile_scalar(expr.a, scope, soft, soft_prefix),
            _compile_scalar(expr.b, scope, soft, soft_prefix),
        )
    if isinstance(expr, LLMCheck):
        name = f"{soft_prefix}__llm_check__{len(soft)}"
        soft.append(name)
        return z3.Bool(name)
    if isinstance(expr, AliasRef):
        raise ValueError(
            f"unresolved alias reference '{expr.name}'; resolve aliases before compilation"
        )
    raise ValueError(f"unknown scalar predicate op: {type(expr).__name__}")


def compile_to_z3(
    expr: Any,
    plan: ActionPlan,
    envelope: Envelope,
) -> CompiledPredicate:
    """Emit a real Z3 expression tree for the predicate over the concrete plan.

    This is the honest version of the v0.9.0 function: every scalar op
    (Equals, Matches, InSet, NumericRange, …) becomes a Z3 term; And/Or/
    Not/Implies map to their Z3 connectives; ForallSteps/ExistsStep unroll
    over the concrete step list into Z3 And/Or respectively. The only soft
    edges are LLMCheck and regexes the translator can't handle — those
    become free Z3 Booleans listed in ``soft_nodes``.
    """
    step_dicts = [_step_to_dict(s) for s in plan.steps]
    soft: list[str] = []
    variables: dict[str, z3.ExprRef] = {}
    assumptions: list[z3.BoolRef] = []

    def build_scope(idx: int, step: dict[str, Any]) -> _Scope:
        sc = _Scope(prefix=f"step_{idx}", concrete=step)
        return sc

    def go(e: Any) -> z3.BoolRef:
        if isinstance(e, ForallSteps):
            if not step_dicts:
                return z3.BoolVal(True)
            terms: list[z3.BoolRef] = []
            for i, step in enumerate(step_dicts):
                sc = build_scope(i, step)
                terms.append(_compile_scalar(e.pred, sc, soft, sc.prefix))
                variables.update(sc.vars)
                assumptions.extend(sc.assumptions)
            return z3.And(*terms) if len(terms) > 1 else terms[0]
        if isinstance(e, ExistsStep):
            if not step_dicts:
                return z3.BoolVal(False)
            terms = []
            for i, step in enumerate(step_dicts):
                sc = build_scope(i, step)
                terms.append(_compile_scalar(e.pred, sc, soft, sc.prefix))
                variables.update(sc.vars)
                assumptions.extend(sc.assumptions)
            return z3.Or(*terms) if len(terms) > 1 else terms[0]
        if isinstance(e, ForallOutputs):
            outputs = [
                {"output": s.get("metadata", {}).get("output")}
                for s in step_dicts
                if s.get("metadata", {}).get("output") is not None
            ]
            if not outputs:
                return z3.BoolVal(True)
            terms = []
            for i, out in enumerate(outputs):
                sc = _Scope(prefix=f"out_{i}", concrete=out)
                terms.append(_compile_scalar(e.pred, sc, soft, sc.prefix))
                variables.update(sc.vars)
                assumptions.extend(sc.assumptions)
            return z3.And(*terms) if len(terms) > 1 else terms[0]
        if isinstance(e, DependsOn):
            for s in step_dicts:
                if s.get("id") == e.step_id_a:
                    return z3.BoolVal(e.step_id_b in (s.get("depends_on") or []))
            return z3.BoolVal(False)
        if isinstance(e, Before):
            ids = [s.get("id") for s in step_dicts]
            if e.step_id_a not in ids or e.step_id_b not in ids:
                return z3.BoolVal(False)
            return z3.BoolVal(ids.index(e.step_id_a) < ids.index(e.step_id_b))
        if isinstance(e, LLMCheck):
            name = f"plan__llm_check__{len(soft)}"
            soft.append(name)
            return z3.Bool(name)
        if isinstance(e, And):
            return z3.And(*[go(c) for c in e.children]) if e.children else z3.BoolVal(True)
        if isinstance(e, Or):
            return z3.Or(*[go(c) for c in e.children]) if e.children else z3.BoolVal(False)
        if isinstance(e, Not):
            return z3.Not(go(e.child))
        if isinstance(e, Implies):
            return z3.Implies(go(e.a), go(e.b))
        # Scalar at plan root: evaluate against a synthetic "steps" scope.
        synthetic = {"steps": step_dicts}
        sc = _Scope(prefix="plan", concrete=synthetic)
        term = _compile_scalar(e, sc, soft, sc.prefix)
        variables.update(sc.vars)
        assumptions.extend(sc.assumptions)
        return term

    term = go(expr)
    return CompiledPredicate(
        term=term, variables=variables, soft_nodes=soft, assumptions=assumptions
    )


@dataclass
class Counterexample:
    """A specific assignment falsifying a symbolic predicate.

    ``model_values`` maps Z3 variable name → decoded Python value. Callers
    use this to explain *why* verification failed — e.g. for ``envelope
    subsumption``, which concrete step the callee could emit that the
    caller's envelope forbids.
    """

    model_values: dict[str, Any]
    soft_nodes: list[str]


def _decode_model_value(z3_val: z3.ExprRef) -> Any:
    if z3.is_string_value(z3_val):
        return z3_val.as_string()
    if z3.is_int_value(z3_val):
        return z3_val.as_long()
    if z3.is_rational_value(z3_val):
        try:
            return float(z3_val.as_decimal(6).rstrip("?"))
        except Exception:
            return str(z3_val)
    if z3.is_bool(z3_val):
        return bool(z3_val)
    return str(z3_val)


def verify_predicate_z3(
    expr: Any,
    plan: ActionPlan,
    envelope: Envelope,
    *,
    timeout_ms: int = 500,
) -> tuple[bool, Counterexample | None]:
    """Prove ``expr`` holds over ``plan`` using the Z3 solver.

    Returns ``(True, None)`` iff Z3 can't satisfy ``Not(compiled.term)``
    under the variable-binding assumptions (i.e. the predicate is entailed).
    Returns ``(False, Counterexample)`` when Z3 finds an assignment that
    falsifies the predicate — for concrete plans this is typically triggered
    by a regex violation or an out-of-set value. Raises
    ``VerificationTimeout`` on Z3 ``unknown``.
    """
    compiled = compile_to_z3(expr, plan, envelope)
    solver = z3.Solver()
    solver.set("timeout", timeout_ms)
    for a in compiled.assumptions:
        solver.add(a)
    solver.add(z3.Not(compiled.term))
    result = solver.check()
    if result == z3.unknown:
        raise VerificationTimeout(
            f"Z3 predicate check exceeded {timeout_ms}ms"
        )
    if result == z3.unsat:
        return True, None
    model = solver.model()
    values: dict[str, Any] = {}
    for decl in model.decls():
        values[decl.name()] = _decode_model_value(model[decl])
    return False, Counterexample(model_values=values, soft_nodes=compiled.soft_nodes)


__all__ = [
    "CompiledPredicate",
    "Counterexample",
    "compile_to_z3",
    "evaluate_predicate",
    "verify_predicate_z3",
]
