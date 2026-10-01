//! `verify(plan_template, envelope, z3_timeout_ms)` as `import_pathway`
//! runs it, with the oracle's messages: delegation safety, permissions,
//! skill delegations, the two Z3 stages on the linked Z3 with the given
//! timeout, the predicate stage, the robotics stage and the DAG.
//!
//! A Z3 check that answers unknown is a timeout: a warning, or under strict
//! mode or physical stakes a violation, and import refuses on either
//! (VERIFICATION_TIMEOUT). A Z3 error is a violation: the check did not
//! pass.

use super::PwErr;
use crate::gate::envelope::from_dump;
use crate::gate::predicate::Stage;
use crate::gate::pyjson::{Object, Value};
use crate::gate::Fault;
use crate::models::{self, ActionPlan, Envelope};
use crate::shell_decompose::ShellParser;
use crate::violation::Violation;
use crate::z3py::{with_main, Check, Ctx, Sort, Term, Z3Error};

/// What verify found.
pub struct Outcome {
    pub violations: Vec<Violation>,
    /// The Z3-timeout texts: warnings, and the `z3_message` of each
    /// timeout violation, in the order import lists them.
    pub timeouts: Vec<String>,
    /// `VerificationResult.warnings`, in the oracle's order and words.
    pub warnings: Vec<String>,
    /// Set when the oracle may add a warning this port does not compute
    /// (a skill delegation whose contract holds may name unverified
    /// invariants): a caller that stores the warnings must refuse.
    pub warnings_unmodeled: bool,
}

/// The task an envelope carries in a test to make Z3 answer unknown.
#[cfg(test)]
pub const FORCE_Z3_UNKNOWN_TASK: &str = "test: Z3 answers unknown";

fn unreadable(why: impl Into<String>) -> PwErr {
    PwErr::Unreadable(why.into())
}

/// The verifier's typed plan and envelope, read from their JSON.
fn typed(plan: &Value, env: &Value) -> Result<(ActionPlan, Envelope), PwErr> {
    let p = to_serde(plan)?;
    let e = to_serde(env)?;
    let plan = models::parse_plan(&p).map_err(unreadable)?;
    let env: Envelope = serde_json::from_value(e).map_err(|e| unreadable(e.to_string()))?;
    Ok((plan, env))
}

/// Whether an envelope reads as the verifier's typed Envelope, as
/// `Envelope.model_validate` reads it before a recall.
pub fn check_envelope(env: &Value) -> Result<(), PwErr> {
    let e = to_serde(env)?;
    serde_json::from_value::<Envelope>(e).map(|_| ()).map_err(|e| unreadable(e.to_string()))
}

/// A validated value as serde_json holds it, keys in order, at any depth
/// (serde_json's text reader stops at 128 levels; pydantic reads 200).
fn to_serde(v: &Value) -> Result<serde_json::Value, PwErr> {
    Ok(match v {
        Value::Null => serde_json::Value::Null,
        Value::Bool(b) => serde_json::Value::Bool(*b),
        // pydantic takes an int of any size. The typed copy holds one
        // past 64 bits at the nearest bound, which keeps its sign and its
        // order against every small number; the one int a check reads
        // exactly (the max time, for Z3) is taken from the value itself.
        Value::Int(t) => serde_json::Value::Number(match t.parse::<i64>() {
            Ok(n) => n.into(),
            Err(_) if t.starts_with('-') => i64::MIN.into(),
            Err(_) => i64::MAX.into(),
        }),
        Value::Float(f) => match serde_json::Number::from_f64(*f) {
            Some(n) => serde_json::Value::Number(n),
            None => return Err(unreadable("a number that is not finite inside the plan or the envelope")),
        },
        Value::Str(s) => serde_json::Value::String(s.clone()),
        Value::List(l) | Value::Tuple(l) => serde_json::Value::Array(l.iter().map(to_serde).collect::<Result<_, _>>()?),
        Value::Obj(o) => {
            let mut m = serde_json::Map::new();
            for k in o.keys() {
                m.insert(k.clone(), to_serde(o.value(k))?);
            }
            serde_json::Value::Object(m)
        }
    })
}

