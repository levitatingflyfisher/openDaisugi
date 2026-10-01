//! A test instrument for `clients/alias_compare.py`: reads the alias cases
//! file named by its one argument and, for each case, registers and
//! resolves aliases as `clients/alias_cases.py` has the oracle do, writing
//! one JSON line per case. It is not shipped.

use std::io::{BufRead, Write};

use daisugi_verify::aliases::{load_system, parse_and_resolve, Alias, Error, Registry};
use daisugi_verify::gate::pyjson::{dumps, loads, Object, Value};

fn outcome(r: Result<Value, Error>) -> Value {
    Value::Obj(match r {
        Ok(v) => Object::new().with("ok", v),
        Err(e) => Object::new()
            .with("error", e.class.as_str())
            .with("message", e.msg.as_str()),
    })
}

fn list(v: &Value) -> Vec<Value> {
    match v {
        Value::List(l) => l.clone(),
        _ => vec![],
    }
}

fn text(o: &Object, k: &str) -> String {
    o.value(k).as_str().unwrap_or("").to_string()
}

fn run(c: &Object) -> Object {
    let mut reg = Registry::new();
    let mut out = Object::new();
    if c.value("system").truthy() {
        out.set(
            "system",
            outcome(load_system(&mut reg).map(|_| Value::Null)),
        );
    }
    let mut regs = vec![];
    for x in list(c.value("register")) {
        let a = x.as_obj().cloned().unwrap_or_default();
        let alias = Alias {
            name: text(&a, "name"),
            params: list(a.value("params"))
                .iter()
                .filter_map(|p| p.as_str().map(String::from))
                .collect(),
            expr: a.value("expr").clone(),
            tier: text(&a, "tier"),
            description: text(&a, "description"),
        };
        regs.push(outcome(reg.register(alias).map(|_| Value::Null)));
    }
    let res: Vec<Value> = list(c.value("resolve"))
        .iter()
        .map(|e| outcome(parse_and_resolve(&reg, e)))
        .collect();
    let looks: Vec<Value> = list(c.value("lookup"))
        .iter()
        .map(|n| outcome(reg.lookup(n.as_str().unwrap_or("")).map(|a| a.dump())))
        .collect();
    out.set("register", Value::List(regs));
    out.set("resolve", Value::List(res));
    out.set("lookup", Value::List(looks));
    out
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 2 {
        eprintln!("usage: alias-probe CASES.jsonl");
        std::process::exit(2);
    }
    let f = std::fs::File::open(&args[1]).unwrap_or_else(|e| {
        eprintln!("{e}");
        std::process::exit(1)
    });
    let stdout = std::io::stdout();
    let mut w = std::io::BufWriter::new(stdout.lock());
    for line in std::io::BufReader::new(f).lines() {
        let line = line.expect("a line of the cases file");
        let Ok(Value::Obj(c)) = loads(&line) else {
            eprintln!("a case that is not a JSON object");
            std::process::exit(1)
        };
        let r = Object::new()
            .with("id", c.value("id").clone())
            .with("result", Value::Obj(run(&c)));
        writeln!(w, "{}", dumps(&Value::Obj(r), true)).expect("stdout");
    }
}
