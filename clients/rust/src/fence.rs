//! `daisugi fence`: a Landlock ruleset around any agent harness, applied in
//! this process before it executes the harness, so every child inherits it
//! and none can lift it.
//!
//! `plan` is `opendaisugi/fence.py`, the pure rule list. It reads no file
//! and asks the kernel nothing; `clients/fixtures/fence` holds the oracle's
//! golden cases and the tests read them. `landlock` applies a list. The Go
//! client's `internal/fence` is the reference for the rest.
//!
//! The binary dispatches here before anything else loads, so the fence
//! costs one exec and a few system calls.

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::io::Write;

/// One grant. Mode "ro" is read and execute, "rw" is every file-system
/// right but the unix-socket connect right, "unix" is only that right,
/// "sock" is "ro" and "unix", and "rw+unix" is "rw" and "unix".
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Rule {
    pub path: String,
    pub mode: String,
}

#[derive(Serialize, Deserialize, Clone, Debug, Default, PartialEq)]
pub struct Envelope {
    #[serde(default)]
    pub file_read: Vec<String>,
    #[serde(default)]
    pub file_write: Vec<String>,
}

#[derive(Serialize, Deserialize, Clone, Debug, Default)]
pub struct Input {
    #[serde(default)]
    pub harness: String,
    pub workspace: String,
    pub home: String,
    #[serde(default)]
    pub other_homes: Vec<String>,
    #[serde(default)]
    pub tmpdir: String,
    #[serde(default)]
    pub env: HashMap<String, String>,
    #[serde(default)]
    pub path_dirs: Vec<String>,
    #[serde(default)]
    pub exe_dirs: Vec<String>,
    #[serde(default)]
    pub gate_root: String,
    #[serde(default)]
    pub envelope: Envelope,
    #[serde(default)]
    pub allow: Vec<String>,
    #[serde(default)]
    pub workdirs: Vec<String>,
    #[serde(default)]
    pub sockets: Vec<String>,
    #[serde(default)]
    pub real: HashMap<String, String>,
    #[serde(default)]
    pub never_extra: Vec<String>,
    #[serde(default)]
    pub state_extra: Vec<String>,
}

#[derive(Serialize, Deserialize, Clone, Debug, Default, PartialEq)]
pub struct Output {
    pub rules: Vec<Rule>,
    pub notes: Vec<String>,
    pub refusals: Vec<String>,
}

/// fence.SECRETS: paths under HOME that hold keys and tokens.
pub const SECRETS: &[&str] = &[
    ".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gh", ".config/gcloud",
    ".config/rclone", ".config/hub", ".config/doctl", ".config/op", ".config/Bitwarden",
    ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/Code",
    ".config/git/credentials", ".config/sops", ".config/age", ".config/huggingface", ".config/tailscale",
    ".mozilla", ".thunderbird", ".age", ".pki", ".ollama", ".cache/huggingface/token", ".netrc",
    ".npmrc", ".pypirc", ".git-credentials", ".password-store", ".local/share/keyrings",
    ".cargo/credentials", ".cargo/credentials.toml", ".gem/credentials", ".vault-token",
    ".terraform.d/credentials.tfrc.json",
];

/// fence.HARNESS: what a harness keeps for itself under HOME: its config
/// read only, and read and write on the parts it writes. ~/.claude.json and
/// OpenCode's config, binaries and plugin packages are read only (FN-R-22).
/// OpenCode's snapshot repos are read only, and of Claude's projects dir
/// only this workspace's own project dir is read and write (plan,
/// FN-R-31).
pub fn harness_paths(name: &str) -> &'static [(&'static str, &'static str)] {
    match name {
        "claude" => &[
            (".claude", "ro"),
            (".claude/todos", "rw"),
            (".claude/shell-snapshots", "rw"),
            (".claude/statsig", "rw"),
            (".claude/session-env", "rw"),
            (".claude/file-history", "rw"),
            (".claude/plans", "rw"),
            (".claude/debug", "rw"),
            (".claude/ide", "rw"),
            (".claude/.credentials.json", "rw"),
            (".claude.json", "ro"),
            (".config/claude", "ro"),
            (".cache/claude", "rw"),
            (".local/state/claude", "rw"),
            (".local/share/claude", "ro"),
        ],
        "codex" => &[
            (".codex", "ro"),
            (".codex/sessions", "rw"),
            (".codex/log", "rw"),
            (".codex/history.jsonl", "rw"),
            (".codex/auth.json", "rw"),
        ],
        "pi" => &[(".pi", "ro"), (".pi/agent/sessions", "rw")],
        "opencode" => &[
            (".config/opencode", "ro"),
            (".local/share/opencode", "ro"),
            (".local/share/opencode/storage", "rw"),
            (".local/share/opencode/log", "rw"),
            (".local/share/opencode/project", "rw"),
            (".local/share/opencode/auth.json", "rw"),
            (".local/state/opencode", "rw"),
            (".cache/opencode", "ro"),
        ],
        "sprig" => &[(".local/share/sprig", "ro")],
        _ => &[],
    }
}

/// fence.CLAUDE_PROJECT_NAME_MAX: past it Claude Code shortens the name
/// and adds a hash the fence cannot know.
pub const CLAUDE_PROJECT_NAME_MAX: usize = 200;

/// fence.claude_project_name: Claude Code's dir name for workspace under
/// ~/.claude/projects, every character but an ASCII letter or digit made
/// "-". None when the fence cannot name it (not ASCII, or too long).
pub fn claude_project_name(workspace: &str) -> Option<String> {
    if !workspace.is_ascii() || workspace.len() > CLAUDE_PROJECT_NAME_MAX {
        return None;
    }
    Some(workspace.chars().map(|c| if c.is_ascii_alphanumeric() { c } else { '-' }).collect())
}

const SYSTEM_RO: &[&str] = &[
    "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt", "/proc", "/sys",
    "/run/systemd/resolve",
];

const DEVICES: &[&str] = &[
    "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/ptmx",
    "/dev/pts",
];

/// fence.SOCKET_WIDE: a socket dir at or above one of these never takes
/// the connect right.
const SOCKET_WIDE: &[&str] = &["/tmp", "/run", "/var/run", "/dev"];

/// fence.MODES: each mode's rights (r read, w write, u socket connect).
const MODES: &[(&str, &str)] = &[("ro", "r"), ("rw", "rw"), ("unix", "u"), ("sock", "ru"), ("rw+unix", "rwu")];

fn mode_rights(m: &str) -> &'static str {
    MODES.iter().find(|(n, _)| *n == m).map(|(_, r)| *r).unwrap_or("")
}

/// fence.merge: the mode with the rights of both.
fn merge(a: &str, b: &str) -> String {
    let want: Vec<char> = "rwu".chars().filter(|c| mode_rights(a).contains(*c) || mode_rights(b).contains(*c)).collect();
    let want: String = want.into_iter().collect();
    MODES.iter().find(|(_, r)| *r == want).map(|(n, _)| n.to_string()).unwrap_or_else(|| a.to_string())
}

/// path.Clean for the paths this module sees.
pub fn norm(p: &str) -> String {
    let rooted = p.starts_with('/');
    let mut out: Vec<&str> = vec![];
    for part in p.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if matches!(out.last(), Some(l) if *l != "..") {
                    out.pop();
                } else if !rooted {
                    out.push("..");
                }
            }
            _ => out.push(part),
        }
    }
    let joined = out.join("/");
    if rooted {
        format!("/{joined}")
    } else if joined.is_empty() {
        ".".to_string()
    } else {
        joined
    }
}

fn dirname(p: &str) -> String {
    match p.rfind('/') {
        Some(0) => "/".to_string(),
        Some(i) => p[..i].to_string(),
        None => ".".to_string(),
    }
}

/// fence.expand: ~ and ~/ to home, a relative path joined to base.
pub fn expand(p: &str, home: &str, base: &str) -> String {
    let mut p = p.to_string();
    if p == "~" {
        p = home.to_string();
    } else if let Some(rest) = p.strip_prefix("~/") {
        p = format!("{home}/{rest}");
    }
    if !p.starts_with('/') {
        p = format!("{base}/{p}");
    }
    norm(&p)
}

