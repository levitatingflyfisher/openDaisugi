//! `dag.topological_order` and `distiller.plan_structure_signature` over
//! a plan's `model_dump()`.

use std::collections::HashMap;

use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};

/// An exception the oracle raises: its type and str().
#[derive(Debug, Clone)]
pub struct PyError {
    pub typ: &'static str,
    pub msg: String,
}

/// A plan's steps.
pub fn steps(plan: &Object) -> Vec<&Object> {
    match plan.value("steps") {
        Value::List(l) => l.iter().filter_map(|s| s.as_obj()).collect(),
        _ => vec![],
    }
}

fn deps(s: &Object) -> Vec<String> {
    match s.value("depends_on") {
        Value::List(l) => l.iter().filter_map(|d| d.as_str().map(|x| x.to_string())).collect(),
        _ => vec![],
    }
}

/// `dag.topological_order`: networkx's topological_sort of the plan's
/// graph (nodes in step order, then each unknown dependency as its first
/// edge adds it; each node's successors in the order their edges were
/// added), taken generation by generation. The error is the one Python
/// raises: ValueError for a cycle, KeyError for a dependency that is not
/// a step.
pub fn topo_order(plan: &Object) -> Result<Vec<&Object>, PyError> {
    let ss = steps(plan);
    let mut by_id: HashMap<String, &Object> = HashMap::new();
    let mut nodes: Vec<String> = vec![];
    let mut index: HashMap<String, usize> = HashMap::new();
    let add = |n: &str, nodes: &mut Vec<String>, index: &mut HashMap<String, usize>| {
        if !index.contains_key(n) {
            index.insert(n.to_string(), nodes.len());
            nodes.push(n.to_string());
        }
    };
    for s in &ss {
        let id = s.value("id").as_str().unwrap_or("").to_string();
        by_id.insert(id.clone(), s);
        add(&id, &mut nodes, &mut index);
    }
    let mut succ: HashMap<String, Vec<String>> = HashMap::new();
    let mut edges: std::collections::HashSet<(String, String)> = Default::default();
    let mut indeg: HashMap<String, usize> = HashMap::new();
    for s in &ss {
        let to = s.value("id").as_str().unwrap_or("").to_string();
        for from in deps(s) {
            add(&from, &mut nodes, &mut index);
            if !edges.insert((from.clone(), to.clone())) {
                continue;
            }
            succ.entry(from).or_default().push(to.clone());
            *indeg.entry(to.clone()).or_default() += 1;
        }
    }
    let mut zero: Vec<String> = nodes.iter().filter(|n| indeg.get(*n).copied().unwrap_or(0) == 0).cloned().collect();
    let mut remaining: HashMap<String, usize> = indeg.into_iter().filter(|(_, d)| *d > 0).collect();
    let mut ids: Vec<String> = vec![];
    while !zero.is_empty() {
        let gen = std::mem::take(&mut zero);
        for n in &gen {
            if let Some(cs) = succ.get(n) {
                for c in cs {
                    if let Some(d) = remaining.get_mut(c) {
                        *d -= 1;
                        if *d == 0 {
                            remaining.remove(c);
                            zero.push(c.clone());
                        }
                    }
                }
            }
        }
        ids.extend(gen);
    }
    if !remaining.is_empty() {
        return Err(PyError { typ: "ValueError", msg: "Plan has a cycle; run verify(plan, envelope) before supervising".into() });
    }
    let mut order = vec![];
    for id in ids {
        match by_id.get(&id) {
            Some(s) => order.push(*s),
            None => return Err(PyError { typ: "KeyError", msg: repr(&id) }),
        }
    }
    Ok(order)
}

/// `distiller.plan_structure_signature`: the step types in topological
/// order joined by an arrow; None where Python raises.
pub fn structure_signature(plan: &Object) -> Option<String> {
    let order = topo_order(plan).ok()?;
    Some(order.iter().map(|s| s.value("type").as_str().unwrap_or("")).collect::<Vec<_>>().join("\u{2192}"))
}
