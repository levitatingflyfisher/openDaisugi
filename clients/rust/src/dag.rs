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
        violations.push(Violation::step("dag", d));
    }
    if !violations.is_empty() {
        return violations; // graph checks below are meaningless with duplicate ids
    }

    // Missing dependency detection.
    let step_ids: HashSet<&str> = plan.steps.iter().map(|s| s.id.as_str()).collect();
    for step in &plan.steps {
        for dep in &step.depends_on {
            if !step_ids.contains(dep.as_str()) {
                violations.push(Violation::step("dag", step.id.clone()));
            }
        }
    }
    if !violations.is_empty() {
        return violations;
    }

    if has_cycle(plan) {
        violations.push(Violation::plan("dag"));
    }
    violations
}

#[derive(Clone, Copy, PartialEq)]
enum State {
    Unvisited,
    Visiting,
    Done,
}

fn has_cycle(plan: &ActionPlan) -> bool {
    // Edge dep -> step (dep must run before step), matching dag._build_graph.
    let mut adj: HashMap<&str, Vec<&str>> = HashMap::new();
    for step in &plan.steps {
        adj.entry(step.id.as_str()).or_default();
    }
    for step in &plan.steps {
        for dep in &step.depends_on {
            adj.entry(dep.as_str()).or_default().push(step.id.as_str());
        }
    }

    let mut state: HashMap<&str, State> = adj.keys().map(|k| (*k, State::Unvisited)).collect();
    let nodes: Vec<&str> = adj.keys().copied().collect();

    fn dfs<'a>(node: &'a str, adj: &HashMap<&'a str, Vec<&'a str>>, state: &mut HashMap<&'a str, State>) -> bool {
        state.insert(node, State::Visiting);
        if let Some(neighbors) = adj.get(node) {
            for &n in neighbors {
                match state.get(n) {
                    Some(State::Visiting) => return true,
                    Some(State::Done) => continue,
                    _ => {
                        if dfs(n, adj, state) {
                            return true;
                        }
                    }
                }
            }
        }
        state.insert(node, State::Done);
        false
    }

    for node in nodes {
        if state.get(node) == Some(&State::Unvisited) && dfs(node, &adj, &mut state) {
            return true;
        }
    }
    false
}
