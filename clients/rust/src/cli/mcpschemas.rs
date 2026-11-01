//! `opendaisugi/mcp_schemas.py` and the lint half of `mcp_rules.py`: the
//! tools/list results on file, and the check `warden register` runs on an
//! envelope's `mcp_rules` (MP-5, MP-8). The messages are the oracle's.

use crate::warden::paths::os_error;
use crate::warden::predicate::py_eq;
use crate::warden::pyjson::{self, py_str, Object, Value};
use crate::warden::{statefile, statewrite};
use std::fs;

fn server_ok(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 64
        && s.chars().all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '.' || c == '-')
}

/// A refusal of `schemas import`, in the oracle's words.
pub struct SchemaError(pub String);

const NOT_TOOLS_LIST: &str = "not a tools/list result: expected {\"tools\": [{\"name\": ..., \"inputSchema\": {...}}]}";

fn valid_tools_list(v: &Value) -> Option<&Object> {
    let o = v.as_obj()?;
    let Value::List(tools) = o.get("tools")? else { return None };
    for t in tools {
        let to = t.as_obj()?;
        to.get("name")?.as_str()?;
        to.get("inputSchema")?.as_obj()?;
    }
    Some(o)
}

fn tool_list(o: &Object) -> Vec<&Object> {
    match o.value("tools") {
        Value::List(l) => l.iter().filter_map(Value::as_obj).collect(),
        _ => vec![],
    }
}

/// `mcp_schemas.import_schema`: the path stored and the tool count.
pub fn import_schema(root: &str, server: &str, src: &str) -> Result<(String, usize), SchemaError> {
    if !server_ok(server) || server == "." || server == ".." {
        return Err(SchemaError(format!("not a server name: {}", pyjson::repr(server).unwrap_or_default())));
    }
    let raw = fs::read(src).map_err(|e| SchemaError(format!("not a tools/list result: {}", os_error(&e, src).msg)))?;
    let text = String::from_utf8(raw).map_err(|_| SchemaError("not a tools/list result: not UTF-8 text".into()))?;
    let v = pyjson::loads_py(&text, pyjson::MAX_DEPTH).map_err(|m| SchemaError(format!("not a tools/list result: {m}")))?;
    let Some(o) = valid_tools_list(&v) else { return Err(SchemaError(NOT_TOOLS_LIST.into())) };
    let body = format!("{}\n", pyjson::dumps_indent(&v, 2, false));
    let path = statewrite::write_file(root, &["schemas"], &format!("{server}.json"), &body, false)
        .map_err(|e| match statewrite::write_refused(&e) {
            Some(t) => SchemaError(t.to_string()),
            None => SchemaError(e.to_string()),
        })?;
    Ok((path, tool_list(o).len()))
}

/// One schema file read back.
pub struct Loaded {
    pub server: String,
    pub obj: Object,
}

/// `mcp_schemas.load_schemas`, sorted by file name. A file that cannot be
/// read is skipped with a warning on `warn`.
pub fn load_schemas(root: &str, warn: &mut dyn FnMut(String)) -> Vec<Loaded> {
    let dir = super::wardenroot::join(root, "schemas");
    let Ok(rd) = fs::read_dir(&dir) else { return vec![] };
    let mut names: Vec<String> = rd.filter_map(|e| e.ok()).filter_map(|e| e.file_name().into_string().ok()).collect();
    names.sort();
    let mut out = Vec::new();
    for name in names {
        let Some(server) = name.strip_suffix(".json") else { continue };
        if !server_ok(server) {
            continue;
        }
        let why = match statefile::probe_read(root, &["schemas"], &name) {
            Err(miss) => miss.refusal().unwrap_or("unreadable").to_string(),
            Ok(Err(e)) => e.to_string(),
            Ok(Ok(raw)) => match String::from_utf8(raw) {
                Err(_) => "not UTF-8 text".to_string(),
                Ok(text) => match pyjson::loads_py(&text, pyjson::MAX_DEPTH) {
                    Err(m) => m,
                    Ok(v) => match valid_tools_list(&v) {
                        Some(o) => {
                            out.push(Loaded { server: server.to_string(), obj: o.clone() });
                            continue;
                        }
                        None => NOT_TOOLS_LIST.to_string(),
                    },
                },
            },
        };
        warn(format!("warden: skipped the schema file {name}: {why}"));
    }
    out
}

fn same_kind_eq(a: &Value, b: &Value) -> bool {
    std::mem::discriminant(a) == std::mem::discriminant(b) && py_eq(a, b)
}

