//! A test instrument for clients/k1_compare.py: it runs one query of the
//! `envgen` library (or recall's bind path) and prints the result as one
//! JSON line, the way the oracle's script in clients/k1_cases.py prints
//! its own. The query is argv[1]; its "kind" is generate, inherit, bind,
//! compose or recall. A path this binary does not take prints
//! {"unported": reason}. It is not shipped.

use std::collections::{BTreeMap, HashMap};

use daisugi_verify::envgen::bind::bind;
use daisugi_verify::envgen::cache::Cache;
use daisugi_verify::envgen::compose::{contract_envelopes, handlers_for, run_skill, Executor};
use daisugi_verify::envgen::inherit::verify_inheritance;
use daisugi_verify::envgen::tier1::{load_configured, Tier1};
use daisugi_verify::envgen::{generate, GenErr, Options, PyError, DEFAULT_MODEL};
use daisugi_verify::gate::pyjson::{self, loads, Object, Value};
use daisugi_verify::gateway::recall::recall;
use daisugi_verify::llm::Client;
use daisugi_verify::pathways::pmodel::{take_model, Id, Mode};
use daisugi_verify::pathways::potion;
use daisugi_verify::pathways::store::Store;
use daisugi_verify::pathways::verify::{check_envelope, verify};
use daisugi_verify::tracejournal::Journal;

fn die(why: &str) -> ! {
    eprintln!("envelope-probe: {why}");
    std::process::exit(1)
}

/// `Envelope.model_validate(d)`, as the oracle's script reads its inputs.
fn envelope(v: &Value) -> Object {
    match take_model(Id::Envelope, v.clone(), Mode::Python) {
        Ok(Value::Obj(o)) => o,
        Ok(_) => die("Envelope: not an object"),
        Err(e) => die(&format!("Envelope: {}", e.text())),
    }
}

fn silent_potion(vars: &HashMap<String, String>, home: &str) -> potion::Env {
    let mut pe = potion::Env::from_process(vars.clone(), home.to_string());
    pe.notice = Box::new(|_| {});
    pe
}

fn matcher_key(home: &str) -> String {
    daisugi_verify::cli::config::load(&format!("{home}/.opendaisugi/config.yaml"))
        .map(|c| c.matcher_model)
        .unwrap_or_else(|e| die(&format!("the config file: {e}")))
}

fn py_err(e: &PyError) -> Object {
    let mut o = Object::new();
    o.set("error", e.class.as_str());
    o.set("message", e.msg.as_str());
    o
}

/// A generate error as the oracle's script prints it; None for one this
/// binary stops on.
fn gen_err(e: GenErr) -> Result<Object, String> {
    match e {
        GenErr::Py(p) => Ok(py_err(&p)),
        GenErr::Inherit(ie) => Ok(py_err(&PyError::new("EnvelopeInheritanceError", ie.to_string()))),
        GenErr::CacheRow(_) => Ok(py_err(&PyError::new("ValidationError", ""))),
        GenErr::Unported(why) => {
            let mut o = Object::new();
            o.set("unported", format!("{why} is not in this binary yet"));
            Ok(o)
        }
        GenErr::Other(why) => Err(why),
    }
}

