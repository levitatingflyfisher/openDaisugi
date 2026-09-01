//! `pathway_params`: typed data holes (ADR-0008) and the do-nothing
//! salvage of divergent positions.

use crate::glob_engine::{normpath, path_matches_any};
use crate::gate::pyjson::{py_repr, Object, Value};
use crate::pathways::pmodel::{steps as step_models, take_model, Id, Loc, Mode};
use crate::tracejournal::dag::{topo_order, PyError};

/// `_CAP_FIELD`: the one field a step type parameterizes.
fn cap_field(t: &str) -> Option<&'static str> {
    match t {
        "file_read" | "file_write" => Some("path"),
        "network" => Some("url"),
        _ => None,
    }
}

/// `_LEAF_VALUE_FIELD`.
fn leaf_value_field(t: &str) -> Option<&'static str> {
    match t {
        "shell" => Some("command"),
        "file_read" | "file_write" => Some("path"),
        "network" => Some("url"),
        _ => None,
    }
}

/// `_LEAF_TOOLS`.
fn leaf_tools(t: &str) -> &'static [&'static str] {
    match t {
        "shell" => &["Bash"],
        "file_read" => &["Read", "Glob", "Grep"],
        "file_write" => &["Read", "Write", "Edit"],
        "network" => &["WebFetch"],
        _ => &[],
    }
}

const MAX_LEAF_VARIANTS: usize = 5;

fn typ(s: &Object) -> &str {
    s.value("type").as_str().unwrap_or("")
}

/// `posixpath.dirname`.
fn dirname(p: &str) -> String {
    let i = p.rfind('/').map(|i| i + 1).unwrap_or(0);
    let head = &p[..i];
    if !head.is_empty() && head.chars().any(|c| c != '/') {
        head.trim_end_matches('/').to_string()
    } else {
        head.to_string()
    }
}

/// The scheme and netloc of `urllib.parse.urlsplit` (Python 3.12); None
/// where it raises ValueError.
fn urlsplit(url: &str) -> Option<(String, String)> {
    let mut url: String = url.trim_start_matches(|c: char| (c as u32) <= 0x20).to_string();
    url.retain(|c| c != '\t' && c != '\r' && c != '\n');
    let mut scheme = String::new();
    if let Some(i) = url.find(':') {
        let cand = &url[..i];
        let b = cand.as_bytes();
        if i > 0
            && b[0].is_ascii_alphabetic()
            && b.iter().all(|c| c.is_ascii_alphanumeric() || *c == b'+' || *c == b'-' || *c == b'.')
        {
            scheme = cand.to_ascii_lowercase();
            url = url[i + 1..].to_string();
        }
    }
    let mut netloc = String::new();
    if let Some(rest) = url.strip_prefix("//") {
        let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
        netloc = rest[..end].to_string();
        if netloc.contains('[') != netloc.contains(']') {
            return None;
        }
        if let Some(k) = netloc.find('[') {
            if !netloc[k..].contains(']') {
                return None;
            }
        }
    }
    Some((scheme, netloc))
}

/// `_capability_head`: the directory of a path, the scheme and host of a
/// URL; None where Python returns None.
fn capability_head(step_type: &str, value: &str) -> Option<String> {
    match step_type {
        "file_read" | "file_write" => {
            let d = dirname(value);
            (!d.is_empty()).then_some(d)
        }
        "network" => {
            let (scheme, netloc) = urlsplit(value)?;
            (!netloc.is_empty()).then(|| format!("{scheme}://{netloc}"))
        }
        _ => None,
    }
}

/// `[topological_order(p) for p in plans]`, the error Python raises for
/// the first plan that fails.
fn ordered_plans(plans: &[Object]) -> Result<Vec<Vec<&Object>>, PyError> {
    plans.iter().map(topo_order).collect()
}

fn signature(steps: &[&Object]) -> String {
    steps.iter().map(|s| typ(s)).collect::<Vec<_>>().join("\u{2192}")
}

fn same_shape(ordered: &[Vec<&Object>]) -> bool {
    let first = signature(&ordered[0]);
    ordered[1..].iter().all(|o| signature(o) == first)
}

/// `[getattr(steps[i], field, None) for steps in ordered]`; None when any
/// is not a str.
fn field_values(ordered: &[Vec<&Object>], i: usize, field: &str) -> Option<Vec<String>> {
    ordered.iter().map(|steps| steps.get(i).and_then(|s| s.value(field).as_str()).map(|s| s.to_string())).collect()
}

