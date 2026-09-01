//! `verify.verify(plan, envelope)` for the one-step plan the gate builds
//! from a record: permissions, the two Z3 stages on the linked Z3, the
//! predicate stage, and the robotics and DAG stages (which a one-step plan
//! of gate steps always passes).

use super::envelope::Envelope;
use super::py::text::lower;
use super::urls::{hostname, urlsplit};
use super::predicate::Stage;
use super::{Fault, PyErr};
use crate::z3py::{with_main, Check, Ctx, Sort, Term, Z3Error};
use super::pyjson::{self, float_repr, Object, Value};
use super::pystr::{py_split, py_strip};
use super::frames::{depth, frame, frames, trace};
use super::record::Record;
use super::{undecided, Runner, R};
use super::globs::{head_allowed, path_matches_any};
use crate::interpreter_parse::parse_interpreter;
use crate::shell_decompose::{Decomposition, ShellParser};
use crate::verify::shell_metachar_hit;
use std::cell::RefCell;

/// `models.Violation`.
#[derive(Debug, Clone)]
pub struct Violation {
    pub stage: String,
    pub message: String,
    pub detail: Option<Object>,
    pub remediation: Option<String>,
}

impl Violation {
    pub fn new(stage: &str, message: String, detail: Object) -> Self {
        Violation { stage: stage.into(), message, detail: Some(detail), remediation: None }
    }

    pub fn obj(&self) -> Object {
        Object::new()
            .with("stage", self.stage.as_str())
            .with("message", self.message.as_str())
            .with("detail", self.detail.clone().map(Value::Obj).unwrap_or(Value::Null))
            .with("suggested_remediation", self.remediation.clone())
    }
}

const MAX_INTERPRETER_DEPTH: i64 = 4;
const SANCTIONED_WRITE_SINKS: &[&str] = &["/dev/null", "/dev/stdout", "/dev/stderr"];
const SANCTIONED_READ_SOURCES: &[&str] = &["/dev/null", "/dev/stdin"];

fn depth_suffix(depth: i64) -> String {
    if depth == 0 {
        String::new()
    } else {
        format!(" (inside interpreter at depth {depth})")
    }
}

pub fn repr(s: &str) -> R<String> {
    match pyjson::repr(s) {
        Some(r) => Ok(r),
        None => undecided("a repr of non-ASCII text"),
    }
}

pub fn repr_list(ss: &[String]) -> R<String> {
    match pyjson::repr_list(ss) {
        Some(r) => Ok(r),
        None => undecided("a repr of non-ASCII text"),
    }
}

thread_local! {
    static PARSER: RefCell<Option<ShellParser>> = const { RefCell::new(None) };
}

impl Runner {
    /// The one-step plan's verdict. A glob match the oracle could not
    /// finish in the budget is its timeout.
    /// `extra` is the frames the dispatch adds above `verify`. The second
    /// list is verify's Z3-timeout warnings, which the gate denies on.
    pub fn verify_record(&self, rec: &Record, env: &Envelope, extra: i64) -> R<(Vec<Violation>, Vec<String>)> {
        self.verify_record_inner(rec, env, extra)
    }

