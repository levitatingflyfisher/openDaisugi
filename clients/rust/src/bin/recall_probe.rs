//! A test instrument for clients/gateway_compare.py: it runs the recall
//! library on one query per input line and prints the result the way the
//! oracle's MCP tools return it. A line is {"kind": "recall", "db",
//! "matcher", "task", "envelope", "z3_timeout_ms"} or {"kind": "answer",
//! "answers", "matcher", "task", "now", "max_age_seconds", "ground_hash"}.

use std::collections::HashMap;
use std::io::{BufRead, Write};

use daisugi_verify::distill::load_embedder;
use daisugi_verify::gate::pyjson::{self, loads, Object, Value};
use daisugi_verify::gateway::answers::load_answers;
use daisugi_verify::gateway::recall::{recall, recall_answer, DEFAULT_MAX_AGE, DEFAULT_MODEL};
use daisugi_verify::pathways::find::select_matcher;
use daisugi_verify::pathways::potion;
use daisugi_verify::pathways::store::Store;

fn die(why: &str) -> ! {
    eprintln!("{why}");
    std::process::exit(1)
}

fn reason(o: &mut Object, r: Option<&str>) {
    o.set("reason", r.map(|s| Value::Str(s.into())).unwrap_or(Value::Null));
}

fn f(v: &Value) -> Option<f64> {
    match v {
        Value::Float(x) => Some(*x),
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

fn main() {
    let vars: HashMap<String, String> = std::env::vars().collect();
    let home = vars.get("HOME").cloned().unwrap_or_default();
    // The model client that binds a typed pathway's holes, over this
    // process's environment, as the oracle's recall makes one.
    let mut lc = daisugi_verify::llm::Client::new(vars.clone(), &home);
    let pe = potion::Env::from_process(vars, home);
    let stdin = std::io::stdin();
    let mut out = std::io::BufWriter::new(std::io::stdout());
    for line in stdin.lock().lines() {
        let line = line.unwrap_or_else(|e| die(&e.to_string()));
        if line.trim().is_empty() {
            continue;
        }
        let Ok(Value::Obj(q)) = loads(&line) else { die("a query is not an object") };
        let s = |k: &str| q.value(k).as_str().unwrap_or_default().to_string();
        let m = match select_matcher(&s("matcher"), &pe) {
            Ok(Some(m)) => m,
            _ => die(&format!("matcher: {}", s("matcher"))),
        };
        let mut o = Object::new();
        match s("kind").as_str() {
            "recall" => {
                let store = Store::open_read_only(&s("db")).unwrap_or_else(|e| die(&format!("{e:?}")));
                let z3: u32 = match q.value("z3_timeout_ms") {
                    Value::Int(t) => t.parse().unwrap_or(500),
                    _ => 500,
                };
                match recall(&store, &s("matcher"), &pe, &s("task"), q.value("envelope"), z3, Some(&mut lc), DEFAULT_MODEL) {
                    Err(e) => {
                        o.set("error", e.as_str());
                    }
                    Ok(r) => {
                        o.set("hit", r.hit);
                        reason(&mut o, r.reason);
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
                    }
                }
            }
            "answer" => {
                let result = load_answers(&s("answers")).map_err(|e| e.to_string()).and_then(|entries| {
                    let emb = load_embedder(&m, &pe)?;
                    let max_age = f(q.value("max_age_seconds")).unwrap_or(DEFAULT_MAX_AGE);
                    let now = f(q.value("now")).unwrap_or(0.0);
                    let ground = q.value("ground_hash").as_str().map(String::from);
                    recall_answer(&s("task"), &entries, now, &emb, m.threshold(), max_age, ground.as_deref())
                });
                match result {
                    Err(e) => {
                        o.set("error", e.as_str());
                    }
                    Ok(r) => {
                        o.set("hit", r.hit);
                        reason(&mut o, r.reason);
                        o.set("answer", r.answer);
                        match r.provenance {
                            Some(p) => {
                                let mut x = Object::new();
                                x.set("similarity", p.similarity);
                                x.set("age_seconds", p.age_seconds);
                                x.set("created_at", p.created_at);
                                x.set("ground_hash", p.ground_hash);
                                o.set("provenance", Value::Obj(x));
                            }
                            None => {
                                o.set("provenance", Value::Null);
                            }
                        }
                    }
                }
            }
            other => die(&format!("unknown kind {other}")),
        }
        let _ = writeln!(out, "{}", pyjson::dumps(&Value::Obj(o), true));
        let _ = out.flush();
    }
}
