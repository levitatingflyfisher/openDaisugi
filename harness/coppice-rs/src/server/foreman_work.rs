//! A worker is one `daisugi tree run` the foreman's spawn tool started. The
//! server is the tree's starter for it (it proves the edge, reserves the
//! budget and runs sprig), so the foreman has one acting tool and the gate
//! stays outside it. A worker's final report comes back as one `work`
//! message. A work message starts no turn. The twin of Go's
//! foreman_work.go.

use std::collections::BTreeMap;
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use super::foreman_chat::{first_line, write_0600, ForemanChat};
use super::foreman_turn::{run_cmd, CmdOpts};
use super::Server;
use crate::config;
use crate::foreman::chat as fchat;
use crate::foreman::ctx::Ctx;
use crate::gojson;

/// The worker states.
pub const WORK_RUNNING: &str = "running";
pub(super) const WORK_RAN: &str = "ran";
const WORK_REFUSED: &str = "refused";
const WORK_ASKED: &str = "asked";
pub(super) const WORK_FAILED: &str = "failed";
const WORK_STOPPED: &str = "stopped";
/// Pool workers (FM-R-93): the job waits for an entry, or its process group
/// was stopped to make room.
pub(super) const WORK_QUEUED: &str = "queued";
pub(super) const WORK_PAUSED: &str = "paused";

/// The tools every worker gets.
const WORKER_TOOLS: [&str; 4] = ["read", "write", "edit", "bash"];

/// How long a stopped worker has to end its node before its group is
/// killed.
const WORKER_GRACE: Duration = Duration::from_secs(5);

/// How much of a worker's own log zoom(label) returns: the newest bytes.
const WORKER_LOG_BYTES: u64 = 32 << 10;

/// One worker in the table, as workers.json keeps it.
#[derive(Clone, Debug, Default, serde::Deserialize)]
#[serde(default)]
pub struct WorkerRec {
    pub label: String,
    pub project: String,
    pub dir: String,
    pub profile: String,
    pub model: String,
    pub started: String,
    pub state: String,
    pub session: String,
    pub session_file: String,
    /// True when sprig ran under the fence: its session log is then in the
    /// work dir's sprig-sessions, outside the gate root (FN-R-25).
    pub fenced: bool,
    /// Set for a worker whose profile names a pool: the pool, and the entry
    /// it runs on (empty while it waits).
    pub pool: String,
    pub entry: String,
    pub background: bool,
}

impl WorkerRec {
    fn to_value(&self) -> Value {
        let mut v = vec![
            ("label", json!(self.label)),
            ("project", json!(self.project)),
            ("dir", json!(self.dir)),
            ("profile", json!(self.profile)),
            ("model", json!(self.model)),
            ("started", json!(self.started)),
            ("state", json!(self.state)),
        ];
        if !self.session.is_empty() {
            v.push(("session", json!(self.session)));
        }
        if !self.session_file.is_empty() {
            v.push(("session_file", json!(self.session_file)));
        }
        if self.fenced {
            v.push(("fenced", json!(true)));
        }
        if !self.pool.is_empty() {
            v.push(("pool", json!(self.pool)));
        }
        if !self.entry.is_empty() {
            v.push(("entry", json!(self.entry)));
        }
        if self.background {
            v.push(("background", json!(true)));
        }
        gojson::obj(v)
    }
}

/// The worker table of the server.
#[derive(Default)]
pub struct WorkState {
    pub work: Vec<WorkerRec>,
    /// Numbers the workers of each project.
    pub next: BTreeMap<String, i64>,
    pub loaded: bool,
    pub ctx: Option<Ctx>,
    /// The pools and the queue (foreman_pool.rs).
    pub pool: super::foreman_pool::PoolRuntime,
}

/// The operator's file for one project: the envelope its workers run in
/// and the budget of its root.
pub(super) struct ProjectEnvelope {
    /// The envelope as the file has it, word for word.
    envelope: String,
    tokens: i64,
    turns: i64,
}

/// Whether v can be the value of a flag the model chose: the characters of
/// a profile name or a model id, and no leading dash.
/// A fenced worker's tree run argv: --sprig-fence before the closing "-",
/// so a daisugi that cannot apply the fence refuses the run (FN-R-28).
fn require_sprig_fence(args: &mut Vec<String>) {
    let dash = args.pop();
    args.push("--sprig-fence".to_string());
    args.extend(dash);
}

fn flag_value(v: &str) -> bool {
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r"^[A-Za-z0-9._:/@+][A-Za-z0-9._:/@+-]*$").unwrap())
        .is_match(v)
}