    fn verify_record_inner(&self, rec: &Record, env: &Envelope, extra: i64) -> R<(Vec<Violation>, Vec<String>)> {
        // _dispatch_verify, verify and _verify, on the verifier's thread.
        let _f = frames(3 + extra);
        let step = "s0";
        let mut vs = Vec::new();
        match rec.step_type.as_str() {
            "shell" => {
                if !env.shell {
                    return Ok((
                        vec![Violation::new(
                            "permissions",
                            format!("Step '{step}' requires shell but envelope forbids it"),
                            Object::new().with("step", step),
                        )],
                        vec![],
                    ));
                }
                // check_permissions
                let _p = frame();
                vs = self.check_shell_command(&rec.command, step, env, 0)?;
            }
            "network" => vs = check_network(&rec.url, step, env)?,
            "file_read" => {
                if !path_matches_any(&rec.path, &env.file_read)? {
                    vs.push(Violation::new(
                        "permissions",
                        format!(
                            "Step '{step}' file_read path '{}' not permitted by file_read {}",
                            rec.path,
                            repr_list(&env.file_read)?
                        ),
                        Object::new().with("step", step).with("path", rec.path.as_str()),
                    ));
                }
            }
            "file_write" => {
                if !path_matches_any(&rec.path, &env.file_write)? {
                    vs.push(Violation::new(
                        "permissions",
                        format!(
                            "Step '{step}' file_write path '{}' not permitted by file_write {}",
                            rec.path,
                            repr_list(&env.file_write)?
                        ),
                        Object::new().with("step", step).with("path", rec.path.as_str()),
                    ));
                }
            }
            "mcp" => {
                let key = format!("{}/{}", rec.mcp_server, rec.mcp_tool);
                if !head_allowed(&key, &env.mcp_allowlist)? {
                    vs.push(Violation::new(
                        "permissions",
                        format!(
                            "Step '{step}' MCP tool '{key}' not in mcp_allowlist {}",
                            repr_list(&env.mcp_allowlist)?
                        ),
                        Object::new().with("step", step).with("mcp_tool", key.as_str()),
                    ));
                }
            }
            _ => {}
        }
        if !vs.is_empty() {
            return Ok((vs, vec![]));
        }
        let strict = env.stakes == "high" || env.stakes == "physical";
        let mut timeouts = Vec::new();
        match self_consistency(env)? {
            Check::Unsat => {
                return Ok((
                    vec![Violation::new(
                        "z3",
                        "Envelope is internally inconsistent".into(),
                        Object::new().with("unsat_core", "[]"),
                    )],
                    timeouts,
                ));
            }
            // Z3 could not finish. At high or physical stakes that is the
            // stage's violation; otherwise it is a warning, the later stages
            // still run, and the gate denies if they all pass.
            Check::Unknown => {
                let z3_message = format!("Z3 self-consistency check exceeded {Z3_TIMEOUT_MS}ms");
                if strict {
                    return Ok((
                        vec![Violation::new(
                            "z3",
                            "verifier timed out (envelope self-consistency check); raise the Z3 timeout".into(),
                            Object::new().with("reason", "z3_timeout").with("z3_message", z3_message),
                        )],
                        timeouts,
                    ));
                }
                timeouts.push(z3_message);
            }
            Check::Sat => {}
        }
        if plan_against_envelope(env, &rec.step_type) == Check::Unsat {
            return Ok((
                vec![Violation::new(
                    "z3",
                    "Plan requirements contradict envelope permissions".into(),
                    Object::new().with("unsat_core", "[]"),
                )],
                timeouts,
            ));
        }
        let vs = Stage::new(self, env, step_dict(rec), strict).run()?;
        // The robotics handlers read only joint and Cartesian steps, and a
        // one-step plan with no dependencies passes the DAG stage.
        Ok((vs, timeouts))
    }

        /// `verify._check_shell_command`.
    fn check_shell_command(&self, command: &str, step: &str, env: &Envelope, depth: i64) -> R<Vec<Violation>> {
        if depth > MAX_INTERPRETER_DEPTH {
            return Ok(vec![Violation::new(
                "permissions",
                format!("Step '{step}' interpreter recursion exceeded max depth {MAX_INTERPRETER_DEPTH}"),
                Object::new().with("step", step).with("command", command).with("depth", depth),
            )]);
        }
        let _f = frame();
        let stripped = py_strip(command);
        if stripped.is_empty() {
            return Ok(vec![]);
        }
        if !shell_metachar_hit(command) {
            return self.verify_simple_command(stripped, step, env, depth);
        }
        let mut refused = String::new();
        if env.shell_allow_decomposition {
            let d = self.decompose(command)?;
            if d.ok {
                let mut vs = check_redirect_scopes(&d, step, env)?;
                for simple in &d.commands {
                    vs.extend(self.verify_simple_command(simple, step, env, depth)?);
                }
                return Ok(vs);
            }
            refused = d.reason;
        }
        let mut detail = Object::new().with("step", step).with("command", command).with("depth", depth);
        if !refused.is_empty() {
            detail.set("decomposition_refused", refused);
        }
        Ok(vec![Violation {
            stage: "permissions".into(),
            message: format!(
                "Step '{step}' shell command contains dangerous metacharacters (;, |, &, `, <, >, $(, newline){}",
                depth_suffix(depth)
            ),
            detail: Some(detail),
            remediation: decomposition_remediation(step, command)?,
        }])
    }

