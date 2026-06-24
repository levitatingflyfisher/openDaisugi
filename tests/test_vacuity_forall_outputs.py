"""v0.27.0 fixup — check_vacuity must strip ForallOutputs like the other
quantifiers. Previously a ForallOutputs expression fell through to _compile_scalar
and raised, so vacuity detection was a silent no-op for output predicates.
"""
from __future__ import annotations

from opendaisugi.predicate import parse_expression
from opendaisugi.vacuity import check_vacuity


def _fo(pred):
    return parse_expression({"op": "forall_outputs", "pred": pred})


def test_forall_outputs_tautology_detected():
    expr = _fo({"op": "or", "children": [
        {"op": "equals", "path": "x", "value": "a"},
        {"op": "not_equals", "path": "x", "value": "a"}]})
    assert check_vacuity(expr) == "tautology"


def test_forall_outputs_contradiction_detected():
    expr = _fo({"op": "and", "children": [
        {"op": "equals", "path": "x", "value": "a"},
        {"op": "not_equals", "path": "x", "value": "a"}]})
    assert check_vacuity(expr) == "contradiction"


def test_forall_outputs_nontrivial():
    expr = _fo({"op": "equals", "path": "x", "value": "a"})
    assert check_vacuity(expr) == "non_trivial"
