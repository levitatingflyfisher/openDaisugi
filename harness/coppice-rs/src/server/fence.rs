//! A pane's harness under `daisugi fence`: when it is fenced and the
//! command prefix that does it. The Go server's `internal/server/fence.go`
//! is the reference.

use super::{role::Role, Server};
use crate::layout;
use std::collections::BTreeMap;

/// The harness names `daisugi fence` carries a table for.
const FENCE_HARNESSES: &[&str] = &["claude", "codex", "pi", "opencode", "sprig"];

/// The message a pane, a plugin or an unplaced peer gets for turning a
/// pane's fence off (FN-R-15).
pub const FENCE_REFUSAL: &str = "only the operator turns a pane's fence off.";

/// The message a fenced pane gets for typing into a pane with no fence.
pub const TYPE_FENCE_REFUSAL: &str = "a fenced pane may type only into a fenced pane.";

/// What a pane or a plugin gets for pane.ack_changed.
pub const ACK_REFUSAL: &str = "only the operator acks a changed workspace.";

/// Guards workspace-holds.json (FN-R-24).
static HOLD_MU: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// A workspace as the holds key it: links resolved.
fn ws_key(dir: &str) -> String {
    match std::fs::canonicalize(dir) {
        Ok(p) => p.to_string_lossy().into_owned(),
        Err(_) => clean(dir),
    }
}

/// The variables a request may not set for a fenced pane: the server's
/// own values stand (FN-R-15).
const PINNED_ENV: &[&str] = &[
    "HOME",
    "TMPDIR",
    "XDG_DATA_HOME",
    "XDG_CONFIG_HOME",
    "XDG_STATE_HOME",
    "XDG_RUNTIME_DIR",
    "XDG_CACHE_HOME",
    "OPENDAISUGI_HOME",
];

/// A fenced pane's extra env without the pinned keys and DAISUGI_FENCE*.
pub fn pin_fenced_env(env: &BTreeMap<String, String>) -> BTreeMap<String, String> {
    env.iter()
        .filter(|(k, _)| !k.starts_with("DAISUGI_FENCE") && !PINNED_ENV.contains(&k.as_str()))
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect()
}

/// True when root holds envelopes and no DISARMED marker. Only a missing
/// envelopes dir reads as off (FN-R-17).
fn gate_armed(root: &std::path::Path) -> bool {
    if let Err(e) = std::fs::metadata(root.join("envelopes")) {
        if e.kind() == std::io::ErrorKind::NotFound {
            return false;
        }
    }
    std::fs::symlink_metadata(root.join("DISARMED")).is_err()
}

/// One word quoted for sh, always, as the Go server's shellQuote does.
fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// The files in a workspace that run code outside any fence later: project
/// MCP servers (.mcp.json), direnv (.envrc), git submodules, git config and
/// hooks (of the git dir a gitfile points to, and of its common dir, too),
/// and the project config of Claude, Codex and OpenCode. Landlock cannot
/// cut them out of a granted tree, so the server hashes them when a fenced
/// pane starts and ends (FN-R-13, FN-R-24).
pub fn workspace_watched(dir: &str) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    if dir.is_empty() {
        return out;
    }
    let mut w = Watch { dir: dir.trim_end_matches('/').to_string(), budget: WATCH_BUDGET, out: &mut out };
    for f in [".mcp.json", ".envrc", ".gitmodules", "opencode.json", "opencode.jsonc"] {
        w.add(&format!("{}/{f}", w.dir), 0);
    }
    for d in [".claude", ".codex", ".opencode"] {
        w.add(&format!("{}/{d}", w.dir), WATCH_DEPTH);
    }
    let git = format!("{}/.git", w.dir);
    match std::fs::symlink_metadata(&git) {
        Err(_) => {}
        Ok(m) if m.is_dir() => w.git_dir(&git),
        Ok(m) if m.is_file() => {
            // A gitfile (a worktree, a submodule): the file itself, the
            // git dir it names and that dir's common dir.
            w.add(&git, 0);
            if let Some(gd) = gitfile_dir(&git) {
                w.git_dir(&gd);
                if let Ok(t) = std::fs::read_to_string(format!("{gd}/commondir")) {
                    let cd = t.trim();
                    let cd = if cd.starts_with('/') { cd.to_string() } else { format!("{gd}/{cd}") };
                    w.git_dir(&clean(&cd));
                }
            }
        }
        Ok(_) => w.add(&git, 0),
    }
    out
}

/// The watch reads at most WATCH_BUDGET entries, WATCH_DEPTH levels into a
/// harness config dir; past the budget it records WATCH_TOO_MANY.
const WATCH_BUDGET: i64 = 4096;
const WATCH_DEPTH: u32 = 8;
const WATCH_TOO_MANY: &str = "(more files than the watch reads)";

struct Watch<'a> {
    dir: String,
    budget: i64,
    out: &'a mut BTreeMap<String, String>,
}