pub fn covers(a: &str, b: &str) -> bool {
    if a == b {
        return true;
    }
    if a.ends_with('/') {
        b.starts_with(a)
    } else {
        b.starts_with(&format!("{a}/"))
    }
}

/// fence.glob_prefix: the longest literal directory of an envelope glob.
pub fn glob_prefix(pattern: &str, home: &str, base: &str) -> String {
    let mut p = pattern.to_string();
    if p == "~" {
        p = home.to_string();
    } else if let Some(rest) = p.strip_prefix("~/") {
        p = format!("{home}/{rest}");
    }
    if !p.starts_with('/') {
        p = format!("{base}/{p}");
    }
    let mut keep: Vec<&str> = vec![];
    for part in p.split('/') {
        if part.contains(['*', '?', '[', '{']) {
            break;
        }
        keep.push(part);
    }
    let j = keep.join("/");
    norm(if j.is_empty() { "/" } else { &j })
}

fn push_uniq(v: &mut Vec<String>, p: String) {
    if !v.contains(&p) {
        v.push(p);
    }
}

fn usable(v: Option<&String>, home: &str) -> Option<String> {
    let v = v?;
    if v.is_empty() {
        return None;
    }
    let v = if v == "~" {
        home.to_string()
    } else if let Some(rest) = v.strip_prefix("~/") {
        format!("{home}/{rest}")
    } else {
        v.clone()
    };
    if v.starts_with('/') {
        Some(v)
    } else {
        None
    }
}

/// fence.data_dirs (datahome.guarded_data_dirs).
pub fn data_dirs(env: &HashMap<String, String>, home: &str) -> Vec<String> {
    let mut out = vec![norm(&format!("{home}/.opendaisugi"))];
    if let Some(d) = usable(env.get("OPENDAISUGI_HOME"), home) {
        push_uniq(&mut out, norm(&d));
    }
    if let Some(x) = usable(env.get("XDG_DATA_HOME"), home) {
        push_uniq(&mut out, norm(&format!("{x}/opendaisugi")));
    }
    out
}

fn or_default(v: Option<&String>, d: String) -> String {
    match v {
        Some(s) if !s.is_empty() => s.clone(),
        _ => d,
    }
}

/// fence.coppice_dirs: tokens and keys live there.
pub fn coppice_dirs(env: &HashMap<String, String>, home: &str) -> Vec<String> {
    let cfg = or_default(env.get("XDG_CONFIG_HOME"), format!("{home}/.config"));
    let state = or_default(env.get("XDG_STATE_HOME"), format!("{home}/.local/state"));
    let data = or_default(env.get("XDG_DATA_HOME"), format!("{home}/.local/share"));
    let mut out = vec![];
    for p in [format!("{cfg}/coppice"), format!("{state}/coppice"), format!("{data}/coppice")] {
        push_uniq(&mut out, norm(&p));
    }
    if let Some(c) = env.get("COPPICE_DATA_DIR") {
        if c.starts_with('/') {
            push_uniq(&mut out, norm(c));
        }
    }
    out
}

fn dominates(o: &str, r: &str) -> bool {
    let have = mode_rights(o);
    mode_rights(r).chars().all(|c| have.contains(c))
}

/// fence.tmp_parents: the dirs whose `fence-tmp/<leaf>` is the fence's own
/// temp dir.
pub fn tmp_parents(env: &HashMap<String, String>, homes: &[String]) -> Vec<String> {
    let mut out: Vec<String> = vec![];
    for h in homes {
        for d in data_dirs(env, h) {
            push_uniq(&mut out, d);
        }
        for d in coppice_dirs(env, h) {
            push_uniq(&mut out, d);
        }
        let data = or_default(env.get("XDG_DATA_HOME"), format!("{h}/.local/share"));
        push_uniq(&mut out, norm(&format!("{data}/opendaisugi")));
    }
    out.into_iter().map(|p| format!("{p}/fence-tmp")).collect()
}

/// The home list fence.plan uses: home first, then each other absolute one.
pub fn homes(home: &str, others: &[String]) -> Vec<String> {
    let mut out = vec![norm(home)];
    for h in others {
        if h.starts_with('/') {
            push_uniq(&mut out, norm(h));
        }
    }
    out
}

/// The plan's protected paths, for the check at apply time.
pub struct Guard {
    secrets: Vec<String>,
    state: Vec<String>,
}

impl Guard {
    pub fn new(inp: &Input) -> Guard {
        let mut g = Guard { secrets: vec![], state: vec![] };
        for h in homes(&inp.home, &inp.other_homes) {
            for s in SECRETS {
                g.secrets.push(norm(&format!("{h}/{s}")));
            }
            g.secrets.extend(coppice_dirs(&inp.env, &h));
            g.state.extend(data_dirs(&inp.env, &h));
        }
        g.secrets.extend(inp.never_extra.iter().map(|x| norm(x)));
        if !inp.gate_root.is_empty() {
            g.state.push(norm(&inp.gate_root));
        }
        g.state.extend(inp.state_extra.iter().map(|x| norm(x)));
        g
    }

    /// The plan's test again on the path a rule's root resolved to.
    pub fn check(&self, real: &str, mode: &str, spelled: &str) -> Option<String> {
        if let Some(s) = self.secrets.iter().find(|s| covers(real, s) || covers(s, real)) {
            return Some(format!(
                "the rule {spelled} now resolves to {real}, which reaches the secret {s}. Nothing was run."
            ));
        }
        if mode == "rw" || mode == "rw+unix" {
            if let Some(s) = self.state.iter().find(|s| covers(real, s) || covers(s, real)) {
                return Some(format!(
                    "the rule {spelled} now resolves to {real}, inside the gate's state under {s}. Nothing was run."
                ));
            }
        }
        None
    }
}

