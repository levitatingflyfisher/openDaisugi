"""Alias registration and resolution (v0.9.0).

An alias is a named, parameterizable predicate expression. Three tiers:

    - ``system``    shipped with opendaisugi, immutable
    - ``household`` workspace-shared, authored by agents or operators
    - ``envelope``  private to a single envelope

Resolution: lookup by name picks the highest-precedence tier
(envelope > household > system). Parameter substitution walks the
expression tree and replaces any value equal to ``$<param>`` with the
corresponding argument.

Static check at registration time: the alias expression must reference
at least one plan path (via Equals/NotEquals/InSet/Matches/.../Exists
path field) - catches trivial tautologies. Full counterexample-based
vacuity check (tautology/contradiction) runs via Z3 as of v0.27.0.
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field

from opendaisugi.predicate import (
    AliasRef,
    And,
    ExistsStep,
    ForallOutputs,
    ForallSteps,
    Implies,
    Not,
    Or,
    parse_expression,
)

Tier = Literal["system", "household", "envelope"]
_TIER_ORDER: dict[str, int] = {"envelope": 0, "household": 1, "system": 2}


class Alias(BaseModel):
    name: str
    params: list[str] = Field(default_factory=list)
    expr: Any
    tier: Tier = "envelope"
    description: str = ""


class UnknownAliasError(KeyError):
    """Raised when resolving an AliasRef whose name isn't registered."""


class AliasCycleError(ValueError):
    """Raised when alias resolution encounters a cycle."""


class VacuousAliasError(ValueError):
    """Raised when registering an alias whose expr is a tautology or contradiction (v0.27.0)."""


_PATH_OPS = frozenset({
    "equals", "not_equals", "in_set", "not_in_set",
    "matches", "not_matches", "numeric_range", "exists", "is_empty",
    "depends_on", "before", "alias", "llm_check",
})


def _as_dict(expr: Any) -> dict[str, Any] | None:
    """Return the dict shape of an expression; None if not expressible as one."""
    if isinstance(expr, dict):
        return expr
    if hasattr(expr, "model_dump"):
        return expr.model_dump()
    return None


def _references_a_path(expr: Any) -> bool:
    d = _as_dict(expr)
    if d is None:
        return False
    op = d.get("op")
    if op in _PATH_OPS:
        return True
    if op in ("and", "or"):
        return any(_references_a_path(c) for c in d.get("children", []))
    if op == "not":
        return _references_a_path(d.get("child"))
    if op == "implies":
        return _references_a_path(d.get("a")) or _references_a_path(d.get("b"))
    if op in ("forall_steps", "exists_step", "forall_outputs"):
        return _references_a_path(d.get("pred"))
    return False


def _substitute_params(expr: Any, args: dict[str, Any]) -> Any:
    """Substitute placeholders and return a raw (dict/list/scalar) form.

    Pydantic models are expanded to dicts. The result is always plain data,
    so it can carry typed values through Pydantic-unsafe placeholders like
    `$max_scale` being spliced into a NumericRange.max float field.

    Substitution is **longest-key-first** (v0.28.3) so that ``$principal``
    cannot munge ``$principal_name`` — without this, dict-iteration order
    determines whether the longer placeholder gets seen. Without
    longest-first ordering, ``{"principal": "alice", "principal_name":
    "bob"}`` substituted into ``"$principal_name"`` could yield
    ``"alice_name"``.
    """
    if isinstance(expr, str):
        if expr.startswith("$") and expr[1:] in args:
            return args[expr[1:]]
        out = expr
        for name in sorted(args, key=len, reverse=True):
            out = out.replace(f"${name}", str(args[name]))
        return out

    if isinstance(expr, list):
        return [_substitute_params(x, args) for x in expr]

    if hasattr(expr, "model_dump"):
        return _substitute_params(expr.model_dump(), args)

    if isinstance(expr, dict):
        return {k: _substitute_params(v, args) for k, v in expr.items()}

    return expr


