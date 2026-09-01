//! weave's attempts (`weave.py`): a task step with `attempts: N` asks the
//! model N times, ranks the answers with `rank`, goes on with the leader
//! and opens a card; a step that cannot be undone below an open choice goes
//! to the terminal ask. The Go client's `cli/weaveattempts.go` is the
//! reference.

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::io::{BufRead, Write};

use num_bigint::BigInt;
use regex::Regex;

use super::weavecmd::{ancestors, collect};
use crate::gate::py::text::{lower, repr, strip};
use crate::gate::pyjson::{Object, Value};
use crate::rank;
use crate::supervise::{str_of, Approver, Decision, DefaultApproval, ExecResult};

const MIN_ATTEMPTS: i64 = 2;
const MAX_ATTEMPTS: i64 = 8;

/// The attempts half of `weave.read_slots`.
pub(super) fn read_attempts(raw: &[Value], steps: &[Object]) -> Result<BTreeMap<String, usize>, String> {
    let id_re = Regex::new(r"^[A-Za-z0-9._-]{1,29}$").unwrap();
    let mut out = BTreeMap::new();
    for (r, step) in raw.iter().zip(steps) {
        let Value::Obj(ro) = r else { continue };
        let n = ro.value("attempts");
        if n.is_null() {
            continue;
        }
        let sid = str_of(step, "id");
        if str_of(step, "type") != "task" {
            return Err(format!("step {sid}: only a task step runs attempts"));
        }
        let v = match n {
            Value::Int(t) => t.parse::<BigInt>().ok().filter(|b| *b >= BigInt::from(MIN_ATTEMPTS) && *b <= BigInt::from(MAX_ATTEMPTS)),
            _ => None,
        };
        let Some(v) = v else {
            return Err(format!("step {sid}: attempts must be a whole number from {MIN_ATTEMPTS} to {MAX_ATTEMPTS}"));
        };
        if !id_re.is_match(&sid) {
            return Err(format!("step {sid}: a step with attempts needs an id of 1 to 29 of A-Z a-z 0-9 . _ -"));
        }
        out.insert(sid, v.to_string().parse().unwrap_or(0));
    }
    Ok(out)
}

/// What the hook, the task executor and the ask share.
pub(super) struct Shared {
    pub data_dir: String,
    pub digest: String,
    pub project: String,
    pub run_id: String,
    pub choices: Object,
    pub answered: BTreeSet<String>,
    pub steps: Vec<Object>,
    pub attempts: BTreeMap<String, usize>,
    pub outputs: BTreeMap<String, Object>,
}

fn choice(cid: Value, chosen: Value, status: Value, recorded: bool, ranking: Value) -> Value {
    let mut o = Object::new();
    o.set("choice_id", cid).set("chosen", chosen).set("status", status).set("recorded", recorded).set("ranking", ranking);
    Value::Obj(o)
}

impl Shared {
    pub(super) fn ranking_id(&self, sid: &str) -> String {
        let d = self.digest.rsplit(':').next().unwrap_or("");
        format!("weave:{}:{sid}", &d[..d.len().min(16)])
    }

    /// `WeaveHook.below`: every step that depends on sid, in id order.
    fn below(&self, sid: &str) -> Vec<Value> {
        let mut ids: Vec<String> =
            self.steps.iter().map(|s| str_of(s, "id")).filter(|id| ancestors(&self.steps, id).contains(sid)).collect();
        ids.sort();
        ids.into_iter().map(Value::from).collect()
    }

