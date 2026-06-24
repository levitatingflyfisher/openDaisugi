"""v0.27.0 fixup — alias registration must vacuity-check dict-form expressions.

register() called check_vacuity(alias.expr) directly; when expr is a raw dict
(Alias.expr is typed Any and not coerced), _compile_scalar raised and the broad
except swallowed it, so a tautological/contradictory dict-form alias registered
silently — defeating the documented "reject tautological aliases" invariant.
"""
from __future__ import annotations

import pytest

from opendaisugi.aliases import Alias, AliasRegistry, VacuousAliasError


def _taut_dict():
    return {"op": "or", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]}


def _contradiction_dict():
    return {"op": "and", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]}


def test_dict_form_tautology_rejected_at_registration():
    reg = AliasRegistry()
    with pytest.raises(VacuousAliasError):
        reg.register(Alias(name="useless", expr=_taut_dict(), tier="household"))


def test_dict_form_contradiction_rejected_at_registration():
    reg = AliasRegistry()
    with pytest.raises(VacuousAliasError):
        reg.register(Alias(name="impossible", expr=_contradiction_dict(), tier="household"))


def test_dict_form_nontrivial_still_registers():
    reg = AliasRegistry()
    reg.register(Alias(name="real", tier="household",
                       expr={"op": "equals", "path": "command", "value": "ls"}))
    assert "real" in reg
