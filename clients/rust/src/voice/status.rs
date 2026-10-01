//! `daisugi voice status` (`opendaisugi.voice.status`): the engine, its
//! model, and a fetch under way. Read from disk; it never asks a server.

use std::time::SystemTime;

use super::engine::{py_path_str, resolve_engine};
use super::moonshine::{moonshine_dir, ModelEnv, MOONSHINE_MODELS};
use super::parakeet::{parakeet_dir, PARAKEET_MODELS};
use crate::gate::pyjson::{self, Value};

/// What this build calls itself in voice status.
pub const STATUS_KIND: &str = "rust";
/// A partial download not written for this long is a fetch that stopped
/// (ruling VO-R-1).
pub const STATUS_FRESH_S: f64 = 30.0;

#[derive(Debug, PartialEq)]
pub struct Status {
    pub engine: String,
    pub model: String,
    pub model_bytes: u64,
    pub fetched: bool,
    pub fetch: Option<(u64, u64)>,
}

fn file_size(p: &str) -> Option<u64> {
    let m = std::fs::metadata(p).ok()?;
    m.is_file().then(|| m.len())
}

/// The largest partial file of `path`, and whether any is fresh.
fn part_sizes(path: &str, now: SystemTime) -> (u64, bool) {
    let (dir, base) = match path.rfind('/') {
        Some(i) => (&path[..=i], &path[i + 1..]),
        None => ("./", path),
    };
    let (mut best, mut fresh) = (0u64, false);
    let Ok(rd) = std::fs::read_dir(dir) else { return (0, false) };
    for e in rd.flatten() {
        let n = e.file_name().to_string_lossy().to_string();
        if n != format!("{base}.part") && !n.starts_with(&format!("{base}.part-")) {
            continue;
        }
        let Ok(m) = std::fs::metadata(e.path()) else { continue };
        best = best.max(m.len());
        let age = m.modified().ok().and_then(|t| now.duration_since(t).ok()).map(|d| d.as_secs_f64());
        // A file written in the future counts as fresh, as the oracle's clock does.
        if age.map_or(true, |a| a <= STATUS_FRESH_S) {
            fresh = true;
        }
    }
    (best, fresh)
}

fn progress(dir: &str, files: &[(&str, u64)], now: SystemTime, also: &[&str]) -> Option<(u64, u64)> {
    let (mut done, mut total, mut active) = (0u64, 0u64, false);
    for (name, size) in files {
        total += size;
        let path = format!("{dir}/{name}");
        if file_size(&path) == Some(*size) {
            done += size;
            continue;
        }
        let (part, fresh) = part_sizes(&path, now);
        done += part.min(*size);
        active = active || fresh;
    }
    for name in also {
        active = active || part_sizes(&format!("{dir}/{name}"), now).1;
    }
    active.then_some((done, total))
}

fn path_bytes(p: &str) -> (u64, bool) {
    let Ok(m) = std::fs::metadata(p) else { return (0, false) };
    if m.is_file() {
        return (m.len(), true);
    }
    if !m.is_dir() {
        return (0, false);
    }
    let mut total = 0;
    if let Ok(rd) = std::fs::read_dir(p) {
        for e in rd.flatten() {
            if let Ok(m) = std::fs::metadata(e.path()) {
                if m.is_file() {
                    total += m.len();
                }
            }
        }
    }
    (total, true)
}

