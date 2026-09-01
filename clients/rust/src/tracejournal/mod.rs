//! The trace journal of the Python oracle (`opendaisugi/journal.py`): YAML
//! trace bodies under `<data_dir>/journal/traces` and the SQLite index
//! `<data_dir>/journal/index.db`, with the same schema, migrations and
//! rows, so this binary and the Python CLI read and write one journal. It
//! carries what the distiller and the capture conversion use. The Go
//! client's `internal/tracejournal` is the reference.

pub mod dag;

use rusqlite::types::ValueRef;
use rusqlite::{params, Connection, OpenFlags};

use crate::gate::py::text::repr;
use crate::gate::pyjson::{any_json, dumps, Object, Value};
use crate::pathways::dumped::load_dumped;
use crate::pathways::pmodel::{self, take_model, Id, Mode};
use crate::pathways::store::sql_err;
use crate::pathways::yamldump::safe_dump;
use crate::pathways::PwErr;

/// `journal._SCHEMA`, byte for byte: SQLite keeps each CREATE text in
/// sqlite_master, and the Python CLI reads the same file.
const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS traces (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    task TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    envelope_id TEXT NOT NULL,
    ok INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    violations_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS refinement_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    record_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refinement_session ON refinement_log(session_id);
CREATE TABLE IF NOT EXISTS receipts (
    run_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    timestamp REAL NOT NULL,
    evidence_hash TEXT NOT NULL,
    verify_result INTEGER NOT NULL,
    verify_details TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '{}',
    model_id TEXT,
    PRIMARY KEY (run_id, step_id)
);
CREATE INDEX IF NOT EXISTS idx_receipts_run ON receipts(run_id);
CREATE TABLE IF NOT EXISTS hook_conversions (
    session_id TEXT PRIMARY KEY,
    trace_id TEXT NOT NULL,
    converted_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS provenance_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    detail_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
";

fn unreadable(why: impl Into<String>) -> PwErr {
    PwErr::Unreadable(why.into())
}

/// The path SQLite is given: this SQLite reads a name that starts with
/// "file:" as a URI, and Python's sqlite3.connect never does.
fn dsn(path: &str) -> String {
    if path.starts_with("file:") {
        format!("./{path}")
    } else {
        path.to_string()
    }
}

/// `journal.Journal` over one data directory.
pub struct Journal {
    con: Connection,
    pub data_dir: String,
    pub traces_dir: String,
    /// A journal opened for the checks: not migrated, so a column or
    /// table a later release added may be absent. `has` names what is
    /// there ("table" and "table.column").
    read_only: bool,
    has: Vec<String>,
}

/// `journal.DistillableTrace`.
#[derive(Debug, Clone)]
pub struct Distillable {
    pub trace_id: String,
    pub task: String,
    pub envelope_id: String,
    pub plan_id: String,
    pub created_at: String,
    pub run_id: Option<String>,
    pub run_status: Option<String>,
    pub signature: Option<String>,
}

/// `journal.TraceRecord`: the envelope, plan and result as `model_dump()`
/// values.
#[derive(Debug, Clone)]
pub struct Record {
    pub envelope: Object,
    pub plan: Object,
    pub result: Object,
}

/// A `load_trace` that raises in Python: the exception's type and str().
#[derive(Debug, Clone)]
pub struct LoadError {
    pub typ: String,
    pub msg: String,
}

impl Journal {
    /// `Journal(data_dir=...)`: the traces directory made, the index
    /// created, and each user_version's migrations run in order, an ALTER
    /// that fails (the column is there) ignored.
    pub fn open(data_dir: &str) -> Result<Journal, PwErr> {
        let traces = format!("{data_dir}/journal/traces");
        std::fs::create_dir_all(&traces).map_err(PwErr::Io)?;
        let flags = OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_CREATE | OpenFlags::SQLITE_OPEN_NO_MUTEX;
        let con = Connection::open_with_flags(dsn(&format!("{data_dir}/journal/index.db")), flags).map_err(sql_err)?;
        let j = Journal { con, data_dir: data_dir.into(), traces_dir: traces, read_only: false, has: vec![] };
        j.migrate()?;
        Ok(j)
    }

    /// An existing journal opened for the checks a command makes before
    /// it writes: nothing is made or migrated, and the index takes no
    /// write. What a later migration would add reads as Python reads it
    /// after that migration: an absent column as NULL, an absent table as
    /// empty.
    pub fn open_read_only(data_dir: &str) -> Result<Journal, PwErr> {
        let path = format!("{data_dir}/journal/index.db");
        std::fs::metadata(&path).map_err(PwErr::Io)?;
        let flags = OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX;
        let con = Connection::open_with_flags(dsn(&path), flags).map_err(sql_err)?;
        let mut j = Journal {
            con,
            data_dir: data_dir.into(),
            traces_dir: format!("{data_dir}/journal/traces"),
            read_only: true,
            has: vec![],
        };
        j.read_shape()?;
        Ok(j)
    }

    fn read_shape(&mut self) -> Result<(), PwErr> {
        let tables: Vec<String> = {
            let mut st = self.con.prepare("SELECT name FROM sqlite_master WHERE type = 'table'").map_err(sql_err)?;
            let rows = st.query_map([], |r| r.get::<_, String>(0)).map_err(sql_err)?;
            rows.collect::<Result<_, _>>().map_err(sql_err)?
        };
        for t in tables {
            let mut st = self.con.prepare("SELECT name FROM pragma_table_info(?)").map_err(sql_err)?;
            let cols: Vec<String> =
                st.query_map([&t], |r| r.get::<_, String>(0)).map_err(sql_err)?.collect::<Result<_, _>>().map_err(sql_err)?;
            for c in cols {
                self.has.push(format!("{t}.{c}"));
            }
            self.has.push(t);
        }
        Ok(())
    }

    /// Whether a migrated journal would read the table from disk.
    fn has_table(&self, t: &str) -> bool {
        !self.read_only || self.has.iter().any(|h| h == t)
    }

    /// A traces column, or NULL for one this journal lacks.
    fn col(&self, c: &str) -> String {
        if !self.read_only || self.has.iter().any(|h| *h == format!("traces.{c}")) {
            c.to_string()
        } else {
            "NULL".to_string()
        }
    }

    fn migrate(&self) -> Result<(), PwErr> {
        let c = &self.con;
        c.execute_batch(SCHEMA).map_err(sql_err)?;
        let version: i64 = c.query_row("PRAGMA user_version", [], |r| r.get(0)).map_err(sql_err)?;
        let attempt = |q: &str| {
            let _ = c.execute_batch(q);
        };
        let must = |q: &str| c.execute_batch(q).map_err(sql_err);
        if version < 2 {
            for col in ["run_id TEXT", "run_status TEXT", "failed_step_id TEXT", "total_duration_ms REAL"] {
                attempt(&format!("ALTER TABLE traces ADD COLUMN {col}"));
            }
            must("PRAGMA user_version = 2")?;
        }
        if version < 3 {
            attempt("ALTER TABLE refinement_log ADD COLUMN cache_key TEXT");
            must("CREATE INDEX IF NOT EXISTS idx_refinement_cache_key ON refinement_log(cache_key)")?;
            must("PRAGMA user_version = 3")?;
        }
        if version < 4 {
            attempt("ALTER TABLE receipts ADD COLUMN model_id TEXT");
            must("PRAGMA user_version = 4")?;
        }
        if version < 5 {
            attempt("ALTER TABLE traces ADD COLUMN structure_signature TEXT");
            must("CREATE INDEX IF NOT EXISTS idx_traces_structure ON traces(structure_signature)")?;
            must("PRAGMA user_version = 5")?;
        }
        if version < 6 {
            attempt(
                "CREATE TABLE IF NOT EXISTS provenance_log (id INTEGER PRIMARY KEY AUTOINCREMENT, \
                 detail_json TEXT NOT NULL, inserted_at REAL NOT NULL)",
            );
            must("PRAGMA user_version = 6")?;
        }
        if version < 7 {
            for col in ["effect_class TEXT", "reversibility TEXT", "reversal_json TEXT"] {
                attempt(&format!("ALTER TABLE receipts ADD COLUMN {col}"));
            }
            must("PRAGMA user_version = 7")?;
        }
        Ok(())
    }

    /// `list_successful_traces`: succeeded runs and verified imports,
    /// newest first. `since` None reads them all.
    pub fn list_successful(&self, since: Option<f64>) -> Result<Vec<Distillable>, PwErr> {
        if !self.has_table("traces") {
            return Ok(vec![]);
        }
        let status = self.col("run_status");
        let mut q = format!(
            "SELECT id, task, envelope_id, plan_id, {}, {status}, created_at, {} FROM traces \
             WHERE ({status} = 'succeeded' OR ({status} IS NULL AND ok = 1))",
            self.col("run_id"),
            self.col("structure_signature"),
        );
        let since_text = since.map(since_iso);
        if since_text.is_some() {
            q.push_str(" AND created_at >= ?");
        }
        // rowid breaks a tie: created_at has one-second steps, and the
        // later insert is the newer trace.
        q.push_str(" ORDER BY created_at DESC, rowid DESC");
        let mut st = self.con.prepare(&q).map_err(sql_err)?;
        let mut rows = match &since_text {
            Some(t) => st.query(params![t]),
            None => st.query([]),
        }
        .map_err(sql_err)?;
        let not_text = || unreadable("a trace row holds a value that is not text");
        let mut out = vec![];
        while let Some(r) = rows.next().map_err(sql_err)? {
            let text = |i: usize| -> Result<Option<String>, PwErr> {
                match r.get_ref(i).map_err(sql_err)? {
                    ValueRef::Null => Ok(None),
                    ValueRef::Text(b) => Ok(Some(crate::pathways::store::text(b)?)),
                    _ => Err(not_text()),
                }
            };
            let need = |i: usize| -> Result<String, PwErr> { text(i)?.ok_or_else(not_text) };
            out.push(Distillable {
                trace_id: need(0)?,
                task: need(1)?,
                envelope_id: need(2)?,
                plan_id: need(3)?,
                run_id: text(4)?,
                run_status: text(5)?,
                created_at: need(6)?,
                signature: text(7)?,
            });
        }
        Ok(out)
    }

    /// The YAML body of a trace.
    pub fn trace_path(&self, id: &str) -> String {
        format!("{}/{id}.yaml", self.traces_dir)
    }

    /// `load_trace`: the YAML read with safe_load and each part validated.
    /// A body not in the form `yaml.safe_dump` writes is refused; what
    /// Python raises is the inner error.
    pub fn load_trace(&self, id: &str) -> Result<Result<Record, LoadError>, PwErr> {
        let path = self.trace_path(id);
        let raw = match std::fs::read(&path) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Err(LoadError {
                    typ: "FileNotFoundError".into(),
                    msg: format!("No trace with id {} at {path}", repr(id)),
                }))
            }
            Err(e) => return Err(PwErr::Io(e)),
        };
        let text = String::from_utf8(raw).map_err(|_| unreadable(format!("the trace {id} is not UTF-8")))?;
        let text = text.replace("\r\n", "\n").replace('\r', "\n");
        let v = load_dumped(&text).map_err(|w| unreadable(format!("the trace {id}: {}", w.0)))?;
        let o = match v {
            Value::Obj(o) => o,
            _ => return Err(unreadable(format!("the trace {id} is not a mapping"))),
        };
        for k in ["id", "created_at", "task", "envelope", "plan", "result"] {
            if o.get(k).is_none() {
                return Ok(Err(LoadError { typ: "KeyError".into(), msg: repr(k) }));
            }
        }
        let mut parts = vec![];
        for (key, id_) in [("envelope", Id::Envelope), ("plan", Id::ActionPlan), ("result", Id::VerificationResult)] {
            let inner = match o.value(key) {
                Value::Obj(x) => x.clone(),
                _ => return Err(unreadable(format!("the trace {id}: {key} is not a mapping"))),
            };
            // Model(**raw[...]): keyword arguments, so every key is a str.
            match take_model(id_, Value::Obj(inner), Mode::Python) {
                Ok(Value::Obj(x)) => parts.push(x),
                Ok(_) => return Err(unreadable(format!("the trace {id}: {key} did not validate to a mapping"))),
                Err(e) => {
                    if e.unreadable_step() {
                        return Err(unreadable(format!("the trace {id} holds a plan step given as a string")));
                    }
                    return Ok(Err(LoadError { typ: "ValidationError".into(), msg: e.text() }));
                }
            }
        }
        let mut it = parts.into_iter();
        let (envelope, plan, result) = (it.next().unwrap_or_default(), it.next().unwrap_or_default(), it.next().unwrap_or_default());
        Ok(Ok(Record { envelope, plan, result }))
    }

    /// `get_refinements(session_id)`: each record_json read with
    /// `RefinementRecord.model_validate_json`, in insertion order. A record
    /// this binary does not fully read is refused.
    pub fn refinements(&self, session: &str) -> Result<Vec<Object>, PwErr> {
        if !self.has_table("refinement_log") {
            return Ok(vec![]);
        }
        let mut st = self
            .con
            .prepare("SELECT record_json FROM refinement_log WHERE session_id = ? ORDER BY inserted_at ASC")
            .map_err(sql_err)?;
        let mut rows = st.query([session]).map_err(sql_err)?;
        let mut out = vec![];
        while let Some(r) = rows.next().map_err(sql_err)? {
            let s = match r.get_ref(0).map_err(sql_err)? {
                ValueRef::Text(b) => crate::pathways::store::text(b)?,
                _ => return Err(unreadable("a refinement record is not text")),
            };
            match pmodel::validate_json(Id::RefinementRecord, &s) {
                Ok(Value::Obj(o)) => out.push(o),
                Ok(_) => return Err(unreadable(format!("a refinement record of {session} is not a mapping"))),
                Err(e) => return Err(unreadable(format!("a refinement record of {session}: {}", e.text()))),
            }
        }
        Ok(out)
    }

    /// `is_session_converted`.
    pub fn is_converted(&self, session: &str) -> Result<bool, PwErr> {
        if !self.has_table("hook_conversions") {
            return Ok(false);
        }
        let mut st = self.con.prepare("SELECT 1 FROM hook_conversions WHERE session_id = ? LIMIT 1").map_err(sql_err)?;
        st.exists([session]).map_err(sql_err)
    }

    /// `mark_session_converted`.
    pub fn mark_converted(&self, session: &str, trace_id: &str, at: f64) -> Result<(), PwErr> {
        self.con
            .execute(
                "INSERT OR REPLACE INTO hook_conversions (session_id, trace_id, converted_at) VALUES (?, ?, ?)",
                params![session, trace_id, at],
            )
            .map_err(sql_err)?;
        Ok(())
    }

    /// `Journal.log`: the YAML body written first, then the index row; a
    /// failed insert removes the body. `env`, `plan` and `result` are
    /// `model_dump()` values; the YAML holds their JSON-mode dumps.
    pub fn log(&self, task: &str, env: &Object, plan: &Object, result: &Object, trace_id: &str, created_at: &str) -> Result<(), PwErr> {
        let text = trace_body(task, env, plan, result, trace_id, created_at)?;
        let path = self.trace_path(trace_id);
        std::fs::write(&path, text).map_err(PwErr::Io)?;
        let sig = dag::structure_signature(plan);
        let violations: Vec<Value> = match result.value("violations") {
            Value::List(l) => l.iter().map(any_json).collect(),
            _ => vec![],
        };
        let ok = matches!(result.value("ok"), Value::Bool(true)) as i64;
        let duration = match result.value("duration_ms") {
            Value::Float(f) => *f,
            _ => 0.0,
        };
        let r = self.con.execute(
            "INSERT INTO traces (id, created_at, task, plan_id, envelope_id, ok, duration_ms,  violations_json, structure_signature) \
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            params![
                trace_id,
                created_at,
                task,
                plan.value("id").as_str().unwrap_or(""),
                env.value("id").as_str().unwrap_or(""),
                ok,
                duration,
                dumps(&Value::List(violations), true),
                sig
            ],
        );
        if let Err(e) = r {
            let _ = std::fs::remove_file(&path);
            return Err(sql_err(e));
        }
        Ok(())
    }
}

