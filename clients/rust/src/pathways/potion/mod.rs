//! The model2vec matcher of ADR-0018 (potion-base-8M by default): model2vec's
//! `StaticModel.encode`, with the tokenizer ported, and no Python and no
//! tokenizers library.
//!
//! The default model is fetched only on first use, with one line on
//! stderr saying so, from one pinned revision of the official repository,
//! each file checked against its SHA-256 below, into
//! `$XDG_CACHE_HOME/opendaisugi/models/potion-base-8M` (`~/.cache` when
//! unset). A model that cannot be had is no match, never a fallback to
//! another matcher.

pub mod fetch;
pub mod model;
pub mod tables;
pub mod tokenizer;

use std::collections::HashMap;

pub use model::Model;

/// `_search._POTION_DEFAULT`, the identity pathways embedded with the
/// default model carry.
pub const DEFAULT_MODEL: &str = "minishlab/potion-base-8M";

/// `_search._POTION_THRESHOLD`, FPR-matched in ADR-0018.
pub const THRESHOLD: f64 = 0.59;

/// The default model's files, pinned to one revision of the official
/// repository. model.safetensors's sha256 is the LFS oid the Hugging Face
/// tree API reports; the two small files are below the LFS threshold, so
/// theirs is taken from the downloaded bytes. The model is MIT licensed
/// (its model card). The Go client pins the same three values.
pub const PINNED_REVISION: &str = "bf8b056651a2c21b8d2565580b8569da283cab23";

pub struct Pinned {
    pub name: &'static str,
    pub sha256: &'static str,
    pub size: u64,
}

pub const PINNED: &[Pinned] = &[
    Pinned { name: "config.json", sha256: "2a6ac0e9aaa356a68a5688070db78fc3a464fefe85d2f06a1905ce3718687553", size: 202 },
    Pinned {
        name: "tokenizer.json",
        sha256: "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50",
        size: 683_666,
    },
    Pinned {
        name: "model.safetensors",
        sha256: "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2",
        size: 30_236_760,
    },
];

pub fn pinned_base() -> String {
    format!("https://huggingface.co/minishlab/potion-base-8M/resolve/{PINNED_REVISION}/")
}

/// `MatcherNotAvailable`: the operator chose potion and its model cannot
/// be had. It is never a fallback to another matcher.
#[derive(Debug)]
pub struct NotAvailable(pub String);

/// What model resolution reads from the process.
pub struct Env {
    pub vars: HashMap<String, String>,
    pub home: String,
    /// Says the one line before a download starts.
    pub notice: Box<dyn Fn(&str)>,
    /// Refuses to download; a missing model is then not available.
    pub no_fetch: bool,
    /// Where the pinned files come from (the tests point it at a local
    /// server).
    pub base: String,
}

impl Env {
    /// The process's own environment, with the notice on stderr.
    pub fn from_process(vars: HashMap<String, String>, home: String) -> Env {
        Env { vars, home, notice: Box::new(|s| eprintln!("{s}")), no_fetch: false, base: pinned_base() }
    }
}

/// `resolve_potion_model()`: OPENDAISUGI_POTION_MODEL, or the default id.
/// It is the identity pathways are stamped with.
pub fn resolve_model(e: &Env) -> String {
    e.vars.get("OPENDAISUGI_POTION_MODEL").cloned().unwrap_or_else(|| DEFAULT_MODEL.to_string())
}

/// Where the pinned files live: $XDG_CACHE_HOME (or ~/.cache)
/// /opendaisugi/models/potion-base-8M.
pub fn cache_dir(e: &Env) -> String {
    let base = match e.vars.get("XDG_CACHE_HOME") {
        Some(b) if b.starts_with('/') => b.clone(),
        _ => format!("{}/.cache", e.home.trim_end_matches('/')),
    };
    format!("{}/opendaisugi/models/potion-base-8M", base.trim_end_matches('/'))
}

