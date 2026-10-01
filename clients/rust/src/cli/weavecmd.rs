//! `daisugi weave PLAN -e ENVELOPE`: a plan tree on the supervisor, with
//! typed slots between steps (`weave.py`). The Go client's
//! `cli/weavecmd.go` is the reference.

use std::cell::RefCell;
use std::collections::{BTreeMap, BTreeSet};
use std::io::Write;
use std::rc::Rc;

use num_bigint::BigInt;

use super::gateroot::{join, path_str};
use super::pathwayscmd::py_int;
use super::runcmd::{prepare, tty};
use super::weaveattempts::{ChoiceAsk, Shared};
use super::{exit, parse_args, Env, Opt, Res};
use crate::delegate::strip_fence;
use crate::distill::params::capability_head_any;
use crate::gate::py::text::{rstrip, splitlines, strip};
use crate::gate::pyjson::{dumps, dumps_indent, loads, loads_py, LoadError, Object, Value};
use crate::gateway::route::{estimate_difficulty, DEFAULT_CHEAP_MODEL, DEFAULT_FRONTIER_MODEL};
use crate::llm::Client;
use crate::orchestrate::executors::{delegate, task_prompt};
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::supervise::{self, now_iso, str_of, DefaultApproval, ExecResult, Executor, Hook, Outcome, Prepared, Supervisor};
use crate::tracejournal::Journal;

const SLOT_TYPES: [&str; 6] = ["path", "string", "number", "list[path]", "list[string]", "list[number]"];
/// `weave.MODEL_KINDS`: step kinds whose output is model text. Their slot
/// values may fill a path or a URL, never a file's content.
const MODEL_KINDS: [&str; 2] = ["task", "agentic"];
const MAX_TEXT: usize = 4096;
const MAX_LIST: usize = 64;

fn jdump(v: &Value) -> String {
    dumps(v, true)
}

fn jstr(s: &str) -> String {
    dumps(&Value::Str(s.to_string()), true)
}

/// `weave.INPUT_FIELDS`.
fn input_fields(kind: &str, field: &str) -> Option<Vec<&'static str>> {
    match (kind, field) {
        ("file_read", "path") | ("file_write", "path") => Some(vec!["path"]),
        ("file_write", "content") => Some(SLOT_TYPES.to_vec()),
        ("network", "url") => Some(vec!["string"]),
        _ => None,
    }
}

fn slot_name_ok(name: &str) -> bool {
    let mut cs = name.chars();
    let Some(first) = cs.next() else { return false };
    name.chars().count() <= 32 && first.is_ascii_lowercase() && cs.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
}

struct SlotRef {
    field: String,
    src: String,
    slot: String,
}

/// `weave.SlotSpec`.
#[derive(Default)]
struct Spec {
    outputs: BTreeMap<String, Object>,
    inputs: BTreeMap<String, Vec<SlotRef>>,
    attempts: BTreeMap<String, usize>,
}

fn list(v: &Value) -> Vec<Value> {
    match v {
        Value::List(l) => l.clone(),
        _ => vec![],
    }
}

/// `weave._ancestors`.
pub(super) fn ancestors(steps: &[Object], sid: &str) -> BTreeSet<String> {
    let mut deps: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for s in steps {
        let d = list(s.value("depends_on")).into_iter().filter_map(|x| x.as_str().map(String::from)).collect();
        deps.insert(str_of(s, "id"), d);
    }
    let mut seen = BTreeSet::new();
    let mut todo: Vec<String> = deps.get(sid).cloned().unwrap_or_default();
    while let Some(d) = todo.pop() {
        if !seen.insert(d.clone()) {
            continue;
        }
        todo.extend(deps.get(&d).cloned().unwrap_or_default());
    }
    seen
}