/// A trace's YAML body, as `Journal.log` writes it: the JSON-mode dumps of
/// the envelope, plan and result through `yaml.safe_dump`. A value the
/// dumper does not write is refused.
pub fn trace_body(task: &str, env: &Object, plan: &Object, result: &Object, trace_id: &str, created_at: &str) -> Result<String, PwErr> {
    let mut payload = Object::new();
    payload.set("id", trace_id);
    payload.set("created_at", created_at);
    payload.set("task", task);
    payload.set("envelope", any_json(&Value::Obj(env.clone())));
    payload.set("plan", any_json(&Value::Obj(plan.clone())));
    payload.set("result", any_json(&Value::Obj(result.clone())));
    safe_dump(&Value::Obj(payload)).map_err(|w| unreadable(w.0))
}

/// The text `list_successful_traces` compares created_at with:
/// `datetime.fromtimestamp(since, tz=utc).isoformat()` with Z for +00:00,
/// microseconds shown only when not zero.
pub fn since_iso(since: f64) -> String {
    let mut sec = since.trunc() as i64;
    let mut us = ((since - since.trunc()) * 1e6).round_ties_even() as i64;
    if us >= 1_000_000 {
        sec += 1;
        us -= 1_000_000;
    } else if us < 0 {
        sec -= 1;
        us += 1_000_000;
    }
    let mut out = iso_seconds(sec);
    if us != 0 {
        out.push_str(&format!(".{us:06}"));
    }
    out.push('Z');
    out
}

