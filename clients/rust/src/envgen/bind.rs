//! `pathway_bind.bind_parameters` and `pathway_params.apply_bindings`:
//! fill a typed pathway's holes for a new task with one model call, then
//! verify the bound plan against the caller's envelope. Any failure gives
//! the frozen template.

use num_bigint::BigInt;
use num_traits::ToPrimitive;

use super::prompts_gen::BIND_SYSTEM;
use super::{list_of, obj, str_list, PyError};
use crate::distill::params::capability_head_any;
use crate::gate::py::text::repr_list;
use crate::gate::pyjson::{Object, Value};
use crate::llm::{Call, Client, Schema};
use crate::pathways::pmodel::{self, Id};
use crate::tracejournal::dag::topo_order;

fn s(v: &Value) -> String {
    v.as_str().unwrap_or_default().to_string()
}

/// The pathway's parameters.
pub fn params(pathway: &Object) -> Vec<Object> {
    list_of(pathway.value("parameters")).iter().map(obj).collect()
}

/// `_user_prompt`.
pub fn bind_prompt(ps: &[Object], task: &str) -> String {
    let holes: Vec<String> = ps
        .iter()
        .map(|p| {
            format!(
                "- {}: fills the `{}` of a step whose head is `{}`; past values: {}",
                s(p.value("name")),
                s(p.value("field")),
                s(p.value("head")),
                repr_list(&str_list(p.value("observed")))
            )
        })
        .collect();
    let heads: std::collections::BTreeSet<String> = ps.iter().map(|p| s(p.value("head"))).collect();
    let heads: Vec<String> = heads.into_iter().collect();
    format!(
        "Task: {task}\n\nHoles to fill (keep each head `{}` exactly):\n{}\n\nReturn {{name: concrete value}} for every hole.",
        heads.join("`, `"),
        holes.join("\n")
    )
}

/// Whether a step type declares `field`, and whether it holds a string
/// (so a bound string keeps the plan well formed).
fn holds_a_string(step_type: &str, field: &str) -> bool {
    let Some(m) = pmodel::steps().get(step_type) else { return false };
    match m.fields.iter().find(|f| f.name == field).map(|f| &f.schema) {
        Some(pmodel::Schema::Str) | Some(pmodel::Schema::StrMax(_)) => true,
        Some(pmodel::Schema::Nullable(inner)) => matches!(**inner, pmodel::Schema::Str | pmodel::Schema::StrMax(_)),
        _ => false,
    }
}

/// `apply_bindings`: the bound plan on a copy, or None when a hole is
/// unbound or a value would change its capability head. A binding into a
/// field that does not hold a string (or that the step does not declare)
/// raises ValueError, as the oracle does (K1-4).
pub fn apply_bindings(template: &Object, ps: &[Object], values: &Object) -> Result<Option<Object>, PyError> {
    let mut plan = template.clone();
    // The steps in topological order, as places in the plan's step list.
    let order: Vec<usize> = {
        let all: Vec<*const Object> = match plan.value("steps") {
            Value::List(l) => l.iter().map(|v| v.as_obj().map(|o| o as *const Object).unwrap_or(std::ptr::null())).collect(),
            _ => vec![],
        };
        let topo = topo_order(&plan).map_err(|e| PyError::new(e.typ, e.msg))?;
        topo.iter().filter_map(|o| all.iter().position(|p| std::ptr::eq(*p, *o))).collect()
    };
    for p in ps {
        let name = s(p.value("name"));
        let idx: BigInt = match p.value("step_index") {
            Value::Int(t) => t.parse().unwrap_or_default(),
            _ => BigInt::default(),
        };
        let n = order.len() as i64;
        let Some(v) = values.get(&name) else { return Ok(None) };
        let mut i = match idx.to_i64() {
            Some(i) => i,
            None if idx > BigInt::default() => return Ok(None),
            None => return Err(PyError::new("IndexError", "list index out of range")),
        };
        if i >= n {
            return Ok(None);
        }
        if i < 0 {
            // A negative index counts from the end, as a list does.
            i += n;
            if i < 0 {
                return Err(PyError::new("IndexError", "list index out of range"));
            }
        }
        let value = s(v);
        let at = order[i as usize];
        let Value::List(steps) = plan.get_mut("steps").expect("the plan has steps") else { unreachable!() };
        let Value::Obj(step) = &mut steps[at] else { unreachable!() };
        let st = s(step.value("type"));
        match capability_head_any(&st, &value) {
            Some(h) if h == s(p.value("head")) => {}
            _ => return Ok(None),
        }
        let field = s(p.value("field"));
        if !holds_a_string(&st, &field) {
            return Err(PyError::new(
                "ValueError",
                format!("the {st} step has no string field {}", crate::gate::py::text::repr(&field)),
            ));
        }
        step.set(&field, value);
    }
    Ok(Some(plan))
}