/// `status.voice_status` for this binary. `now` is the clock.
pub fn voice_status(engine: &str, model: &str, env: &ModelEnv, now: SystemTime) -> Status {
    let (engine, model) = resolve_engine(engine, model, env, &mut |_| {});
    let mut out = Status { engine: engine.clone(), model: model.clone(), model_bytes: 0, fetched: false, fetch: None };
    let moon = MOONSHINE_MODELS.iter().find(|(n, _)| *n == model).map(|(_, f)| *f);
    let para = PARAKEET_MODELS.iter().find(|(n, _)| *n == model).map(|(_, m)| m);
    match (engine.as_str(), moon, para) {
        ("moonshine", Some(files), _) => {
            let dir = moonshine_dir(&model, env);
            let pinned: Vec<(&str, u64)> = files.iter().map(|f| (f.name, f.size)).collect();
            out.model_bytes = pinned.iter().map(|(_, s)| s).sum();
            out.fetched = pinned.iter().all(|(n, s)| file_size(&format!("{dir}/{n}")) == Some(*s));
            out.fetch = progress(&dir, &pinned, now, &[]);
        }
        ("parakeet", _, Some(m)) => {
            let dir = parakeet_dir(&model, env);
            out.model_bytes = m.quantized.size;
            out.fetched = file_size(&format!("{dir}/{}", m.quantized.name)) == Some(m.quantized.size);
            if !out.fetched {
                out.fetch = progress(&dir, &[(m.source.name, m.source.size)], now, &[m.quantized.name]);
            }
        }
        ("moonshine" | "parakeet" | "whisper.cpp", _, _) => {
            (out.model_bytes, out.fetched) = path_bytes(&py_path_str(&model));
        }
        _ => {}
    }
    out
}

fn mb(n: u64) -> u64 {
    (n + 500_000) / 1_000_000
}

impl Status {
    /// `status.describe`: the one line voice status prints without --json.
    pub fn describe(&self) -> String {
        if let Some((done, total)) = self.fetch {
            return format!("Getting the {} model, {} of {} MB.", self.engine, mb(done), mb(total));
        }
        let what = format!("The {} {} model", self.engine, self.model);
        if self.fetched {
            format!("{what} is on this box ({} MB).", mb(self.model_bytes))
        } else {
            format!("{what} is not on this box yet.")
        }
    }

    /// `json.dumps` of the status dict, in the oracle's key order.
    pub fn json(&self) -> String {
        let mut out = format!(
            "{{\"engine\": {}, \"model\": {}, \"model_bytes\": {}, \"fetched\": {}, \"kind\": \"{STATUS_KIND}\"",
            pyjson::dumps(&Value::Str(self.engine.clone()), true),
            pyjson::dumps(&Value::Str(self.model.clone()), true),
            self.model_bytes,
            self.fetched
        );
        if let Some((done, total)) = self.fetch {
            out.push_str(&format!(", \"fetch\": {{\"done\": {done}, \"total\": {total}}}"));
        }
        out.push('}');
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::time::Duration;

    fn env(home: &std::path::Path) -> ModelEnv {
        let mut vars = HashMap::new();
        vars.insert("XDG_CACHE_HOME".to_string(), format!("{}/cache", home.display()));
        ModelEnv { vars, home: home.display().to_string(), hardware: None, timeouts: Default::default() }
    }

    fn sparse(path: &str, size: u64) {
        std::fs::create_dir_all(std::path::Path::new(path).parent().unwrap()).unwrap();
        std::fs::File::create(path).unwrap().set_len(size).unwrap();
    }

    fn scratch(tag: &str) -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!("status-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn kind_is_rust_and_empty_cache_is_not_fetched() {
        let d = scratch("empty");
        let st = voice_status("moonshine", "tiny", &env(&d), SystemTime::now());
        assert!(st.json().contains("\"kind\": \"rust\""));
        assert!(!st.fetched && st.fetch.is_none());
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn a_fresh_part_is_a_fetch_and_a_stale_one_is_not() {
        let d = scratch("part");
        let e = env(&d);
        sparse(&format!("{}/encoder.ort.part-ab12", moonshine_dir("tiny", &e)), 500);
        let st = voice_status("moonshine", "tiny", &e, SystemTime::now());
        assert_eq!(st.fetch.map(|f| f.0), Some(500));
        let later = SystemTime::now() + Duration::from_secs(3600);
        assert!(voice_status("moonshine", "tiny", &e, later).fetch.is_none());
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn fetched_when_every_size_matches() {
        let d = scratch("full");
        let e = env(&d);
        for (n, files) in MOONSHINE_MODELS {
            if *n == "tiny" {
                for f in *files {
                    sparse(&format!("{}/{}", moonshine_dir("tiny", &e), f.name), f.size);
                }
            }
        }
        let st = voice_status("moonshine", "tiny", &e, SystemTime::now());
        assert!(st.fetched && st.fetch.is_none());
        std::fs::remove_dir_all(&d).ok();
    }
}
