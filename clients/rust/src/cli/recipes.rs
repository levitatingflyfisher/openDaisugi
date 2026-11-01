//! The four recipe files and their reader, as the oracle's
//! `opendaisugi.recipes` reads them: a recipe is a JSON file whose steps
//! each name one install primitive. The files are the Python package's
//! own, included byte for byte.

use crate::warden::pyjson::{dumps, loads, Object, Value};
use std::collections::HashMap;
use std::sync::OnceLock;

use regex::Regex;

const FILES: &[(&str, &str)] = &[
    ("bring-your-own", include_str!("../../../../src/opendaisugi/recipes/bring-your-own.json")),
    ("coppice-claude-warden", include_str!("../../../../src/opendaisugi/recipes/coppice-claude-warden.json")),
    ("router-and-routing-mode", include_str!("../../../../src/opendaisugi/recipes/router-and-routing-mode.json")),
    ("warden-only", include_str!("../../../../src/opendaisugi/recipes/warden-only.json")),
];

pub const STEP_KINDS: &[&str] = &["install", "harness", "config", "warden-init", "note"];
pub const CONFIG_KEYS: &[&str] = &[
    "routing_mode",
    "routing_model",
    "router_local_model",
    "switchyard_efficient_model",
    "switchyard_capable_model",
    "switchyard_api_key_env",
];
pub const ROUTING_MODES: &[&str] = &["rules", "switchyard", "knn", "model", "off"];

fn param_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"\{([A-Za-z_][A-Za-z0-9_]*)\}").expect("a fixed pattern"))
}

/// `recipes.names()`: the shipped recipe names, sorted.
pub fn names() -> Vec<&'static str> {
    let mut n: Vec<&'static str> = FILES.iter().map(|(n, _)| *n).collect();
    n.sort_unstable();
    n
}

/// The bytes of a shipped recipe file.
pub fn text(name: &str) -> Option<&'static str> {
    FILES.iter().find(|(n, _)| *n == name).map(|(_, t)| *t)
}

/// A validated recipe, as the file holds it.
pub struct Recipe {
    pub obj: Object,
}

fn s(o: &Object, k: &str) -> String {
    o.value(k).as_str().unwrap_or("").to_string()
}

fn bad(file: &str, where_: &str, why: &str) -> String {
    format!("{file}: {where_}: {why}")
}

fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().map(|x| x.as_str().unwrap_or("").to_string()).collect(),
        _ => vec![],
    }
}

fn is_str_list(v: &Value) -> bool {
    matches!(v, Value::List(l) if l.iter().all(|x| matches!(x, Value::Str(_))))
}

fn py_str(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        other => dumps(other, true),
    }
}

fn check_strings(file: &str, where_: &str, v: &Value, declared: &[String]) -> Result<(), String> {
    match v {
        Value::Str(t) => {
            for m in param_re().captures_iter(t) {
                if !declared.iter().any(|d| d == &m[1]) {
                    return Err(bad(file, where_, &format!("param '{}' is not declared", &m[1])));
                }
            }
        }
        Value::List(l) => {
            for (i, e) in l.iter().enumerate() {
                check_strings(file, &format!("{where_}[{i}]"), e, declared)?;
            }
        }
        Value::Obj(o) => {
            for k in o.keys() {
                check_strings(file, &format!("{where_}.{k}"), o.value(k), declared)?;
            }
        }
        _ => {}
    }
    Ok(())
}

/// `recipes.load`.
pub fn load(name: &str) -> Result<Recipe, String> {
    let file = format!("{name}.json");
    let Some(t) = text(name) else { return Err(format!("{file}: no such recipe")) };
    let v = loads(t).map_err(|e| format!("{file}: {e:?}"))?;
    validate(v, &file)
}

