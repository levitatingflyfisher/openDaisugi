"""Tests for predicate -> Z3 compilation / evaluation."""

from __future__ import annotations

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate


def _plan(*steps) -> ActionPlan:
    return ActionPlan(source="test", task="t", steps=list(steps))


def _envelope() -> Envelope:
    return Envelope(generated_by="test", task="t", permissions=Permission())


def test_equals_path_hits_step_field():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({"op": "forall_steps", "pred": {"op": "equals", "path": "type", "value": "shell"}})
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_equals_false_when_mismatch():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({"op": "forall_steps", "pred": {"op": "equals", "path": "type", "value": "file_read"}})
    assert evaluate_predicate(expr, plan, _envelope()) is False


def test_not_equals():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({"op": "forall_steps", "pred": {"op": "not_equals", "path": "type", "value": "file_read"}})
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_and_conjunction():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {
            "op": "and",
            "children": [
                {"op": "equals", "path": "type", "value": "shell"},
                {"op": "equals", "path": "command", "value": "ls"},
            ],
        },
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_or_disjunction():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {
            "op": "or",
            "children": [
                {"op": "equals", "path": "type", "value": "shell"},
                {"op": "equals", "path": "type", "value": "file_read"},
            ],
        },
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_not_inverts():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {
            "op": "not",
            "child": {"op": "equals", "path": "type", "value": "file_read"},
        },
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_implies_true_when_antecedent_false():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {
            "op": "implies",
            "a": {"op": "equals", "path": "type", "value": "file_read"},
            "b": {"op": "equals", "path": "type", "value": "anything"},
        },
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_exists_step_matches_at_least_one():
    from opendaisugi.models import FileReadStep
    plan = _plan(
        ShellStep(id="s1", command="ls"),
        FileReadStep(id="s2", path="/tmp/x"),
    )
    expr = parse_expression({
        "op": "exists_step",
        "pred": {"op": "equals", "path": "type", "value": "file_read"},
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_in_set_membership():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "in_set", "path": "type", "values": ["shell", "file_read"]},
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_matches_regex():
    plan = _plan(ShellStep(id="s1", command="ls -la /tmp"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "matches", "path": "command", "regex": r"^ls"},
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_numeric_range_inclusive():
    from opendaisugi.models import JointMoveStep
    plan = _plan(JointMoveStep(id="j1", joint_targets={"a": 0.5}, duration_s=2.0))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "numeric_range", "path": "duration_s", "min": 1.0, "max": 3.0},
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_exists_path_true_when_field_present():
    plan = _plan(ShellStep(id="s1", command="ls"))
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "exists", "path": "command"},
    })
    assert evaluate_predicate(expr, plan, _envelope()) is True


def test_unknown_op_raises_known_error():
    class Bogus:
        op = "xyz_nonsense"
        path = "type"

    plan = _plan(ShellStep(id="s1", command="ls"))
    with pytest.raises(ValueError, match="unknown predicate op"):
        evaluate_predicate(Bogus(), plan, _envelope())
