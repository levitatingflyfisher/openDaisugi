//! Argument rules for MCP calls: `opendaisugi/mcp_rules.py` and
//! `verify._check_mcp_step` (plan w3-mcp-args-and-parked, MP-1 to MP-10).
//! Every message is the oracle's, byte for byte.

use super::check::{repr_list, Violation};
use super::envelope::{Envelope, McpRule};
use super::globs::{head_allowed, path_matches_any};
use super::paths::{isabs, lexists, normpath, os_path};
use super::predicate::{Expr, Stage};
use super::pyjson::{canonical_json, Object, Value};
use super::record::Record;
use super::{catch, Runner, R};

/// `verify._SPAWN_TOOL_KEY`: the delegate's spawn tool, whose path the warden
/// reads with a check of its own.
const SPAWN_TOOL_KEY: &str = "opendaisugi/spawn";
const MAX_MCP_ARGUMENTS_CHARS: usize = 256 * 1024;
const MAX_MCP_ARGUMENTS_DEPTH: usize = 64;

/// `mcp_rules.place_arg_path`: None when the path cannot be placed. A
/// relative value is never placed (owner, 2026-11-03); `cwd` is not read.
pub(super) fn place_arg_path(value: &str, cwd: Option<&str>) -> Option<String> {
    if value.is_empty() {
        return None;
    }
    let space = |c: char| matches!(c, ' ' | '\t' | '\n' | '\r' | '\x0b' | '\x0c');
    if uri_scheme(value)
        || value.contains(['\\', '\0'])
        || value.starts_with(space)
        || value.ends_with(space)
    {
        return None;
    }
    if isabs(value) {
        return Some(normpath(value));
    }
    let _ = cwd;
    None
}

/// `_URI_SCHEME.match`: a letter, then letters, digits and `+ . -`, then a
/// colon. A Windows drive prefix is the one-letter case.
pub(crate) fn uri_scheme(s: &str) -> bool {
    let mut chars = s.chars();
    if !chars.next().is_some_and(|c| c.is_ascii_alphabetic()) {
        return false;
    }
    for c in chars {
        match c {
            ':' => return true,
            c if c.is_ascii_alphanumeric() || matches!(c, '+' | '.' | '-') => {}
            _ => return false,
        }
    }
    false
}

/// `mcp_rules.ruled_under_host_name`: the tool pattern of a rule that names
/// this host-flattened tool (`opencode/<id>`, `pi/<name>`) when only an
/// allowlist entry for another name admits the call.
fn ruled_under_host_name(env: &Envelope, key: &str) -> R<Option<String>> {
    let Some((server, flat)) = key.split_once('/') else { return Ok(None) };
    if flat.is_empty() || !matches!(server, "opencode" | "pi") || env.mcp_allowlist.iter().any(|a| a == key) {
        return Ok(None);
    }
    let flat = crate::rxrisk::flat_name(flat);
    for rule in &env.mcp_rules {
        if head_allowed(&flat, &[crate::rxrisk::flat_pattern(&rule.tool)])? {
            return Ok(Some(rule.tool.clone()));
        }
    }
    Ok(None)
}

/// `mcp_rules.rule_regexes`.
fn rule_regexes(e: &Expr, out: &mut Vec<String>) {
    match e {
        Expr::Matches { regex, .. } | Expr::NotMatches { regex, .. } => out.push(regex.clone()),
        Expr::And(cs) | Expr::Or(cs) => cs.iter().for_each(|c| rule_regexes(c, out)),
        Expr::Not(c) => rule_regexes(c, out),
        Expr::Implies(a, b) => {
            rule_regexes(a, out);
            rule_regexes(b, out);
        }
        _ => {}
    }
}

/// The `path` attribute of an expression that has one.
fn expr_path(e: &Expr) -> Option<&str> {
    match e {
        Expr::Equals { path, .. }
        | Expr::NotEquals { path, .. }
        | Expr::InSet { path, .. }
        | Expr::NotInSet { path, .. }
        | Expr::Matches { path, .. }
        | Expr::NotMatches { path, .. }
        | Expr::NumericRange { path, .. }
        | Expr::LengthRange { path, .. }
        | Expr::Exists { path }
        | Expr::IsEmpty { path }
        | Expr::PathIn { path, .. } => Some(path),
        _ => None,
    }
}

fn walk_paths<'a>(e: &'a Expr, out: &mut Vec<&'a str>) {
    match e {
        Expr::And(cs) | Expr::Or(cs) => cs.iter().for_each(|c| walk_paths(c, out)),
        Expr::Not(c) => walk_paths(c, out),
        Expr::Implies(a, b) => {
            walk_paths(a, out);
            walk_paths(b, out);
        }
        other => {
            if let Some(p) = expr_path(other) {
                out.push(p);
            }
        }
    }
}

