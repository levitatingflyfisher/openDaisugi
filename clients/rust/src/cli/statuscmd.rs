//! `daisugi status`: day-one readiness (`onboarding.gather_status` and
//! the CLI's report of it). The Go client's `statuscmd.go` is the
//! reference; rulings C-12, C-13 and C-17 say where it differs from
//! Python.

use std::io::Read;
use std::process::{Command, Stdio};
use std::time::Duration;

use super::config::{self, RowsErr};
use super::configcmd::JSON_OPT;
use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Parsed, Res, Stop};
use crate::gate::argparse::py_float;
use crate::gate::py::text::{repr, splitlines, strip};
use crate::gate::pyjson::{dumps_indent, Object, Value};
use crate::pathways::store::{Col, Store};
use crate::tracejournal::Journal;

pub(super) const DATA_DIR_OPT: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");

/// `active_threshold` of each built matcher.
pub(super) fn matcher_threshold(key: &str) -> Option<f64> {
    match key {
        "all-MiniLM-L6-v2" => Some(0.55),
        "potion" => Some(0.59),
        "lexical" => Some(0.25),
        "int8" => Some(0.45),
        _ => None,
    }
}

/// Whether this binary carries the matcher a config key names.
fn carried(key: &str) -> bool {
    key == "lexical" || key == "potion"
}

/// `format(f, ".<n>f")`: Python writes nan and inf in lower case.
pub(super) fn py_fixed(f: f64, n: usize) -> String {
    if f.is_nan() {
        return "nan".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "inf".into() } else { "-inf".into() };
    }
    format!("{f:.n$}")
}

/// `round(f, 1)`.
pub(super) fn py_round1(f: f64) -> f64 {
    if !f.is_finite() {
        return f;
    }
    format!("{f:.1}").parse().unwrap_or(f)
}

/// `onboarding.StatusReport`.
struct Status {
    data_dir: String,
    search_installed: bool,
    pathway_count: i64,
    /// The hits as their int() text.
    pathway_hits: String,
    threshold: f64,
    total: i64,
    passed: i64,
    failed: i64,
    decompose_enabled: bool,
    plans_would_deny: usize,
    calls_would_deny: usize,
}

/// `journal.word_would_deny`: the traces whose verification result holds a
/// dialect audit warning. It never fails: a missing journal counts 0, and
/// a trace it cannot read, that is not UTF-8, or whose result.warnings is
/// not a list is skipped. A trace whose text does not hold the word
/// dialect is not loaded. A trace the port's reader does not handle (not
/// in the form yaml.safe_dump writes) is skipped too.
fn plans_would_deny(data_dir: &str) -> usize {
    let dir = format!("{data_dir}/journal/traces");
    let Ok(entries) = std::fs::read_dir(&dir) else { return 0 };
    let mut names: Vec<String> = entries
        .flatten()
        .filter_map(|e| e.file_name().into_string().ok())
        .filter(|n| n.ends_with(".yaml"))
        .collect();
    names.sort();
    let prefix = crate::dialect::AUDIT_PREFIX;
    let mut count = 0;
    for name in names {
        let Ok(raw) = std::fs::read(format!("{dir}/{name}")) else { continue };
        let Ok(text) = String::from_utf8(raw) else { continue };
        let text = text.replace("\r\n", "\n").replace('\r', "\n");
        if !text.contains("dialect") {
            continue;
        }
        let Ok(Value::Obj(o)) = crate::pathways::dumped::load_dumped(&text) else { continue };
        let Value::Obj(result) = o.value("result") else { continue };
        let Value::List(warnings) = result.value("warnings") else { continue };
        if warnings.iter().any(|w| matches!(w, Value::Str(s) if s.starts_with(prefix))) {
            count += 1;
        }
    }
    count
}

impl Env {
    /// A `--data-dir` option as pathlib prints it, or the default.
    pub(super) fn data_dir_of(&self, p: &Parsed) -> String {
        if p.has("--data-dir") {
            return path_str(&p.str("--data-dir", ""));
        }
        join(&self.home, ".opendaisugi")
    }

