//! The pathway store of the Python oracle (`opendaisugi/pathway_store.py`):
//! the same SQLite schema, migrations and rows, so this binary and the
//! Python CLI read and write one file. SQLite is the amalgamation rusqlite
//! bundles, compiled into the binary.

use rusqlite::types::ValueRef;
use rusqlite::{Connection, OpenFlags};

use super::PwErr;

/// How many connections read the table at once.
const READERS: usize = 4;

/// `_SCHEMA`, byte for byte: SQLite keeps this text in sqlite_master, and
/// the Python CLI reads the same file.
pub const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS pathways (
    id TEXT PRIMARY KEY,
    task_description TEXT NOT NULL,
    task_embedding_json TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    plan_template_json TEXT NOT NULL,
    source_trace_ids_json TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    hit_count INTEGER NOT NULL DEFAULT 0,
    distilled_at REAL NOT NULL,
    embedding_model TEXT NOT NULL DEFAULT '',
    embedding_model_version TEXT NOT NULL DEFAULT '',
    last_activation_at REAL NOT NULL DEFAULT 0.0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    structure_signature TEXT,
    parameters_json TEXT NOT NULL DEFAULT '[]'
);
";

/// The columns added after the first schema, each with its ALTER, in the
/// oracle's order.
const ADDITIVE: &[(&str, &str)] = &[
    ("embedding_model", "ALTER TABLE pathways ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''"),
    ("embedding_model_version", "ALTER TABLE pathways ADD COLUMN embedding_model_version TEXT NOT NULL DEFAULT ''"),
    ("last_activation_at", "ALTER TABLE pathways ADD COLUMN last_activation_at REAL NOT NULL DEFAULT 0.0"),
    ("failure_count", "ALTER TABLE pathways ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0"),
    ("structure_signature", "ALTER TABLE pathways ADD COLUMN structure_signature TEXT"),
    ("parameters_json", "ALTER TABLE pathways ADD COLUMN parameters_json TEXT NOT NULL DEFAULT '[]'"),
];

/// Removed on open when present; a failed drop is ignored, as the oracle
/// ignores it.
const DROPPED: &[&str] = &["pitfalls_json", "validation_score"];

/// One column value as the driver read it.
#[derive(Debug, Clone, PartialEq)]
pub enum Col {
    Null,
    Int(i64),
    Real(f64),
    Text(String),
    Blob,
}

impl Col {
    pub fn from_ref(v: ValueRef<'_>) -> Result<Col, PwErr> {
        Ok(match v {
            ValueRef::Null => Col::Null,
            ValueRef::Integer(i) => Col::Int(i),
            ValueRef::Real(f) => Col::Real(f),
            ValueRef::Text(b) => Col::Text(text(b)?),
            ValueRef::Blob(_) => Col::Blob,
        })
    }
}

/// A TEXT value, as Python's sqlite3 decodes it.
pub fn text(b: &[u8]) -> Result<String, PwErr> {
    match std::str::from_utf8(b) {
        Ok(s) => Ok(s.to_string()),
        Err(_) => Err(PwErr::Invalid("sqlite3.OperationalError: Could not decode to UTF-8 column".into())),
    }
}

/// `sqlite3` errors, as Python names them.
pub fn sql_err(e: rusqlite::Error) -> PwErr {
    let kind = match &e {
        rusqlite::Error::SqliteFailure(f, _) if f.code == rusqlite::ErrorCode::ConstraintViolation => "IntegrityError",
        _ => "OperationalError",
    };
    PwErr::Invalid(format!("sqlite3.{kind}: {e}"))
}

/// The path SQLite is given. This SQLite reads a name that starts with
/// "file:" as a URI; Python's sqlite3.connect never does, so such a
/// relative name is given as "./file:...", the same file.
fn dsn(path: &str) -> String {
    if path.starts_with("file:") {
        format!("./{path}")
    } else {
        path.to_string()
    }
}

fn connect(path: &str) -> Result<Connection, PwErr> {
    connect_with(path, false)
}