/// "YYYY-MM-DDTHH:MM:SS" of a Unix time in UTC.
pub fn iso_seconds(t: i64) -> String {
    let days = t.div_euclid(86_400);
    let secs = t.rem_euclid(86_400);
    let (y, m, d) = civil_from_days(days);
    format!("{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}", secs / 3600, secs % 3600 / 60, secs % 60)
}

/// The proleptic Gregorian date of a day count from 1970-01-01.
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// `envelope.ENVELOPE_PROMPT_VERSION`.
const ENVELOPE_PROMPT_VERSION: &str = "2026-04-18";

/// `EnvelopeCache(path, prompt_version=...)`, which the facade builds for
/// every command that makes one: the parent made, the schema created, and
/// rows of another prompt version evicted.
pub fn ensure_envelope_cache(path: &str) -> Result<(), PwErr> {
    if let Some(parent) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(parent).map_err(PwErr::Io)?;
    }
    let con = Connection::open(dsn(path)).map_err(sql_err)?;
    con.execute_batch(
        "
CREATE TABLE IF NOT EXISTS envelope_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_version ON envelope_cache(prompt_version);
",
    )
    .map_err(sql_err)?;
    con.execute("DELETE FROM envelope_cache WHERE prompt_version != ?", [ENVELOPE_PROMPT_VERSION]).map_err(sql_err)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn since_is_written_as_python_writes_it() {
        assert_eq!(since_iso(0.0), "1970-01-01T00:00:00Z");
        assert_eq!(since_iso(1_700_000_000.25), "2023-11-14T22:13:20.250000Z");
        assert_eq!(since_iso(951_782_400.0), "2000-02-29T00:00:00Z");
        assert_eq!(since_iso(-1.5), "1969-12-31T23:59:58.500000Z");
    }
}