/// `mcp_rules.known_keys`.
fn known_keys(rule: &McpRule) -> Vec<String> {
    if let Some(k) = &rule.known {
        return k.clone();
    }
    let mut paths = Vec::new();
    rule.args.iter().for_each(|e| walk_paths(e, &mut paths));
    paths.iter().map(|p| p.split('.').next().unwrap_or("").to_string()).collect()
}

fn describe(e: &Expr) -> String {
    match expr_path(e) {
        Some(p) => format!("{} on '{p}'", e.op()),
        None => e.op().to_string(),
    }
}

/// `mcp_rules.matching_rules`.
pub(super) fn matching_rules<'a>(rules: &'a [McpRule], key: &str) -> R<Vec<&'a McpRule>> {
    let mut out = Vec::new();
    for r in rules {
        if head_allowed(key, std::slice::from_ref(&r.tool))? {
            out.push(r);
        }
    }
    Ok(out)
}

fn too_deep(v: &Value, depth: usize) -> bool {
    match v {
        Value::Obj(o) => {
            let d = depth + 1;
            d > MAX_MCP_ARGUMENTS_DEPTH || o.keys().iter().any(|k| too_deep(o.value(k), d))
        }
        Value::List(l) | Value::Tuple(l) => {
            let d = depth + 1;
            d > MAX_MCP_ARGUMENTS_DEPTH || l.iter().any(|e| too_deep(e, d))
        }
        _ => false,
    }
}

/// `mcp_rules.check_caps`: None when the arguments are within both caps.
pub(super) fn check_caps(args: &Value) -> Option<String> {
    if canonical_json(args).chars().count() > MAX_MCP_ARGUMENTS_CHARS {
        return Some(format!(
            "mcp arguments are longer than {MAX_MCP_ARGUMENTS_CHARS} characters; the warden does not read them"
        ));
    }
    if too_deep(args, 0) {
        return Some(format!("mcp arguments are nested deeper than {MAX_MCP_ARGUMENTS_DEPTH} levels"));
    }
    None
}

/// `mcp_rules.path_shaped_strings`.
fn path_shaped_strings(args: &Value, home: Option<&str>) -> Vec<(String, String)> {
    fn walk(v: &Value, name: &str, home: Option<&str>, out: &mut Vec<(String, String)>) {
        match v {
            Value::Str(s) => {
                if s.starts_with('/') {
                    out.push((name.to_string(), s.clone()));
                } else if let (Some(rest), Some(h)) = (s.strip_prefix("~/"), home) {
                    if !h.is_empty() {
                        out.push((name.to_string(), format!("{}/{rest}", h.trim_end_matches('/'))));
                    }
                }
            }
            Value::Obj(o) => {
                for k in o.keys() {
                    let n = if name.is_empty() { k.clone() } else { format!("{name}.{k}") };
                    walk(o.value(k), &n, home, out);
                }
            }
            Value::List(l) | Value::Tuple(l) => {
                for (i, e) in l.iter().enumerate() {
                    walk(e, &format!("{name}[{i}]"), home, out);
                }
            }
            _ => {}
        }
    }
    let mut out = Vec::new();
    walk(args, "", home, &mut out);
    out
}

fn is_dir(p: &str) -> bool {
    os_path("stat", p).map(|x| std::fs::metadata(x).map(|m| m.is_dir()).unwrap_or(false)).unwrap_or(false)
}