/// `weave.read_slots`.
fn read_slots(raw: &[Value], steps: &[Object]) -> Result<Spec, String> {
    let mut spec = Spec::default();
    let ids: BTreeSet<String> = steps.iter().map(|s| str_of(s, "id")).collect();
    for (r, step) in raw.iter().zip(steps) {
        let Value::Obj(ro) = r else { continue };
        let (sid, kind) = (str_of(step, "id"), str_of(step, "type"));
        let outs = match ro.get("outputs") {
            None | Some(Value::Null) => continue,
            Some(Value::Obj(o)) => o.clone(),
            Some(_) => return Err(format!("step {sid}: outputs must be an object")),
        };
        if outs.iter().next().is_some() && !["shell", "file_read", "network", "task"].contains(&kind.as_str()) {
            return Err(format!("step {sid}: a {kind} step cannot declare outputs"));
        }
        for (name, typ) in outs.iter() {
            if !slot_name_ok(name) {
                return Err(format!("step {sid}: a slot name is 1 to 32 of a-z 0-9 _, starting with a letter: {}", jstr(name)));
            }
            if !matches!(typ, Value::Str(t) if SLOT_TYPES.contains(&t.as_str())) {
                return Err(format!("step {sid}: slot {name} has type {}; the types are {}", jdump(typ), SLOT_TYPES.join(", ")));
            }
        }
        spec.outputs.insert(sid, outs);
    }
    for (r, step) in raw.iter().zip(steps) {
        let Value::Obj(ro) = r else { continue };
        let (sid, kind) = (str_of(step, "id"), str_of(step, "type"));
        let ins = match ro.get("inputs") {
            None | Some(Value::Null) => continue,
            Some(Value::Obj(o)) => o.clone(),
            Some(_) => return Err(format!("step {sid}: inputs must be an object")),
        };
        let up = ancestors(steps, &sid);
        let mut got = vec![];
        for (fname, rv) in ins.iter() {
            let Some(takes) = input_fields(&kind, fname) else {
                return Err(format!(
                    "step {sid}: a slot cannot fill {kind}.{fname}; a slot fills data only \
                     (a file path, a file's content, a URL), never a command or a prompt"
                ));
            };
            let r = match rv {
                Value::Str(s) if s.contains('.') => s.clone(),
                _ => return Err(format!("step {sid}: input {fname} must name a slot as STEP.SLOT")),
            };
            let dot = r.rfind('.').unwrap_or(0);
            let (src, slot) = (r[..dot].to_string(), r[dot + 1..].to_string());
            if !ids.contains(&src) {
                return Err(format!("step {sid}: input {fname} names no step {}", jstr(&src)));
            }
            if !up.contains(&src) {
                return Err(format!("step {sid}: input {fname} names step {src}, which it does not depend on"));
            }
            let typ = match spec.outputs.get(&src).and_then(|o| o.get(&slot)) {
                Some(Value::Str(t)) => t.clone(),
                _ => return Err(format!("step {sid}: step {src} declares no slot {}", jstr(&slot))),
            };
            if !takes.contains(&typ.as_str()) {
                return Err(format!("step {sid}: {kind}.{fname} takes {}, not {typ} ({r})", takes.join(" or ")));
            }
            let src_kind = steps.iter().find(|s| str_of(s, "id") == src).map(|s| str_of(s, "type")).unwrap_or_default();
            if fname == "content" && MODEL_KINDS.contains(&src_kind.as_str()) {
                return Err(format!(
                    "step {sid}: {kind}.{fname} cannot take {r}: {src} is a {src_kind} step, and model text \
                     never fills a file's content (it may fill a path or a URL)"
                ));
            }
            if fname == "path" || fname == "url" {
                let v = str_of(step, fname);
                if capability_head_any(&kind, &v).is_none() {
                    let what = if fname == "path" { "directory" } else { "host" };
                    return Err(format!("step {sid}: {kind}.{fname} {} has no {what} for a slot to keep", jstr(&v)));
                }
            }
            got.push(SlotRef { field: fname.clone(), src, slot });
        }
        if !got.is_empty() {
            spec.inputs.insert(sid, got);
        }
    }
    spec.attempts = super::weaveattempts::read_attempts(raw, steps)?;
    Ok(spec)
}

/// `weave._check_value`.
fn check_value(typ: &str, v: &Value) -> bool {
    if let Some(inner) = typ.strip_prefix("list[").and_then(|t| t.strip_suffix(']')) {
        return match v {
            Value::List(l) => l.len() <= MAX_LIST && l.iter().all(|x| check_value(inner, x)),
            _ => false,
        };
    }
    if typ == "number" {
        return match v {
            Value::Int(t) => {
                let n: BigInt = t.parse().unwrap_or_default();
                let lim = BigInt::from(1u64 << 53);
                n <= lim && n >= -lim
            }
            Value::Float(f) => f.is_finite(),
            _ => false,
        };
    }
    let Value::Str(s) = v else { return false };
    if s.chars().count() > MAX_TEXT || s.contains('\0') {
        return false;
    }
    if typ == "string" {
        return true;
    }
    s.starts_with('/') && !s.split('/').any(|p| p == "..") && !s.chars().any(|c| (c as u32) < 32 || c as u32 == 127)
}