    /// `verify._verify_simple_command`.
    fn verify_simple_command(&self, command: &str, step: &str, env: &Envelope, depth: i64) -> R<Vec<Violation>> {
        let _f = frame();
        let stripped = py_strip(command);
        let head = match shell_head(stripped) {
            None => return Ok(vec![]),
            Some(h) => h,
        };
        if !head_allowed(head, &env.shell_allowlist)? {
            return Ok(vec![Violation {
                stage: "permissions".into(),
                message: format!(
                    "Step '{step}' shell command '{head}' not in allowlist {}{}",
                    repr_list(&env.shell_allowlist)?,
                    depth_suffix(depth)
                ),
                detail: Some(Object::new().with("step", step).with("command_head", head).with("depth", depth)),
                remediation: Some(format!("Add '{head}' to permissions.shell_allowlist")),
            }]);
        }
        let payload = match parse_interpreter(command) {
            None => return Ok(vec![]),
            Some(p) => p,
        };
        if payload.opaque {
            if env.policy == "strict" {
                return Ok(vec![Violation::new(
                    "permissions",
                    format!(
                        "Step '{step}' invokes opaque interpreter '{}' whose payload cannot be recursively \
                         verified (strict shell_interpreter_policy rejects)",
                        payload.head
                    ),
                    Object::new().with("step", step).with("interpreter", payload.head.as_str()),
                )]);
            }
            return Ok(vec![]);
        }
        let mut vs = Vec::new();
        for inner in &payload.inner_commands {
            vs.extend(self.check_shell_command(inner, step, env, depth + 1)?);
        }
        Ok(vs)
    }

    /// `shell_decompose.decompose_command(command)`, called with the Python
    /// frames the oracle has on its stack here (`frames::depth`). Raises
    /// what the oracle raises.
    pub fn decompose(&self, command: &str) -> R<Decomposition> {
        trace("decompose");
        let at = depth();
        PARSER.with(|p| {
            let mut p = p.borrow_mut();
            Ok(p.get_or_insert_with(ShellParser::new).decompose_py(command, Some(at))?)
        })
    }
}

/// `models.ShellStep(...).model_dump()` and its kin for the record's step.
pub fn step_dict(rec: &Record) -> Value {
    let mut o = Object::new()
        .with("id", "s0")
        .with("depends_on", Value::List(vec![]))
        .with("metadata", Object::new())
        .with("postcondition", Value::Null)
        .with("preferred_model", Value::Null)
        .with("type", rec.step_type.as_str());
    match rec.step_type.as_str() {
        "shell" => o.set("command", rec.command.as_str()),
        "file_read" => o.set("path", rec.path.as_str()),
        "file_write" => o.set("path", rec.path.as_str()).set("content", ""),
        "network" => o.set("url", rec.url.as_str()).set("method", "GET").set("headers", Object::new()),
        _ => o
            .set("server", rec.mcp_server.as_str())
            .set("tool", rec.mcp_tool.as_str())
            .set("arguments", rec.arguments.clone()),
    };
    Value::Obj(o)
}

fn z3_fault(e: Z3Error) -> Fault {
    Fault::Raised(PyErr::new("Z3Exception", e.0))
}

/// The Z3 budget of one check, in ms: verify's default, which the gate uses.
const Z3_TIMEOUT_MS: u32 = 500;

/// A task that makes `self_consistency` answer unknown in tests: no real
/// envelope makes Z3 time out on these pinned terms.
#[cfg(test)]
pub const FORCE_Z3_UNKNOWN_TASK: &str = "test: Z3 answers unknown";