impl Runner {
    /// `mcp_rules.evaluate_rules`: the violation messages for one call.
    fn evaluate_mcp_rules(
        &self,
        matching: &[&McpRule],
        env: &Envelope,
        key: &str,
        args: &Value,
        cwd: Option<&str>,
    ) -> R<Vec<String>> {
        let prefix = format!("MCP tool '{key}': ");
        if matching.is_empty() {
            return Ok(vec![]);
        }
        // A host that joins the server and tool names hides the rule a flat
        // name belongs to: those rules apply too (host_rules in the oracle).
        let tools: Vec<&str> = env.mcp_rules.iter().map(|r| r.tool.as_str()).collect();
        let extra = crate::rxrisk::host_rule_indexes(&tools, key, &|h, p| head_allowed(h, &[p.to_string()]))?;
        let mut all: Vec<&McpRule> = matching.to_vec();
        all.extend(extra.iter().map(|&i| &env.mcp_rules[i]));
        let matching: &[&McpRule] = &all;
        if let Some(c) = check_caps(args) {
            return Ok(vec![format!("{prefix}{c}")]);
        }
        let Value::Obj(obj) = args else {
            return Ok(vec![format!("{prefix}arguments are not an object")]);
        };
        let mut known: Vec<String> = matching.iter().flat_map(|r| known_keys(r)).collect();
        known.sort();
        known.dedup();
        let mut out = Vec::new();
        for k in obj.keys() {
            if !known.contains(k) {
                out.push(format!(
                    "{prefix}argument '{k}' is not named by its rule (known: {})",
                    known.join(", ")
                ));
            }
        }
        if !out.is_empty() {
            return Ok(out);
        }
        let stage = Stage::new(self, env, Value::Null, false);
        *stage.rule_cwd.borrow_mut() = Some(cwd.map(str::to_string));
        for rule in matching {
            for e in &rule.args {
                let mut rxs = Vec::new();
                rule_regexes(e, &mut rxs);
                if let Some(rx) = rxs.iter().find(|rx| !crate::rulere::valid(rx)) {
                    out.push(format!("{prefix}argument rule refused: {}", crate::rulere::invalid_reason(rx)));
                    continue;
                }
                match catch(stage.eval_scalar(e, args))? {
                    Ok(true) => {}
                    Ok(false) => out.push(format!("{prefix}argument rule does not hold: {}", describe(e))),
                    Err(x) if x.is("RuleRefused") => out.push(format!("{prefix}{}", x.msg)),
                    Err(_) => out.push(format!("{prefix}argument rule could not be evaluated: {}", describe(e))),
                }
            }
        }
        Ok(out)
    }