    pub(super) fn status_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "status";
        let opts = [
            DATA_DIR_OPT,
            Opt::val(&["--threshold"], "FLOAT", "Pathway retrieval threshold to display; default: the active backend's."),
            JSON_OPT,
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, "", "Show day-one readiness: are token savings live and are actions verified?", &opts);
        }
        let threshold = self.click_float(CMD, &p, "--threshold", 0.0)?;
        let mut rep = Status {
            data_dir: self.data_dir_of(&p),
            search_installed: false,
            pathway_count: 0,
            pathway_hits: "0".into(),
            threshold: 0.0,
            total: 0,
            passed: 0,
            failed: 0,
            decompose_enabled: false,
            plans_would_deny: 0,
            calls_would_deny: 0,
        };
        // The matcher comes from ~/.opendaisugi/config.yaml, as the
        // pathway commands read it. The binary carries lexical and potion
        // only; under any other matcher it reuses no pathway, so it says so
        // (C-12).
        let hcfg = config::load(&join(&self.home, ".opendaisugi/config.yaml"));
        let key = match &hcfg {
            Ok(c) => c.matcher_model.clone(),
            Err(_) => "lexical".to_string(),
        };
        rep.search_installed = carried(&key);
        if p.has("--threshold") {
            rep.threshold = threshold;
        } else {
            if let Err(e) = &hcfg {
                return Err(self.config_load_err(CMD, &RowsErr::Config(e.clone())));
            }
            match matcher_threshold(&key) {
                Some(t) => rep.threshold = t,
                None => {
                    self.errf(&format!("matcher_model={} is not a built embedder.\n", repr(&key)));
                    return exit(1);
                }
            }
        }
        status_pathways(&mut rep);
        status_journal(&mut rep);
        let cfg = match config::load(&format!("{}/config.yaml", rep.data_dir)) {
            Ok(c) => c,
            Err(e) => return Err(self.config_load_err(CMD, &RowsErr::Config(e))),
        };
        rep.decompose_enabled = cfg.shell_allow_decomposition;
        rep.plans_would_deny = plans_would_deny(&rep.data_dir);
        rep.calls_would_deny = match super::journal::word_would_deny(&join(&rep.data_dir, "gate")) {
            Ok(n) => n,
            Err(e) => return self.refuse(CMD, &e.to_string()),
        };
        let token_ready = rep.search_installed && rep.pathway_count > 0;
        let trust_ready = rep.total > 0;
        // The bash grammar is linked into this binary.
        let decompose_ready = rep.decompose_enabled;
        if p.flag("--json") {
            let o = Object::new()
                .with("data_dir", rep.data_dir.as_str())
                .with("search_extra_installed", rep.search_installed)
                .with("pathway_count", Value::Int(rep.pathway_count.to_string()))
                .with("pathway_hits", Value::Int(rep.pathway_hits.clone()))
                .with("retrieval_threshold", rep.threshold)
                .with("journal_total", Value::Int(rep.total.to_string()))
                .with("journal_passed", Value::Int(rep.passed.to_string()))
                .with("journal_failed", Value::Int(rep.failed.to_string()))
                .with("shell_decomposition_enabled", rep.decompose_enabled)
                .with("shell_grammar_installed", true)
                .with("word_would_deny_plans", rep.plans_would_deny as i64)
                .with("word_would_deny_calls", rep.calls_would_deny as i64)
                .with("token_savings_ready", token_ready)
                .with("trust_ready", trust_ready)
                .with("shell_decomposition_ready", decompose_ready);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        const OK: &str = "✓";
        const NO: &str = "✗";
        let mut b = format!("opendaisugi status (data dir: {})\n\n", rep.data_dir);
        b.push_str("Token savings (pathway routing):\n");
        if rep.search_installed {
            b.push_str(&format!("  {OK} [search] extra installed\n"));
        } else {
            b.push_str(&format!(
                "  {NO} matcher {key} is not in this binary — pathways disabled; set matcher_model: lexical or potion\n"
            ));
        }
        b.push_str(&format!("  • compiled pathways: {} ({} hits)\n", rep.pathway_count, rep.pathway_hits));
        b.push_str(&format!("  • retrieval threshold: {}\n", py_fixed(rep.threshold, 2)));
        if token_ready {
            b.push_str(&format!("  → {OK} token savings are LIVE\n"));
        } else {
            b.push_str(&format!("  → {NO} not yet — run `daisugi onboard`\n"));
        }
        b.push_str("\nTrust (verified actions):\n");
        b.push_str(&format!("  • journal traces: {} ({} verified, {} rejected)\n", rep.total, rep.passed, rep.failed));
        b.push_str("  • verification: strict at stakes high/physical (rejects unprovable invariants)\n");
        b.push_str(&format!(
            "  • dialect would-denies: {} plans in the journal, {} calls in the gate's audit log\n",
            rep.plans_would_deny, rep.calls_would_deny
        ));
        if !rep.decompose_enabled {
            b.push_str("  • compound shell (`a && b`): rejected outright (ADR-0010 opt-in is off)\n");
        } else {
            b.push_str("  • compound shell (`a && b`): decomposed, every head allowlist-checked\n");
        }
        if trust_ready {
            b.push_str(&format!("  → {OK} journal populated; replay any action with `daisugi journal replay <id>`\n"));
        } else {
            b.push_str(&format!("  → {NO} empty — run `daisugi onboard` or start capturing\n"));
        }
        b.push_str("\nLocal model (Tier-1 — cheap envelope generation):\n");
        let wired = self.tier1(&rep.data_dir)?;
        if !wired.is_empty() {
            b.push_str(&format!("  {OK} wired: {wired}\n"));
            b.push_str("  → onboard/tend defer bulk envelope generation to your local model\n");
            self.out(&b);
            return Ok(());
        }
        let budget = self.model_budget();
        b.push_str(&format!(
            "  {NO} none wired — hardware budget ~{}GB → recommends a {}-class model\n",
            py_fixed(budget, 0),
            size_class(budget)
        ));
        b.push_str("  → run `daisugi tiers setup` to pick, qualify, and wire a local model\n");
        self.out(&b);
        Ok(())
    }

    /// `load_configured_tier1(data_dir)`, worded as status prints it:
    /// "<model> @ <base_url>", or "" when none is wired. An unreadable or
    /// invalid file is ignored with a warning a logger with no handler
    /// drops (C-17).
    fn tier1(&mut self, data_dir: &str) -> Result<String, Stop> {
        const CMD: &str = "status";
        let path = format!("{data_dir}/local_tier1.json");
        if std::fs::metadata(&path).is_err() {
            return Ok(String::new());
        }
        let raw = match std::fs::read(&path) {
            Ok(r) => r,
            Err(_) => return Ok(String::new()),
        };
        let text = match String::from_utf8(raw) {
            Ok(t) => t,
            Err(_) => return Err(self.refuse(CMD, &format!("{path} is not UTF-8")).unwrap_err()),
        };
        let v = match load_json(&text) {
            Ok(v) => v,
            Err(JsonErr::Unread) => {
                return Err(self.refuse(CMD, &format!("{path} holds JSON this binary does not read yet")).unwrap_err())
            }
            Err(JsonErr::Raised(_)) => return Ok(String::new()),
        };
        let o = match v {
            Value::Obj(o) => o,
            _ => return Err(self.refuse(CMD, &format!("{path} does not hold a JSON object")).unwrap_err()),
        };
        let model = o.get("model").cloned().unwrap_or(Value::Null);
        if !model.truthy() {
            return Ok(String::new());
        }
        let mut m = match model {
            Value::Str(s) => s,
            _ => return Err(self.refuse(CMD, &format!("the model in {path} is not a string")).unwrap_err()),
        };
        let base = o.get("base_url").cloned().unwrap_or(Value::Null);
        let bs = match py_str_plain(&base) {
            Some(s) => s,
            None => return Err(self.refuse(CMD, &format!("the base_url in {path} is not a string")).unwrap_err()),
        };
        if base != Value::Null && !m.contains('/') {
            m = format!("openai/{m}");
        }
        Ok(format!("{m} @ {bs}"))
    }

    /// `detect_hardware().model_budget_gb` on Linux: 80% of the first
    /// GPU's memory as nvidia-smi reports it, else 60% of MemTotal. torch
    /// is not consulted (C-13).
    fn model_budget(&self) -> f64 {
        let vram = self.gpu_mem_gb();
        if vram > 0.0 {
            return py_round1(vram * 0.8);
        }
        match ram_gb() {
            Some(r) if r != 0.0 => py_round1(r * 0.6),
            _ => 0.0,
        }
    }

    /// `_detect_gpu`'s memory: 0 when there is no GPU it reads.
    fn gpu_mem_gb(&self) -> f64 {
        self.gpu_probe().0
    }

    /// `_detect_gpu`'s nvidia-smi probe: the first line's memory in GiB
    /// and the GPU's name, or 0 and None when there is no nvidia-smi or it
    /// fails.
    pub(super) fn gpu_probe(&self) -> (f64, Option<String>) {
        let Some(smi) = super::install::look_path(&self.env, "nvidia-smi") else { return (0.0, None) };
        let child = Command::new(&smi)
            .args(["--query-gpu=memory.total,name", "--format=csv,noheader,nounits"])
            .env_clear()
            .envs(&self.env)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn();
        let Ok(mut child) = child else { return (0.0, None) };
        let mut pipe = child.stdout.take();
        let reader = std::thread::spawn(move || {
            let mut b = vec![];
            if let Some(p) = pipe.as_mut() {
                let _ = p.read_to_end(&mut b);
            }
            b
        });
        let ok = wait_timeout(child, Duration::from_secs(5)).is_some_and(|s| s.success());
        let out = reader.join().unwrap_or_default();
        if !ok {
            return (0.0, None);
        }
        let Ok(text) = String::from_utf8(out) else { return (0.0, None) };
        let text = strip(&text);
        if text.is_empty() {
            return (0.0, None);
        }
        let first = splitlines(text)[0];
        let Some((mem, name)) = first.split_once(',') else { return (0.0, None) };
        match py_float(strip(mem)) {
            Some(f) => (py_round1(f / 1024.0), Some(strip(name).to_string())),
            None => (0.0, None),
        }
    }
}

