//! `daisugi onboard`: existing agent transcripts found, parsed (with the
//! split cache), ingested as verified traces, and distilled by tend, with
//! the flags, output, exit codes and files of the Python CLI. The Go
//! client's `cli/onboardcmd.go` is the reference; rulings K3-10 to K3-12
//! say where it differs from Python.

use std::collections::HashSet;

use rusqlite::{Connection, OpenFlags};

use super::config;
use super::gardencmd::now_seconds;
use super::gatecmds::DECOMPOSE_OPT;
use super::gateroot::{join, path_str};
use super::journalparse::{now_iso, trace_id_ok, Item, SPLIT_DEFAULT_MODEL};
use super::statuscmd::load_json;
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::{dumps, dumps_indent, Object, Value};
use crate::pathways::pmodel::{self, Loc, Mode};
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;
use crate::tracejournal::{self, Journal};
use crate::transcript::{self, Fail};

/// `parsers.claude_code.SPLIT_PROMPT_VERSION`.
const SPLIT_PROMPT_VERSION: &str = "2026-08-03";

const SPLIT_CACHE_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS split_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    boundaries_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_split_prompt_version ON split_cache(prompt_version);
";

fn sha256_hex(s: &str) -> String {
    crate::gate::sha256::hexdigest(s.as_bytes())
}

fn split_key(model: &str, content: &str) -> String {
    sha256_hex(&format!(
        "version:{SPLIT_PROMPT_VERSION}\nmodel:{model}\ncontent:{content}"
    ))
}

/// `split_cache.SplitCache`: rows read from the database when the run
/// starts, and rows the run adds, written when the run writes.
struct SplitCache {
    path: String,
    rows: std::collections::HashMap<String, String>,
    pending: Vec<(String, String, f64)>,
}

impl SplitCache {
    /// Reads the rows of the current prompt version, without writing: a
    /// row of another version is evicted when the cache is made.
    fn read(path: &str) -> Result<SplitCache, String> {
        let mut c = SplitCache {
            path: path.to_string(),
            rows: Default::default(),
            pending: vec![],
        };
        if std::fs::metadata(path).is_err() {
            return Ok(c);
        }
        let db = Connection::open_with_flags(
            path,
            OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
        )
        .map_err(|e| e.to_string())?;
        let mut st = match db
            .prepare("SELECT cache_key, boundaries_json FROM split_cache WHERE prompt_version = ?")
        {
            Ok(st) => st,
            Err(e) if e.to_string().contains("no such table") => return Ok(c),
            Err(e) => return Err(e.to_string()),
        };
        let mut rows = st
            .query([SPLIT_PROMPT_VERSION])
            .map_err(|e| e.to_string())?;
        while let Some(r) = rows.next().map_err(|e| e.to_string())? {
            use rusqlite::types::ValueRef;
            match (
                r.get_ref(0).map_err(|e| e.to_string())?,
                r.get_ref(1).map_err(|e| e.to_string())?,
            ) {
                (ValueRef::Text(k), ValueRef::Text(b)) => {
                    let (Ok(k), Ok(b)) = (std::str::from_utf8(k), std::str::from_utf8(b)) else {
                        return Err("a split cache row this binary does not read".into());
                    };
                    c.rows.insert(k.to_string(), b.to_string());
                }
                _ => return Err("a split cache row this binary does not read".into()),
            }
        }
        Ok(c)
    }

    fn get(&self, model: &str, content: &str) -> Result<Option<Value>, String> {
        let Some(text) = self.rows.get(&split_key(model, content)) else {
            return Ok(None);
        };
        match load_json(text) {
            Ok(v) => Ok(Some(v)),
            Err(_) => Err("a split cache row that does not read as JSON".into()),
        }
    }

    fn put(&mut self, v: &Value, model: &str, content: &str) {
        let k = split_key(model, content);
        let text = dumps(v, true);
        self.rows.insert(k.clone(), text.clone());
        self.pending.push((k, text, now_seconds()));
    }

