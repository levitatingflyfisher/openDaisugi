//! A pane's harness under `daisugi fence`: when it is fenced and the
//! command prefix that does it. The Go server's `internal/server/fence.go`
//! is the reference.

use super::{role::Role, Server};
use crate::layout;
use std::collections::BTreeMap;

/// The harness names `daisugi fence` carries a table for.
const FENCE_HARNESSES: &[&str] = &["claude", "codex", "pi", "opencode", "sprig", "hermes"];

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
pub(super) fn warden_armed(root: &std::path::Path) -> bool {
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
    for (d, list) in HARNESS_WATCHED {
        for (name, depth) in *list {
            w.add(&format!("{}/{d}/{name}", w.dir), *depth);
        }
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

/// What the watch reads inside a harness config dir: an allowlist of the
/// files that change how the harness behaves (settings, hooks, MCP config,
/// instruction files, commands, agents, skills, plugin config). The rest of
/// the dir is state the harness writes while it runs. A depth of 0 is one
/// file; 1 is the files directly in a dir; more walks the tree. The Go
/// server's harnessWatched is the reference.
const HARNESS_WATCHED: &[(&str, &[(&str, u32)])] = &[
    (
        ".claude",
        &[
            ("settings.json", 0),
            ("settings.local.json", 0),
            ("CLAUDE.md", 0),
            ("CLAUDE.local.md", 0),
            (".mcp.json", 0),
            ("mcp.json", 0),
            ("keybindings.json", 0),
            ("hooks", WATCH_DEPTH),
            ("commands", WATCH_DEPTH),
            ("agents", WATCH_DEPTH),
            ("skills", WATCH_DEPTH),
            ("output-styles", WATCH_DEPTH),
            ("plugins", 1),
        ],
    ),
    (
        ".codex",
        &[
            ("config.toml", 0),
            ("hooks.json", 0),
            ("AGENTS.md", 0),
            ("AGENTS.override.md", 0),
            ("prompts", WATCH_DEPTH),
            ("skills", WATCH_DEPTH),
            ("agents", WATCH_DEPTH),
            ("hooks", WATCH_DEPTH),
            ("rules", WATCH_DEPTH),
        ],
    ),
    (
        ".opencode",
        &[
            ("opencode.json", 0),
            ("opencode.jsonc", 0),
            ("AGENTS.md", 0),
            ("agent", WATCH_DEPTH),
            ("agents", WATCH_DEPTH),
            ("command", WATCH_DEPTH),
            ("commands", WATCH_DEPTH),
            ("plugin", WATCH_DEPTH),
            ("plugins", WATCH_DEPTH),
            ("tool", WATCH_DEPTH),
            ("tools", WATCH_DEPTH),
            ("mode", WATCH_DEPTH),
            ("skill", WATCH_DEPTH),
            ("skills", WATCH_DEPTH),
            ("package.json", 0),
        ],
    ),
];

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

/// What a fenced pane started in the home folder gets, or None. `daisugi
/// fence` shuts the home folder because it holds ~/.ssh, ~/.age and the
/// like, so the refusal comes in any case; this says it first, in one line
/// that names what to do.
fn home_folder_refusal(cwd: &str) -> Option<String> {
    home_refusal_for(&crate::sys::home_dir()?.to_string_lossy(), cwd)
}

fn home_refusal_for(home: &str, cwd: &str) -> Option<String> {
    if cwd.is_empty() || home.is_empty() || ws_key(cwd) != ws_key(home) {
        return None;
    }
    Some(format!(
        "the fence refuses your home folder ({cwd}), because it holds secrets such as ~/.ssh and ~/.age. Open the pane in a project folder, for example under ~/code, or set fence = \"off\" for that pane."
    ))
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

/// The one line the floor shows while agent work runs with the warden on and
/// no fence. The Python oracle holds the same text (fence_notice.py).
/// The Landlock ABI `daisugi fence` needs; below it the fence refuses to start
/// unless told to allow weak IPC, which coppice never does.
pub const FENCE_MIN_ABI: i64 = 9;

/// The line for "auto, but this kernel cannot fence": the setting is already
/// right, so it names the kernel instead of telling the operator to set it.
pub fn unconfined_no_kernel_line(abi: i64) -> String {
    let what = if abi <= 0 {
        "no Landlock, the fence needs ABI 9".to_string()
    } else {
        format!("Landlock ABI {abi}, the fence needs 9")
    };
    format!(
        "Files are not confined: the warden checks commands, but only a fence stops a file access. This kernel cannot fence ({what})."
    )
}

pub const UNCONFINED_LINE: &str = "Files are not confined: the warden checks commands, but only a fence stops \
a file access. Turn the fence on with fence = \"auto\" in coppice.toml.";

impl Server {
    /// Sets the fence setting from the config: "auto", "on" or "off".
    pub fn set_fence_mode(&self, mode: &str) {
        *self.fence_mode.lock().unwrap_or_else(|e| e.into_inner()) = mode.to_string();
    }

    /// Sets the home whose guarded data dirs hold warden roots.
    pub fn set_hook_home(&self, home: std::path::PathBuf) {
        *self.hook_home.lock().unwrap_or_else(|e| e.into_inner()) = home;
    }

    fn fence_mode(&self) -> String {
        self.fence_mode.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// The warden roots this floor's panes may use: the one beside the data
    /// dir, then each guarded data dir's warden under the hook home. The
    /// data-dir variables count only for the user's own home.
    pub(super) fn warden_roots(&self) -> Vec<std::path::PathBuf> {
        let mut out = vec![self.warden_root()];
        let home = self.hook_home.lock().unwrap_or_else(|e| e.into_inner()).to_string_lossy().into_owned();
        if !home.starts_with('/') {
            return out;
        }
        let mut add = |p: std::path::PathBuf| {
            if !out.contains(&p) {
                out.push(p);
            }
        };
        add(std::path::PathBuf::from(format!("{home}/.opendaisugi/warden")));
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
                add(std::path::PathBuf::from(format!("{d}/warden")));
            }
            if let Some(x) = std::env::var("XDG_DATA_HOME").ok().and_then(usable) {
                add(std::path::PathBuf::from(format!("{x}/opendaisugi/warden")));
            }
        }
        out
    }

    /// True when any warden root this floor's panes may use is armed.
    pub(super) fn warden_on(&self) -> bool {
        self.warden_roots().iter().any(|r| warden_armed(r))
    }

    /// Whether a pane's harness starts under the fence: the pane's own
    /// setting first, then the config's. Under "auto" the warden decides. A
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
            _ => self.auto_fence(),
        }
    }

    /// The Landlock ABI this kernel reports (0 for none): the version call
    /// `daisugi fence` makes first.
    pub(super) fn landlock_abi(&self) -> i64 {
        if let Some(v) = *self.landlock_override.lock().unwrap_or_else(|e| e.into_inner()) {
            return v;
        }
        // SAFETY: landlock_create_ruleset(NULL, 0, VERSION) reads no memory.
        let v = unsafe { libc::syscall(444, 0usize, 0usize, 1u32) };
        i64::from(v).max(0)
    }

    /// Whether this kernel can run `daisugi fence`: it needs ABI 9, and
    /// coppice never allows weak IPC (that would let a child reach D-Bus or
    /// the user systemd outside the fence).
    pub(super) fn landlock_usable(&self) -> bool {
        self.landlock_abi() >= FENCE_MIN_ABI
    }

    /// A test sets the ABI the kernel answers.
    #[cfg(test)]
    pub(super) fn set_landlock_abi_for_test(&self, abi: i64) {
        *self.landlock_override.lock().unwrap_or_else(|e| e.into_inner()) = Some(abi);
    }

    /// A test sets whether the kernel can fence (ABI 9 or none).
    #[cfg(test)]
    pub(super) fn set_landlock_for_test(&self, ok: bool) {
        self.set_landlock_abi_for_test(if ok { FENCE_MIN_ABI } else { 0 });
    }

    /// The "auto" decision: fence while the warden is on and this kernel can
    /// fence. A kernel that cannot runs the work bare and the floor says
    /// so (`unconfined_notice`). Only "on" fails closed (ADR-0028).
    fn auto_fence(&self) -> bool {
        self.warden_on() && self.landlock_usable()
    }

    /// The one-line notice while agent work runs with the warden on and no
    /// fence: the config says off (turn it on), or it says auto and the
    /// kernel cannot fence (name the kernel). None otherwise.
    pub fn unconfined_notice(&self) -> Option<String> {
        if !self.warden_on() {
            return None;
        }
        match self.fence_mode().as_str() {
            "off" => Some(UNCONFINED_LINE.to_string()),
            "on" => None,
            _ => {
                let abi = self.landlock_abi();
                (abi < FENCE_MIN_ABI).then(|| unconfined_no_kernel_line(abi))
            }
        }
    }

    /// Whether agent work runs with the warden on and no fence.
    pub fn unconfined(&self) -> bool {
        self.unconfined_notice().is_some()
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
        if let Some(msg) = home_folder_refusal(&rec.cwd) {
            return Err(msg);
        }
        let mut w = vec![path, "fence".to_string()];
        if FENCE_HARNESSES.contains(&rec.harness.as_str()) {
            w.push("--harness".into());
            w.push(rec.harness.clone());
        }
        if !rec.cwd.is_empty() {
            w.push("--workspace".into());
            w.push(rec.cwd.clone());
        }
        w.push("--warden-root".into());
        w.push(self.warden_root().to_string_lossy().into_owned());
        w.push("--tmpdir".into());
        w.push(tmp.to_string());
        w.push("--coppice-socket".into());
        w.push(self.cfg.socket_path.to_string_lossy().into_owned());
        // The steward's own tools live on the standing socket, in the
        // steward log dir. A fenced steward must reach it to have its tools.
        if super::role::is_steward_label(&rec.label) {
            w.push("--coppice-socket".into());
            w.push(self.standing_sock_path().to_string_lossy().into_owned());
        }
        // A pane of a Pando branch reads its branch and everything below it,
        // read only; the rest of the memory stays shut at the kernel.
        if let Some(d) = self.pane_branch_dir(rec) {
            w.push("--allow".into());
            w.push(d.to_string_lossy().into_owned());
        }
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

    /// The prefix that runs a steward worker or turn under the fence
    /// (FN-R-17), or empty when the warden is off and the config does not
    /// say on.
    pub(super) fn fence_cmd(&self, harness: &str, dir: &str, allow: &[String], keep_fd3: bool) -> Result<Vec<String>, String> {
        self.fence_cmd_sockets(harness, dir, allow, keep_fd3, &[])
    }

    /// `fence_cmd` with more sockets the command may connect to.
    pub(super) fn fence_cmd_sockets(
        &self,
        harness: &str,
        dir: &str,
        allow: &[String],
        keep_fd3: bool,
        sockets: &[String],
    ) -> Result<Vec<String>, String> {
        self.fence_cmd_full(harness, dir, allow, keep_fd3, sockets, daisugi_bin())
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
        self.fence_cmd_full(harness, dir, allow, keep_fd3, &[], bin)
    }

    /// `fence_cmd` with the sockets and the daisugi binary named.
    pub(super) fn fence_cmd_full(
        &self,
        harness: &str,
        dir: &str,
        allow: &[String],
        keep_fd3: bool,
        sockets: &[String],
        bin: String,
    ) -> Result<Vec<String>, String> {
        match self.fence_mode().as_str() {
            "off" => return Ok(vec![]),
            "on" => {}
            _ => {
                if !self.auto_fence() {
                    return Ok(vec![]);
                }
            }
        }
        let Some(path) = found(&bin) else {
            return Err(format!("the warden is on, so this runs under daisugi fence, and {bin} is not on PATH"));
        };
        let mut w = vec![path, "fence".to_string()];
        if !harness.is_empty() {
            w.push("--harness".into());
            w.push(harness.to_string());
        }
        w.push("--workspace".into());
        w.push(dir.to_string());
        w.push("--warden-root".into());
        w.push(self.warden_root().to_string_lossy().into_owned());
        for a in allow {
            w.push("--allow".into());
            w.push(format!("{a}:rw"));
        }
        w.push("--coppice-socket".into());
        w.push(self.cfg.socket_path.to_string_lossy().into_owned());
        for sock in sockets {
            w.push("--coppice-socket".into());
            w.push(sock.clone());
        }
        if keep_fd3 {
            w.push("--keep-fd".into());
            w.push("3".into());
        }
        w.push("--".into());
        Ok(w)
    }

    /// The `daisugi fence` prefix (no "--") a steward worker's sprig starts
    /// under, or None when the warden is off and the config does not say on.
    /// The warden root is the worker's own, so the fence starts its resident
    /// warden and keeps it read only; the tree run adds sprig's session dir
    /// and its key descriptors (FN-R-25).
    pub(super) fn sprig_fence(&self, dir: &str, warden_root: &str) -> Result<Option<Vec<String>>, String> {
        self.sprig_fence_with(dir, warden_root, daisugi_bin())
    }

    /// `sprig_fence` with the daisugi binary named.
    pub(super) fn sprig_fence_with(&self, dir: &str, warden_root: &str, bin: String) -> Result<Option<Vec<String>>, String> {
        let mut w = self.fence_cmd_with("", dir, &[], false, bin)?;
        if w.is_empty() {
            return Ok(None);
        }
        w.pop(); // the "--"
        if let Some(i) = w.iter().position(|a| a == "--warden-root") {
            w.drain(i..(i + 2).min(w.len()));
        }
        w.push("--warden-root".into());
        w.push(warden_root.to_string());
        Ok(Some(w))
    }

    /// The --settings document a fenced claude pane starts with (FN-R-13).
    pub(super) fn claude_settings(&self, daisugi: &str, session: &str) -> String {
        let root = self.warden_root();
        let root = if warden_armed(&root) {
            root
        } else {
            self.warden_roots().into_iter().find(|r| warden_armed(r)).unwrap_or(root)
        };
        let pin = if session.is_empty() {
            String::new()
        } else {
            format!(" --session {}", shell_quote(session))
        };
        let cmd = format!(
            "DAISUGI_WARDEN_HOOK=opendaisugi.warden {} warden check --root {}{pin} --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2",
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
    pub(super) fn fenced_claude_argv(&self, rec: &layout::Pane, wrap: &[String], argv: Vec<String>, pty: bool, session: &str) -> Vec<String> {
        if wrap.is_empty() || rec.harness != "claude" {
            return argv;
        }
        let settings = vec!["--settings".to_string(), self.claude_settings(&wrap[0], session)];
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
        std::fs::create_dir_all(s.warden_root().join("envelopes")).unwrap();
    }

    fn temp_server(tag: &str) -> (Arc<Server>, std::path::PathBuf) {
        let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-fence-{tag}-{}", std::process::id())));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("data")).unwrap();
        let s = Server::new(super::super::Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.join("data"),
            start_dir: dir.clone(),
            ..Default::default()
        });
        s.set_landlock_for_test(true);
        (s, dir)
    }

    /// ADR-0028: under auto a kernel without Landlock runs agent work bare
    /// and the floor says files are not confined. "on" stays fail closed.
    #[test]
    fn auto_runs_bare_on_a_kernel_without_landlock() {
        let (s, dir) = temp_server("nolandlock");
        armed(&s);
        let bin = fake_bin(&dir);
        s.set_landlock_for_test(false);
        let rec = layout::Pane { id: "w1:p1".into(), harness: "claude".into(), cwd: "/w".into(), ..Default::default() };
        for mode in ["", "auto"] {
            s.set_fence_mode(mode);
            assert!(!s.fence_on(&rec), "{mode:?}");
            assert!(s.fence_cmd_with("", "/w", &[], false, bin.clone()).unwrap().is_empty(), "{mode:?}");
            assert!(s.unconfined(), "{mode:?}");
        }
        s.set_fence_mode("on");
        assert!(s.fence_on(&rec));
        assert!(!s.unconfined());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Review F2: `daisugi fence` needs Landlock ABI 9. Under auto a kernel
    /// below it runs agent work bare and the notice names the kernel; "on"
    /// stays fail closed; coppice never adds --allow-weak-ipc.
    #[test]
    fn auto_fences_only_at_landlock_abi_9() {
        let (s, dir) = temp_server("abi9");
        armed(&s);
        let bin = fake_bin(&dir);
        let rec = layout::Pane { id: "w1:p1".into(), harness: "claude".into(), cwd: "/w".into(), ..Default::default() };
        for (abi, fence) in [(0, false), (3, false), (8, false), (9, true), (10, true)] {
            s.set_landlock_abi_for_test(abi);
            s.set_fence_mode("auto");
            assert_eq!(s.fence_on(&rec), fence, "ABI {abi}");
            let cmd = s.fence_cmd_with("", "/w", &[], false, bin.clone()).unwrap();
            assert_eq!(!cmd.is_empty(), fence, "ABI {abi}");
            assert!(cmd.iter().all(|a| !a.contains("weak-ipc")), "ABI {abi}: {cmd:?}");
            assert_eq!(s.unconfined(), !fence, "ABI {abi}");
            let line = s.unconfined_notice();
            if fence {
                assert_eq!(line, None, "ABI {abi}");
            } else {
                let line = line.unwrap();
                assert!(line.contains("cannot fence") && line.contains("needs"), "{line}");
                assert!(!line.contains("Turn the fence on"), "{line}");
            }
            s.set_fence_mode("on");
            assert!(s.fence_on(&rec), "ABI {abi}: on is fail closed");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The no-fence-kernel notice names the ABI and does not say to set auto.
    #[test]
    fn the_no_fence_kernel_notice_names_the_abi() {
        let (s, dir) = temp_server("noticeabi");
        armed(&s);
        s.set_fence_mode("auto");
        s.set_landlock_abi_for_test(3);
        assert_eq!(
            s.unconfined_notice().unwrap(),
            "Files are not confined: the warden checks commands, but only a fence stops a file access. \
             This kernel cannot fence (Landlock ABI 3, the fence needs 9)."
        );
        s.set_landlock_abi_for_test(0);
        assert_eq!(
            s.unconfined_notice().unwrap(),
            "Files are not confined: the warden checks commands, but only a fence stops a file access. \
             This kernel cannot fence (no Landlock, the fence needs ABI 9)."
        );
        s.set_fence_mode("off");
        assert_eq!(s.unconfined_notice().unwrap(), UNCONFINED_LINE);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The notice shows only with the warden on and the config at off.
    #[test]
    fn unconfined_names_the_off_config_only_with_the_warden_on() {
        let (s, dir) = temp_server("unconfined");
        s.set_fence_mode("off");
        assert!(!s.unconfined());
        armed(&s);
        assert!(s.unconfined());
        s.set_fence_mode("auto");
        assert!(!s.unconfined());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn auto_follows_the_warden_and_a_disarmed_warden_is_off() {
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
        let root = s.warden_root().to_string_lossy().into_owned();
        assert!(w.windows(2).any(|p| p[0] == "--warden-root" && p[1] == root), "{w:?}");
        std::fs::write(s.warden_root().join("DISARMED"), "").unwrap();
        assert!(s.fence_wrap_with(&rec, bin, "/t").unwrap().is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Round 4: the watch reads a harness's config, not its state; Codex
    /// rules and OpenCode's package.json count as config.
    #[test]
    fn the_watch_skips_harness_state_but_keeps_harness_config() {
        let (_s, dir) = temp_server("watchstate");
        let ws = dir.join("ws");
        for p in [".claude/projects/x", ".claude/todos", ".claude/hooks", ".codex/sessions", ".codex/rules", ".opencode/plugin"] {
            std::fs::create_dir_all(ws.join(p)).unwrap();
        }
        let w = |p: &str, b: &str| std::fs::write(ws.join(p), b).unwrap();
        w(".claude/settings.json", "{}");
        w(".claude/hooks/h.sh", "a");
        w(".claude/projects/x/y.txt", "a");
        w(".claude/todos/t.json", "a");
        w(".codex/sessions/s.jsonl", "a");
        w(".codex/rules/r.rules", "a");
        w(".opencode/package.json", "{}");
        let d = ws.to_string_lossy().into_owned();
        let before = workspace_watched(&d);
        w(".claude/projects/x/y.txt", "changed");
        w(".claude/todos/new.json", "new");
        w(".codex/sessions/s.jsonl", "changed");
        assert!(workspace_changes(&before, &workspace_watched(&d)).is_empty());
        w(".claude/settings.json", "{\"hooks\":{}}");
        w(".claude/hooks/h.sh", "b");
        w(".codex/rules/r.rules", "b");
        w(".opencode/package.json", "{\"dependencies\":{}}");
        assert_eq!(
            workspace_changes(&before, &workspace_watched(&d)),
            [".claude/hooks/h.sh", ".claude/settings.json", ".codex/rules/r.rules", ".opencode/package.json"]
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_home_folder_is_refused_in_one_line_and_hermes_and_the_steward_socket_are_known() {
        assert!(home_refusal_for("/home/u", "/home/u/code/x").is_none());
        assert!(home_refusal_for("/home/u", "").is_none());
        let m = home_refusal_for("/home/u", "/home/u").unwrap();
        assert!(m.contains("project folder") && m.contains("fence = \"off\"") && !m.contains('\n'), "{m}");
        let (s, dir) = temp_server("hermes");
        std::fs::write(dir.join("fakebin"), "").unwrap();
        let bin = dir.join("fakebin").to_string_lossy().into_owned();
        let rec = layout::Pane { harness: "hermes".into(), fence: "on".into(), cwd: "/work".into(), ..Default::default() };
        let w = s.fence_wrap_with(&rec, bin.clone(), "/t").unwrap();
        assert!(w.windows(2).any(|p| p == ["--harness", "hermes"]), "{w:?}");
        let socks = |w: &[String]| w.iter().filter(|a| *a == "--coppice-socket").count();
        assert_eq!(socks(&w), 1);
        let st = layout::Pane { harness: "claude".into(), fence: "on".into(), cwd: "/work".into(), label: "steward".into(), ..Default::default() };
        let w = s.fence_wrap_with(&st, bin, "/t").unwrap();
        let standing = s.standing_sock_path().to_string_lossy().into_owned();
        assert_eq!(socks(&w), 2, "{w:?}");
        assert!(w.windows(2).any(|p| p[0] == "--coppice-socket" && p[1] == standing), "{w:?}");
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
    fn n4_a_workers_sprig_fence_grants_no_write_over_its_warden_root() {
        let (s, dir) = temp_server("n4");
        armed(&s);
        let bin = fake_bin(&dir);
        let root = "/data/steward/work/warden";
        let w = s.sprig_fence_with("/repo", root, bin.clone()).unwrap().unwrap();
        assert_ne!(w.last().map(String::as_str), Some("--"), "{w:?}");
        assert_eq!(w.iter().filter(|a| *a == "--warden-root").count(), 1, "{w:?}");
        assert!(has_pair(&w, "--warden-root", root) && has_pair(&w, "--workspace", "/repo"), "{w:?}");
        assert!(!w.iter().any(|a| a == "--allow" || a == "--workdir"), "a write grant: {w:?}");
        std::fs::remove_dir_all(s.warden_root().join("envelopes")).unwrap();
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

    /// FN-V-5: only a missing envelopes dir turns the warden off.
    #[test]
    fn fn_v5_an_unreadable_warden_root_still_counts_as_on() {
        use std::os::unix::fs::PermissionsExt;
        let (s, dir) = temp_server("chmod");
        armed(&s);
        let root = s.warden_root();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0)).unwrap();
        let on = s.warden_on();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        assert!(on, "a chmod 000 warden root reads as off");
        std::fs::remove_dir_all(root.join("envelopes")).unwrap();
        assert!(!s.warden_on());
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
        let got = s.fenced_claude_argv(&rec, &wrap, vec!["claude".into(), "--model".into(), "x".into()], true, "");
        let root = s.warden_root().to_string_lossy().into_owned();
        let want = format!(
            "{{\"disableAllHooks\":false,\"hooks\":{{\"ConfigChange\":[{{\"hooks\":[{{\"command\":\"exit 2\",\"type\":\"command\"}}]}}],\
             \"PreToolUse\":[{{\"hooks\":[{{\"command\":\"DAISUGI_WARDEN_HOOK=opendaisugi.warden '/opt/bin/daisugi' warden check --root '{root}' --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2\",\
             \"onFailure\":\"block\",\"timeout\":105,\"type\":\"command\"}}],\"matcher\":\"*\"}}]}}}}"
        );
        assert_eq!(got, vec!["claude".to_string(), "--settings".into(), want, "--model".into(), "x".into()]);
        let adapter = s.fenced_claude_argv(&rec, &wrap, vec!["-p".into()], false, "");
        assert_eq!((adapter[0].as_str(), adapter[2].as_str()), ("--settings", "-p"));
        assert_eq!(s.fenced_claude_argv(&rec, &[], vec!["claude".into()], true, "").len(), 1);
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
        let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-ws-{}", std::process::id())));
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

    /// FN-V-5: a steward worker runs under the fence while the warden is on.
    #[test]
    fn fn_v5_a_worker_is_wrapped_while_the_warden_is_on() {
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

    /// The daisugi this repository builds, or COPPICE_TEST_DAISUGI; None
    /// when neither exists (the test then does nothing).
    fn real_daisugi() -> Option<String> {
        if let Ok(p) = std::env::var("COPPICE_TEST_DAISUGI") {
            return Some(p);
        }
        let p = format!("{}/../../clients/go/daisugi", env!("CARGO_MANIFEST_DIR"));
        std::fs::metadata(&p).ok().map(|_| p)
    }

    /// The warden hook line a claude pane gets, pinned to session ("" for
    /// an unpinned pane).
    fn hook_command(s: &Server, bin: &str, session: &str) -> String {
        let v: serde_json::Value = serde_json::from_str(&s.claude_settings(bin, session)).unwrap();
        v["hooks"]["PreToolUse"][0]["hooks"][0]["command"].as_str().unwrap().to_string()
    }

    /// A scratch warden root beside dir in enforce mode: allow maps a
    /// session ("" is the default envelope) to the one shell head it allows.
    fn scratch_warden(bin: &str, dir: &std::path::Path, allow: &[(&str, &str)]) -> std::path::PathBuf {
        let root = dir.join("warden");
        let home = dir.join("home");
        std::fs::create_dir_all(&home).unwrap();
        std::fs::write(dir.join("config.yaml"), "warden_mode: enforce\n").unwrap();
        for (session, head) in allow {
            let name = if session.is_empty() { "default" } else { session };
            let file = dir.join(format!("{name}.json"));
            std::fs::write(
                &file,
                format!(
                    "{{\"id\":\"env_{name}\",\"generated_by\":\"t\",\"task\":\"t\",\"permissions\":{{\"shell\":true,\"shell_allowlist\":[\"{head}\"],\"file_read\":[\"/tmp\",\"/tmp/**\"]}}}}"
                ),
            )
            .unwrap();
            let mut c = std::process::Command::new(bin);
            c.args(["warden", "register", &file.to_string_lossy(), "--root", &root.to_string_lossy()]);
            if !session.is_empty() {
                c.args(["--session", session]);
            }
            let out = c.env("HOME", &home).output().unwrap();
            assert!(out.status.success(), "{}", String::from_utf8_lossy(&out.stderr));
        }
        root
    }

    /// Runs a hook line with one Claude PreToolUse payload on stdin.
    fn run_hook(cmd: &str, home: &std::path::Path, payload_session: &str, shell: &str) -> (i32, String) {
        use std::io::Write;
        let payload = format!(
            "{{\"session_id\":\"{payload_session}\",\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"tool_input\":{{\"command\":\"{shell}\"}},\"cwd\":\"/tmp\"}}"
        );
        let mut c = std::process::Command::new("sh")
            .args(["-c", cmd])
            .env("HOME", home)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .spawn()
            .unwrap();
        c.stdin.take().unwrap().write_all(payload.as_bytes()).unwrap();
        let out = c.wait_with_output().unwrap();
        let text = format!("{}{}", String::from_utf8_lossy(&out.stdout), String::from_utf8_lossy(&out.stderr));
        (out.status.code().unwrap_or(-1), text)
    }

    /// AT-1: a pane that names another pane's session in its hook payload is
    /// still decided by its own hook line; the payload never selects an
    /// envelope (SEC-3). The twin of warden_session_test.go.
    #[test]
    fn at1_a_pane_cannot_name_another_panes_session() {
        let Some(bin) = real_daisugi() else { return };
        let (s, dir) = temp_server("at1");
        let root = scratch_warden(&bin, &dir, &[("", "ls"), ("sA", "git")]);
        assert_eq!(root, s.warden_root());
        let home = dir.join("home");
        let hook_b = hook_command(&s, &bin, "");
        let hook_a = hook_command(&s, &bin, "sA");
        assert!(!hook_b.contains("--session"), "{hook_b}");
        assert_eq!(run_hook(&hook_b, &home, "sA", "git status").0, 2, "B decided under sA's envelope");
        assert_eq!(run_hook(&hook_b, &home, "sA", "ls").0, 0, "B was not decided under the default");
        assert_eq!(run_hook(&hook_a, &home, "nobody", "git status").0, 0, "A lost its envelope");
        assert_eq!(run_hook(&hook_a, &home, "default", "ls").0, 2, "A read the default envelope");
        let _ = std::fs::remove_dir_all(&dir);
        // A root with no default envelope: B is denied whatever the payload says.
        let (s2, dir2) = temp_server("at1b");
        scratch_warden(&bin, &dir2, &[("sA", "git")]);
        let (code, out) = run_hook(&hook_command(&s2, &bin, ""), &dir2.join("home"), "sA", "git status");
        assert!(code == 2 && out.contains("no envelope registered"), "{code} {out}");
        let _ = std::fs::remove_dir_all(&dir2);
    }

    /// A pane pinned to a task's session never reads the default envelope.
    #[test]
    fn a_pinned_pane_never_reads_default() {
        let Some(bin) = real_daisugi() else { return };
        let (s, dir) = temp_server("pinned");
        scratch_warden(&bin, &dir, &[("", "ls"), ("t1-aa", "git")]);
        let home = dir.join("home");
        let pinned = hook_command(&s, &bin, "t1-aa");
        assert!(pinned.contains("--session 't1-aa'"), "{pinned}");
        assert_eq!(run_hook(&pinned, &home, "x", "ls").0, 2);
        assert_eq!(run_hook(&hook_command(&s, &bin, ""), &home, "x", "ls").0, 0);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