pub(super) fn envelope_dir() -> PathBuf {
    config::path()
        .parent()
        .map(Path::to_path_buf)
        .unwrap_or_default()
        .join("envelopes")
}

/// Go's filepath.Base.
pub(super) fn base(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return "/".into();
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

fn dir_exists(p: &str) -> bool {
    !p.is_empty() && std::fs::metadata(p).is_ok_and(|m| m.is_dir())
}

pub(super) fn read_project_envelope(path: &Path, name: &str) -> Result<ProjectEnvelope, String> {
    let no = || {
        format!(
            "no envelope for the project {name}. Write one at {}: {{\"envelope\": {{...}}, \"tokens\": N, \"turns\": N}}",
            path.display()
        )
    };
    let bad = || {
        format!(
            "the envelope file {} does not read. It must be {{\"envelope\": {{...}}, \"tokens\": N, \"turns\": N}}",
            path.display()
        )
    };
    let b = std::fs::read(path).map_err(|_| no())?;
    #[derive(serde::Deserialize)]
    struct File<'a> {
        #[serde(borrow, default)]
        envelope: Option<&'a serde_json::value::RawValue>,
        #[serde(default)]
        tokens: i64,
        #[serde(default)]
        turns: i64,
    }
    let f: File = serde_json::from_slice(&b).map_err(|_| bad())?;
    let env = f.envelope.map(|r| r.get().to_string()).unwrap_or_default();
    if env.is_empty() || env == "null" {
        return Err(bad());
    }
    Ok(ProjectEnvelope {
        envelope: env,
        tokens: f.tokens,
        turns: f.turns,
    })
}

/// The newest max bytes of the file, starting at a line and at a
/// character, with how many bytes it left out.
fn tail_of(f: &std::fs::File, size: u64, max: u64) -> Result<String, String> {
    use std::os::unix::fs::FileExt;
    let off = size.saturating_sub(max);
    let mut buf = vec![0u8; (size - off) as usize];
    let n = f
        .read_at(&mut buf, off)
        .map_err(|e| crate::sys::go_errno_text(&e))?;
    buf.truncate(n);
    let mut cut = off;
    let mut start = 0usize;
    if off > 0 {
        // Start at the first whole line; a log with no line break keeps
        // the bytes, from the first whole character.
        match buf.iter().position(|&b| b == b'\n') {
            Some(i) if i + 1 < buf.len() => {
                start = i + 1;
                cut += (i + 1) as u64;
            }
            _ => {
                while start < buf.len() && (buf[start] & 0xC0) == 0x80 {
                    start += 1;
                    cut += 1;
                }
            }
        }
    }
    let text = String::from_utf8_lossy(&buf[start..]).into_owned();
    if cut > 0 {
        return Ok(format!(
            "(the first {cut} bytes of this log are not shown)\n{text}"
        ));
    }
    Ok(text)
}

impl Server {
    pub(super) fn work_lock(&self) -> std::sync::MutexGuard<'_, WorkState> {
        self.work.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub(super) fn work_dir(&self, fc: &ForemanChat) -> PathBuf {
        fc.dir.join("work")
    }

    /// The operator's project by name or by path among the pinned and the
    /// recent directories.
    fn resolve_project(&self, name: &str) -> Result<String, String> {
        let pinned = config::load()
            .ok()
            .flatten()
            .map(|c| c.projects)
            .unwrap_or_default();
        let mut dirs = pinned.clone();
        dirs.extend(
            self.recent_dirs()
                .into_iter()
                .filter(|d| !pinned.contains(d)),
        );
        let mut seen = std::collections::HashSet::new();
        let (mut matched, mut names) = (Vec::new(), Vec::new());
        for d in dirs {
            if seen.contains(&d) || !dir_exists(&d) {
                continue;
            }
            seen.insert(d.clone());
            names.push(base(&d));
            if d == name || base(&d) == name {
                matched.push(d);
            }
        }
        match matched.len() {
            1 => Ok(matched.remove(0)),
            0 => Err(format!(
                "{} is not one of the operator's projects. They are: {}",
                crate::sys::go_quote(name),
                names.join(", ")
            )),
            n => Err(format!(
                "{n} projects are named {} ({}). Name one by its full path",
                crate::sys::go_quote(name),
                matched.join(", ")
            )),
        }
    }