    /// Makes the cache (its schema, the stale rows evicted) and adds the
    /// run's rows.
    fn write(&self) -> Result<(), String> {
        if let Some(parent) = std::path::Path::new(&self.path).parent() {
            std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
        }
        let db = Connection::open(&self.path).map_err(|e| e.to_string())?;
        db.execute_batch(SPLIT_CACHE_SCHEMA)
            .map_err(|e| e.to_string())?;
        db.execute(
            "DELETE FROM split_cache WHERE prompt_version != ?",
            [SPLIT_PROMPT_VERSION],
        )
        .map_err(|e| e.to_string())?;
        for (k, b, at) in &self.pending {
            db.execute(
                "INSERT OR REPLACE INTO split_cache (cache_key, prompt_version, boundaries_json, inserted_at) VALUES (?, ?, ?, ?)",
                rusqlite::params![k, SPLIT_PROMPT_VERSION, b, at],
            )
            .map_err(|e| e.to_string())?;
        }
        Ok(())
    }
}

/// `onboarding.DiscoveredTranscript`.
struct Discovered {
    path: String,
    harness: String,
    mtime: f64,
}

/// `discover_transcripts` under one root: every regular *.jsonl file
/// (journal.jsonl aside), walked in name order without following linked
/// directories.
fn walk(
    dir: &str,
    harness: &str,
    seen: &mut HashSet<String>,
    found: &mut Vec<Discovered>,
    root: bool,
) -> Result<(), String> {
    let rd = match std::fs::read_dir(dir) {
        Ok(rd) => rd,
        Err(e) if root => return Err(e.to_string()),
        Err(_) => return Ok(()),
    };
    let mut names: Vec<(String, bool)> = vec![];
    for ent in rd.flatten() {
        let Ok(name) = ent.file_name().into_string() else {
            continue;
        };
        let is_dir = ent.file_type().map(|t| t.is_dir()).unwrap_or(false);
        names.push((name, is_dir));
    }
    names.sort();
    for (name, is_dir) in names {
        let p = format!("{}/{name}", dir.trim_end_matches('/'));
        if is_dir {
            walk(&p, harness, seen, found, false)?;
            continue;
        }
        if !name.ends_with(".jsonl") || name == "journal.jsonl" {
            continue;
        }
        let Ok(md) = std::fs::metadata(&p) else {
            continue;
        };
        if !md.is_file() {
            continue;
        }
        let Ok(resolved) = std::fs::canonicalize(&p) else {
            continue;
        };
        let resolved = resolved.to_string_lossy().into_owned();
        if seen.contains(&resolved) || md.len() == 0 {
            continue;
        }
        seen.insert(resolved);
        let mtime = md
            .modified()
            .ok()
            .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
            .map(|d| d.as_nanos() as f64 / 1e9)
            .unwrap_or(0.0);
        found.push(Discovered {
            path: path_str(&p),
            harness: harness.to_string(),
            mtime,
        });
    }
    Ok(())
}