/// fence.plan.
pub fn plan(inp: &Input) -> Output {
    let home = norm(&inp.home);
    let homes = homes(&home, &inp.other_homes);
    let workspace = norm(&inp.workspace);
    let env = &inp.env;
    let mut out = Output::default();

    let mut home_secrets: Vec<String> = vec![];
    for h in &homes {
        for s in SECRETS {
            push_uniq(&mut home_secrets, norm(&format!("{h}/{s}")));
        }
    }
    for x in &inp.never_extra {
        push_uniq(&mut home_secrets, norm(x));
    }
    let mut secrets = home_secrets.clone();
    let mut state_dirs: Vec<String> = vec![];
    for h in &homes {
        for s in coppice_dirs(env, h) {
            push_uniq(&mut secrets, s);
        }
        for d in data_dirs(env, h) {
            push_uniq(&mut state_dirs, d);
        }
    }
    let mut state = state_dirs.clone();
    if !inp.gate_root.is_empty() {
        push_uniq(&mut state, norm(&inp.gate_root));
    }
    for x in &inp.state_extra {
        push_uniq(&mut state, norm(x));
    }
    secrets.sort();
    home_secrets.sort();
    state.sort();

    let resolved = |p: &str| -> String { norm(inp.real.get(p).map(String::as_str).unwrap_or(p)) };

    let mut grants: HashMap<String, String> = HashMap::new();
    let mut put = |p: String, mode: &str| {
        let m = match grants.get(&p) {
            Some(old) => merge(old, mode),
            None => mode.to_string(),
        };
        grants.insert(p, m);
    };

    let opens_unnamed = |p: &str| -> Option<String> {
        secrets.iter().find(|s| covers(p, s) && p != s.as_str()).cloned()
    };
    let secret_hit = |p: &str| -> Option<String> {
        secrets.iter().find(|s| covers(p, s) || covers(s, p)).cloned()
    };
    let state_hit = |p: &str| -> Option<String> {
        state.iter().find(|s| covers(p, s) || covers(s, p)).cloned()
    };

    let mut refusals: Vec<String> = vec![];
    let mut notes: Vec<String> = vec![];

    let allow_specs: Vec<(String, &str)> = inp
        .allow
        .iter()
        .map(|spec| {
            if let Some(x) = spec.strip_suffix(":rw") {
                (expand(x, &home, &workspace), "rw")
            } else if let Some(x) = spec.strip_suffix(":ro") {
                (expand(x, &home, &workspace), "ro")
            } else {
                (expand(spec, &home, &workspace), "ro")
            }
        })
        .collect();
    let under_allow = |p: &str, mode: &str| -> bool {
        allow_specs.iter().any(|(a, m)| {
            covers(a, p)
                && dominates(m, mode)
                && opens_unnamed(a).is_none()
                && !(*m == "rw" && state_hit(a).is_some())
        })
    };
    let strict = |refusals: &mut Vec<String>, put: &mut dyn FnMut(String, &str), p: String, mode: &str, who: &str| {
        if under_allow(&p, mode) {
            put(p, mode);
            return;
        }
        if let Some(s) = secret_hit(&p) {
            refusals.push(format!(
                "{who} {p} would reach the secret {s}. Only --allow opens a secret. Name another directory, or run with --no-fence."
            ));
            return;
        }
        if mode != "ro" {
            if let Some(s) = state_hit(&p) {
                refusals.push(format!(
                    "{who} {p} would let the agent write the gate's state under {s}. Name another directory, or run with --no-fence."
                ));
                return;
            }
        }
        put(p, mode);
    };
    let allowed = |refusals: &mut Vec<String>, put: &mut dyn FnMut(String, &str), p: String, mode: &str| {
        if let Some(s) = opens_unnamed(&p) {
            refusals.push(format!(
                "--allow {p} would also open {s}. Name a narrower directory, or run with --no-fence."
            ));
            return;
        }
        if mode == "rw" {
            if let Some(s) = state_hit(&p) {
                refusals.push(format!(
                    "--allow {p} would let the agent write the gate's state under {s}. Name another directory, or run with --no-fence."
                ));
                return;
            }
        }
        put(p, mode);
    };
    let ambient = |put: &mut dyn FnMut(String, &str), p: String, mode: &str| {
        if secret_hit(&p).is_some() || (mode == "rw" && state_hit(&p).is_some()) {
            return;
        }
        put(p, mode);
    };

    for p in SYSTEM_RO {
        ambient(&mut put, p.to_string(), "ro");
    }
    for p in DEVICES {
        put(p.to_string(), "rw");
    }
    strict(&mut refusals, &mut put, workspace.clone(), "rw", "the workspace");
    for d in &inp.workdirs {
        strict(&mut refusals, &mut put, expand(d, &home, &workspace), "rw", "the work dir");
    }
    if !inp.tmpdir.is_empty() {
        let tmp = norm(&inp.tmpdir);
        let own = tmp_parents(env, &homes).contains(&dirname(&tmp));
        if own && opens_unnamed(&tmp).is_none() {
            put(tmp, "rw");
        } else {
            strict(&mut refusals, &mut put, tmp, "rw", "the temp dir");
        }
    }
    for (rel, mode) in harness_paths(&inp.harness) {
        strict(&mut refusals, &mut put, resolved(&norm(&format!("{home}/{rel}"))), mode, "the harness path");
    }
    if inp.harness == "claude" {
        let projects = resolved(&norm(&format!("{home}/.claude/projects")));
        match claude_project_name(&workspace) {
            Some(name) => strict(&mut refusals, &mut put, format!("{projects}/{name}"), "rw", "the harness path"),
            None => {
                strict(&mut refusals, &mut put, projects, "rw", "the harness path");
                notes.push(
                    "the fence cannot name Claude's project dir for this workspace; ~/.claude/projects stays \
                     writable, the memory of every project with it."
                        .to_string(),
                );
            }
        }
    }
    for d in &inp.path_dirs {
        if d.starts_with('/') {
            ambient(&mut put, norm(d), "ro");
        }
    }
    for d in &inp.exe_dirs {
        if d.starts_with('/') {
            ambient(&mut put, norm(d), "ro");
        }
    }
    for d in &state_dirs {
        put(format!("{d}/gate"), "sock");
        put(format!("{d}/config.yaml"), "ro");
    }
    if !inp.gate_root.is_empty() {
        put(norm(&inp.gate_root), "sock");
    }

    let mut wide: Vec<String> = SOCKET_WIDE.iter().map(|s| s.to_string()).collect();
    wide.extend(homes.iter().cloned());
    if let Some(rt) = env.get("XDG_RUNTIME_DIR").filter(|r| r.starts_with('/')) {
        wide.push(norm(rt));
    }
    for sock in &inp.sockets {
        if !sock.starts_with('/') {
            continue;
        }
        let d = dirname(&norm(sock));
        if wide.iter().any(|w| covers(&d, w)) {
            notes.push(format!("socket dir {d}: too wide for the fence; dropped."));
            continue;
        }
        if home_secrets.iter().any(|s| covers(&d, s) || covers(s, &d)) {
            notes.push(format!("socket dir {d}: would reach a secret directory; dropped."));
            continue;
        }
        put(d, "unix");
    }

    for (key, mode, pats) in
        [("file_read", "ro", &inp.envelope.file_read), ("file_write", "rw", &inp.envelope.file_write)]
    {
        for pat in pats {
            let pre = resolved(&glob_prefix(pat, &home, &workspace));
            if covers(&workspace, &pre) {
                continue;
            }
            if pre == "/" || homes.iter().any(|h| covers(&pre, h)) {
                notes.push(format!("envelope {key} {pat}: too wide for the fence; dropped."));
            } else if secret_hit(&pre).is_some() {
                notes.push(format!("envelope {key} {pat}: would reach a secret directory; dropped."));
            } else if mode == "rw" && state_hit(&pre).is_some() {
                notes.push(format!("envelope {key} {pat}: would reach the gate's state; dropped."));
            } else {
                put(pre, mode);
            }
        }
    }

    for (p, mode) in &allow_specs {
        allowed(&mut refusals, &mut put, p.clone(), mode);
    }
    drop(put);

    let mut paths: Vec<&String> = grants.keys().collect();
    paths.sort();
    let rules: Vec<Rule> =
        paths.iter().map(|p| Rule { path: (*p).clone(), mode: grants[*p].clone() }).collect();
    for r in &rules {
        let redundant = rules
            .iter()
            .any(|o| o.path != r.path && covers(&o.path, &r.path) && dominates(&o.mode, &r.mode));
        if !redundant {
            out.rules.push(r.clone());
        }
    }
    out.notes = notes;
    out.refusals = refusals;
    out
}

// ---------------------------------------------------------------- landlock

const SYS_CREATE_RULESET: libc::c_long = 444;
const SYS_ADD_RULE: libc::c_long = 445;
const SYS_RESTRICT_SELF: libc::c_long = 446;
const CREATE_RULESET_VERSION: libc::c_ulong = 1;
const RULE_PATH_BENEATH: libc::c_ulong = 1;
const O_PATH: libc::c_int = 0o10000000;

const ACC_EXECUTE: u64 = 1 << 0;
const ACC_WRITE_FILE: u64 = 1 << 1;
const ACC_READ_FILE: u64 = 1 << 2;
const ACC_READ_DIR: u64 = 1 << 3;
const ACC_TRUNCATE: u64 = 1 << 14;
const ACC_IOCTL_DEV: u64 = 1 << 15;
const ACC_RESOLVE_UNIX: u64 = 1 << 16;
const SCOPE_ABSTRACT_UNIX: u64 = 1 << 0;
const SCOPE_SIGNAL: u64 = 1 << 1;
const SYS_CLOSE_RANGE: libc::c_long = 436;
const CLOSE_RANGE_CLOEXEC: libc::c_uint = 1 << 2;

/// The oldest Landlock ABI that keeps every file deny: ABI 3 adds truncate.
pub const MIN_ABI: i32 = 3;
/// The ABI that stops unix-socket connects outside the granted dirs.
pub const IPC_ABI: i32 = 9;
/// The ABI that keeps abstract sockets and signals inside the fence.
pub const SCOPE_ABI: i32 = 6;

/// Why the kernel cannot fence.
#[derive(Debug, Clone)]
pub struct NoLandlock(pub String);