    /// Reads the worker table; a worker that was running when the server
    /// stopped is marked stopped and reported once.
    pub fn load_workers(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        let mut stopped = Vec::new();
        {
            let mut w = self.work_lock();
            if w.loaded {
                return;
            }
            w.loaded = true;
            w.next = BTreeMap::new();
            w.ctx = Some(Ctx::background());
            let Ok(b) = std::fs::read(self.work_dir(fc).join("workers.json")) else {
                return;
            };
            #[derive(serde::Deserialize, Default)]
            #[serde(default)]
            struct File {
                workers: Option<Vec<WorkerRec>>,
                next: Option<BTreeMap<String, i64>>,
            }
            let Ok(f) = serde_json::from_slice::<File>(&b) else {
                return;
            };
            w.work = f.workers.unwrap_or_default();
            w.next = f.next.unwrap_or_default();
            let mut changed = false;
            let (held, queue_note) = self.restore_held(fc);
            if !queue_note.is_empty() {
                stopped.push(queue_note.clone());
            }
            let mut keep = Vec::new();
            for r in w.work.iter_mut() {
                if r.state == WORK_RUNNING || r.state == WORK_PAUSED {
                    r.state = WORK_STOPPED.into();
                    changed = true;
                    stopped.push(format!("[{}] was stopped when coppice stopped", r.label));
                }
                if r.state == WORK_QUEUED {
                    if let Some(h) = held.get(&r.label).filter(|_| r.background) {
                        keep.push(h.clone());
                        continue;
                    }
                    r.state = WORK_STOPPED.into();
                    changed = true;
                    let why = if r.background && !queue_note.is_empty() {
                        "the queue file did not read"
                    } else {
                        "a foreground job does not wait across a restart"
                    };
                    stopped.push(format!("[{}] was dropped when coppice stopped: {why}", r.label));
                }
            }
            for job in keep {
                w.pool.held.push(super::foreman_pool::HeldEntry { job, fc: fc.clone() });
            }
            if changed {
                self.save_workers_locked(fc, &w);
            }
        }
        for t in stopped {
            self.log_work(fc, &t);
        }
    }

    /// Writes the table. The caller holds the work lock.
    pub(super) fn save_workers_locked(&self, fc: &ForemanChat, w: &WorkState) {
        let dir = self.work_dir(fc);
        if crate::foreman::fsutil::ensure_dir_plain(&dir).is_err() {
            return;
        }
        let next: Vec<(&str, Value)> = w.next.iter().map(|(k, v)| (k.as_str(), json!(v))).collect();
        let body = gojson::marshal(&gojson::map(vec![
            (
                "workers",
                Value::Array(w.work.iter().map(WorkerRec::to_value).collect()),
            ),
            ("next", gojson::map(next)),
        ]));
        let tmp = dir.join("workers.json.tmp");
        if write_0600(&tmp, body.as_bytes()).is_ok() {
            let _ = std::fs::rename(&tmp, dir.join("workers.json"));
        }
    }

    /// Logs a worker's line as one work message. It starts no turn.
    pub fn log_work(self: &Arc<Server>, fc: &ForemanChat, text: &str) {
        if !fc.has_chat() {
            self.report_to_pane(text);
            return;
        }
        if let Err(e) = self.chat_append(fc, fchat::KIND_WORK, text) {
            self.note_once(&format!(
                "The foreman's log could not take a worker's report: {e}"
            ));
        }
    }

    /// The foreman's spawn tool in chat mode, with the background flag
    /// (FM-R-97).
    pub fn tool_spawn_background(
        self: &Arc<Server>,
        project: &str,
        brief: &str,
        profile: &str,
        model: &str,
        background: bool,
    ) -> Result<String, String> {
        let fc = self.ensure_chat();
        if !fc.has_chat() {
            return Err(super::foreman_chat::ERR_NO_CHAT.into());
        }
        self.tool_spawn_opts(&fc, project, brief, profile, model, background)
    }

