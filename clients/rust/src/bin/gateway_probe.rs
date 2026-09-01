//! A test instrument for clients/gateway_compare.py --fuzz: it runs turn
//! sequences through the gateway pipeline in process, the way the proxy
//! runs each turn, and prints what the oracle's fuzz driver prints. Each
//! input line is one sequence: a gateway config and its turns (the request
//! body, the usage the upstream reported, the answer text, whether the
//! fail-open retry served it, the target an external router named).

use std::io::{BufRead, Write};

use daisugi_verify::gate::py::text::fsdecode;
use daisugi_verify::gate::pyjson::{self, loads, Object, Value};
use daisugi_verify::gateway::meter::{Price, Prices};
use daisugi_verify::gateway::pipeline::{now_ns, prepare_bytes, record, External, Gateway, Settings};
use daisugi_verify::gateway::pyops::{encode_surrogatepass, loads_text};
use daisugi_verify::gateway::sniff::normalize_openai_usage;

fn die(why: &str) -> ! {
    eprintln!("{why}");
    std::process::exit(1)
}

fn lines(path: &str) -> Vec<String> {
    if path.is_empty() {
        return vec![];
    }
    match std::fs::read(path) {
        Ok(b) => String::from_utf8_lossy(&b).split('\n').filter(|l| !l.is_empty()).map(String::from).collect(),
        Err(_) => vec![],
    }
}

fn without_created_at(line: &str) -> Value {
    let Ok(Value::Obj(o)) = loads_text(line) else { die("a journal line is not an object") };
    let mut out = Object::new();
    for (k, v) in o.iter() {
        if k != "created_at" {
            out.set(k, v.clone());
        }
    }
    Value::Obj(out)
}

fn s(o: &Object, k: &str) -> String {
    o.value(k).as_str().unwrap_or_default().to_string()
}

fn main() {
    let base = std::env::var("FUZZ_DIR").unwrap_or_else(|_| die("FUZZ_DIR is not set"));
    let stdin = std::io::stdin();
    let mut out = std::io::BufWriter::new(std::io::stdout());
    for (n, line) in stdin.lock().lines().enumerate() {
        let line = line.unwrap_or_else(|e| die(&e.to_string()));
        let Ok(Value::Obj(seq)) = loads(&line) else { die("a sequence is not an object") };
        let Value::Obj(c) = seq.value("config") else { die("no config") };
        let dir = format!("{base}/probe-{}-{n}", std::process::id());
        std::fs::create_dir_all(&dir).unwrap_or_else(|e| die(&e.to_string()));
        let mut st = Settings {
            cheap_model: s(c, "cheap"),
            router_mode: s(c, "mode"),
            journal_path: format!("{dir}/gateway/turns.jsonl"),
            local_model: s(c, "local"),
            ..Settings::default()
        };
        if c.value("capture") == &Value::Bool(true) {
            st.capture_answers = true;
            st.answers_path = format!("{dir}/gateway/answers.jsonl");
        }
        if let Value::Obj(e) = c.value("external") {
            let mut prices = Prices::new();
            if let Value::Obj(p) = e.value("prices") {
                for (k, v) in p.iter() {
                    let Value::List(pair) = v else { die("a price is not a pair") };
                    let f = |x: &Value| match x {
                        Value::Float(f) => *f,
                        Value::Int(t) => t.parse().unwrap_or(0.0),
                        _ => 0.0,
                    };
                    prices.insert(k.clone(), Price { input: f(&pair[0]), output: f(&pair[1]) });
                }
            }
            st.external = Some(External {
                route_id: s(e, "route_id"),
                capable_target: s(e, "capable"),
                efficient_target: s(e, "efficient"),
                prices,
            });
        }
        let openai = c.value("openai") == &Value::Bool(true);
        let journal = st.journal_path.clone();
        let answers = st.answers_path.clone();
        let mut gw = Gateway::new(st).unwrap_or_else(|e| die(&e));
        let Value::List(turns) = seq.value("turns") else { die("no turns") };
        let mut results = vec![];
        for t in turns {
            let Value::Obj(t) = t else { die("a turn is not an object") };
            let body = encode_surrogatepass(t.value("body").as_str().unwrap_or_default());
            let (outbound, prepared, stream) = prepare_bytes(&mut gw, &body);
            let mut usage = loads_text(t.value("usage").as_str().unwrap_or_default()).unwrap_or_else(|_| die("usage"));
            if openai {
                usage = Value::Obj(normalize_openai_usage(Some(&usage)));
            }
            let before = lines(&journal).len();
            if let Some(p) = &prepared {
                let original = t.value("original") == &Value::Bool(true) && p.decision.downgraded;
                let served = t.value("served").as_str().map(String::from);
                record(&gw, p, &usage, original, t.value("answer").as_str().unwrap_or_default(), served.as_deref(), now_ns());
            }
            let after = lines(&journal);
            let recs: Vec<Value> = after[before.min(after.len())..]
                .iter()
                .map(|l| {
                    let mut v = without_created_at(l);
                    // A turn's time differs run to run; only its type is compared.
                    if let Value::Obj(o) = &mut v {
                        if matches!(o.value("elapsed_ms"), Value::Float(_) | Value::Int(_)) {
                            o.set("elapsed_ms", "num");
                        }
                    }
                    v
                })
                .collect();
            let ans = lines(&answers);
            let last = ans.last().map(|l| without_created_at(l)).unwrap_or(Value::Null);
            let mut r = Object::new();
            r.set("outbound", fsdecode(&outbound));
            r.set("prepared", prepared.is_some());
            r.set("stream", stream);
            r.set("records", Value::List(recs));
            r.set("answers", ans.len() as i64);
            r.set("last_answer", last);
            results.push(Value::Obj(r));
        }
        let mut o = Object::new();
        o.set("results", Value::List(results));
        let _ = writeln!(out, "{}", pyjson::dumps(&Value::Obj(o), true));
        let _ = out.flush();
        let _ = std::fs::remove_dir_all(&dir);
    }
}