/// `mcp_rules._top_level_alternation`: a `|` outside every group and
/// character class.
fn top_level_alternation(re: &str) -> bool {
    let r: Vec<char> = re.chars().collect();
    let (mut depth, mut in_class, mut i) = (0i32, false, 0usize);
    while i < r.len() {
        let c = r[i];
        if c == '\\' {
            i += 2;
            continue;
        }
        if in_class {
            if c == ']' {
                in_class = false;
            }
        } else if c == '[' {
            in_class = true;
            if r.get(i + 1) == Some(&'^') {
                i += 1;
            }
            if r.get(i + 1) == Some(&']') {
                i += 1;
            }
        } else if c == '(' {
            depth += 1;
        } else if c == ')' {
            depth -= 1;
        } else if c == '|' && depth == 0 {
            return true;
        }
        i += 1;
    }
    false
}

/// `mcp_rules._split_flag`: a leading `(?i)` and the rest.
fn split_flag(re: &str) -> (&str, &str) {
    match re.strip_prefix("(?i)") {
        Some(rest) => ("(?i)", rest),
        None => ("", re),
    }
}

fn anchored(re: &str) -> bool {
    let (_, body) = split_flag(re);
    body.starts_with('^') && body.ends_with('$') && !top_level_alternation(body)
}

const RULE_OPS: &[&str] = &[
    "equals", "not_equals", "in_set", "not_in_set", "matches", "not_matches", "numeric_range", "length_range",
    "exists", "is_empty", "path_in", "and", "or", "not", "implies",
];

fn lint_exprs(tool: &str, e: &Value, refusals: &mut Vec<String>, perms: Option<&Value>) {
    let Some(o) = e.as_obj() else { return };
    let op = o.value("op").as_str().unwrap_or("");
    if !RULE_OPS.contains(&op) {
        refusals.push(format!("mcp_rules: {tool}: op '{op}' cannot be used in a rule"));
        return;
    }
    if op == "path_in" {
        // Owner, 2026-11-03: path_in takes absolute paths only, so its scope
        // may hold no relative glob.
        if let Some(p) = perms.and_then(|p| p.as_obj()) {
            let scope = o.value("scope").as_str().unwrap_or("");
            let path = o.value("path").as_str().unwrap_or("");
            if let Value::List(globs) = p.value(scope) {
                for g in globs {
                    if let Some(gs) = g.as_str() {
                        if !gs.starts_with('/') {
                            refusals.push(format!(
                                "mcp_rules: {tool}: path_in on '{path}' reads {scope}, which holds the relative glob '{gs}'. {}: write the glob from /",
                                crate::mcp_rules::PATH_IN_ABSOLUTE
                            ));
                        }
                    }
                }
            }
        }
    }
    if op == "matches" || op == "not_matches" {
        let re = o.value("regex").as_str().unwrap_or("");
        if !crate::rulere::valid(re) {
            refusals.push(format!("mcp_rules: {tool}: {}", crate::rulere::invalid_reason(re)));
        } else {
            if !anchored(re) {
                let (flag, body) = split_flag(re);
                let mut fix = String::from(flag);
                if top_level_alternation(body) {
                    fix.push_str(&format!("^(?:{body})$"));
                } else {
                    if !body.starts_with('^') {
                        fix.push('^');
                    }
                    fix.push_str(body);
                    if !body.ends_with('$') {
                        fix.push('$');
                    }
                }
                refusals.push(format!("mcp_rules: {tool}: regex '{re}' is not anchored; write {fix}"));
            }
        }
    }
    match op {
        "and" | "or" => {
            if let Value::List(cs) = o.value("children") {
                cs.iter().for_each(|c| lint_exprs(tool, c, refusals, perms));
            }
        }
        "not" => lint_exprs(tool, o.value("child"), refusals, perms),
        "implies" => {
            lint_exprs(tool, o.value("a"), refusals, perms);
            lint_exprs(tool, o.value("b"), refusals, perms);
        }
        _ => {}
    }
}

fn lint_paths(e: &Value, out: &mut Vec<String>) {
    let Some(o) = e.as_obj() else { return };
    match o.value("op").as_str().unwrap_or("") {
        "and" | "or" => {
            if let Value::List(cs) = o.value("children") {
                cs.iter().for_each(|c| lint_paths(c, out));
            }
        }
        "not" => lint_paths(o.value("child"), out),
        "implies" => {
            lint_paths(o.value("a"), out);
            lint_paths(o.value("b"), out);
        }
        _ => {
            if let Some(p) = o.value("path").as_str() {
                out.push(p.to_string());
            }
        }
    }
}

