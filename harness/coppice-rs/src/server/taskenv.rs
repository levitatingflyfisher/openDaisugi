//! An envelope on a task (design-delegation-tree.md, option 3). coppice
//! asks daisugi to prove each edge (tree check), to register the envelope
//! under a minted session (tree root, tree spawn) and to enforce it (warden
//! check --session). It decides nothing itself (ADR-0020). The Go server's
//! `internal/server/taskenv.go` is the reference.

use std::io::Read;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

use serde_json::Value;

use super::role::Role;
use super::Server;
use crate::layout;

/// The message a pane or a plugin gets for making a root task with an
/// envelope: only the operator's envelope can be a root.
pub const TASK_REFUSAL: &str = "only the operator makes a root task.";

/// The message a pinned pane gets for starting a pane outside its own task
/// and the tasks below it (W3K-12).
pub const OWN_TASK_REFUSAL: &str = "a pane starts panes only under its own task.";

const DAISUGI_TASK_TIMEOUT: Duration = Duration::from_secs(60);

/// Serializes the registration of task sessions, so two panes started into
/// one task together mint one session.
static PIN_MU: std::sync::Mutex<()> = std::sync::Mutex::new(());

fn daisugi_bin() -> String {
    std::env::var("COPPICE_DAISUGI_BIN")
        .ok()
        .filter(|b| !b.is_empty())
        .unwrap_or_else(|| "daisugi".to_string())
}

/// --data-dir for the daisugi calls that read or write the tree ledger,
/// from COPPICE_DAISUGI_DATA_DIR, or none.
fn daisugi_data_args() -> Vec<String> {
    match std::env::var("COPPICE_DAISUGI_DATA_DIR") {
        Ok(d) if !d.is_empty() => vec!["--data-dir".into(), d],
        _ => Vec::new(),
    }
}

/// Runs daisugi with args and returns its combined output and whether it
/// exited 0. Err is a failure to run it at all.
pub(super) fn run_daisugi_task(args: &[String]) -> Result<(String, bool, bool), String> {
    let mut cmd = Command::new(daisugi_bin());
    cmd.args(args).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = cmd
        .spawn()
        .map_err(|e| format!("fork/exec {}: {}", daisugi_bin(), e))?;
    let (tx, rx) = mpsc::channel::<Vec<u8>>();
    for mut pipe in [
        child.stdout.take().map(|p| Box::new(p) as Box<dyn Read + Send>),
        child.stderr.take().map(|p| Box::new(p) as Box<dyn Read + Send>),
    ]
    .into_iter()
    .flatten()
    {
        let tx = tx.clone();
        std::thread::spawn(move || {
            let mut b = Vec::new();
            let _ = pipe.read_to_end(&mut b);
            let _ = tx.send(b);
        });
    }
    drop(tx);
    let deadline = Instant::now() + DAISUGI_TASK_TIMEOUT;
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break Some(s),
            Ok(None) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                break None;
            }
        }
    };
    let mut out = Vec::new();
    for b in rx.iter() {
        out.extend(b);
    }
    let text = String::from_utf8_lossy(&out).trim().to_string();
    match status {
        Some(s) => Ok((text, s.success(), true)),
        None => Ok((text, false, false)),
    }
}

/// The first lines of daisugi's output, joined, for a refusal.
pub(super) fn reasons_of(out: &str) -> String {
    out.split('\n')
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .take(6)
        .collect::<Vec<_>>()
        .join(" ")
}

pub(super) fn rand_hex(n: usize) -> String {
    let mut b = vec![0u8; n];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut b);
    }
    b.iter().map(|x| format!("{x:02x}")).collect()
}

impl Server {
    /// Writes env to a private file under the data dir and returns its
    /// path. The files stay: daisugi reads them again on a resume.
    fn envelope_file(&self, task_id: &str, env: &str) -> Result<PathBuf, String> {
        let dir = self.cfg.data_dir.join("task-envelopes");
        {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(&dir)
                .map_err(|e| e.to_string())?;
        }
        let path = dir.join(format!("{task_id}.json"));
        write_private(&path, env.as_bytes())?;
        Ok(path)
    }

    /// The nearest task above id (not id itself) that has an envelope.
    fn envelope_above(&self, id: &str) -> Option<layout::Task> {
        let mut tk = self.tree.task(id)?;
        for _ in 0..64 {
            if tk.parent.is_empty() {
                return None;
            }
            tk = self.tree.task(&tk.parent)?;
            if !tk.envelope.is_empty() {
                return Some(tk);
            }
        }
        None
    }

    /// The task whose envelope binds panes in task id: the task itself when
    /// it has one, else the nearest one above.
    pub(super) fn pinned_task(&self, id: &str) -> Option<layout::Task> {
        let tk = self.tree.task(id)?;
        if !tk.envelope.is_empty() {
            return Some(tk);
        }
        self.envelope_above(id)
    }