fn ask_kernel() -> Result<i32, NoLandlock> {
    let r = unsafe {
        libc::syscall(SYS_CREATE_RULESET, std::ptr::null::<u8>(), 0 as libc::c_ulong, CREATE_RULESET_VERSION)
    };
    if r >= 0 {
        return Ok(r as i32);
    }
    let e = std::io::Error::last_os_error();
    Err(NoLandlock(match e.raw_os_error() {
        Some(libc::ENOSYS) => "this kernel was built without Landlock".to_string(),
        Some(libc::EOPNOTSUPP) => {
            "Landlock is built into this kernel but switched off at boot (add landlock to lsm=)".to_string()
        }
        _ => format!("the kernel refused to report its Landlock version: {e}"),
    }))
}

/// The kernel's ABI. DAISUGI_FENCE_PROBE can only make the answer worse
/// (enosys, eopnotsupp, abi:N below the real one), so a test can drive the
/// refusal paths and nothing can use it to fake a fence.
pub fn kernel_abi() -> Result<i32, NoLandlock> {
    let abi = ask_kernel()?;
    match std::env::var("DAISUGI_FENCE_PROBE").unwrap_or_default().as_str() {
        "enosys" => Err(NoLandlock("this kernel was built without Landlock".to_string())),
        "eopnotsupp" => Err(NoLandlock(
            "Landlock is built into this kernel but switched off at boot (add landlock to lsm=)".to_string(),
        )),
        v => {
            if let Some(n) = v.strip_prefix("abi:").and_then(|n| n.parse::<i32>().ok()) {
                if n < abi {
                    return Ok(n);
                }
            }
            Ok(abi)
        }
    }
}

/// The probe and the floor: the ABI to fence with, or why not. `weak_ipc`
/// lets a kernel before IPC_ABI fence the files only (FN-R-10).
pub fn check_abi(probe: &dyn Fn() -> Result<i32, NoLandlock>, weak_ipc: bool) -> Result<i32, NoLandlock> {
    let abi = probe()?;
    if abi < MIN_ABI {
        return Err(NoLandlock(format!(
            "this kernel has Landlock ABI {abi} and the fence needs ABI {MIN_ABI} or newer (it must stop truncate, which older kernels cannot)"
        )));
    }
    if abi < IPC_ABI && !weak_ipc {
        return Err(NoLandlock(format!(
            "this kernel has Landlock ABI {abi} and the fence needs ABI {IPC_ABI} to stop connects to unix sockets (the session bus, ssh-agent) and ABI {SCOPE_ABI} to stop signals; --allow-weak-ipc fences the files only"
        )));
    }
    Ok(abi)
}

/// Every file-system right the kernel at this ABI knows, the socket
/// connect right among them from ABI 9, and no network right (FN-5).
fn handled(abi: i32) -> u64 {
    let mut h: u64 = (1 << 15) - 1; // bits 0..=14: execute .. truncate
    if abi >= 5 {
        h |= ACC_IOCTL_DEV;
    }
    if abi >= IPC_ABI {
        h |= ACC_RESOLVE_UNIX;
    }
    h
}

/// Abstract unix sockets and signals stay inside the fence from ABI 6.
fn scoped(abi: i32) -> u64 {
    if abi >= SCOPE_ABI {
        SCOPE_ABSTRACT_UNIX | SCOPE_SIGNAL
    } else {
        0
    }
}

const FILE_RIGHTS: u64 = ACC_EXECUTE | ACC_WRITE_FILE | ACC_READ_FILE | ACC_TRUNCATE | ACC_IOCTL_DEV;
const RO_RIGHTS: u64 = ACC_EXECUTE | ACC_READ_FILE | ACC_READ_DIR;

fn rights(mode: &str, is_dir: bool, h: u64) -> u64 {
    let mut r = match mode {
        "rw" => h & !ACC_RESOLVE_UNIX,
        "rw+unix" => h,
        "unix" => ACC_RESOLVE_UNIX,
        "sock" => RO_RIGHTS | ACC_RESOLVE_UNIX,
        _ => RO_RIGHTS,
    };
    if !is_dir {
        r &= FILE_RIGHTS;
    }
    r & h
}

/// Mark every descriptor from 3 up close-on-exec, then clear the mark on
/// each kept one: Landlock checks only at open time, so a file or socket
/// the caller left open must not cross the exec (FN-R-18).
pub fn close_inherited(keep: &[i32]) -> Result<(), String> {
    let rc = unsafe { libc::syscall(SYS_CLOSE_RANGE, 3 as libc::c_uint, libc::c_uint::MAX, CLOSE_RANGE_CLOEXEC) };
    if rc != 0 {
        let e = std::io::Error::last_os_error();
        if !matches!(e.raw_os_error(), Some(libc::ENOSYS) | Some(libc::EINVAL)) {
            return Err(format!("close_range: {e}"));
        }
        if let Ok(rd) = std::fs::read_dir("/proc/self/fd") {
            for ent in rd.flatten() {
                if let Ok(n) = ent.file_name().to_string_lossy().parse::<i32>() {
                    if n >= 3 {
                        unsafe { libc::fcntl(n, libc::F_SETFD, libc::FD_CLOEXEC) };
                    }
                }
            }
        }
    }
    for fd in keep {
        if unsafe { libc::fcntl(*fd, libc::F_SETFD, 0) } != 0 {
            return Err(format!("--keep-fd {fd}: {}", std::io::Error::last_os_error()));
        }
    }
    Ok(())
}

/// What `apply` did.
pub struct Applied {
    pub abi: i32,
    pub granted: usize,
    pub skipped: Vec<String>,
}

/// Set no_new_privs, build the ruleset from `rules`, and restrict the
/// calling thread and every process it starts.
/// `guard`, when set, checks the real path each rule opened (FN-R-16).
pub fn apply(
    rules: &[Rule],
    probe: &dyn Fn() -> Result<i32, NoLandlock>,
    weak_ipc: bool,
    guard: Option<&Guard>,
) -> Result<Applied, String> {
    let abi = check_abi(probe, weak_ipc).map_err(|e| e.0)?;
    let h = handled(abi);
    if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
        return Err(format!("no_new_privs: {}", std::io::Error::last_os_error()));
    }
    // struct landlock_ruleset_attr: handled_access_fs, handled_access_net
    // (0: FN-5) and scoped. A kernel before ABI 6 knows only the first.
    let mut attr = [0u8; 24];
    attr[..8].copy_from_slice(&h.to_le_bytes());
    attr[16..].copy_from_slice(&scoped(abi).to_le_bytes());
    let size: usize = if abi < SCOPE_ABI { 8 } else { 24 };
    let fd = unsafe { libc::syscall(SYS_CREATE_RULESET, attr.as_ptr(), size, 0 as libc::c_ulong) };
    if fd < 0 {
        return Err(format!("landlock_create_ruleset: {}", std::io::Error::last_os_error()));
    }
    let rs = fd as libc::c_int;
    let mut out = Applied { abi, granted: 0, skipped: vec![] };
    for r in rules {
        let c = match std::ffi::CString::new(r.path.as_bytes()) {
            Ok(c) => c,
            Err(_) => {
                out.skipped.push(r.path.clone());
                continue;
            }
        };
        let pf = unsafe { libc::open(c.as_ptr(), O_PATH | libc::O_CLOEXEC) };
        if pf < 0 {
            out.skipped.push(r.path.clone());
            continue;
        }
        let mut st: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstat(pf, &mut st) } != 0 {
            unsafe { libc::close(pf) };
            out.skipped.push(r.path.clone());
            continue;
        }
        if let Some(g) = guard {
            let real = std::fs::read_link(format!("/proc/self/fd/{pf}"))
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_default();
            let why = if real.is_empty() {
                Some(format!("cannot read where {} resolves. Nothing was run.", r.path))
            } else if real != r.path {
                g.check(&real, &r.mode, &r.path)
            } else {
                None
            };
            if let Some(why) = why {
                unsafe {
                    libc::close(pf);
                    libc::close(rs);
                }
                return Err(format!("refused: {why}"));
            }
        }
        let is_dir = st.st_mode & libc::S_IFMT == libc::S_IFDIR;
        let mask = rights(&r.mode, is_dir, h);
        if mask == 0 {
            // The connect right on a kernel before ABI 9 (weak IPC allowed).
            unsafe { libc::close(pf) };
            out.skipped.push(r.path.clone());
            continue;
        }
        let mut buf = [0u8; 12]; // struct landlock_path_beneath_attr is packed
        buf[..8].copy_from_slice(&mask.to_le_bytes());
        buf[8..].copy_from_slice(&(pf as u32).to_le_bytes());
        let rc = unsafe { libc::syscall(SYS_ADD_RULE, rs, RULE_PATH_BENEATH, buf.as_ptr(), 0 as libc::c_ulong) };
        let err = std::io::Error::last_os_error();
        unsafe { libc::close(pf) };
        if rc != 0 {
            unsafe { libc::close(rs) };
            return Err(format!("landlock_add_rule {}: {err}", r.path));
        }
        out.granted += 1;
    }
    let rc = unsafe { libc::syscall(SYS_RESTRICT_SELF, rs, 0 as libc::c_ulong) };
    let err = std::io::Error::last_os_error();
    unsafe { libc::close(rs) };
    if rc != 0 {
        return Err(format!("landlock_restrict_self: {err}"));
    }
    Ok(out)
}