/// Loads the model an identity names. A local directory is read as it
/// is. The default id is read from the cache, fetched first (with one
/// notice line) when a pinned file is missing or does not match its hash.
/// Any other Hugging Face id is not available: this binary fetches only
/// pinned files.
pub fn open(identity: &str, e: &Env) -> Result<Model, NotAvailable> {
    if std::fs::metadata(identity).map(|m| m.is_dir()).unwrap_or(false) {
        return Model::load_dir(identity)
            .map_err(|why| NotAvailable(format!("potion model {} could not be loaded ({why})", go_quote(identity))));
    }
    if identity != DEFAULT_MODEL {
        return Err(NotAvailable(format!(
            "potion model {} is not a local directory, and this binary fetches only the pinned {DEFAULT_MODEL}",
            go_quote(identity)
        )));
    }
    let dir = cache_dir(e);
    for f in PINNED {
        let url = format!("{}{}", e.base, f.name);
        if let Err(why) = fetch::fetch(&url, f, &format!("{dir}/{}", f.name), e) {
            return Err(NotAvailable(format!(
                "potion model {} could not be fetched: {why}. Pre-fetch it and set \
                 OPENDAISUGI_POTION_MODEL=<local dir>, or set matcher_model: lexical for a zero-download matcher",
                go_quote(identity)
            )));
        }
    }
    // The bytes loaded are hashed again: a file changed after the fetch
    // checked it is refused, not used.
    let mut data = vec![];
    for f in PINNED {
        let b = std::fs::read(format!("{dir}/{}", f.name)).map_err(|err| NotAvailable(err.to_string()))?;
        if crate::gate::sha256::hexdigest(&b) != f.sha256 {
            return Err(NotAvailable(format!("{} changed after it was checked", f.name)));
        }
        data.push(b);
    }
    Model::load(&data[0], &data[1], &data[2])
        .map_err(|why| NotAvailable(format!("potion files in {dir} could not be loaded: {why}")))
}

/// Go's `%q` of a plain string, as the Go binary words these lines.
fn go_quote(s: &str) -> String {
    format!("{s:?}")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::{loads, Value};

    fn fixtures() -> String {
        concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/pathways").to_string()
    }

    #[test]
    fn the_tiny_model_matches_the_oracle() {
        let m = Model::load_dir(&format!("{}/potion-tiny", fixtures())).unwrap();
        let u = loads(&std::fs::read_to_string(format!("{}/units.json", fixtures())).unwrap()).unwrap();
        let o = u.as_obj().unwrap();
        let Value::List(toks) = o.value("tiny_tokens") else { panic!() };
        assert!(toks.len() > 30);
        for t in toks {
            let t = t.as_obj().unwrap();
            let text = t.value("text").as_str().unwrap();
            let Value::List(ids) = t.value("ids") else { panic!() };
            let want: Vec<usize> = ids
                .iter()
                .map(|i| match i {
                    Value::Int(x) => x.parse().unwrap(),
                    _ => panic!(),
                })
                .collect();
            assert_eq!(m.tok.encode(text), want, "{text:?}");
        }
        let Value::List(vecs) = o.value("tiny_vectors") else { panic!() };
        for v in vecs {
            let v = v.as_obj().unwrap();
            let text = v.value("text").as_str().unwrap();
            let Value::List(want) = v.value("vec") else { panic!() };
            let got = m.encode(text);
            assert_eq!(got.len(), want.len());
            for (g, w) in got.iter().zip(want) {
                let w = match w {
                    Value::Float(f) => *f,
                    _ => panic!(),
                };
                assert!((g - w).abs() < 1e-6, "{text:?}: {g} vs {w}");
            }
        }
    }

    #[test]
    fn an_unpinned_id_is_not_available() {
        let e = Env::from_process(HashMap::new(), "/nonexistent".into());
        match open("someone/else-model", &e) {
            Err(NotAvailable(why)) => assert!(why.contains("fetches only the pinned"), "{why}"),
            Ok(_) => panic!("an unpinned id loaded"),
        }
    }
}