fn sorted_set(vals: &[String]) -> Vec<String> {
    let mut v = vals.to_vec();
    v.sort();
    v.dedup();
    v
}

/// `{_capability_head(t, v) for v in values}`: the one head, or None when
/// the heads differ or one is None.
fn heads_of(step_type: &str, vals: &[String]) -> Option<String> {
    let mut head: Option<String> = None;
    for v in vals {
        let h = capability_head(step_type, v)?;
        match &head {
            None => head = Some(h),
            Some(x) if *x != h => return None,
            _ => {}
        }
    }
    head
}

fn parameter(step0: &Object, i: usize, field: &str, head: &str, vals: &[String]) -> Value {
    let id = step0.value("id").as_str().unwrap_or("").to_string();
    let mut o = Object::new();
    o.set("name", format!("{id}.{field}"));
    o.set("step_index", Value::Int(i.to_string()));
    o.set("step_id", id.as_str());
    o.set("field", field);
    o.set("head", head);
    o.set("observed", Value::List(sorted_set(vals).into_iter().map(Value::Str).collect()));
    Value::Obj(o)
}

/// `diff_plans_for_parameters`.
pub fn diff_plans_for_parameters(plans: &[Object]) -> Result<Vec<Value>, PyError> {
    if plans.len() < 2 {
        return Ok(vec![]);
    }
    let ordered = ordered_plans(plans)?;
    if !same_shape(&ordered) {
        return Ok(vec![]);
    }
    let mut params = vec![];
    for (i, step0) in ordered[0].iter().enumerate() {
        let field = match cap_field(typ(step0)) {
            Some(f) => f,
            None => continue,
        };
        let vals = match field_values(&ordered, i, field) {
            Some(v) if !v.iter().all(|x| *x == v[0]) => v,
            _ => continue,
        };
        let head = match heads_of(typ(step0), &vals) {
            Some(h) => h,
            None => return Ok(vec![]),
        };
        params.push(parameter(step0, i, field, &head, &vals));
    }
    Ok(params)
}

/// `rekey_to_template`.
pub fn rekey_to_template(params: Vec<Value>, template: &Object) -> Result<Vec<Value>, PyError> {
    if params.is_empty() {
        return Ok(vec![]);
    }
    let steps = topo_order(template)?;
    let mut out = vec![];
    for x in params {
        let mut p = match x {
            Value::Obj(o) => o,
            _ => continue,
        };
        let idx: usize = match p.value("step_index") {
            Value::Int(t) => t.parse().unwrap_or(usize::MAX),
            _ => usize::MAX,
        };
        let t = match steps.get(idx) {
            Some(t) => t,
            None => return Ok(vec![]),
        };
        if cap_field(typ(t)).map(Value::from) != Some(p.value("field").clone()) {
            return Ok(vec![]);
        }
        p.set("step_id", t.value("id").clone());
        out.push(Value::Obj(p));
    }
    Ok(out)
}

/// `plan_divergence`: the divergent positions and the typed holes.
pub fn plan_divergence(plans: &[Object]) -> Result<(Vec<usize>, Vec<Value>), PyError> {
    if plans.len() < 2 {
        return Ok((vec![], vec![]));
    }
    let ordered = ordered_plans(plans)?;
    if !same_shape(&ordered) {
        return Ok((vec![], vec![]));
    }
    let mut divergent = vec![];
    let mut params = vec![];
    for (i, step0) in ordered[0].iter().enumerate() {
        let t = typ(step0);
        let field = match leaf_value_field(t) {
            Some(f) => f,
            None => continue,
        };
        let vals = match field_values(&ordered, i, field) {
            Some(v) if !v.iter().all(|x| *x == v[0]) => v,
            _ => continue,
        };
        if t == "shell" {
            divergent.push(i);
            continue;
        }
        match heads_of(t, &vals) {
            None => divergent.push(i),
            Some(head) => params.push(parameter(step0, i, field, &head, &vals)),
        }
    }
    if !divergent.is_empty() && divergent.len() >= ordered[0].len() {
        return Ok((vec![], vec![]));
    }
    Ok((divergent, params))
}