// ------------------------------------------------------------------ gather

/// What the command line and the environment say.
#[derive(Default, Clone)]
pub struct Options {
    pub harness: String,
    pub workspace: String,
    pub allow: Vec<String>,
    pub workdirs: Vec<String>,
    pub envelope: String,
    pub gate_root: String,
    pub tmpdir: String,
    pub sockets: Vec<String>,
    pub cmd: Vec<String>,
    pub environ: HashMap<String, String>,
    pub cwd: String,
    pub self_exe: String,
    pub pw_home: String,
    pub pid: u32,
}

fn real(p: &str) -> String {
    std::fs::canonicalize(p).map(|r| r.to_string_lossy().into_owned()).unwrap_or_else(|_| p.to_string())
}

fn real_ok(p: &str) -> Option<String> {
    std::fs::canonicalize(p).ok().map(|r| r.to_string_lossy().into_owned())
}

fn is_exec_file(p: &str) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).map(|m| !m.is_dir() && m.permissions().mode() & 0o111 != 0).unwrap_or(false)
}

fn look_path(name: &str, env: &HashMap<String, String>, cwd: &str) -> String {
    if name.contains('/') {
        return if name.starts_with('/') { name.to_string() } else { norm(&format!("{cwd}/{name}")) };
    }
    for d in env.get("PATH").map(String::as_str).unwrap_or("").split(':') {
        if !d.starts_with('/') {
            continue;
        }
        let p = format!("{d}/{name}");
        if is_exec_file(&p) {
            return p;
        }
    }
    String::new()
}

/// The table key for a command: the base name of its path.
pub fn harness_name(cmd0: &str) -> String {
    let b = cmd0.rsplit('/').next().unwrap_or(cmd0);
    if harness_paths(b).is_empty() {
        String::new()
    } else {
        b.to_string()
    }
}

/// The password entry's home directory, or "".
pub fn pw_home() -> String {
    let mut buf = vec![0 as libc::c_char; 16384];
    let mut pw: libc::passwd = unsafe { std::mem::zeroed() };
    let mut out: *mut libc::passwd = std::ptr::null_mut();
    let rc = unsafe { libc::getpwuid_r(libc::getuid(), &mut pw, buf.as_mut_ptr(), buf.len(), &mut out) };
    if rc != 0 || out.is_null() || pw.pw_dir.is_null() {
        return String::new();
    }
    unsafe { std::ffi::CStr::from_ptr(pw.pw_dir) }.to_string_lossy().into_owned()
}

/// Read the environment and the disk and build the pure input. Every
/// symlink is resolved first, and the other spellings of each protected
/// path go along. HOME is the one the harness sees; the password entry's
/// home, when it differs, keeps its secrets shut too (FN-R-15). Returns
/// the input and the program to run ("" if none).
pub fn gather(o: &Options) -> Result<(Input, String), String> {
    let env = &o.environ;
    let mut home = env.get("HOME").cloned().unwrap_or_default();
    if !home.starts_with('/') {
        home = o.pw_home.clone();
    }
    if !home.starts_with('/') {
        return Err("HOME is unset or not an absolute path".to_string());
    }
    let home = real(&norm(&home));
    let mut ws = if o.workspace.is_empty() { o.cwd.clone() } else { o.workspace.clone() };
    if !ws.starts_with('/') {
        ws = norm(&format!("{}/{ws}", o.cwd));
    }
    let ws = real(&norm(&ws));

    let mut inp = Input { home: home.clone(), workspace: ws.clone(), harness: o.harness.clone(), ..Default::default() };
    if o.pw_home.starts_with('/') {
        let pw = real(&norm(&o.pw_home));
        if pw != home {
            inp.other_homes.push(pw);
        }
    }
    for k in ["OPENDAISUGI_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "COPPICE_DATA_DIR"] {
        if let Some(v) = env.get(k) {
            inp.env.insert(k.to_string(), v.clone());
        }
    }
    let exe = o.cmd.first().map(|c| look_path(c, env, &o.cwd)).unwrap_or_default();
    if inp.harness.is_empty() {
        if let Some(c) = o.cmd.first() {
            inp.harness = harness_name(c);
        }
    }
    for d in env.get("PATH").map(String::as_str).unwrap_or("").split(':') {
        if d.starts_with('/') {
            if let Some(r) = real_ok(d) {
                inp.path_dirs.push(r);
            }
        }
    }
    for e in [exe.as_str(), o.self_exe.as_str()] {
        if !e.is_empty() {
            if let Some(r) = real_ok(e) {
                inp.exe_dirs.push(dirname(&r));
            }
        }
    }
    let mut tmp_opt_out = false;
    for spec in &o.allow {
        let (mut p, mut suffix) = (spec.as_str(), "");
        for s in [":rw", ":ro"] {
            if let Some(x) = spec.strip_suffix(s) {
                p = x;
                suffix = s;
            }
        }
        let rp = real(&expand(p, &home, &ws));
        if suffix == ":rw" && rp == real("/tmp") {
            tmp_opt_out = true;
        }
        inp.allow.push(format!("{rp}{suffix}"));
    }
    for d in &o.workdirs {
        inp.workdirs.push(real(&expand(d, &home, &ws)));
    }
    for sck in &o.sockets {
        if sck.starts_with('/') {
            let n = norm(sck);
            let base = n.rsplit('/').next().unwrap_or("").to_string();
            inp.sockets.push(format!("{}/{base}", real(&dirname(&n))).replace("//", "/"));
        }
    }
    if !o.gate_root.is_empty() {
        inp.gate_root = real(&expand(&o.gate_root, &home, &ws));
    }
    if !o.tmpdir.is_empty() {
        inp.tmpdir = real(&expand(&o.tmpdir, &home, &ws));
    } else if !tmp_opt_out {
        inp.tmpdir = own_tmpdir(env, &home, o.pid);
    }

    let mut secrets: Vec<String> = vec![];
    let mut state: Vec<String> = vec![];
    for h in homes(&home, &inp.other_homes) {
        secrets.extend(SECRETS.iter().map(|s| norm(&format!("{h}/{s}"))));
        secrets.extend(coppice_dirs(&inp.env, &h));
        state.extend(data_dirs(&inp.env, &h));
    }
    for p in &secrets {
        let r = real(p);
        if &r != p {
            inp.never_extra.push(r);
        }
    }
    for p in &state {
        let r = real(p);
        if &r != p {
            inp.state_extra.push(r);
        }
    }
    // The harness table's roots, as they resolve (FN-R-16).
    for (rel, _) in harness_paths(&inp.harness) {
        let p = norm(&format!("{home}/{rel}"));
        let r = real(&p);
        if r != p {
            inp.real.insert(p, r);
        }
    }

    let mut env_path = o.envelope.clone();
    if env_path.is_empty() {
        let candidates: Vec<String> = if inp.gate_root.is_empty() {
            data_dirs(&inp.env, &home).iter().map(|d| format!("{d}/gate/envelopes/default.json")).collect()
        } else {
            vec![format!("{}/envelopes/default.json", inp.gate_root)]
        };
        for c in candidates {
            if std::fs::metadata(&c).is_ok() {
                env_path = c;
                break;
            }
        }
    }
    if !env_path.is_empty() {
        if let Ok(raw) = std::fs::read_to_string(&env_path) {
            if let Ok(doc) = serde_json::from_str::<serde_json::Value>(&raw) {
                let list = |k: &str| -> Option<Vec<String>> {
                    doc.get("permissions")?
                        .get(k)?
                        .as_array()
                        .map(|a| a.iter().filter_map(|x| x.as_str().map(String::from)).collect())
                };
                if let Some(v) = list("file_read") {
                    inp.envelope.file_read = v;
                }
                if let Some(v) = list("file_write") {
                    inp.envelope.file_write = v;
                }
            }
        }
    }
    let pats: Vec<String> = inp.envelope.file_read.iter().chain(inp.envelope.file_write.iter()).cloned().collect();
    for pat in pats {
        let p = glob_prefix(&pat, &home, &ws);
        let r = real(&p);
        if r != p {
            inp.real.insert(p, r);
        }
    }
    Ok((inp, exe))
}