    /// Spawn with every option. It checks the project and its envelope,
    /// records the worker and returns at once; a thread runs it. A profile
    /// that names a pool goes through the pool's queue (foreman_pool.rs);
    /// any other starts at once.
    pub fn tool_spawn_opts(
        self: &Arc<Server>,
        fc: &Arc<ForemanChat>,
        project: &str,
        brief: &str,
        profile: &str,
        model: &str,
        background: bool,
    ) -> Result<String, String> {
        if brief.trim().is_empty() {
            return Err("the brief is empty".into());
        }
        let dir = self.resolve_project(project)?;
        let name = base(&dir);
        let env_path = envelope_dir().join(format!("{name}.json"));
        let pe = read_project_envelope(&env_path, &name)?;
        let profile = if profile.is_empty() {
            config::DEFAULT_COMPACTOR_PROFILE
        } else {
            profile
        };
        if !flag_value(profile) {
            return Err(format!(
                "the profile {} is not a profile name",
                crate::sys::go_quote(profile)
            ));
        }
        if !model.is_empty() && !flag_value(model) {
            return Err(format!(
                "the model {} is not a model id",
                crate::sys::go_quote(model)
            ));
        }
        let pool = self.pool_for(fc, profile);
        if pool.is_none() && background {
            return Err(format!(
                "a background job needs a pool: the profile {profile} names none in {}",
                self.pools_path().display()
            ));
        }
        if let Some(pool) = pool {
            if !fc.has_chat() {
                return Err("pools run for the chat foreman only; the pane foreman starts a worker on a plain profile".into());
            }
            if !model.is_empty() && model != pool.model {
                return Err(format!(
                    "the pool {} serves the model {} only; a spawn on it cannot ask for {model}",
                    pool.name, pool.model
                ));
            }
            return self.spawn_pooled(fc, dir, brief, &pool, background);
        }
        if !self.work_lock().loaded {
            self.load_workers(fc);
        }
        let mut w = self.work_lock();
        if self.is_closed() {
            return Err("the server is closing".into());
        }
        let n = {
            let e = w.next.entry(name.clone()).or_insert(0);
            *e += 1;
            *e
        };
        let rec = WorkerRec {
            label: format!("{name}-{n}"),
            project: name,
            dir,
            profile: profile.to_string(),
            model: model.to_string(),
            started: self.fm_date(),
            state: WORK_RUNNING.into(),
            ..Default::default()
        };
        w.work.push(rec.clone());
        self.save_workers_locked(fc, &w);
        self.work_wg.add();
        let ctx = w.ctx.clone().unwrap_or_default();
        drop(w);
        let (s, f, brief) = (self.clone(), fc.clone(), brief.to_string());
        let label = rec.label.clone();
        thread::spawn(move || {
            s.run_worker(&ctx, &f, rec, pe, &brief, "");
            s.work_wg.done();
        });
        Ok(format!("started {label}"))
    }

    /// The session file sprig writes for a session id: <id>.jsonl in the
    /// gate root's sessions dir, or in the work dir's sprig-sessions when
    /// sprig ran under the fence (FN-R-25).
    fn worker_session_path(&self, fc: &ForemanChat, session: &str, fenced: bool) -> PathBuf {
        self.worker_sessions_dir(fc, fenced).join(format!("{session}.jsonl"))
    }

    /// Where sprig keeps a worker's sessions.
    fn worker_sessions_dir(&self, fc: &ForemanChat, fenced: bool) -> PathBuf {
        if fenced {
            self.work_dir(fc).join("sprig-sessions")
        } else {
            self.work_dir(fc).join("gate").join("sessions")
        }
    }

    /// Sets a worker's end state and logs its message, unless the server is
    /// closing: a worker the close stopped is reported at the next start.
    fn finish_worker(
        self: &Arc<Server>,
        ctx: &Ctx,
        fc: &Arc<ForemanChat>,
        label: &str,
        state: &str,
        session: &str,
        text: &str,
    ) {
        if ctx.err().is_some() {
            return;
        }
        let lines = {
            let mut w = self.work_lock();
            let fenced = w.work.iter().find(|r| r.label == label).is_some_and(|r| r.fenced);
            let file = if session.is_empty() {
                String::new()
            } else {
                self.worker_session_path(fc, session, fenced)
                    .to_string_lossy()
                    .into_owned()
            };
            if let Some(r) = w.work.iter_mut().find(|r| r.label == label) {
                r.state = state.into();
                r.session = session.into();
                if !file.is_empty() {
                    r.session_file = file;
                }
            }
            let pooled = w.work.iter().any(|r| r.label == label && !r.pool.is_empty());
            let lines = if pooled {
                self.pool_ended_locked(&mut w, fc, label, state)
            } else {
                vec![]
            };
            self.save_workers_locked(fc, &w);
            lines
        };
        self.log_work(fc, text);
        self.flush_lines(lines);
    }

    /// The environment of a worker's tree commands: the server's own, with
    /// COPPICE_DATA_DIR set as coppice sets it for every pane.
    fn worker_env(&self) -> Vec<(std::ffi::OsString, std::ffi::OsString)> {
        let mut env: Vec<_> = std::env::vars_os()
            .filter(|(k, _)| k != "COPPICE_DATA_DIR")
            .collect();
        env.push((
            "COPPICE_DATA_DIR".into(),
            self.cfg.data_dir.clone().into_os_string(),
        ));
        env
    }

