//! Port of `opendaisugi/subsumption.py`'s `envelope_subsumes` — the
//! skill-delegation "delegation" stage. Only the `.holds` boolean is
//! wire-relevant (violation comparison is by `(stage, step)`, never by
//! reason), so this port skips reconstructing a Z3 counterexample /
//! `SubsumptionResult` detail payload the oracle builds for logging.
//!
//! Every Z3 call here (like `z3_bridge.rs`) reasons over a fully symbolic
//! scope — `outer ⊨ inner` asks "is there any command Z3 can find that
//! inner would admit and outer would reject", never a concrete plan.

use crate::models::{Envelope, InvariantDecl, Permission};
use crate::predicate::{self, Expr};
use crate::z3_bridge::{self, Scope};
use std::collections::HashSet;

pub struct SubsumptionResult {
    pub holds: bool,
}

fn freeze_box(b: &([f64; 3], [f64; 3])) -> ((u64, u64, u64), (u64, u64, u64)) {
    let f = |x: f64| x.to_bits();
    let (lo, hi) = b;
    ((f(lo[0]), f(lo[1]), f(lo[2])), (f(hi[0]), f(hi[1]), f(hi[2])))
}

/// `_robot_capability_violation` — fail-closed: an outer physical bound the
/// inner exceeds or leaves undeclared blocks delegation. `None` = no
/// violation on this axis (also the correct answer for two non-robot
/// envelopes, where every outer bound is `None`/empty).
fn robot_capability_violation(outer: &Permission, inner: &Permission) -> bool {
    if let Some((o_min, o_max)) = outer.workspace_bounds {
        match inner.workspace_bounds {
            None => return true,
            Some((i_min, i_max)) => {
                for k in 0..3 {
                    if i_min[k] < o_min[k] || i_max[k] > o_max[k] {
                        return true;
                    }
                }
            }
        }
    }
    if let Some(o_lim) = outer.velocity_limit {
        match inner.velocity_limit {
            None => return true,
            Some(i_lim) if i_lim > o_lim => return true,
            _ => {}
        }
    }
    if let Some(o_lim) = outer.torque_limit {
        match inner.torque_limit {
            None => return true,
            Some(i_lim) if i_lim > o_lim => return true,
            _ => {}
        }
    }
    for (joint, (o_lo, o_hi)) in &outer.joint_limits {
        match inner.joint_limits.get(joint) {
            None => return true,
            Some((i_lo, i_hi)) => {
                if i_lo < o_lo || i_hi > o_hi {
                    return true;
                }
            }
        }
    }
    let outer_set: HashSet<_> = outer.obstacles.iter().map(freeze_box).collect();
    let inner_set: HashSet<_> = inner.obstacles.iter().map(freeze_box).collect();
    outer_set.difference(&inner_set).next().is_some()
}

fn glob_unsupported(glob: &str) -> bool {
    if glob == "**" || glob.ends_with("/**") || !glob.contains('*') {
        return false;
    }
    if glob.starts_with('*') && !glob[1..].contains('*') {
        return false;
    }
    glob.matches('*').count() != 1
}

fn smt_string_literal(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\"\""),
            '\n' => out.push_str("\\u{a}"),
            '\r' => out.push_str("\\u{d}"),
            other => out.push(other),
        }
    }
    out.push('"');
    out
}

/// `_glob_to_z3` — a permission glob as a Z3 String predicate over `v`.
fn glob_to_z3(glob: &str) -> String {
    if glob == "**" {
        return "true".to_string();
    }
    if let Some(prefix) = glob.strip_suffix("/**") {
        return format!(
            "(or (str.prefixof {} v) (= v {}))",
            smt_string_literal(&format!("{prefix}/")),
            smt_string_literal(prefix)
        );
    }
    if !glob.contains('*') {
        return format!("(= v {})", smt_string_literal(glob));
    }
    if glob.starts_with('*') && !glob[1..].contains('*') {
        return format!("(str.suffixof {} v)", smt_string_literal(&glob[1..]));
    }
    if glob.matches('*').count() == 1 {
        let idx = glob.find('*').unwrap();
        return format!(
            "(and (str.prefixof {} v) (str.suffixof {} v))",
            smt_string_literal(&glob[..idx]),
            smt_string_literal(&glob[idx + 1..])
        );
    }
    "true".to_string() // unsupported shape; caller fail-closes on the OUTER side via glob_unsupported
}