/// `recipes._validate`.
pub fn validate(v: Value, file: &str) -> Result<Recipe, String> {
    let Value::Obj(raw) = v else { return Err(format!("{file}: the recipe is not an object")) };
    for k in ["name", "title", "doc"] {
        if !matches!(raw.value(k), Value::Str(_)) {
            return Err(bad(file, k, "must be a string"));
        }
    }
    if !matches!(raw.value("slots"), Value::Obj(_)) {
        return Err(bad(file, "slots", "must be an object"));
    }
    let Value::Obj(params) = raw.value("params") else { return Err(bad(file, "params", "must be an object")) };
    let declared: Vec<String> = params.keys().to_vec();
    for pn in params.keys() {
        match params.value(pn) {
            Value::Obj(p) if matches!(p.value("help"), Value::Str(_)) => {}
            _ => return Err(bad(file, &format!("params.{pn}"), "needs a help string")),
        }
    }
    let Value::List(steps) = raw.value("steps") else { return Err(bad(file, "steps", "must be a list")) };
    let known: Vec<&str> = super::components::COMPONENTS.iter().map(|c| c.name).collect();
    for (i, sv) in steps.iter().enumerate() {
        let where_ = format!("steps[{i}]");
        let Value::Obj(step) = sv else { return Err(bad(file, &where_, "must be an object")) };
        let kind = step.value("kind").as_str().unwrap_or("").to_string();
        if !STEP_KINDS.contains(&kind.as_str()) {
            return Err(format!(
                "{file}: {where_}.kind '{}' is not one of {}",
                py_str(step.value("kind")),
                STEP_KINDS.join(", ")
            ));
        }
        if let Some(w) = step.get("when") {
            if !matches!(w, Value::Null) {
                let ws = w.as_str().unwrap_or("");
                if !declared.iter().any(|d| d == ws) {
                    return Err(bad(file, &format!("{where_}.when"), &format!("param '{ws}' is not declared")));
                }
            }
        }
        match kind.as_str() {
            "install" => {
                let only = step.value("only");
                let Value::List(ol) = only else { return Err(bad(file, &format!("{where_}.only"), "must be a list of component names")) };
                if ol.is_empty() || !is_str_list(only) {
                    return Err(bad(file, &format!("{where_}.only"), "must be a list of component names"));
                }
                for x in ol {
                    let n = x.as_str().unwrap_or("");
                    if !known.contains(&n) {
                        return Err(bad(file, &format!("{where_}.only"), &format!("unknown component '{n}'")));
                    }
                }
                if let Some(rt) = step.get("runtime") {
                    if !is_str_list(rt) {
                        return Err(bad(file, &format!("{where_}.runtime"), "must be a list of runtime names"));
                    }
                }
                if let Some(en) = step.get("enforce") {
                    if !matches!(en, Value::Bool(_)) {
                        return Err(bad(file, &format!("{where_}.enforce"), "must be true or false"));
                    }
                }
                for k in ["report", "base_url"] {
                    if let Some(x) = step.get(k) {
                        if !matches!(x, Value::Null | Value::Str(_)) {
                            return Err(bad(file, &format!("{where_}.{k}"), "must be a string or null"));
                        }
                    }
                }
            }
            "harness" => {
                let n = step.value("name").as_str().unwrap_or("");
                if n != "pi" && n != "opencode" {
                    return Err(bad(file, &format!("{where_}.name"), &format!("'{}' is not one of pi, opencode", py_str(step.value("name")))));
                }
            }
            "config" => {
                let Value::Obj(sets) = step.value("set") else {
                    return Err(bad(file, &format!("{where_}.set"), "must be an object of config keys"));
                };
                if sets.is_empty() {
                    return Err(bad(file, &format!("{where_}.set"), "must be an object of config keys"));
                }
                for k in sets.keys() {
                    if !CONFIG_KEYS.contains(&k.as_str()) {
                        return Err(bad(file, &format!("{where_}.set.{k}"), &format!("'{k}' is not a key a recipe may set")));
                    }
                    if !matches!(sets.value(k), Value::Str(_)) {
                        return Err(bad(file, &format!("{where_}.set.{k}"), "must be a string"));
                    }
                }
            }
            "warden-init" => {
                if !matches!(step.value("workspace"), Value::Str(_)) {
                    return Err(bad(file, &format!("{where_}.workspace"), "must be a string"));
                }
            }
            _ => {
                if !matches!(step.value("text"), Value::Str(_)) {
                    return Err(bad(file, &format!("{where_}.text"), "must be a string"));
                }
            }
        }
        let mut rest = Object::new();
        for k in step.keys() {
            if k != "kind" {
                rest.set(k, step.value(k).clone());
            }
        }
        check_strings(file, &where_, &Value::Obj(rest), &declared)?;
    }
    Ok(Recipe { obj: raw })
}

