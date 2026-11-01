//! Argument rules for MCP calls in the standalone verifier:
//! `opendaisugi/mcp_rules.py` and `verify._check_mcp_step` (plan
//! w3-mcp-args-and-parked, MP-1 to MP-10). The warden has its own twin in
//! `warden/mcprules.rs`; the messages are the oracle's in both.

use crate::glob_engine::{head_allowed, normpath, path_matches_any};
use crate::models::{McpRule, Permission, Step};
use crate::predicate::{eval_scalar, parse_expression, with_path_ctx, Expr, PathCtx, Py};
use crate::violation::{kv, Violation};
use crate::warden::pyjson::Value as PValue;
use crate::warden::py::text::repr_list;
use serde_json::Value;

const SPAWN_TOOL_KEY: &str = "opendaisugi/spawn";
const MAX_MCP_ARGUMENTS_CHARS: usize = 256 * 1024;
const MAX_MCP_ARGUMENTS_DEPTH: usize = 64;

fn sv(s: &str) -> PValue {
    PValue::Str(s.to_string())
}

/// `mcp_rules.PATH_IN_ABSOLUTE` (owner, 2026-11-03): an MCP server runs in its
/// own working directory, so a relative path argument cannot be placed.
pub const PATH_IN_ABSOLUTE: &str = "path_in takes absolute paths only";

/// `mcp_rules.place_arg_path`. A relative value is never placed; `cwd` is
/// not read.
pub fn place_arg_path(value: &str, cwd: Option<&str>) -> Option<String> {
    if value.is_empty() {
        return None;
    }
    let space = |c: char| matches!(c, ' ' | '\t' | '\n' | '\r' | '\x0b' | '\x0c');
    if crate::warden::mcprules::uri_scheme(value)
        || value.contains(['\\', '\0'])
        || value.starts_with(space)
        || value.ends_with(space)
    {
        return None;
    }
    if value.starts_with('/') {
        return Some(normpath(value));
    }
    let _ = cwd;
    None
}

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
        Expr::And { children } | Expr::Or { children } => children.iter().for_each(|c| walk_paths(c, out)),
        Expr::Not { child } => walk_paths(child, out),
        Expr::Implies { a, b } => {
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
pub fn known_keys(rule: &McpRule) -> Vec<String> {
    if let Some(k) = &rule.known {
        return k.clone();
    }
    let exprs: Vec<Expr> = rule.args.iter().filter_map(|a| parse_expression(a).ok()).collect();
    let mut paths = Vec::new();
    exprs.iter().for_each(|e| walk_paths(e, &mut paths));
    paths.iter().map(|p| p.split('.').next().unwrap_or("").to_string()).collect()
}

fn describe(e: &Expr) -> String {
    let op = match e {
        Expr::Equals { .. } => "equals",
        Expr::NotEquals { .. } => "not_equals",
        Expr::InSet { .. } => "in_set",
        Expr::NotInSet { .. } => "not_in_set",
        Expr::Matches { .. } => "matches",
        Expr::NotMatches { .. } => "not_matches",
        Expr::NumericRange { .. } => "numeric_range",
        Expr::LengthRange { .. } => "length_range",
        Expr::Exists { .. } => "exists",
        Expr::IsEmpty { .. } => "is_empty",
        Expr::PathIn { .. } => "path_in",
        Expr::And { .. } => "and",
        Expr::Or { .. } => "or",
        Expr::Not { .. } => "not",
        Expr::Implies { .. } => "implies",
        Expr::ForallSteps { .. } => "forall_steps",
        Expr::ExistsStep { .. } => "exists_step",
        Expr::ForallOutputs { .. } => "forall_outputs",
        Expr::ForallWrites { .. } => "forall_writes",
        Expr::DependsOn { .. } => "depends_on",
        Expr::Before { .. } => "before",
        Expr::AliasRef { .. } => "alias",
        Expr::LLMCheck { .. } => "llm_check",
    };
    match expr_path(e) {
        Some(p) => format!("{op} on '{p}'"),
        None => op.to_string(),
    }
}

/// `mcp_rules.matching_rules`.
pub fn matching_rules<'a>(rules: &'a [McpRule], key: &str) -> Vec<&'a McpRule> {
    rules.iter().filter(|r| head_allowed(key, std::slice::from_ref(&r.tool))).collect()
}

fn too_deep(v: &Value, depth: usize) -> bool {
    match v {
        Value::Object(o) => depth + 1 > MAX_MCP_ARGUMENTS_DEPTH || o.values().any(|x| too_deep(x, depth + 1)),
        Value::Array(l) => depth + 1 > MAX_MCP_ARGUMENTS_DEPTH || l.iter().any(|x| too_deep(x, depth + 1)),
        _ => false,
    }
}