/// `discover_transcripts`: once each by resolved path, newest first.
fn discover(roots: &[(String, String)]) -> Result<Vec<Discovered>, String> {
    let mut seen = HashSet::new();
    let mut found = vec![];
    for (harness, root) in roots {
        if !std::fs::metadata(root).map(|m| m.is_dir()).unwrap_or(false) {
            continue;
        }
        walk(root, harness, &mut seen, &mut found, true)?;
    }
    found.sort_by(|a, b| {
        b.mtime
            .partial_cmp(&a.mtime)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    Ok(found)
}

/// Python's `seq[:limit]` length.
fn py_slice_len(n: usize, limit: i128) -> usize {
    let n_i = n as i128;
    let end = if limit < 0 {
        (n_i + limit).max(0)
    } else {
        limit.min(n_i)
    };
    end as usize
}

/// One transcript's parse and ingest, worked out before anything is
/// written.
struct Run {
    t: Discovered,
    episodes: usize,
    items: Vec<Item>,
    /// Set when the transcript is skipped (no parser, or a parse that
    /// raised).
    warning: String,
    processed: bool,
}

impl Env {
    fn expand_user(&self, p: &str) -> String {
        if p == "~" {
            return self.home.clone();
        }
        match p.strip_prefix("~/") {
            Some(rest) => format!("{}/{rest}", self.home),
            None => p.to_string(),
        }
    }

    /// `default_transcript_roots`: the harness roots, in order, with
    /// OPENDAISUGI_TRANSCRIPT_ROOTS on top.
    pub(super) fn transcript_roots(&self) -> Result<Vec<(String, String)>, String> {
        let mut roots = vec![
            (
                "claude-code".to_string(),
                join(&self.home, ".claude/projects"),
            ),
            ("codex".to_string(), join(&self.home, ".codex/sessions")),
        ];
        let raw = self
            .env
            .get("OPENDAISUGI_TRANSCRIPT_ROOTS")
            .cloned()
            .unwrap_or_default();
        let env = strip(&raw);
        if env.is_empty() {
            return Ok(roots);
        }
        for entry in env.split(':') {
            let entry = strip(entry);
            if entry.is_empty() {
                continue;
            }
            let (mut harness, mut path) = ("custom".to_string(), entry.to_string());
            if let Some((h, p)) = entry.split_once('=') {
                harness = strip(h).to_string();
                path = strip(p).to_string();
                if harness.is_empty() {
                    harness = "custom".into();
                }
            }
            if path.starts_with('~') && path != "~" && !path.starts_with("~/") {
                return Err("a transcript root under another user's home (~user)".into());
            }
            let path = path_str(&self.expand_user(&path));
            match roots.iter_mut().find(|(h, _)| *h == harness) {
                Some(r) => r.1 = path,
                None => roots.push((harness, path)),
            }
        }
        Ok(roots)
    }

    /// `get_parser(format, ...).parse(path)` with the split cache: the
    /// finalized episodes, or `Ok(Err(text))` for the exception the parse
    /// raised (onboard warns and goes on), or `Err` for input this binary
    /// does not read the oracle's way.
    #[allow(clippy::too_many_arguments)]
    fn parse_for_onboard(
        &self,
        path: &str,
        format: &str,
        min_tools: i128,
        max_tools: i128,
        model: &str,
        via_api: bool,
        cache: &mut Option<SplitCache>,
        neutral: &mut Option<String>,
    ) -> Result<Result<Vec<Value>, String>, String> {
        let raw = std::fs::read(path).map_err(|e| e.to_string())?;
        let eps = match transcript::read(&raw, format) {
            Ok(e) => e,
            Err(Fail::Parse(m)) => return Ok(Err(m)),
            Err(Fail::Unreadable(why)) => return Err(why),
        };
        let eps = transcript::merge_small(eps, min_tools);
        let split = transcript::split_large(eps, max_tools, &mut |content: &str| {
            if let Some(c) = cache.as_ref() {
                match c.get(model, content) {
                    Ok(Some(v)) => return Ok(Some(v)),
                    Ok(None) => {}
                    Err(why) => return Err(Fail::Unreadable(why)),
                }
            }
            let v = if via_api {
                self.split_api(model, content)?
            } else {
                self.split_claude(content, neutral)?
            };
            if let (Some(v), Some(c)) = (&v, cache.as_mut()) {
                c.put(v, model, content);
            }
            Ok(v)
        });
        match split {
            Ok(eps) => match transcript::finalize(&eps) {
                Ok(v) => Ok(Ok(v)),
                Err(Fail::Parse(m)) => Ok(Err(m)),
                Err(Fail::Unreadable(why)) => Err(why),
            },
            Err(Fail::Parse(m)) => Ok(Err(m)),
            Err(Fail::Unreadable(why)) => Err(why),
        }
    }

    /// `ingest_episodes` up to the journal writes: each episode's status,
    /// with what a real run journals for it.
    #[allow(clippy::too_many_arguments)]
    fn prepare_ingest(
        &mut self,
        cmd: &str,
        source_file: &str,
        episodes: &[Value],
        traces_dir: &str,
        decompose: bool,
        dry: bool,
        preview_max: i128,
        parser: &mut ShellParser,
    ) -> Result<Vec<Item>, Stop> {
        let sum = sha256_hex(source_file);
        let mut items = vec![];
        for x in episodes {
            let ep = x.as_obj().cloned().unwrap_or_default();
            let mut it = Item {
                id: ep.value("id").as_str().unwrap_or("").to_string(),
                task: ep.value("task").as_str().unwrap_or("").to_string(),
                ..Default::default()
            };
            // The parser's episodes hold step models: a plan made of them
            // dumps every field, defaults included.
            let raw = match ep.value("steps") {
                Value::List(l) => l.clone(),
                _ => vec![],
            };
            let mut steps = Vec::with_capacity(raw.len());
            for s in &raw {
                let typ = s
                    .as_obj()
                    .and_then(|o| o.value("type").as_str())
                    .unwrap_or("")
                    .to_string();
                let Some(m) = pmodel::steps().get(&typ) else {
                    return Err(self
                        .refuse(
                            cmd,
                            &format!("a step of type {} this binary does not read", repr(&typ)),
                        )
                        .unwrap_err());
                };
                let (v, errs) = m.validate(s, &Loc::Root, Mode::Python);
                if !errs.is_empty() {
                    let e = pmodel::ValidationError {
                        title: m.name.into(),
                        errs,
                    };
                    let first = e.text().split('\n').next().unwrap_or("").to_string();
                    return Err(self
                        .refuse(cmd, &format!("a step that does not validate ({first})"))
                        .unwrap_err());
                }
                steps.push(v);
            }
            it.steps = steps.len();
            it.trace_id = format!("import-{}-{}", &sum[..8], it.id);
            match tracejournal::load_trace(traces_dir, &it.trace_id) {
                Ok(Ok(_)) => {
                    it.status = "SKIP".into();
                    items.push(it);
                    continue;
                }
                Ok(Err(le)) if le.typ == "FileNotFoundError" => {}
                Ok(Err(le)) => {
                    return Err(self
                        .refuse(
                            cmd,
                            &format!(
                                "a journal trace this binary does not read ({}: {})",
                                le.typ, le.msg
                            ),
                        )
                        .unwrap_err())
                }
                Err(e) => {
                    return Err(self
                        .refuse(
                            cmd,
                            &format!("a journal trace this binary does not read ({e})"),
                        )
                        .unwrap_err())
                }
            }
            if dry && it.steps as i128 > preview_max {
                it.status = "TOO-LARGE".into();
                items.push(it);
                continue;
            }
            if !trace_id_ok(&it.trace_id) {
                return Err(self
                    .refuse(
                        cmd,
                        &format!("a trace id the journal rejects ({})", it.trace_id),
                    )
                    .unwrap_err());
            }
            self.ingest_one(cmd, &mut it, &steps, decompose, dry, parser)?;
            items.push(it);
        }
        Ok(items)
    }

    pub(super) fn onboard(&mut self, args: &[String]) -> Res {
        const CMD: &str = "onboard";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(&["--model"], "TEXT", "Model for episode splitting + distillation (envelopes are inferred, no LLM)."),
            Opt::val(&["--llm"], "TEXT", "LLM backend: api | claude-code. Default: auto-detect."),
            Opt::val(&["--limit"], "INTEGER", "Process only the N most recent transcripts."),
            Opt::many(&["--harness"], "TEXT", "Only process these harnesses (repeatable). Default: all discovered."),
            Opt::val(&["--min-tools"], "INTEGER", "Merge episodes below this tool-call count."),
            Opt::val(&["--max-tools"], "INTEGER", "LLM-split episodes above this tool-call count."),
            Opt::val(&["--min-traces"], "INTEGER", "Minimum cluster size to distill a pathway."),
            Opt::val(
                &["--lookback-days"],
                "INTEGER",
                "How far back to scan ingested traces when distilling (default: all history).",
            ),
            Opt::val(&["--threshold"], "FLOAT", "Pathway clustering/retrieval similarity threshold (0-1)."),
            Opt::flag(
                &["--dry-run"],
                "Preview: discover and deterministically verify each episode; makes NO model calls and writes no traces or pathways.",
            ),
            Opt::flag(&["--allow-no-embedder"], "Onboard without the pathway embedder."),
            DECOMPOSE_OPT,
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Turn existing conversations into token-saving pathways — the day-one flow.",
                &opts,
            );
        }
        let limit = if p.has("--limit") {
            Some(self.click_int(CMD, &p, "--limit", 0)?)
        } else {
            None
        };
        let min_tools = self.click_int(CMD, &p, "--min-tools", 3)?;
        let max_tools = self.click_int(CMD, &p, "--max-tools", 30)?;
        let min_traces = self.click_int(CMD, &p, "--min-traces", 3)?;
        let lookback = self.click_int(CMD, &p, "--lookback-days", 3650)?;
        let threshold = if p.has("--threshold") {
            Some(self.click_float(CMD, &p, "--threshold", 0.0)?)
        } else {
            None
        };
        if p.has("--llm") {
            let v = p.str("--llm", "");
            self.check_llm_flag(&v)?;
            self.env.insert("OPENDAISUGI_LLM_BACKEND".into(), v);
        }
        let dry = p.flag("--dry-run");
        let as_json = p.flag("--json");
        let model = p.str("--model", SPLIT_DEFAULT_MODEL);
        let data_dir = path_str(&p.str("--data-dir", &join(&self.home, ".opendaisugi")));
        self.renamed_backend_at(CMD, &data_dir)?;
        if self.env.contains_key("OPENDAISUGI_CONFORMANCE_RECORD") && !dry {
            return self.not_yet("daisugi onboard with OPENDAISUGI_CONFORMANCE_RECORD set");
        }
        // Everything below, up to the first write, only reads: a refusal
        // changes nothing. The oracle checks the matcher in effect: a key
        // that nothing builds turns pathways off. A matcher the oracle
        // builds and this binary does not is refused.
        let notes = std::rc::Rc::new(std::cell::RefCell::new(String::new()));
        let pick = match self.matcher_pick(&notes) {
            Ok(p) => p,
            Err(why) => return self.refuse(CMD, &why),
        };
        // A dry run's note goes to stderr, after the resolved line, so a
        // --json report on stdout stays valid JSON.
        let mut dry_note = String::new();
        if pick.matcher.is_none() {
            let fix = "  Set matcher_model: lexical or potion in config.yaml";
            if dry {
                dry_note = format!(
                    "note: matcher_model {} is not a built embedder, so a real onboard would distil NO token-saving pathways.\n{fix}\n",
                    repr(&pick.key)
                );
            } else if !p.flag("--allow-no-embedder") {
                if let Err(why) = self.echo_resolved_at(&data_dir) {
                    return self.refuse(CMD, &why);
                }
                self.errf(&format!(
                    "onboard needs the pathway embedder to turn traces into token-saving pathways, but matcher_model {} \
                     is not a built embedder. Without it this run would spend model tokens on envelope generation and \
                     distil ZERO pathways.\n{fix}\n  Or build only the verified journal (no pathways):  --allow-no-embedder\n",
                    repr(&pick.key)
                ));
                return exit(2);
            }
        }
        let c = self.llm_client();
        let via_api = c.backend() != "claude-code";
        if !dry {
            if let Err(why) = c.check(&model) {
                return self.refuse(CMD, &why);
            }
        }
        let mut decompose = p.flag("--allow-shell-decomposition");
        if !p.flag_set("--allow-shell-decomposition") {
            match config::load(&join(&data_dir, "config.yaml")) {
                Ok(c) => decompose = c.shell_allow_decomposition,
                Err(e) => return Err(self.config_load_err(CMD, &config::RowsErr::Config(e))),
            }
        }
        let tier1 = format!("{data_dir}/local_tier1.json");
        if std::fs::metadata(&tier1).is_ok() {
            // load_configured_tier1 reads it; the facade's Tier-1 slot is
            // not used by tend, so only a config the oracle raises on
            // matters.
            let ok = std::fs::read(&tier1)
                .map(|b| String::from_utf8(b).is_ok())
                .unwrap_or(false);
            if !ok {
                return self.refuse(CMD, "a Tier-1 config this binary does not read");
            }
        }
        let mut cache = if dry {
            None
        } else {
            match SplitCache::read(&format!("{data_dir}/split_cache.db")) {
                Ok(c) => Some(c),
                Err(why) => return self.refuse(CMD, &why),
            }
        };
        let roots = match self.transcript_roots() {
            Ok(r) => r,
            Err(why) => return self.refuse(CMD, &why),
        };
        let mut found = match discover(&roots) {
            Ok(f) => f,
            Err(why) => return self.refuse(CMD, &why),
        };
        let hs = p.list("--harness");
        if !hs.is_empty() {
            found.retain(|t| hs.contains(&t.harness));
        }
        let n_found = found.len();
        if let Some(l) = limit {
            found.truncate(py_slice_len(found.len(), l));
        }
        let eff_max = if dry { 1_000_000_000 } else { max_tools };
        let traces_dir = format!("{data_dir}/journal/traces");
        let mut runs: Vec<Run> = vec![];
        let mut neutral: Option<String> = None;
        let mut parser = ShellParser::new();
        let mut stop: Option<Stop> = None;
        for t in found {
            let mut r = Run {
                t,
                episodes: 0,
                items: vec![],
                warning: String::new(),
                processed: false,
            };
            if r.t.harness != "claude-code" && r.t.harness != "codex" {
                let name = r.t.path.rsplit('/').next().unwrap_or("").to_string();
                r.warning = format!("no parser for harness {}: {name}", repr(&r.t.harness));
                runs.push(r);
                continue;
            }
            let parsed = self.parse_for_onboard(
                &r.t.path,
                &r.t.harness.clone(),
                min_tools,
                eff_max,
                &model,
                via_api,
                &mut cache,
                &mut neutral,
            );
            let eps = match parsed {
                Err(why) => {
                    stop = Some(
                        self.refuse(CMD, &format!("{}: {why}", r.t.path))
                            .unwrap_err(),
                    );
                    break;
                }
                Ok(Err(perr)) => {
                    r.warning = format!("parse failed for {}: {perr}", r.t.path);
                    runs.push(r);
                    continue;
                }
                Ok(Ok(eps)) => eps,
            };
            r.processed = true;
            r.episodes = eps.len();
            let path = r.t.path.clone();
            match self.prepare_ingest(
                CMD,
                &path,
                &eps,
                &traces_dir,
                decompose,
                dry,
                max_tools,
                &mut parser,
            ) {
                Ok(items) => r.items = items,
                Err(s) => {
                    stop = Some(s);
                    break;
                }
            }
            runs.push(r);
        }
        if let Some(d) = &neutral {
            let _ = std::fs::remove_dir_all(d);
        }
        if let Some(s) = stop {
            return Err(s);
        }
        // The writes, in the oracle's order: the journal, the split cache,
        // each transcript's traces, then tend.
        if let Err(why) = self.echo_resolved_at(&data_dir) {
            return self.refuse(CMD, &why);
        }
        self.errf(&dry_note);
        let j = Journal::open(&data_dir).map_err(|e| self.after_writes(CMD, e))?;
        if let Some(c) = &cache {
            if let Err(why) = c.write() {
                return Err(self.after_writes(CMD, PwErr::Invalid(why)));
            }
        }
        let mut warnings: Vec<String> = vec![];
        let mut by_harness = Object::new();
        let (
            mut processed,
            mut episodes,
            mut passed,
            mut failed,
            mut skipped,
            mut preview,
            mut errored,
        ) = (0usize, 0usize, 0usize, 0usize, 0usize, 0usize, 0usize);
        let mut first_error = String::new();
        let say = |e: &mut Env, msg: &str| {
            if !as_json {
                e.note(msg);
            }
        };
        if runs.is_empty() {
            warnings.push(
                "no transcripts discovered — check OPENDAISUGI_TRANSCRIPT_ROOTS or your harness data directory".into(),
            );
            say(
                self,
                "onboard: no transcripts discovered; nothing to distill",
            );
        } else {
            say(
                self,
                &format!("onboard: processing {} transcript(s)", runs.len()),
            );
            for r in &runs {
                if !r.processed {
                    warnings.push(r.warning.clone());
                    continue;
                }
                processed += 1;
                let n = match by_harness.get(&r.t.harness) {
                    Some(Value::Int(t)) => t.parse::<i64>().unwrap_or(0),
                    _ => 0,
                };
                by_harness.set(&r.t.harness, n + 1);
                episodes += r.episodes;
                let mut p1 = 0;
                for it in &r.items {
                    if !dry && (it.status == "OK" || it.status == "FAIL") {
                        if let Err(e) = j.log(
                            &it.task,
                            &it.env,
                            &it.plan,
                            &it.result,
                            &it.trace_id,
                            &now_iso(),
                        ) {
                            return Err(self.after_writes(CMD, e));
                        }
                    }
                    match it.status.as_str() {
                        "OK" => {
                            passed += 1;
                            p1 += 1;
                        }
                        "FAIL" => failed += 1,
                        "SKIP" => skipped += 1,
                        "TOO-LARGE" => preview += 1,
                        "ERROR" => {
                            errored += 1;
                            if first_error.is_empty() {
                                if let Some(e) = &it.err {
                                    first_error = e.clone();
                                }
                            }
                        }
                        _ => {}
                    }
                }
                let name = r.t.path.rsplit('/').next().unwrap_or("");
                say(
                    self,
                    &format!("onboard: {name} -> {} episode(s) ({p1} passed)", r.episodes),
                );
            }
            if processed > 0 && errored > 0 && passed == 0 && failed == 0 {
                let mut hint = format!(
                    "onboard: envelope generation failed for all {errored} processed episode(s) and no traces were \
                     logged — is your model backend configured? Set OPENDAISUGI_LLM_BACKEND=claude-code for no-API-key \
                     mode, or provide an API key for your model."
                );
                if !first_error.is_empty() {
                    hint.push_str(&format!(" First error: {first_error}"));
                }
                warnings.push(hint.clone());
                say(self, &hint);
            }
        }
        drop(j);
        let (mut created, mut updated) = (0usize, 0usize);
        if !runs.is_empty() {
            if dry {
                say(
                    self,
                    "onboard: dry-run — skipping distillation (no pathways written)",
                );
            } else {
                say(self, "onboard: distilling pathways (tend)…");
                let rep =
                    self.run_tend(&data_dir, &model, min_traces, lookback, false, threshold)?;
                created = rep.rep.created as usize;
                updated = rep.rep.updated as usize;
                warnings.extend(rep.rep.warnings.iter().cloned());
                say(
                    self,
                    &format!("onboard: distilled {created} new pathway(s), {updated} updated"),
                );
            }
        }
        let int = |n: usize| Value::Int(n.to_string());
        if as_json {
            let report = Object::new()
                .with("transcripts_found", int(n_found))
                .with("transcripts_processed", int(processed))
                .with("by_harness", Value::Obj(by_harness))
                .with("episodes_total", int(episodes))
                .with("traces_passed", int(passed))
                .with("traces_failed", int(failed))
                .with("traces_skipped", int(skipped))
                .with("traces_preview_skipped", int(preview))
                .with("traces_errored", int(errored))
                .with("pathways_created", int(created))
                .with("pathways_updated", int(updated))
                .with("dry_run", dry)
                .with(
                    "warnings",
                    Value::List(warnings.into_iter().map(Value::Str).collect()),
                );
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(report), 2, true)));
            return Ok(());
        }
        let mut b = String::from("\n");
        let mut keys: Vec<String> = by_harness.keys().to_vec();
        keys.sort();
        let parts: Vec<String> = keys
            .iter()
            .map(|k| {
                format!(
                    "{k}: {}",
                    super::statuscmd::py_str_plain(by_harness.value(k)).unwrap_or_default()
                )
            })
            .collect();
        let by = if parts.is_empty() {
            "—".to_string()
        } else {
            parts.join(", ")
        };
        b.push_str(&format!(
            "Discovered {n_found} transcript(s); processed {processed} ({by}).\n"
        ));
        if dry {
            let previewed = passed + failed;
            b.push_str("Preview — deterministic verify, no model calls, nothing written:\n");
            let mut line = format!(
                "  {previewed} episode(s) previewed: {passed} would verify, {failed} would fail"
            );
            if skipped > 0 {
                line.push_str(&format!("; {skipped} already present"));
            }
            b.push_str(&format!("{line}\n"));
            if preview > 0 {
                b.push_str(&format!(
                    "  {preview} more episode(s) too large to preview here — a real onboard LLM-splits each into \
                     sub-episodes (which mostly verify), so the counts above cover only the {previewed} that don't need \
                     splitting.\n"
                ));
            }
            if !decompose && failed > 0 {
                b.push_str(
                    "  tip: many failures are compound shells (a && b) rejected without decomposition — add \
                     --allow-shell-decomposition to preview them as they'd run.\n",
                );
            }
            b.push_str("Dry run — re-run without --dry-run to journal + distill.\n");
        } else {
            b.push_str(&format!(
                "Journal: {passed} verified, {failed} failed, {skipped} already present.\n"
            ));
            b.push_str(&format!("Pathways: {created} new, {updated} updated.\n"));
            if created > 0 || updated > 0 {
                b.push_str("  → Token routing is live: matching tasks now skip envelope generation (Tier-0).\n");
            }
            b.push_str(&format!(
                "  → Trust: replay any action with `daisugi journal replay <id>`; journal at {}.\n",
                join(&data_dir, "journal")
            ));
        }
        for w in &warnings {
            b.push_str(&format!("  warning: {w}\n"));
        }
        self.out(&b);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_limit_slices_as_python_does() {
        assert_eq!(py_slice_len(3, 1), 1);
        assert_eq!(py_slice_len(3, 0), 0);
        assert_eq!(py_slice_len(3, -1), 2);
        assert_eq!(py_slice_len(3, -9), 0);
        assert_eq!(py_slice_len(3, 9), 3);
    }

    #[test]
    fn transcripts_are_found_newest_first_and_once() {
        let d = std::env::temp_dir().join(format!("onboard-walk-{}", std::process::id()));
        let root = d.join("r");
        std::fs::create_dir_all(root.join("b")).unwrap();
        std::fs::write(root.join("a.jsonl"), "x").unwrap();
        std::fs::write(root.join("b/c.jsonl"), "x").unwrap();
        std::fs::write(root.join("journal.jsonl"), "x").unwrap();
        std::fs::write(root.join("empty.jsonl"), "").unwrap();
        std::os::unix::fs::symlink(root.join("a.jsonl"), root.join("z.jsonl")).unwrap();
        let roots = vec![(
            "claude-code".to_string(),
            root.to_string_lossy().into_owned(),
        )];
        let found = discover(&roots).unwrap();
        let names: Vec<String> = found
            .iter()
            .map(|f| f.path.rsplit('/').next().unwrap().to_string())
            .collect();
        let _ = std::fs::remove_dir_all(&d);
        assert_eq!(names.len(), 2, "{names:?}");
        assert!(names.contains(&"a.jsonl".to_string()) && names.contains(&"c.jsonl".to_string()));
    }
}