/// `recipes._step_summary`.
pub fn step_summary(step: &Object) -> String {
    match step.value("kind").as_str().unwrap_or("") {
        "install" => {
            let mut parts = vec![format!("--only {}", str_list(step.value("only")).join(","))];
            let rt = str_list(step.value("runtime"));
            if !rt.is_empty() {
                parts.push(format!("--runtime {}", rt.join(",")));
            }
            if step.value("enforce").truthy() {
                parts.push("--enforce".into());
            }
            let rep = s(step, "report");
            if !rep.is_empty() {
                parts.push(format!("--report {rep}"));
            }
            let bu = s(step, "base_url");
            if !bu.is_empty() {
                parts.push(format!("--base-url {bu}"));
            }
            parts.join(" ")
        }
        "harness" => s(step, "name"),
        "config" => match step.value("set") {
            Value::Obj(sets) => sets.keys().iter().map(|k| format!("{k}={}", s(sets, k))).collect::<Vec<_>>().join(" "),
            _ => String::new(),
        },
        "warden-init" => format!("workspace={}", s(step, "workspace")),
        _ => s(step, "text"),
    }
}

impl Recipe {
    pub fn name(&self) -> String {
        s(&self.obj, "name")
    }
    pub fn title(&self) -> String {
        s(&self.obj, "title")
    }
    pub fn doc(&self) -> String {
        s(&self.obj, "doc")
    }
    pub fn param_names(&self) -> Vec<String> {
        match self.obj.value("params") {
            Value::Obj(p) => p.keys().to_vec(),
            _ => vec![],
        }
    }
    pub fn param(&self, name: &str) -> Option<Object> {
        match self.obj.value("params") {
            Value::Obj(p) => match p.value(name) {
                Value::Obj(o) => Some(o.clone()),
                _ => None,
            },
            _ => None,
        }
    }
    pub fn steps(&self) -> Vec<Object> {
        match self.obj.value("steps") {
            Value::List(l) => l.iter().filter_map(|v| if let Value::Obj(o) = v { Some(o.clone()) } else { None }).collect(),
            _ => vec![],
        }
    }
    pub fn metavar(&self, param: &str) -> String {
        self.param(param).and_then(|p| p.value("metavar").as_str().map(String::from)).unwrap_or_else(|| "VALUE".into())
    }

    /// `recipes.render_show`.
    pub fn render_show(&self) -> String {
        let mut lines = vec![self.title(), String::new(), "Slots".to_string()];
        if let Value::Obj(slots) = self.obj.value("slots") {
            for k in slots.keys() {
                lines.push(format!("  {k}: {}", s(slots, k)));
            }
        }
        let names = self.param_names();
        if !names.is_empty() {
            lines.push(String::new());
            lines.push("Params".into());
            for pn in &names {
                let p = self.param(pn).unwrap_or_default();
                let tag = if p.value("required").truthy() {
                    "required".to_string()
                } else if let Some(d) = p.get("default") {
                    format!("default {}", py_str(d))
                } else {
                    "optional".to_string()
                };
                lines.push(format!("  {pn} ({tag}): {}", s(&p, "help")));
            }
        }
        lines.push(String::new());
        lines.push("Steps".into());
        for (i, step) in self.steps().iter().enumerate() {
            let mut line = format!("  {}. {}  {}", i + 1, s(step, "kind"), step_summary(step));
            let w = s(step, "when");
            if !w.is_empty() {
                line.push_str(&format!("  (only with --set {w}={})", self.metavar(&w)));
            }
            lines.push(line);
        }
        lines.join("\n")
    }