/// `weave.collect`.
pub(super) fn collect(outputs: &Object, stdout: &str, fence: bool) -> Result<Object, String> {
    let text = if fence { strip_fence(stdout) } else { strip(stdout).to_string() };
    let Ok(Value::Obj(obj)) = loads(&text) else {
        return Err("the output is not one JSON object".into());
    };
    let mut got = Object::new();
    for (name, typ) in outputs.iter() {
        let t = typ.as_str().unwrap_or("");
        let Some(val) = obj.get(name) else {
            return Err(format!("the output has no slot {name}"));
        };
        if !check_value(t, val) {
            return Err(format!("slot {name} is not a {t}"));
        }
        got.set(name, val.clone());
    }
    Ok(got)
}

/// `weave.Prior`.
#[derive(Default)]
struct Prior {
    runs: Vec<String>,
    /// Each step's runs that marked it started, in order.
    started: BTreeMap<String, Vec<String>>,
    done: BTreeMap<String, (String, String)>,
    /// Every (run, step) with a receipt of any kind.
    receipted: BTreeSet<(String, String)>,
}

/// The state file part of `weave.read_prior`.
fn read_marks(path: &str) -> Result<Prior, String> {
    let mut p = Prior::default();
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(p),
        Err(e) => return Err(e.to_string()),
    };
    let text = String::from_utf8(raw).map_err(|_| "the weave state file is not UTF-8".to_string())?;
    for line in splitlines(&text) {
        let o = match loads(line) {
            Ok(Value::Obj(o)) => o,
            Err(LoadError::Unsupported) => return Err("the weave state file holds JSON this binary does not read".into()),
            _ => continue,
        };
        let (Value::Str(run), Value::Str(sid)) = (o.value("run"), o.value("step")) else { continue };
        if !p.runs.contains(run) {
            p.runs.push(run.clone());
        }
        let marked = p.started.entry(sid.clone()).or_default();
        if !marked.contains(run) {
            marked.push(run.clone());
        }
    }
    Ok(p)
}

impl Prior {
    /// `Prior.unreceipted`: the first run that marked `sid` started and
    /// holds no receipt for it.
    fn unreceipted(&self, sid: &str) -> Option<String> {
        self.started
            .get(sid)?
            .iter()
            .find(|run| !self.receipted.contains(&((*run).clone(), sid.to_string())))
            .cloned()
    }

    fn read_receipts(&mut self, j: &Journal) -> Result<(), String> {
        for run in self.runs.clone() {
            for r in j.receipts(&run).map_err(|e| e.to_string())? {
                self.receipted.insert((run.clone(), r.step_id.clone()));
                if r.verify_result && r.evidence.value("status").as_str() == Some("succeeded") {
                    let out = r.evidence.value("stdout").as_str().unwrap_or("").to_string();
                    self.done.insert(r.step_id.clone(), (run.clone(), out));
                }
            }
        }
        Ok(())
    }
}

/// `weave.WeaveHook`.
struct WeaveHook<'s> {
    spec: &'s Spec,
    state: String,
    prior: Option<Prior>,
    rerun: BTreeSet<String>,
    slots: Object,
    skipped: Object,
    filled: Object,
    /// The plan and envelope the run verified, and each step as it runs:
    /// a filled step in its placeholder's place.
    plan: Object,
    env: Object,
    current: BTreeMap<String, Object>,
    /// What the task executor and the ask share with the hook (attempts).
    shared: Rc<RefCell<Shared>>,
}

fn outcome(sid: &str, status: &str, error: Option<String>) -> Outcome {
    Outcome { step_id: sid.into(), status: status.into(), started_at: now_iso(), error, ..Default::default() }
}

impl WeaveHook<'_> {
    fn verdict(&self, step: &Object) -> &'static str {
        let (sid, kind) = (str_of(step, "id"), str_of(step, "type"));
        let Some(p) = &self.prior else { return "run" };
        if kind == "file_read" || kind == "network" {
            return "run";
        }
        if p.done.contains_key(&sid) {
            return "skip";
        }
        if p.unreceipted(&sid).is_some() && kind != "task" && !self.rerun.contains(&sid) {
            return "ask";
        }
        "run"
    }
}