/// Waits for a child for at most `limit`: its status, or None when it
/// failed or ran out of time, when it is killed.
pub(super) fn wait_timeout(mut child: std::process::Child, limit: Duration) -> Option<std::process::ExitStatus> {
    let pid = child.id() as libc::pid_t;
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait());
    });
    match rx.recv_timeout(limit) {
        Ok(Ok(s)) => Some(s),
        Ok(Err(_)) => None,
        Err(_) => {
            // SAFETY: kill() sends a signal and touches no memory; the
            // waiting thread still holds the child, so the pid is not
            // reused before it is reaped.
            unsafe {
                libc::kill(pid, libc::SIGKILL);
            }
            None
        }
    }
}

/// The store's count and hits. A store that cannot be read counts as
/// empty: gather_status logs a warning through a logger with no handler,
/// so nothing is printed (C-17).
fn status_pathways(rep: &mut Status) {
    let db = format!("{}/pathways.db", rep.data_dir);
    if std::fs::metadata(&db).is_err() {
        return;
    }
    let Ok(s) = Store::open(&db) else { return };
    let Ok((count, hits)) = s.stats() else { return };
    rep.pathway_count = count;
    match hits {
        Col::Int(h) => rep.pathway_hits = h.to_string(),
        Col::Real(h) => {
            // int() of the float SUM; int() of nan or inf raises after
            // the count is read: the count is kept and the hits stay 0.
            if !h.is_finite() {
                return;
            }
            rep.pathway_hits = format!("{:.0}", h.trunc());
        }
        _ => {}
    }
}

