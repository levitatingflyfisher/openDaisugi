//! `robot-probe`: the robotics executors on the robotics cases
//! (clients/robotics_cases.py). It reads CASES, a JSON line per case, and
//! writes {"id", "result"} per case, each result in the shape the oracle's
//! run_case gives it. The twin of the Go probe; only the mujoco feature
//! builds it.

use std::collections::BTreeMap;
use std::io::BufRead;
use std::path::Path;

use crate::gate::pyjson::{dumps, loads, Object, Value};
use crate::mujoco::{Data, Model, Obj, Renderer};
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::robotics::{self, mock_predictor, Num, Options, PyError, Vla};
use crate::supervise::{approval::Always, ExecResult, Executor, Supervisor};
use crate::tracejournal::runs::ReceiptRow;
use crate::tracejournal::Journal;

type R<T> = Result<T, String>;

pub fn main() -> i32 {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.len() != 2 {
        eprintln!("usage: robot-probe CASES_JSONL REPO_ROOT");
        return 2;
    }
    let f = match std::fs::File::open(&args[0]) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("{e}");
            return 1;
        }
    };
    for line in std::io::BufReader::new(f).lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("{e}");
                return 1;
            }
        };
        let c = match loads(&line) {
            Ok(Value::Obj(o)) => o,
            _ => {
                eprintln!("a case line is not a JSON object");
                return 1;
            }
        };
        match robot_case(&c, &args[1]) {
            Ok(res) => {
                let out = Object::new()
                    .with("id", c.value("id").clone())
                    .with("result", res);
                println!("{}", dumps(&Value::Obj(out), true));
            }
            Err(e) => {
                eprintln!("case {}: {e}", dumps(c.value("name"), true));
                return 1;
            }
        }
    }
    0
}

fn robot_case(c: &Object, root: &str) -> R<Value> {
    match c.value("kind").as_str() {
        Some("steps") => robot_steps(c, root),
        Some("vla") => robot_vla(c, root),
        Some("run") => robot_run(c, root),
        Some("render") => robot_render(c, root),
        _ => Err(format!("no case kind {}", dumps(c.value("kind"), true))),
    }
}

fn mjcf_of(c: &Object, root: &str) -> String {
    match c.value("mjcf").as_str() {
        Some(p) if !p.is_empty() => Path::new(root).join(p).to_string_lossy().into_owned(),
        _ => String::new(),
    }
}

/// MuJoCoExecutor's keyword arguments from the case.
fn robot_options(c: &Object) -> Options {
    let mut o = Options::default();
    let Some(kw) = c.value("executor").as_obj() else {
        return o;
    };
    for (k, v) in kw.iter() {
        let n = Num(v.clone());
        match k.as_str() {
            "settle_steps" => o.settle_steps = n.f() as i64,
            "position_tol" => o.position_tol = n.f(),
            "torque_limit" if !matches!(v, Value::Null) => o.torque_limit = Some(n),
            "forbid_contacts" => o.forbid_contacts = matches!(v, Value::Bool(true)),
            "ee_body" => o.ee_body = v.as_str().unwrap_or("").to_string(),
            "ik_max_iter" => o.ik_max_iter = n.f() as i64,
            "ik_tol" => o.ik_tol = n,
            "ik_damping" => o.ik_damping = n.f(),
            _ => {}
        }
    }
    o
}

fn guards(o: &Options) -> Value {
    let tl = o
        .torque_limit
        .as_ref()
        .map(|n| n.json())
        .unwrap_or(Value::Null);
    Value::Obj(
        Object::new()
            .with("torque_limit", tl)
            .with("forbid_contacts", o.forbid_contacts),
    )
}

fn floats(xs: &[f64]) -> Value {
    Value::List(xs.iter().map(|x| Value::Float(*x)).collect())
}

fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(xs) => xs
            .iter()
            .map(|x| x.as_str().unwrap_or("").to_string())
            .collect(),
        _ => vec![],
    }
}

/// The simulation state a case ends with.
fn robot_state(m: &Model, d: &Data, c: &Object) -> Value {
    let named = |kind: Obj, names: Vec<String>, pos: &dyn Fn(usize) -> [f64; 3]| {
        let mut out = Object::new();
        for n in names {
            let i = m.name2id(kind, &n);
            let v = if i < 0 {
                Value::Null
            } else {
                floats(&pos(i as usize))
            };
            out.set(&n, v);
        }
        Value::Obj(out)
    };
    Value::Obj(
        Object::new()
            .with("qpos", floats(d.qpos()))
            .with("qvel", floats(d.qvel()))
            .with("ctrl", floats(d.ctrl()))
            .with("ncon", Value::Int(d.ncon().to_string()))
            .with(
                "bodies",
                named(Obj::Body, str_list(c.value("bodies")), &|i| d.xpos(i)),
            )
            .with(
                "sites",
                named(Obj::Site, str_list(c.value("sites")), &|i| d.site_xpos(i)),
            ),
    )
}