class AliasRegistry:
    """Tiered registry of named aliases."""

    def __init__(self, *, refinement_sink: Any = None) -> None:
        self._entries: dict[str, list[Alias]] = {}
        self._refinement_sink = refinement_sink

    def __contains__(self, name: str) -> bool:
        return name in self._entries

    def register(self, alias: Alias) -> None:
        if not _references_a_path(alias.expr):
            raise ValueError(
                f"alias '{alias.name}' has no plan-path reference (looks vacuous); "
                "static check requires at least one Equals/NotEquals/Matches/... on a path"
            )
        # v0.27.0: Z3-backed vacuity check — tautologies and contradictions are rejected.
        # Alias.expr is typed Any and may be a raw dict; parse it to an Expression
        # first so check_vacuity actually runs (a dict reaches _compile_scalar and
        # raises, which the broad except below would otherwise swallow — silently
        # admitting a vacuous dict-form alias).
        vacuity_verdict: str = "unknown"
        try:
            from opendaisugi.vacuity import check_vacuity
            expr_for_check = (
                parse_expression(alias.expr) if isinstance(alias.expr, dict) else alias.expr
            )
            vacuity_verdict = check_vacuity(expr_for_check)
            if vacuity_verdict in ("tautology", "contradiction"):
                raise VacuousAliasError(
                    f"alias '{alias.name}' is {vacuity_verdict} (constrains nothing / never satisfiable); "
                    "the predicate must be non-trivial to be registered"
                )
        except VacuousAliasError:
            raise
        except Exception:
            # Z3 unavailable, timeout, or unsupported expr — skip vacuity check gracefully.
            pass
        self._entries.setdefault(alias.name, []).append(alias)
        # v0.27.0: emit provenance to the refinement sink (fail-soft — never crashes register).
        if self._refinement_sink is not None:
            import logging
            _log = logging.getLogger("opendaisugi.aliases")
            try:
                self._refinement_sink.write_provenance({
                    "alias": alias.name,
                    "vacuity": vacuity_verdict,
                    "tier": alias.tier,
                })
            except Exception as exc:
                _log.warning("alias provenance write failed: %s", exc)

    def lookup(self, name: str) -> Alias:
        if name not in self._entries:
            raise UnknownAliasError(name)
        entries = self._entries[name]
        return sorted(entries, key=lambda a: _TIER_ORDER[a.tier])[0]

    def resolve(self, expr: Any, _seen: set[str] | None = None) -> Any:
        seen = _seen or set()

        if isinstance(expr, AliasRef):
            if expr.name in seen:
                raise AliasCycleError(f"alias cycle detected: {expr.name} -> ... -> {expr.name}")
            alias = self.lookup(expr.name)
            missing = [p for p in alias.params if p not in expr.args]
            if missing:
                raise ValueError(
                    f"alias '{expr.name}' missing required args: {missing}"
                )
            substituted = _substitute_params(alias.expr, expr.args)
            if isinstance(substituted, dict):
                substituted = parse_expression(substituted)
            return self.resolve(substituted, seen | {expr.name})

        if isinstance(expr, And):
            return And(children=[self.resolve(c, seen) for c in expr.children])
        if isinstance(expr, Or):
            return Or(children=[self.resolve(c, seen) for c in expr.children])
        if isinstance(expr, Not):
            return Not(child=self.resolve(expr.child, seen))
        if isinstance(expr, Implies):
            return Implies(a=self.resolve(expr.a, seen), b=self.resolve(expr.b, seen))
        if isinstance(expr, ForallSteps):
            return ForallSteps(pred=self.resolve(expr.pred, seen))
        if isinstance(expr, ExistsStep):
            return ExistsStep(pred=self.resolve(expr.pred, seen))
        if isinstance(expr, ForallOutputs):
            return ForallOutputs(pred=self.resolve(expr.pred, seen))
        return expr


__all__ = [
    "Alias",
    "AliasCycleError",
    "AliasRegistry",
    "Tier",
    "UnknownAliasError",
    "VacuousAliasError",
]
