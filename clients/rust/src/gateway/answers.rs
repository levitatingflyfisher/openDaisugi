//! `gateway_answers.AnswerStore` and `capture_answer`: the bounded ring of
//! captured answers, one JSON line each.

use super::meter::turn_signature;
use super::pyops::{loads_text, JsonErr};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

/// One loaded line, its values as the file held them.
#[derive(Debug, Clone)]
pub struct AnswerEntry {
    pub signature: Value,
    pub task: Value,
    pub answer: Value,
    pub created_at: Value,
    pub ground_hash: Value,
}

impl AnswerEntry {
    fn json(&self) -> String {
        let mut o = Object::with_capacity(5);
        o.set("signature", self.signature.clone());
        o.set("task", self.task.clone());
        o.set("answer", self.answer.clone());
        o.set("created_at", self.created_at.clone());
        o.set("ground_hash", self.ground_hash.clone());
        pyjson::dumps(&Value::Obj(o), true)
    }
}

/// Why a store was not read.
#[derive(Debug)]
pub enum StoreErr {
    /// A file the oracle raises on while reading it: not UTF-8, or a line
    /// json.loads raises something other than JSONDecodeError on.
    Unreadable,
    Io(std::io::Error),
}

impl std::fmt::Display for StoreErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StoreErr::Unreadable => write!(f, "unreadable"),
            StoreErr::Io(e) => write!(f, "{e}"),
        }
    }
}

/// `open(path, "a").write(line + "\n")` after mkdir -p.
pub fn append_line(path: &str, line: &str) -> std::io::Result<()> {
    use std::io::Write;
    if let Some(dir) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(dir)?;
    }
    let mut f = std::fs::OpenOptions::new().append(true).create(true).open(path)?;
    let mut b = Vec::with_capacity(line.len() + 1);
    b.extend_from_slice(&super::pyops::encode_surrogatepass(line));
    b.push(b'\n');
    f.write_all(&b)
}

/// `time.time()` for a time in nanoseconds since the epoch.
pub fn py_time(ns: i128) -> f64 {
    if ns % 1_000_000_000 == 0 {
        return (ns / 1_000_000_000) as f64;
    }
    ns as f64 / 1e9
}

/// `capture_answer(AnswerStore(path, max_entries=max), task=task,
/// answer=answer, created_at=time.time())`.
pub fn capture_answer(path: &str, max: usize, task: &str, answer: &str, now_ns: i128) -> Result<(), StoreErr> {
    let sig = turn_signature(task).map_err(|_| StoreErr::Unreadable)?;
    let e = AnswerEntry {
        signature: Value::Str(sig),
        task: Value::Str(task.into()),
        answer: Value::Str(answer.into()),
        created_at: Value::Float(py_time(now_ns)),
        ground_hash: Value::Null,
    };
    append_line(path, &e.json()).map_err(StoreErr::Io)?;
    let entries = load_answers(path)?;
    if entries.len() <= max {
        return Ok(());
    }
    let mut b = String::new();
    for e in &entries[entries.len() - max..] {
        b.push_str(&e.json());
        b.push('\n');
    }
    let tmp = format!("{path}.tmp");
    std::fs::write(&tmp, super::pyops::encode_surrogatepass(&b)).map_err(StoreErr::Io)?;
    std::fs::rename(&tmp, path).map_err(StoreErr::Io)
}

/// `AnswerStore.load()`: a line that is not JSON, or not an entry, is
/// skipped.
pub fn load_answers(path: &str) -> Result<Vec<AnswerEntry>, StoreErr> {
    let objs = load_records(path, &["signature", "task", "answer", "created_at"])?;
    Ok(objs
        .into_iter()
        .map(|o| AnswerEntry {
            signature: o.value("signature").clone(),
            task: o.value("task").clone(),
            answer: o.value("answer").clone(),
            created_at: o.value("created_at").clone(),
            ground_hash: o.value("ground_hash").clone(),
        })
        .collect())
}

/// A JSONL store read the way `GatewayJournal.load` and `AnswerStore.load`
/// read it: each line a dict holding every required field.
pub fn load_records(path: &str, required: &[&str]) -> Result<Vec<Object>, StoreErr> {
    let raw = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(vec![]),
        Err(e) => return Err(StoreErr::Io(e)),
    };
    let t = String::from_utf8(raw).map_err(|_| StoreErr::Unreadable)?;
    let mut out = vec![];
    for line in text::splitlines(&t) {
        let line = text::strip(line);
        if line.is_empty() {
            continue;
        }
        let v = match loads_text(line) {
            Ok(v) => v,
            Err(JsonErr::Decode) => continue,
            Err(_) => return Err(StoreErr::Unreadable),
        };
        let Value::Obj(o) = v else { continue };
        if required.iter().all(|f| o.get(f).is_some()) {
            out.push(o);
        }
    }
    Ok(out)
}