impl Hook for WeaveHook<'_> {
    fn prepare(&mut self, step: &Object) -> Prepared {
        let (sid, kind) = (str_of(step, "id"), str_of(step, "type"));
        match self.verdict(step) {
            "skip" => {
                let (run, stdout) = self.prior.as_ref().and_then(|p| p.done.get(&sid)).cloned().unwrap_or_default();
                if let Some(outs) = self.spec.outputs.get(&sid).filter(|o| o.iter().next().is_some()) {
                    match collect(outs, &stdout, kind == "task") {
                        Ok(got) => {
                            self.slots.set(&sid, Value::Obj(got));
                        }
                        Err(why) => {
                            return Prepared::Stop(
                                outcome(&sid, "aborted", Some(format!("resume: the receipt in {run}: {why}"))),
                                supervise::ABORTED,
                            )
                        }
                    }
                }
                self.skipped.set(&sid, run.as_str());
                self.shared.borrow_mut().open_card_of(&sid);
                return Prepared::Skip(outcome(&sid, "skipped", None));
            }
            "ask" => {
                let run = self.prior.as_ref().and_then(|p| p.unreceipted(&sid)).unwrap_or_default();
                let msg = format!(
                    "resume: step {sid} ({kind}) started in {run} and has no receipt, so it may have run. \
                     Check it, then run again with --rerun {sid}"
                );
                return Prepared::Stop(outcome(&sid, "aborted", Some(msg)), supervise::ABORTED);
            }
            _ => {}
        }
        let Some(ins) = self.spec.inputs.get(&sid).filter(|i| !i.is_empty()) else {
            return Prepared::Run(step.clone());
        };
        let mut update = Object::new();
        for r in ins {
            let value = match self.slots.value(&r.src) {
                Value::Obj(o) => o.get(&r.slot).cloned(),
                _ => None,
            };
            let Some(value) = value else {
                let msg = format!("rejected: slot {}.{} has no value", r.src, r.slot);
                return Prepared::Stop(outcome(&sid, "rejected_halted", Some(msg)), supervise::HALTED);
            };
            if r.field == "content" {
                match &value {
                    Value::Str(s) => update.set(&r.field, s.as_str()),
                    v => update.set(&r.field, jdump(v)),
                };
                continue;
            }
            let s = value.as_str().unwrap_or("").to_string();
            if capability_head_any(&kind, &s) != capability_head_any(&kind, &str_of(step, &r.field)) {
                let what = if r.field == "path" { "directory" } else { "scheme and host" };
                let msg = format!(
                    "rejected: slot {}.{} would change the {what} of {kind}.{}: {}",
                    r.src,
                    r.slot,
                    r.field,
                    jdump(&value)
                );
                return Prepared::Stop(outcome(&sid, "rejected_halted", Some(msg)), supervise::HALTED);
            }
            update.set(&r.field, s.as_str());
        }
        let mut next = step.clone();
        for (k, v) in update.iter() {
            next.set(k, v.clone());
        }
        self.filled.set(&sid, Value::Obj(update));
        Prepared::Run(next)
    }

    /// `WeaveHook.checked`: a filled step that passed its per-step verify
    /// is verified again as part of the whole plan. A Z3 check that does
    /// not finish fails closed (K2-2).
    fn checked(&mut self, step: &Object) -> Option<(Outcome, &'static str)> {
        let sid = str_of(step, "id");
        self.filled.get(&sid)?;
        self.current.insert(sid.clone(), step.clone());
        let steps: Vec<Value> = list(self.plan.value("steps"))
            .into_iter()
            .map(|s| match s.as_obj().and_then(|o| self.current.get(&str_of(o, "id"))) {
                Some(c) => Value::Obj(c.clone()),
                None => s,
            })
            .collect();
        let mut again = self.plan.clone();
        again.set("steps", Value::List(steps));
        let msg = match crate::pathways::verify::verify(&Value::Obj(again), &Value::Obj(self.env.clone()), None, 500) {
            Err(e) => Some(format!("the filled plan does not read: {e}")),
            Ok(r) => match (r.violations.first(), r.timeouts.first()) {
                (Some(v), _) => Some(v.message.clone()),
                (None, Some(t)) => Some(t.clone()),
                (None, None) => None,
            },
        }?;
        Some((outcome(&sid, "rejected_halted", Some(format!("rejected: the filled plan: {msg}"))), supervise::HALTED))
    }

    fn started(&mut self, step: &Object, run_id: &str) -> Option<String> {
        self.shared.borrow_mut().run_id = run_id.to_string();
        {
            let mut sh = self.shared.borrow_mut();
            if !sh.resumed_cards.is_empty() && !sh.data_dir.is_empty() {
                let t = crate::rank::now();
                let rows: Vec<Object> = sh
                    .resumed_cards
                    .iter()
                    .map(|(cid, rid)| {
                        Object::new()
                            .with("choice_id", cid.as_str())
                            .with("ranking_id", rid.as_str())
                            .with("event", "resumed")
                            .with("run_id", run_id)
                            .with("ts", t)
                    })
                    .collect();
                if let Err(e) = crate::rank::append_rows(&sh.data_dir, &rows) {
                    return Some(format!("the resume of the open choice was not recorded: {e}"));
                }
                sh.resumed_cards.clear();
            }
        }
        let line = dumps(&Value::Obj(Object::new().with("run", run_id).with("step", str_of(step, "id").as_str())), true) + "\n";
        let res = (|| -> std::io::Result<()> {
            if let Some(dir) = std::path::Path::new(&self.state).parent() {
                std::fs::create_dir_all(dir)?;
            }
            let mut f = std::fs::OpenOptions::new().create(true).append(true).open(&self.state)?;
            f.write_all(line.as_bytes())?;
            f.sync_all()
        })();
        res.err().map(|e| format!("the start mark was not written: {e}"))
    }

    fn finish(&mut self, step: &Object, mut out: Outcome) -> Outcome {
        let sid = str_of(step, "id");
        let Some(outs) = self.spec.outputs.get(&sid).filter(|o| o.iter().next().is_some()) else { return out };
        if out.status != supervise::SUCCEEDED {
            return out;
        }
        match collect(outs, &out.stdout, str_of(step, "type") == "task") {
            Ok(got) => {
                self.slots.set(&sid, Value::Obj(got));
            }
            Err(why) => {
                out.status = supervise::FAILED.into();
                out.error = Some(format!("slot outputs: {why}"));
            }
        }
        out
    }
}

