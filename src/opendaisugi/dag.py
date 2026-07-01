"""DAG construction and structural checks for action plans.

Uses NetworkX for cycle detection and missing-dependency checks. Multi-root
plans are permitted — independent dependency chains under the same envelope
are valid. All checks are sync and pure — no I/O.
"""

from __future__ import annotations

import networkx as nx

from opendaisugi.models import ActionPlan, Violation


def _build_graph(plan: ActionPlan) -> nx.DiGraph:
    g = nx.DiGraph()
    for step in plan.steps:
        g.add_node(step.id)
    for step in plan.steps:
        for dep in step.depends_on:
            # Edge: dep -> step (dep must run before step)
            g.add_edge(dep, step.id)
    return g


def check_dag(plan: ActionPlan) -> list[Violation]:
    """Run all DAG structural checks on a plan. Returns a list of violations."""
    violations: list[Violation] = []

    # Duplicate step ids — the graph node set and topological_order's step_by_id
    # dict both collapse duplicates, so only one of two same-id steps executes,
    # and the set-based integrity check can't see the dropped step. Reject up front.
    seen: set[str] = set()
    dupes: list[str] = []
    for step in plan.steps:
        if step.id in seen and step.id not in dupes:
            dupes.append(step.id)
        seen.add(step.id)
    for dup in dupes:
        violations.append(Violation(
            stage="dag",
            message=f"duplicate step id '{dup}' — step ids must be unique",
            detail={"step": dup},
        ))
    if violations:
        return violations  # graph checks below are meaningless with duplicate ids

    # Missing dependency detection — must run before building the graph,
    # since nx.add_edge silently creates missing nodes.
    step_ids = {s.id for s in plan.steps}
    for step in plan.steps:
        for dep in step.depends_on:
            if dep not in step_ids:
                violations.append(
                    Violation(
                        stage="dag",
                        message=f"Step '{step.id}' depends on unknown step '{dep}'",
                        detail={"step": step.id, "missing_dep": dep},
                    )
                )

    # If any missing deps, skip the cycle check (the graph is structurally broken).
    if violations:
        return violations

    g = _build_graph(plan)

    # Cycle detection
    try:
        cycle = nx.find_cycle(g, orientation="original")
        cycle_nodes = [edge[0] for edge in cycle]
        violations.append(
            Violation(
                stage="dag",
                message=f"Plan contains a cycle: {' -> '.join(cycle_nodes)}",
                detail={"cycle": cycle_nodes},
            )
        )
    except nx.NetworkXNoCycle:
        pass

    return violations


def topological_order(plan: ActionPlan) -> list:
    """Return plan steps in dependency-respecting order.

    Precondition: the plan has been verified (no cycles, no missing deps).
    Raises ``ValueError`` if the plan has a cycle.
    """
    step_by_id = {s.id: s for s in plan.steps}
    g = _build_graph(plan)
    try:
        ordered_ids = list(nx.topological_sort(g))
    except nx.NetworkXUnfeasible as e:
        raise ValueError(
            "Plan has a cycle; run verify(plan, envelope) before supervising"
        ) from e
    return [step_by_id[i] for i in ordered_ids]