    /// Asks daisugi whether child fits inside parent. A refusal is
    /// daisugi's reasons, joined.
    pub(super) fn prove_edge(&self, parent: &layout::Task, child: &str) -> Result<(), String> {
        let pf = self.envelope_file(&parent.id, &parent.envelope)?;
        let dir = self.cfg.data_dir.join("task-envelopes");
        let cf = dir.join(format!("candidate-{}.json", rand_hex(6)));
        write_private(&cf, child.as_bytes())?;
        let res = run_daisugi_task(&[
            "tree".into(),
            "check".into(),
            pf.to_string_lossy().into_owned(),
            cf.to_string_lossy().into_owned(),
        ]);
        let _ = std::fs::remove_file(&cf);
        match res {
            Ok((_, true, _)) => Ok(()),
            Ok((out, false, true)) => Err(reasons_of(&out)),
            Ok((out, false, false)) => Err(format!("could not run daisugi tree check: {}", reasons_of(&out))),
            Err(e) => Err(format!("could not run daisugi tree check: {e}")),
        }
    }

    /// The warden root a pinned pane's hook reads.
    pub(super) fn pin_root(&self) -> Option<PathBuf> {
        let root = self.warden_root();
        if super::fence::warden_armed(&root) {
            return Some(root);
        }
        self.warden_roots().into_iter().find(|r| super::fence::warden_armed(r))
    }

    /// The daisugi session task tk's envelope is registered under. It
    /// registers it first when no pane has yet: tree root for a task with
    /// no envelope above it, tree spawn --parent for one with, which proves
    /// the edge again (TR-R-8). The session is minted t<id>-<16 hex>.
    pub(super) fn task_session(&self, tk: &layout::Task) -> Result<String, String> {
        let _g = PIN_MU.lock().unwrap_or_else(|e| e.into_inner());
        self.task_session_locked(&tk.id)
    }

    fn task_session_locked(&self, id: &str) -> Result<String, String> {
        let tk = self.tree.task(id).ok_or_else(|| format!("no task {id}"))?;
        if !tk.session.is_empty() {
            return Ok(tk.session);
        }
        let Some(root) = self.pin_root() else {
            return Err(format!(
                "task {id} has an envelope, and no warden is armed to enforce it. Run daisugi install, or start the server with a warden root."
            ));
        };
        let file = self.envelope_file(id, &tk.envelope)?;
        let session = format!("{id}-{}", rand_hex(8));
        let mut args: Vec<String> = vec!["tree".into()];
        let verb;
        if let Some(up) = self.envelope_above(id) {
            let parent_session = self.task_session_locked(&up.id)?;
            verb = "spawn";
            args.extend([
                "spawn".into(),
                file.to_string_lossy().into_owned(),
                "--parent".into(),
                parent_session,
                "--session".into(),
                session.clone(),
            ]);
        } else {
            verb = "root";
            args.extend([
                "root".into(),
                file.to_string_lossy().into_owned(),
                "--session".into(),
                session.clone(),
            ]);
        }
        args.extend(["--root".into(), root.to_string_lossy().into_owned()]);
        args.extend(daisugi_data_args());
        match run_daisugi_task(&args) {
            Ok((_, true, _)) => {}
            Ok((out, false, true)) => {
                return Err(format!(
                    "daisugi {verb} refused task {id}'s envelope: {}",
                    reasons_of(&out)
                ))
            }
            Ok((out, false, false)) => return Err(format!("could not run daisugi {verb}: {}", reasons_of(&out))),
            Err(e) => return Err(format!("could not run daisugi {verb}: {e}")),
        }
        self.tree.set_task_session(id, &session).map_err(|e| e.to_string())?;
        self.mark_layout_dirty();
        Ok(session)
    }

    /// The session a pane about to start under rec is pinned to, or "" when
    /// its task (and every task above it) has no envelope. An error refuses
    /// the start: a pane in a task with an envelope never runs unpinned.
    pub(super) fn pane_session(&self, rec: &layout::Pane) -> Result<String, String> {
        if rec.task_id.is_empty() {
            return Ok(String::new());
        }
        match self.pinned_task(&rec.task_id) {
            None => Ok(String::new()),
            Some(pt) => self.task_session(&pt),
        }
    }

    /// True when the connection is a pane whose task is pinned to an
    /// envelope and task_id is neither that task nor one below it. Any other
    /// connection, and a pane with no pinned task, may start a pane
    /// anywhere, as before.
    pub(super) fn own_task_only(&self, ro: Option<&Role>, task_id: &str) -> bool {
        let Some(ro) = ro else { return false };
        if !ro.pane || ro.pane_id.is_empty() {
            return false;
        }
        let Some(p) = self.tree.pane(&ro.pane_id) else {
            return false;
        };
        if p.task_id.is_empty() || self.pinned_task(&p.task_id).is_none() {
            return false;
        }
        let mut id = task_id.to_string();
        for _ in 0..64 {
            if id.is_empty() {
                break;
            }
            if id == p.task_id {
                return false;
            }
            match self.tree.task(&id) {
                Some(tk) => id = tk.parent,
                None => break,
            }
        }
        true
    }
}

/// Writes a file readable by its owner only.
fn write_private(path: &std::path::Path, b: &[u8]) -> Result<(), String> {
    use std::os::unix::fs::OpenOptionsExt;
    use std::io::Write;
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)
        .map_err(|e| e.to_string())?;
    f.write_all(b).map_err(|e| e.to_string())
}

/// Whether a JSON text is an object.
pub(super) fn is_object(text: &str) -> bool {
    matches!(serde_json::from_str::<Value>(text), Ok(Value::Object(_)))
}