    /// `WeaveHook.choose`.
    pub(super) fn choose(&mut self, step: &Object, results: Vec<ExecResult>) -> ExecResult {
        let sid = str_of(step, "id");
        let outs = self.outputs.get(&sid).filter(|o| !o.is_empty()).cloned();
        let mut atts = vec![];
        for (k, res) in results.iter().enumerate() {
            let pass = |ok: bool| if ok { "pass" } else { "fail" };
            let mut tests = vec![Value::Obj(
                Object::new().with("name", "the call answered").with("required", true).with("result", pass(res.rc == 0)),
            )];
            if let Some(o) = &outs {
                let ok = res.rc == 0 && collect(o, &res.stdout, true).is_ok();
                tests.push(Value::Obj(
                    Object::new().with("name", "the slot outputs read").with("required", true).with("result", pass(ok)),
                ));
            }
            let hash = crate::gate::sha256::hexdigest(res.stdout.as_bytes());
            let mut a = Object::new();
            a.set("id", format!("{sid}#{}", k + 1)).set("content_hash", format!("sha256:{hash}"));
            a.set("author", res.model.clone().unwrap_or_default()).set("tests", Value::List(tests));
            a.set("where", Object::new().with("kind", "output").with("text", res.stdout.as_str()));
            atts.push(Value::Obj(a));
        }
        let mut doc = Object::new();
        doc.set("ranking_id", self.ranking_id(&sid)).set("task", str_of(step, "prompt")).set("project", self.project.as_str());
        doc.set("attempts", Value::List(atts)).set("comparisons", Value::List(vec![]));
        let fail = |text: String, first: &ExecResult| ExecResult {
            rc: 1,
            stdout: text,
            duration_ms: first.duration_ms,
            model: first.model.clone(),
            ..Default::default()
        };
        let r = match rank::parse(&Value::Obj(doc)) {
            Ok(r) => r,
            Err(e) => return fail(format!("attempts: {e}"), &results[0]),
        };
        let mut warnings = vec![];
        let by_id: HashMap<String, &rank::Attempt> = r.attempts.iter().map(|a| (a.id.clone(), a)).collect();
        let owner = rank::owner_answers(&self.data_dir, &r.id, &by_id, &mut warnings);
        let res = rank::fit(&r, vec![], owner, warnings);
        if res.value("status").as_str() == Some("none_survived") {
            let mut parts = vec![];
            if let Value::List(el) = res.value("eliminated") {
                for e in el {
                    let eo = e.as_obj().cloned().unwrap_or_default();
                    let rs: Vec<String> = match eo.value("reasons") {
                        Value::List(l) => l.iter().map(|x| x.as_str().unwrap_or("").to_string()).collect(),
                        _ => vec![],
                    };
                    parts.push(format!("{}: {}", eo.value("id").as_str().unwrap_or(""), rs.join(", ")));
                }
            }
            self.choices.set(&sid, choice(Value::Null, Value::Null, "none_survived".into(), false, Value::Obj(res)));
            return fail(format!("attempts: none survived: {}", parts.join("; ")), &results[0]);
        }
        let t = rank::now();
        let mut rows = rank::sweep(&self.data_dir, t);
        let mut cid = Value::Null;
        let mut recorded = false;
        if matches!(res.value("order"), Value::List(l) if l.len() > 1) {
            let mut run = Object::new();
            run.set("run_id", self.run_id.as_str()).set("step", sid.as_str()).set("downstream", Value::List(self.below(&sid)));
            let row = rank::opened_row(&r, &res, t, Some(run));
            let id = row.value("choice_id").as_str().unwrap_or("").to_string();
            if !rank::read_cards(&self.data_dir).iter().any(|c| c.id() == id) {
                rows.push(row);
                recorded = true;
            }
            cid = Value::from(id);
        }
        if let Err(e) = rank::append_rows(&self.data_dir, &rows) {
            return fail(format!("attempts: the card was not written: {e}"), &results[0]);
        }
        let leader = res.value("leader").as_str().unwrap_or("").to_string();
        let status = res.value("status").clone();
        self.choices.set(&sid, choice(cid, leader.as_str().into(), status, recorded, Value::Obj(res)));
        let k: usize = leader.rsplit('#').next().and_then(|x| x.parse().ok()).unwrap_or(1);
        results.into_iter().nth(k - 1).unwrap_or_default()
    }

    /// `WeaveHook._open_card_of`.
    pub(super) fn open_card_of(&mut self, sid: &str) {
        if !self.attempts.contains_key(sid) {
            return;
        }
        let rid = self.ranking_id(sid);
        let t = rank::now();
        for card in rank::read_cards(&self.data_dir).iter().rev() {
            if card.opened.value("ranking_id").as_str() != Some(rid.as_str()) {
                continue;
            }
            if card.close.is_none() && card.answer.is_none() && rank::decay(card, &self.data_dir, t).is_none() {
                let c = choice(
                    card.id().into(),
                    card.opened.value("chosen").clone(),
                    card.opened.value("status").clone(),
                    false,
                    Value::Null,
                );
                self.choices.set(sid, c);
            }
            return;
        }
    }

