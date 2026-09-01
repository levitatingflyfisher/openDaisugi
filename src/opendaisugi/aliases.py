"""Alias registration and resolution (v0.9.0).

An alias is a named, parameterizable predicate expression. Three tiers:

    - ``system``    shipped with opendaisugi, immutable
    - ``household`` workspace-shared, authored by agents or operators
    - ``envelope``  private to a single envelope

Resolution: lookup by name picks the highest-precedence tier
(envelope > household). A system alias's name cannot be registered again
at any tier, so no lower tier can redefine a system word. Parameter
substitution walks the expression tree once: a value equal to
``$<param>`` becomes the argument itself, a ``$<param>`` inside a string
becomes its text, and inside a regex field its escaped text.

Static check at registration time: the alias expression must reference
at least one plan path (via Equals/NotEquals/InSet/Matches/.../Exists
path field) - catches trivial tautologies. Full counterexample-based
vacuity check (tautology/contradiction) runs via Z3 as of v0.27.0.
"""

from __future__ import annotations

import re
from typing import Any, Literal

from pydantic import BaseModel, Field, ValidationError

from opendaisugi.predicate import (
    AliasRef,
    And,
    ExistsStep,
    ForallOutputs,
    ForallSteps,
    ForallWrites,
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


_PATH_OPS = frozenset(
    {
        "equals",
        "not_equals",
        "in_set",
        "not_in_set",
        "matches",
        "not_matches",
        "numeric_range",
        "exists",
        "is_empty",
        "depends_on",
        "before",
        "alias",
        "llm_check",
    }
)


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
    if op in ("forall_steps", "exists_step", "forall_outputs", "forall_writes"):
        return _references_a_path(d.get("pred"))
    return False


_REGEX_FIELDS = frozenset({"regex"})


def _placeholder_re(args: dict[str, Any]) -> re.Pattern[str] | None:
    """One pattern for every ``$name``, longest name first, so ``$p`` never
    takes the front of ``$p_name``."""
    if not args:
        return None
    names = sorted(args, key=len, reverse=True)
    return re.compile(r"\$(" + "|".join(re.escape(n) for n in names) + ")")


def _substitute_params(expr: Any, args: dict[str, Any]) -> Any:
    """Substitute placeholders and return a raw (dict/list/scalar) form.

    Pydantic models are expanded to dicts. The result is always plain data,
    so it can carry typed values through Pydantic-unsafe placeholders like
    `$max_scale` being spliced into a NumericRange.max float field.

    Substitution is one pass over the structure. Each string is scanned
    once, and text an argument puts in is never scanned again, so a value
    that holds ``$other`` stays as it is. Names are tried longest first, so
    ``$principal`` cannot take the front of ``$principal_name``. A value
    put into a regex field is escaped, so it matches only itself and can
    never widen the pattern.
    """
    return _substitute(expr, args, _placeholder_re(args), in_regex=False)


def _substitute(
    expr: Any, args: dict[str, Any], pattern: re.Pattern[str] | None, *, in_regex: bool
) -> Any:
    if isinstance(expr, str):
        if pattern is None:
            return expr
        if expr.startswith("$") and expr[1:] in args and not in_regex:
            return args[expr[1:]]
        if in_regex:
            return pattern.sub(lambda m: re.escape(str(args[m.group(1)])), expr)
        return pattern.sub(lambda m: str(args[m.group(1)]), expr)

    if isinstance(expr, list):
        return [_substitute(x, args, pattern, in_regex=in_regex) for x in expr]

    if hasattr(expr, "model_dump"):
        return _substitute(expr.model_dump(), args, pattern, in_regex=in_regex)

    if isinstance(expr, dict):
        return {
            k: _substitute(v, args, pattern, in_regex=k in _REGEX_FIELDS) for k, v in expr.items()
        }

    return expr


def _has_placeholder(expr: Any, params: list[str]) -> bool:
    """True when a string anywhere in ``expr`` names one of ``params``."""
    pattern = _placeholder_re(dict.fromkeys(params))
    if pattern is None:
        return False

    def walk(x: Any) -> bool:
        if isinstance(x, str):
            return pattern.search(x) is not None
        if isinstance(x, dict):
            return any(walk(v) for v in x.values())
        if isinstance(x, list):
            return any(walk(v) for v in x)
        return False

    return walk(_as_dict(expr) if hasattr(expr, "model_dump") else expr)


def _names_an_alias(expr: Any) -> bool:
    """True when ``expr`` holds an alias reference at any depth."""
    d = _as_dict(expr)
    if d is not None:
        if d.get("op") == "alias":
            return True
        return any(_names_an_alias(v) for v in d.values())
    if isinstance(expr, list):
        return any(_names_an_alias(v) for v in expr)
    return False


class AliasRegistry:
    """Tiered registry of named aliases."""

    def __init__(self, *, refinement_sink: Any = None) -> None:
        self._entries: dict[str, list[Alias]] = {}
        self._refinement_sink = refinement_sink

    def __contains__(self, name: str) -> bool:
        return name in self._entries

    def register(self, alias: Alias) -> None:
        """Add an alias, or raise and add nothing.

        A system alias's name is taken for good: no alias of any tier may
        share it, in either order, so no lower tier can redefine a system
        word to mean less. An error in the vacuity check refuses the
        register, as a vacuous alias does.
        """
        taken = self._entries.get(alias.name, [])
        if taken and (alias.tier == "system" or any(a.tier == "system" for a in taken)):
            raise ValueError(
                f"alias '{alias.name}' is a system alias name; a {alias.tier} alias "
                "cannot share it, since it would redefine what the system word means"
            )
        if not _references_a_path(alias.expr):
            raise ValueError(
                f"alias '{alias.name}' has no plan-path reference (looks vacuous); "
                "static check requires at least one Equals/NotEquals/Matches/... on a path"
            )
        vacuity_verdict = self._vacuity(alias)
        self._entries.setdefault(alias.name, []).append(alias)
        # v0.27.0: emit provenance to the refinement sink (fail-soft — never crashes register).
        if self._refinement_sink is not None:
            import logging

            _log = logging.getLogger("opendaisugi.aliases")
            try:
                self._refinement_sink.write_provenance(
                    {
                        "alias": alias.name,
                        "vacuity": vacuity_verdict,
                        "tier": alias.tier,
                    }
                )
            except Exception as exc:
                _log.warning("alias provenance write failed: %s", exc)

    @staticmethod
    def _vacuity(alias: Alias) -> str:
        """The Z3 vacuity verdict for an alias body, or ``deferred``.

        Tautologies and contradictions are refused. A body with a typed
        field that holds a placeholder, such as ``max: $max_scale``, parses
        only once its argument is bound, so its check is deferred. Any other
        error refuses the register. A body that names another alias is
        deferred too: what it means depends on that alias, which may not
        be registered yet, and each alias is checked when it registers.
        """
        from opendaisugi.vacuity import check_vacuity

        if _names_an_alias(alias.expr):
            return "deferred"
        try:
            expr = parse_expression(alias.expr) if isinstance(alias.expr, dict) else alias.expr
        except ValidationError as exc:
            if alias.params and _has_placeholder(alias.expr, alias.params):
                return "deferred"
            raise ValueError(f"alias '{alias.name}' body is not a valid predicate: {exc}") from exc
        try:
            verdict = check_vacuity(expr)
        except Exception as exc:  # noqa: BLE001 - an unjudged alias is refused
            raise ValueError(
                f"alias '{alias.name}' could not be checked for vacuity, so it is "
                f"not registered: {exc}"
            ) from exc
        if verdict in ("tautology", "contradiction"):
            raise VacuousAliasError(
                f"alias '{alias.name}' is {verdict} (constrains nothing / never satisfiable); "
                "the predicate must be non-trivial to be registered"
            )
        return verdict

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
                raise ValueError(f"alias '{expr.name}' missing required args: {missing}")
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
        if isinstance(expr, ForallWrites):
            return ForallWrites(pred=self.resolve(expr.pred, seen))
        return expr


__all__ = [
    "Alias",
    "AliasCycleError",
    "AliasRegistry",
    "Tier",
    "UnknownAliasError",
    "VacuousAliasError",
]