fn connect_with(path: &str, read_only: bool) -> Result<Connection, PwErr> {
    let flags = if read_only {
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX
    } else {
        OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_CREATE | OpenFlags::SQLITE_OPEN_NO_MUTEX
    };
    let c = Connection::open_with_flags(dsn(path), flags).map_err(sql_err)?;
    // Memory-mapped reads halve the time a scan of every stored vector
    // takes; the file on disk is the same.
    let _ = c.execute_batch("PRAGMA mmap_size=1073741824");
    Ok(c)
}

/// PathwayStore over one database file.
pub struct Store {
    pub con: Connection,
    pub path: String,
    /// A store opened for the checks a command makes before it writes:
    /// not migrated, and read on read-only connections.
    read_only: bool,
    /// The additive columns a read-only store lacks, each read as the
    /// value its ALTER gives an existing row.
    missing: Vec<usize>,
    /// A read-only store's file holds no pathways table: it reads as
    /// empty.
    no_table: bool,
}

/// The values the ALTERs of `ADDITIVE` give an existing row, as SQL.
const ADDITIVE_DEFAULTS: &[&str] = &["''", "''", "0.0", "0", "NULL", "'[]'"];

impl Store {
    /// `PathwayStore(db_path)`: the parent directories made, the table
    /// created when absent, and the additive and dropped columns applied.
    pub fn open(path: &str) -> Result<Store, PwErr> {
        if let Some(parent) = std::path::Path::new(path).parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent).map_err(PwErr::Io)?;
            }
        }
        let con = connect(path)?;
        let s = Store { con, path: path.to_string(), read_only: false, missing: vec![], no_table: false };
        s.migrate()?;
        Ok(s)
    }

    /// An existing store opened for the checks a command makes before it
    /// writes: nothing is made or migrated. A row reads as it would after
    /// the migration.
    pub fn open_read_only(path: &str) -> Result<Store, PwErr> {
        std::fs::metadata(path).map_err(PwErr::Io)?;
        let con = connect_with(path, true)?;
        let mut s = Store { con, path: path.to_string(), read_only: true, missing: vec![], no_table: false };
        let cols = s.columns()?;
        s.no_table = cols.is_empty();
        s.missing = (0..ADDITIVE.len()).filter(|i| !cols.iter().any(|c| c == ADDITIVE[*i].0)).collect();
        Ok(s)
    }

    /// The select list for every column: `*`, and on a read-only store
    /// each missing additive column as its default.
    pub fn all_columns(&self) -> String {
        let mut cols = "*".to_string();
        for i in &self.missing {
            cols.push_str(&format!(", {} AS {}", ADDITIVE_DEFAULTS[*i], ADDITIVE[*i].0));
        }
        cols
    }

    /// `_load_all_rows()` as `reembed_stale` reads it: each row's id, task
    /// and provenance stamp, in table order.
    pub fn provenance(&self) -> Result<Vec<[Col; 4]>, PwErr> {
        if self.no_table {
            return Ok(vec![]);
        }
        let q = format!(
            "SELECT id, task_description, embedding_model, embedding_model_version FROM \
             (SELECT rowid AS row_order, {} FROM pathways) ORDER BY row_order",
            self.all_columns()
        );
        let mut st = self.con.prepare(&q).map_err(sql_err)?;
        let mut rows = st.query([]).map_err(sql_err)?;
        let mut out = vec![];
        while let Some(r) = rows.next().map_err(sql_err)? {
            let c = |i: usize| -> Result<Col, PwErr> { Col::from_ref(r.get_ref(i).map_err(sql_err)?) };
            out.push([c(0)?, c(1)?, c(2)?, c(3)?]);
        }
        Ok(out)
    }

    /// `reembed_stale`'s UPDATE of one row.
    pub fn update_embedding(&self, id: &str, embedding: &str, model: &str, version: &str) -> Result<(), PwErr> {
        self.con
            .execute(
                "UPDATE pathways SET task_embedding_json = ?, embedding_model = ?, embedding_model_version = ? WHERE id = ?",
                [embedding, model, version, id],
            )
            .map_err(sql_err)?;
        Ok(())
    }

    fn migrate(&self) -> Result<(), PwErr> {
        self.con.execute_batch(SCHEMA).map_err(sql_err)?;
        let existing = self.columns()?;
        for (col, ddl) in ADDITIVE {
            if !existing.iter().any(|c| c == col) {
                self.con.execute_batch(ddl).map_err(sql_err)?;
            }
        }
        for col in DROPPED {
            if existing.iter().any(|c| c == col) {
                let _ = self.con.execute_batch(&format!("ALTER TABLE pathways DROP COLUMN {col}"));
            }
        }
        Ok(())
    }

    fn columns(&self) -> Result<Vec<String>, PwErr> {
        let mut st = self.con.prepare("PRAGMA table_info(pathways)").map_err(sql_err)?;
        let rows = st.query_map([], |r| r.get::<_, String>(1)).map_err(sql_err)?;
        let mut out = vec![];
        for r in rows {
            out.push(r.map_err(sql_err)?);
        }
        Ok(out)
    }

    /// `PathwayStore.delete`: true when a row went.
    pub fn delete(&self, id: &str) -> Result<bool, PwErr> {
        let n = self.con.execute("DELETE FROM pathways WHERE id = ?", [id]).map_err(sql_err)?;
        Ok(n > 0)
    }

    /// `PathwayStore.stats`: the row count and SUM(hit_count), each 0 when
    /// NULL. The sum keeps SQLite's type: an int, or a float when a row
    /// holds a REAL.
    pub fn stats(&self) -> Result<(i64, Col), PwErr> {
        self.con
            .query_row("SELECT COUNT(*), SUM(hit_count) FROM pathways", [], |r| {
                let n: i64 = r.get(0)?;
                let s = match r.get_ref(1)? {
                    ValueRef::Null => Col::Int(0),
                    ValueRef::Integer(i) => Col::Int(i),
                    ValueRef::Real(f) if f == 0.0 => Col::Int(0),
                    ValueRef::Real(f) => Col::Real(f),
                    _ => Col::Blob,
                };
                Ok((n, s))
            })
            .map_err(sql_err)
    }

    /// Reads `cols` of every row in table (rowid) order, split into rowid
    /// ranges that separate connections read at once. `f` gets each
    /// range's rows in order and returns what it made of them.
    ///
    /// The whole scan reads one state of the table: a read transaction on
    /// the store's own connection holds SQLite's shared lock from the
    /// first read to the last, so no writer can commit between ranges.
    pub fn scan_parts<T: Send>(
        &self,
        cols: &str,
        f: &(dyn Fn(&mut rusqlite::Rows<'_>) -> Result<T, PwErr> + Sync),
    ) -> Result<Vec<T>, PwErr> {
        if self.no_table {
            return Ok(vec![]);
        }
        self.con.execute_batch("BEGIN").map_err(sql_err)?;
        let out = self.scan_locked(cols, f);
        let _ = self.con.execute_batch("ROLLBACK");
        out
    }

    fn scan_locked<T: Send>(
        &self,
        cols: &str,
        f: &(dyn Fn(&mut rusqlite::Rows<'_>) -> Result<T, PwErr> + Sync),
    ) -> Result<Vec<T>, PwErr> {
        let (lo, hi): (Option<i64>, Option<i64>) = self
            .con
            .query_row("SELECT min(rowid), max(rowid) FROM pathways", [], |r| Ok((r.get(0)?, r.get(1)?)))
            .map_err(sql_err)?;
        let (lo, hi) = match (lo, hi) {
            (Some(a), Some(b)) => (a, b),
            _ => return Ok(vec![]),
        };
        let span = (hi as i128) - (lo as i128) + 1;
        let q = format!("SELECT {cols} FROM pathways WHERE rowid BETWEEN ? AND ? ORDER BY rowid");
        if span < (READERS as i128) * 64 || span > (1i128 << 40) {
            let mut st = self.con.prepare(&q).map_err(sql_err)?;
            let mut rows = st.query([lo, hi]).map_err(sql_err)?;
            return Ok(vec![f(&mut rows)?]);
        }
        let span = span as i64;
        let parts = READERS as i64;
        let ranges: Vec<(i64, i64)> = (0..parts)
            .map(|k| {
                let a = lo + span * k / parts;
                let b = if k == parts - 1 { hi } else { lo + span * (k + 1) / parts - 1 };
                (a, b)
            })
            .collect();
        let path = self.path.clone();
        let read_only = self.read_only;
        let results: Vec<Result<T, PwErr>> = std::thread::scope(|sc| {
            let handles: Vec<_> = ranges
                .iter()
                .map(|&(a, b)| {
                    let q = &q;
                    let path = &path;
                    sc.spawn(move || -> Result<T, PwErr> {
                        let c = connect_with(path, read_only)?;
                        let mut st = c.prepare(q).map_err(sql_err)?;
                        let mut rows = st.query([a, b]).map_err(sql_err)?;
                        f(&mut rows)
                    })
                })
                .collect();
            handles
                .into_iter()
                .map(|h| h.join().unwrap_or_else(|_| Err(PwErr::Invalid("RuntimeError: a reader thread failed".into()))))
                .collect()
        });
        results.into_iter().collect()
    }

    /// `put()`: INSERT OR REPLACE of every column, each already in its
    /// stored form.
    pub fn put(&self, r: &PutRow) -> Result<(), PwErr> {
        self.con.execute(INSERT_SQL, &r.params()[..]).map_err(sql_err)?;
        Ok(())
    }

    /// Import's overwrite: the row that had this id deleted and the new
    /// one written, in one transaction. It reports whether a row went.
    pub fn replace(&mut self, r: &PutRow) -> Result<bool, PwErr> {
        let tx = self.con.transaction().map_err(sql_err)?;
        let n = tx.execute("DELETE FROM pathways WHERE id = ?", [&r.id]).map_err(sql_err)?;
        tx.execute(INSERT_SQL, &r.params()[..]).map_err(sql_err)?;
        tx.commit().map_err(sql_err)?;
        Ok(n > 0)
    }

    /// Whether the table holds any row.
    pub fn any(&self) -> Result<bool, PwErr> {
        self.con.query_row("SELECT EXISTS (SELECT 1 FROM pathways)", [], |r| r.get::<_, i64>(0)).map(|n| n != 0).map_err(sql_err)
    }

    /// Every column of the row with this rowid, by name.
    pub fn row_by_rowid(&self, rowid: i64) -> Result<Option<Row>, PwErr> {
        let mut st = self.con.prepare("SELECT * FROM pathways WHERE rowid = ?").map_err(sql_err)?;
        let names: Vec<String> = st.column_names().iter().map(|s| s.to_string()).collect();
        let mut rows = st.query([rowid]).map_err(sql_err)?;
        match rows.next().map_err(sql_err)? {
            None => Ok(None),
            Some(r) => {
                let mut out = Row::default();
                for (i, n) in names.iter().enumerate() {
                    out.cols.push((n.clone(), Col::from_ref(r.get_ref(i).map_err(sql_err)?)?));
                }
                Ok(Some(out))
            }
        }
    }
}

/// One row: each column by name, as the driver read it.
#[derive(Debug, Clone, Default)]
pub struct Row {
    pub cols: Vec<(String, Col)>,
}

impl Row {
    pub fn get(&self, name: &str) -> Option<&Col> {
        self.cols.iter().find(|(n, _)| n == name).map(|(_, c)| c)
    }
}

const INSERT_SQL: &str = "INSERT OR REPLACE INTO pathways \
    (id, task_description, task_embedding_json, envelope_json, \
    plan_template_json, source_trace_ids_json, \
    version, hit_count, distilled_at, \
    embedding_model, embedding_model_version, \
    last_activation_at, failure_count, \
    structure_signature, parameters_json) \
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)";

/// One row to write, each column in its stored form.
#[derive(Debug, Clone)]
pub struct PutRow {
    pub id: String,
    pub task: String,
    pub embedding: String,
    pub envelope: String,
    pub plan: String,
    pub traces: String,
    pub version: i64,
    pub hits: i64,
    pub distilled_at: f64,
    pub model: String,
    pub model_version: String,
    pub last_activation: f64,
    pub failures: i64,
    pub signature: Option<String>,
    pub parameters: String,
}

impl PutRow {
    fn params(&self) -> [&dyn rusqlite::ToSql; 15] {
        [
            &self.id,
            &self.task,
            &self.embedding,
            &self.envelope,
            &self.plan,
            &self.traces,
            &self.version,
            &self.hits,
            &self.distilled_at,
            &self.model,
            &self.model_version,
            &self.last_activation,
            &self.failures,
            &self.signature,
            &self.parameters,
        ]
    }
}