/// The journal's counts, read read-only: a missing journal is not made
/// and reads as empty. A journal that cannot be read counts as empty,
/// silently.
fn status_journal(rep: &mut Status) {
    let Ok(j) = Journal::open_read_only(&rep.data_dir) else { return };
    let Ok(st) = j.stats() else { return };
    rep.total = st.total;
    rep.passed = st.passed;
    rep.failed = st.failed;
}

/// `_detect_ram_gb` without psutil: MemTotal from /proc/meminfo.
pub(super) fn ram_gb() -> Option<f64> {
    let raw = std::fs::read_to_string("/proc/meminfo").ok()?;
    let line = raw.lines().find(|l| l.starts_with("MemTotal:"))?;
    let kb: i64 = line.split_whitespace().nth(1)?.parse().ok()?;
    Some(py_round1((kb * 1024) as f64 / 1e9))
}

/// `recommend_model`'s size label for a budget.
fn size_class(budget: f64) -> &'static str {
    if budget < 3.0 {
        "≤1B"
    } else if budget < 6.0 {
        "~3B"
    } else if budget < 12.0 {
        "~8B"
    } else if budget < 24.0 {
        "~14B"
    } else {
        "~32B"
    }
}

/// str(v) for the JSON values whose str is plain.
pub(super) fn py_str_plain(v: &Value) -> Option<String> {
    match v {
        Value::Null => Some("None".into()),
        Value::Bool(true) => Some("True".into()),
        Value::Bool(false) => Some("False".into()),
        Value::Int(t) => Some(t.clone()),
        Value::Str(s) => Some(s.clone()),
        _ => None,
    }
}

/// Why `load_json` gave no value.
pub(super) enum JsonErr {
    /// json.loads raises; the text is its str().
    Raised(String),
    /// JSON this binary does not read the way Python does (nested too
    /// deep, or a value it does not keep).
    Unread,
}

/// `json.loads(text)`, nesting past 900 levels refused.
pub(super) fn load_json(text: &str) -> Result<Value, JsonErr> {
    use crate::gate::pyjson::{loads, py_decode_error, LoadError, PyDecodeFail};
    match py_decode_error(text, 900) {
        Some(PyDecodeFail::Raised(m)) => return Err(JsonErr::Raised(m)),
        Some(PyDecodeFail::TooDeep) => return Err(JsonErr::Unread),
        None => {}
    }
    match loads(text) {
        Ok(v) => Ok(v),
        Err(LoadError::Syntax(m)) => Err(JsonErr::Raised(m.to_string())),
        Err(_) => Err(JsonErr::Unread),
    }
}
