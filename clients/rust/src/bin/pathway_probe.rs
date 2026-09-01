//! `pathway-probe` runs the pathway matcher on many queries in one
//! process, for `clients/pathway_compare.py`. It is a test instrument: the
//! daisugi binary has no find command, as the Python CLI has none.
//!
//! ```text
//! pathway-probe find DB_PATH < queries.jsonl   one {"task": ...} per line
//! pathway-probe tokens TOKENIZER_JSON < texts.jsonl
//! pathway-probe time DB_PATH TASK N            N finds, each on a fresh store, timed
//! ```
//!
//! find reads matcher_model from $HOME/.opendaisugi/config.yaml, as the
//! oracle's find does, and writes one JSON line per query: the matched id
//! and score, the stale warning, and why nothing could match. The output
//! is the Go probe's, field for field.

use std::collections::HashMap;
use std::io::{BufRead, Write};

use daisugi_verify::pathways::find::{Cache, FindErr};
use daisugi_verify::pathways::potion::{self, tokenizer::Tokenizer};
use daisugi_verify::pathways::store::Store;

fn env() -> potion::Env {
    let vars: HashMap<String, String> = std::env::vars().collect();
    let home = vars.get("HOME").cloned().unwrap_or_default();
    let mut e = potion::Env::from_process(vars.clone(), home);
    e.no_fetch = vars.get("PATHWAY_PROBE_NO_FETCH").is_some_and(|v| !v.is_empty());
    e
}

fn matcher_key() -> Result<String, String> {
    let home = std::env::var("HOME").map_err(|_| "HOME is unset".to_string())?;
    daisugi_verify::cli::config::load(&format!("{home}/.opendaisugi/config.yaml"))
        .map(|c| c.matcher_model)
        .map_err(|_| "the config file is not read by this binary".to_string())
}

/// A JSON string as Go's encoder writes it (no HTML escaping).
fn jstr(s: &str) -> String {
    serde_json::to_string(s).unwrap_or_else(|_| "\"\"".into())
}

fn find(db: &str) -> i32 {
    let key = match matcher_key() {
        Ok(k) => k,
        Err(e) => {
            eprintln!("config: {e}");
            return 1;
        }
    };
    let s = match Store::open(db) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("open: {e}");
            return 1;
        }
    };
    let pe = env();
    let mut cache = Cache::default();
    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = std::io::BufWriter::new(stdout.lock());
    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("query: {e}");
                return 1;
            }
        };
        if line.trim().is_empty() {
            continue;
        }
        let q: serde_json::Value = match serde_json::from_str(&line) {
            Ok(q) => q,
            Err(e) => {
                eprintln!("query: {e}");
                return 1;
            }
        };
        let task = q["task"].as_str().unwrap_or("").to_string();
        let threshold = q["threshold"].as_f64();
        let mut parts: Vec<String> = vec![];
        match s.find(&task, &key, &pe, threshold, &mut cache) {
            Err(FindErr::NotCarried(n)) => {
                parts.push("\"id\":null".into());
                parts.push("\"score\":null".into());
                parts.push(format!("\"refused\":{}", jstr(&n.0)));
            }
            Err(FindErr::Err(e)) => {
                parts.push("\"id\":null".into());
                parts.push("\"score\":null".into());
                parts.push(format!("\"error\":{}", jstr(&e.to_string())));
            }
            Ok(r) => {
                match &r.matched {
                    Some((p, score)) => {
                        parts.push(format!("\"id\":{}", jstr(p.id())));
                        parts.push(format!("\"score\":{}", serde_json::to_string(score).unwrap_or("null".into())));
                    }
                    None => {
                        parts.push("\"id\":null".into());
                        parts.push("\"score\":null".into());
                    }
                }
                if !r.warning.is_empty() {
                    parts.push(format!("\"warning\":{}", jstr(&r.warning)));
                }
                if !r.reason.is_empty() {
                    parts.push(format!("\"reason\":{}", jstr(&r.reason)));
                }
            }
        }
        let _ = writeln!(out, "{{{}}}", parts.join(","));
    }
    0
}

fn tokens(path: &str) -> i32 {
    let data = match std::fs::read(path) {
        Ok(d) => d,
        Err(e) => {
            eprintln!("{e}");
            return 1;
        }
    };
    let tok = match Tokenizer::parse(&data) {
        Ok(t) => t,
        Err(e) => {
            eprintln!("{e}");
            return 1;
        }
    };
    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = std::io::BufWriter::new(stdout.lock());
    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("text: {e}");
                return 1;
            }
        };
        let text: String = match serde_json::from_str(&line) {
            Ok(t) => t,
            Err(e) => {
                eprintln!("text: {e}");
                return 1;
            }
        };
        let ids = tok.encode(&text);
        let _ = writeln!(out, "{}", serde_json::to_string(&ids).unwrap_or_else(|_| "[]".into()));
    }
    0
}

fn time_find(args: &[String]) -> i32 {
    if args.len() != 3 {
        return 2;
    }
    let n: usize = args[2].parse().unwrap_or(0);
    let key = match matcher_key() {
        Ok(k) => k,
        Err(e) => {
            eprintln!("{e}");
            return 1;
        }
    };
    for _ in 0..n {
        let t0 = std::time::Instant::now();
        let s = match Store::open(&args[0]) {
            Ok(s) => s,
            Err(e) => {
                eprintln!("{e}");
                return 1;
            }
        };
        let mut cache = Cache::default();
        let id = match s.find(&args[1], &key, &env(), None, &mut cache) {
            Ok(r) => r.matched.map(|(p, _)| p.id().to_string()).unwrap_or_default(),
            Err(FindErr::NotCarried(e)) => {
                eprintln!("{}", e.0);
                return 1;
            }
            Err(FindErr::Err(e)) => {
                eprintln!("{e}");
                return 1;
            }
        };
        println!("{:.2} ms {id}", t0.elapsed().as_secs_f64() * 1000.0);
    }
    0
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 3 {
        eprintln!("usage: pathway-probe find DB | tokens TOKENIZER_JSON | time DB TASK N");
        std::process::exit(2);
    }
    let code = match args[1].as_str() {
        "find" => find(&args[2]),
        "tokens" => tokens(&args[2]),
        "time" => time_find(&args[2..]),
        _ => 2,
    };
    std::process::exit(code)
}