/// A standalone fence's private temp dir:
/// `$XDG_DATA_HOME/opendaisugi/fence-tmp/<pid>-<random>`, on disk, not /tmp
/// (FN-R-14). Each run gets a fresh leaf, so a reused pid never finds an
/// earlier run's files (FN-R-23); the pid lets the next run sweep the
/// leaves of dead ones.
pub fn own_tmpdir(env: &HashMap<String, String>, home: &str, pid: u32) -> String {
    let data = match env.get("XDG_DATA_HOME") {
        Some(d) if d.starts_with('/') => d.clone(),
        _ => format!("{home}/.local/share"),
    };
    format!("{}/opendaisugi/fence-tmp/{pid}-{}", real(&norm(&data)), nonce())
}

/// 16 random hex digits: a temp leaf name no earlier run used.
pub fn nonce() -> String {
    let mut b = [0u8; 8];
    let ok = std::fs::File::open("/dev/urandom").and_then(|mut f| std::io::Read::read_exact(&mut f, &mut b)).is_ok();
    if !ok {
        // No randomness: the time still makes the name new, and the
        // create refuses a leaf that is there.
        let t = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0);
        return format!("{t:x}");
    }
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// Make the private temp dir 0700, fresh: a path that is already there is
/// refused, so no run reuses another run's files (FN-R-23). `sweep` also
/// removes the leaves of dead runs: a standalone fence execs, so nothing
/// is left to remove its own dir at exit.
pub fn make_tmpdir(dir: &str, sweep: bool) -> Result<(), String> {
    use std::os::unix::fs::{DirBuilderExt, MetadataExt, PermissionsExt};
    let parent = dirname(dir);
    std::fs::DirBuilder::new().recursive(true).mode(0o700).create(&parent).map_err(|e| e.to_string())?;
    if let Err(e) = std::fs::DirBuilder::new().mode(0o700).create(dir) {
        if e.kind() == std::io::ErrorKind::AlreadyExists {
            return Err(format!("{dir} is already there; each run needs a fresh one"));
        }
        return Err(e.to_string());
    }
    let m = std::fs::symlink_metadata(dir).map_err(|e| e.to_string())?;
    if !m.is_dir() || m.uid() != unsafe { libc::getuid() } {
        return Err(format!("{dir} is not a directory of this user"));
    }
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)).map_err(|e| e.to_string())?;
    if sweep {
        if let Ok(rd) = std::fs::read_dir(&parent) {
            for ent in rd.flatten() {
                let name = ent.file_name().to_string_lossy().into_owned();
                let Ok(n) = name.split('-').next().unwrap_or("").parse::<u32>() else { continue };
                let path = format!("{parent}/{name}");
                if path != dir && std::fs::metadata(format!("/proc/{n}")).is_err() {
                    let _ = std::fs::remove_dir_all(&path);
                }
            }
        }
    }
    Ok(())
}

/// Make each read-write directory of the harness table under home that is
/// missing while its parent is there: the parent is read only in the
/// fence. Files (.json, .jsonl) are left alone. For claude it makes
/// ~/.claude/projects and this workspace's project dir in it the same way
/// (FN-R-31).
pub fn ensure_harness_dirs(harness: &str, home: &str, workspace: &str) {
    use std::os::unix::fs::DirBuilderExt;
    if harness == "claude" {
        if let Some(name) = claude_project_name(workspace) {
            let projects = format!("{home}/.claude/projects");
            for p in [projects.clone(), format!("{projects}/{name}")] {
                if std::fs::symlink_metadata(&p).is_ok() {
                    continue;
                }
                if std::fs::metadata(dirname(&p)).map(|m| m.is_dir()).unwrap_or(false) {
                    let _ = std::fs::DirBuilder::new().mode(0o700).create(&p);
                }
            }
        }
    }
    for (rel, mode) in harness_paths(harness) {
        if *mode != "rw" || rel.ends_with(".json") || rel.ends_with(".jsonl") {
            continue;
        }
        let p = format!("{home}/{rel}");
        if std::fs::symlink_metadata(&p).is_ok() {
            continue;
        }
        if std::fs::metadata(dirname(&p)).map(|m| m.is_dir()).unwrap_or(false) {
            let _ = std::fs::DirBuilder::new().mode(0o700).create(&p);
        }
    }
}

/// The gate roots the plan connects to: --gate-root, else each data dir's.
pub fn gate_roots(inp: &Input) -> Vec<String> {
    if !inp.gate_root.is_empty() {
        return vec![inp.gate_root.clone()];
    }
    data_dirs(&inp.env, &inp.home).into_iter().map(|d| format!("{d}/gate")).collect()
}

/// A gate root is in use unless its envelopes dir is missing (FN-R-17).
pub fn gate_on(root: &str) -> bool {
    match std::fs::metadata(format!("{root}/envelopes")) {
        Ok(_) => true,
        Err(e) => e.kind() != std::io::ErrorKind::NotFound,
    }
}

/// The variables that point the harness at a daemon outside the fence
/// (FN-R-10), and any DAISUGI_FENCED the caller set.
const STRIPPED: &[&str] =
    &["SSH_AUTH_SOCK", "GPG_AGENT_INFO", "DBUS_SESSION_BUS_ADDRESS", "DISPLAY", "WAYLAND_DISPLAY", "DAISUGI_FENCED"];

/// Whether a resident gate answers on sock.
fn gate_live(sock: &str) -> bool {
    use std::io::Read;
    match std::os::unix::net::UnixStream::connect(sock) {
        Ok(mut c) => {
            let _ = c.shutdown(std::net::Shutdown::Write);
            let _ = c.set_read_timeout(Some(std::time::Duration::from_secs(1)));
            let mut sink = Vec::new();
            let _ = c.read_to_end(&mut sink);
            true
        }
        Err(e) => matches!(e.raw_os_error(), Some(libc::EAGAIN)),
    }
}

/// The captures root a resident gate on root gets, whoever starts it
/// (`daisugi fence` or `daisugi start`): <root>/captures, sprig's own
/// default (FN-R-32, FN-R-35).
pub fn gate_captures_args(root: &str) -> [String; 2] {
    ["--captures-root".to_string(), format!("{}/captures", root.trim_end_matches('/'))]
}

/// Start `<self> gate serve --root root --captures-root root/captures` in
/// a session of its own, with no stdin, stdout or stderr. The captures
/// root is fixed here, sprig's own default, so a fenced run's captures
/// land (FN-R-32). The pid, or 0.
fn spawn_gate_server(self_exe: &str, root: &str) -> u32 {
    use std::os::unix::process::CommandExt;
    let mut c = std::process::Command::new(self_exe);
    c.args(["gate", "serve", "--root", root]).args(gate_captures_args(root))
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    // SAFETY: setsid() is async-signal-safe and touches no memory.
    unsafe {
        c.pre_exec(|| {
            libc::setsid();
            Ok(())
        });
    }
    c.spawn().map(|ch| ch.id()).unwrap_or(0)
}

