//! Port of `opendaisugi/verify.py`: the permission stage, delegation
//! safety, the predicate-algebra stage, and the `_verify` pipeline
//! orchestration (short-circuit order between stages).

use crate::glob_engine;
use crate::interpreter_parse::parse_interpreter;
use crate::models::{self, ActionPlan, Envelope, Permission, Step, StepKind};
use crate::predicate::{self, Expr};
use crate::shell_decompose::{Decomposition, ShellParser};
use crate::subsumption;
use crate::violation::Violation;
use crate::z3_bridge::Vacuity;
use crate::z3_checks;
use serde_json::Value;
use std::sync::OnceLock;

const MAX_INTERPRETER_DEPTH: u32 = 4;

fn metachar_re() -> &'static regex::Regex {
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r"[;|&`<>\n\r]|\$\(").unwrap())
}

/// `_SHELL_METACHAR_RE.search(command)` exposed for fixture testing.
pub fn shell_metachar_hit(command: &str) -> bool {
    metachar_re().is_match(command)
}

fn env_assign_re() -> &'static regex::Regex {
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r"^[A-Za-z_][A-Za-z0-9_]*=").unwrap())
}

/// `_extract_shell_head`.
pub fn extract_shell_head(stripped: &str) -> Option<String> {
    if stripped.is_empty() || stripped.starts_with('#') {
        return None;
    }
    for tok in stripped.split_whitespace() {
        if env_assign_re().is_match(tok) {
            continue;
        }
        return Some(tok.to_string());
    }
    None
}

const SANCTIONED_WRITE_SINKS: [&str; 3] = ["/dev/null", "/dev/stdout", "/dev/stderr"];
const SANCTIONED_READ_SOURCES: [&str; 2] = ["/dev/null", "/dev/stdin"];

/// `_check_redirect_scopes`.
fn check_redirect_scopes(decomp: &Decomposition, step_id: &str, perms: &Permission) -> Vec<Violation> {
    let mut out = Vec::new();
    for path in &decomp.writes {
        if SANCTIONED_WRITE_SINKS.contains(&path.as_str()) {
            continue;
        }
        if !glob_engine::path_matches_any(path, &perms.file_write) {
            out.push(Violation::step("permissions", step_id.to_string()));
        }
    }
    for path in &decomp.reads {
        if SANCTIONED_READ_SOURCES.contains(&path.as_str()) {
            continue;
        }
        if !glob_engine::path_matches_any(path, &perms.file_read) {
            out.push(Violation::step("permissions", step_id.to_string()));
        }
    }
    out
}

/// `_check_shell_command`.
fn check_shell_command(
    command: &str,
    step_id: &str,
    perms: &Permission,
    policy: &str,
    depth: u32,
    shell_parser: &mut ShellParser,
) -> Vec<Violation> {
    if depth > MAX_INTERPRETER_DEPTH {
        return vec![Violation::step("permissions", step_id.to_string())];
    }
    let stripped = command.trim();
    if stripped.is_empty() {
        return vec![];
    }
    if metachar_re().is_match(command) {
        if perms.shell_allow_decomposition && depth <= MAX_INTERPRETER_DEPTH {
            let decomp = shell_parser.decompose(command);
            if decomp.ok {
                let mut violations = check_redirect_scopes(&decomp, step_id, perms);
                for simple in &decomp.commands {
                    violations.extend(verify_simple_command(simple, step_id, perms, policy, depth, shell_parser));
                }
                return violations;
            }
        }
        return vec![Violation::step("permissions", step_id.to_string())];
    }
    verify_simple_command(stripped, step_id, perms, policy, depth, shell_parser)
}

