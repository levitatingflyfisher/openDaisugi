//! The decomposer, sizing and budget, and a Z3 check that does not finish
//! in the orchestrator's verifies (K2-2).

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::atomic::{AtomicUsize, Ordering};

use super::*;
use crate::gate::pyjson::{dumps, loads, Value};
use crate::pathways::pmodel::{steps as registry, Id};
use crate::pathways::verify::Outcome as Verified;
use crate::pathways::PwErr;
use crate::supervise::tests::{model, scratch};

/// A client on the claude-code backend whose `claude` answers every call
/// with `reply` as the result text.
fn fake_claude(reply: &str) -> (Rc<RefCell<Client>>, String) {
    let dir = scratch("orch");
    let env = dumps(&Value::Obj(Object::new().with("type", "result").with("is_error", false).with("result", reply)), false);
    let script = format!("{dir}/claude");
    std::fs::write(&script, format!("#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n{env}\nEOF\n")).unwrap();
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
    let vars: HashMap<String, String> = [
        ("OPENDAISUGI_LLM_BACKEND".to_string(), "claude-code".to_string()),
        ("PATH".to_string(), format!("{dir}:/usr/bin:/bin")),
        ("TMPDIR".to_string(), dir.clone()),
    ]
    .into();
    (Rc::new(RefCell::new(Client::new(vars, &dir))), dir)
}

fn envelope() -> Value {
    loads(
        r#"{"id": "env_00000001", "generated_by": "t", "task": "t",
        "permissions": {"file_read": [], "file_write": [], "network": false, "shell": false, "shell_allowlist": []}}"#,
    )
    .unwrap()
}

const ONE_TASK: &str = r#"{"steps": [{"id": "a", "type": "task", "prompt": "x"}]}"#;

fn verify_unknown(_: &Value, _: &Value, _: Option<bool>, _: u32) -> Result<Verified, PwErr> {
    Ok(Verified { violations: vec![], timeouts: vec!["Z3 returned unknown".into()], warnings: vec![], warnings_unmodeled: false })
}

fn msg(e: DecomposeErr) -> String {
    match e {
        DecomposeErr::Decomposition { msg, .. } => msg,
        other => panic!("{other:?}"),
    }
}

#[test]
fn a_prompt_is_decomposed() {
    let (c, dir) = fake_claude(ONE_TASK);
    let plan = decompose(&c, "p", DEFAULT_DECOMPOSE_MODEL, &envelope(), 500, crate::pathways::verify::verify).unwrap();
    assert_eq!(plan.value("source").as_str(), Some("decomposer"));
    assert_eq!(crate::tracejournal::dag::steps(&plan).len(), 1);
    let _ = std::fs::remove_dir_all(dir);
}