/// `verify(plan, envelope).ok`, failing closed: a Z3 check that did not
/// finish is a failure here, where the oracle's lenient verify keeps it as
/// a warning (GW-11, K1-3).
pub fn plan_verifies(plan: &Object, env: &Value, z3_ms: u32) -> bool {
    match crate::pathways::verify::verify(&Value::Obj(plan.clone()), env, None, z3_ms) {
        Ok(o) => o.violations.is_empty() && o.timeouts.is_empty(),
        Err(_) => false,
    }
}

/// `bind_parameters`: a verified concrete plan for task, or the frozen
/// template. No client is no model, which gives the template.
pub fn bind(c: Option<&mut Client>, pathway: &Object, task: &str, env: &Value, model: &str, z3_ms: u32) -> Object {
    let template = obj(pathway.value("plan_template"));
    let ps = params(pathway);
    let Some(c) = c else { return template };
    if ps.is_empty() || c.preflight_model(model).is_err() {
        return template;
    }
    let user = bind_prompt(&ps, task);
    let call = Call::new(model, BIND_SYSTEM, &user, Schema { name: "Bindings", model: Id::Bindings }, 2);
    let Ok(resp) = c.structured(&call) else { return template };
    let Value::Obj(values) = resp.value("values") else { return template };
    match apply_bindings(&template, &ps, values) {
        Ok(Some(bound)) if plan_verifies(&bound, env, z3_ms) => bound,
        _ => template,
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::gate::pyjson::loads;
    use crate::pathways::verify::FORCE_Z3_UNKNOWN_TASK;

    fn o(t: &str) -> Object {
        match loads(t) {
            Ok(Value::Obj(o)) => o,
            _ => panic!("{t}"),
        }
    }

    fn plan() -> Object {
        o(r#"{"id":"p","source":"s","task":"t","steps":[
            {"id":"s1","depends_on":[],"metadata":{},"postcondition":null,"preferred_model":null,"type":"shell","command":"make test"},
            {"id":"s2","depends_on":["s1"],"metadata":{},"postcondition":null,"preferred_model":null,"type":"file_read","path":"/work/a"}]}"#)
    }

    fn param(field: &str, idx: i64, head: &str) -> Object {
        o(&format!(r#"{{"name":"p","step_index":{idx},"step_id":"s2","field":"{field}","head":"{head}","observed":["/work/a"]}}"#))
    }

    #[test]
    fn a_hole_is_bound_on_a_copy() {
        let t = plan();
        let v = o(r#"{"p":"/work/b"}"#);
        let bound = apply_bindings(&t, &[param("path", 1, "/work")], &v).unwrap().unwrap();
        assert_eq!(dag_path(&bound), "/work/b");
        assert_eq!(dag_path(&t), "/work/a");
        // -1 counts from the end.
        assert!(apply_bindings(&t, &[param("path", -1, "/work")], &v).unwrap().is_some());
        assert!(apply_bindings(&t, &[param("path", 5, "/work")], &v).unwrap().is_none());
        assert!(apply_bindings(&t, &[param("path", 1, "/etc")], &v).unwrap().is_none());
        assert!(apply_bindings(&t, &[param("path", 1, "/work")], &o(r#"{"q":"/work/b"}"#)).unwrap().is_none());
    }

    fn dag_path(p: &Object) -> String {
        match p.value("steps") {
            Value::List(l) => s(l[1].as_obj().unwrap().value("path")),
            _ => String::new(),
        }
    }

    #[test]
    fn a_field_that_is_not_a_string_raises() {
        let v = o(r#"{"p":"/work/b"}"#);
        for f in ["depends_on", "metadata", "type", "postcondition", "bogus"] {
            let e = apply_bindings(&plan(), &[param(f, 1, "/work")], &v).unwrap_err();
            assert_eq!(e.class, "ValueError", "{f}");
            assert_eq!(e.msg, format!("the file_read step has no string field '{f}'"));
        }
        assert!(apply_bindings(&plan(), &[param("id", 1, "/work")], &v).unwrap().is_some());
        assert!(apply_bindings(&plan(), &[param("preferred_model", 1, "/work")], &v).unwrap().is_some());
    }

    /// K1-3: a bound plan whose verify did not finish is not served; bind
    /// gives the template.
    #[test]
    fn a_verify_that_did_not_finish_gives_the_template() {
        let text = r#"{"id":"env_1","generated_by":"g","task":"TASK","permissions":{"file_read":["/work/**"],"shell":true,"shell_allowlist":["make"]}}"#;
        let env = loads(&text.replace("TASK", "t")).unwrap();
        let bound = apply_bindings(&plan(), &[param("path", 1, "/work")], &o(r#"{"p":"/work/b"}"#)).unwrap().unwrap();
        assert!(plan_verifies(&bound, &env, 500));
        let unknown = loads(&text.replace("TASK", FORCE_Z3_UNKNOWN_TASK)).unwrap();
        assert!(!plan_verifies(&bound, &unknown, 500));
    }
}