/// `mcp_rules.check_caps`.
pub fn check_caps(args: &Value) -> Option<String> {
    if serde_json::to_string(args).map(|s| s.chars().count()).unwrap_or(0) > MAX_MCP_ARGUMENTS_CHARS {
        return Some(format!(
            "mcp arguments are longer than {MAX_MCP_ARGUMENTS_CHARS} characters; the warden does not read them"
        ));
    }
    if too_deep(args, 0) {
        return Some(format!("mcp arguments are nested deeper than {MAX_MCP_ARGUMENTS_DEPTH} levels"));
    }
    None
}

/// `mcp_rules.evaluate_rules`.
fn evaluate_rules(matching: &[&McpRule], key: &str, args: &Value, perms: &Permission, cwd: Option<&str>) -> Vec<String> {
    // A host that joins the server and tool names hides the rule a flat name
    // belongs to: those rules apply too (host_rules in the oracle).
    let tools: Vec<&str> = perms.mcp_rules.iter().map(|r| r.tool.as_str()).collect();
    let extra = crate::rxrisk::host_rule_indexes::<()>(&tools, key, &|h, p| Ok(head_allowed(h, &[p.to_string()]))).unwrap_or_default();
    let mut all: Vec<&McpRule> = matching.to_vec();
    if !matching.is_empty() {
        all.extend(extra.iter().map(|&i| &perms.mcp_rules[i]));
    }
    let matching: &[&McpRule] = &all;
    let prefix = format!("MCP tool '{key}': ");
    if matching.is_empty() {
        return vec![];
    }
    if let Some(c) = check_caps(args) {
        return vec![format!("{prefix}{c}")];
    }
    let Value::Object(obj) = args else { return vec![format!("{prefix}arguments are not an object")] };
    let mut known: Vec<String> = matching.iter().flat_map(|r| known_keys(r)).collect();
    known.sort();
    known.dedup();
    let mut out: Vec<String> = obj
        .keys()
        .filter(|k| !known.contains(k))
        .map(|k| format!("{prefix}argument '{k}' is not named by its rule (known: {})", known.join(", ")))
        .collect();
    if !out.is_empty() {
        return out;
    }
    let ctx = PathCtx { file_read: perms.file_read.clone(), file_write: perms.file_write.clone(), cwd: cwd.map(String::from) };
    with_path_ctx(ctx, || {
        let scope = Py::Json(args.clone());
        for rule in matching {
            for raw in &rule.args {
                let parsed = parse_expression(raw);
                if let Ok(e) = &parsed {
                    let mut rxs = Vec::new();
                    rule_regexes(e, &mut rxs);
                    if let Some(rx) = rxs.iter().find(|rx| !crate::rulere::valid(rx)) {
                        out.push(format!("{prefix}argument rule refused: {}", crate::rulere::invalid_reason(rx)));
                        continue;
                    }
                }
                let shown = parsed.as_ref().map(describe).unwrap_or_else(|_| raw.get("op").and_then(Value::as_str).unwrap_or("").to_string());
                match parsed.and_then(|e| eval_scalar(&e, &scope)) {
                    Ok(true) => {}
                    Ok(false) => out.push(format!("{prefix}argument rule does not hold: {shown}")),
                    Err(e) if e.starts_with(crate::predicate::RULE_REFUSED) => {
                        out.push(format!("{prefix}{}", &e[crate::predicate::RULE_REFUSED.len()..]))
                    }
                    Err(_) => out.push(format!("{prefix}argument rule could not be evaluated: {shown}")),
                }
            }
        }
    });
    out
}

