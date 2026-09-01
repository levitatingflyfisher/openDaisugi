//! Port of `opendaisugi/dag.py`'s `check_dag`: duplicate step ids, missing
//! dependencies, then a cycle check — each tier short-circuits the next.

use crate::models::ActionPlan;
use crate::violation::Violation;
use std::collections::{HashMap, HashSet};

pub fn check_dag(plan: &ActionPlan) -> Vec<Violation> {
    let mut violations = Vec::new();

    // Duplicate step ids.
    let mut seen: HashSet<&str> = HashSet::new();
    let mut dupe_seen: HashSet<&str> = HashSet::new();
    let mut dupes: Vec<&str> = Vec::new();
    for step in &plan.steps {
        let id = step.id.as_str();
        if seen.contains(id) && !dupe_seen.contains(id) {
            dupes.push(id);
            dupe_seen.insert(id);
        }
        seen.insert(id);
    }
    for d in dupes {
        violations.push(Violation::step("dag", d).msg(format!("duplicate step id '{d}' \u{2014} step ids must be unique")));
    }
    if !violations.is_empty() {
        return violations; // graph checks below are meaningless with duplicate ids
    }

    // Missing dependency detection.
    let step_ids: HashSet<&str> = plan.steps.iter().map(|s| s.id.as_str()).collect();
    for step in &plan.steps {
        for dep in &step.depends_on {
            if !step_ids.contains(dep.as_str()) {
                violations.push(
                    Violation::step("dag", step.id.clone())
                        .msg(format!("Step '{}' depends on unknown step '{dep}'", step.id)),
                );
            }
        }
    }
    if !violations.is_empty() {
        return violations;
    }

    if let Some(cycle) = find_cycle(plan) {
        violations.push(Violation::plan("dag").msg(format!("Plan contains a cycle: {}", cycle.join(" -> "))));
    }
    violations
}

/// `nx.find_cycle(g, orientation="original")` over the graph
/// `dag._build_graph` makes (a node per step, an edge dep -> step, in plan
/// order): the nodes of the cycle it reports, or None when there is none.
/// It is networkx's own walk, so the cycle named is networkx's.
fn find_cycle(plan: &ActionPlan) -> Option<Vec<String>> {
    let mut nodes: Vec<&str> = vec![];
    let mut adj: HashMap<&str, Vec<&str>> = HashMap::new();
    let mut has_edge: HashSet<(&str, &str)> = HashSet::new();
    fn add<'a>(n: &'a str, nodes: &mut Vec<&'a str>, adj: &mut HashMap<&'a str, Vec<&'a str>>) {
        if !adj.contains_key(n) {
            adj.insert(n, vec![]);
            nodes.push(n);
        }
    }
    for s in &plan.steps {
        add(&s.id, &mut nodes, &mut adj);
    }
    for s in &plan.steps {
        for dep in &s.depends_on {
            add(dep, &mut nodes, &mut adj);
            if has_edge.insert((dep.as_str(), s.id.as_str())) {
                adj.get_mut(dep.as_str()).expect("added above").push(s.id.as_str());
            }
        }
    }
    let mut explored: HashSet<&str> = HashSet::new();
    for &start in &nodes {
        if explored.contains(start) {
            continue;
        }
        let mut edges: Vec<(&str, &str)> = vec![];
        let mut seen: HashSet<&str> = HashSet::from([start]);
        let mut active: HashSet<&str> = HashSet::from([start]);
        let mut prev_head: Option<&str> = None;
        for (tail, head) in edge_dfs(&adj, start) {
            if explored.contains(head) {
                continue;
            }
            if prev_head.is_some_and(|p| tail != p) {
                loop {
                    match edges.pop() {
                        None => {
                            active = HashSet::from([tail]);
                            break;
                        }
                        Some(popped) => {
                            active.remove(popped.1);
                            if edges.last().is_some_and(|e| tail == e.1) {
                                break;
                            }
                        }
                    }
                }
            }
            edges.push((tail, head));
            if active.contains(head) {
                let i = edges.iter().position(|e| e.0 == head).unwrap_or(0);
                return Some(edges[i..].iter().map(|e| e.0.to_string()).collect());
            }
            seen.insert(head);
            active.insert(head);
            prev_head = Some(head);
        }
        explored.extend(seen);
    }
    None
}

/// `nx.edge_dfs` from one start node: every edge once, in the order a
/// depth-first walk over the adjacency lists meets it.
fn edge_dfs<'a>(adj: &HashMap<&'a str, Vec<&'a str>>, start: &'a str) -> Vec<(&'a str, &'a str)> {
    let mut out = vec![];
    let mut visited_edges: HashSet<(&str, &str)> = HashSet::new();
    let mut next: HashMap<&str, usize> = HashMap::new();
    let mut stack = vec![start];
    while let Some(&cur) = stack.last() {
        let k = *next.entry(cur).or_insert(0);
        let targets = adj.get(cur).map(|v| v.as_slice()).unwrap_or(&[]);
        if k >= targets.len() {
            stack.pop();
            continue;
        }
        let to = targets[k];
        next.insert(cur, k + 1);
        if visited_edges.insert((cur, to)) {
            stack.push(to);
            out.push((cur, to));
        }
    }
    out
}