/// `verify(plan, envelope, z3_timeout_ms=timeout_ms)` with strict unset.
pub fn verify(plan_v: &Value, env_v: &Value, strict: Option<bool>, timeout_ms: u32) -> Result<Outcome, PwErr> {
    let (plan, env) = typed(plan_v, env_v)?;
    let strict = models::resolve_strict(strict, &env);
    let timeout_is_violation = strict || env.stakes == "physical";
    let mut out = Outcome { violations: vec![], timeouts: vec![], warnings: vec![], warnings_unmodeled: false };
    let done = |out: &Outcome| !out.violations.is_empty();

    out.violations.extend(crate::verify::check_delegation_safety(&plan, &env));
    if done(&out) {
        return Ok(out);
    }
    let mut parser = ShellParser::new();
    out.violations.extend(crate::verify::check_permissions(&plan, &env, strict, &mut parser));
    if done(&out) {
        return Ok(out);
    }
    // The delegation proof reads the typed copy, which clamps an int past
    // i64 (`to_serde`): with one in the caller's envelope or a contract,
    // refuse rather than prove over a wrong numeral.
    let skills: Vec<&Value> = match plan_v.as_obj().map(|o| o.value("steps")) {
        Some(Value::List(l)) => l
            .iter()
            .filter_map(|s| s.as_obj())
            .filter(|s| s.value("type").as_str() == Some("skill"))
            .map(|s| s.value("contract_envelope"))
            .filter(|c| c.as_obj().is_some())
            .collect(),
        _ => vec![],
    };
    if !skills.is_empty() && (wide_int(env_v) || skills.iter().any(|c| wide_int(c))) {
        return Err(unreadable(
            "an int past 2^63 in an envelope a skill delegation proof reads, which this binary does not hold exactly",
        ));
    }
    let delegations = crate::verify::check_skill_delegations(
        &plan,
        &env,
        strict,
        Some((timeout_ms, &mut out.timeouts)),
        Some((&mut out.warnings, &mut out.warnings_unmodeled)),
    )
    .map_err(unreadable)?;
    out.violations.extend(delegations);
    if done(&out) {
        return Ok(out);
    }

    // The max time as pydantic holds it: an int of any size.
    let max_time = match env_v.as_obj().and_then(|o| o.get("permissions")).and_then(|p| p.as_obj()).and_then(|p| p.get("max_execution_time_s")) {
        Some(Value::Int(t)) => t.clone(),
        _ => env.permissions.max_execution_time_s.to_string(),
    };
    let ground: [(&str, &str, &str, Box<dyn Fn(&mut Ctx) -> Result<Check, Z3Error>>); 2] = [
        (
            "envelope self-consistency check",
            "Z3 self-consistency check exceeded",
            "Envelope is internally inconsistent",
            Box::new(|c: &mut Ctx| self_consistency_on(c, &env, &max_time, timeout_ms)),
        ),
        (
            "plan-vs-envelope check",
            "Z3 plan-vs-envelope check exceeded",
            "Plan requirements contradict envelope permissions",
            Box::new(|c: &mut Ctx| plan_against_envelope_on(c, &plan, &env, timeout_ms)),
        ),
    ];
    for (which, exceeded, unsat_msg, check) in ground.iter() {
        #[cfg(test)]
        let forced = env_v.as_obj().map(|o| o.value("task")) == Some(&Value::Str(FORCE_Z3_UNKNOWN_TASK.into()));
        #[cfg(not(test))]
        let forced = false;
        let r = if forced { Ok(Check::Unknown) } else { with_main(|c| check(c)).and_then(|r| r) };
        match r {
            Err(e) => out.violations.push(Violation::plan("z3").msg(format!("Z3 could not run the {which}: {}", e.0))),
            // z3's unsat_core() with no tracked assertions prints "[]".
            Ok(Check::Unsat) => out.violations.push(
                Violation::plan("z3").msg(*unsat_msg).with(Object::new().with("unsat_core", "[]"), None),
            ),
            Ok(Check::Sat) => {}
            Ok(Check::Unknown) => {
                let text = format!("{exceeded} {timeout_ms}ms");
                if timeout_is_violation {
                    out.violations.push(
                        Violation::plan("z3").msg(format!("verifier timed out ({which}); raise the Z3 timeout")),
                    );
                } else {
                    out.warnings.push(text.clone());
                }
                out.timeouts.push(text);
            }
        }
        if done(&out) {
            return Ok(out);
        }
    }

    // Stage 2b: the predicate stage, each step as its model_dump().
    let env_obj = env_v.as_obj().cloned().unwrap_or_default();
    let genv = from_dump(env_obj);
    let steps: Vec<Value> = match plan_v.as_obj().map(|o| o.value("steps")) {
        Some(Value::List(l)) => l.iter().map(dumped_step).collect(),
        _ => vec![],
    };
    let task = plan_v.as_obj().and_then(|o| o.value("task").as_str()).unwrap_or("").to_string();
    let mut stage = Stage::for_plan(&genv, &task, steps, strict);
    let vs = match stage.run() {
        Ok(vs) => vs,
        Err(Fault::Undecided(why)) => return Err(unreadable(why)),
        Err(Fault::Raised(e)) => {
            return Err(PwErr::Invalid(if e.kind == "ValidationError" {
                format!("pydantic_core._pydantic_core.ValidationError: {}", e.msg)
            } else {
                format!("{}: {}", e.kind, e.msg)
            }))
        }
    };
    out.warnings.append(&mut stage.warnings);
    for v in vs {
        // Each predicate violation carries the detail the stage words, as
        // the Go port words it.
        let pv = Violation::plan("predicate").msg(v.message);
        out.violations.push(match v.detail {
            Some(d) => pv.with(d, None),
            None => pv,
        });
    }
    // Stage 2c: the robotics trajectory checks.
    out.violations.extend(crate::z3_checks::check_plan_invariants(&plan, &env));
    if done(&out) {
        return Ok(out);
    }
    out.violations.extend(crate::dag::check_dag(&plan));
    Ok(out)
}

/// `check_envelope_self_consistency(envelope)` on the linked Z3, failing
/// closed: true only when Z3 answers sat. A check that did not finish
/// (unknown), a Z3 error, or an envelope that does not read is not
/// consistent. The text is the violation's message, or the timeout's or
/// error's text.
pub fn self_consistent(env_v: &Value, timeout_ms: u32) -> (bool, String) {
    let env: Envelope = match to_serde(env_v).and_then(|e| serde_json::from_value(e).map_err(|e| unreadable(e.to_string()))) {
        Ok(e) => e,
        Err(e) => return (false, format!("the envelope does not read: {e}")),
    };
    let max_time = match env_v.as_obj().and_then(|o| o.get("permissions")).and_then(|p| p.as_obj()).and_then(|p| p.get("max_execution_time_s")) {
        Some(Value::Int(t)) => t.clone(),
        _ => env.permissions.max_execution_time_s.to_string(),
    };
    #[cfg(test)]
    if env_v.as_obj().map(|o| o.value("task")) == Some(&Value::Str(FORCE_Z3_UNKNOWN_TASK.into())) {
        return (false, format!("Z3 self-consistency check exceeded {timeout_ms}ms"));
    }
    match with_main(|c| self_consistency_on(c, &env, &max_time, timeout_ms)).and_then(|r| r) {
        Ok(Check::Sat) => (true, String::new()),
        Ok(Check::Unsat) => (false, "Envelope is internally inconsistent".into()),
        Ok(Check::Unknown) => (false, format!("Z3 self-consistency check exceeded {timeout_ms}ms")),
        Err(e) => (false, e.0),
    }
}