/// `z3_checks.check_envelope_self_consistency`. Every term is a pinned
/// constant, so Z3 answers sat or unsat at once and never unknown. It is
/// decided here directly, which saves making a Z3 context (about 9 ms);
/// a test asks Z3 the oracle's own formulas over every combination of the
/// inputs and checks the answers agree. A time that is not plain decimal
/// text still goes to Z3.
fn self_consistency(env: &Envelope) -> R<Check> {
    #[cfg(test)]
    if env.task == FORCE_Z3_UNKNOWN_TASK {
        return Ok(Check::Unknown);
    }
    let Some(time_ok) = time_in_range(&env.max_execution_time_s) else {
        return with_main(|c| self_consistency_on(c, env)).map_err(z3_fault)?;
    };
    // A non-empty allowlist asserts shell; a file_exists postcondition
    // asserts can_write; and 0 < max_execution_time_s <= 3600.
    let shell_ok = env.shell_allowlist.is_empty() || env.shell;
    let write_ok = !env.file_write.is_empty() || !env.postconditions.iter().any(|p| p.typ == "file_exists");
    Ok(if shell_ok && write_ok && time_ok { Check::Sat } else { Check::Unsat })
}

/// Whether decimal text lies in 0 < t <= 3600, at any size; `None` when
/// it is not an optional minus sign and digits.
fn time_in_range(text: &str) -> Option<bool> {
    let (neg, digits) = match text.strip_prefix('-') {
        Some(d) => (true, d),
        None => (false, text),
    };
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let d = digits.trim_start_matches('0');
    if d.is_empty() || neg {
        return Some(false);
    }
    // Four digits compare as text the way they compare as numbers.
    Some(d.len() < 4 || (d.len() == 4 && d <= "3600"))
}

fn self_consistency_on(c: &mut Ctx, env: &Envelope) -> R<Check> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3_fault);
    let shell = z(c.constant("shell", Sort::Bool))?;
    let can_write = z(c.constant("can_write", Sort::Bool))?;
    let mut ts = Vec::new();
    let v = z(c.bool_val(env.shell))?;
    ts.push(z(c.eq(shell, v))?);
    let v = z(c.bool_val(!env.file_write.is_empty()))?;
    ts.push(z(c.eq(can_write, v))?);
    let t = z(c.bool_val(true))?;
    if !env.shell_allowlist.is_empty() {
        ts.push(z(c.eq(shell, t))?);
    }
    for pc in &env.postconditions {
        if pc.typ == "file_exists" {
            ts.push(z(c.eq(can_write, t))?);
        }
    }
    let max_time = z(c.constant("max_time", Sort::Int))?;
    let v = z(c.numeral(&env.max_execution_time_s, Sort::Int))?;
    ts.push(z(c.eq(max_time, v))?);
    let zero = z(c.numeral("0", Sort::Int))?;
    ts.push(z(c.gt(max_time, zero))?);
    let hour = z(c.numeral("3600", Sort::Int))?;
    ts.push(z(c.le(max_time, hour))?);
    c.check(Z3_TIMEOUT_MS, &ts).map_err(|e| Fault::Raised(PyErr::new("Z3Exception", e.0)))
}

/// `z3_checks.check_plan_against_envelope` for a one-step plan, decided
/// directly as `self_consistency` is: a shell step needs shell, a
/// file_write step needs a file_write scope.
fn plan_against_envelope(env: &Envelope, step_type: &str) -> Check {
    if (step_type == "shell" && !env.shell) || (step_type == "file_write" && env.file_write.is_empty()) {
        Check::Unsat
    } else {
        Check::Sat
    }
}

#[cfg(test)]
fn plan_against_envelope_on(c: &mut Ctx, env: &Envelope, step_type: &str) -> R<Check> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3_fault);
    let shell = z(c.constant("shell_available", Sort::Bool))?;
    let write = z(c.constant("write_available", Sort::Bool))?;
    let mut ts = Vec::new();
    let v = z(c.bool_val(env.shell))?;
    ts.push(z(c.eq(shell, v))?);
    let v = z(c.bool_val(!env.file_write.is_empty()))?;
    ts.push(z(c.eq(write, v))?);
    let t = z(c.bool_val(true))?;
    if step_type == "shell" {
        ts.push(z(c.eq(shell, t))?);
    }
    if step_type == "file_write" {
        ts.push(z(c.eq(write, t))?);
    }
    c.check(Z3_TIMEOUT_MS, &ts).map_err(|e| Fault::Raised(PyErr::new("Z3Exception", e.0)))
}

