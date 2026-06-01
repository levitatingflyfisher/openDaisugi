"""Tests for the llm_check predicate primitive (v0.9.0)."""

from __future__ import annotations

from unittest.mock import patch

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate


def _plan():
    return ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo", metadata={"body": "Hi editor, thanks!"}),
    ])


def test_llm_check_passes_when_model_says_satisfied():
    with patch("opendaisugi.llm_check._invoke_model", return_value=(True, "looks clean")):
        expr = parse_expression({"op": "llm_check", "rule": "body is professional"})
        envelope = Envelope(
            generated_by="t",
            task="t",
            permissions=Permission(),
            stakes="low",
        )
        assert evaluate_predicate(expr, _plan(), envelope) is True


def test_llm_check_fails_when_model_says_unsatisfied():
    with patch("opendaisugi.llm_check._invoke_model", return_value=(False, "offensive content")):
        expr = parse_expression({"op": "llm_check", "rule": "body is professional"})
        envelope = Envelope(
            generated_by="t",
            task="t",
            permissions=Permission(),
            stakes="low",
        )
        assert evaluate_predicate(expr, _plan(), envelope) is False


def test_llm_check_blocked_on_physical_stakes():
    expr = parse_expression({"op": "llm_check", "rule": "body is professional"})
    envelope = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        stakes="physical",
    )
    with pytest.raises(ValueError, match="llm_check blocked for physical stakes"):
        evaluate_predicate(expr, _plan(), envelope)