    /// `recipes.resolve_params`: `order` is the --set keys as given.
    pub fn resolve_params(&self, order: &[String], given: &HashMap<String, String>) -> Result<Params, String> {
        for k in order {
            if self.param(k).is_none() {
                return Err(format!("recipe '{}' has no param '{k}'", self.name()));
            }
        }
        let mut out = Params::default();
        for pn in self.param_names() {
            let p = self.param(&pn).unwrap_or_default();
            if let Some(v) = given.get(&pn) {
                out.keys.push(pn.clone());
                out.vals.insert(pn, v.clone());
            } else if let Some(d) = p.get("default") {
                out.keys.push(pn.clone());
                out.vals.insert(pn, py_str(d));
            } else if p.value("required").truthy() {
                return Err(format!("recipe '{}' needs --set {pn}={}", self.name(), self.metavar(&pn)));
            }
        }
        Ok(out)
    }

    /// `recipes._fill`: each {param} replaced by its value.
    pub fn fill(&self, v: &Value, params: &Params, index: usize) -> Result<Value, String> {
        Ok(match v {
            Value::Str(t) => {
                let mut err = None;
                let out = param_re().replace_all(t, |c: &regex::Captures| match params.vals.get(&c[1]) {
                    Some(val) => val.clone(),
                    None => {
                        err = Some(format!("step {index} needs --set {}={}", &c[1], self.metavar(&c[1])));
                        c[0].to_string()
                    }
                });
                if let Some(e) = err {
                    return Err(e);
                }
                Value::Str(out.into_owned())
            }
            Value::List(l) => Value::List(l.iter().map(|e| self.fill(e, params, index)).collect::<Result<_, _>>()?),
            Value::Obj(o) => {
                let mut out = Object::new();
                for k in o.keys() {
                    out.set(k, self.fill(o.value(k), params, index)?);
                }
                Value::Obj(out)
            }
            other => other.clone(),
        })
    }
}

/// Param values, in the recipe's param order.
#[derive(Default)]
pub struct Params {
    pub keys: Vec<String>,
    pub vals: HashMap<String, String>,
}

impl Params {
    pub fn has(&self, k: &str) -> bool {
        self.vals.contains_key(k)
    }
    /// The params as the receipt writes them.
    pub fn object(&self) -> Object {
        let mut o = Object::new();
        for k in &self.keys {
            o.set(k, self.vals[k].as_str());
        }
        o
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn four_recipes_ship_and_validate() {
        assert_eq!(names(), vec!["bring-your-own", "coppice-claude-warden", "router-and-routing-mode", "warden-only"]);
        for n in names() {
            load(n).unwrap_or_else(|e| panic!("{n}: {e}"));
        }
    }

    #[test]
    fn a_shell_step_is_refused() {
        let v = loads(r#"{"name":"x","title":"t","doc":"d","slots":{},"params":{},"steps":[{"kind":"shell","command":"rm -rf /"}]}"#).unwrap();
        assert_eq!(
            validate(v, "x.json").err().unwrap(),
            "x.json: steps[0].kind 'shell' is not one of install, harness, config, warden-init, note"
        );
    }

    #[test]
    fn an_undeclared_param_is_refused() {
        let v = loads(r#"{"name":"x","title":"t","doc":"d","slots":{},"params":{},"steps":[{"kind":"warden-init","workspace":"{workspace}"}]}"#).unwrap();
        assert_eq!(
            validate(v, "x.json").err().unwrap(),
            "x.json: steps[0].workspace: param 'workspace' is not declared"
        );
    }
}