    /// `WeaveHook.open_choice_above`.
    fn open_choice_above(&self, step: &Object) -> Option<String> {
        ancestors(&self.steps, &str_of(step, "id")).into_iter().find(|a| {
            !self.answered.contains(a)
                && self.choices.value(a).as_obj().is_some_and(|c| !c.value("choice_id").is_null())
        })
    }
}

/// `os.path.normpath` on POSIX.
fn normpath(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let initial = if p.starts_with("//") && !p.starts_with("///") {
        2
    } else if p.starts_with('/') {
        1
    } else {
        0
    };
    let mut comps: Vec<&str> = vec![];
    for c in p.split('/') {
        if c.is_empty() || c == "." {
            continue;
        }
        if c != ".." || (initial == 0 && comps.is_empty()) || comps.last() == Some(&"..") {
            comps.push(c);
        } else if !comps.is_empty() {
            comps.pop();
        }
    }
    let out = format!("{}{}", "/".repeat(initial), comps.join("/"));
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

/// `weave.step_tier`.
pub(super) fn step_tier(step: &Object, cwd: &str) -> &'static str {
    match str_of(step, "type").as_str() {
        "file_read" | "network" | "task" => return "undoable",
        "file_write" => {
            let root = normpath(cwd);
            let path = str_of(step, "path");
            let path = if path.starts_with('/') { path } else { format!("{root}/{path}") };
            let p = normpath(&path);
            if p == root || p.starts_with(&format!("{}/", root.trim_end_matches('/'))) {
                return "undoable";
            }
        }
        _ => {}
    }
    "permanent"
}

/// `weave.ChoiceAsk`: in front of the default approval, a step that cannot
/// be undone below an open choice goes to the terminal ask.
pub(super) struct ChoiceAsk {
    pub inner: DefaultApproval,
    pub shared: std::rc::Rc<std::cell::RefCell<Shared>>,
    pub cwd: String,
    pub terminal: bool,
}

impl Approver for ChoiceAsk {
    fn decide(&mut self, step: &Object, env: &Object) -> Result<Decision, String> {
        let src = self.shared.borrow().open_choice_above(step);
        let Some(src) = src.filter(|_| step_tier(step, &self.cwd) != "undoable") else {
            return self.inner.decide(step, env);
        };
        let (cid, chosen) = {
            let sh = self.shared.borrow();
            let c = sh.choices.value(&src).as_obj().cloned().unwrap_or_default();
            (c.value("choice_id").as_str().unwrap_or("").to_string(), c.value("chosen").as_str().unwrap_or("").to_string())
        };
        if !self.terminal {
            return Ok(Decision {
                approved: false,
                approved_by: "denied".into(),
                reason: format!(
                    "step {} cannot be undone and the choice {cid} on step {src} is open; answer it in a terminal, \
                     or review it with daisugi rank queue and run again",
                    str_of(step, "id")
                ),
            });
        }
        let mut what = str_of(step, "command");
        for k in ["path", "url", "id"] {
            if what.is_empty() {
                what = str_of(step, k);
            }
        }
        let mut out = std::io::stdout();
        let _ = writeln!(out, "Step {src} kept {chosen} of its attempts (card {cid}).");
        let _ = write!(out, "Approve step {} ({what})? [y/N] ", repr(&str_of(step, "id")));
        let _ = out.flush();
        let mut line = String::new();
        let n = std::io::stdin().lock().read_line(&mut line).unwrap_or(0);
        if n == 0 {
            return Err("EOFError: EOF when reading a line".into());
        }
        let answer = lower(strip(line.strip_suffix('\n').unwrap_or(&line)));
        let ok = answer == "y" || answer == "yes";
        if ok {
            let mut sh = self.shared.borrow_mut();
            let mut row = Object::new();
            row.set("choice_id", cid.as_str()).set("ranking_id", sh.ranking_id(&src)).set("event", "confirmed");
            row.set("how", "permanent_ask").set("ts", rank::now());
            let _ = rank::append_rows(&sh.data_dir, &[row]);
            sh.answered.insert(src.clone());
        }
        Ok(Decision { approved: ok, approved_by: "tty".into(), reason: format!("user answered {}", repr(&answer)) })
    }
}
