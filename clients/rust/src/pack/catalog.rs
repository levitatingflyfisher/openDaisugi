//! `opendaisugi.pack.catalog`: the pinned Python and the packs, each with
//! its lock file of hashed requirements. The built-in catalog and locks
//! are the repository's own files (`packs/`), included at build time.

use crate::gate::pyjson::{loads, Object, Value};

/// The catalog's pinned CPython.
#[derive(Debug, Clone, Default)]
pub struct Python {
    pub version: String,
    pub file: String,
    pub url: String,
    pub sha256: String,
    pub size: u64,
}

/// One catalog entry.
#[derive(Debug, Clone, Default)]
pub struct Pack {
    pub name: String,
    pub gpu: bool,
    pub summary: String,
    pub lock: String,
    pub index_url: String,
    pub extra_index_urls: Vec<String>,
    pub check: Vec<String>,
}

/// `packs/catalog.json`. `dir` holds the locks; None is the built-in one.
#[derive(Debug, Clone, Default)]
pub struct Catalog {
    pub protocol: String,
    pub platform: String,
    pub python: Python,
    pub packs: Vec<Pack>,
    pub dir: Option<String>,
}

/// One pinned requirement.
#[derive(Debug, Clone, PartialEq)]
pub struct Req {
    pub name: String,
    pub version: String,
    pub hashes: Vec<String>,
}

fn s(o: &Object, k: &str) -> String {
    o.get(k)
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

fn strs(o: &Object, k: &str) -> Vec<String> {
    match o.get(k) {
        Some(Value::List(l)) => l
            .iter()
            .filter_map(|v| v.as_str().map(str::to_string))
            .collect(),
        _ => vec![],
    }
}

impl Catalog {
    /// The catalog at `path`, or the built-in one.
    pub fn load(path: Option<&str>) -> Result<Catalog, String> {
        let text = match path {
            Some(p) => std::fs::read_to_string(p).map_err(|e| e.to_string())?,
            None => super::CATALOG_JSON.to_string(),
        };
        let v = loads(&text).map_err(|_| "the pack catalog is not JSON".to_string())?;
        let o = v.as_obj().ok_or("the pack catalog is not a JSON object")?;
        let py = o
            .get("python")
            .and_then(|v| v.as_obj())
            .cloned()
            .unwrap_or_default();
        let size = match py.get("size") {
            Some(Value::Int(t)) => t.parse().unwrap_or(0),
            _ => 0,
        };
        let mut packs = vec![];
        if let Some(Value::List(l)) = o.get("packs") {
            for p in l.iter().filter_map(|v| v.as_obj()) {
                packs.push(Pack {
                    name: s(p, "name"),
                    gpu: matches!(p.get("gpu"), Some(Value::Bool(true))),
                    summary: s(p, "summary"),
                    lock: s(p, "lock"),
                    index_url: s(p, "index_url"),
                    extra_index_urls: strs(p, "extra_index_urls"),
                    check: strs(p, "check"),
                });
            }
        }
        Ok(Catalog {
            protocol: s(o, "protocol"),
            platform: s(o, "platform"),
            python: Python {
                version: s(&py, "version"),
                file: s(&py, "file"),
                url: s(&py, "url"),
                sha256: s(&py, "sha256"),
                size,
            },
            packs,
            dir: path.map(|p| {
                std::path::Path::new(p)
                    .parent()
                    .map(|d| d.to_string_lossy().into_owned())
                    .unwrap_or_default()
            }),
        })
    }

    pub fn find(&self, name: &str) -> Option<&Pack> {
        self.packs.iter().find(|p| p.name == name)
    }

    /// The pack's lock file.
    pub fn lock_text(&self, p: &Pack) -> Result<String, String> {
        match &self.dir {
            Some(d) => std::fs::read_to_string(std::path::Path::new(d).join(&p.lock))
                .map_err(|e| e.to_string()),
            None => match p.lock.as_str() {
                "train.lock" => Ok(super::TRAIN_LOCK.to_string()),
                "vla-ref.lock" => Ok(super::VLA_REF_LOCK.to_string()),
                other => Err(format!("no built-in lock {other}")),
            },
        }
    }
}

/// The PEP 503 form of a project name.
pub fn normalize(name: &str) -> String {
    let mut out = String::new();
    let mut sep = false;
    for c in name.chars() {
        if c == '-' || c == '_' || c == '.' {
            sep = true;
            continue;
        }
        if sep {
            out.push('-');
            sep = false;
        }
        out.extend(c.to_lowercase());
    }
    if sep {
        out.push('-');
    }
    out
}

fn clip(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

fn hash_arg(w: &str) -> Option<String> {
    let h = w.strip_prefix("--hash=sha256:")?;
    (h.len() == 64
        && h.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
    .then(|| h.to_string())
}

/// `catalog.parse_lock`.
pub fn parse_lock(text: &str) -> Result<Vec<Req>, String> {
    let mut logical = vec![];
    let mut cur = String::new();
    for raw in text.lines() {
        let mut line = raw.trim();
        if line.starts_with('#') {
            continue;
        }
        if let Some(i) = line.find(" #") {
            line = &line[..i];
        }
        let line = line.trim_end();
        if let Some(head) = line.strip_suffix('\\') {
            cur.push_str(head);
            cur.push(' ');
            continue;
        }
        cur.push_str(line);
        if !cur.trim().is_empty() {
            logical.push(cur.trim().to_string());
        }
        cur.clear();
    }
    if !cur.trim().is_empty() {
        logical.push(cur.trim().to_string());
    }
    let mut out = vec![];
    for ln in logical {
        let words: Vec<&str> = ln.split_whitespace().collect();
        let head = words[0];
        if !head.contains("==") || ln.contains(';') {
            return Err(format!("not one pinned version: {}", clip(&ln, 80)));
        }
        let (name, version) = head.split_once("==").unwrap_or_default();
        let mut hashes = vec![];
        for w in &words[1..] {
            match hash_arg(w) {
                Some(h) => hashes.push(h),
                None => return Err(format!("not a sha256 hash: {}", clip(w, 80))),
            }
        }
        if hashes.is_empty() || name.is_empty() || version.is_empty() {
            return Err(format!("no sha256 hash: {}", clip(&ln, 80)));
        }
        out.push(Req {
            name: normalize(name),
            version: version.to_string(),
            hashes,
        });
    }
    Ok(out)
}

/// `catalog.wheel_key`.
pub fn wheel_key(filename: &str) -> Option<(String, String)> {
    let stem = filename.strip_suffix(".whl")?;
    let parts: Vec<&str> = stem.split('-').collect();
    if parts.len() != 5 && parts.len() != 6 {
        return None;
    }
    Some((normalize(parts[0]), parts[1].to_string()))
}