/// `weave.task_prompt`.
fn weave_task_prompt(step: &Object, outputs: Option<&Object>) -> String {
    match outputs.filter(|o| o.iter().next().is_some()) {
        None => task_prompt(step),
        Some(o) => {
            let keys: Vec<String> = o.iter().map(|(k, t)| format!("{k} ({})", t.as_str().unwrap_or(""))).collect();
            format!(
                "{}\n\nAnswer with one JSON object and nothing else. Its keys and their types: {}. A path is absolute.",
                str_of(step, "prompt"),
                keys.join(", ")
            )
        }
    }
}

/// The task executor weave wires: `DelegatingExecutor` with weave's prompt;
/// a step with attempts asks N times, each prompt naming its attempt, and
/// the answers are ranked (`weave.AttemptsExecutor`).
struct WeaveTask {
    llm: Rc<RefCell<Client>>,
    outputs: BTreeMap<String, Object>,
    shared: Rc<RefCell<Shared>>,
}

impl Executor for WeaveTask {
    fn run(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Result<ExecResult, String> {
        let sid = str_of(step, "id");
        let prompt = weave_task_prompt(step, self.outputs.get(&sid));
        let n = self.shared.borrow().attempts.get(&sid).copied().unwrap_or(0);
        if n == 0 {
            return Ok(delegate(&self.llm, step, &prompt, timeout_s, max_out));
        }
        let results: Vec<ExecResult> = (1..=n)
            .map(|k| delegate(&self.llm, step, &format!("{prompt}\n\nThis is attempt {k} of {n}."), timeout_s, max_out))
            .collect();
        Ok(self.shared.borrow_mut().choose(step, results))
    }
}

impl Env {
    pub(super) fn weave_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "weave";
        let opts = [
            Opt::val(&["--envelope", "-e"], "PATH", "Path to envelope YAML."),
            Opt::val(&["--data-dir"], "PATH", "Root data directory for the journal."),
            Opt::flag(&["--yes", "-y"], "Auto-approve every step (sets DAISUGI_APPROVE=always for this run)."),
            Opt::flag(&["--json"], "Emit the run session as JSON on stdout."),
            Opt::flag(&["--resume"], "Skip steps an earlier run of this plan file finished."),
            Opt { multiple: true, ..Opt::val(&["--rerun"], "TEXT", "On resume, run this started step again (repeatable).") },
            Opt::val(&["--max-parallel"], "INTEGER", "Run up to this many independent steps of a level at once."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "PLAN_PATH", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " PLAN_PATH", "Run the plan tree in PLAN against ENVELOPE, with typed slots between steps.", &opts);
        }
        let max_par: i128 = if p.has("--max-parallel") {
            let raw = p.str("--max-parallel", "");
            match py_int(&raw) {
                Some(n) => n,
                None => {
                    let m = format!("Invalid value for '--max-parallel': {} is not a valid int.", crate::gate::py::text::repr(&raw));
                    return self.usage_args(CMD, "PLAN_PATH", &m);
                }
            }
        } else {
            1
        };
        if p.args.is_empty() {
            return self.usage_args(CMD, "PLAN_PATH", "Missing argument 'PLAN_PATH'.");
        }
        let plan_path = p.args[0].clone();
        if let Some(m) = Self::click_path("'plan_path'", &plan_path) {
            return self.usage_args(CMD, "PLAN_PATH", &m);
        }
        if !p.has("--envelope") {
            return self.usage_args(CMD, "PLAN_PATH", "Missing option '--envelope' / '-e'.");
        }
        let env_path = p.str("--envelope", "");
        if let Some(m) = Self::click_path("'--envelope' / '-e'", &env_path) {
            return self.usage_args(CMD, "PLAN_PATH", &m);
        }
        let data_dir = path_str(&p.str("--data-dir", &join(&self.home, ".opendaisugi")));
        let env = match Self::load_model_yaml(&env_path, Id::Envelope) {
            Ok(o) => o,
            Err(Err(why)) => return self.refuse(CMD, &why),
            Err(Ok(perr)) => {
                return Err(self.fail3(
                    &format!("Tried to read the envelope {env_path}."),
                    &format!("It did not parse: {perr}"),
                    "Fix the file and run again.",
                    2,
                ))
            }
        };
        let raw = match std::fs::read(&plan_path) {
            Ok(r) => r,
            Err(e) => return self.refuse(CMD, &format!("{plan_path} cannot be read: {e}")),
        };
        let Ok(text) = String::from_utf8(raw.clone()) else {
            return self.refuse(CMD, &format!("{plan_path} is not UTF-8"));
        };
        let plan_fail = |e: &mut Env, why: &str| -> Res {
            Err(e.fail3(&format!("Tried to read the plan {plan_path}."), &format!("It did not parse: {why}"), "Fix the file and run again.", 2))
        };
        let obj = match loads_py(&text, 900) {
            Ok(v) => v,
            Err(m) if m == "unsupported" || m.starts_with("maximum recursion") || m.starts_with("Exceeds the limit") => {
                return self.refuse(CMD, &format!("{plan_path} holds JSON this binary does not read"))
            }
            Err(m) => return plan_fail(self, m.split('\n').next().unwrap_or("")),
        };
        let Value::Obj(po) = &obj else {
            return plan_fail(self, "the plan is not a JSON object");
        };
        let mut plan = match validate_model(Id::ActionPlan, &obj, Mode::Python) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => return plan_fail(self, "the plan is not a JSON object"),
            Err(e) => return plan_fail(self, e.text().split('\n').next().unwrap_or("")),
        };
        let steps: Vec<Object> = list(plan.value("steps")).into_iter().filter_map(|s| s.as_obj().cloned()).collect();
        let spec = match read_slots(&list(po.value("steps")), &steps) {
            Ok(s) => s,
            Err(why) => {
                return Err(self.fail3(&format!("Tried to weave the plan {plan_path}."), &why, "Fix the plan and run again.", 2))
            }
        };
        if max_par < 1 {
            self.echo_err("Error: --max-parallel must be 1 or more.\n");
            return exit(2);
        }
        if max_par > 1 {
            return self.refuse(CMD, "--max-parallel above 1 is not in this binary yet (K2-4)");
        }
        let mut models = Object::new();
        let mut routed = vec![];
        for s in &steps {
            if str_of(s, "type") != "task" {
                routed.push(Value::Obj(s.clone()));
                continue;
            }
            let mut s = s.clone();
            if str_of(&s, "preferred_model").is_empty() {
                let model =
                    if estimate_difficulty(&str_of(&s, "prompt")) >= 0.5 { DEFAULT_FRONTIER_MODEL } else { DEFAULT_CHEAP_MODEL };
                s.set("preferred_model", model);
            }
            models.set(&str_of(&s, "id"), s.value("preferred_model").clone());
            routed.push(Value::Obj(s));
        }
        plan.set("steps", Value::List(routed));
        if p.flag("--yes") {
            self.env.insert("DAISUGI_APPROVE".into(), "always".into());
        }
        let hexd = crate::gate::sha256::hexdigest(&raw);
        let digest = format!("sha256:{hexd}");
        let state = join(&join(&data_dir, "weave"), &format!("sha256-{hexd}.jsonl"));
        let mut prior = None;
        if p.flag("--resume") {
            match read_marks(&state) {
                Ok(pr) => prior = Some(pr),
                Err(why) => return self.refuse(CMD, &why),
            }
        }
        let pre = match prepare(&plan, &env, crate::pathways::verify::verify) {
            Ok(p) => p,
            Err(why) => return self.refuse(CMD, &why),
        };
        let fallback = match env.value("fallback") {
            Value::Obj(f) if f.value("strategy").as_str() == Some("tier2_recompute") => {
                Some(supervise::recompute(self.llm_client(), env.clone(), 500))
            }
            _ => None,
        };
        let j = self.open_journal(CMD, &data_dir)?;
        if let Some(pr) = prior.as_mut() {
            if let Err(e) = pr.read_receipts(&j) {
                return self.fail(CMD, &e);
            }
        }
        let rerun: BTreeSet<String> = p.list("--rerun").into_iter().collect();
        let cwd = super::gateroot::getwd().unwrap_or_else(|_| self.home.clone());
        let shared = Rc::new(RefCell::new(Shared {
            data_dir: data_dir.clone(),
            digest: digest.clone(),
            project: cwd.clone(),
            run_id: String::new(),
            choices: Object::new(),
            answered: BTreeSet::new(),
            steps: list(plan.value("steps")).into_iter().filter_map(|s| s.as_obj().cloned()).collect(),
            attempts: spec.attempts.clone(),
            outputs: spec.outputs.clone(),
            resumed_cards: vec![],
        }));
        let mut hook = WeaveHook {
            spec: &spec,
            state: state.clone(),
            prior,
            rerun,
            slots: Object::new(),
            skipped: Object::new(),
            filled: Object::new(),
            plan: plan.clone(),
            env: env.clone(),
            current: BTreeMap::new(),
            shared: shared.clone(),
        };
        let mut executors: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
        for (k, ex) in supervise::default_executors(self.env.clone()) {
            executors.insert(k, ex);
        }
        let llm = Rc::new(RefCell::new(self.llm_client()));
        executors.insert("task".into(), Box::new(WeaveTask { llm, outputs: spec.outputs.clone(), shared: shared.clone() }));
        let me = match self.self_path() {
            Ok(m) => m,
            Err(e) => return self.fail(CMD, &e),
        };
        executors.insert(
            "agentic".into(),
            Box::new(supervise::agentic::Agentic {
                envelope: env.clone(),
                model: "haiku".into(),
                claude: self.llm_client(),
                self_path: me,
                temp_dir: gettempdir(&self.env),
            }),
        );
        let terminal = tty(0) && tty(1);
        if terminal {
            self.flush();
        }
        let approval = Box::new(ChoiceAsk {
            inner: DefaultApproval { env: self.env.clone(), terminal },
            shared: shared.clone(),
            cwd: cwd.clone(),
            terminal,
        });
        let mut sup = Supervisor::new(executors, approval, Some(&j));
        sup.fallback = fallback;
        sup.hook = Some(&mut hook);
        let sess = sup.run(&plan, &env, pre.verification);
        let log_err = sup.log_err.take();
        drop(sup);
        if let Some(e) = log_err {
            return self.fail(CMD, &e);
        }
        if p.flag("--json") {
            let mut o = sess.json();
            o.set(
                "weave",
                Value::Obj(
                    Object::new()
                        .with("plan_hash", digest.as_str())
                        .with("state", state.as_str())
                        .with("models", Value::Obj(models))
                        .with("skipped", Value::Obj(hook.skipped.clone()))
                        .with("filled", Value::Obj(hook.filled.clone()))
                        .with("slots", Value::Obj(hook.slots.clone()))
                        .with("choices", Value::Obj(shared.borrow().choices.clone())),
                ),
            );
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.echo(&format!("Run {} ({})\n", sess.id, sess.status));
            for o in &sess.steps {
                if o.status == "skipped" {
                    let run = hook.skipped.value(&o.step_id).as_str().unwrap_or("").to_string();
                    self.echo(&format!("  {}: skipped (receipt in {run})\n", o.step_id));
                    continue;
                }
                let rc = o.rc.map(|n| n.to_string()).unwrap_or_else(|| "None".into());
                let by = o.approved_by.clone().unwrap_or_else(|| "None".into());
                self.echo(&format!("  {}: {} (rc={rc}, approved_by={by}, {:.1} ms)\n", o.step_id, o.status, o.duration_ms));
                if let (Some(ins), Value::Obj(filled)) = (spec.inputs.get(&o.step_id), hook.filled.value(&o.step_id)) {
                    for r in ins {
                        if filled.get(&r.field).is_some() {
                            self.echo(&format!("      filled {} from {}.{}\n", r.field, r.src, r.slot));
                        }
                    }
                }
                let choice = shared.borrow().choices.value(&o.step_id).as_obj().cloned();
                if let Some(ch) = choice.filter(|c| !c.value("chosen").is_null()) {
                    let card = match ch.value("choice_id").as_str() {
                        Some(id) => format!("; card {id}"),
                        None => "; no card".into(),
                    };
                    self.echo(&format!(
                        "      attempts: chose {} ({}){card}\n",
                        ch.value("chosen").as_str().unwrap_or(""),
                        ch.value("status").as_str().unwrap_or("")
                    ));
                }
                if let Some(e) = o.error.as_ref().filter(|e| !e.is_empty()) {
                    self.echo(&format!("      error: {e}\n"));
                }
                if !o.stdout.is_empty() {
                    for ln in splitlines(rstrip(&o.stdout)).into_iter().take(5) {
                        self.echo(&format!("      {ln}\n"));
                    }
                }
            }
            if let Some(t) = &sess.trace_id {
                self.echo(&format!("Journal: {t}\n"));
            }
        }
        match sess.status.as_str() {
            supervise::SUCCEEDED => Ok(()),
            supervise::REJECTED => {
                let vs = list(sess.verification.value("violations"));
                let field = |v: &Value, k: &str| v.as_obj().map(|o| o.value(k).as_str().unwrap_or("").to_string()).unwrap_or_default();
                for v in &vs {
                    self.echo_err(&format!("  {}: {}\n", field(v, "stage"), field(v, "message")));
                }
                let first = match vs.first() {
                    Some(v) => format!("[{}] {}", field(v, "stage"), field(v, "message")),
                    None => "Verification rejected the plan.".into(),
                };
                Err(self.fail3(
                    &format!("Tried to weave the plan {plan_path}."),
                    &first,
                    "Edit the plan or widen the envelope; see the violation above.",
                    2,
                ))
            }
            supervise::ABORTED => exit(130),
            _ => exit(1),
        }
    }
}

/// `tempfile.gettempdir()`: the first of $TMPDIR, $TEMP, $TMP, /tmp,
/// /var/tmp and /usr/tmp that is a directory a file can be made in, made
/// absolute.
fn gettempdir(env: &std::collections::HashMap<String, String>) -> String {
    let mut dirs: Vec<String> =
        ["TMPDIR", "TEMP", "TMP"].iter().filter_map(|k| env.get(*k).filter(|v| !v.is_empty()).cloned()).collect();
    dirs.extend(["/tmp", "/var/tmp", "/usr/tmp"].iter().map(|s| s.to_string()));
    for d in dirs {
        let abs = if d.starts_with('/') {
            d
        } else {
            match std::env::current_dir() {
                Ok(c) => format!("{}/{d}", c.display()),
                Err(_) => continue,
            }
        };
        let probe = format!("{abs}/.daisugi-probe-{}", std::process::id());
        if std::fs::OpenOptions::new().write(true).create_new(true).open(&probe).is_ok() {
            let _ = std::fs::remove_file(&probe);
            return abs;
        }
    }
    "/tmp".into()
}
