"""Tests for opendaisugi.dag — cycle + reachability + missing-dep checks."""

from opendaisugi.dag import check_dag
from opendaisugi.models import ActionPlan, ActionStep, ShellStep


def _plan(steps: list[ActionStep]) -> ActionPlan:
    return ActionPlan(source="test", task="dag test", steps=steps)


# ----- Cycle detection -----


def test_dag_no_cycle_single_step():
    plan = _plan([ShellStep(id="s1", command="echo 1")])
    violations = check_dag(plan)
    assert violations == []


def test_dag_no_cycle_linear():
    plan = _plan([
        ShellStep(id="s1", command="echo 1"),
        ShellStep(id="s2", command="echo 2", depends_on=["s1"]),
        ShellStep(id="s3", command="echo 3", depends_on=["s2"]),
    ])
    assert check_dag(plan) == []


def test_dag_detects_simple_cycle():
    plan = _plan([
        ShellStep(id="s1", command="echo 1", depends_on=["s2"]),
        ShellStep(id="s2", command="echo 2", depends_on=["s1"]),
    ])
    violations = check_dag(plan)
    assert len(violations) == 1
    assert violations[0].stage == "dag"
    assert "cycle" in violations[0].message.lower()


def test_dag_detects_longer_cycle():
    plan = _plan([
        ShellStep(id="s1", command="a", depends_on=["s3"]),
        ShellStep(id="s2", command="b", depends_on=["s1"]),
        ShellStep(id="s3", command="c", depends_on=["s2"]),
    ])
    violations = check_dag(plan)
    assert any("cycle" in v.message.lower() for v in violations)


# ----- Missing dependency detection -----


def test_dag_detects_missing_dependency():
    plan = _plan([
        ShellStep(id="s1", command="a"),
        ShellStep(id="s2", command="b", depends_on=["s99"]),
    ])
    violations = check_dag(plan)
    assert len(violations) == 1
    assert violations[0].stage == "dag"
    assert "s99" in violations[0].message


def test_dag_detects_multiple_missing_deps():
    plan = _plan([
        ShellStep(id="s1", command="a", depends_on=["ghost1", "ghost2"]),
    ])
    violations = check_dag(plan)
    # Should report both ghost1 and ghost2 (either in one violation or two).
    joined = " ".join(v.message for v in violations)
    assert "ghost1" in joined
    assert "ghost2" in joined


# ----- Orphan / reachability -----


def test_dag_orphan_step_detected():
    # s3 is disconnected from the s1->s2 subgraph
    plan = _plan([
        ShellStep(id="s1", command="a"),
        ShellStep(id="s2", command="b", depends_on=["s1"]),
        ShellStep(id="s3", command="c", depends_on=["s_nonexistent"]),
    ])
    violations = check_dag(plan)
    # At minimum, the missing dep should be flagged.
    joined = " ".join(v.message for v in violations)
    assert "s_nonexistent" in joined


def test_dag_multi_root_allowed():
    # Two independent chains — both reachable from their own roots. Allowed.
    plan = _plan([
        ShellStep(id="a1", command="a"),
        ShellStep(id="a2", command="a2", depends_on=["a1"]),
        ShellStep(id="b1", command="b"),
        ShellStep(id="b2", command="b2", depends_on=["b1"]),
    ])
    assert check_dag(plan) == []


def test_dag_empty_plan_is_valid():
    plan = _plan([])
    assert check_dag(plan) == []