    /// Makes the project's root in the tree once, as project-<name>.
    fn ensure_root(
        &self,
        ctx: &Ctx,
        fc: &ForemanChat,
        name: &str,
        pe: &ProjectEnvelope,
    ) -> Result<(), String> {
        let work = self.work_dir(fc);
        let marker = work.join("roots").join(name);
        if marker.exists() {
            return Ok(());
        }
        crate::foreman::fsutil::ensure_dir_plain(&work.join("roots"))?;
        let env_file = work.join("roots").join(format!("{name}.envelope.json"));
        write_0600(&env_file, pe.envelope.as_bytes())?;
        let mut args = vec![
            "tree".to_string(),
            "root".to_string(),
            env_file.to_string_lossy().into_owned(),
            "--session".to_string(),
            format!("project-{name}"),
            "--data-dir".to_string(),
            work.to_string_lossy().into_owned(),
            "--root".to_string(),
            work.join("gate").to_string_lossy().into_owned(),
        ];
        if pe.tokens > 0 {
            args.extend(["--tokens".to_string(), pe.tokens.to_string()]);
        }
        if pe.turns > 0 {
            args.extend(["--turns".to_string(), pe.turns.to_string()]);
        }
        let (out, err) = run_cmd(
            ctx,
            CmdOpts {
                dir: work.clone(),
                name: self.daisugi_bin(),
                args,
                env: Some(self.worker_env()),
                grace: WORKER_GRACE,
                ..Default::default()
            },
        );
        if let Some(e) = err {
            return Err(format!("cannot run daisugi: {}", e.text()));
        }
        if out.code != 0 {
            return Err(format!(
                "daisugi tree root failed: {}",
                first_line(&out.stderr, "")
            ));
        }
        write_0600(&marker, format!("{}\n", self.fm_date()).as_bytes())
    }

    /// One worker: the root, the profile bundle, then `daisugi tree run`.
    pub(super) fn run_worker(
        self: &Arc<Server>,
        ctx: &Ctx,
        fc: &Arc<ForemanChat>,
        w: WorkerRec,
        pe: ProjectEnvelope,
        brief: &str,
        base_url: &str,
    ) {
        let label = w.label.clone();
        let my_pid = Arc::new(std::sync::atomic::AtomicI32::new(0));
        self.run_worker_inner(ctx, fc, w, pe, brief, base_url, &my_pid);
        self.pool_forget(&label, my_pid.load(std::sync::atomic::Ordering::SeqCst));
    }