/// `_verify_simple_command`. Callers guarantee `command` is one simple
/// command. `parse_interpreter` is called on the ORIGINAL `command`
/// argument (not a re-stripped copy) to match the oracle exactly — it
/// strips internally.
fn verify_simple_command(
    command: &str,
    step_id: &str,
    perms: &Permission,
    policy: &str,
    depth: u32,
    shell_parser: &mut ShellParser,
) -> Vec<Violation> {
    let stripped = command.trim();
    let head = match extract_shell_head(stripped) {
        Some(h) => h,
        None => return vec![],
    };
    if !glob_engine::head_allowed(&head, &perms.shell_allowlist) {
        return vec![Violation::step("permissions", step_id.to_string())];
    }
    let payload = match parse_interpreter(command) {
        Some(p) => p,
        None => return vec![],
    };
    if payload.opaque {
        if policy == "strict" {
            return vec![Violation::step("permissions", step_id.to_string())];
        }
        return vec![];
    }
    let mut out = Vec::new();
    for inner in &payload.inner_commands {
        out.extend(check_shell_command(inner, step_id, perms, policy, depth + 1, shell_parser));
    }
    out
}

const AGENTIC_TOOL_CAPABILITIES: &[(&str, &str)] = &[
    ("Bash", "shell"),
    ("Read", "file_read"),
    ("Glob", "file_read"),
    ("Grep", "file_read"),
    ("Write", "file_write"),
    ("Edit", "file_write"),
    ("MultiEdit", "file_write"),
    ("WebFetch", "network"),
    ("WebSearch", "network"),
];

fn capability_granted(perms: &Permission, cap: &str) -> bool {
    match cap {
        "shell" => perms.shell,
        "file_read" => !perms.file_read.is_empty(),
        "file_write" => !perms.file_write.is_empty(),
        "network" => perms.network,
        _ => false,
    }
}

/// `_check_agentic_step`.
fn check_agentic_step(step: &Step, perms: &Permission) -> Vec<Violation> {
    let (workspace, tools) = match &step.kind {
        StepKind::Agentic { workspace, tools } => (workspace, tools),
        _ => return vec![],
    };
    let mut out = Vec::new();
    if tools.is_empty() {
        out.push(Violation::step("permissions", step.id.clone()));
        return out;
    }
    if !glob_engine::path_matches_any(workspace, &perms.file_read) {
        out.push(Violation::step("permissions", step.id.clone()));
    }
    for tool in tools {
        match AGENTIC_TOOL_CAPABILITIES.iter().find(|(n, _)| n == tool) {
            None => out.push(Violation::step("permissions", step.id.clone())),
            Some((_, cap)) => {
                if !capability_granted(perms, cap) {
                    out.push(Violation::step("permissions", step.id.clone()));
                }
            }
        }
    }
    out
}

/// Minimal `urllib.parse.urlparse` scheme+hostname extraction — only used
/// where the oracle uses it (network step scheme/host gating).
fn parse_url_scheme_host(url: &str) -> (String, String) {
    let colon = match url.find(':') {
        Some(i) => i,
        None => return (String::new(), String::new()),
    };
    let candidate = &url[..colon];
    let scheme = if !candidate.is_empty()
        && candidate.chars().next().unwrap().is_ascii_alphabetic()
        && candidate.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '-' | '.'))
    {
        candidate.to_lowercase()
    } else {
        String::new()
    };
    let mut host = String::new();
    let rest = &url[colon + 1..];
    if let Some(after_slashes) = rest.strip_prefix("//") {
        let end = after_slashes.find(['/', '?', '#']).unwrap_or(after_slashes.len());
        let mut authority = &after_slashes[..end];
        if let Some(at) = authority.rfind('@') {
            authority = &authority[at + 1..];
        }
        if let Some(inner) = authority.strip_prefix('[') {
            if let Some(close) = inner.find(']') {
                host = inner[..close].to_lowercase();
            }
        } else if let Some(colon2) = authority.rfind(':') {
            host = authority[..colon2].to_lowercase();
        } else {
            host = authority.to_lowercase();
        }
    }
    (scheme, host)
}