/// `salvage_workspace`: the workspace of a salvaged leaf, the fixed
/// prefix of a file_read glob. The first prefix the globs admit under
/// verify's own path matcher wins; None when no glob gives one.
pub fn salvage_workspace(file_read: &[String]) -> Option<String> {
    for glob in file_read {
        let segs: Vec<&str> = glob.split('/').collect();
        let n = segs.iter().take_while(|s| !s.contains(['*', '?', '['])).count();
        if n == segs.len() {
            continue;
        }
        let mut prefix = segs[..n].join("/");
        if prefix.is_empty() && glob.starts_with('/') {
            prefix = "/".into();
        }
        let workspace = normpath(&prefix);
        if path_matches_any(&workspace, file_read) {
            return Some(workspace);
        }
    }
    None
}

/// `str(getattr(step, field, ""))`.
fn py_str_value(v: Option<&Value>) -> String {
    match v {
        Some(Value::Str(s)) => s.clone(),
        None => String::new(),
        Some(Value::Null) => "None".into(),
        Some(v) => py_repr(v).unwrap_or_default(),
    }
}

/// `build_delegated_template`: the representative with each divergent
/// position an AgenticStep leaf. The plan gets a fresh id, as
/// ActionPlan's default does.
pub fn build_delegated_template(
    representative: &Object,
    divergent: &[usize],
    plans: &[Object],
    workspace: &str,
) -> Result<Object, PyError> {
    let ordered = topo_order(representative)?;
    let all = ordered_plans(plans)?;
    let mut steps = vec![];
    for (i, step) in ordered.iter().enumerate() {
        if !divergent.contains(&i) {
            steps.push(Value::Obj((*step).clone()));
            continue;
        }
        let t = typ(step);
        let field = leaf_value_field(t).unwrap_or("");
        let vals: Vec<String> = all.iter().filter(|s| i < s.len()).map(|s| py_str_value(s[i].get(field))).collect();
        let variants = sorted_set(&vals);
        let shown = &variants[..variants.len().min(MAX_LEAF_VARIANTS)];
        let more = if variants.len() > shown.len() { format!(" (+{} more)", variants.len() - shown.len()) } else { String::new() };
        let prompt = format!(
            "Perform this step of the task. In past successful runs it was one of: {}{more}. Choose and execute the right \
             equivalent for the current task; stay within the granted tools and permissions.",
            shown.join("; ")
        );
        let mut leaf = Object::new();
        leaf.set("id", step.value("id").clone());
        leaf.set("depends_on", step.value("depends_on").clone());
        leaf.set("type", "agentic");
        leaf.set("prompt", prompt);
        leaf.set("workspace", workspace);
        leaf.set("tools", Value::List(leaf_tools(t).iter().map(|x| Value::Str(x.to_string())).collect()));
        let m = step_models().get("agentic").expect("agentic is registered");
        let (v, errs) = m.fields(&leaf, &Loc::Root, Mode::Python);
        if !errs.is_empty() {
            return Err(PyError { typ: "ValidationError", msg: "the salvaged leaf does not validate".into() });
        }
        steps.push(v);
    }
    let mut plan = Object::new();
    plan.set("source", "distiller-salvage");
    plan.set("task", representative.value("task").clone());
    plan.set("steps", Value::List(steps));
    match take_model(Id::ActionPlan, Value::Obj(plan), Mode::Python) {
        Ok(Value::Obj(o)) => Ok(o),
        Ok(_) => Err(PyError { typ: "ValidationError", msg: "the salvaged plan is not a mapping".into() }),
        Err(e) => Err(PyError { typ: "ValidationError", msg: e.text() }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_workspace_is_a_globs_fixed_prefix() {
        let s = |v: &[&str]| v.iter().map(|x| x.to_string()).collect::<Vec<_>>();
        assert_eq!(salvage_workspace(&s(&["/work/**"])).as_deref(), Some("/work"));
        assert_eq!(salvage_workspace(&s(&["**"])).as_deref(), Some("."));
        assert_eq!(salvage_workspace(&s(&["./**"])).as_deref(), Some("."));
        assert_eq!(salvage_workspace(&s(&["/**"])).as_deref(), Some("/"));
        assert_eq!(salvage_workspace(&s(&["/work/out.txt"])), None);
    }

    #[test]
    fn heads_and_dirnames_are_pythons() {
        assert_eq!(dirname("/work/a/x"), "/work/a");
        assert_eq!(dirname("/x"), "/");
        assert_eq!(dirname("x"), "");
        assert_eq!(dirname("//x"), "//");
        assert_eq!(capability_head("network", "https://API.Example.com/v1?q=1").as_deref(), Some("https://API.Example.com"));
        assert_eq!(capability_head("network", "no-scheme"), None);
    }
}
