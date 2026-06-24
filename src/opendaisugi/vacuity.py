"""Vacuity detection: is a compiled predicate trivially true or false? (v0.27.0)

A tautology constrains nothing (safety-theater); a contradiction blocks
everything (a DoS-class bug — the envelope can never pass). Soft (llm_check)
nodes are left UNCONSTRAINED so a predicate that is only "true" because a soft
Bool is free is NOT mis-reported as a real tautology.

Detection is performed symbolically over a single free symbolic step, matching
the encoding used in subsumption (``subsumption.py``). Quantifiers
(``ForallSteps``, ``ExistsStep``) are stripped so the inner predicate is
compiled once over the symbolic step — sufficient to detect per-step vacuity.
For plan-level quantifiers without quantified variables (``DependsOn``,
``Before``), the raw expression is compiled directly.
"""
from __future__ import annotations

from collections import OrderedDict
from typing import Literal

import z3

from opendaisugi.predicate import Expression, ExistsStep, ForallOutputs, ForallSteps
from opendaisugi.predicate_z3 import _Scope, _compile_scalar

Verdict = Literal["tautology", "contradiction", "non_trivial"]


# v0.28.6: bounded LRU cache. A typical envelope has 3-5 invariants and every
# call to ``verify()`` previously rebuilt the Z3 solvers per invariant. The
# cache keys on the JSON serialization of the (already alias-resolved)
# expression so structurally-identical predicates hit. Bounded to 512 entries
# so a hostile caller cannot blow process memory by churning unique exprs.
_VACUITY_CACHE: "OrderedDict[tuple[str, int], Verdict]" = OrderedDict()
_VACUITY_CACHE_MAX = 512


def _cache_key(expr: Expression, timeout_ms: int) -> tuple[str, int]:
    # model_dump_json is canonical for Pydantic — equal predicates hash equal.
    # Timeout is part of the key because changing the budget can flip a
    # ``non_trivial`` (Z3 unknown) into a real verdict; mixing both under the
    # same key would be unsound.
    return (expr.model_dump_json(), timeout_ms)


def clear_vacuity_cache() -> None:
    """Drop every cached vacuity verdict. Test-only helper."""
    _VACUITY_CACHE.clear()


def check_vacuity(expr: Expression, *, timeout_ms: int = 500) -> Verdict:
    """Return the vacuity verdict for a predicate expression.

    Compiles the expression symbolically (a single free symbolic step, no
    concrete plan required). Soft nodes (``LLMCheck``, unsupported regex) are
    left as free Z3 Booleans so they do not accidentally manufacture a
    tautology or block a contradiction finding.

    Result is memoized in a bounded LRU keyed on
    ``(expr.model_dump_json(), timeout_ms)`` (v0.28.6) — verify() typically
    calls this 3-5 times per envelope and the result is structurally
    deterministic.

    Args:
        expr:        The predicate expression to check.
        timeout_ms:  Z3 solver timeout in milliseconds (default 500 ms).

    Returns:
        ``"tautology"``    — negation is UNSAT; predicate constrains nothing.
        ``"contradiction"`` — predicate itself is UNSAT; envelope can never pass.
        ``"non_trivial"``  — neither (or Z3 timed out / returned unknown).
    """
    try:
        key = _cache_key(expr, timeout_ms)
    except Exception:
        # Non-serializable expr — fall through to the uncached path. Cannot
        # happen for shipped Expression types but keeps the cache from
        # gating correctness on Pydantic's serializer.
        key = None
    if key is not None and key in _VACUITY_CACHE:
        _VACUITY_CACHE.move_to_end(key)
        return _VACUITY_CACHE[key]

    verdict = _compute_vacuity(expr, timeout_ms)

    if key is not None:
        _VACUITY_CACHE[key] = verdict
        if len(_VACUITY_CACHE) > _VACUITY_CACHE_MAX:
            _VACUITY_CACHE.popitem(last=False)
    return verdict


def _compute_vacuity(expr: Expression, timeout_ms: int) -> Verdict:
    # Strip outer quantifiers — vacuity operates on the per-step/per-output predicate.
    inner: Expression
    if isinstance(expr, (ForallSteps, ExistsStep, ForallOutputs)):
        inner = expr.pred
    else:
        inner = expr

    soft: list[str] = []
    scope = _Scope(prefix="vac", concrete=None)  # fully symbolic — no concrete plan
    term = _compile_scalar(inner, scope, soft, "vac")

    # Domain assumptions (e.g. string variable equality constraints). Soft nodes
    # stay free (not added to any solver).
    assumptions: list[z3.BoolRef] = list(scope.assumptions)

    # --- Contradiction check: is the predicate UNSAT within the domain? ---
    # Assumptions belong here: we ask whether a satisfying assignment exists at all.
    s_sat = z3.Solver()
    s_sat.set("timeout", timeout_ms)
    if assumptions:
        s_sat.add(*assumptions)
    s_sat.add(term)
    sat_result = s_sat.check()
    if sat_result == z3.unsat:
        return "contradiction"

    # --- Tautology check: is the negation UNSAT? ---
    # Assumptions are deliberately NOT added here. Adding them would report a
    # predicate as a tautology when it is only "always true" within the assumed
    # domain — a genuine constraint, not safety-theater. We want unconditional
    # tautologies only.
    s_taut = z3.Solver()
    s_taut.set("timeout", timeout_ms)
    s_taut.add(z3.Not(term))
    taut_result = s_taut.check()
    if taut_result == z3.unsat:
        return "tautology"

    return "non_trivial"