impl Watch<'_> {
    fn key(&self, p: &str) -> String {
        match p.strip_prefix(&format!("{}/", self.dir)) {
            Some(rel) => rel.to_string(),
            None => p.to_string(),
        }
    }

    fn git_dir(&mut self, gd: &str) {
        self.add(&format!("{gd}/config"), 0);
        self.add(&format!("{gd}/hooks"), 1);
    }

    fn add(&mut self, p: &str, depth: u32) {
        let Ok(m) = std::fs::symlink_metadata(p) else { return };
        self.budget -= 1;
        if self.budget < 0 {
            self.out.insert(WATCH_TOO_MANY.to_string(), "-".to_string());
            return;
        }
        let ft = m.file_type();
        if ft.is_symlink() {
            let t = std::fs::read_link(p).map(|t| t.to_string_lossy().into_owned()).unwrap_or_default();
            self.out.insert(self.key(p), format!("link:{t}"));
        } else if ft.is_dir() {
            if depth == 0 {
                return;
            }
            let mut names: Vec<String> = std::fs::read_dir(p)
                .map(|rd| rd.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect())
                .unwrap_or_default();
            names.sort();
            for n in names {
                self.add(&format!("{p}/{n}"), depth - 1);
            }
        } else if ft.is_file() {
            if let Ok(b) = std::fs::read(p) {
                self.out.insert(self.key(p), crate::sha256::hex(&b));
            }
        }
    }
}

/// The git dir a gitfile names ("gitdir: <path>"), resolved against the
/// file's directory.
fn gitfile_dir(gitfile: &str) -> Option<String> {
    let t = std::fs::read_to_string(gitfile).ok()?;
    let gd = t.lines().next()?.trim().strip_prefix("gitdir:")?.trim().to_string();
    if gd.is_empty() {
        return None;
    }
    if gd.starts_with('/') {
        return Some(clean(&gd));
    }
    let parent = std::path::Path::new(gitfile).parent()?.to_string_lossy().into_owned();
    Some(clean(&format!("{parent}/{gd}")))
}

/// filepath.Clean for an absolute path.
fn clean(p: &str) -> String {
    let mut parts: Vec<&str> = Vec::new();
    for c in p.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            _ => parts.push(c),
        }
    }
    format!("/{}", parts.join("/"))
}

/// The watched files that differ between two hashes, sorted.
pub fn workspace_changes(before: &BTreeMap<String, String>, after: &BTreeMap<String, String>) -> Vec<String> {
    let mut out: Vec<String> = after
        .iter()
        .filter(|(k, v)| before.get(*k) != Some(*v))
        .map(|(k, _)| k.clone())
        .collect();
    out.extend(before.keys().filter(|k| !after.contains_key(*k)).cloned());
    out.sort();
    out
}

fn daisugi_bin() -> String {
    std::env::var("COPPICE_DAISUGI_BIN")
        .ok()
        .filter(|b| !b.is_empty())
        .unwrap_or_else(|| "daisugi".to_string())
}

fn found(bin: &str) -> Option<String> {
    if bin.contains('/') {
        std::fs::metadata(bin).is_ok().then(|| bin.to_string())
    } else {
        crate::pane::look_path(bin)
    }
}

impl Server {
    /// Sets the fence setting from the config: "auto", "on" or "off".
    pub fn set_fence_mode(&self, mode: &str) {
        *self.fence_mode.lock().unwrap_or_else(|e| e.into_inner()) = mode.to_string();
    }

    /// Sets the home whose guarded data dirs hold gate roots.
    pub fn set_hook_home(&self, home: std::path::PathBuf) {
        *self.hook_home.lock().unwrap_or_else(|e| e.into_inner()) = home;
    }

    fn fence_mode(&self) -> String {
        self.fence_mode.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// The gate roots this floor's panes may use: the one beside the data
    /// dir, then each guarded data dir's gate under the hook home. The
    /// data-dir variables count only for the user's own home.
    pub(super) fn gate_roots(&self) -> Vec<std::path::PathBuf> {
        let mut out = vec![self.gate_root()];
        let home = self.hook_home.lock().unwrap_or_else(|e| e.into_inner()).to_string_lossy().into_owned();
        if !home.starts_with('/') {
            return out;
        }
        let mut add = |p: std::path::PathBuf| {
            if !out.contains(&p) {
                out.push(p);
            }
        };
        add(std::path::PathBuf::from(format!("{home}/.opendaisugi/gate")));
        let user = crate::sys::home_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
        if home == user {
            let usable = |v: String| -> Option<String> {
                let v = if v == "~" {
                    home.clone()
                } else if let Some(r) = v.strip_prefix("~/") {
                    format!("{home}/{r}")
                } else {
                    v
                };
                v.starts_with('/').then_some(v)
            };
            if let Some(d) = std::env::var("OPENDAISUGI_HOME").ok().and_then(usable) {
                add(std::path::PathBuf::from(format!("{d}/gate")));
            }
            if let Some(x) = std::env::var("XDG_DATA_HOME").ok().and_then(usable) {
                add(std::path::PathBuf::from(format!("{x}/opendaisugi/gate")));
            }
        }
        out
    }

    /// True when any gate root this floor's panes may use is armed.
    pub(super) fn gate_on(&self) -> bool {
        self.gate_roots().iter().any(|r| gate_armed(r))
    }

    /// Whether a pane's harness starts under the fence: the pane's own
    /// setting first, then the config's. Under "auto" the gate decides. A
    /// plain shell pane is fenced only when its own setting says on.
    pub(super) fn fence_on(&self, rec: &layout::Pane) -> bool {
        match rec.fence.as_str() {
            "off" => return false,
            "on" => return true,
            _ => {}
        }
        if rec.harness.is_empty() || rec.harness == "shell" {
            return false;
        }
        match self.fence_mode().as_str() {
            "off" => false,
            "on" => true,
            _ => self.gate_on(),
        }
    }

    /// Whether the live pane id runs under the fence.
    pub(super) fn pane_fenced(&self, id: &str) -> bool {
        self.live(id).is_some_and(|lp| lp.fenced)
    }

    /// Whether a connection may be a fenced process: a fenced pane, a pane
    /// the server cannot name, a plugin, or an unplaced peer (FN-R-15).
    pub(super) fn caller_fenced(&self, ro: &Role) -> bool {
        if ro.unknown || !ro.plugin.is_empty() {
            return true;
        }
        ro.pane && (ro.pane_id.is_empty() || self.pane_fenced(&ro.pane_id))
    }

    /// The fence part of guard: a typing verb from a caller that may be
    /// fenced reaches only a fenced pane.
    pub(super) fn fence_guard(&self, ro: &Role, typing: bool, target: &str) -> Option<&'static str> {
        (typing && ro.no_allow && self.caller_fenced(ro) && !self.pane_fenced(target)).then_some(TYPE_FENCE_REFUSAL)
    }

