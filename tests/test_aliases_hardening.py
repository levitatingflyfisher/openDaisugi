"""Alias substitution, system-word protection and register-time errors.

Three holes, each of which let an alias mean less than it says:

- Substitution spliced each argument into strings one name after
  another, so a value holding ``$other`` was rewritten again by the next
  name, and a value spliced into a regex field became regex syntax.
- A household or envelope alias with a system alias's name shadowed it,
  so ``no_secrets`` could be redefined to forbid almost nothing.
- An error in the register-time vacuity check was swallowed, so an alias
  the check could not judge registered as if it had passed.
"""

from __future__ import annotations

import re

import pytest

from opendaisugi.aliases import Alias, AliasRegistry, VacuousAliasError
from opendaisugi.predicate import Matches, NotMatches, parse_expression
from opendaisugi.system_aliases import load_system_aliases

# --- (a) one structural pass ------------------------------------------------


def _two_param_registry() -> AliasRegistry:
    reg = AliasRegistry()
    reg.register(
        Alias(
            name="two",
            params=["a", "b"],
            expr={"op": "equals", "path": "metadata.x", "value": "$a and $b"},
            tier="envelope",
        )
    )
    return reg


def test_a_value_holding_another_placeholder_is_not_rewritten():
    reg = _two_param_registry()
    ref = parse_expression({"op": "alias", "name": "two", "args": {"a": "$b", "b": "zzz"}})
    assert reg.resolve(ref).value == "$b and zzz"


def test_a_value_holding_a_longer_placeholder_is_not_rewritten():
    reg = AliasRegistry()
    reg.register(
        Alias(
            name="pn",
            params=["p", "p_name"],
            expr={"op": "equals", "path": "metadata.x", "value": "$p/$p_name"},
            tier="envelope",
        )
    )
    ref = parse_expression({"op": "alias", "name": "pn", "args": {"p": "$p_name", "p_name": "Q"}})
    assert reg.resolve(ref).value == "$p_name/Q"


def test_a_value_spliced_into_a_regex_is_a_literal():
    """never_impersonates splices the principal into a regex. A name made
    of regex syntax must match only itself, not widen the pattern."""
    reg = AliasRegistry()
    load_system_aliases(reg)
    ref = parse_expression(
        {"op": "alias", "name": "never_impersonates", "args": {"principal": ".*"}}
    )
    resolved = reg.resolve(ref)
    regexes = [c.regex for c in resolved.pred.b.children if isinstance(c, NotMatches)]
    assert len(regexes) == 1
    pattern = re.compile(regexes[0])
    assert pattern.search("signed - .*")
    assert not pattern.search("signed - Ada"), "the principal became regex syntax"


def test_a_whole_regex_placeholder_is_a_literal_too():
    reg = AliasRegistry()
    reg.register(
        Alias(
            name="body_has",
            params=["pat"],
            expr={"op": "matches", "path": "metadata.body", "regex": "$pat"},
            tier="envelope",
        )
    )
    ref = parse_expression({"op": "alias", "name": "body_has", "args": {"pat": "a|b"}})
    resolved = reg.resolve(ref)
    assert isinstance(resolved, Matches)
    assert re.compile(resolved.regex).fullmatch("a|b")
    assert not re.compile(resolved.regex).fullmatch("a")


def test_a_non_regex_field_keeps_the_value_as_given():
    reg = _two_param_registry()
    ref = parse_expression({"op": "alias", "name": "two", "args": {"a": "x.*", "b": "(y)"}})
    assert reg.resolve(ref).value == "x.* and (y)"


def test_a_whole_placeholder_keeps_its_type():
    reg = AliasRegistry()
    load_system_aliases(reg)
    ref = parse_expression(
        {"op": "alias", "name": "velocity_scale_bounded", "args": {"max_scale": 0.5}}
    )
    assert reg.resolve(ref).pred.b.max == 0.5


# --- (b) no lower tier redefines a system word ------------------------------

