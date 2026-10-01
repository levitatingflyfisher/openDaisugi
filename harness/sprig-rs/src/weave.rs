//! weave: a small JSON graph of steps, each a sprig task, run one at a
//! time in dependency order. A workflow with a cycle or a dangling need
//! does not parse, so it cannot run.

use crate::agent::Agent;
use crate::goerr::quote;
use crate::json::{encode, GoSlice, J};

#[derive(Debug, Clone, Default, PartialEq)]
pub struct Step {
    pub id: String,
    pub task: String,
    pub needs: GoSlice<String>,
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct Workflow {
    pub name: String,
    pub steps: GoSlice<Step>,
}

/// One step's outcome.
#[derive(Debug, Clone, PartialEq)]
pub struct StepResult {
    pub step_id: String,
    pub output: Vec<u8>,
    pub err: Option<Vec<u8>>,
}

/// Reads and checks a workflow: ids unique, every need real, no cycle.
pub fn parse(data: &[u8]) -> Result<Workflow, Vec<u8>> {
    let mut wf = Workflow::default();
    let r = crate::json::unmarshal(data, |d, n| {
        d.object(
            n,
            "Workflow",
            "weave.Workflow",
            &["name", "steps"],
            |d, i, v| match i {
                0 => d.string(v, &mut wf.name),
                _ => d.slice(v, &mut wf.steps, "[]weave.Step", |d, v, s| {
                    d.object(
                        v,
                        "Step",
                        "weave.Step",
                        &["id", "task", "needs"],
                        |d, i, v| match i {
                            0 => d.string(v, &mut s.id),
                            1 => d.string(v, &mut s.task),
                            _ => d.slice(v, &mut s.needs, "[]string", |d, v, x| d.string(v, x)),
                        },
                    )
                }),
            },
        )
    });
    if let Err(e) = r {
        return Err(format!("weave: bad JSON: {e}").into_bytes());
    }
    let mut seen = std::collections::HashSet::new();
    for s in wf.steps.items() {
        if s.id.is_empty() {
            return Err(b"weave: a step has no id".to_vec());
        }
        if !seen.insert(s.id.clone()) {
            return Err(
                format!("weave: duplicate step id {}", quote(s.id.as_bytes())).into_bytes(),
            );
        }
    }
    for s in wf.steps.items() {
        for n in s.needs.items() {
            if !seen.contains(n) {
                return Err(format!(
                    "weave: step {} needs {}, which does not exist",
                    quote(s.id.as_bytes()),
                    quote(n.as_bytes())
                )
                .into_bytes());
            }
        }
    }
    topo_order(wf.steps.items())?;
    Ok(wf)
}

/// The steps in dependency order (Kahn's algorithm, ready steps in the
/// order they are declared), or the cycle error.
pub fn topo_order(steps: &[Step]) -> Result<Vec<Step>, Vec<u8>> {
    use std::collections::HashMap;
    let mut by_id: HashMap<&str, &Step> = HashMap::new();
    let mut indeg: HashMap<&str, usize> = HashMap::new();
    let mut dependents: HashMap<&str, Vec<&str>> = HashMap::new();
    for s in steps {
        by_id.insert(&s.id, s);
        indeg.entry(&s.id).or_insert(0);
    }
    for s in steps {
        for n in s.needs.items() {
            *indeg.entry(&s.id).or_insert(0) += 1;
            dependents.entry(n).or_default().push(&s.id);
        }
    }
    let mut queue: std::collections::VecDeque<&str> = steps
        .iter()
        .filter(|s| indeg[s.id.as_str()] == 0)
        .map(|s| s.id.as_str())
        .collect();
    let mut order = Vec::new();
    while let Some(id) = queue.pop_front() {
        order.push(by_id[id].clone());
        if let Some(ds) = dependents.get(id) {
            for d in ds {
                let e = indeg.entry(d).or_insert(0);
                *e = e.saturating_sub(1);
                if *e == 0 {
                    queue.push_back(d);
                }
            }
        }
    }
    if order.len() != steps.len() {
        return Err(b"weave: the workflow has a cycle".to_vec());
    }
    Ok(order)
}

impl Workflow {
    /// Runs the steps in order, each on the agent `new_agent` builds;
    /// stops at the first step that fails.
    pub fn run(
        &self,
        new_agent: &mut dyn FnMut(&Step) -> Agent,
    ) -> Result<Vec<StepResult>, Vec<u8>> {
        let order = topo_order(self.steps.items())?;
        let mut results = Vec::new();
        for step in &order {
            let r = new_agent(step).run(step.task.as_bytes());
            let failed = r.is_err();
            results.push(match r {
                Ok(out) => StepResult {
                    step_id: step.id.clone(),
                    output: out,
                    err: None,
                },
                Err(e) => StepResult {
                    step_id: step.id.clone(),
                    output: Vec::new(),
                    err: Some(e),
                },
            });
            if failed {
                break;
            }
        }
        Ok(results)
    }
}

/// The results as `json.NewEncoder(os.Stdout).Encode(results)` writes
/// them: a nil list is `null`, and an error is `{}`.
pub fn results_json(results: &[StepResult]) -> Vec<u8> {
    let mut out = if results.is_empty() {
        b"null".to_vec()
    } else {
        encode(&J::Arr(
            results
                .iter()
                .map(|r| {
                    J::Obj(vec![
                        ("StepID".into(), J::s(&r.step_id)),
                        ("Output".into(), J::s(&r.output)),
                        (
                            "Err".into(),
                            if r.err.is_some() {
                                J::Obj(Vec::new())
                            } else {
                                J::Null
                            },
                        ),
                    ])
                })
                .collect(),
        ))
    };
    out.push(b'\n');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn err(s: &str) -> Vec<u8> {
        parse(s.as_bytes()).unwrap_err()
    }

    #[test]
    fn parse_follows_go() {
        assert_eq!(err("{"), b"weave: bad JSON: unexpected end of JSON input");
        assert_eq!(
            err("[]"),
            b"weave: bad JSON: json: cannot unmarshal array into Go value of type weave.Workflow"
        );
        assert_eq!(
            err(r#"{"steps":{}}"#),
            b"weave: bad JSON: json: cannot unmarshal object into Go struct field Workflow.steps of type []weave.Step"
        );
        assert_eq!(
            err(r#"{"steps":[{"id":5}]}"#),
            b"weave: bad JSON: json: cannot unmarshal number into Go struct field Step.steps.id of type string"
        );
        assert_eq!(
            err(r#"{"steps":[{"task":"t"}]}"#),
            b"weave: a step has no id"
        );
        assert_eq!(
            err(r#"{"steps":[{"id":"a"},{"id":"a"}]}"#),
            b"weave: duplicate step id \"a\""
        );
        assert_eq!(
            err(r#"{"steps":[{"id":"a","needs":["zé"]}]}"#),
            "weave: step \"a\" needs \"z\u{e9}\", which does not exist".as_bytes()
        );
        assert_eq!(
            err(r#"{"steps":[{"id":"a","needs":["a"]}]}"#),
            b"weave: the workflow has a cycle"
        );
        let wf = parse(
            br#"{"steps":[{"id":"c","needs":["a","b"]},{"id":"a"},{"id":"b","needs":["a"]}]}"#,
        )
        .unwrap();
        let order: Vec<String> = topo_order(wf.steps.items())
            .unwrap()
            .into_iter()
            .map(|s| s.id)
            .collect();
        assert_eq!(order, vec!["a", "b", "c"]);
    }

    #[test]
    fn json_follows_go() {
        assert_eq!(results_json(&[]), b"null\n");
        let r = vec![
            StepResult {
                step_id: "a".into(),
                output: b"<o>".to_vec(),
                err: None,
            },
            StepResult {
                step_id: "b".into(),
                output: Vec::new(),
                err: Some(b"x".to_vec()),
            },
        ];
        assert_eq!(
            results_json(&r),
            b"[{\"StepID\":\"a\",\"Output\":\"\\u003co\\u003e\",\"Err\":null},{\"StepID\":\"b\",\"Output\":\"\",\"Err\":{}}]\n"
        );
        let _ = (quote(b""), encode(&J::Null));
    }
}
