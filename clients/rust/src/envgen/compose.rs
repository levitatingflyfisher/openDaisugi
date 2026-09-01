//! `compose`: a frozen pathway as a skill a plan's SkillStep calls.
//! Safety needs nothing new: a SkillStep that names a pathway carries the
//! pathway's envelope as its contract_envelope, and verify proves the
//! caller's envelope subsumes it.

use std::collections::BTreeMap;

use super::{list_of, obj, PyError};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};
use crate::pathways::pathway::Pathway;
use crate::pathways::store::Store;
use crate::pathways::PwErr;
use crate::tracejournal::dag::topo_order;

/// An executor runs one step (`executor.run`): its stdout, or the error
/// it raises.
pub trait Executor {
    fn run(&self, step: &Object, timeout_s: u64, max_output_bytes: usize) -> Result<String, PyError>;
}

/// `pathway_skill_handler`: the pathway's plan run step by step in
/// topological order, each output tagged with its step.
pub fn run_skill(pathway: &Object, executors: &BTreeMap<String, Box<dyn Executor>>) -> Result<String, PyError> {
    let plan = obj(pathway.value("plan_template"));
    let steps = topo_order(&plan).map_err(|e| PyError::new(e.typ, e.msg))?;
    let mut outputs = vec![];
    for st in steps {
        let t = st.value("type").as_str().unwrap_or_default();
        let Some(ex) = executors.get(t) else {
            return Err(PyError::new("RuntimeError", format!("no executor for composed step type {}", repr(t))));
        };
        let out = ex.run(st, 30, 65_536)?;
        outputs.push(format!("[{}·{t}] {out}", st.value("id").as_str().unwrap_or_default()));
    }
    Ok(outputs.join("\n"))
}

fn frozen(p: &Pathway) -> bool {
    list_of(p.obj.value("parameters")).is_empty()
}

/// `pathway_skill_handlers_for`: the named pathways that exist and are
/// frozen, by id. A typed pathway is not composed.
pub fn handlers_for(store: &Store, skill_ids: &[String]) -> Result<BTreeMap<String, Pathway>, PwErr> {
    let mut out = BTreeMap::new();
    for id in skill_ids {
        if let Some(p) = store.get_pathway(id)? {
            if frozen(&p) {
                out.insert(id.clone(), p);
            }
        }
    }
    Ok(out)
}

/// `pathway_contract_envelopes`: each frozen pathway's envelope by its id.
pub fn contract_envelopes(store: &Store) -> Result<BTreeMap<String, Value>, PwErr> {
    let mut out = BTreeMap::new();
    for p in store.read_all(&|_| false)? {
        if frozen(&p) {
            out.insert(p.id().to_string(), p.obj.value("envelope").clone());
        }
    }
    Ok(out)
}
