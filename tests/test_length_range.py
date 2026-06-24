"""LengthRange predicate operator tests (v0.15.0)."""

from __future__ import annotations

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import ForallSteps, LengthRange, parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate, verify_predicate_z3


def _plan(body: str) -> ActionPlan:
    return ActionPlan(
        source="t",
        task="t",
        steps=[
            ShellStep(
                id="s1",
                command="echo hi",
                metadata={"body": body},
            )
        ],
    )


def _env() -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


def test_parse_length_range_minimal():
    expr = parse_expression({"op": "length_range", "path": "x", "min": 1})
    assert isinstance(expr, LengthRange)
    assert expr.min == 1
    assert expr.max is None


def test_parse_length_range_full():
    expr = parse_expression({
        "op": "length_range", "path": "x", "min": 10, "max": 100,
    })
    assert isinstance(expr, LengthRange)
    assert (expr.min, expr.max) == (10, 100)


def test_eval_string_within_bounds():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", min=2, max=10))
    assert evaluate_predicate(expr, _plan("hello"), _env()) is True


def test_eval_string_below_min():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", min=10))
    assert evaluate_predicate(expr, _plan("hi"), _env()) is False


def test_eval_string_above_max():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", max=3))
    assert evaluate_predicate(expr, _plan("too long"), _env()) is False


def test_eval_open_ended_upper():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", min=1))
    assert evaluate_predicate(expr, _plan("x"), _env()) is True
    assert evaluate_predicate(expr, _plan(""), _env()) is False


def test_eval_missing_path_is_false():
    expr = ForallSteps(pred=LengthRange(path="metadata.absent", min=0, max=100))
    assert evaluate_predicate(expr, _plan("hi"), _env()) is False


def test_eval_list_length():
    plan = ActionPlan(
        source="t",
        task="t",
        steps=[
            ShellStep(
                id="s1",
                command="echo hi",
                metadata={"tags": ["a", "b", "c"]},
            )
        ],
    )
    expr = ForallSteps(pred=LengthRange(path="metadata.tags", min=2, max=5))
    assert evaluate_predicate(expr, plan, _env()) is True
    expr_tight = ForallSteps(pred=LengthRange(path="metadata.tags", max=2))
    assert evaluate_predicate(expr_tight, plan, _env()) is False


def test_z3_verify_string_in_bounds():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", min=1, max=10))
    ok, ce = verify_predicate_z3(expr, _plan("hello"), _env())
    assert ok is True and ce is None


def test_z3_verify_string_too_short_produces_counterexample():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", min=10))
    ok, ce = verify_predicate_z3(expr, _plan("hi"), _env())
    assert ok is False
    assert ce is not None


def test_z3_verify_string_too_long_produces_counterexample():
    expr = ForallSteps(pred=LengthRange(path="metadata.body", max=3))
    ok, ce = verify_predicate_z3(expr, _plan("definitely way too long"), _env())
    assert ok is False
    assert ce is not None


def test_length_range_as_json_roundtrip():
    expr = LengthRange(path="metadata.body", min=10, max=5000)
    data = expr.model_dump()
    assert data == {"op": "length_range", "path": "metadata.body", "min": 10, "max": 5000}
    back = parse_expression(data)
    assert isinstance(back, LengthRange)
    assert (back.min, back.max) == (10, 5000)