fn model(id: Id, v: &Value) -> R<Object> {
    match validate_model(id, v, Mode::Python).map_err(|e| format!("{e:?}"))? {
        Value::Obj(o) => Ok(o),
        _ => Err("a model validates to an object".into()),
    }
}

/// The case's steps, validated as ActionPlan.steps.
fn case_steps(c: &Object) -> R<Vec<Object>> {
    let plan = Object::new()
        .with("source", "robotics-case")
        .with("task", "robotics case")
        .with("steps", c.value("steps").clone());
    let p = model(Id::ActionPlan, &Value::Obj(plan))?;
    Ok(match p.value("steps") {
        Value::List(xs) => xs.iter().filter_map(|x| x.as_obj().cloned()).collect(),
        _ => vec![],
    })
}

fn py_err(e: &PyError) -> Value {
    Value::Obj(
        Object::new()
            .with("error", e.ty.as_str())
            .with("message", e.msg.as_str()),
    )
}

fn result_object(r: &ExecResult) -> Value {
    Value::Obj(
        Object::new()
            .with("rc", Value::Int(r.rc.to_string()))
            .with("stdout", r.stdout.as_str())
            .with("timed_out", r.timed_out),
    )
}

fn robot_steps(c: &Object, root: &str) -> R<Value> {
    let mut x = match robotics::MuJoCo::new(&mjcf_of(c, root), robot_options(c)) {
        Ok(x) => x,
        Err(e) => return Ok(Value::Obj(Object::new().with("made", py_err(&e)))),
    };
    let mut out = Object::new();
    if !matches!(c.value("envelope"), Value::Null) {
        x.configure_from_envelope(&model(Id::Envelope, c.value("envelope"))?);
    }
    out.set("guards", guards(&x.opt));
    let mut results = vec![];
    for s in case_steps(c)? {
        results.push(match x.run_step(&s) {
            Ok(r) => Value::Obj(Object::new().with("ok", result_object(&r))),
            Err(e) => py_err(&e),
        });
    }
    out.set("results", Value::List(results));
    out.set("state", robot_state(x.model(), x.data(), c));
    Ok(Value::Obj(out))
}

fn new_case_vla(c: &Object, root: &str) -> R<Vla> {
    let empty = Object::new();
    let cfg = c.value("vla").as_obj().unwrap_or(&empty);
    let mut path = mjcf_of(c, root);
    if path.is_empty() {
        path = cfg.value("mjcf_path").as_str().unwrap_or("").to_string();
    }
    let kw = cfg.value("kwargs").as_obj().unwrap_or(&empty);
    let int_kw = |k: &str, d: i64| kw.get(k).map(|v| Num(v.clone()).f() as i64).unwrap_or(d);
    let v = if cfg.value("base").truthy() {
        Vla::new(&path, int_kw("max_actions_global", 200), None)
    } else {
        Vla::new(&path, 200, Some(mock_predictor(int_kw("num_actions", 10))))
    };
    v.map_err(|e| format!("{}: {}", e.ty, e.msg))
}

fn robot_vla(c: &Object, root: &str) -> R<Value> {
    let mut v = new_case_vla(c, root)?;
    let mut results = vec![];
    for s in case_steps(c)? {
        results.push(result_object(&v.run_step(&s, 30)));
    }
    let names = Value::List(
        v.joint_names()
            .iter()
            .map(|n| Value::Str(n.clone()))
            .collect(),
    );
    let mut out = Object::new()
        .with("joint_names", names)
        .with("results", Value::List(results));
    if let Some((m, d)) = v.sim() {
        out.set("state", robot_state(m, d, c));
    }
    Ok(Value::Obj(out))
}

/// Receipt.model_dump(mode="json").
fn receipt_dump(r: &ReceiptRow) -> Value {
    let opt = |s: &Option<String>| s.clone().map(Value::Str).unwrap_or(Value::Null);
    Value::Obj(
        Object::new()
            .with("step_id", r.step_id.as_str())
            .with("run_id", r.run_id.as_str())
            .with("timestamp", Value::Float(r.timestamp))
            .with("evidence", Value::Obj(r.evidence.clone()))
            .with("evidence_hash", r.evidence_hash.as_str())
            .with("verify_result", r.verify_result)
            .with("verify_details", r.verify_details.as_str())
            .with("model_id", opt(&r.model_id))
            .with("effect_class", opt(&r.effect_class))
            .with("reversibility", opt(&r.reversibility))
            .with(
                "reversal",
                r.reversal.clone().map(Value::Obj).unwrap_or(Value::Null),
            ),
    )
}