    fn run_worker_inner(
        self: &Arc<Server>,
        ctx: &Ctx,
        fc: &Arc<ForemanChat>,
        w: WorkerRec,
        pe: ProjectEnvelope,
        brief: &str,
        base_url: &str,
        my_pid: &Arc<std::sync::atomic::AtomicI32>,
    ) {
        let label = w.label.clone();
        let stderr_text = std::cell::RefCell::new(String::new());
        let fail = |why: &str| {
            if self.pool_out(ctx, fc, &label, brief, &format!("{why}\n{}", stderr_text.borrow())) {
                return;
            }
            self.finish_worker(
                ctx,
                fc,
                &label,
                WORK_FAILED,
                "",
                &format!("[{label}] failed: {why}"),
            )
        };
        if let Err(e) = self.ensure_root(ctx, fc, &w.project, &pe) {
            return fail(&e);
        }
        let work = self.work_dir(fc);
        let mut args = vec![
            "tree".to_string(),
            "run".to_string(),
            "--parent".to_string(),
            format!("project-{}", w.project),
            format!("--profile={}", w.profile),
        ];
        if !w.model.is_empty() {
            args.push(format!("--model={}", w.model));
        }
        let mut bundle = None;
        if w.profile != config::DEFAULT_COMPACTOR_PROFILE {
            match self.bundle_bytes(ctx) {
                Ok(mut b) => {
                    if !base_url.is_empty() {
                        match super::foreman_pool::patch_bundle_url(&mut b, &w.profile, base_url) {
                            Ok(p) => b = p,
                            Err(e) => return fail(&e),
                        }
                    }
                    bundle = Some(b)
                }
                Err(e) => return fail(&e),
            }
            args.extend(["--keys-fd".to_string(), "3".to_string()]);
        }
        args.extend([
            "--data-dir".to_string(),
            work.to_string_lossy().into_owned(),
            "--root".to_string(),
            work.join("gate").to_string_lossy().into_owned(),
            "-".to_string(),
        ]);
        let tools: Vec<Value> = WORKER_TOOLS.iter().map(|t| json!(t)).collect();
        let envelope: Value =
            serde_json::from_str(&gojson::compact(&pe.envelope)).unwrap_or(Value::Null);
        let stdin = gojson::marshal(&gojson::map(vec![
            ("task", json!(brief)),
            ("envelope", envelope),
            ("tools", Value::Array(tools)),
        ]));
        // The tree run stays outside the fence: it writes the worker's gate
        // root (its envelopes). Only sprig and its tools run fenced, with
        // the worktree and their session dir read and write; the gate root
        // is no rw grant of theirs (FN-R-17, FN-R-25).
        let mut env = self.worker_env();
        match self.sprig_fence(&w.dir, &work.join("gate").to_string_lossy()) {
            Ok(Some(f)) => {
                env.push(("DAISUGI_SPRIG_FENCE".into(), serde_json::to_string(&f).unwrap_or_default().into()));
                // --sprig-fence: the fence is required. A daisugi that does
                // not know the flag refuses it, so its worker never starts
                // bare (FN-R-28).
                require_sprig_fence(&mut args);
                let mut ws = self.work_lock();
                if let Some(r) = ws.work.iter_mut().find(|r| r.label == label) {
                    r.fenced = true;
                }
            }
            Ok(None) => {}
            Err(e) => return fail(&e),
        }
        let (out, err) = run_cmd(
            ctx,
            CmdOpts {
                dir: PathBuf::from(&w.dir),
                name: self.daisugi_bin(),
                args,
                stdin: Some(stdin.into_bytes()),
                // The bundle moves to the writer, which zeroes it.
                fd3: bundle.take(),
                env: Some(env),
                grace: WORKER_GRACE,
                started: Some({
                    let (s, l, mp) = (self.clone(), label.clone(), my_pid.clone());
                    Arc::new(move |pid| {
                        mp.store(pid, std::sync::atomic::Ordering::SeqCst);
                        s.pool_started(&l, pid)
                    })
                }),
                ..Default::default()
            },
        );
        if ctx.err().is_some() {
            return;
        }
        *stderr_text.borrow_mut() = out.stderr.clone();
        if let Some(e) = err {
            return fail(&format!("cannot run daisugi: {}", e.text()));
        }
        let parsed: Option<Value> = serde_json::from_str(out.stdout.trim()).ok();
        let code = out.code;
        let tag = format!("[{label}]");
        let Some(v) = parsed.filter(Value::is_object) else {
            if code == 0 {
                return fail("tree run gave an answer that is not JSON");
            }
            return fail(&first_line(
                &out.stderr,
                &format!("tree run exited with {code}"),
            ));
        };
        let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let (status, session) = (s("status"), s("session"));
        let reasons: Vec<String> = v
            .get("reasons")
            .and_then(Value::as_array)
            .map(|a| {
                a.iter()
                    .filter_map(|x| x.as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default();
        let reasons = reasons.join("; ");
        if code == 0 && status == "ran" {
            return self.finish_worker(
                ctx,
                fc,
                &label,
                WORK_RAN,
                &session,
                &format!("{tag} report\n{}", s("answer")),
            );
        }
        if code == 3 || status == "asked" {
            let mut text = format!("{tag} sent to the operator as ask {}", s("ask_id"));
            if !reasons.is_empty() {
                text.push_str(&format!(". {reasons}"));
            }
            return self.finish_worker(ctx, fc, &label, WORK_ASKED, &session, &text);
        }
        if status == "refused" {
            return self.finish_worker(
                ctx,
                fc,
                &label,
                WORK_REFUSED,
                &session,
                &format!("{tag} refused: {reasons}"),
            );
        }
        let why = if reasons.is_empty() {
            first_line(&out.stderr, &format!("tree run exited with {code}"))
        } else {
            reasons
        };
        if self.pool_out(ctx, fc, &label, brief, &format!("{why}\n{}", out.stderr)) {
            return;
        }
        self.finish_worker(
            ctx,
            fc,
            &label,
            WORK_FAILED,
            &session,
            &format!("{tag} failed: {why}"),
        );
    }

    /// Runs daisugi agent-profiles bundle and returns the bytes it wrote.
    /// The caller hands them on and clears them. The command is in a group
    /// of its own that dies with the context, the read is capped at 1 MiB,
    /// and it ends when the command does, so a grandchild that keeps
    /// descriptor 3 open cannot hold the call.
    pub fn bundle_bytes(&self, ctx: &Ctx) -> Result<Vec<u8>, String> {
        let dir = self.compactor_dir();
        crate::foreman::fsutil::ensure_dir_plain(&dir)?;
        let mut fds = [0i32; 2];
        // SAFETY: fds points at two ints; pipe2 fills them.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
            return Err(crate::sys::go_errno_text(&std::io::Error::last_os_error()));
        }
        // SAFETY: both descriptors are new and ours.
        let r = unsafe { std::fs::File::from_raw_fd(fds[0]) };
        let w = unsafe { std::fs::File::from_raw_fd(fds[1]) };
        let wfd = w.as_raw_fd();
        let name = self.daisugi_bin();
        let mut cmd = Command::new(&name);
        cmd.args(["agent-profiles", "bundle", "--fd", "3"])
            .current_dir(&dir)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .process_group(0);
        // SAFETY: fd_as_3 runs in the child before exec and makes only
        // async-signal-safe calls.
        unsafe {
            cmd.pre_exec(move || super::foreman_turn::fd_as_3(wfd));
        }
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                let why = if name.contains('/') {
                    format!("fork/exec {name}: {}", crate::sys::go_errno_text(&e))
                } else if e.kind() == std::io::ErrorKind::NotFound {
                    format!(
                        "exec: {}: executable file not found in $PATH",
                        crate::sys::go_quote(&name)
                    )
                } else {
                    format!("fork/exec {name}: {}", crate::sys::go_errno_text(&e))
                };
                return Err(format!(
                    "cannot start daisugi to read the agent profiles: {why}"
                ));
            }
        };
        drop(w);
        let pid = child.id() as i32;
        let mut stderr = child.stderr.take();
        let err_reader = thread::spawn(move || {
            let mut b = Vec::new();
            if let Some(se) = stderr.as_mut() {
                let _ = se.take(4096).read_to_end(&mut b);
            }
            b
        });
        // Read descriptor 3 until it closes, the command ends (then 200 ms
        // more), the cap, or the context ends.
        // SAFETY: the read end is ours; O_NONBLOCK lets the loop poll.
        unsafe {
            let fl = libc::fcntl(r.as_raw_fd(), libc::F_GETFL);
            libc::fcntl(r.as_raw_fd(), libc::F_SETFL, fl | libc::O_NONBLOCK);
        }
        let mut buf = Vec::new();
        let mut chunk = [0u8; 8192];
        let mut exited: Option<Instant> = None;
        let mut status = None;
        loop {
            if ctx.err().is_some() {
                // SAFETY: kill takes plain integers.
                unsafe { libc::kill(-pid, libc::SIGKILL) };
            }
            if status.is_none() {
                if let Ok(Some(s)) = child.try_wait() {
                    status = Some(s);
                    exited = Some(Instant::now());
                }
            }
            if exited.is_some_and(|t| t.elapsed() > Duration::from_millis(200)) {
                break;
            }
            let mut pfd = libc::pollfd {
                fd: r.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            };
            // SAFETY: one pollfd that lives across the call.
            unsafe { libc::poll(&mut pfd, 1, 20) };
            match (&r).read(&mut chunk) {
                Ok(0) => {
                    if status.is_none() {
                        status = child.wait().ok();
                    }
                    break;
                }
                Ok(n) => {
                    let room = (1usize << 20).saturating_sub(buf.len());
                    buf.extend_from_slice(&chunk[..n.min(room)]);
                    chunk.fill(0);
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(_) => break,
            }
        }
        let status = match status {
            Some(s) => Some(s),
            None => child.wait().ok(),
        };
        let stderr = err_reader.join().unwrap_or_default();
        let ok = status.is_some_and(|s| s.success());
        if !ok {
            buf.fill(0);
            use std::os::unix::process::ExitStatusExt;
            let werr = match status {
                Some(s) if s.signal().is_some() => "signal: killed".to_string(),
                Some(s) => format!("exit status {}", s.code().unwrap_or(-1)),
                None => "wait failed".to_string(),
            };
            return Err(format!(
                "daisugi agent-profiles bundle failed: {}",
                first_line(&String::from_utf8_lossy(&stderr), &werr)
            ));
        }
        Ok(buf)
    }

    /// Stops every worker: its whole process group gets SIGTERM, then
    /// SIGKILL. A worker the close stopped is reported at the next start.
    pub fn close_workers(&self) {
        let ctx = self.work_lock().ctx.clone();
        let Some(ctx) = ctx else { return };
        // A paused job goes on first, so that the stop reaches a process that
        // can end its node (FM-R-108).
        {
            let mut w = self.work_lock();
            let now = self.pool_now();
            let paused: Vec<String> = w
                .work
                .iter()
                .filter(|r| r.state == WORK_PAUSED)
                .map(|r| r.label.clone())
                .collect();
            for l in paused {
                self.thaw_locked(&mut w, &l, now);
            }
        }
        ctx.cancel();
        self.close_pools();
        self.work_wg.wait(WORKER_GRACE + Duration::from_secs(5));
    }

    /// The workers part of a turn's state block.
    pub fn workers_state(&self) -> String {
        let w = self.work_lock();
        let mut b = String::new();
        for r in &w.work {
            if r.state == WORK_RUNNING {
                b.push_str(&format!(
                    "- {} on {} since {}\n",
                    r.label, r.project, r.started
                ));
            }
        }
        let mut wait = String::new();
        for r in &w.work {
            if r.state == WORK_QUEUED || r.state == WORK_PAUSED {
                wait.push_str(&format!(
                    "- {} on {} is {} (pool {})\n",
                    r.label, r.project, r.state, r.pool
                ));
            }
        }
        if b.is_empty() && wait.is_empty() {
            return "workers: none\n".into();
        }
        let mut out = String::new();
        if !b.is_empty() {
            out = format!("workers running:\n{b}");
        }
        if !wait.is_empty() {
            out.push_str(&format!("workers waiting:\n{wait}"));
        }
        out
    }

    /// zoom(label): the newest 32 KiB of that worker's own log, its session
    /// file. The file is the one the worker table names, and only when it
    /// lies below the work dir's sessions.
    pub fn tool_zoom_worker(&self, fc: &ForemanChat, label: &str) -> Result<String, String> {
        let (rec, labels) = {
            let w = self.work_lock();
            let labels: Vec<String> = w.work.iter().map(|r| r.label.clone()).collect();
            (
                w.work.iter().rev().find(|r| r.label == label).cloned(),
                labels,
            )
        };
        let q = crate::sys::go_quote(label);
        let Some(rec) = rec else {
            if labels.is_empty() {
                return Err(format!("no worker is named {q}. There are no workers"));
            }
            return Err(format!(
                "no worker is named {q}. The workers are {}",
                labels.join(", ")
            ));
        };
        if rec.session_file.is_empty() {
            if rec.state == WORK_RUNNING {
                return Err(format!(
                    "{label} is still running. Its log can be read when it ends; status shows its state"
                ));
            }
            return Err(format!("{label} left no session log (it {})", rec.state));
        }
        let outside =
            || format!("the log of {label} is outside the workers' sessions and is not read");
        let sessions = self.worker_sessions_dir(fc, rec.fenced);
        let file = PathBuf::from(crate::sys::clean(&rec.session_file));
        let rel_ok = |root: &Path, p: &Path| -> bool {
            match p.strip_prefix(root) {
                Ok(r) => !r.as_os_str().is_empty(),
                Err(_) => false,
            }
        };
        if !rel_ok(&sessions, &file) {
            return Err(outside());
        }
        // The directory is resolved; the file itself is opened without
        // following a link and without blocking (a fifo), and checked on
        // the open descriptor, so nothing can be swapped in between.
        let parent = file.parent().map(Path::to_path_buf).unwrap_or_default();
        let (Ok(real_dir), Ok(real_root)) = (
            std::fs::canonicalize(&parent),
            std::fs::canonicalize(&sessions),
        ) else {
            return Err(format!(
                "the log of {label} cannot be read: it is not there"
            ));
        };
        if real_dir != real_root && !real_dir.starts_with(&real_root) {
            return Err(outside());
        }
        let path = real_dir.join(file.file_name().unwrap_or_default());
        let f = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK | libc::O_NOFOLLOW)
            .open(&path)
            .map_err(|e| {
                format!(
                    "the log of {label} cannot be read: {}",
                    crate::sys::go_path_err("open", &path, &e)
                )
            })?;
        let md = f
            .metadata()
            .map_err(|_| format!("the log of {label} is not a file"))?;
        if !md.is_file() {
            return Err(format!("the log of {label} is not a file"));
        }
        tail_of(&f, md.len(), WORKER_LOG_BYTES)
    }