fn int(v: &Value) -> Option<i64> {
    match v {
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

fn gen(q: &Object, c: &mut Client, vars: &HashMap<String, String>, home: &str) -> Object {
    let s = |k: &str| q.value(k).as_str().map(String::from);
    let mut o = Options { task: s("task").unwrap_or_default(), context: s("context"), summarize: q.value("summarize").truthy(), ..Default::default() };
    if let Some(v) = s("stakes").filter(|v| !v.is_empty()) {
        o.stakes = v;
    }
    if let Some(v) = s("thinking").filter(|v| !v.is_empty()) {
        o.thinking = v;
    }
    if let Some(n) = int(q.value("max_retries")) {
        o.max_retries = n.max(0) as usize;
    }
    if let Some(n) = int(q.value("max_task_chars")) {
        o.max_task_chars = n.max(0) as usize;
    }
    match q.value("model") {
        Value::Null => {}
        Value::Str(m) => o.models = vec![m.clone()],
        Value::List(l) => {
            o.models = l.iter().map(|x| x.as_str().unwrap_or_default().to_string()).collect();
            o.single = false;
        }
        _ => die("model: neither a string nor a list"),
    }
    if let Some(t) = match q.value("threshold") {
        Value::Float(f) => Some(*f),
        Value::Int(t) => t.parse().ok(),
        _ => None,
    } {
        o.threshold = Some(t);
    }
    if !q.value("low_stakes").is_null() {
        o.low_stakes = Some(envelope(q.value("low_stakes")));
    }
    if !q.value("parent").is_null() {
        o.parent = Some(envelope(q.value("parent")));
    }
    if let Some(p) = s("cache") {
        o.cache = Some(Cache::open(&p).unwrap_or_else(|e| die(&format!("the cache: {e:?}"))));
    }
    let store;
    let pe = silent_potion(vars, home);
    if let Some(p) = s("pathways") {
        store = Store::open(&p).unwrap_or_else(|e| die(&format!("the store: {e}")));
        o.store = Some(&store);
        o.matcher_key = matcher_key(home);
        o.potion = Some(&pe);
    }
    let journal;
    if let Some(d) = s("journal") {
        journal = Journal::open(&d).unwrap_or_else(|e| die(&format!("the journal: {e}")));
        o.journal = Some(&journal);
    }
    match q.value("tier1") {
        Value::Null => {}
        Value::Str(dir) => match load_configured(dir) {
            Ok(t) => o.tier1 = t,
            Err(e) => {
                let mut out = gen_err(e).unwrap_or_else(|w| die(&w));
                if !out.get("unported").is_some() {
                    out.set("warnings", Value::List(vec![]));
                }
                return out;
            }
        },
        Value::Obj(t) => {
            let ts = |k: &str| t.value(k).as_str().map(String::from);
            let mut base = ts("base_url");
            if let Some(var) = ts("base_url_env").filter(|v| !v.is_empty()) {
                // The fake server's address, which only the environment knows.
                base = Some(vars.get(&var).cloned().unwrap_or_default());
            }
            o.tier1 = Some(Tier1::new(&ts("model").unwrap_or_default(), base, ts("api_key"), &ts("name").unwrap_or_default()));
        }
        _ => die("tier1: neither a directory nor a provider"),
    }
    let r = generate(&o, c);
    let warnings: Vec<Value> = if r.find_warning.is_empty() { vec![] } else { vec![Value::Str(r.find_warning)] };
    match r.envelope {
        Ok(env) => {
            let mut out = Object::new();
            out.set("envelope", Value::Obj(env));
            out.set("warnings", Value::List(warnings));
            out
        }
        Err(e) => {
            let mut out = gen_err(e).unwrap_or_else(|w| die(&w));
            if !out.get("unported").is_some() {
                out.set("warnings", Value::List(warnings));
            }
            out
        }
    }
}

fn inherit(q: &Object) -> Object {
    let child = envelope(q.value("child"));
    let parent = envelope(q.value("parent"));
    let vs: Vec<Value> = verify_inheritance(&child, &parent).into_iter().map(Value::Str).collect();
    let mut o = Object::new();
    o.set("violations", Value::List(vs));
    o
}

fn z3(q: &Object) -> u32 {
    int(q.value("z3_timeout_ms")).unwrap_or(500).clamp(0, u32::MAX as i64) as u32
}

fn bind_q(q: &Object, c: &mut Client) -> Object {
    let db = q.value("db").as_str().unwrap_or_default();
    let id = q.value("id").as_str().unwrap_or_default();
    let store = Store::open_read_only(db).unwrap_or_else(|e| die(&format!("the store: {e}")));
    let p = match store.get_pathway(id) {
        Ok(Some(p)) => p,
        Ok(None) => die(&format!("no pathway {id}")),
        Err(e) => die(&format!("the pathway {id}: {e}")),
    };
    check_envelope(q.value("envelope")).unwrap_or_else(|e| die(&format!("the envelope: {e}")));
    let model = q.value("model").as_str().unwrap_or(DEFAULT_MODEL).to_string();
    let plan = bind(Some(c), &p.obj, q.value("task").as_str().unwrap_or_default(), q.value("envelope"), &model, z3(q));
    let mut o = Object::new();
    o.set("plan", Value::Obj(plan));
    o
}

/// The probe's executor: "<type>:<the step's capability field>".
struct Echo;

impl Executor for Echo {
    fn run(&self, step: &Object, _: u64, _: usize) -> Result<String, PyError> {
        let t = step.value("type").as_str().unwrap_or_default();
        let f = match t {
            "shell" => "command",
            "file_read" | "file_write" => "path",
            "network" => "url",
            _ => "",
        };
        Ok(format!("{t}:{}", step.value(f).as_str().unwrap_or_default()))
    }
}

fn keys<V>(m: &BTreeMap<String, V>) -> Value {
    Value::List(m.keys().map(|k| Value::Str(k.clone())).collect())
}

fn compose(q: &Object) -> Object {
    let store = Store::open_read_only(q.value("db").as_str().unwrap_or_default()).unwrap_or_else(|e| die(&format!("the store: {e}")));
    let contracts = contract_envelopes(&store).unwrap_or_else(|e| die(&e.to_string()));
    let mut executors: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
    for t in daisugi_verify::distill::str_list(q.value("executors")) {
        executors.insert(t, Box::new(Echo));
    }
    let ids = daisugi_verify::distill::str_list(q.value("skill_ids"));
    let handlers = handlers_for(&store, &ids).unwrap_or_else(|e| die(&e.to_string()));
    let mut runs = Object::new();
    for (id, p) in &handlers {
        match run_skill(&p.obj, &executors) {
            Ok(out) => runs.set(id, out),
            Err(e) => runs.set(id, Value::Obj(py_err(&e))),
        };
    }
    // The plan, each skill step carrying its pathway's contract.
    let mut plan = q.value("plan").clone();
    if let Value::Obj(po) = &mut plan {
        if let Some(Value::List(steps)) = po.get_mut("steps") {
            for st in steps.iter_mut() {
                if let Value::Obj(so) = st {
                    if so.value("type").as_str() == Some("skill") {
                        if let Some(env) = so.value("skill_id").as_str().and_then(|id| contracts.get(id)) {
                            so.set("contract_envelope", env.clone());
                        }
                    }
                }
            }
        }
    }
    let plan = match take_model(Id::ActionPlan, plan, Mode::Python) {
        Ok(p) => p,
        Err(e) => die(&format!("ActionPlan: {}", e.text())),
    };
    check_envelope(q.value("envelope")).unwrap_or_else(|e| die(&format!("the envelope: {e}")));
    let res = verify(&plan, q.value("envelope"), None, z3(q)).unwrap_or_else(|e| die(&format!("verify: {e}")));
    let vs: Vec<Value> = res
        .violations
        .iter()
        .map(|v| {
            // A refused delegation is worded by each side (PW-12).
            let msg = if v.stage == "delegation" { Value::Null } else { Value::Str(v.message.clone()) };
            Value::List(vec![Value::Str(v.stage.to_string()), msg])
        })
        .collect();
    let mut o = Object::new();
    o.set("contracts", keys(&contracts));
    o.set("handlers", keys(&handlers));
    o.set("runs", Value::Obj(runs));
    o.set("ok", res.violations.is_empty() && res.timeouts.is_empty());
    o.set("violations", Value::List(vs));
    o
}

fn recall_q(q: &Object, c: &mut Client, vars: &HashMap<String, String>, home: &str) -> Object {
    let key = matcher_key(home);
    let store = Store::open_read_only(q.value("db").as_str().unwrap_or_default()).unwrap_or_else(|e| die(&format!("the store: {e}")));
    let pe = silent_potion(vars, home);
    let task = q.value("task").as_str().unwrap_or_default();
    let r = recall(&store, &key, &pe, task, q.value("envelope"), z3(q), Some(c), DEFAULT_MODEL).unwrap_or_else(|e| die(&e));
    let mut o = Object::new();
    o.set("hit", r.hit);
    o.set("reason", r.reason.map(Value::from).unwrap_or(Value::Null));
    o.set("plan", r.plan);
    match r.provenance {
        Some(p) => {
            let mut x = Object::new();
            x.set("pathway_id", p.pathway_id.as_str());
            x.set("similarity", p.similarity);
            x.set("tier", p.tier);
            x.set("source_trace_count", p.source_trace_count as i64);
            x.set("distilled_at", p.distilled_at);
            x.set("hit_count", p.hit_count);
            o.set("provenance", Value::Obj(x));
        }
        None => {
            o.set("provenance", Value::Null);
        }
    }
    o
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 2 {
        eprintln!("usage: envelope-probe QUERY-JSON");
        std::process::exit(2);
    }
    let Ok(Value::Obj(q)) = loads(&args[1]) else {
        eprintln!("envelope-probe: the query is not a JSON object");
        std::process::exit(2);
    };
    let vars: HashMap<String, String> = std::env::vars().collect();
    let home = vars.get("HOME").cloned().unwrap_or_default();
    let mut c = Client::new(vars.clone(), &home);
    let out = match q.value("kind").as_str().unwrap_or_default() {
        "generate" => gen(&q, &mut c, &vars, &home),
        "inherit" => inherit(&q),
        "bind" => bind_q(&q, &mut c),
        "compose" => compose(&q),
        "recall" => recall_q(&q, &mut c, &vars, &home),
        other => die(&format!("unknown kind {other:?}")),
    };
    println!("{}", pyjson::dumps(&Value::Obj(out), true));
}