fn enum_uses(e: &Value, out: &mut Vec<(String, Value)>) {
    let Some(o) = e.as_obj() else { return };
    match o.value("op").as_str().unwrap_or("") {
        "and" => {
            if let Value::List(cs) = o.value("children") {
                cs.iter().for_each(|c| enum_uses(c, out));
            }
        }
        "equals" => {
            let p = o.value("path").as_str().unwrap_or("");
            if !p.contains('.') {
                out.push((p.to_string(), o.value("value").clone()));
            }
        }
        "in_set" => {
            let p = o.value("path").as_str().unwrap_or("");
            if !p.contains('.') {
                if let Value::List(vs) = o.value("values") {
                    vs.iter().for_each(|v| out.push((p.to_string(), v.clone())));
                }
            }
        }
        _ => {}
    }
}

fn shown(v: &Value) -> String {
    py_str(v).unwrap_or_default()
}

/// `mcp_rules.rule_lint` over the validated mcp_rules list:
/// (refusals, notes).
/// `perms`, when given, is the envelope's permissions: a path_in whose scope
/// holds a relative glob is refused too.
pub fn lint_rules(rules: &[Value], schemas: &[Loaded], perms: Option<&Value>) -> (Vec<String>, Vec<String>) {
    let mut refusals = Vec::new();
    let mut notes: Vec<String> = Vec::new();
    for r in rules {
        let Some(rule) = r.as_obj() else { continue };
        let tool = rule.value("tool").as_str().unwrap_or("").to_string();
        let args: Vec<Value> = match rule.value("args") {
            Value::List(a) => a.clone(),
            _ => vec![],
        };
        for e in &args {
            lint_exprs(&tool, e, &mut refusals, perms);
        }
        if tool.contains(['*', '?', '[']) || !tool.contains('/') {
            continue;
        }
        let (server, name) = tool.split_once('/').unwrap_or((&tool, ""));
        let Some(schema) = schemas.iter().find(|s| s.server == server) else {
            let note = format!("mcp_rules: no schema on file for {server}; argument names are not checked (daisugi warden schemas import)");
            if !notes.contains(&note) {
                notes.push(note);
            }
            continue;
        };
        let tools = tool_list(&schema.obj);
        let found = tools.iter().rev().find(|t| t.value("name").as_str() == Some(name));
        let Some(found) = found else {
            let mut have: Vec<String> = tools.iter().map(|t| t.value("name").as_str().unwrap_or("").to_string()).collect();
            have.sort();
            have.dedup();
            refusals.push(format!(
                "mcp_rules: {tool}: the schema of {server} has no tool '{name}'; it has: {}",
                have.join(", ")
            ));
            continue;
        };
        let props = match found.value("inputSchema").as_obj().map(|i| i.value("properties")) {
            Some(Value::Obj(p)) => p.clone(),
            _ => Object::new(),
        };
        let mut names: Vec<String> = match rule.value("known") {
            Value::List(k) => k.iter().filter_map(|v| v.as_str().map(String::from)).collect(),
            _ => vec![],
        };
        let mut paths = Vec::new();
        args.iter().for_each(|e| lint_paths(e, &mut paths));
        for p in paths {
            let key = p.split('.').next().unwrap_or("").to_string();
            if !names.contains(&key) {
                names.push(key);
            }
        }
        let mut prop_names: Vec<String> = props.keys().to_vec();
        prop_names.sort();
        for n in &names {
            if props.get(n).is_none() {
                refusals.push(format!("mcp_rules: {tool} names argument '{n}'; the schema has: {}", prop_names.join(", ")));
            }
        }
        for e in &args {
            let mut uses = Vec::new();
            enum_uses(e, &mut uses);
            for (path, value) in uses {
                let Some(Value::List(en)) = props.get(&path).and_then(Value::as_obj).and_then(|p| p.get("enum")) else {
                    continue;
                };
                if !en.iter().any(|ev| same_kind_eq(&value, ev)) {
                    let listed: Vec<String> = en.iter().map(shown).collect();
                    refusals.push(format!(
                        "mcp_rules: {tool}: value '{}' for argument '{path}' is not in the schema enum ({}); the rule can never pass",
                        shown(&value),
                        listed.join(", ")
                    ));
                }
            }
        }
    }
    (refusals, notes)
}