/// A Z3 check that does not finish when the decomposed plan is verified
/// fails the decomposition (fail closed), where the oracle would warn.
#[test]
fn a_decomposition_whose_z3_check_did_not_finish_fails() {
    let (c, dir) = fake_claude(ONE_TASK);
    let e = decompose(&c, "p", DEFAULT_DECOMPOSE_MODEL, &envelope(), 500, verify_unknown).unwrap_err();
    assert_eq!(msg(e), "decomposed plan failed verify against envelope (out of policy): [z3] Z3 returned unknown");
    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn a_decomposition_is_refused() {
    for (reply, want) in [
        (r#"{"steps": []}"#, "decomposition produced no steps"),
        (r#"{"steps": [{"id": "a", "type": "task", "prompt": "x", "depends_on": ["a"]}]}"#, "decomposed plan is not a valid DAG: "),
        (r#"{"steps": [{"id": "a", "type": "shell"}]}"#, "step 'a' (type 'shell') is missing required fields: 1 validation error for ShellStep"),
        (
            r#"{"steps": [{"id": "a", "type": "shell", "command": "ls"}]}"#,
            "decomposed plan failed verify against envelope (out of policy): [permissions]",
        ),
        (r#"{"steps": [{"id": "a", "type": "task", "prompt": "x", "arguments": {"n": NaN}}]}"#, "decomposition LLM call failed: "),
    ] {
        let (c, dir) = fake_claude(reply);
        let e = decompose(&c, "p", DEFAULT_DECOMPOSE_MODEL, &envelope(), 500, crate::pathways::verify::verify).unwrap_err();
        let m = msg(e);
        assert!(m.starts_with(want), "{reply}: {m}");
        let _ = std::fs::remove_dir_all(dir);
    }
}

fn task_step(text: &str) -> Object {
    let i = registry().order.iter().position(|t| *t == "task").unwrap();
    model(Id::Step(i), text)
}

/// Sizing: the step type's floor, the prompt's signals, fan-in, and the
/// budget's downgrade.
#[test]
fn a_step_is_sized() {
    let l = build_ladder("");
    let easy = task_step(r#"{"id": "a", "type": "task", "prompt": "x", "depends_on": ["b"]}"#);
    let s = sizing::size_step(&easy, &l, None, "");
    assert!(s.difficulty == 0.35 && s.tier == "cheap" && s.affordable, "{s:?}");
    let hard = task_step(r#"{"id": "h", "type": "task", "prompt": "design the security architecture"}"#);
    assert_eq!(sizing::size_step(&hard, &l, None, "").tier, "frontier");
    let s = sizing::size_step(&hard, &l, Some(3000.0), "");
    assert!(s.tier == "cheap" && s.downgraded, "{s:?}");
    let s = sizing::size_step(&hard, &l, Some(10.0), "");
    assert!(!s.affordable && s.downgraded, "{s:?}");
}

#[test]
fn the_budget_report() {
    let t = Tracker::default();
    assert_eq!(
        dumps(&Value::Obj(t.report().dump()), true),
        r#"{"total": null, "spent": 0, "remaining": null, "step_count": 0, "by_model": {}, "approx_cost_usd": 0, "measured_cost_usd": null}"#
    );
    let mut t = Tracker::new(Some(100), true);
    assert!(t.record("a", "claude-haiku-4-5", 101, None).is_err(), "strict overrun not raised");
    assert!(t.exhausted() && t.report().spent == 101, "the spend was not counted");
}

static CALLS: AtomicUsize = AtomicUsize::new(0);

fn second_unknown(p: &Value, e: &Value, s: Option<bool>, z: u32) -> Result<Verified, PwErr> {
    if CALLS.fetch_add(1, Ordering::SeqCst) == 0 {
        return crate::pathways::verify::verify(p, e, s, z);
    }
    verify_unknown(p, e, s, z)
}

/// A Z3 check that does not finish in the verify just before the run
/// rejects the run (fail closed): nothing runs, and it is journaled.
#[test]
fn a_run_whose_z3_check_did_not_finish_is_rejected() {
    let (c, dir) = fake_claude(ONE_TASK);
    let env = model(Id::Envelope, r#"{"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}}"#);
    let j = crate::tracejournal::Journal::open(&dir).unwrap();
    let r = run(Options {
        llm: c,
        prompt: "p".into(),
        env,
        budget: None,
        strict_budget: false,
        synth_llm: false,
        store: None,
        matcher_key: "lexical".into(),
        potion: None,
        journal: Some(&j),
        decompose_model: DEFAULT_DECOMPOSE_MODEL.into(),
        z3_timeout_ms: 500,
        step_timeout_s: 30,
        ladder: build_ladder(""),
        fallback: None,
        shell_env: HashMap::new(),
        check: second_unknown,
        max_parallel: 1,
        warn: None,
        agentic: None,
    })
    .map_err(|e| format!("{e:?}"))
    .unwrap();
    assert_eq!(r.session.status, "rejected");
    assert!(r.session.steps.is_empty());
    let body = std::fs::read_to_string(j.trace_path(r.session.trace_id.as_deref().unwrap())).unwrap();
    assert!(body.contains("stage: z3"), "{body}");
    let _ = std::fs::remove_dir_all(dir);
}