/// `check_permissions` — Stage 1.
pub fn check_permissions(plan: &ActionPlan, env: &Envelope, strict: bool, shell_parser: &mut ShellParser) -> Vec<Violation> {
    let mut out = Vec::new();
    let perms = &env.permissions;
    for step in &plan.steps {
        match &step.kind {
            StepKind::Shell { command } => {
                if !perms.shell {
                    out.push(Violation::step("permissions", step.id.clone()));
                    continue;
                }
                out.extend(check_shell_command(command, &step.id, perms, &env.shell_interpreter_policy, 0, shell_parser));
            }
            StepKind::Network { url } => {
                if !perms.network {
                    out.push(Violation::step("permissions", step.id.clone()));
                    continue;
                }
                let (scheme, host) = parse_url_scheme_host(url);
                if scheme != "http" && scheme != "https" {
                    out.push(Violation::step("permissions", step.id.clone()));
                    continue;
                }
                if !perms.network_hosts.is_empty() {
                    let allowed: std::collections::HashSet<String> =
                        perms.network_hosts.iter().map(|h| h.to_lowercase()).collect();
                    if !allowed.contains(&host) {
                        out.push(Violation::step("permissions", step.id.clone()));
                    }
                }
            }
            StepKind::FileRead { path } => {
                if !glob_engine::path_matches_any(path, &perms.file_read) {
                    out.push(Violation::step("permissions", step.id.clone()));
                }
            }
            StepKind::FileWrite { path } => {
                if !glob_engine::path_matches_any(path, &perms.file_write) {
                    out.push(Violation::step("permissions", step.id.clone()));
                }
            }
            StepKind::Mcp { server, tool } => {
                let key = format!("{server}/{tool}");
                if !glob_engine::head_allowed(&key, &perms.mcp_allowlist) {
                    out.push(Violation::step("permissions", step.id.clone()));
                }
            }
            StepKind::Agentic { .. } => {
                out.extend(check_agentic_step(step, perms));
            }
            _ => {
                if strict
                    && !models::KNOWN_STEP_TYPES.contains(&step.step_type.as_str())
                    && !perms.custom_step_allowlist.iter().any(|c| c == &step.step_type)
                {
                    out.push(Violation::step("permissions", step.id.clone()));
                }
            }
        }
    }
    out
}

/// `_check_delegation_safety`.
pub fn check_delegation_safety(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    if env.stakes != "physical" {
        return vec![];
    }
    let mut out = Vec::new();
    for step in &plan.steps {
        if step.step_type == "agentic" {
            out.push(Violation::step("permissions", step.id.clone()));
            continue;
        }
        if step.preferred_model.as_deref().is_some_and(|m| !m.is_empty()) {
            out.push(Violation::step("permissions", step.id.clone()));
        }
    }
    out
}

/// `check_skill_delegations` — Stage 1b.
pub fn check_skill_delegations(plan: &ActionPlan, env: &Envelope, strict: bool) -> Result<Vec<Violation>, String> {
    let mut out = Vec::new();
    for step in &plan.steps {
        let contract_env = match &step.kind {
            StepKind::Skill { contract_envelope, .. } => contract_envelope,
            _ => continue,
        };
        match contract_env {
            None => {
                if strict {
                    out.push(Violation::step("delegation", step.id.clone()));
                }
            }
            Some(inner_env) => {
                let sub = subsumption::envelope_subsumes(env, inner_env, strict)?;
                if !sub.holds {
                    out.push(Violation::step("delegation", step.id.clone()));
                }
            }
        }
    }
    Ok(out)
}

/// `_robotics_backing_missing`.
fn robotics_backing_missing(type_name: &str, perms: &Permission) -> bool {
    (type_name == "end_effector_in_workspace" && perms.workspace_bounds.is_none())
        || (type_name == "velocity_bounded" && perms.velocity_limit.is_none())
}

const RECOGNIZED_STAGE2_POSTCONDITION_TYPES: &[&str] = &["exit_code", "file_exists", "file_size_range"];