    /// The foreman's status tool: the worker table.
    pub fn tool_status(&self) -> String {
        let w = self.work_lock();
        if w.work.is_empty() {
            return "no workers".into();
        }
        let mut rows: Vec<&WorkerRec> = w.work.iter().collect();
        rows.sort_by(|a, b| a.started.cmp(&b.started));
        rows.iter()
            .map(|r| {
                let model = if r.model.is_empty() { "-" } else { &r.model };
                let file = if r.session_file.is_empty() {
                    "-"
                } else {
                    &r.session_file
                };
                format!(
                    "{} | {} | {} | {model} | {} | {} | {file}",
                    r.label, r.project, r.profile, r.started, r.state
                )
            })
            .collect::<Vec<_>>()
            .join("\n")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_worker_gets_its_tools_with_lowercase_names() {
        assert_eq!(WORKER_TOOLS, ["read", "write", "edit", "bash"]);
        assert!(WORKER_TOOLS.iter().all(|t| *t == t.to_lowercase()));
    }

    #[test]
    fn a_fenced_worker_requires_the_sprig_fence() {
        let mut args: Vec<String> = ["tree", "run", "--root", "/w/gate", "-"].iter().map(|s| s.to_string()).collect();
        require_sprig_fence(&mut args);
        assert_eq!(args, ["tree", "run", "--root", "/w/gate", "--sprig-fence", "-"]);
    }
}
