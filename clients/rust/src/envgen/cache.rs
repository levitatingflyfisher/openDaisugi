//! `envelope_cache.py`: `make_cache_key` and `EnvelopeCache`, one SQLite
//! file whose rows of another prompt version are evicted when it opens.

use rusqlite::{params, Connection, OpenFlags};

use super::prompts_gen::PROMPT_VERSION;
use crate::gate::pyjson::{Object, Value};
use crate::pathways::pathway::dump_json;
use crate::pathways::pmodel::{validate_json, Id};

/// The inputs `make_cache_key` hashes. `context` and `parent` None are
/// Python's None; `tier1` None leaves the tier1 line out, so a Tier-2 key
/// is the key older releases wrote.
pub struct KeyArgs<'a> {
    pub task: &'a str,
    pub context: Option<&'a str>,
    pub model: &'a str,
    pub parent: Option<&'a str>,
    pub summarize: bool,
    pub thinking: &'a str,
    pub tier1: Option<&'a str>,
}

/// `make_cache_key`: the SHA-256 of the lines, not normalized.
pub fn cache_key(a: &KeyArgs) -> String {
    let mut lines = vec![
        format!("task:{}", a.task),
        format!("context:{}", a.context.unwrap_or("")),
        format!("model:{}", a.model),
        format!("parent:{}", a.parent.unwrap_or("")),
        format!("summarize:{}", if a.summarize { "1" } else { "0" }),
        format!("thinking:{}", a.thinking),
    ];
    if let Some(t) = a.tier1 {
        lines.push(format!("tier1:{t}"));
    }
    crate::gate::sha256::hexdigest(lines.join("\n").as_bytes())
}

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS envelope_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_version ON envelope_cache(prompt_version);
";

/// Why a cache call failed.
#[derive(Debug)]
pub enum CacheErr {
    /// A cached envelope that does not validate: `get` raises pydantic's
    /// ValidationError, which the caller does not catch.
    Row(String),
    /// The file could not be opened or read.
    Sql(String),
}

/// `EnvelopeCache(path, prompt_version=PROMPT_VERSION)`.
pub struct Cache {
    path: String,
    /// How many rows the open evicted.
    pub evicted: usize,
}

fn sql(e: rusqlite::Error) -> CacheErr {
    CacheErr::Sql(e.to_string())
}

impl Cache {
    fn open_db(&self) -> Result<Connection, CacheErr> {
        // sqlite3.connect reads a path, never a URI.
        let dsn = if self.path.starts_with("file:") { format!("./{}", self.path) } else { self.path.clone() };
        Connection::open_with_flags(
            dsn,
            OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_CREATE | OpenFlags::SQLITE_OPEN_NO_MUTEX,
        )
        .map_err(sql)
    }

    /// Opens (and makes) the cache and evicts the rows of another prompt
    /// version.
    pub fn open(path: &str) -> Result<Cache, CacheErr> {
        if path.contains('?') {
            return Err(CacheErr::Sql("a cache path with '?' is not read the way Python reads it".into()));
        }
        if let Some(parent) = std::path::Path::new(path).parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent).map_err(|e| CacheErr::Sql(e.to_string()))?;
            }
        }
        let mut c = Cache { path: path.to_string(), evicted: 0 };
        let db = c.open_db()?;
        db.execute_batch(SCHEMA).map_err(sql)?;
        c.evicted = db.execute("DELETE FROM envelope_cache WHERE prompt_version != ?", [PROMPT_VERSION]).map_err(sql)?;
        Ok(c)
    }

    /// `EnvelopeCache.get` for a key: the envelope, or None on a miss.
    pub fn get(&self, key: &str) -> Result<Option<Object>, CacheErr> {
        let db = self.open_db()?;
        let r = db.query_row("SELECT envelope_json FROM envelope_cache WHERE cache_key = ?", [key], |r| {
            Ok(match r.get_ref(0)? {
                rusqlite::types::ValueRef::Text(b) => Some(String::from_utf8_lossy(b).into_owned()),
                _ => None,
            })
        });
        let text = match r {
            Ok(Some(t)) => t,
            Ok(None) => return Err(CacheErr::Row("a cached envelope is not text".into())),
            Err(rusqlite::Error::QueryReturnedNoRows) => return Ok(None),
            Err(e) => return Err(sql(e)),
        };
        match validate_json(Id::Envelope, &text) {
            Ok(Value::Obj(o)) => Ok(Some(o)),
            Ok(_) => Err(CacheErr::Row("a cached envelope is not an object".into())),
            Err(e) => Err(CacheErr::Row(e.text())),
        }
    }

    /// `EnvelopeCache.put`: best effort, a failed write is dropped.
    pub fn put(&self, env: &Object, key: &str) {
        let Ok(db) = self.open_db() else { return };
        let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0);
        let _ = db.execute(
            "INSERT OR REPLACE INTO envelope_cache (cache_key, prompt_version, envelope_json, inserted_at) VALUES (?, ?, ?, ?)",
            params![key, PROMPT_VERSION, dump_json(&Value::Obj(env.clone())), now],
        );
    }

    /// `get_inserted_at`: the row's inserted_at, or None on a miss.
    pub fn inserted_at(&self, key: &str) -> Result<Option<f64>, CacheErr> {
        let db = self.open_db()?;
        match db.query_row("SELECT inserted_at FROM envelope_cache WHERE cache_key = ?", [key], |r| r.get::<_, f64>(0)) {
            Ok(v) => Ok(Some(v)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(sql(e)),
        }
    }

    /// `EnvelopeCache.invalidate`: best effort.
    pub fn invalidate(&self, key: &str) -> bool {
        let Ok(db) = self.open_db() else { return false };
        db.execute("DELETE FROM envelope_cache WHERE cache_key = ?", [key]).map(|n| n > 0).unwrap_or(false)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The key tests/test_envelope_cache.py pins for these inputs is the
    /// SHA-256 of these exact lines.
    #[test]
    fn a_key_is_the_hash_of_its_lines() {
        let a = KeyArgs { task: "t", context: None, model: "m", parent: None, summarize: false, thinking: "standard", tier1: None };
        assert_eq!(cache_key(&a), crate::gate::sha256::hexdigest(b"task:t\ncontext:\nmodel:m\nparent:\nsummarize:0\nthinking:standard"));
        let b = KeyArgs { tier1: Some("http:x"), ..a };
        assert_ne!(cache_key(&b), cache_key(&a));
    }
}