/// `_patterns_subsume` — true iff there's a subsumption VIOLATION on this
/// axis (a witness inner admits that outer forbids, or the check couldn't
/// be proven and so fails closed).
fn patterns_subsume_violates(inner_patterns: &[String], outer_patterns: &[String]) -> Result<bool, String> {
    if inner_patterns.is_empty() {
        return Ok(false); // inner admits nothing on this axis
    }
    if outer_patterns.iter().any(|g| glob_unsupported(g)) {
        return Ok(true); // can't soundly encode outer -> can't prove -> deny
    }
    let mut script = String::new();
    script.push_str("(set-logic ALL)\n(declare-const v String)\n");
    let inner_ok = format!("(or {})", inner_patterns.iter().map(|g| glob_to_z3(g)).collect::<Vec<_>>().join(" "));
    let outer_ok = if outer_patterns.is_empty() {
        "false".to_string()
    } else {
        format!("(or {})", outer_patterns.iter().map(|g| glob_to_z3(g)).collect::<Vec<_>>().join(" "))
    };
    script.push_str(&format!("(assert {inner_ok})\n(assert (not {outer_ok}))\n(check-sat)\n"));
    let result = z3_bridge::check_sat(&script)?;
    Ok(result != "unsat") // sat = witness found; unknown also denies (fail-closed)
}

fn network_scope_violation(outer: &Permission, inner: &Permission) -> bool {
    if !inner.network {
        return false;
    }
    if !outer.network {
        return true;
    }
    if outer.network_hosts.is_empty() {
        return false;
    }
    if inner.network_hosts.is_empty() {
        return true;
    }
    let outer_set: HashSet<String> = outer.network_hosts.iter().map(|h| h.to_lowercase()).collect();
    inner.network_hosts.iter().any(|h| !outer_set.contains(&h.to_lowercase()))
}

fn permission_scope_violates(outer: &Permission, inner: &Permission) -> Result<bool, String> {
    if inner.shell_allow_decomposition && !outer.shell_allow_decomposition {
        return Ok(true);
    }
    for (inner_p, outer_p) in
        [(&inner.file_read, &outer.file_read), (&inner.file_write, &outer.file_write), (&inner.mcp_allowlist, &outer.mcp_allowlist)]
    {
        if patterns_subsume_violates(inner_p, outer_p)? {
            return Ok(true);
        }
    }
    Ok(network_scope_violation(outer, inner))
}

fn detect_interpreters(perms: &Permission) -> Vec<String> {
    if !perms.shell {
        return vec![];
    }
    let mut v: Vec<String> =
        perms.shell_allowlist.iter().filter(|n| crate::models::SHELL_INTERPRETERS.contains(&n.as_str())).cloned().collect();
    v.sort();
    v.dedup();
    v
}

fn shell_head_in_allowlist(allowlist: &[String]) -> String {
    if allowlist.is_empty() {
        return "false".to_string();
    }
    let mut pieces = Vec::new();
    for head in allowlist {
        pieces.push(format!("(= ctx_command {})", smt_string_literal(head)));
        pieces.push(format!("(str.prefixof {} ctx_command)", smt_string_literal(&format!("{head} "))));
    }
    format!("(or {})", pieces.join(" "))
}

/// `_encode_shell_admission`.
fn encode_shell_admission(perms: &Permission) -> String {
    if !perms.shell {
        return "false".to_string();
    }
    let head_ok = shell_head_in_allowlist(&perms.shell_allowlist);
    let metachars = [";", "|", "&", "`", "<", ">", "\n", "\r", "$("];
    let no_meta: Vec<String> =
        metachars.iter().map(|ch| format!("(not (str.contains ctx_command {}))", smt_string_literal(ch))).collect();
    format!("(and {head_ok} (and {}))", no_meta.join(" "))
}