fn check_redirect_scopes(d: &Decomposition, step: &str, env: &Envelope) -> R<Vec<Violation>> {
    let mut vs = Vec::new();
    for p in &d.writes {
        if SANCTIONED_WRITE_SINKS.contains(&p.as_str()) {
            continue;
        }
        if !path_matches_any(p, &env.file_write)? {
            vs.push(Violation {
                stage: "permissions".into(),
                message: format!(
                    "Step '{step}' shell redirect writes '{p}' outside file_write scope {}",
                    repr_list(&env.file_write)?
                ),
                detail: Some(Object::new().with("step", step).with("redirect_write", p.as_str())),
                remediation: Some(format!("Add '{p}' (or a covering glob) to permissions.file_write")),
            });
        }
    }
    for p in &d.reads {
        if SANCTIONED_READ_SOURCES.contains(&p.as_str()) {
            continue;
        }
        if !path_matches_any(p, &env.file_read)? {
            vs.push(Violation {
                stage: "permissions".into(),
                message: format!(
                    "Step '{step}' shell redirect reads '{p}' outside file_read scope {}",
                    repr_list(&env.file_read)?
                ),
                detail: Some(Object::new().with("step", step).with("redirect_read", p.as_str())),
                remediation: Some(format!("Add '{p}' (or a covering glob) to permissions.file_read")),
            });
        }
    }
    Ok(vs)
}

/// `verify._extract_shell_head`.
fn shell_head(stripped: &str) -> Option<&str> {
    if stripped.is_empty() || stripped.starts_with('#') {
        return None;
    }
    py_split(stripped).into_iter().find(|t| !is_env_assign(t))
}

/// `verify._ENV_ASSIGN_RE.match`.
fn is_env_assign(tok: &str) -> bool {
    let b = tok.as_bytes();
    if b.is_empty() || !(b[0].is_ascii_alphabetic() || b[0] == b'_') {
        return false;
    }
    for &c in &b[1..] {
        if c == b'=' {
            return true;
        }
        if !(c.is_ascii_alphanumeric() || c == b'_') {
            return false;
        }
    }
    false
}

/// `verify._build_decomposition_remediation`.
fn decomposition_remediation(step: &str, command: &str) -> R<Option<String>> {
    let parts = split_compound_shell(command);
    if parts.len() <= 1 || parts.iter().any(|p| p.contains("$(") || p.contains('`')) {
        return Ok(None);
    }
    let mut lines = Vec::new();
    for (i, p) in parts.iter().enumerate() {
        let depends = if i > 0 { format!(", depends_on=['{step}_d{}']", i - 1) } else { String::new() };
        lines.push(format!("  ShellStep(id='{step}_d{i}', command={}{depends})", repr(p)?));
    }
    Ok(Some(format!("Decompose into sequential ShellSteps:\n{}", lines.join("\n"))))
}

/// `parsers.claude_code._split_compound_shell`.
fn split_compound_shell(command: &str) -> Vec<String> {
    if command.is_empty() {
        return vec![];
    }
    let b = command.as_bytes();
    let mut parts: Vec<String> = Vec::new();
    let mut cur: Vec<u8> = Vec::new();
    let (mut in_single, mut in_double) = (false, false);
    let mut i = 0;
    let flush = |cur: &mut Vec<u8>, parts: &mut Vec<String>| {
        parts.push(py_strip(&String::from_utf8_lossy(cur)).to_string());
        cur.clear();
    };
    while i < b.len() {
        let c = b[i];
        if c == b'\'' && !in_double {
            in_single = !in_single;
            cur.push(c);
        } else if c == b'"' && !in_single {
            in_double = !in_double;
            cur.push(c);
        } else if !in_single && !in_double && c == b';' {
            flush(&mut cur, &mut parts);
        } else if !in_single && !in_double && (c == b'&' || c == b'|') && i + 1 < b.len() && b[i + 1] == c {
            flush(&mut cur, &mut parts);
            i += 1;
        } else {
            cur.push(c);
        }
        i += 1;
    }
    let tail = String::from_utf8_lossy(&cur);
    let tail = py_strip(&tail);
    if !tail.is_empty() {
        parts.push(tail.to_string());
    }
    parts.retain(|p| !p.is_empty());
    parts
}