    /// A fresh private temp dir for one run of a fenced pane, under the
    /// data dir (FN-R-14). Each start gets a new leaf, so no run finds an
    /// earlier run's files (FN-R-23); the live pane keeps it for its end.
    pub(super) fn fence_tmp(&self, id: &str) -> String {
        let mut b = [0u8; 8];
        if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
            use std::io::Read;
            let _ = f.read_exact(&mut b);
        }
        let nonce: String = b.iter().map(|x| format!("{x:02x}")).collect();
        format!("{}/fence-tmp/{}-{nonce}", self.cfg.data_dir.to_string_lossy(), id.replace(['/', ':'], "_"))
    }

    /// The command prefix that runs a pane's harness under `daisugi fence`,
    /// empty when it runs bare. Built at each start and never saved into
    /// the pane's argv. An error says why a fenced pane cannot start; the
    /// harness never runs unfenced by accident.
    pub(super) fn fence_wrap(&self, rec: &layout::Pane, tmp: &str) -> Result<Vec<String>, String> {
        self.fence_wrap_with(rec, daisugi_bin(), tmp)
    }

    /// `fence_wrap` with the daisugi binary named, so a test needs no
    /// environment variable.
    pub(super) fn fence_wrap_with(&self, rec: &layout::Pane, bin: String, tmp: &str) -> Result<Vec<String>, String> {
        if !self.fence_on(rec) {
            return Ok(vec![]);
        }
        let Some(path) = found(&bin) else {
            return Err(format!(
                "this pane starts under daisugi fence, and {bin} is not on PATH. Install daisugi, or set fence = \"off\" in coppice.toml."
            ));
        };
        let mut w = vec![path, "fence".to_string()];
        if FENCE_HARNESSES.contains(&rec.harness.as_str()) {
            w.push("--harness".into());
            w.push(rec.harness.clone());
        }
        if !rec.cwd.is_empty() {
            w.push("--workspace".into());
            w.push(rec.cwd.clone());
        }
        w.push("--gate-root".into());
        w.push(self.gate_root().to_string_lossy().into_owned());
        w.push("--tmpdir".into());
        w.push(tmp.to_string());
        w.push("--coppice-socket".into());
        w.push(self.cfg.socket_path.to_string_lossy().into_owned());
        // A sprig pane keeps its session in a directory it was given: a
        // work dir, checked like the workspace.
        if let Some(argv) = &rec.argv {
            let mut i = 0;
            while i < argv.len() {
                if argv[i] == "--session-dir" && i + 1 < argv.len() {
                    w.push("--workdir".into());
                    w.push(argv[i + 1].clone());
                } else if let Some(v) = argv[i].strip_prefix("--session-dir=") {
                    w.push("--workdir".into());
                    w.push(v.to_string());
                }
                i += 1;
            }
        }
        w.push("--".into());
        Ok(w)
    }

    /// The prefix that runs a foreman worker or turn under the fence
    /// (FN-R-17), or empty when the gate is off and the config does not
    /// say on.
    pub(super) fn fence_cmd(&self, harness: &str, dir: &str, allow: &[String], keep_fd3: bool) -> Result<Vec<String>, String> {
        self.fence_cmd_with(harness, dir, allow, keep_fd3, daisugi_bin())
    }

    /// `fence_cmd` with the daisugi binary named.
    pub(super) fn fence_cmd_with(
        &self,
        harness: &str,
        dir: &str,
        allow: &[String],
        keep_fd3: bool,
        bin: String,
    ) -> Result<Vec<String>, String> {
        match self.fence_mode().as_str() {
            "off" => return Ok(vec![]),
            "on" => {}
            _ => {
                if !self.gate_on() {
                    return Ok(vec![]);
                }
            }
        }
        let Some(path) = found(&bin) else {
            return Err(format!("the gate is on, so this runs under daisugi fence, and {bin} is not on PATH"));
        };
        let mut w = vec![path, "fence".to_string()];
        if !harness.is_empty() {
            w.push("--harness".into());
            w.push(harness.to_string());
        }
        w.push("--workspace".into());
        w.push(dir.to_string());
        w.push("--gate-root".into());
        w.push(self.gate_root().to_string_lossy().into_owned());
        for a in allow {
            w.push("--allow".into());
            w.push(format!("{a}:rw"));
        }
        w.push("--coppice-socket".into());
        w.push(self.cfg.socket_path.to_string_lossy().into_owned());
        if keep_fd3 {
            w.push("--keep-fd".into());
            w.push("3".into());
        }
        w.push("--".into());
        Ok(w)
    }

    /// The `daisugi fence` prefix (no "--") a foreman worker's sprig starts
    /// under, or None when the gate is off and the config does not say on.
    /// The gate root is the worker's own, so the fence starts its resident
    /// gate and keeps it read only; the tree run adds sprig's session dir
    /// and its key descriptors (FN-R-25).
    pub(super) fn sprig_fence(&self, dir: &str, gate_root: &str) -> Result<Option<Vec<String>>, String> {
        self.sprig_fence_with(dir, gate_root, daisugi_bin())
    }

    /// `sprig_fence` with the daisugi binary named.
    pub(super) fn sprig_fence_with(&self, dir: &str, gate_root: &str, bin: String) -> Result<Option<Vec<String>>, String> {
        let mut w = self.fence_cmd_with("", dir, &[], false, bin)?;
        if w.is_empty() {
            return Ok(None);
        }
        w.pop(); // the "--"
        if let Some(i) = w.iter().position(|a| a == "--gate-root") {
            w.drain(i..(i + 2).min(w.len()));
        }
        w.push("--gate-root".into());
        w.push(gate_root.to_string());
        Ok(Some(w))
    }

    /// The --settings document a fenced claude pane starts with (FN-R-13).
    pub(super) fn claude_settings(&self, daisugi: &str) -> String {
        let root = self.gate_root();
        let root = if gate_armed(&root) {
            root
        } else {
            self.gate_roots().into_iter().find(|r| gate_armed(r)).unwrap_or(root)
        };
        let cmd = format!(
            "DAISUGI_GATE_HOOK=opendaisugi.gate {} gate check --root {} --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2",
            shell_quote(daisugi),
            shell_quote(&root.to_string_lossy())
        );
        // Keys in sorted order, as Go's encoding/json writes a map: this
        // serde_json keeps insertion order.
        serde_json::json!({
            "disableAllHooks": false,
            "hooks": {
                "ConfigChange": [{"hooks": [{"command": "exit 2", "type": "command"}]}],
                "PreToolUse": [{"hooks": [{
                    "command": cmd, "onFailure": "block", "timeout": 105, "type": "command"
                }], "matcher": "*"}]
            }
        })
        .to_string()
    }

    /// A fenced claude pane's argv with the --settings document: after the
    /// program for a pty pane, in front for an adapter's.
    pub(super) fn fenced_claude_argv(&self, rec: &layout::Pane, wrap: &[String], argv: Vec<String>, pty: bool) -> Vec<String> {
        if wrap.is_empty() || rec.harness != "claude" {
            return argv;
        }
        let settings = vec!["--settings".to_string(), self.claude_settings(&wrap[0])];
        if pty {
            if argv.is_empty() {
                return argv;
            }
            let mut out = vec![argv[0].clone()];
            out.extend(settings);
            out.extend(argv[1..].iter().cloned());
            return out;
        }
        let mut out = settings;
        out.extend(argv);
        out
    }

    /// The end of a fenced pane: its temp dir goes, and a change to a
    /// watched workspace file is kept on the record and said in a note.
    pub(super) fn fenced_pane_ended(&self, id: &str, cwd: &str, lp: &super::LivePane) {
        if lp.ended_once.swap(true, std::sync::atomic::Ordering::SeqCst) {
            return;
        }
        if !lp.fence_tmp.is_empty() {
            let _ = std::fs::remove_dir_all(&lp.fence_tmp);
        }
        let now = workspace_watched(cwd);
        let changed = workspace_changes(&lp.ws_hash, &now);
        if changed.is_empty() {
            return;
        }
        let list = changed.clone();
        let _ = self.tree.update_pane(id, move |p| p.workspace_changed = list);
        // The next start in this workspace waits for the operator's ack; a
        // pane that runs there now goes on (FN-R-24). A change a start
        // already held (and the operator may have acked) is not held again.
        let unseen = {
            let _g = HOLD_MU.lock().unwrap_or_else(|e| e.into_inner());
            let mut seen = lp.ws_seen.lock().unwrap_or_else(|e| e.into_inner());
            let unseen = workspace_changes(&seen, &now);
            *seen = now;
            unseen
        };
        if !unseen.is_empty() {
            self.add_hold(cwd, &unseen);
        }
        self.note(
            &format!(
                "workspace config changed: a fenced run changed {} in {cwd}. Check them, then run: coppice pane ack-changed {cwd}",
                changed.join(", ")
            ),
            id,
        );
    }

    /// The file that keeps the workspaces that wait for an ack, so a hold
    /// outlives the pane record and a server restart.
    fn holds_path(&self) -> std::path::PathBuf {
        self.cfg.data_dir.join("workspace-holds.json")
    }

    fn read_holds(&self) -> BTreeMap<String, Vec<String>> {
        std::fs::read(self.holds_path()).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default()
    }

    fn write_holds(&self, m: &BTreeMap<String, Vec<String>>) {
        let tmp = self.holds_path().with_extension("json.tmp");
        if let Ok(b) = serde_json::to_vec(m) {
            use std::os::unix::fs::OpenOptionsExt;
            let ok = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)
                .and_then(|mut f| std::io::Write::write_all(&mut f, &b))
                .is_ok();
            if ok {
                let _ = std::fs::rename(&tmp, self.holds_path());
            }
        }
    }

    /// Records that a fenced run changed files in dir.
    pub(super) fn add_hold(&self, dir: &str, files: &[String]) {
        let _g = HOLD_MU.lock().unwrap_or_else(|e| e.into_inner());
        let mut m = self.read_holds();
        let e = m.entry(ws_key(dir)).or_default();
        e.extend(files.iter().cloned());
        e.sort();
        e.dedup();
        self.write_holds(&m);
    }

    /// The watch seeing a change before the pane ends (R2-4): each live
    /// fenced pane whose workspace is dir or above it is hashed again, and
    /// a change since the last look is held now.
    fn refresh_holds(&self, dir: &str) {
        let k = ws_key(dir);
        let lps: Vec<std::sync::Arc<super::LivePane>> = self
            .live
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .filter(|lp| lp.fenced)
            .cloned()
            .collect();
        for lp in lps {
            let (id, cwd) = {
                let i = lp.info.lock().unwrap_or_else(|e| e.into_inner());
                (i.id.clone(), i.cwd.clone())
            };
            if cwd.is_empty() || !under_dir(&k, &ws_key(&cwd)) {
                continue;
            }
            let changed = {
                let _g = HOLD_MU.lock().unwrap_or_else(|e| e.into_inner());
                let now = workspace_watched(&cwd);
                let mut seen = lp.ws_seen.lock().unwrap_or_else(|e| e.into_inner());
                let changed = workspace_changes(&seen, &now);
                *seen = now;
                changed
            };
            if !changed.is_empty() {
                self.add_hold(&cwd, &changed);
                let all = workspace_changes(&lp.ws_hash, &workspace_watched(&cwd));
                let _ = self.tree.update_pane(&id, move |p| p.workspace_changed = all);
            }
        }
    }

    /// The held workspace a start in dir waits on, and its changed files: a
    /// hold covers its workspace and every dir under it (R2-4).
    fn held_over(&self, dir: &str) -> Option<(String, Vec<String>)> {
        if dir.is_empty() {
            return None;
        }
        self.refresh_holds(dir);
        let k = ws_key(dir);
        let _g = HOLD_MU.lock().unwrap_or_else(|e| e.into_inner());
        self.read_holds().into_iter().find(|(h, files)| !files.is_empty() && under_dir(&k, h))
    }

    /// The changed files a start in dir waits on.
    pub(super) fn workspace_hold(&self, dir: &str) -> Vec<String> {
        self.held_over(dir).map(|(_, f)| f).unwrap_or_default()
    }

    /// The start refusal for rec, if its workspace (or one above it) is
    /// held. It names the held workspace, which is what the operator acks.
    pub(super) fn hold_error(&self, rec: &layout::Pane) -> Option<String> {
        let (h, files) = self.held_over(&rec.cwd)?;
        Some(format!(
            "workspace config changed: a fenced run changed {} in {h}. Check them, then run: coppice pane ack-changed {h}",
            files.join(", ")
        ))
    }

    /// pane.ack_changed: the operator has checked the files a fenced run
    /// changed in cwd, and starts there go ahead again.
    pub(super) fn handle_ack_changed(&self, r: &crate::proto::Request) -> crate::proto::Response {
        let dir = r.str("cwd").unwrap_or_default();
        if !dir.starts_with('/') {
            return crate::proto::err_resp(
                &r.id,
                crate::proto::ERR_BAD_REQUEST,
                "pane.ack_changed takes cwd, an absolute directory.",
            );
        }
        let _g = HOLD_MU.lock().unwrap_or_else(|e| e.into_inner());
        let mut m = self.read_holds();
        let k = ws_key(&dir);
        let files = m.remove(&k).unwrap_or_default();
        self.write_holds(&m);
        crate::proto::ok_resp(&r.id, serde_json::json!({"acked": files, "cwd": k}))
    }
}

