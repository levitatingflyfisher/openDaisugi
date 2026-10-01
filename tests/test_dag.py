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
    plan = _plan(
        [
            ShellStep(id="s1", command="echo 1"),
            ShellStep(id="s2", command="echo 2", depends_on=["s1"]),
            ShellStep(id="s3", command="echo 3", depends_on=["s2"]),
        ]
    )
    assert check_dag(plan) == []


def test_dag_detects_simple_cycle():
    plan = _plan(
        [
            ShellStep(id="s1", command="echo 1", depends_on=["s2"]),
            ShellStep(id="s2", command="echo 2", depends_on=["s1"]),
        ]
    )
    violations = check_dag(plan)
    assert len(violations) == 1
    assert violations[0].stage == "dag"
    assert "cycle" in violations[0].message.lower()


def test_dag_detects_longer_cycle():
    plan = _plan(
        [
            ShellStep(id="s1", command="a", depends_on=["s3"]),
            ShellStep(id="s2", command="b", depends_on=["s1"]),
            ShellStep(id="s3", command="c", depends_on=["s2"]),
        ]
    )
    violations = check_dag(plan)
    assert any("cycle" in v.message.lower() for v in violations)


# ----- Missing dependency detection -----


def test_dag_detects_missing_dependency():
    plan = _plan(
        [
            ShellStep(id="s1", command="a"),
            ShellStep(id="s2", command="b", depends_on=["s99"]),
        ]
    )
    violations = check_dag(plan)
    assert len(violations) == 1
    assert violations[0].stage == "dag"
    assert "s99" in violations[0].message


def test_dag_detects_multiple_missing_deps():
    plan = _plan(
        [
            ShellStep(id="s1", command="a", depends_on=["ghost1", "ghost2"]),
        ]
    )
    violations = check_dag(plan)
    # Should report both ghost1 and ghost2 (either in one violation or two).
    joined = " ".join(v.message for v in violations)
    assert "ghost1" in joined
    assert "ghost2" in joined


# ----- Orphan / reachability -----


def test_dag_orphan_step_detected():
    # s3 is disconnected from the s1->s2 subgraph
    plan = _plan(
        [
            ShellStep(id="s1", command="a"),
            ShellStep(id="s2", command="b", depends_on=["s1"]),
            ShellStep(id="s3", command="c", depends_on=["s_nonexistent"]),
        ]
    )
    violations = check_dag(plan)
    # At minimum, the missing dep should be flagged.
    joined = " ".join(v.message for v in violations)
    assert "s_nonexistent" in joined


def test_dag_multi_root_allowed():
    # Two independent chains — both reachable from their own roots. Allowed.
    plan = _plan(
        [
            ShellStep(id="a1", command="a"),
            ShellStep(id="a2", command="a2", depends_on=["a1"]),
            ShellStep(id="b1", command="b"),
            ShellStep(id="b2", command="b2", depends_on=["b1"]),
        ]
    )
    assert check_dag(plan) == []


def test_dag_empty_plan_is_valid():
    plan = _plan([])
    assert check_dag(plan) == []


def test_duplicate_step_ids_rejected():
    # EB-6: duplicate ids collapse in the graph → a step silently doesn't run and
    # the set-based integrity check can't detect the drop. Reject at verify time.
    from opendaisugi.dag import check_dag
    from opendaisugi.models import ActionPlan, ShellStep

    plan = ActionPlan(
        source="t",
        task="x",
        steps=[
            ShellStep(id="dup", command="echo a"),
            ShellStep(id="dup", command="echo b"),  # same id
            ShellStep(id="ok", command="echo c"),
        ],
    )
    violations = check_dag(plan)
    assert any(v.stage == "dag" and "duplicate step id" in v.message for v in violations)


# ----- No networkx: the same answers networkx gave -----


def test_dag_imports_no_networkx():
    import inspect

    import opendaisugi.dag as dag

    src = inspect.getsource(dag)
    assert "import networkx" not in src and "from networkx" not in src


def _has_cycle(succ: dict[str, list[str]]) -> bool:
    """A plain three-colour DFS: True when it meets a back edge."""
    color = dict.fromkeys(succ, 0)

    def visit(n: str) -> bool:
        color[n] = 1
        for m in succ[n]:
            if color[m] == 1 or (color[m] == 0 and visit(m)):
                return True
        color[n] = 2
        return False

    return any(color[n] == 0 and visit(n) for n in succ)


def test_the_walks_hold_their_invariants_on_random_graphs():
    """Always runs, no networkx: on 5000 seeded random plans with
    self-loops and repeated edges, a cycle is named exactly when one
    exists and the named one is real; the levels cover every step once
    and put each edge's tail in an earlier level."""
    import random

    from opendaisugi.dag import _build_graph, _find_cycle, _generations

    rng = random.Random(20261009)
    for _ in range(5000):
        size = rng.randint(1, 9)
        ids = [f"s{i}" for i in range(size)]
        steps = []
        for sid in ids:
            deps = [rng.choice(ids) for _ in range(rng.randint(0, 3))]
            steps.append(ShellStep(id=sid, command="true", depends_on=deps))
        g = _build_graph(_plan(steps))
        edges = {(a, b) for a in g.succ for b in g.succ[a]}
        cycle = _find_cycle(g)
        assert (cycle is not None) == _has_cycle(g.succ), steps
        if cycle is not None:
            assert cycle
            for a, b in zip(cycle, cycle[1:] + cycle[:1], strict=True):
                assert (a, b) in edges, (cycle, steps)
            try:
                _generations(g)
            except ValueError:
                pass
            else:
                raise AssertionError(f"levels taken over a cycle: {cycle}")
            continue
        levels = _generations(g)
        flat = [n for lvl in levels for n in lvl]
        assert sorted(flat) == sorted(g.succ)
        rank = {n: i for i, lvl in enumerate(levels) for n in lvl}
        for a, b in edges:
            assert rank[a] < rank[b], (a, b, levels)