/// `_compile_invariants` — returns (term, strict_blocking_types). Opaque
/// (expr-less) types are tracked in the oracle for `unverified_invariants`
/// reporting only, which is informative-only on the wire, so this port
/// drops that list.
fn compile_invariants(invariants: &[InvariantDecl], scope: &mut Scope, strict: bool) -> Result<(String, Vec<String>), String> {
    let mut strict_blocking = Vec::new();
    let mut terms = Vec::new();
    for inv in invariants {
        if !inv.enforce {
            continue;
        }
        match &inv.expr {
            None => {
                if strict && !crate::z3_checks::RECOGNIZED_OPAQUE_TYPES.contains(&inv.type_.as_str()) {
                    strict_blocking.push(inv.type_.clone());
                }
            }
            Some(v) if v.is_null() => {
                if strict && !crate::z3_checks::RECOGNIZED_OPAQUE_TYPES.contains(&inv.type_.as_str()) {
                    strict_blocking.push(inv.type_.clone());
                }
            }
            Some(v) => {
                let mut expr = predicate::parse_expression(v)?;
                expr = match expr {
                    Expr::ForallSteps { pred } => *pred,
                    Expr::ExistsStep { pred } => *pred,
                    other => other,
                };
                terms.push(z3_bridge::compile_scalar(&expr, scope));
            }
        }
    }
    let term = if terms.is_empty() {
        "true".to_string()
    } else if terms.len() == 1 {
        terms.remove(0)
    } else {
        format!("(and {})", terms.join(" "))
    };
    Ok((term, strict_blocking))
}

/// `envelope_subsumes(outer, inner)` — proves `outer ⊨ inner`. Returns only
/// `.holds`; see the module docstring for why the counterexample detail is
/// dropped.
pub fn envelope_subsumes(outer: &Envelope, inner: &Envelope, strict: bool) -> Result<SubsumptionResult, String> {
    if robot_capability_violation(&outer.permissions, &inner.permissions) {
        return Ok(SubsumptionResult { holds: false });
    }
    if permission_scope_violates(&outer.permissions, &inner.permissions)? {
        return Ok(SubsumptionResult { holds: false });
    }

    let inner_interpreters = detect_interpreters(&inner.permissions);
    if outer.shell_interpreter_policy == "strict" && !inner_interpreters.is_empty() {
        return Ok(SubsumptionResult { holds: false });
    }

    let mut scope_inner = Scope::new("ctx");
    scope_inner.vars.insert("ctx__command".to_string(), ("ctx_command".to_string(), z3_bridge::Sort::Str));
    let mut scope_outer = Scope::new("ctx");
    scope_outer.vars.insert("ctx__command".to_string(), ("ctx_command".to_string(), z3_bridge::Sort::Str));

    let inner_shell = encode_shell_admission(&inner.permissions);
    let outer_shell = encode_shell_admission(&outer.permissions);

    let (inner_inv, inner_strict_blocking) = compile_invariants(&inner.invariants, &mut scope_inner, strict)?;
    let (outer_inv, _outer_strict_blocking) = compile_invariants(&outer.invariants, &mut scope_outer, strict)?;

    if strict && !inner_strict_blocking.is_empty() {
        return Ok(SubsumptionResult { holds: false });
    }

    let outer_soft_unique = scope_outer.soft.iter().any(|n| !scope_inner.soft.contains(n));
    if outer_soft_unique {
        return Ok(SubsumptionResult { holds: false });
    }

    let mut combined = Scope::new("ctx");
    combined.absorb(&scope_inner);
    combined.absorb(&scope_outer);

    let mut script = String::new();
    script.push_str("(set-logic ALL)\n");
    script.push_str(&z3_bridge::declare_block_pub(&combined));
    for name in &scope_inner.soft {
        script.push_str(&format!("(assert (= {name} true))\n"));
    }
    script.push_str(&format!("(assert (and {inner_shell} {inner_inv}))\n"));
    script.push_str(&format!("(assert (not (and {outer_shell} {outer_inv})))\n"));
    script.push_str("(check-sat)\n");

    let result = z3_bridge::check_sat(&script)?;
    Ok(SubsumptionResult { holds: result == "unsat" })
}
