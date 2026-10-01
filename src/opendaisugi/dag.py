"""DAG construction and structural checks for action plans.

The graph and its three walks are our own code, written to give networkx's
answers: the cycle ``find_cycle`` names, ``topological_generations`` and
``topological_sort``. The Rust port (clients/rust/src/dag.rs) walks the same
way. Multi-root plans are permitted: independent dependency chains under the
same envelope are valid. All checks are sync and pure, with no I/O.
"""

from __future__ import annotations

from opendaisugi.models import ActionPlan, Violation

_CYCLE = "Plan has a cycle; run verify(plan, envelope) before supervising"


class _Graph:
    """A directed graph as networkx's DiGraph keeps it: nodes and each
    node's successors in insertion order, a repeated edge kept once."""

    def __init__(self) -> None:
        self.succ: dict[str, list[str]] = {}

    def add_node(self, n: str) -> None:
        self.succ.setdefault(n, [])

    def add_edge(self, a: str, b: str) -> None:
        self.add_node(a)
        self.add_node(b)
        if b not in self.succ[a]:
            self.succ[a].append(b)


def _build_graph(plan: ActionPlan) -> _Graph:
    g = _Graph()
    for step in plan.steps:
        g.add_node(step.id)
    for step in plan.steps:
        for dep in step.depends_on:
            # Edge: dep -> step (dep must run before step)
            g.add_edge(dep, step.id)
    return g


def _edge_dfs(g: _Graph, start: str) -> list[tuple[str, str]]:
    """``nx.edge_dfs`` from one start node: every edge once, in the order a
    depth-first walk over the successor lists meets it."""
    out: list[tuple[str, str]] = []
    visited: set[tuple[str, str]] = set()
    nxt: dict[str, int] = {}
    stack = [start]
    while stack:
        cur = stack[-1]
        k = nxt.get(cur, 0)
        targets = g.succ.get(cur, [])
        if k >= len(targets):
            stack.pop()
            continue
        to = targets[k]
        nxt[cur] = k + 1
        if (cur, to) not in visited:
            visited.add((cur, to))
            stack.append(to)
            out.append((cur, to))
    return out


def _find_cycle(g: _Graph) -> list[str] | None:
    """``nx.find_cycle(g, orientation="original")``: the nodes of the cycle
    networkx names, or None when there is none."""
    explored: set[str] = set()
    for start in g.succ:
        if start in explored:
            continue
        edges: list[tuple[str, str]] = []
        seen = {start}
        active = {start}
        prev_head: str | None = None
        for tail, head in _edge_dfs(g, start):
            if head in explored:
                continue
            if prev_head is not None and tail != prev_head:
                while True:
                    if not edges:
                        active = {tail}
                        break
                    popped = edges.pop()
                    active.discard(popped[1])
                    if edges and tail == edges[-1][1]:
                        break
            edges.append((tail, head))
            if head in active:
                i = next((j for j, e in enumerate(edges) if e[0] == head), 0)
                return [e[0] for e in edges[i:]]
            seen.add(head)
            active.add(head)
            prev_head = head
        explored |= seen
    return None


def _generations(g: _Graph) -> list[list[str]]:
    """``nx.topological_generations``. Raises ValueError on a cycle."""
    indegree: dict[str, int] = {n: 0 for n in g.succ}
    for n in g.succ:
        for child in g.succ[n]:
            indegree[child] += 1
    zero = [n for n, d in indegree.items() if d == 0]
    left = {n: d for n, d in indegree.items() if d > 0}
    out: list[list[str]] = []
    while zero:
        this, zero = zero, []
        for node in this:
            for child in g.succ[node]:
                left[child] -= 1
                if left[child] == 0:
                    zero.append(child)
                    del left[child]
        out.append(this)
    if left:
        raise ValueError(_CYCLE)
    return out


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
        violations.append(
            Violation(
                stage="dag",
                message=f"duplicate step id '{dup}' — step ids must be unique",
                detail={"step": dup},
            )
        )
    if violations:
        return violations  # graph checks below are meaningless with duplicate ids

    # Missing dependency detection: must run before building the graph,
    # since add_edge creates missing nodes.
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
    cycle_nodes = _find_cycle(g)
    if cycle_nodes is not None:
        violations.append(
            Violation(
                stage="dag",
                message=f"Plan contains a cycle: {' -> '.join(cycle_nodes)}",
                detail={"cycle": cycle_nodes},
            )
        )

    return violations


def dependency_levels(plan: ActionPlan) -> list:
    """Return plan steps grouped into dependency levels (topological generations).

    Level 0 has no dependencies; every step in level *k* depends only on steps in
    levels < *k*, so all steps within one level are mutually independent and may
    run concurrently. Flattening the levels yields a valid topological order.

    Precondition: the plan has been verified (no cycles). Raises ``ValueError`` on
    a cycle, matching ``topological_order``.
    """
    step_by_id = {s.id: s for s in plan.steps}
    g = _build_graph(plan)
    return [[step_by_id[i] for i in gen] for gen in _generations(g)]


def topological_order(plan: ActionPlan) -> list:
    """Return plan steps in dependency-respecting order.

    Precondition: the plan has been verified (no cycles, no missing deps).
    Raises ``ValueError`` if the plan has a cycle.
    """
    step_by_id = {s.id: s for s in plan.steps}
    g = _build_graph(plan)
    # nx.topological_sort is the generations, flattened.
    return [step_by_id[i] for gen in _generations(g) for i in gen]