/// The step fields each step type declares as a tuple, which
/// pydantic's `model_dump()` keeps as tuples.
fn tuple_fields(step_type: &str) -> &'static [&'static str] {
    match step_type {
        "cartesian_move" => &["target_position", "target_orientation"],
        "vla" => &["target_pose"],
        _ => &[],
    }
}

/// A validated step as the oracle's predicate stage reads it: its
/// `model_dump()`, each tuple-typed field a tuple, which equals no list.
pub fn dumped_step(step: &Value) -> Value {
    let o = match step.as_obj() {
        Some(o) => o,
        None => return step.clone(),
    };
    let fields = match o.get("type") {
        Some(Value::Str(t)) => tuple_fields(t),
        _ => &[],
    };
    if fields.is_empty() {
        return step.clone();
    }
    let mut out = o.clone();
    for f in fields {
        if let Some(Value::List(l)) = o.get(f) {
            out.set(f, Value::Tuple(l.clone()));
        }
    }
    Value::Obj(out)
}

/// `z3_checks.check_envelope_self_consistency`'s solver, as z3py builds it.
fn self_consistency_on(c: &mut Ctx, env: &Envelope, max_time_text: &str, timeout_ms: u32) -> Result<Check, Z3Error> {
    let shell = c.constant("shell", Sort::Bool)?;
    let can_write = c.constant("can_write", Sort::Bool)?;
    let mut ts: Vec<Term> = Vec::new();
    let v = c.bool_val(env.permissions.shell)?;
    ts.push(c.eq(shell, v)?);
    let v = c.bool_val(!env.permissions.file_write.is_empty())?;
    ts.push(c.eq(can_write, v)?);
    let t = c.bool_val(true)?;
    if !env.permissions.shell_allowlist.is_empty() {
        ts.push(c.eq(shell, t)?);
    }
    for pc in &env.postconditions {
        if pc.type_ == "file_exists" {
            ts.push(c.eq(can_write, t)?);
        }
    }
    let max_time = c.constant("max_time", Sort::Int)?;
    let v = c.numeral(max_time_text, Sort::Int)?;
    ts.push(c.eq(max_time, v)?);
    let zero = c.numeral("0", Sort::Int)?;
    ts.push(c.gt(max_time, zero)?);
    let hour = c.numeral("3600", Sort::Int)?;
    ts.push(c.le(max_time, hour)?);
    c.check(timeout_ms, &ts)
}