fn robot_run(c: &Object, root: &str) -> R<Value> {
    let (pairs, shared) = robotics::robotics_executors(&mjcf_of(c, root), robot_options(c))
        .map_err(|e| format!("{}: {}", e.ty, e.msg))?;
    let mut executors: BTreeMap<String, Box<dyn Executor>> = pairs.into_iter().collect();
    // The VLA executor has its own simulation; the probe keeps a handle to
    // read its state after the run.
    let vla = if matches!(c.value("vla"), Value::Null) {
        None
    } else {
        let v = std::rc::Rc::new(std::cell::RefCell::new(new_case_vla(c, root)?));
        executors.insert("vla".into(), Box::new(SharedVla(v.clone())));
        Some(v)
    };
    let env = model(Id::Envelope, c.value("envelope"))?;
    let plan = model(Id::ActionPlan, c.value("plan"))?;
    let dir = tempdir()?;
    let j = Journal::open(&dir.join("data").to_string_lossy()).map_err(|e| e.to_string())?;
    let result = (|| {
        let pre = super::runcmd::prepare(&plan, &env, crate::pathways::verify::verify)?;
        let mut sup = Supervisor::new(executors, Box::new(Always), Some(&j));
        let s = sup.run(&plan, &env, pre.verification);
        if let Some(e) = sup.log_err.take() {
            return Err(e);
        }
        let rows = j.receipts(&s.id).map_err(|e| e.to_string())?;
        let mut out = Object::new().with("session", Value::Obj(s.json())).with(
            "receipts",
            Value::List(rows.iter().map(receipt_dump).collect()),
        );
        let x = shared.0.borrow();
        out.set("guards", guards(&x.opt));
        out.set("state", robot_state(x.model(), x.data(), c));
        if let Some(v) = &vla {
            if let Some((m, d)) = v.borrow().sim() {
                out.set("vla_state", robot_state(m, d, c));
            }
        }
        Ok(Value::Obj(out))
    })();
    drop(j);
    let _ = std::fs::remove_dir_all(&dir);
    result
}

/// A VLA executor the probe can still read after the supervisor ran it.
struct SharedVla(std::rc::Rc<std::cell::RefCell<Vla>>);

impl Executor for SharedVla {
    fn run(
        &mut self,
        step: &Object,
        timeout_s: u64,
        max_output_bytes: usize,
    ) -> Result<ExecResult, String> {
        self.0.borrow_mut().run(step, timeout_s, max_output_bytes)
    }
}

/// A fresh directory under TMPDIR, removed by the caller.
fn tempdir() -> R<std::path::PathBuf> {
    let base = std::env::temp_dir();
    for i in 0..1000u32 {
        let p = base.join(format!("robot-probe-{}-{i}", std::process::id()));
        if std::fs::create_dir(&p).is_ok() {
            return Ok(p);
        }
    }
    Err("no temporary directory could be made".into())
}

/// Runs the case's steps, then draws its camera: the image's SHA-256, its
/// size and how many colors it holds, or the error.
fn robot_render(c: &Object, root: &str) -> R<Value> {
    let mut x = match robotics::MuJoCo::new(&mjcf_of(c, root), robot_options(c)) {
        Ok(x) => x,
        Err(e) => return Ok(Value::Obj(Object::new().with("made", py_err(&e)))),
    };
    for s in case_steps(c)? {
        if let Err(e) = x.run_step(&s) {
            return Ok(py_err(&e));
        }
    }
    x.data_mut().forward();
    let value_error =
        |m: String| Value::Obj(Object::new().with("error", "ValueError").with("message", m));
    let cam = match c.value("camera").as_str() {
        Some(name) => {
            let id = x.model().name2id(Obj::Camera, name);
            if id < 0 {
                return Ok(value_error(format!(
                    "The camera \"{name}\" does not exist."
                )));
            }
            id
        }
        None => -1,
    };
    let w = Num(c.value("width").clone()).f() as usize;
    let h = Num(c.value("height").clone()).f() as usize;
    let model = x.model().clone();
    let mut r = match Renderer::new(&model, w, h) {
        Ok(r) => r,
        Err(e) => return Ok(value_error(e)),
    };
    let rgb = match r.render(x.data_mut(), cam) {
        Ok(b) => b,
        Err(e) => return Ok(value_error(e)),
    };
    let colors: std::collections::HashSet<&[u8]> = rgb.chunks(3).collect();
    Ok(Value::Obj(
        Object::new()
            .with("sha256", sha256_hex(&rgb))
            .with("width", Value::Int(w.to_string()))
            .with("height", Value::Int(h.to_string()))
            .with("colors", Value::Int(colors.len().to_string())),
    ))
}

/// The SHA-256 of b in hex, through ring, which the tree already holds.
fn sha256_hex(b: &[u8]) -> String {
    let d = ring::digest::digest(&ring::digest::SHA256, b);
    d.as_ref().iter().map(|x| format!("{x:02x}")).collect()
}
