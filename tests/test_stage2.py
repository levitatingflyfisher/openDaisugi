"""Tests for Stage 2 output verification (v0.9.0)."""

from __future__ import annotations

from opendaisugi.models import (
    Envelope,
    Permission,
    Postcondition,
    ShellStep,
)
from opendaisugi.predicate import parse_expression
from opendaisugi.stage2 import verify_completed_step


def _envelope_with_postcondition(expr) -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["*"]),
        postconditions=[
            Postcondition(type="body_safety", description="body rules", expr=expr),
        ],
    )


def test_completed_step_passing_postcondition():
    completed = ShellStep(
        id="s1",
        command="echo hi",
        metadata={"output": "hi"},
    )
    envelope = _envelope_with_postcondition(parse_expression({
        "op": "forall_steps",
        "pred": {"op": "not_matches", "path": "metadata.output", "regex": "ERROR"},
    }))
    violations = verify_completed_step(completed, envelope)
    assert violations == []


def test_completed_step_failing_postcondition():
    completed = ShellStep(
        id="s1",
        command="echo ERROR",
        metadata={"output": "Something ERROR happened"},
    )
    envelope = _envelope_with_postcondition(parse_expression({
        "op": "forall_steps",
        "pred": {"op": "not_matches", "path": "metadata.output", "regex": "ERROR"},
    }))
    violations = verify_completed_step(completed, envelope)
    assert len(violations) == 1
    assert "body_safety" in violations[0].message


def test_impersonation_detection():
    completed = ShellStep(
        id="s1",
        command="send_email",
        metadata={
            "type": "email_send",
            "signature": "Ada Lin",
            "body": "Hi editor, -Ada",
        },
    )
    envelope = _envelope_with_postcondition(parse_expression({
        "op": "forall_steps",
        "pred": {
            "op": "and",
            "children": [
                {"op": "not_equals", "path": "metadata.signature", "value": "Ada Lin"},
                {"op": "not_matches", "path": "metadata.body", "regex": "(?i)(\u2014|-)\\s*ada"},
            ],
        },
    }))
    violations = verify_completed_step(completed, envelope)
    assert len(violations) == 1


def test_postcondition_without_expr_is_skipped():
    completed = ShellStep(id="s1", command="echo hi", metadata={"output": "hi"})
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(),
        postconditions=[
            Postcondition(type="legacy", description="no expr", expr=None),
        ],
    )
    violations = verify_completed_step(completed, envelope)
    assert violations == []


def test_non_enforced_postcondition_skipped():
    completed = ShellStep(id="s1", command="echo ERROR", metadata={"output": "ERROR"})
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(),
        postconditions=[
            Postcondition(
                type="body_safety",
                description="docs only",
                expr=parse_expression({
                    "op": "forall_steps",
                    "pred": {"op": "not_matches", "path": "metadata.output", "regex": "ERROR"},
                }),
                enforce=False,
            ),
        ],
    )
    violations = verify_completed_step(completed, envelope)
    assert violations == []