/// `z3_checks.check_plan_against_envelope`'s solver.
fn plan_against_envelope_on(c: &mut Ctx, plan: &ActionPlan, env: &Envelope, timeout_ms: u32) -> Result<Check, Z3Error> {
    let shell = c.constant("shell_available", Sort::Bool)?;
    let write = c.constant("write_available", Sort::Bool)?;
    let mut ts: Vec<Term> = Vec::new();
    let v = c.bool_val(env.permissions.shell)?;
    ts.push(c.eq(shell, v)?);
    let v = c.bool_val(!env.permissions.file_write.is_empty())?;
    ts.push(c.eq(write, v)?);
    let t = c.bool_val(true)?;
    if plan.steps.iter().any(|s| s.step_type == "shell") {
        ts.push(c.eq(shell, t)?);
    }
    if plan.steps.iter().any(|s| s.step_type == "file_write") {
        ts.push(c.eq(write, t)?);
    }
    c.check(timeout_ms, &ts)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pathways::pmodel::{validate_json, Id};

    fn outcome(plan: &str, env: &str, strict: Option<bool>) -> Outcome {
        let p = validate_json(Id::ActionPlan, plan).unwrap_or_else(|e| panic!("{}", e.text()));
        let e = validate_json(Id::Envelope, env).unwrap_or_else(|e| panic!("{}", e.text()));
        match verify(&p, &e, strict, 500) {
            Ok(o) => o,
            Err(e) => panic!("{e}"),
        }
    }

    /// verify_messages.jsonl is written by clients/pathway_cases.py: plans
    /// and envelopes, and what the oracle's verify() found, message by
    /// message.
    #[test]
    fn violation_messages_match_the_oracle() {
        let path = std::env::var("DAISUGI_VERIFY_MESSAGES")
            .unwrap_or_else(|_| concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/pathways/verify_messages.jsonl").into());
        let text = std::fs::read_to_string(&path).unwrap();
        let (mut n, mut words, mut bad) = (0, 0, vec![]);
        for line in text.lines().filter(|l| !l.trim().is_empty()) {
            let c: serde_json::Value = serde_json::from_str(line).unwrap();
            let strict = c["strict"].as_bool();
            let o = outcome(c["plan"].as_str().unwrap(), c["envelope"].as_str().unwrap(), strict);
            n += 1;
            let want = c["expect"]["violations"].as_array().unwrap();
            let ok = c["expect"]["ok"].as_bool().unwrap();
            if ok != o.violations.is_empty() || want.len() != o.violations.len() {
                bad.push(format!("{}: got {:?}, want {want:?}", c["id"], o.violations));
                continue;
            }
            for (v, w) in o.violations.iter().zip(want) {
                let step = w[1].as_str().map(|s| s.to_string());
                let msg = w[2].as_str().unwrap();
                if v.stage != w[0].as_str().unwrap() || v.step != step {
                    bad.push(format!("{}: {:?} vs {w:?}", c["id"], v));
                    continue;
                }
                words += 1;
                if v.stage == "delegation" && msg.contains("delegation refused") {
                    continue;
                }
                if v.message != msg {
                    bad.push(format!("{}: message\n got {:?}\nwant {msg:?}", c["id"], v.message));
                }
            }
        }
        assert!(bad.is_empty(), "{} of {n} disagree:\n{}", bad.len(), bad[..bad.len().min(20)].join("\n"));
        assert!(n >= 300, "only {n} cases");
        eprintln!("{n} cases, {words} violations worded");
    }

    #[test]
    fn a_z3_unknown_is_a_timeout() {
        let plan = r#"{"id":"p","source":"s","task":"t","steps":[{"id":"s1","type":"task","prompt":"x"}]}"#;
        let low = format!(r#"{{"generated_by":"g","task":"{FORCE_Z3_UNKNOWN_TASK}","permissions":{{}}}}"#);
        let o = outcome(plan, &low, None);
        assert!(o.violations.is_empty());
        assert_eq!(o.timeouts, vec!["Z3 self-consistency check exceeded 500ms", "Z3 plan-vs-envelope check exceeded 500ms"]);
        let high = format!(r#"{{"generated_by":"g","task":"{FORCE_Z3_UNKNOWN_TASK}","permissions":{{}},"stakes":"high"}}"#);
        let o = outcome(plan, &high, None);
        assert_eq!(o.violations.len(), 1);
        assert_eq!(o.violations[0].message, "verifier timed out (envelope self-consistency check); raise the Z3 timeout");
        assert_eq!(o.timeouts, vec!["Z3 self-consistency check exceeded 500ms"]);
    }

    #[test]
    fn a_z3_unknown_in_skill_subsumption_is_a_timeout() {
        // The contract declares no file or MCP globs and the caller's
        // stakes, so the unknown comes from the final subsumption check.
        // It is a violation at every stakes level: an unproved delegation
        // never runs.
        for stakes in ["", r#""stakes":"high","#] {
            let plan = format!(
                r#"{{"id":"p","source":"s","task":"t","steps":[{{"id":"k1","type":"skill","skill_id":"pdf",
                "contract_envelope":{{"id":"{}","generated_by":"g","task":"t",{stakes}"permissions":{{"shell":true,"shell_allowlist":["make"]}}}}}}]}}"#,
                crate::subsumption::FORCE_UNKNOWN_ID
            );
            let env = format!(r#"{{"generated_by":"g","task":"t",{stakes}"permissions":{{"shell":true,"shell_allowlist":["make"]}}}}"#);
            let o = outcome(&plan, &env, None);
            assert_eq!(o.violations.len(), 1, "stakes {stakes:?}");
            assert_eq!(o.violations[0].stage, "delegation");
            assert_eq!(o.violations[0].step.as_deref(), Some("k1"));
            assert_eq!(o.violations[0].message, "verifier timed out (skill-delegation subsumption); raise the Z3 timeout");
            assert_eq!(o.timeouts, vec!["Z3 subsumption check exceeded 500ms"]);
        }
    }

    #[test]
    fn a_tuple_field_compares_as_a_python_tuple() {
        // The oracle's step dict holds the tuple (1.0, 2.0, 3.0), which
        // equals no list: an equals over it is violated, a not_equals
        // holds, and its length is the list's. Each answer is the one the
        // oracle's verify() gave for the same plan and invariant.
        let plan = r#"{"id":"p","source":"s","task":"t","steps":[{"id":"c1","type":"cartesian_move","target_position":[1.0,2.0,3.0]}]}"#;
        let env_with = |pred: &str| {
            format!(r#"{{"generated_by":"g","task":"t","permissions":{{}},"invariants":[{{"type":"rule","description":"d","expr":{pred}}}]}}"#)
        };
        let cases = [
            (r#"{"op":"forall_steps","pred":{"op":"equals","path":"target_position","value":[1.0,2.0,3.0]}}"#, false),
            (r#"{"op":"forall_steps","pred":{"op":"not_equals","path":"target_position","value":[1.0,2.0,3.0]}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"in_set","path":"target_position","values":[[1.0,2.0,3.0]]}}"#, false),
            (r#"{"op":"forall_steps","pred":{"op":"not_in_set","path":"target_position","values":[[1.0,2.0,3.0]]}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"length_range","path":"target_position","min":3,"max":3}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"is_empty","path":"target_orientation"}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"exists","path":"target_position.count"}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"exists","path":"target_position.append"}}"#, false),
            (r#"{"op":"forall_steps","pred":{"op":"exists","path":"target_position.__hash__"}}"#, true),
            (r#"{"op":"forall_steps","pred":{"op":"numeric_range","path":"target_position","min":0,"max":9}}"#, false),
            (r#"{"op":"forall_steps","pred":{"op":"matches","path":"target_position","regex":"1"}}"#, false),
            (r#"{"op":"equals","path":"steps","value":[{"id":"c1","type":"cartesian_move","target_position":[1.0,2.0,3.0]}]}"#, false),
        ];
        let p = validate_json(Id::ActionPlan, plan).unwrap();
        for (pred, holds) in cases {
            let e = validate_json(Id::Envelope, &env_with(pred)).unwrap();
            let o = match verify(&p, &e, None, 500) {
                Ok(o) => o,
                Err(e) => panic!("{pred}: {e}"),
            };
            let msgs: Vec<String> = o.violations.iter().map(|v| v.message.clone()).collect();
            if holds {
                assert!(msgs.is_empty(), "{pred}: {msgs:?}");
            } else {
                assert_eq!(msgs, vec!["invariant 'rule' violated".to_string()], "{pred}");
            }
        }
    }

    #[test]
    fn an_int_of_any_size_is_judged() {
        // pydantic takes 10**20; the max time is out of range, so the
        // envelope is inconsistent, as in the oracle. An int past 64 bits
        // elsewhere is carried, not refused.
        let big = "1".to_string() + &"0".repeat(400);
        let plan = format!(r#"{{"id":"p","source":"s","task":"t","steps":[{{"id":"a","type":"shell","command":"ls","metadata":{{"n":{big}}}}}]}}"#);
        let p = validate_json(Id::ActionPlan, &plan).unwrap();
        for t in ["100000000000000000000", "-100000000000000000000", big.as_str()] {
            let env = format!(r#"{{"generated_by":"g","task":"t","permissions":{{"shell":true,"shell_allowlist":["ls"],"max_execution_time_s":{t},"max_output_size_mb":{t}}}}}"#);
            let e = validate_json(Id::Envelope, &env).unwrap();
            let o = verify(&p, &e, None, 500).unwrap_or_else(|e| panic!("{e}"));
            let msgs: Vec<String> = o.violations.iter().map(|v| v.message.clone()).collect();
            assert_eq!(msgs, vec!["Envelope is internally inconsistent".to_string()]);
        }
        let env = r#"{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["ls"],"max_execution_time_s":3600}}"#;
        let e = validate_json(Id::Envelope, env).unwrap();
        assert!(verify(&p, &e, None, 500).unwrap().violations.is_empty());
    }

    #[test]
    fn a_time_out_of_range_is_inconsistent_on_z3() {
        let plan = r#"{"id":"p","source":"s","task":"t","steps":[]}"#;
        let env = r#"{"generated_by":"g","task":"t","permissions":{"max_execution_time_s":0}}"#;
        let o = outcome(plan, env, None);
        assert_eq!(o.violations.len(), 1);
        assert_eq!(o.violations[0].message, "Envelope is internally inconsistent");
    }
}

/// Lets a test make the per-step check answer as it chooses.
pub type StepCheck = fn(&Value, &Value, u32) -> Result<Outcome, PwErr>;

/// `verify.verify_step`, the supervisor's per-step gate on a one-step plan:
/// delegation safety, permissions, skill delegations, the robotics checks
/// and the (trivial) DAG, each stopping at the first stage with a
/// violation. The envelope's own Z3 checks and the plan-level predicates
/// are the whole-plan verify's.
pub fn verify_step(plan_v: &Value, env_v: &Value, timeout_ms: u32) -> Result<Outcome, PwErr> {
    let (plan, env) = typed(plan_v, env_v)?;
    let mut out = Outcome { violations: vec![], timeouts: vec![], warnings: vec![], warnings_unmodeled: false };
    out.violations.extend(crate::verify::check_delegation_safety(&plan, &env));
    if !out.violations.is_empty() {
        return Ok(out);
    }
    let strict = models::resolve_strict(None, &env);
    let mut parser = ShellParser::new();
    out.violations.extend(crate::verify::check_permissions(&plan, &env, strict, &mut parser));
    if !out.violations.is_empty() {
        return Ok(out);
    }
    let delegations = crate::verify::check_skill_delegations(
        &plan,
        &env,
        strict,
        Some((timeout_ms, &mut out.timeouts)),
        Some((&mut out.warnings, &mut out.warnings_unmodeled)),
    )
    .map_err(unreadable)?;
    out.violations.extend(delegations);
    if !out.violations.is_empty() {
        return Ok(out);
    }
    out.violations.extend(crate::z3_checks::check_plan_invariants(&plan, &env));
    if !out.violations.is_empty() {
        return Ok(out);
    }
    out.violations.extend(crate::dag::check_dag(&plan));
    Ok(out)
}

fn list_of(v: &Value) -> &[Value] {
    match v {
        Value::List(l) | Value::Tuple(l) => l,
        _ => &[],
    }
}

fn text_of(v: &Value) -> String {
    v.as_str().unwrap_or("").to_string()
}

/// Whether a value is JSON null or absent.
fn is_none(v: &Value) -> bool {
    matches!(v, Value::Null)
}

/// Names an enforced invariant or postcondition whose expr is a dict that
/// does not parse as a predicate, or "" when there is none. The oracle
/// raises a ValidationError out of verify there (a traceback); a caller
/// refuses such an envelope before anything runs.
pub fn stage2_refusal(env: &Value) -> String {
    let o = env.as_obj().cloned().unwrap_or_default();
    let unread = |expr: &Value| {
        expr.as_obj().is_some()
            && to_serde(expr).map_err(|e| e.to_string()).and_then(|v| crate::predicate::parse_expression(&v)).is_err()
    };
    for (label, key) in [("invariant", "invariants"), ("postcondition", "postconditions")] {
        for it in list_of(o.value(key)) {
            let Some(it) = it.as_obj() else { continue };
            if !matches!(it.value("enforce"), Value::Bool(true)) {
                continue;
            }
            if unread(it.value("expr")) {
                return format!("{label} '{}' has an expr this binary does not read", text_of(it.value("type")));
            }
        }
    }
    String::new()
}

/// The model a stage-2 llm_check asks: `run_llm_check(rule, payload)` over
/// the pseudo-plan of one completed step, through the world the command
/// set. A failed call is the error the evaluator raises; a call this
/// binary does not make the oracle's way ends in " is not in this binary
/// yet".
fn stage2_llm(task: &Value, step: &Value) -> impl Fn(&str) -> Result<bool, String> {
    let payload = Object::new().with("task", task.clone()).with("steps", Value::List(vec![dumped_step(step)]));
    move |rule: &str| match crate::gate::llm::command_llm_check(rule, &payload) {
        None => Err("llm_check is not reproducible offline (network/model call)".into()),
        Some(Ok(res)) if res.errored => Err(res.reason),
        Some(Ok(res)) => Ok(res.satisfied),
        Some(Err(Fault::Undecided(w))) => Err(format!("{w} is not in this binary yet")),
        Some(Err(Fault::Raised(e))) => Err(e.msg),
    }
}

/// An evaluation error every caller words as the oracle does: an
/// llm_check that failed or was blocked, and an alias left unresolved.
fn worded_eval_error(m: &str) -> bool {
    !m.ends_with(" is not in this binary yet")
        && (m.starts_with("error: llm_check call failed: ")
            || m.starts_with("llm_check blocked for physical stakes")
            || m.starts_with("unresolved alias reference '"))
}

/// A number of a postcondition field compared with `n`: `lo <= n` when
/// `below`, else `n <= hi`. An int of any size compares exactly.
fn int_holds(bound: &Value, n: i128, below: bool) -> bool {
    let Value::Int(t) = bound else { return false };
    match t.parse::<i128>() {
        Ok(b) => {
            if below {
                b <= n
            } else {
                n <= b
            }
        }
        // Past 128 bits: a negative bound is below every size, a positive
        // one above it.
        Err(_) => t.starts_with('-') == below,
    }
}

/// One of stage 2's opaque postcondition handlers: true when discharged.
fn stage2_handler(kind: &str, pc: &crate::gate::pyjson::Object, step: &crate::gate::pyjson::Object) -> bool {
    match kind {
        "exit_code" => {
            let rc = step.value("metadata").as_obj().map(|m| m.value("rc").clone()).unwrap_or(Value::Null);
            match (rc, pc.value("expected")) {
                (Value::Int(a), Value::Int(b)) => a.parse::<i128>().ok().is_some_and(|x| b.parse::<i128>().ok() == Some(x)),
                _ => false,
            }
        }
        "file_exists" => match pc.value("path") {
            Value::Str(p) if !p.is_empty() => std::fs::metadata(p).is_ok(),
            _ => false,
        },
        "file_size_range" => {
            let Value::Str(p) = pc.value("path") else { return false };
            if p.is_empty() || (is_none(pc.value("min")) && is_none(pc.value("max"))) {
                return false;
            }
            let Ok(md) = std::fs::metadata(p) else { return false };
            let size = md.len() as i128;
            let lo_ok = if is_none(pc.value("min")) { 0 <= size } else { int_holds(pc.value("min"), size, true) };
            let hi_ok = is_none(pc.value("max")) || int_holds(pc.value("max"), size, false);
            lo_ok && hi_ok
        }
        _ => false,
    }
}

/// `stage2.verify_completed_step`: the envelope's enforced postconditions
/// over one completed step, whose metadata holds its output and rc. Only
/// the violation messages are returned; the supervisor reads the first.
pub fn verify_completed_step(step: &Value, env: &Value, strict: Option<bool>) -> Vec<String> {
    let o = env.as_obj().cloned().unwrap_or_default();
    let stakes = match o.value("stakes") {
        Value::Str(s) => s.clone(),
        _ => "low".to_string(),
    };
    let effective = models::resolve_strict_stakes(strict, &stakes);
    let step_o = step.as_obj().cloned().unwrap_or_default();
    let step_id = text_of(step_o.value("id"));
    let mut out = vec![];
    for pc in list_of(o.value("postconditions")) {
        let Some(pc) = pc.as_obj() else { continue };
        if !matches!(pc.value("enforce"), Value::Bool(true)) {
            continue;
        }
        let kind = text_of(pc.value("type"));
        let expr = pc.value("expr");
        if is_none(expr) {
            if ["exit_code", "file_exists", "file_size_range"].contains(&kind.as_str()) {
                if !stage2_handler(&kind, pc, &step_o) {
                    out.push(format!("postcondition '{kind}' violated on completed step {step_id}"));
                }
                continue;
            }
            if effective {
                out.push(format!(
                    "postcondition '{kind}' declares a safety property with no verifiable expr; \
                     cannot be discharged under strict mode"
                ));
            }
            continue;
        }
        if expr.as_obj().is_none() {
            out.push(format!(
                "postcondition '{kind}' evaluation error: unknown predicate op: '{}'",
                crate::gate::pyjson::py_type_name(expr)
            ));
            continue;
        }
        if past_u64(expr) || past_u64(&dumped_step(step)) {
            // Fails closed: this evaluator holds ints to 64 bits.
            out.push(format!(
                "postcondition '{kind}' evaluation error: an int past 64 bits, which this binary does not compare exactly"
            ));
            continue;
        }
        let parsed = to_serde_u64(expr).map_err(|e| e.to_string()).and_then(|v| crate::predicate::parse_expression(&v));
        let expr = match parsed {
            Ok(e) => e,
            Err(e) => {
                out.push(format!("postcondition '{kind}' evaluation error: {e}"));
                continue;
            }
        };
        let steps = match to_serde_u64(&dumped_step(step)) {
            Ok(s) => vec![s],
            Err(e) => {
                out.push(format!("postcondition '{kind}' evaluation error: {e}"));
                continue;
            }
        };
        let ask = stage2_llm(o.value("task"), step);
        match crate::predicate::evaluate_predicate_with(&expr, &steps, &stakes, Some(&ask)) {
            Ok(true) => {}
            Ok(false) => out.push(format!("postcondition '{kind}' violated on completed step {step_id}")),
            Err(e) => out.push(format!("postcondition '{kind}' evaluation error: {e}")),
        }
    }
    out
}

/// Whether `v` holds an int past i64: `to_serde` clamps one to the
/// nearest i64 bound, so two of them could compare equal (or read as
/// another float) where Python's exact ints do not. A check that compares
/// such a copy refuses first.
pub(crate) fn wide_int(v: &Value) -> bool {
    match v {
        Value::Int(t) => t.parse::<i64>().is_err(),
        Value::List(l) | Value::Tuple(l) => l.iter().any(wide_int),
        Value::Obj(o) => o.keys().iter().any(|k| wide_int(o.value(k))),
        _ => false,
    }
}

/// Whether `v` holds an int past 64 bits (signed or not), which
/// `to_serde_u64` cannot hold.
pub(crate) fn past_u64(v: &Value) -> bool {
    match v {
        Value::Int(t) => t.parse::<i64>().is_err() && t.parse::<u64>().is_err(),
        Value::List(l) | Value::Tuple(l) => l.iter().any(past_u64),
        Value::Obj(o) => o.keys().iter().any(|k| past_u64(o.value(k))),
        _ => false,
    }
}

/// `to_serde` for stage 2's evaluator: an int to 64 bits, signed or not,
/// held exactly (an int in 2^63..2^64 is a u64, not clamped to i64).
/// The caller refuses an int past that first (`past_u64`).
fn to_serde_u64(v: &Value) -> Result<serde_json::Value, PwErr> {
    Ok(match v {
        Value::Int(t) => match t.parse::<u64>() {
            Ok(n) => serde_json::Value::Number(n.into()),
            Err(_) => to_serde(v)?,
        },
        Value::List(l) | Value::Tuple(l) => {
            serde_json::Value::Array(l.iter().map(to_serde_u64).collect::<Result<_, _>>()?)
        }
        Value::Obj(o) => {
            let mut m = serde_json::Map::new();
            for k in o.keys() {
                m.insert(k.clone(), to_serde_u64(o.value(k))?);
            }
            serde_json::Value::Object(m)
        }
        _ => to_serde(v)?,
    })
}

/// Python's `rc == expected` for a JSON value and an int's text.
fn py_equals_int(v: &Value, want: &str) -> bool {
    let Ok(w) = want.parse::<i128>() else { return false };
    match v {
        Value::Bool(b) => w == *b as i128,
        Value::Int(t) => t.parse::<i128>().ok() == Some(w),
        Value::Float(f) => f.is_finite() && f.fract() == 0.0 && f.abs() < 1e38 && *f as i128 == w,
        _ => false,
    }
}

fn stage2_violation(message: String, detail: Object) -> Value {
    Value::Obj(
        Object::new()
            .with("stage", "stage2")
            .with("message", message)
            .with("detail", Value::Obj(detail))
            .with("suggested_remediation", Value::Null),
    )
}

/// One of `_OPAQUE_POSTCONDITION_HANDLERS` with its detail: whether the
/// postcondition is discharged and what it saw. A path with a NUL, whose
/// handler error this binary does not word, is refused.
fn stage2_handler_detail(kind: &str, pc: &Object, step: &Object) -> Result<(bool, Object), String> {
    let reason = |r: &str| Ok((false, Object::new().with("reason", r)));
    let path = pc.value("path").as_str().unwrap_or("").to_string();
    if path.contains('\0') {
        return Err("a postcondition path with a NUL, whose handler error this binary does not word".into());
    }
    match kind {
        "exit_code" => {
            let rc = step.value("metadata").as_obj().map(|m| m.value("rc").clone()).unwrap_or(Value::Null);
            if rc.is_null() {
                return reason("step metadata missing rc");
            }
            let Value::Int(want) = pc.value("expected") else {
                return reason("exit_code postcondition missing `expected`");
            };
            let ok = py_equals_int(&rc, want);
            Ok((ok, Object::new().with("observed_rc", rc).with("expected", Value::Int(want.clone()))))
        }
        "file_exists" => {
            if path.is_empty() {
                return reason("file_exists postcondition missing `path`");
            }
            let exists = std::fs::metadata(&path).is_ok();
            Ok((exists, Object::new().with("path", path.as_str()).with("exists", exists)))
        }
        _ => {
            if path.is_empty() {
                return reason("file_size_range postcondition missing `path`");
            }
            let lo = match pc.value("min") {
                Value::Int(t) => Some(t.clone()),
                _ => None,
            };
            let hi = match pc.value("max") {
                Value::Int(t) => Some(t.clone()),
                _ => None,
            };
            if lo.is_none() && hi.is_none() {
                return reason(
                    "file_size_range postcondition needs at least one of `min` / `max`; otherwise it constrains nothing",
                );
            }
            let Ok(md) = std::fs::metadata(&path) else {
                return Ok((false, Object::new().with("path", path.as_str()).with("reason", "file does not exist")));
            };
            let size = md.len() as i128;
            let mut ok = true;
            let lo_v = match &lo {
                Some(t) => {
                    ok = int_holds(&Value::Int(t.clone()), size, true);
                    Value::Int(t.clone())
                }
                None => Value::Int("0".into()),
            };
            // inf: None in the JSON dump.
            let hi_v = match &hi {
                Some(t) => {
                    ok = ok && int_holds(&Value::Int(t.clone()), size, false);
                    Value::Int(t.clone())
                }
                None => Value::Null,
            };
            Ok((
                ok,
                Object::new()
                    .with("path", path.as_str())
                    .with("size", Value::Int(size.to_string()))
                    .with("min", lo_v)
                    .with("max", hi_v),
            ))
        }
    }
}

/// `stage2.verify_completed_step` over one validated step, each violation
/// as `model_dump(mode="json")` gives it. An expr this binary does not
/// evaluate the oracle's way is refused (`Err`, the reason).
pub fn stage2_violations(step: &Object, env: &Object) -> Result<Vec<Value>, String> {
    let stakes = match env.value("stakes") {
        Value::Str(s) => s.clone(),
        _ => "low".to_string(),
    };
    let strict = models::resolve_strict_stakes(None, &stakes);
    let pseudo = Object::new()
        .with("id", "plan_00000000")
        .with("source", "stage2")
        .with("task", env.value("task").clone())
        .with("steps", Value::List(vec![Value::Obj(step.clone())]));
    let parsed = to_serde(&Value::Obj(pseudo)).ok().and_then(|v| models::parse_plan(&v).ok());
    if parsed.map(|p| p.steps.len()) != Some(1) {
        return Err("a step the verifier does not read".into());
    }
    let step_id = step.value("id").clone();
    let id_text = crate::gate::pyjson::py_str(&step_id).unwrap_or_default();
    let mut out = vec![];
    for pc in list_of(env.value("postconditions")) {
        let Some(pc) = pc.as_obj() else { continue };
        if !matches!(pc.value("enforce"), Value::Bool(true)) {
            continue;
        }
        let kind = text_of(pc.value("type"));
        let expr = pc.value("expr");
        if is_none(expr) {
            if ["exit_code", "file_exists", "file_size_range"].contains(&kind.as_str()) {
                let (ok, detail) = stage2_handler_detail(&kind, pc, step)?;
                if !ok {
                    let mut d = Object::new().with("postcondition", kind.as_str()).with("step_id", step_id.clone());
                    for (k, v) in detail.iter() {
                        d.set(k, v.clone());
                    }
                    out.push(stage2_violation(
                        format!("postcondition '{kind}' violated on completed step {id_text}"),
                        d,
                    ));
                }
                continue;
            }
            if strict {
                out.push(stage2_violation(
                    format!(
                        "postcondition '{kind}' declares a safety property with no verifiable expr; \
                         cannot be discharged under strict mode"
                    ),
                    Object::new()
                        .with("postcondition", kind.as_str())
                        .with("reason", "opaque_unrecognized")
                        .with("step_id", step_id.clone())
                        .with(
                            "suggested_remediation",
                            "add an `expr` to make it verifiable, or set enforce=False to keep it as documentation",
                        ),
                ));
            }
            continue;
        }
        let eval_error = |m: String| {
            stage2_violation(
                format!("postcondition '{kind}' evaluation error: {m}"),
                Object::new().with("postcondition", kind.as_str()).with("step_id", step_id.clone()),
            )
        };
        if expr.as_obj().is_none() {
            // _normalize_expr passes a non-dict through; evaluation then
            // raises on its Python type.
            out.push(eval_error(format!("unknown predicate op: '{}'", crate::gate::pyjson::py_type_name(expr))));
            continue;
        }
        let step_v = Value::Obj(step.clone());
        if past_u64(expr) || past_u64(&dumped_step(&step_v)) {
            return Err(format!(
                "postcondition '{kind}' compares an int past 64 bits, which this binary does not hold exactly"
            ));
        }
        let pexpr = to_serde_u64(expr)
            .map_err(|e| e.to_string())
            .and_then(|v| crate::predicate::parse_expression(&v))
            .map_err(|_| format!("postcondition '{kind}' has an expr this binary does not read"))?;
        let steps = to_serde_u64(&dumped_step(&step_v))
            .map(|s| vec![s])
            .map_err(|_| "a step the verifier does not read".to_string())?;
        let ask = stage2_llm(env.value("task"), &step_v);
        let ok = match crate::predicate::evaluate_predicate_with(&pexpr, &steps, &stakes, Some(&ask)) {
            Ok(ok) => ok,
            Err(m) if worded_eval_error(&m) => {
                out.push(eval_error(m));
                continue;
            }
            Err(_) => return Err(format!("postcondition '{kind}' meets an evaluation error this binary does not word")),
        };
        if !ok {
            out.push(stage2_violation(
                format!("postcondition '{kind}' violated on completed step {id_text}"),
                Object::new()
                    .with("postcondition", kind.as_str())
                    .with("description", pc.value("description").clone())
                    .with("step_id", step_id.clone()),
            ));
        }
    }
    Ok(out)
}

/// `dag.check_dag(plan)` on a plan's `model_dump()`.
pub fn dag_violations(plan_v: &Value) -> Result<Vec<Violation>, PwErr> {
    let p = to_serde(plan_v)?;
    let plan = models::parse_plan(&p).map_err(unreadable)?;
    Ok(crate::dag::check_dag(&plan))
}