/// `mcp_rules.path_shaped_strings`.
fn path_shaped_strings(args: &Value, home: Option<&str>) -> Vec<(String, String)> {
    fn walk(v: &Value, name: &str, home: Option<&str>, out: &mut Vec<(String, String)>) {
        match v {
            Value::String(s) => {
                if s.starts_with('/') {
                    out.push((name.to_string(), s.clone()));
                } else if let (Some(rest), Some(h)) = (s.strip_prefix("~/"), home) {
                    if !h.is_empty() {
                        out.push((name.to_string(), format!("{}/{rest}", h.trim_end_matches('/'))));
                    }
                }
            }
            Value::Object(o) => {
                for (k, x) in o {
                    let n = if name.is_empty() { k.clone() } else { format!("{name}.{k}") };
                    walk(x, &n, home, out);
                }
            }
            Value::Array(l) => {
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

/// `mcp_rules.ruled_under_host_name`: the tool pattern of a rule that names
/// this host-flattened tool (`opencode/<id>`, `pi/<name>`) when only a
/// wildcard allowlist entry admits the call.
pub fn ruled_under_host_name(perms: &Permission, key: &str) -> Option<String> {
    let (server, flat) = key.split_once('/')?;
    if flat.is_empty() || !matches!(server, "opencode" | "pi") || perms.mcp_allowlist.iter().any(|a| a == key) {
        return None;
    }
    let flat = crate::rxrisk::flat_name(flat);
    perms
        .mcp_rules
        .iter()
        .find(|r| head_allowed(&flat, &[crate::rxrisk::flat_pattern(&r.tool)]))
        .map(|r| r.tool.clone())
}

/// `mcp_rules.rule_regexes`.
fn rule_regexes(e: &Expr, out: &mut Vec<String>) {
    match e {
        Expr::Matches { regex, .. } | Expr::NotMatches { regex, .. } => out.push(regex.clone()),
        Expr::And { children } | Expr::Or { children } => children.iter().for_each(|x| rule_regexes(x, out)),
        Expr::Not { child } => rule_regexes(child, out),
        Expr::Implies { a, b } => {
            rule_regexes(a, out);
            rule_regexes(b, out);
        }
        _ => {}
    }
}

/// `verify._check_mcp_step`.
pub fn check_mcp_step(step: &Step, server: &str, tool: &str, perms: &Permission, call_cwd: Option<&str>) -> Vec<Violation> {
    let key = format!("{server}/{tool}");
    let deny = |message: String, extra: Option<(&str, &str)>| {
        let mut pairs = vec![("step", sv(&step.id)), ("mcp_tool", sv(&key))];
        if let Some((k, v)) = extra {
            pairs.push((k, sv(v)));
        }
        Violation::step("permissions", step.id.clone())
            .msg(format!("Step '{}' {message}", step.id))
            .with(kv(&pairs), None)
    };
    let args = step.raw.get("arguments").cloned().unwrap_or_else(|| Value::Object(Default::default()));
    if let Some(c) = check_caps(&args) {
        return vec![deny(format!("MCP tool '{key}': {c}"), None)];
    }
    let matching = matching_rules(&perms.mcp_rules, &key);
    if !matching.is_empty() {
        let found = evaluate_rules(&matching, &key, &args, perms, call_cwd);
        if !found.is_empty() {
            return found.into_iter().map(|m| deny(m, None)).collect();
        }
    } else if !head_allowed(&key, &perms.mcp_allowlist) {
        return vec![deny(format!("MCP tool '{key}' not in mcp_allowlist {}", repr_list(&perms.mcp_allowlist)), None)];
    } else if let Some(ruled) = ruled_under_host_name(perms, &key) {
        return vec![deny(
            format!(
                "MCP tool '{key}' has an argument rule under '{ruled}'; the host names the tool '{key}', so write the rule with that name"
            ),
            None,
        )];
    }
    let mut out = Vec::new();
    let home = std::env::var("HOME").ok();
    for (name, placed) in path_shaped_strings(&args, home.as_deref()) {
        if key == SPAWN_TOOL_KEY && name == "path" {
            continue;
        }
        if std::fs::symlink_metadata(&placed).is_err() {
            continue;
        }
        let norm = normpath(&placed);
        let ok = if std::fs::metadata(&placed).map(|m| m.is_dir()).unwrap_or(false) {
            path_matches_any(&norm, &perms.file_read) && path_matches_any(&format!("{norm}/_/_"), &perms.file_read)
        } else {
            path_matches_any(&norm, &perms.file_read)
        };
        if !ok {
            out.push(deny(
                format!(
                    "MCP tool '{key}': argument '{name}' reads {placed} outside file_read {}; name it in mcp_rules with path_in to allow it",
                    repr_list(&perms.file_read)
                ),
                Some(("argument", name.as_str())),
            ));
        }
    }
    out
}

/// True when the two tool patterns provably match no common string: both
/// are literals and differ. Anything else may overlap.
fn disjoint(a: &str, b: &str) -> bool {
    let lit = |s: &str| !s.contains(['*', '?', '[']);
    lit(a) && lit(b) && a != b
}

/// Subsumption twin of `subsumption._mcp_rules_subsume`, narrower than the
/// oracle's Z3 proof: an outer rule covers an inner one only when every
/// expression of the outer rule is also an expression of the inner rule
/// (equal as JSON), so a proof the oracle finds by reasoning is a
/// refusal here (fail closed; ruling MP-R-1). True means "violates".
pub fn rules_violate(inner: &Permission, outer: &Permission, patterns_subsume: &dyn Fn(&str, &[String]) -> bool) -> bool {
    for rule in &inner.mcp_rules {
        let overlapping: Vec<&McpRule> = outer.mcp_rules.iter().filter(|o| !disjoint(&rule.tool, &o.tool)).collect();
        if overlapping.is_empty() {
            if !patterns_subsume(&rule.tool, &outer.mcp_allowlist) {
                return true;
            }
            continue;
        }
        for o in overlapping {
            if !patterns_subsume(&rule.tool, std::slice::from_ref(&o.tool)) {
                return true;
            }
            let have = known_keys(rule);
            let outer_known = known_keys(o);
            if have.iter().any(|k| !outer_known.contains(k)) {
                return true;
            }
            if !o.args.iter().all(|oa| rule.args.iter().any(|ia| ia == oa)) {
                return true;
            }
        }
    }
    for pattern in &inner.mcp_allowlist {
        if !pattern.contains('*') && inner.mcp_rules.iter().any(|r| head_allowed(pattern, std::slice::from_ref(&r.tool))) {
            continue;
        }
        if outer.mcp_rules.iter().any(|o| !disjoint(pattern, &o.tool)) {
            return true;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_absolute_paths_are_placed() {
        assert_eq!(place_arg_path("/work/x/y", Some("/work")).as_deref(), Some("/work/x/y"));
        assert_eq!(place_arg_path("x/y", Some("/work")), None);
        assert_eq!(place_arg_path("../x", Some("/work/sub")), None);
        assert_eq!(place_arg_path("file:///etc/passwd", Some("/work")), None);
        assert_eq!(place_arg_path("C:/x", Some("/work")), None);
        assert_eq!(place_arg_path(" /a", Some("/work")), None);
        assert_eq!(place_arg_path("/a\n", Some("/work")), None);
        assert_eq!(place_arg_path("x", None), None);
        assert_eq!(place_arg_path("~/x", Some("/w")), None);
    }

    fn rule(tool: &str, args: &[&str]) -> McpRule {
        McpRule { tool: tool.into(), args: args.iter().map(|a| serde_json::from_str(a).unwrap()).collect(), known: None }
    }

    #[test]
    fn a_rule_is_subsumed_only_by_an_outer_rule_that_holds_all_its_expressions() {
        let eq = r#"{"op":"equals","path":"repo","value":"acme/app"}"#;
        let rng = r#"{"op":"numeric_range","path":"n","min":1,"max":9}"#;
        let yes = |_: &str, _: &[String]| true;
        let mut inner = Permission::default();
        let mut outer = Permission::default();
        inner.mcp_rules = vec![rule("gh/get", &[eq, rng])];
        outer.mcp_rules = vec![rule("gh/get", &[eq])];
        // The outer rule names only "repo": the inner rule's "n" is a wider set of keys.
        assert!(rules_violate(&inner, &outer, &yes));
        outer.mcp_rules[0].known = Some(vec!["repo".into(), "n".into()]);
        assert!(!rules_violate(&inner, &outer, &yes));
        outer.mcp_rules = vec![rule("gh/get", &[eq, rng, r#"{"op":"exists","path":"z"}"#])];
        outer.mcp_rules[0].known = Some(vec!["repo".into(), "n".into(), "z".into()]);
        assert!(rules_violate(&inner, &outer, &yes));
        // An inner allowlist entry for a tool the outer constrains by rule is wider.
        inner.mcp_rules = vec![];
        inner.mcp_allowlist = vec!["gh/get".into()];
        assert!(rules_violate(&inner, &outer, &yes));
        outer.mcp_rules = vec![rule("gh/other", &[eq])];
        assert!(!rules_violate(&inner, &outer, &yes));
    }

    fn mcp_step() -> Step {
        Step {
            id: "s0".into(),
            step_type: "mcp".into(),
            depends_on: vec![],
            preferred_model: None,
            raw: serde_json::json!({}),
            kind: crate::models::StepKind::Unknown,
        }
    }

    fn perms_with(rules: &str, allow: &[&str]) -> Permission {
        let mut p = Permission::default();
        p.mcp_rules = serde_json::from_str::<Vec<serde_json::Value>>(rules)
            .unwrap()
            .into_iter()
            .map(|r| McpRule {
                tool: r["tool"].as_str().unwrap().into(),
                args: r["args"].as_array().unwrap().clone(),
                known: None,
            })
            .collect();
        p.mcp_allowlist = allow.iter().map(|a| a.to_string()).collect();
        p
    }

    fn decide(p: &Permission, server: &str, tool: &str, args: Value) -> Vec<String> {
        let mut step = mcp_step();
        step.raw = serde_json::json!({ "arguments": args });
        check_mcp_step(&step, server, tool, p, Some("/work")).into_iter().map(|v| v.message).collect()
    }

    fn rx_rule(rx: &str) -> Permission {
        perms_with(&serde_json::json!([{"tool":"github/get_issue","args":[{"op":"matches","path":"repo","regex":rx}]}]).to_string(), &[])
    }

    /// A rule regex tests the whole string, not a search: `acme/.*` does not
    /// admit `evil/acme/app`.
    #[test]
    fn a_rule_regex_matches_the_whole_string() {
        let p = rx_rule("acme/.*");
        for (repo, denied) in [("acme/app", false), ("evil/acme/app", true), ("acme/app\n", true)] {
            let got = decide(&p, "github", "get_issue", serde_json::json!({ "repo": repo }));
            assert_eq!(!got.is_empty(), denied, "{repo:?}: {got:?}");
        }
    }

    /// A pattern outside the subset denies with one reason: a stray `)`,
    /// lookahead, lookbehind, a backreference, a repeat over 1000, a flag
    /// group, a `\p` name.
    #[test]
    fn a_pattern_outside_the_subset_denies_with_one_reason() {
        for rx in ["x)|(?:.*", "^a)|(b$", "^(a$", "^(?=a)a$", "^(?!a).*$", "^(?<=a)b$", "^(a)\\1$", "^a{1001}$", "(?x)a|.*", "[!--/]", "\\p{Ascii}", "\\p{Letter}"] {
            let got = decide(&rx_rule(rx), "github", "get_issue", serde_json::json!({ "repo": "aEVIL" }));
            assert_eq!(
                got,
                vec![format!("Step 's0' MCP tool 'github/get_issue': argument rule refused: regex '{rx}' is outside the supported subset")],
                "{rx}"
            );
        }
    }

    #[test]
    fn backtracking_shapes_answer_at_once_on_4096_characters() {
        let start = std::time::Instant::now();
        for rx in ["^(a|aa)+$", "^(a+)+$", "^(.*)*$", "^a*a*a*a*b$"] {
            for val in ["a".repeat(4096), "a".repeat(4095) + "!"] {
                decide(&rx_rule(rx), "github", "get_issue", serde_json::json!({ "repo": val }));
            }
        }
        assert!(start.elapsed().as_secs() < 1);
    }

    #[test]
    fn a_long_string_denies() {
        let p = rx_rule("^[a-z]+$");
        assert!(decide(&p, "github", "get_issue", serde_json::json!({ "repo": "a".repeat(4096) })).is_empty());
        assert_eq!(
            decide(&p, "github", "get_issue", serde_json::json!({ "repo": "a".repeat(4097) })),
            vec!["Step 's0' MCP tool 'github/get_issue': argument rule refused: argument 'repo' is longer than 4096 characters"]
        );
    }

    /// The host-name guard: a wildcard allowlist entry does not admit a tool a
    /// rule names, and a wildcard rule does not skip that rule.
    #[test]
    fn a_host_flattened_tool_is_not_admitted_by_a_wildcard_when_a_rule_names_it() {
        let rules = r#"[{"tool":"github/get_issue","args":[{"op":"in_set","path":"repo","values":["acme/app"]}]},
                         {"tool":"opencode/*","args":[{"op":"not_matches","path":"repo","regex":"^secret$"}]}]"#;
        let p = perms_with(rules, &["opencode/*"]);
        let bad = decide(&p, "opencode", "github_get_issue", serde_json::json!({ "repo": "evil" }));
        assert_eq!(bad.len(), 1, "{bad:?}");
        assert!(bad[0].contains("in_set on 'repo'"), "{}", bad[0]);
        assert!(decide(&p, "opencode", "github_get_issue", serde_json::json!({ "repo": "acme/app" })).is_empty());
        assert!(decide(&p, "opencode", "slack_post", serde_json::json!({ "repo": "x" })).is_empty());
        // No wildcard rule: the allowlist branch denies a ruled tool by its flat name, case and separators aside.
        let mut p = perms_with(r#"[{"tool":"github/get_issue","args":[]}]"#, &["opencode/*", "pi/*"]);
        for key in ["pi/GitHub_get_issue", "pi/github-get_issue", "opencode/GITHUB_get_issue", "opencode/github_get_issue"] {
            assert_eq!(ruled_under_host_name(&p, key).as_deref(), Some("github/get_issue"), "{key}");
        }
        p.mcp_allowlist.push("opencode/github_get_issue".into());
        assert_eq!(ruled_under_host_name(&p, "opencode/github_get_issue"), None);
    }
}
