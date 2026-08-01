"""F-3 (found by the TS client port): NotEquals with a numeric value.

predicate_z3._compile_scalar's NotEquals branch always resolved a Z3 String
variable, unlike Equals/InSet/NotInSet which branch numeric-vs-string. A
numeric not_equals therefore raised a Z3 sort mismatch: swallowed as
"non_trivial" in the vacuity path, but propagating all the way out of
verify() on the skill-subsumption path. These tests pin the fixed behavior.
"""

import pytest
import z3

from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, SkillStep, TaskStep
from opendaisugi.predicate import NotEquals, parse_expression
from opendaisugi.predicate_z3 import compile_to_z3
from opendaisugi.verify import verify


def _plan_env():
    plan = ActionPlan(source="test", task="t", steps=[TaskStep(id="s1", prompt="d")])
    env = Envelope(generated_by="test", task="t", stakes="low", permissions=Permission())
    return plan, env


def test_numeric_not_equals_compiles_to_a_numeric_constraint():
    plan, env = _plan_env()
    compiled = compile_to_z3(NotEquals(path="step.velocity_scale", value=3), plan, env)
    # A real arithmetic constraint, not a String-sorted comparison and not a crash.
    assert compiled.term is not None
    solver = z3.Solver()
    solver.add(compiled.term)
    assert solver.check() in (z3.sat, z3.unsat)


def test_numeric_not_equals_matches_equals_typing():
    plan, env = _plan_env()
    eq = compile_to_z3(parse_expression({"op": "equals", "path": "step.n", "value": 3}), plan, env)
    ne = compile_to_z3(
        parse_expression({"op": "not_equals", "path": "step.n", "value": 3}), plan, env
    )
    # Same path, same value: both sides must have picked the same (numeric) sort,
    # so conjoining them must solve rather than raising a Z3 sort mismatch.
    solver = z3.Solver()
    solver.add(eq.term, ne.term)
    assert solver.check() in (z3.sat, z3.unsat)


def test_skill_subsumption_with_numeric_not_equals_never_raises():
    contract_env = Envelope(
        generated_by="test",
        task="sub",
        stakes="low",
        permissions=Permission(),
        invariants=[
            Invariant(
                type="custom_bound",
                description="numeric not_equals in a skill contract",
                expr={"op": "not_equals", "path": "step.retries", "value": 3},
            )
        ],
    )
    plan = ActionPlan(
        source="test",
        task="t",
        steps=[SkillStep(id="k1", skill_id="sk", contract_envelope=contract_env)],
    )
    caller = Envelope(
        generated_by="test", task="t", stakes="low", permissions=Permission()
    )
    result = verify(plan, caller)  # must return a result, never raise Z3ExceptionObj
    assert result is not None
