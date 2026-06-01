"""Tests for the AliasRegistry (v0.9.0)."""

from __future__ import annotations

import pytest

from opendaisugi.aliases import (
    Alias,
    AliasCycleError,
    AliasRegistry,
    UnknownAliasError,
    VacuousAliasError,
)
from opendaisugi.predicate import Equals, Implies, parse_expression


def test_register_and_lookup_roundtrip():
    reg = AliasRegistry()
    reg.register(Alias(
        name="type_is",
        params=["value"],
        expr=parse_expression({"op": "equals", "path": "type", "value": "$value"}),
        tier="envelope",
    ))
    assert "type_is" in reg


def test_resolve_substitutes_params():
    reg = AliasRegistry()
    reg.register(Alias(
        name="type_is",
        params=["value"],
        expr=parse_expression({"op": "equals", "path": "type", "value": "$value"}),
        tier="envelope",
    ))
    ref = parse_expression({"op": "alias", "name": "type_is", "args": {"value": "shell"}})
    resolved = reg.resolve(ref)
    assert isinstance(resolved, Equals)
    assert resolved.value == "shell"


def test_resolve_nested_alias_reference():
    reg = AliasRegistry()
    reg.register(Alias(
        name="is_email",
        params=[],
        expr=parse_expression({"op": "equals", "path": "type", "value": "email_send"}),
        tier="system",
    ))
    reg.register(Alias(
        name="never_impersonates",
        params=["principal"],
        expr=parse_expression({
            "op": "implies",
            "a": {"op": "alias", "name": "is_email", "args": {}},
            "b": {"op": "not_equals", "path": "metadata.signature", "value": "$principal"},
        }),
        tier="household",
    ))
    ref = parse_expression({"op": "alias", "name": "never_impersonates", "args": {"principal": "Ada"}})
    resolved = reg.resolve(ref)
    assert isinstance(resolved, Implies)
    assert isinstance(resolved.a, Equals)
    assert resolved.a.value == "email_send"
    assert resolved.b.value == "Ada"


def test_substitute_params_longest_key_wins():
    """v0.28.3 regression: pre-fix, ``$principal`` could munge
    ``$principal_name`` because dict-iteration order decided substitution
    sequence. Longest-key-first ordering closes this."""
    reg = AliasRegistry()
    reg.register(Alias(
        name="checks_two_principals",
        params=["principal", "principal_name"],
        expr=parse_expression({
            "op": "implies",
            "a": {"op": "equals", "path": "metadata.actor", "value": "$principal"},
            "b": {"op": "equals", "path": "metadata.display", "value": "$principal_name"},
        }),
        tier="envelope",
    ))
    ref = parse_expression({
        "op": "alias", "name": "checks_two_principals",
        "args": {"principal": "alice", "principal_name": "Alice Smith"},
    })
    resolved = reg.resolve(ref)
    assert isinstance(resolved, Implies)
    assert resolved.a.value == "alice"
    # Pre-fix: this assertion failed with value="alice_name" because
    # `$principal` replaced the prefix of `$principal_name` first.
    assert resolved.b.value == "Alice Smith"


def test_cycle_detection_raises():
    reg = AliasRegistry()
    reg.register(Alias(
        name="a",
        params=[],
        expr=parse_expression({"op": "alias", "name": "b", "args": {}}),
        tier="envelope",
    ))
    reg.register(Alias(
        name="b",
        params=[],
        expr=parse_expression({"op": "alias", "name": "a", "args": {}}),
        tier="envelope",
    ))
    ref = parse_expression({"op": "alias", "name": "a", "args": {}})
    with pytest.raises(AliasCycleError):
        reg.resolve(ref)


def test_unknown_alias_raises():
    reg = AliasRegistry()
    ref = parse_expression({"op": "alias", "name": "nope", "args": {}})
    with pytest.raises(UnknownAliasError):
        reg.resolve(ref)


def test_tier_precedence_envelope_overrides_household():
    reg = AliasRegistry()
    reg.register(Alias(
        name="shared",
        params=[],
        expr=parse_expression({"op": "equals", "path": "type", "value": "household"}),
        tier="household",
    ))
    reg.register(Alias(
        name="shared",
        params=[],
        expr=parse_expression({"op": "equals", "path": "type", "value": "envelope"}),
        tier="envelope",
    ))
    ref = parse_expression({"op": "alias", "name": "shared", "args": {}})
    resolved = reg.resolve(ref)
    assert isinstance(resolved, Equals)
    assert resolved.value == "envelope"


def test_static_check_rejects_tautological_path_reference():
    """v0.27.0: Z3 vacuity check rejects tautological aliases even if they reference a path."""
    reg = AliasRegistry()
    tautology = Alias(
        name="always",
        params=[],
        expr=parse_expression({
            "op": "or",
            "children": [
                {"op": "equals", "path": "nonexistent_but_static", "value": "a"},
                {"op": "not_equals", "path": "nonexistent_but_static", "value": "a"},
            ],
        }),
        tier="envelope",
    )
    with pytest.raises(VacuousAliasError):
        reg.register(tautology)


def test_static_check_rejects_no_plan_path_reference():
    reg = AliasRegistry()
    bad = Alias(
        name="vacuous",
        params=[],
        expr=parse_expression({
            "op": "and",
            "children": [],
        }),
        tier="envelope",
    )
    with pytest.raises(ValueError, match="no plan-path"):
        reg.register(bad)