    /// `verify._check_mcp_step`: the permission stage for one MCP call.
    pub(super) fn check_mcp_step(
        &self,
        rec: &Record,
        env: &Envelope,
        step: &str,
        call_cwd: Option<&str>,
    ) -> R<Vec<Violation>> {
        let key = format!("{}/{}", rec.mcp_server, rec.mcp_tool);
        let deny = |message: String, extra: Option<(&str, &str)>| {
            let mut d = Object::new().with("step", step).with("mcp_tool", key.as_str());
            if let Some((k, v)) = extra {
                d.set(k, v);
            }
            Violation::new("permissions", format!("Step '{step}' {message}"), d)
        };
        // The host's value when it sent no object (MCPStep.call_arguments).
        let args = match &rec.raw_arguments {
            Some(raw) => raw.clone(),
            None => Value::Obj(rec.arguments.clone()),
        };
        if let Some(c) = check_caps(&args) {
            return Ok(vec![deny(format!("MCP tool '{key}': {c}"), None)]);
        }
        let matching = matching_rules(&env.mcp_rules, &key)?;
        if !matching.is_empty() {
            let found = self.evaluate_mcp_rules(&matching, env, &key, &args, call_cwd)?;
            if !found.is_empty() {
                return Ok(found.into_iter().map(|m| deny(m, None)).collect());
            }
        } else if !head_allowed(&key, &env.mcp_allowlist)? {
            return Ok(vec![deny(
                format!("MCP tool '{key}' not in mcp_allowlist {}", repr_list(&env.mcp_allowlist)?),
                None,
            )]);
        } else if let Some(ruled) = ruled_under_host_name(env, &key)? {
            return Ok(vec![deny(
                format!(
                    "MCP tool '{key}' has an argument rule under '{ruled}'; the host names the tool '{key}', so write the rule with that name"
                ),
                None,
            )]);
        }
        let mut out = Vec::new();
        let home = self.env.get("HOME").map(String::as_str);
        for (name, placed) in path_shaped_strings(&args, home) {
            if key == SPAWN_TOOL_KEY && name == "path" {
                continue; // the warden's spawn check reads this file, with its own tier and words
            }
            if !lexists(&placed) {
                continue;
            }
            let norm = normpath(&placed);
            let ok = if is_dir(&placed) {
                path_matches_any(&norm, &env.file_read)? && path_matches_any(&format!("{norm}/_/_"), &env.file_read)?
            } else {
                path_matches_any(&norm, &env.file_read)?
            };
            if !ok {
                out.push(deny(
                    format!(
                        "MCP tool '{key}': argument '{name}' reads {placed} outside file_read {}; name it in mcp_rules with path_in to allow it",
                        repr_list(&env.file_read)?
                    ),
                    Some(("argument", name.as_str())),
                ));
            }
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::warden::envelope::parse_envelope;
    use crate::warden::predicate::kind_eq;

    #[test]
    fn paths_are_placed_as_the_file_tools_place_them() {
        assert_eq!(place_arg_path("/a/../b", None).as_deref(), Some("/b"));
        assert_eq!(place_arg_path("x/y", Some("/work")), None);
        assert_eq!(place_arg_path("../../etc/passwd", Some("/work/sub")), None);
        for bad in [
            "a/b/../c", "a/..", "file:///etc/passwd", "C:/x", "c:x", "a\\b", "..\\..\\x", " /a", "/a\n", "/a ",
            "a\0b",
        ] {
            assert_eq!(place_arg_path(bad, Some("/work")), None, "{bad:?}");
        }
        assert_eq!(place_arg_path("a..b/c", Some("/work")), None);
        assert_eq!(place_arg_path("b/c", Some("/work")), None);
        assert_eq!(place_arg_path("x", None), None);
        assert_eq!(place_arg_path("~/x", Some("/work")), None);
        assert_eq!(place_arg_path("$HOME/x", Some("/work")), None);
        assert_eq!(place_arg_path("", Some("/work")), None);
    }

    #[test]
    fn a_host_flattened_tool_is_not_admitted_by_a_wildcard_when_a_rule_names_it() {
        let mut e = parse_envelope(
            r#"{"generated_by":"g","task":"t","permissions":{"mcp_allowlist":["opencode/*","pi/*"],"mcp_rules":[
                {"tool":"github/get_issue","args":[{"op":"exists","path":"repo"}]},{"tool":"gh/*","args":[{"op":"exists","path":"q"}]}]}}"#,
        )
        .unwrap();
        for (key, want) in [
            ("opencode/github_get_issue", Some("github/get_issue")),
            ("pi/github_get_issue", Some("github/get_issue")),
            ("opencode/gh_anything", Some("gh/*")),
            ("opencode/slack_post", None),
            ("github/get_issue", None),
        ] {
            assert_eq!(ruled_under_host_name(&e, key).unwrap().as_deref(), want, "{key}");
        }
        e.mcp_allowlist.push("opencode/github_get_issue".into());
        assert_eq!(ruled_under_host_name(&e, "opencode/github_get_issue").unwrap(), None);
    }

    #[test]
    fn the_caps_are_counts() {
        let long = Value::Obj(Object::new().with("a", "x".repeat(MAX_MCP_ARGUMENTS_CHARS)));
        assert!(check_caps(&long).unwrap().starts_with("mcp arguments are longer than 262144 characters"));
        let mut deep = Value::Null;
        for _ in 0..65 {
            deep = Value::List(vec![deep]);
        }
        assert_eq!(check_caps(&deep).unwrap(), "mcp arguments are nested deeper than 64 levels");
        assert!(check_caps(&Value::Obj(Object::new().with("a", 1i64))).is_none());
    }

    #[test]
    fn a_bool_is_never_a_number() {
        assert!(!kind_eq(&Value::Bool(true), &Value::Int("1".into())));
        assert!(!kind_eq(&Value::Int("1".into()), &Value::Bool(true)));
        assert!(kind_eq(&Value::Bool(true), &Value::Bool(true)));
        assert!(kind_eq(&Value::Int("1".into()), &Value::Float(1.0)));
    }

    #[test]
    fn an_envelope_carries_its_rules() {
        let e = parse_envelope(
            r#"{"generated_by":"g","task":"t","permissions":{"mcp_rules":[{"tool":"github/get_issue","args":[
                {"op":"equals","path":"repo","value":"acme/app"},
                {"op":"path_in","path":"file","scope":"file_read"}]}]}}"#,
        )
        .unwrap();
        assert_eq!(e.mcp_rules.len(), 1);
        assert_eq!(e.mcp_rules[0].args.len(), 2);
        assert_eq!(known_keys(&e.mcp_rules[0]), vec!["repo".to_string(), "file".to_string()]);
        let dumped = e.dump.value("permissions").as_obj().unwrap().value("mcp_rules").clone();
        assert_eq!(
            crate::warden::pyjson::dumps(&dumped, false),
            r#"[{"tool": "github/get_issue", "args": [{"op": "equals", "path": "repo", "value": "acme/app"}, {"op": "path_in", "path": "file", "scope": "file_read"}], "known": null}]"#
        );
    }

    #[test]
    fn a_bad_rule_reads_as_pydantics() {
        let err = match parse_envelope(
            r#"{"generated_by":"g","task":"t","permissions":{"mcp_rules":[{"tool":"a/b","args":[{"op":"path_in","path":"p","scope":"nope"}]}]}}"#,
        ) {
            Err(crate::warden::Fault::Raised(e)) => e.msg,
            other => panic!("expected an error, got {other:?}"),
        };
        assert!(err.contains("permissions.mcp_rules.0.args.0.path_in.scope\n  Input should be 'file_read' or 'file_write' [type=literal_error"), "{err}");
    }
}