/// Make sure the resident gate answers on each gate root in use, starting
/// it outside the fence (before the ruleset) when it does not: inside the
/// fence the gate hook only asks it (FN-R-12). A middle process starts
/// the server and ends, so the server's parent is not the harness.
fn fence_gate_server(inp: &Input, self_exe: &str) {
    for root in gate_roots(inp) {
        if !gate_on(&root) {
            continue;
        }
        // This process becomes the harness (exec keeps the pid), and it is
        // still outside the fence: the record binds the workspace for the
        // resident gate's checkpoints (FN-R-21).
        if let Err(e) = crate::gate::fenceruns::register(&root, std::process::id() as i64, &inp.workspace) {
            eprint_all(&format!(
                "daisugi fence: cannot record this run in {root} ({e}); the resident gate will deny its checkpoints\n"
            ));
        }
        let sock = format!("{root}/gate.sock");
        if gate_live(&sock) {
            continue;
        }
        if std::fs::symlink_metadata(&sock).is_ok() {
            let _ = std::fs::remove_file(&sock);
        }
        let pid = std::process::Command::new(self_exe)
            .args(["fence", "--spawn-gate", &root])
            .stderr(std::process::Stdio::null())
            .output()
            .ok()
            .and_then(|o| String::from_utf8_lossy(&o.stdout).trim().parse::<u32>().ok())
            .unwrap_or(0);
        let t0 = std::time::Instant::now();
        while !gate_live(&sock) && t0.elapsed() < std::time::Duration::from_secs(2) {
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        if gate_live(&sock) {
            eprint_all(&format!("daisugi fence: started the resident gate on {sock} (pid {pid})\n"));
        } else {
            eprint_all(&format!(
                "daisugi fence: the resident gate did not start on {sock}; the gate hook will deny every call. Run `daisugi gate serve --root {root}` to see why.\n"
            ));
        }
    }
}

// --------------------------------------------------------------------- cli

const HELP: &str = "Usage: daisugi fence [OPTIONS] [--] COMMAND [ARGS]...

  Run any agent harness under a Landlock fence. The kernel then holds the
  line the gate only reads: the harness and every process it starts can
  read and write the workspace, the harness's own directory and the temp
  dir, read the system, and reach nothing else under HOME. ~/.ssh, ~/.aws,
  ~/.gnupg and the other secret directories stay shut, and the gate's own
  state is never writable.

  The temp dir is a private one on disk, made for this run: /tmp is not
  reachable. Unix sockets connect only in the gate root and the coppice
  socket's directory; abstract sockets and signals stay inside the fence.
  The gate hook inside the fence asks the resident gate, which the fence
  starts outside it when the gate is on.

  The fence adds one exec and a few system calls at start and nothing per
  call. It refuses to start the harness when the kernel has no usable
  Landlock (Linux 6.2 or newer with landlock in lsm=), or a Landlock older
  than ABI 9 unless --allow-weak-ipc is given.

  The network is not limited in this version: Landlock limits ports, not
  hosts. A harness under the fence can still reach any host. Nor are
  chmod, chown and other metadata calls.

Options:
  --harness NAME           Whose own paths to allow: claude, codex, pi, opencode,
                           sprig. Default: the command's name.
  --workspace DIR          The directory to read and write. Default: the current one.
  --allow PATH[:rw]        Also allow PATH, read only unless it ends in :rw. Repeatable.
                           Only --allow opens a secret directory. --allow /tmp:rw
                           keeps the shared /tmp as the temp dir.
  --workdir DIR            Also read and write DIR, checked like the workspace.
                           Repeatable.
  --envelope FILE          Read file_read and file_write from this envelope. Default:
                           the gate's default envelope.
  --gate-root DIR          The gate root, when it is not under the data dir.
  --tmpdir DIR             Use DIR (made 0700) as the private temp dir. The caller
                           removes it. Default: a new one under the data dir.
  --coppice-socket PATH    Let the harness connect to the coppice socket at PATH.
  --keep-fd N              Keep descriptor N open in the harness. Repeatable.
  --allow-weak-ipc         Fence the files only on a kernel before Landlock ABI 9.
  --print                  Show the rules this would apply, and run nothing.
  --json                   With --print, write JSON.
  --no-fence               Run COMMAND with no fence.
  --help                   Show this message and exit.
";

#[derive(Serialize)]
struct Printed<'a> {
    harness: &'a str,
    workspace: &'a str,
    home: &'a str,
    tmpdir: &'a str,
    landlock_abi: Option<i32>,
    landlock: String,
    rules: &'a [Rule],
    notes: &'a [String],
    refusals: &'a [String],
}

fn say(s: &str) {
    let _ = std::io::stdout().write_all(s.as_bytes());
    let _ = std::io::stdout().flush();
}

fn eprint_all(s: &str) {
    let _ = std::io::stderr().write_all(s.as_bytes());
}

fn usage_error(msg: &str) -> ! {
    eprint_all(&format!("Usage: daisugi fence [OPTIONS]\nTry 'daisugi fence --help' for help.\n\nError: {msg}\n"));
    std::process::exit(2);
}

fn exec_program(path: &str, argv: &[String], environ: &HashMap<String, String>, extra: &[(&str, &str)]) -> ! {
    use std::os::unix::process::CommandExt;
    let mut c = std::process::Command::new(path);
    c.arg0(&argv[0]).args(&argv[1..]).env_clear().envs(environ.iter());
    for (k, v) in extra {
        c.env(k, v);
    }
    let err = c.exec();
    eprint_all(&format!("daisugi fence: cannot start {}: {err}", argv[0]));
    if err.raw_os_error() == Some(libc::EACCES) {
        eprint_all(" (the fence may hide the program or the file it points to; add its directory with --allow)");
    }
    eprint_all("\n");
    std::process::exit(126);
}

fn exec_unfenced(cmd: &[String], environ: &HashMap<String, String>, cwd: &str) -> ! {
    let exe = look_path(&cmd[0], environ, cwd);
    if exe.is_empty() {
        eprint_all(&format!("daisugi fence: {} is not on PATH.\n", cmd[0]));
        std::process::exit(127);
    }
    exec_program(&exe, cmd, environ, &[])
}

fn print_plan(inp: &Input, plan: &Output, abi: &Result<i32, NoLandlock>, as_json: bool) {
    let (n, why) = match abi {
        Ok(n) => (Some(*n), "ok".to_string()),
        Err(e) => (None, e.0.clone()),
    };
    if as_json {
        let p = Printed {
            harness: &inp.harness,
            workspace: &inp.workspace,
            home: &inp.home,
            tmpdir: &inp.tmpdir,
            landlock_abi: n,
            landlock: why,
            rules: &plan.rules,
            notes: &plan.notes,
            refusals: &plan.refusals,
        };
        say(&format!("{}\n", serde_json::to_string_pretty(&p).unwrap_or_default()));
        return;
    }
    let h = if inp.harness.is_empty() { "(none)" } else { &inp.harness };
    say(&format!("harness {h}, workspace {}\n", inp.workspace));
    match n {
        Some(n) => say(&format!("landlock: ABI {n}\n")),
        None => say(&format!("landlock: unusable ({why}). The harness would not start.\n")),
    }
    for r in &plan.rules {
        say(&format!("  {:<7} {}\n", r.mode, r.path));
    }
    for x in &plan.notes {
        say(&format!("note: {x}\n"));
    }
    for x in &plan.refusals {
        say(&format!("refused: {x}\n"));
    }
    say("network: not limited (Landlock limits ports, not hosts)\n");
}

