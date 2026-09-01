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
use crate::gate::pyjson::Value;
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
    let mut out = Outcome { violations: vec![], timeouts: vec![] };
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
    let delegations = crate::verify::check_skill_delegations(&plan, &env, strict, Some((timeout_ms, &mut out.timeouts)))
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
            Ok(Check::Unsat) => out.violations.push(Violation::plan("z3").msg(*unsat_msg)),
            Ok(Check::Sat) => {}
            Ok(Check::Unknown) => {
                let text = format!("{exceeded} {timeout_ms}ms");
                if timeout_is_violation {
                    out.violations.push(
                        Violation::plan("z3").msg(format!("verifier timed out ({which}); raise the Z3 timeout")),
                    );
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
    let vs = match Stage::for_plan(&genv, steps, strict).run() {
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
    for v in vs {
        out.violations.push(Violation::plan("predicate").msg(v.message));
    }
    // Stage 2c: the robotics trajectory checks.
    out.violations.extend(crate::z3_checks::check_plan_invariants(&plan, &env));
    if done(&out) {
        return Ok(out);
    }
    out.violations.extend(crate::dag::check_dag(&plan));
    Ok(out)
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
        // The contract declares no file or MCP globs, so the unknown comes
        // from the final subsumption check, as the oracle's
        // VerificationTimeout does.
        let plan = format!(
            r#"{{"id":"p","source":"s","task":"t","steps":[{{"id":"k1","type":"skill","skill_id":"pdf",
            "contract_envelope":{{"id":"{}","generated_by":"g","task":"t","permissions":{{"shell":true,"shell_allowlist":["make"]}}}}}}]}}"#,
            crate::subsumption::FORCE_UNKNOWN_ID
        );
        let low = r#"{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["make"]}}"#;
        let o = outcome(&plan, low, None);
        assert!(o.violations.is_empty(), "{:?}", o.violations);
        assert_eq!(o.timeouts, vec!["Z3 subsumption check exceeded 500ms"]);
        let high = low.replace(r#""permissions""#, r#""stakes":"high","permissions""#);
        let o = outcome(&plan, &high, None);
        assert_eq!(o.violations.len(), 1);
        assert_eq!(o.violations[0].stage, "delegation");
        assert_eq!(o.violations[0].step.as_deref(), Some("k1"));
        assert_eq!(o.violations[0].message, "verifier timed out (skill-delegation subsumption); raise the Z3 timeout");
        assert_eq!(o.timeouts, vec!["Z3 subsumption check exceeded 500ms"]);
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
