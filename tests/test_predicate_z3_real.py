"""Tests that ``compile_to_z3`` emits real Z3 expression trees (v0.11.0).

These are the tests that would have caught the v0.9 through v0.10.3 load-
bearing fake: ``compile_to_z3`` wrapping ``evaluate_predicate``'s Python
bool in ``z3.BoolVal``. Structural assertions here fail against the old
implementation and pass against the real one.
"""

from __future__ import annotations

import z3

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import (
    CompiledPredicate,
    compile_to_z3,
    verify_predicate_z3,
)


def _env():
    return Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo", "rm"]),
    )


def _forall_not_rm():
    return parse_expression({
        "op": "forall_steps",
        "pred": {"op": "not_matches", "path": "command", "regex": r"^rm "},
    })


def _has_inre(node: z3.ExprRef) -> bool:
    """Walk the AST and report whether any InRe term is present."""
    # Z3's AST navigation is via children(); use a DFS.
    stack = [node]
    while stack:
        n = stack.pop()
        decl = n.decl() if hasattr(n, "decl") else None
        if decl is not None and decl.name() == "str.in_re":
            return True
        if hasattr(n, "children"):
            stack.extend(n.children())
    return False


def test_compile_returns_real_boolref_not_boolval():
    """The load-bearing claim: compile_to_z3 no longer wraps Python eval."""
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    compiled = compile_to_z3(_forall_not_rm(), plan, _env())
    assert isinstance(compiled, CompiledPredicate)
    # Fails under the v0.10.0 fake (which returned BoolVal(True)).
    assert not z3.is_true(compiled.term)
    assert not z3.is_false(compiled.term)


def test_compiled_tree_contains_regex_operator():
    """The predicate ``not_matches command ^rm `` must lower to a real Z3
    regex InRe node, not to a Python-evaluated BoolVal."""
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    compiled = compile_to_z3(_forall_not_rm(), plan, _env())
    assert _has_inre(compiled.term), (
        "compile_to_z3 no longer contains InRe — regex predicate was not "
        "symbolically translated"
    )


def test_compiled_tree_declares_symbolic_string_variable():
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    compiled = compile_to_z3(_forall_not_rm(), plan, _env())
    # At least one declared String variable tied to the step's command field.
    assert any("step_0__command" in name for name in compiled.variables)


def test_verify_predicate_z3_passes_clean_plan():
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    ok, cex = verify_predicate_z3(_forall_not_rm(), plan, _env())
    assert ok is True
    assert cex is None


def test_verify_predicate_z3_catches_regex_violation():
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="rm -rf /")])
    ok, cex = verify_predicate_z3(_forall_not_rm(), plan, _env())
    assert ok is False
    assert cex is not None
    # Z3 returned the concrete command that falsified the predicate.
    recovered = cex.model_values["step_0__command"]
    assert recovered.startswith("rm ")


def test_llm_check_marked_as_soft_node():
    expr = parse_expression({"op": "llm_check", "rule": "body is professional"})
    plan = ActionPlan(source="t", task="t", steps=[])
    compiled = compile_to_z3(expr, plan, _env())
    assert len(compiled.soft_nodes) == 1
    # The soft node name becomes a Z3 Bool in the term.
    assert not z3.is_true(compiled.term)
    assert not z3.is_false(compiled.term)


def test_unsupported_regex_becomes_soft_node_not_silent_pass():
    """Regex the translator can't handle (e.g. ``(?i)``) falls back to a
    soft Z3 Bool — not silent True."""
    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "not_matches", "path": "metadata.body", "regex": r"(?i)hello"},
    })
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo", metadata={"body": "HELLO"})])
    compiled = compile_to_z3(expr, plan, _env())
    assert compiled.soft_nodes, "unsupported regex should register a soft node"


def test_compile_numeric_range_emits_real_constraints():
    """NumericRange on a per-step field compiles to real >= / <= over a
    Z3 Real variable — not a collapsed Python bool."""
    from opendaisugi.models import JointMoveStep

    expr = parse_expression({
        "op": "forall_steps",
        "pred": {"op": "numeric_range", "path": "velocity_scale", "min": 0.0, "max": 1.0},
    })
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="m1", joint_targets={"j1": 0.5}, duration_s=0.5, velocity_scale=0.5),
    ])
    compiled = compile_to_z3(expr, plan, _env())
    # Declares a Real variable for the step field.
    assert any("velocity_scale" in name for name in compiled.variables)
    # Not trivially collapsed.
    assert not z3.is_true(compiled.term)
    assert not z3.is_false(compiled.term)