/// `daisugi fence`. `args` are the words after "fence". Never returns.
pub fn main(args: Vec<String>) -> ! {
    let mut o = Options::default();
    let (mut print, mut as_json, mut no_fence, mut weak_ipc) = (false, false, false, false);
    let mut keep: Vec<i32> = vec![];
    let mut plan_from = String::new();
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        let (name, inline) = match a.split_once('=') {
            Some((k, v)) if k.starts_with("--") => (k.to_string(), Some(v.to_string())),
            _ => (a.clone(), None),
        };
        match name.as_str() {
            "--" => {
                i += 1;
                break;
            }
            "--help" => {
                say(HELP);
                std::process::exit(0);
            }
            "--print" => print = true,
            "--json" => as_json = true,
            "--no-fence" => no_fence = true,
            "--allow-weak-ipc" => weak_ipc = true,
            "--harness" | "--workspace" | "--allow" | "--envelope" | "--gate-root" | "--plan-from" | "--workdir"
            | "--tmpdir" | "--coppice-socket" | "--keep-fd" | "--spawn-gate" => {
                let v = match inline {
                    Some(v) => v,
                    None => {
                        i += 1;
                        match args.get(i) {
                            Some(v) => v.clone(),
                            None => usage_error(&format!("Option '{a}' requires an argument.")),
                        }
                    }
                };
                match name.as_str() {
                    "--harness" => o.harness = v,
                    "--workspace" => o.workspace = v,
                    "--allow" => o.allow.push(v),
                    "--workdir" => o.workdirs.push(v),
                    "--envelope" => o.envelope = v,
                    "--gate-root" => o.gate_root = v,
                    "--tmpdir" => o.tmpdir = v,
                    "--coppice-socket" => o.sockets.push(v),
                    "--keep-fd" => match v.parse::<i32>() {
                        Ok(n) if n >= 3 => keep.push(n),
                        _ => usage_error(&format!("--keep-fd takes a descriptor number of 3 or more, not {v}.")),
                    },
                    "--spawn-gate" => {
                        // Hidden: start `gate serve` in a session of its own,
                        // print its pid and end, so the server's parent is not
                        // the harness.
                        let me = std::env::current_exe().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
                        say(&format!("{}\n", spawn_gate_server(&me, &v)));
                        std::process::exit(0);
                    }
                    _ => plan_from = v,
                }
            }
            _ => {
                if a.starts_with('-') && a != "-" {
                    usage_error(&format!("No such option: {a}"));
                }
                break;
            }
        }
        i += 1;
    }
    o.cmd = args[i.min(args.len())..].to_vec();

    if !plan_from.is_empty() {
        let mut text = String::new();
        let read = if plan_from == "-" {
            std::io::Read::read_to_string(&mut std::io::stdin(), &mut text).map(|_| ())
        } else {
            std::fs::read_to_string(&plan_from).map(|t| text = t)
        };
        if let Err(e) = read {
            eprint_all(&format!("daisugi fence: {e}. Nothing was changed.\n"));
            std::process::exit(2);
        }
        match serde_json::from_str::<Input>(&text) {
            Ok(inp) => {
                say(&format!("{}\n", serde_json::to_string(&plan(&inp)).unwrap_or_default()));
                std::process::exit(0);
            }
            Err(e) => {
                eprint_all(&format!("daisugi fence: {e}. Nothing was changed.\n"));
                std::process::exit(2);
            }
        }
    }
    if o.cmd.is_empty() && !print {
        usage_error("Missing command. Write: daisugi fence -- COMMAND [ARGS]");
    }
    o.environ = std::env::vars_os()
        .filter_map(|(k, v)| Some((k.to_str()?.to_string(), v.to_str()?.to_string())))
        .collect();
    o.cwd = std::env::current_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
    o.self_exe = std::env::current_exe().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();

    // No variable turns the fence off: a process inside a fenced pane sets
    // its children's environment (FN-R-15). Only --no-fence does.
    if no_fence {
        if print {
            say("fence: off\n");
            std::process::exit(0);
        }
        exec_unfenced(&o.cmd, &o.environ, &o.cwd);
    }
    o.pw_home = pw_home();
    o.pid = std::process::id();
    let (inp, exe) = match gather(&o) {
        Ok(x) => x,
        Err(e) => {
            eprint_all(&format!("daisugi fence: {e}. Nothing was changed.\n"));
            std::process::exit(2);
        }
    };
    let pl = plan(&inp);
    let abi = check_abi(&kernel_abi, weak_ipc);
    if print {
        print_plan(&inp, &pl, &abi, as_json);
        std::process::exit(0);
    }
    if !pl.refusals.is_empty() {
        for r in &pl.refusals {
            eprint_all(&format!("daisugi fence: not starting: {r}\n"));
        }
        std::process::exit(1);
    }
    if let Err(e) = &abi {
        eprint_all(&format!(
            "daisugi fence: not starting the harness: {}.\nNothing was run. To run it without a fence, use --no-fence, or set fence = \"off\" in coppice's config.\n",
            e.0
        ));
        std::process::exit(1);
    }
    if exe.is_empty() {
        eprint_all(&format!("daisugi fence: {} is not on PATH.\n", o.cmd[0]));
        std::process::exit(127);
    }
    if !inp.tmpdir.is_empty() {
        if let Err(e) = make_tmpdir(&inp.tmpdir, o.tmpdir.is_empty()) {
            eprint_all(&format!("daisugi fence: not starting: the private temp dir: {e}\n"));
            std::process::exit(1);
        }
    }
    ensure_harness_dirs(&inp.harness, &inp.home, &inp.workspace);
    fence_gate_server(&inp, &o.self_exe);
    let guard = Guard::new(&inp);
    if let Err(e) = apply(&pl.rules, &kernel_abi, weak_ipc, Some(&guard)) {
        match e.strip_prefix("refused: ") {
            Some(why) => eprint_all(&format!("daisugi fence: not starting: {why}\n")),
            None => eprint_all(&format!("daisugi fence: not starting the harness: {e}.\n")),
        }
        std::process::exit(1);
    }
    if let Err(e) = close_inherited(&keep) {
        eprint_all(&format!("daisugi fence: not starting the harness: {e}.\n"));
        std::process::exit(1);
    }
    let mut environ = o.environ.clone();
    for k in STRIPPED {
        environ.remove(*k);
    }
    if !inp.tmpdir.is_empty() {
        environ.insert("TMPDIR".to_string(), inp.tmpdir.clone());
    }
    exec_program(&exe, &o.cmd, &environ, &[("DAISUGI_FENCED", "landlock")])
}

#[cfg(test)]
mod tmp_tests {
    use super::*;

    fn base(tag: &str) -> String {
        let t = std::env::var("TMPDIR").unwrap_or_else(|_| format!("{}/target", env!("CARGO_MANIFEST_DIR")));
        let d = format!("{t}/fence-n5-{tag}-{}", std::process::id());
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn n5_each_run_gets_a_fresh_leaf() {
        let d = base("fresh");
        let env: HashMap<String, String> = [("XDG_DATA_HOME".to_string(), d.clone())].into();
        let (a, b) = (own_tmpdir(&env, "/h", 42), own_tmpdir(&env, "/h", 42));
        assert_ne!(a, b);
        assert!(a.rsplit('/').next().unwrap().starts_with("42-"), "{a}");
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn n5_a_leaf_that_is_there_is_refused_and_dead_leaves_go() {
        let d = base("sweep");
        let parent = format!("{d}/fence-tmp");
        let old = format!("{parent}/7-old");
        std::fs::create_dir_all(&old).unwrap();
        assert!(make_tmpdir(&old, false).is_err());
        let dead = format!("{parent}/999999999-ab");
        let live = format!("{parent}/{}-cd", std::process::id());
        std::fs::create_dir_all(&dead).unwrap();
        std::fs::create_dir_all(&live).unwrap();
        make_tmpdir(&format!("{parent}/{}-ef", std::process::id()), true).unwrap();
        assert!(std::fs::metadata(&dead).is_err());
        assert!(std::fs::metadata(&live).is_ok());
        let _ = std::fs::remove_dir_all(&d);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn golden_rule_list_matches_the_oracle() {
        let path = std::env::var("FENCE_GOLDEN")
            .unwrap_or_else(|_| concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/fence/cases.jsonl").to_string());
        let text = std::fs::read_to_string(path).expect("golden cases");
        let mut n = 0;
        for line in text.lines() {
            let c: serde_json::Value = serde_json::from_str(line).unwrap();
            let inp: Input = serde_json::from_value(c["input"].clone()).unwrap();
            let want: Output = serde_json::from_value(c["expect"].clone()).unwrap();
            assert_eq!(plan(&inp), want, "case {}", c["name"]);
            n += 1;
        }
        assert!(n >= 40, "only {n} golden cases");
    }

    #[test]
    fn norm_matches_path_clean() {
        assert_eq!(norm("/home/u//proj/./x/.."), "/home/u/proj");
        assert_eq!(norm("/.."), "/");
        assert_eq!(norm("/"), "/");
        assert_eq!(norm("a/../.."), "..");
        assert_eq!(norm(""), ".");
    }
}