/// Whether d is dir or below it, by path parts: /ws does not hold /wsx.
fn under_dir(d: &str, dir: &str) -> bool {
    if dir == "/" {
        return d.starts_with('/');
    }
    d == dir || d.starts_with(&format!("{}/", dir.trim_end_matches('/')))
}

/// The fence setting of a pane started for a caller: "on" when the caller
/// may be fenced, else the record's own.
pub fn fence_or(own: &str, fenced: bool) -> String {
    if fenced {
        "on".to_string()
    } else {
        own.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::super::role::operator_verb_refusal;
    use super::*;
    use std::sync::Arc;

    #[test]
    fn the_shell_pane_and_a_pane_with_no_harness_are_never_fenced() {
        let s = Server::new(Default::default());
        s.set_fence_mode("on");
        for h in ["", "shell"] {
            let rec = layout::Pane { harness: h.into(), ..Default::default() };
            assert!(!s.fence_on(&rec), "{h:?}");
        }
        let rec = layout::Pane { harness: "claude".into(), ..Default::default() };
        assert!(s.fence_on(&rec));
    }

    #[test]
    fn the_pane_setting_beats_the_config() {
        let s = Server::new(Default::default());
        s.set_fence_mode("off");
        let on = layout::Pane { harness: "claude".into(), fence: "on".into(), ..Default::default() };
        let plain = layout::Pane { harness: "claude".into(), ..Default::default() };
        assert!(s.fence_on(&on));
        assert!(!s.fence_on(&plain));
        let s = Server::new(Default::default());
        s.set_fence_mode("on");
        let off = layout::Pane { harness: "claude".into(), fence: "off".into(), ..Default::default() };
        assert!(!s.fence_on(&off));
    }

    fn armed(s: &Server) {
        std::fs::create_dir_all(s.gate_root().join("envelopes")).unwrap();
    }

    fn temp_server(tag: &str) -> (Arc<Server>, std::path::PathBuf) {
        let dir = std::env::temp_dir().join(format!("coppice-rs-fence-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("data")).unwrap();
        let s = Server::new(super::super::Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.join("data"),
            start_dir: dir.clone(),
            ..Default::default()
        });
        (s, dir)
    }

    #[test]
    fn auto_follows_the_gate_and_a_disarmed_gate_is_off() {
        let (s, dir) = temp_server("auto");
        let rec = layout::Pane { harness: "claude".into(), cwd: "/work".into(), ..Default::default() };
        assert!(s.fence_wrap_with(&rec, "daisugi".into(), "/t").unwrap().is_empty());
        armed(&s);
        std::fs::write(dir.join("fakebin"), "").unwrap();
        let bin = dir.join("fakebin").to_string_lossy().into_owned();
        let w = s.fence_wrap_with(&rec, bin.clone(), "/t").unwrap();
        assert_eq!(w[0], bin);
        assert_eq!(w[1], "fence");
        assert_eq!(w.last().map(String::as_str), Some("--"));
        for want in [
            ["--harness", "claude"],
            ["--workspace", "/work"],
        ] {
            assert!(w.windows(2).any(|p| p == want), "{want:?} in {w:?}");
        }
        let root = s.gate_root().to_string_lossy().into_owned();
        assert!(w.windows(2).any(|p| p[0] == "--gate-root" && p[1] == root), "{w:?}");
        std::fs::write(s.gate_root().join("DISARMED"), "").unwrap();
        assert!(s.fence_wrap_with(&rec, bin, "/t").unwrap().is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_missing_daisugi_refuses_instead_of_running_bare() {
        let (s, dir) = temp_server("missing");
        let rec = layout::Pane { harness: "claude".into(), fence: "on".into(), ..Default::default() };
        let e = s.fence_wrap_with(&rec, "/no/such/daisugi".into(), "/t").unwrap_err();
        assert!(e.contains("fence = \"off\""), "{e}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_sprig_session_dir_stays_reachable() {
        let (s, dir) = temp_server("sprig");
        std::fs::write(dir.join("fakebin"), "").unwrap();
        let bin = dir.join("fakebin").to_string_lossy().into_owned();
        let rec = layout::Pane {
            harness: "sprig".into(),
            fence: "on".into(),
            argv: Some(vec!["--session-dir".into(), "/data/sess".into()]),
            ..Default::default()
        };
        let w = s.fence_wrap_with(&rec, bin, "/t").unwrap();
        assert_eq!(&w[w.len() - 3..], ["--workdir", "/data/sess", "--"]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_wrapped_command_starts_the_wrap_then_the_harness() {
        let wrap: Vec<String> = ["/bin/sh", "-c", "exec \"$@\"", "sh"].iter().map(|s| s.to_string()).collect();
        let cmd = crate::adapters::command("echo", &["x".to_string()], "", vec![], &wrap).unwrap();
        assert_eq!(cmd.get_program(), "/bin/sh");
        let args: Vec<_> = cmd.get_args().map(|a| a.to_string_lossy().into_owned()).collect();
        assert_eq!(args, ["-c", "exec \"$@\"", "sh", "echo", "x"]);
        let bare = crate::adapters::command("/bin/echo", &["x".to_string()], "", vec![], &[]).unwrap();
        assert_eq!(bare.get_program(), "/bin/echo");
    }

    #[test]
    fn n3_the_watch_covers_what_runs_later_and_follows_a_gitfile() {
        let (_s, dir) = temp_server("watch");
        let base = std::fs::canonicalize(&dir).unwrap();
        let ws = base.join("ws");
        let common = base.join("main.git");
        let wt = common.join("worktrees").join("ws");
        for p in [ws.clone(), wt.join("hooks"), common.join("hooks"), ws.join(".claude/commands"), ws.join(".codex"), ws.join(".opencode/plugin")] {
            std::fs::create_dir_all(p).unwrap();
        }
        let s = |p: &std::path::Path| p.to_string_lossy().into_owned();
        for (p, b) in [
            (ws.join(".git"), format!("gitdir: {}\n", s(&wt))),
            (wt.join("commondir"), "../..\n".to_string()),
            (ws.join(".mcp.json"), "{}".into()),
            (ws.join(".envrc"), "x".into()),
            (ws.join(".gitmodules"), "x".into()),
            (ws.join("opencode.json"), "{}".into()),
            (ws.join(".claude/commands/c.md"), "x".into()),
            (ws.join(".codex/config.toml"), "x".into()),
            (ws.join(".opencode/plugin/p.ts"), "x".into()),
            (wt.join("config"), "x".into()),
            (common.join("config"), "x".into()),
            (common.join("hooks/pre-commit"), "x".into()),
        ] {
            std::fs::write(p, b).unwrap();
        }
        let before = workspace_watched(&s(&ws));
        for k in [
            ".git".to_string(),
            ".mcp.json".into(),
            ".envrc".into(),
            ".gitmodules".into(),
            "opencode.json".into(),
            ".claude/commands/c.md".into(),
            ".codex/config.toml".into(),
            ".opencode/plugin/p.ts".into(),
            s(&wt.join("config")),
            s(&common.join("config")),
            s(&common.join("hooks/pre-commit")),
        ] {
            assert!(before.contains_key(&k), "{k} not watched: {before:?}");
        }
        std::fs::write(common.join("hooks/post-checkout"), "x").unwrap();
        assert_eq!(workspace_changes(&before, &workspace_watched(&s(&ws))), [s(&common.join("hooks/post-checkout"))]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn n3_a_change_holds_new_starts_until_the_ack() {
        let (s, dir) = temp_server("hold");
        let ws = dir.join("ws");
        std::fs::create_dir_all(&ws).unwrap();
        let w = ws.to_string_lossy().into_owned();
        let rec = layout::Pane { cwd: w.clone(), ..Default::default() };
        assert!(s.hold_error(&rec).is_none());
        s.add_hold(&w, &[".mcp.json".to_string()]);
        let e = s.hold_error(&rec).unwrap();
        assert!(e.starts_with("workspace config changed: a fenced run changed .mcp.json in "), "{e}");
        // On disk: a second server on the same data dir holds too.
        let s2 = Server::new(super::super::Config { data_dir: dir.join("data"), ..Default::default() });
        assert!(s2.hold_error(&rec).is_some());
        assert_eq!(operator_verb_refusal("pane.ack_changed"), Some(ACK_REFUSAL));
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// R2-4: a hold covers the workspace and every dir under it, it is set
    /// as soon as a start finds the change (the fenced pane still runs),
    /// and the refusal names the held workspace, which the operator acks.
    #[test]
    fn r2_4_a_hold_covers_subdirs_and_is_set_while_the_fenced_pane_runs() {
        let (s, dir) = temp_server("hold-sub");
        let base = std::fs::canonicalize(&dir).unwrap();
        let ws = base.join("ws");
        for d in [ws.join(".git/hooks"), ws.join("sub"), base.join("wsx")] {
            std::fs::create_dir_all(d).unwrap();
        }
        let w = ws.to_string_lossy().into_owned();
        let rec = layout::Pane { id: "w1:p1".into(), cwd: w.clone(), ..Default::default() };
        s.put_live("w1:p1", Arc::new(super::super::LivePane::fenced_for_test(&rec)));
        std::fs::write(ws.join(".git/hooks/pre-commit"), "#!/bin/sh\n").unwrap();
        let sub = layout::Pane { cwd: ws.join("sub").to_string_lossy().into_owned(), ..Default::default() };
        let e = s.hold_error(&sub).expect("a start under a changed workspace waits");
        assert!(e.contains("changed .git/hooks/pre-commit in ") && e.ends_with(&format!("ack-changed {w}")), "{e}");
        let sib = layout::Pane { cwd: base.join("wsx").to_string_lossy().into_owned(), ..Default::default() };
        assert!(s.hold_error(&sib).is_none(), "a sibling is held");
        let line = serde_json::json!({"id": "a1", "cmd": "pane.ack_changed", "cwd": w}).to_string();
        let ack = s.handle_ack_changed(&crate::proto::decode_line(&line).0);
        assert!(ack.ok, "ack");
        assert!(s.hold_error(&sub).is_none(), "held again after the ack");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn n5_each_run_gets_a_fresh_temp_leaf() {
        let (s, dir) = temp_server("leaf");
        let (a, b) = (s.fence_tmp("w1:p1"), s.fence_tmp("w1:p1"));
        assert_ne!(a, b);
        assert!(a.contains("/fence-tmp/w1_p1-"), "{a}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn n4_a_workers_sprig_fence_grants_no_write_over_its_gate_root() {
        let (s, dir) = temp_server("n4");
        armed(&s);
        let bin = fake_bin(&dir);
        let root = "/data/foreman/work/gate";
        let w = s.sprig_fence_with("/repo", root, bin.clone()).unwrap().unwrap();
        assert_ne!(w.last().map(String::as_str), Some("--"), "{w:?}");
        assert_eq!(w.iter().filter(|a| *a == "--gate-root").count(), 1, "{w:?}");
        assert!(has_pair(&w, "--gate-root", root) && has_pair(&w, "--workspace", "/repo"), "{w:?}");
        assert!(!w.iter().any(|a| a == "--allow" || a == "--workdir"), "a write grant: {w:?}");
        std::fs::remove_dir_all(s.gate_root().join("envelopes")).unwrap();
        assert!(s.sprig_fence_with("/repo", root, bin).unwrap().is_none());
        let _ = std::fs::remove_dir_all(&dir);
    }

    fn fake_bin(dir: &std::path::Path) -> String {
        std::fs::write(dir.join("fakebin"), "").unwrap();
        dir.join("fakebin").to_string_lossy().into_owned()
    }

    fn has_pair(w: &[String], a: &str, b: &str) -> bool {
        w.windows(2).any(|p| p[0] == a && p[1] == b)
    }

    /// FN-V-2, FN-V-9: the wrap names this floor's socket and a private tmp.
    #[test]
    fn fn_v2_v9_the_wrap_names_the_socket_and_a_private_tmp() {
        let (s, dir) = temp_server("tmp");
        let bin = fake_bin(&dir);
        let rec = layout::Pane { id: "w1:p1".into(), harness: "claude".into(), fence: "on".into(), cwd: "/w".into(), ..Default::default() };
        let leaf = s.fence_tmp("w1:p1");
        let w = s.fence_wrap_with(&rec, bin, &leaf).unwrap();
        assert!(has_pair(&w, "--coppice-socket", &s.cfg.socket_path.to_string_lossy()), "{w:?}");
        let tmp = format!("{}/fence-tmp/w1_p1-", s.cfg.data_dir.to_string_lossy());
        assert!(leaf.starts_with(&tmp) && has_pair(&w, "--tmpdir", &leaf), "{w:?}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// FN-V-5: only a missing envelopes dir turns the gate off.
    #[test]
    fn fn_v5_an_unreadable_gate_root_still_counts_as_on() {
        use std::os::unix::fs::PermissionsExt;
        let (s, dir) = temp_server("chmod");
        armed(&s);
        let root = s.gate_root();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0)).unwrap();
        let on = s.gate_on();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        assert!(on, "a chmod 000 gate root reads as off");
        std::fs::remove_dir_all(root.join("envelopes")).unwrap();
        assert!(!s.gate_on());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// FN-V-6: a fenced claude pane gets the pinned --settings document, the
    /// same bytes as the Go server writes.
    #[test]
    fn fn_v6_a_fenced_claude_pane_gets_the_pinned_settings() {
        let (s, dir) = temp_server("settings");
        armed(&s);
        let wrap = vec!["/opt/bin/daisugi".to_string(), "fence".into(), "--".into()];
        let rec = layout::Pane { harness: "claude".into(), ..Default::default() };
        let got = s.fenced_claude_argv(&rec, &wrap, vec!["claude".into(), "--model".into(), "x".into()], true);
        let root = s.gate_root().to_string_lossy().into_owned();
        let want = format!(
            "{{\"disableAllHooks\":false,\"hooks\":{{\"ConfigChange\":[{{\"hooks\":[{{\"command\":\"exit 2\",\"type\":\"command\"}}]}}],\
             \"PreToolUse\":[{{\"hooks\":[{{\"command\":\"DAISUGI_GATE_HOOK=opendaisugi.gate '/opt/bin/daisugi' gate check --root '{root}' --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2\",\
             \"onFailure\":\"block\",\"timeout\":105,\"type\":\"command\"}}],\"matcher\":\"*\"}}]}}}}"
        );
        assert_eq!(got, vec!["claude".to_string(), "--settings".into(), want, "--model".into(), "x".into()]);
        let adapter = s.fenced_claude_argv(&rec, &wrap, vec!["-p".into()], false);
        assert_eq!((adapter[0].as_str(), adapter[2].as_str()), ("--settings", "-p"));
        assert_eq!(s.fenced_claude_argv(&rec, &[], vec!["claude".into()], true).len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// FN-V-1: the env a fenced pane asks for cannot move HOME or turn the
    /// fence off; a caller that may be fenced types only into fenced panes.
    #[test]
    fn fn_v1_pinned_env_and_who_may_be_fenced() {
        let env: BTreeMap<String, String> = [("HOME", "/elsewhere"), ("DAISUGI_FENCE", "off"), ("XDG_DATA_HOME", "/x"), ("K", "v")]
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        let got = pin_fenced_env(&env);
        assert_eq!(got.into_iter().collect::<Vec<_>>(), vec![("K".to_string(), "v".to_string())]);
        let (s, dir) = temp_server("caller");
        let plugin = Role { no_allow: true, plugin: "p".into(), ..Default::default() };
        let unknown = Role { no_allow: true, unknown: true, ..Default::default() };
        let unnamed = Role { no_allow: true, pane: true, ..Default::default() };
        let bare = Role { no_allow: true, pane: true, pane_id: "w1:p1".into(), ..Default::default() };
        for r in [&plugin, &unknown, &unnamed] {
            assert!(s.caller_fenced(r));
            assert_eq!(s.fence_guard(r, true, "w1:p2"), Some(TYPE_FENCE_REFUSAL));
        }
        assert!(!s.caller_fenced(&bare), "a pane the server places as unfenced");
        assert_eq!(s.fence_guard(&bare, true, "w1:p2"), None);
        assert_eq!(s.fence_guard(&plugin, false, "w1:p2"), None);
        assert_eq!(fence_or("", true), "on");
        assert_eq!(fence_or("off", false), "off");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// FN-V-6: the watched workspace files and what changed.
    #[test]
    fn fn_v6_the_workspace_watch_sees_a_planted_hook() {
        let dir = std::env::temp_dir().join(format!("coppice-rs-ws-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join(".git/hooks")).unwrap();
        std::fs::write(dir.join(".git/config"), "[core]\n").unwrap();
        let d = dir.to_string_lossy().into_owned();
        let before = workspace_watched(&d);
        std::fs::write(dir.join(".git/hooks/pre-commit"), "#!/bin/sh\n").unwrap();
        std::fs::write(dir.join(".git/config"), "[core]\nhooksPath=x\n").unwrap();
        std::fs::create_dir_all(dir.join(".claude")).unwrap();
        std::fs::write(dir.join(".claude/settings.local.json"), "{}").unwrap();
        let changed = workspace_changes(&before, &workspace_watched(&d));
        assert_eq!(changed, vec![".claude/settings.local.json", ".git/config", ".git/hooks/pre-commit"]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// FN-V-5: a foreman worker runs under the fence while the gate is on.
    #[test]
    fn fn_v5_a_worker_is_wrapped_while_the_gate_is_on() {
        let (s, dir) = temp_server("worker");
        let bin = fake_bin(&dir);
        assert!(s.fence_cmd_with("", "/repo/wt", &["/data/work".into()], true, bin.clone()).unwrap().is_empty());
        armed(&s);
        let w = s.fence_cmd_with("", "/repo/wt", &["/data/work".into()], true, bin).unwrap();
        for (a, b) in [("--workspace", "/repo/wt"), ("--allow", "/data/work:rw"), ("--keep-fd", "3")] {
            assert!(has_pair(&w, a, b), "{a} {b}: {w:?}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }
}