_WEAK = {"op": "not_matches", "path": "content", "regex": "never-in-any-real-text"}


@pytest.mark.parametrize("tier", ["household", "envelope"])
def test_a_lower_tier_cannot_redefine_a_system_alias(tier):
    reg = AliasRegistry()
    load_system_aliases(reg)
    with pytest.raises(ValueError, match="system alias"):
        reg.register(Alias(name="no_secrets", expr=_WEAK, tier=tier))
    resolved = reg.resolve(parse_expression({"op": "alias", "name": "no_secrets", "args": {}}))
    assert "AKIA" in resolved.model_dump_json()


@pytest.mark.parametrize("tier", ["household", "envelope"])
def test_a_system_alias_cannot_follow_a_lower_one_of_the_same_name(tier):
    """Order must not matter: a lower-tier alias registered first would
    otherwise audit the system word that follows it."""
    reg = AliasRegistry()
    reg.register(Alias(name="no_secrets", expr=_WEAK, tier=tier))
    with pytest.raises(ValueError, match="system alias"):
        load_system_aliases(reg)


def test_a_second_system_alias_of_the_same_name_is_refused():
    reg = AliasRegistry()
    load_system_aliases(reg)
    with pytest.raises(ValueError, match="system alias"):
        reg.register(Alias(name="no_secrets", expr=_WEAK, tier="system"))


def test_a_household_file_cannot_claim_the_system_tier(tmp_path):
    from opendaisugi.integrations.hermes import load_household_aliases

    f = tmp_path / "aliases.yaml"
    f.write_text(
        "aliases:\n"
        "  - name: family_word\n"
        "    tier: system\n"
        "    expr: {op: equals, path: type, value: shell}\n"
    )
    with pytest.raises(ValueError, match="tier"):
        load_household_aliases(f)


def test_envelope_still_overrides_household():
    reg = AliasRegistry()
    reg.register(
        Alias(name="shared", expr={"op": "equals", "path": "type", "value": "h"}, tier="household")
    )
    reg.register(
        Alias(name="shared", expr={"op": "equals", "path": "type", "value": "e"}, tier="envelope")
    )
    resolved = reg.resolve(parse_expression({"op": "alias", "name": "shared", "args": {}}))
    assert resolved.value == "e"


# --- (c) a vacuity check that errors refuses the register -------------------


def test_an_error_in_the_vacuity_check_refuses_the_register(monkeypatch):
    import opendaisugi.vacuity as vacuity

    def boom(expr, **kw):
        raise RuntimeError("solver broke")

    monkeypatch.setattr(vacuity, "check_vacuity", boom)
    reg = AliasRegistry()
    with pytest.raises(ValueError, match="solver broke"):
        reg.register(
            Alias(name="x", expr={"op": "equals", "path": "type", "value": "s"}, tier="household")
        )
    assert "x" not in reg


def test_a_body_that_does_not_parse_refuses_the_register():
    reg = AliasRegistry()
    with pytest.raises(ValueError):
        reg.register(
            Alias(
                name="bad",
                expr={"op": "numeric_range", "path": "n", "min": "low", "max": 3},
                tier="household",
            )
        )
    assert "bad" not in reg


def test_a_typed_placeholder_defers_the_check_and_registers():
    """velocity_scale_bounded holds $max_scale in a float field. Its body
    parses only once the argument is bound, so the check is deferred, not
    failed."""
    reg = AliasRegistry()
    load_system_aliases(reg)
    assert "velocity_scale_bounded" in reg


def test_a_vacuous_alias_is_still_refused():
    reg = AliasRegistry()
    taut = {
        "op": "or",
        "children": [
            {"op": "equals", "path": "type", "value": "s"},
            {"op": "not_equals", "path": "type", "value": "s"},
        ],
    }
    with pytest.raises(VacuousAliasError):
        reg.register(Alias(name="t", expr=taut, tier="household"))