fn check_network(raw_url: &str, step: &str, env: &Envelope) -> R<Vec<Violation>> {
    if !env.network {
        return Ok(vec![Violation::new(
            "permissions",
            format!("Step '{step}' requires network but envelope forbids it"),
            Object::new().with("step", step),
        )]);
    }
    // urlparse(step.url), twice: its ValueError is the verifier's.
    let split = urlsplit(raw_url)?;
    let scheme = split.scheme.clone();
    if scheme != "http" && scheme != "https" {
        return Ok(vec![Violation::new(
            "permissions",
            format!(
                "Step '{step}' network URL scheme '{scheme}' not allowed (only http/https); got {}",
                repr(raw_url)?
            ),
            Object::new().with("step", step).with("scheme", scheme.as_str()).with("url", raw_url),
        )]);
    }
    if !env.network_hosts.is_empty() {
        let host = hostname(&split.netloc).unwrap_or_default();
        let found = env.network_hosts.iter().any(|h| lower(h) == host);
        if !found {
            return Ok(vec![Violation::new(
                "permissions",
                format!(
                    "Step '{step}' network host '{host}' not in network_hosts allowlist {}",
                    repr_list(&env.network_hosts)?
                ),
                Object::new().with("step", step).with("host", host.as_str()).with("url", raw_url),
            )]);
        }
    }
    Ok(vec![])
}

/// The deny wording of a verifier that ran out of budget.
pub fn verify_late_reason(timeout_s: f64) -> String {
    format!(
        "gate internal error (denied fail-closed): verifier exceeded the gate's inner time budget ({}s).",
        float_repr(timeout_s)
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The two ground stages, decided directly, agree with Z3 asked the
    /// oracle's own formulas over every combination of what they read.
    #[test]
    fn ground_checks_agree_with_z3() {
        let times = ["-5", "0", "1", "30", "3600", "3601", "100000000000000000000000", "-100000000000000000000000"];
        let pcs = ["[]", r#"[{"type":"file_exists","description":"d"}]"#, r#"[{"type":"other","description":"d"}]"#,
            r#"[{"type":"other","description":"d"},{"type":"file_exists","description":"d"}]"#];
        let mut n = 0;
        for shell in [false, true] {
            for writes in ["[]", r#"["/work/**"]"#] {
                for allow in ["[]", r#"["ls"]"#] {
                    for pc in pcs {
                        for t in times {
                            let text = format!(
                                r#"{{"id":"e","generated_by":"g","task":"t","postconditions":{pc},"permissions":{{"shell":{shell},"file_write":{writes},"shell_allowlist":{allow},"max_execution_time_s":{t}}}}}"#
                            );
                            let env = super::super::envelope::parse_envelope(&text).unwrap_or_else(|_| panic!("{text}"));
                            assert_eq!(env.max_execution_time_s, t);
                            let z = with_main(|c| self_consistency_on(c, &env)).unwrap().unwrap_or_else(|_| panic!("z3 {text}"));
                            assert_ne!(z, Check::Unknown);
                            assert_eq!(self_consistency(&env).unwrap_or_else(|_| panic!("{text}")), z, "{text}");
                            n += 1;
                            for step in ["shell", "file_read", "file_write", "network", "mcp"] {
                                let z = with_main(|c| plan_against_envelope_on(c, &env, step)).unwrap().unwrap_or_else(|_| panic!("z3"));
                                assert_ne!(z, Check::Unknown);
                                assert_eq!(plan_against_envelope(&env, step), z, "{step} {text}");
                                n += 1;
                            }
                        }
                    }
                }
            }
        }
        assert_eq!(n, 2 * 2 * 2 * 4 * 8 * 6);
    }

    #[test]
    fn time_text_outside_decimal_goes_to_z3() {
        assert_eq!(time_in_range("30"), Some(true));
        assert_eq!(time_in_range("-0"), Some(false));
        assert_eq!(time_in_range("03600"), Some(true));
        assert_eq!(time_in_range("3601"), Some(false));
        assert_eq!(time_in_range(""), None);
        assert_eq!(time_in_range("-"), None);
        assert_eq!(time_in_range("1e3"), None);
    }

    #[test]
    fn compound_split_is_the_parsers() {
        assert_eq!(split_compound_shell("a && b; 'c;d' || e"), vec!["a", "b", "'c;d'", "e"]);
    }
}
