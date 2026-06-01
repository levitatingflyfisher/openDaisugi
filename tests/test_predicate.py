"""Tests for the predicate expression tree (v0.9.0)."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from opendaisugi.predicate import (
    And,
    Equals,
    Exists,
    ForallSteps,
    Implies,
    InSet,
    Matches,
    Not,
    NotEquals,
    NumericRange,
    Or,
    Expression,
    parse_expression,
)


def test_equals_parses_from_dict():
    expr = parse_expression({"op": "equals", "path": "type", "value": "email_send"})
    assert isinstance(expr, Equals)
    assert expr.path == "type"
    assert expr.value == "email_send"


def test_in_set_parses_list_of_values():
    expr = parse_expression({"op": "in_set", "path": "type", "values": ["shell", "file_read"]})
    assert isinstance(expr, InSet)
    assert expr.values == ["shell", "file_read"]


def test_matches_requires_regex():
    expr = parse_expression({"op": "matches", "path": "metadata.body", "regex": r"^Hello"})
    assert isinstance(expr, Matches)


def test_numeric_range_inclusive():
    expr = parse_expression({"op": "numeric_range", "path": "duration_s", "min": 0.0, "max": 10.0})
    assert isinstance(expr, NumericRange)
    assert expr.min == 0.0
    assert expr.max == 10.0


def test_exists_on_path():
    expr = parse_expression({"op": "exists", "path": "metadata.signature"})
    assert isinstance(expr, Exists)


def test_and_composes_children():
    expr = parse_expression({
        "op": "and",
        "children": [
            {"op": "equals", "path": "type", "value": "email_send"},
            {"op": "exists", "path": "metadata.body"},
        ],
    })
    assert isinstance(expr, And)
    assert len(expr.children) == 2


def test_or_composes_children():
    expr = parse_expression({
        "op": "or",
        "children": [
            {"op": "equals", "path": "type", "value": "shell"},
            {"op": "equals", "path": "type", "value": "file_read"},
        ],
    })
    assert isinstance(expr, Or)


def test_not_wraps_single_child():
    expr = parse_expression({
        "op": "not",
        "child": {"op": "equals", "path": "type", "value": "shell"},
    })
    assert isinstance(expr, Not)
    assert isinstance(expr.child, Equals)


def test_implies_two_children():
    expr = parse_expression({
        "op": "implies",
        "a": {"op": "equals", "path": "type", "value": "email_send"},
        "b": {"op": "not_equals", "path": "metadata.signature", "value": "Ada Lin"},
    })
    assert isinstance(expr, Implies)


def test_forall_steps_wraps_predicate():
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "equals", "path": "type", "value": "shell"},
    })
    assert isinstance(expr, ForallSteps)


def test_nested_round_trip_preserves_structure():
    payload = {
        "op": "forall_steps",
        "pred": {
            "op": "implies",
            "a": {"op": "equals", "path": "type", "value": "email_send"},
            "b": {"op": "not_equals", "path": "metadata.signature", "value": "Ada"},
        },
    }
    expr = parse_expression(payload)
    assert isinstance(expr, ForallSteps)
    assert isinstance(expr.pred, Implies)


def test_unknown_op_raises_validation_error():
    with pytest.raises((ValidationError, ValueError)):
        parse_expression({"op": "xyz_nonsense", "path": "type"})


def test_expression_union_accepts_any_primitive():
    parsed = parse_expression({"op": "equals", "path": "x", "value": 1})
    assert isinstance(parsed, Equals)