/// `_check_predicate_item` — invariant/postcondition share this one path.
#[allow(clippy::too_many_arguments)]
fn check_predicate_item(
    label: &str,
    type_name: &str,
    raw_expr: &Option<Value>,
    enforce: bool,
    steps_raw: &[Value],
    stakes: &str,
    perms: &Permission,
    strict: bool,
) -> Result<Vec<Violation>, String> {
    if !enforce {
        return Ok(vec![]);
    }
    let expr_value: Option<&Value> = match raw_expr {
        None => None,
        Some(v) if v.is_null() => None,
        Some(v) if v.is_object() => Some(v),
        // A non-null, non-object raw expr: `_normalize_expr` passes it through
        // unchanged (not a dict, so `parse_expression` is never called), and it
        // then fails BOTH the vacuity compiler and `evaluate_predicate`'s
        // exhaustive isinstance chain with an uncaught-inside-but-caught-by-the-
        // outer-try ValueError -> always exactly one "evaluation error" violation.
        Some(_) => return Ok(vec![Violation::plan("predicate")]),
    };

    let expr_value = match expr_value {
        Some(v) => v,
        None => {
            if label == "invariant" && robotics_backing_missing(type_name, perms) {
                return Ok(vec![Violation::plan("predicate")]);
            }
            let discharged = (label == "invariant" && z3_checks::RECOGNIZED_OPAQUE_TYPES.contains(&type_name))
                || (label == "postcondition" && RECOGNIZED_STAGE2_POSTCONDITION_TYPES.contains(&type_name));
            if strict && !discharged {
                return Ok(vec![Violation::plan("predicate")]);
            }
            return Ok(vec![]);
        }
    };

    let expr = predicate::parse_expression(expr_value)?;
    if let Expr::AliasRef { .. } = &expr {
        // aliases is always None on the wire (recording skips calls that carry
        // an AliasRegistry — see docs/spec/conformance.md).
        return Ok(vec![Violation::plan("predicate")]);
    }

    let vacuity = crate::z3_bridge::check_vacuity(&expr);
    if vacuity == Vacuity::Contradiction {
        return Ok(vec![Violation::plan("predicate")]);
    }
    if vacuity == Vacuity::Tautology && strict {
        return Ok(vec![Violation::plan("predicate")]);
    }

    match predicate::evaluate_predicate(&expr, steps_raw, stakes) {
        Err(_) => Ok(vec![Violation::plan("predicate")]),
        Ok(true) => Ok(vec![]),
        Ok(false) => Ok(vec![Violation::plan("predicate")]),
    }
}

/// `_check_predicate_invariants` — Stage 2b.
pub fn check_predicate_invariants(plan: &ActionPlan, env: &Envelope, strict: bool) -> Result<Vec<Violation>, String> {
    let steps_raw: Vec<Value> = plan.steps.iter().map(|s| s.raw.clone()).collect();
    let mut out = Vec::new();
    for inv in &env.invariants {
        out.extend(check_predicate_item(
            "invariant",
            &inv.type_,
            &inv.expr,
            inv.enforce,
            &steps_raw,
            &env.stakes,
            &env.permissions,
            strict,
        )?);
    }
    for pc in &env.postconditions {
        out.extend(check_predicate_item(
            "postcondition",
            &pc.type_,
            &pc.expr,
            pc.enforce,
            &steps_raw,
            &env.stakes,
            &env.permissions,
            strict,
        )?);
    }
    Ok(out)
}

pub struct VerifyOutcome {
    pub ok: bool,
    pub violations: Vec<Violation>,
}

/// `_verify` — the full pipeline. Short-circuits after each stage that
/// produced violations, in the oracle's exact order; Stage 2b (predicate)
/// and Stage 2c (robotics z3) both run unconditionally before their
/// combined result can short-circuit Stage 3 (dag).
pub fn verify(
    plan: &ActionPlan,
    env: &Envelope,
    strict: Option<bool>,
    shell_parser: &mut ShellParser,
) -> Result<VerifyOutcome, String> {
    let effective_strict = models::resolve_strict(strict, env);
    let mut violations = Vec::new();

    violations.extend(check_delegation_safety(plan, env));
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(check_permissions(plan, env, effective_strict, shell_parser));
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(check_skill_delegations(plan, env, effective_strict)?);
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(z3_checks::check_envelope_self_consistency(env));
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(z3_checks::check_plan_against_envelope(plan, env));
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(check_predicate_invariants(plan, env, effective_strict)?);
    violations.extend(z3_checks::check_plan_invariants(plan, env));
    if !violations.is_empty() {
        return Ok(VerifyOutcome { ok: false, violations });
    }

    violations.extend(crate::dag::check_dag(plan));

    let ok = violations.is_empty();
    Ok(VerifyOutcome { ok, violations })
}
