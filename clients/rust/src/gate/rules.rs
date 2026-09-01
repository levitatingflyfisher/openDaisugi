//! The gate's hard-deny rules: pane_rule.py (no agent answers an ask or
//! reads a coppice secret) and floor_config.py (no agent edits the floor's
//! config or OpenCode's). Each pattern is the oracle's own, run by the port
//! of Python's `re`, so Unicode text reads as it does in Python.

use super::effects::MAX_CWDS;
use super::frames::frame;
use super::paths::{
    basename, isabs, join2, normpath, path_join, path_name, path_parts, path_str, path_under_or_equal,
};
use super::py::re as pyre;
use super::pyjson::Object;
use super::pystr::py_split;
use super::record::{py_strings, Record};
use super::search::search_paths;
use super::{catch, Runner, R};
use crate::interpreter_parse::shlex_split;

pub const PANE_REFUSAL: &str = "a pane can propose. It cannot allow.";
pub const FLOOR_REFUSAL: &str = "this is the floor's own config. Edit it yourself.";
pub const OPENCODE_REFUSAL: &str = "this is OpenCode's config or gate plugin. Edit it yourself.";

/// An oracle pattern, tried only when the text holds every literal in
/// `all` and one of `any` (when `any` is not empty): a match needs them,
/// so most calls compile nothing, which keeps a cold gate fast.
struct Rule {
    all: &'static [&'static str],
    any: &'static [&'static str],
    src: &'static str,
}

impl Rule {
    const fn new(all: &'static [&'static str], any: &'static [&'static str], src: &'static str) -> Rule {
        Rule { all, any, src }
    }

    /// `pattern.search(t) is not None`.
    fn is_match(&self, t: &str) -> bool {
        if !self.all.iter().all(|n| t.contains(n)) || !(self.any.is_empty() || self.any.iter().any(|n| t.contains(n))) {
            return false;
        }
        // The patterns are the oracle's and compile; a pattern the port
        // could not run would read as a match, which denies.
        pyre::cached_compile(self.src).and_then(|r| r.search(t)).unwrap_or(true)
    }

    fn compiled(&self) -> std::sync::Arc<pyre::Regex> {
        pyre::cached_compile(self.src).expect("an oracle rule pattern compiles")
    }
}

// `coppice agent deny` is not an allow and passes: a task foreman denies
// the asks it holds that way, and coppice lets a pane deny only its own.
static ALLOW_TEXT: Rule = Rule::new(
    &["coppice", "agent", "allow"],
    &[],
    r##"\bcoppice\b[^\n;&|]*?\bagent\b[\s'\"]+(allow)\b"##,
);
static WIRE_TEXT: Rule = Rule::new(
    &["agent", "allow"],
    &[],
    r##"['\"]?(cmd|method)\\?['\"]?\s*[:=]\s*\\?['\"]agent\.(allow)\\?['\"]|['\"]agent['\"]\s*,\s*['\"](allow)['\"]"##,
);
static WEB_TOKEN_CMD: Rule =
    Rule::new(&["coppice", "web", "token"], &[], r##"\bcoppice\b[^\n;&|]*?\bweb\b[\s'\"]+token\b"##);
/// `pane_rule._SECRET_FILE`: every secret file a coppice data directory
/// holds, wherever the data directory lives.
static SECRET_FILE: Rule = Rule::new(
    &[],
    &["token", ".key"],
    r##"(^|[^A-Za-z0-9_])(web/token|web/ca/ca\.key|web/ca/leaf\.key|web/tls/tailscale\.key|voice/token)\b"##,
);
static ASK_PATH_TEXT: Rule = Rule::new(&["gate/"], &[], r##"(^|[\s'\"=:(/])gate/(asks|answers)\b"##);
static ASK_NAMES: Rule = Rule::new(&[], &["asks", "answers"], r##"\b(asks|answers)\b"##);
/// `floor_config._VARS`.
static HOME_VARS: Rule =
    Rule::new(&["$"], &[], r##"\$\{(HOME|XDG_CONFIG_HOME|XDG_DATA_HOME)\}|\$(HOME|XDG_CONFIG_HOME|XDG_DATA_HOME)\b"##);
/// `floor_config._OPENCODE_TEXT`.
static OPENCODE_TEXT: Rule = Rule::new(
    &[],
    &[".opencode", "opencode.json"],
    r##"(^|[^\w.-])\.opencode(/+(plugins?|tools?)(\b|/)|/*(?=$|[\s'\"`;&|)]))|\bopencode\.jsonc?\b"##,
);
/// `pane_rule._HOME_TILDE`: the `re.sub` pattern of `_home_spellings`.
static HOME_TILDE: Rule = Rule::new(&["~"], &[], r##"(^|[\s'\"=:(])~(?=/)"##);

/// `pane_rule._DENY_TEXT`: a deny in the CLI form or the wire form.
static DENY_TEXT: Rule = Rule::new(
    &["deny"],
    &[],
    r##"\bcoppice\b[^\n;&|]*?\bagent\b[\s'\"]+deny\b|['\"]?(cmd|method)\\?['\"]?\s*[:=]\s*\\?['\"]agent\.deny\\?['\"]|['\"]agent['\"]\s*,\s*['\"]deny['\"]"##,
);
/// `pane_rule._OTHER_SERVER`: another server or socket than the pane's own.
static OTHER_SERVER: Rule =
    Rule::new(&[], &[], r##"(^|[\s'\"])--(remote|socket)(=|[\s'\"]|$)|\bXDG_RUNTIME_DIR\b|\bHOME="##);
/// `pane_rule._WRAPPER_HEAD`: a wrapper at the head of a command, after a
/// quote, or after env assignments or a prefix such as env or exec.
static WRAPPER_HEAD: Rule = Rule::new(
    &[],
    &[],
    r##"(?:^|[\n;&|(){}`'\"!])\s*(?:\w+=\S*\s+)*(?:(?:\S*/)?(?:env|exec|command|builtin|nice|time|stdbuf|ionice|chrt|taskset|timeout)\b[^\n;&|]*?\s)?(?:\S*/)?(?:ssh|mosh|setsid|systemd-run|nohup|at|batch|tmux|screen|dtach|abduco|zellij|byobu|script|unbuffer|expect|daemonize|start-stop-daemon|sudo|doas|pkexec|su|runuser|machinectl|crontab|flatpak-spawn|distrobox-host-exec)(?:$|[\s;&|)'\"`])"##,
);
/// `pane_rule._WRAPPER_ANYWHERE`: wrapper names too telling to need the head.
static WRAPPER_ANYWHERE: Rule = Rule::new(
    &[],
    &[],
    r##"\b(ssh|setsid|systemd-run|nohup|tmux|daemonize|start-stop-daemon|start_new_session)\b"##,
);

/// `pane_rule.shell_denies_from_outside`: a shell line sends a deny through
/// another server or socket, or under a program that runs it outside the
/// pane's process tree and session, where the server would read it as the
/// operator's.
fn shell_denies_from_outside(command: &str) -> bool {
    DENY_TEXT.is_match(command)
        && (OTHER_SERVER.is_match(command) || WRAPPER_HEAD.is_match(command) || WRAPPER_ANYWHERE.is_match(command))
}

/// `pane_rule._SECRET_DIRS`: each directory under a coppice data directory
/// that holds a secret file.
const SECRET_DIRS: &[&str] = &["", "web", "web/ca", "web/tls", "voice"];

const WEB_ANSWER_ROUTE: &str = "/api/ask/answer";
const VALUE_OPTIONS: &[&str] = &["--socket", "--remote", "--data-dir"];
const ASK_DIRS: &[&str] = &["asks", "answers"];

/// `pane_rule._is_allow_words`.
fn is_allow_words(words: &[String]) -> bool {
    let _f = frame();
    for (i, w) in words.iter().enumerate() {
        if basename(w) != "coppice" {
            continue;
        }
        let mut j = i + 1;
        while j < words.len() && words[j].starts_with("--") {
            j += if VALUE_OPTIONS.contains(&words[j].as_str()) { 2 } else { 1 };
        }
        if j + 1 < words.len() && words[j] == "agent" && words[j + 1] == "allow" {
            return true;
        }
    }
    false
}

fn secret_file(text: &str) -> bool {
    SECRET_FILE.is_match(text)
}

/// `pane_rule.web_door`.
fn web_door(text: &str) -> bool {
    let _f = frame();
    WEB_TOKEN_CMD.is_match(text) || secret_file(text) || text.contains(WEB_ANSWER_ROUTE)
}

/// `pane_rule._under_ask_dirs`.
fn under_ask_dirs(p: &str, roots: &[String]) -> bool {
    let _f = frame();
    roots.iter().any(|root| ASK_DIRS.iter().any(|sub| path_under_or_equal(p, &path_join(root, sub))))
}

/// The floor and OpenCode rules: directories by prefix, a rule on a placed
/// absolute path, and a rule on raw text.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Guard {
    Floor,
    Opencode,
}

/// `floor_config.Guard`: a rule's roots, computed on each use, or worked
/// out once for one check (`_fixed`).
#[derive(Clone)]
pub struct GuardRoots {
    kind: Guard,
    fixed: Option<Vec<String>>,
}

/// `floor_config._opencode_path`.
fn opencode_path(p: &str) -> bool {
    let _f = frame();
    let parts = path_parts(p);
    let last = match parts.last() {
        None => return false,
        Some(l) => l.as_str(),
    };
    if last == "opencode.json" || last == "opencode.jsonc" || last == ".opencode" {
        return true;
    }
    // any(... for a, b in zip(...)) runs a generator frame.
    parts
        .windows(2)
        .any(|w| w[0] == ".opencode" && ["plugin", "plugins", "tool", "tools"].contains(&w[1].as_str()))
}

/// `floor_config._opencode_text`.
fn opencode_text(t: &str) -> bool {
    let _f = frame();
    OPENCODE_TEXT.is_match(t)
}

impl GuardRoots {
    /// `guard.names(path)`.
    fn names(&self, p: &str) -> bool {
        match self.kind {
            Guard::Opencode => opencode_path(p),
            Guard::Floor => {
                let _f = frame();
                false
            }
        }
    }

    /// `guard.text(t)`.
    fn text(&self, t: &str) -> bool {
        match self.kind {
            Guard::Opencode => opencode_text(t),
            Guard::Floor => {
                let _f = frame();
                false
            }
        }
    }
}

/// `git`'s global options that take the next word as their value.
const GIT_GLOBAL_VALUE: &[&str] =
    &["-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env"];
const GIT_MESSAGE_VERBS: &[&str] = &["commit", "tag", "merge", "notes"];

/// `floor_config._git_message_values`.
fn git_message_values(tokens: &[String]) -> Vec<String> {
    let _f = frame();
    if tokens.first().map(String::as_str) != Some("git") {
        return vec![];
    }
    let mut i = 1;
    while i < tokens.len() && tokens[i].starts_with('-') {
        i += if GIT_GLOBAL_VALUE.contains(&tokens[i].as_str()) { 2 } else { 1 };
    }
    if i >= tokens.len() {
        return vec![];
    }
    let verb = tokens[i].as_str();
    i += 1;
    if verb == "stash" {
        if i >= tokens.len() || (tokens[i] != "push" && tokens[i] != "save") {
            return vec![];
        }
        i += 1;
    } else if !GIT_MESSAGE_VERBS.contains(&verb) {
        return vec![];
    }
    let mut out = Vec::new();
    while i < tokens.len() {
        let t = tokens[i].as_str();
        if t == "--" {
            break;
        }
        if t == "-m"
            || t == "--message"
            || (t.starts_with('-') && !t.starts_with("--") && t.ends_with('m') && t.chars().count() > 2)
        {
            if i + 1 < tokens.len() {
                out.push(tokens[i + 1].clone());
            }
            i += 2;
            continue;
        }
        if let Some(v) = t.strip_prefix("--message=") {
            out.push(v.to_string());
        } else if t.starts_with("-m") && t.chars().count() > 2 {
            out.push(t[2..].to_string());
        }
        i += 1;
    }
    out
}

/// `floor_config._without_messages`.
fn without_messages(text: &str, messages: &[String]) -> String {
    let _f = frame();
    let mut t = text.to_string();
    for m in messages {
        if !m.is_empty() {
            t = t.replacen(m.as_str(), " ", 1);
        }
    }
    t
}

impl Runner {
    /// `pane_rule._home_spellings`.
    pub(super) fn home_spellings(&self, text: &str) -> R<String> {
        let _f = frame();
        let home = self.path_home()?;
        let text = text.replace("${HOME}", &home).replace("$HOME", &home);
        if !HOME_TILDE.is_match(&text) {
            return Ok(text);
        }
        // re.sub calls the lambda in a frame of its own; it reaches no
        // recursion.
        Ok(HOME_TILDE
            .compiled()
            .sub(&text, |m, s| format!("{}{home}", m.group(s, 1).unwrap_or_default()))
            .unwrap_or(text))
    }

    /// `str(Path(home_spellings(raw)).expanduser())`, joined to cwd (or the
    /// process's cwd) when relative: the start of each pane path check.
    fn pane_path(&self, raw: &str, cwd: &str) -> R<String> {
        let p = self.path_expanduser(&self.home_spellings(raw)?)?;
        if isabs(&p) {
            return Ok(p);
        }
        let base = if cwd.is_empty() { self.getwd()? } else { cwd.to_string() };
        Ok(path_join(&base, &p))
    }

    /// `pane_rule._commands`: the simple commands in a shell line, or the
    /// whole line when it does not parse.
    fn pane_commands(&self, command: &str) -> R<Vec<String>> {
        let _f = frame();
        Ok(match catch(self.decompose(command))? {
            Ok(d) if d.ok && !d.commands.is_empty() => d.commands,
            _ => vec![command.to_string()],
        })
    }

    /// `pane_rule.shell_allows`.
    fn shell_allows(&self, command: &str) -> R<bool> {
        let _f = frame();
        if web_door(command) {
            return Ok(true);
        }
        for c in &self.pane_commands(command)? {
            let words = match shlex_split(c) {
                Ok(w) => w,
                Err(_) => py_split(c).into_iter().map(String::from).collect(),
            };
            if is_allow_words(&words) {
                return Ok(true);
            }
        }
        Ok(ALLOW_TEXT.is_match(command) || WIRE_TEXT.is_match(command))
    }

    /// `pane_rule._roots`: the gate root in force and the default, each as
    /// written and resolved.
    fn gate_roots(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out: Vec<String> = Vec::new();
        for g in [&self.root, &self.default_root] {
            let e = self.path_expanduser(g)?;
            let r = self.resolve(&self.path_expanduser(g)?)?;
            for p in [e, r] {
                if !out.contains(&p) {
                    out.push(p);
                }
            }
        }
        Ok(out)
    }

    /// `pane_rule.path_touches_asks`: a tool's file path is inside a gate's
    /// asks or answers.
    fn path_touches_asks(&self, raw: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        if raw.is_empty() {
            return Ok(false);
        }
        let p = self.pane_path(raw, cwd)?;
        let roots = self.gate_roots()?;
        let typed = path_str(&normpath(&p));
        let resolved = self.resolve(&p)?;
        // any(... for c in candidates) runs a generator frame.
        let _g = frame();
        Ok(under_ask_dirs(&typed, &roots) || under_ask_dirs(&resolved, &roots))
    }

    /// `pane_rule.path_names_secret`: a file path, as typed and as the OS
    /// resolves it from cwd, names a coppice secret.
    fn path_names_secret(&self, raw: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        if raw.is_empty() {
            return Ok(false);
        }
        if secret_file(raw) {
            return Ok(true);
        }
        let p = self.pane_path(raw, cwd)?;
        let normalized = normpath(&p);
        let resolved = self.resolve(&p)?;
        Ok(secret_file(&normalized) || secret_file(&resolved))
    }

    /// `pane_rule._secret_dirs`: every directory that holds a coppice secret,
    /// as written and as the OS resolves it.
    fn secret_dirs(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out: Vec<String> = Vec::new();
        for d in self.coppice_data_dirs()? {
            for sub in SECRET_DIRS {
                let p = if sub.is_empty() { d.clone() } else { path_join(&d, sub) };
                let r = self.resolve(&p)?;
                for s in [p, r] {
                    if !out.contains(&s) {
                        out.push(s);
                    }
                }
            }
        }
        Ok(out)
    }

    /// `pane_rule.path_holds_secret`: a path, as typed, normalized and
    /// resolved from cwd, is a directory that holds a coppice secret.
    fn path_holds_secret(&self, raw: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        if raw.is_empty() {
            return Ok(false);
        }
        let p = self.pane_path(raw, cwd)?;
        let dirs = self.secret_dirs()?;
        if dirs.contains(&normpath(&p)) {
            return Ok(true);
        }
        Ok(dirs.contains(&self.resolve(&p)?))
    }

    /// `pane_rule._search_refuses`.
    fn search_refuses(&self, command: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        self.shell_search_reaches(command, cwd)
    }

    /// `pane_rule.shell_touches_asks`.
    fn shell_touches_asks(&self, command: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        let text = self.home_spellings(command)?;
        if ASK_PATH_TEXT.is_match(&text) {
            return Ok(true);
        }
        let roots = self.gate_roots()?;
        let names = ASK_NAMES.is_match(&text);
        for root in &roots {
            for sub in ASK_DIRS {
                if text.contains(&format!("{root}/{sub}")) {
                    return Ok(true);
                }
            }
            if names && text.contains(root.as_str()) {
                return Ok(true);
            }
        }
        if names && !cwd.is_empty() {
            let here = path_str(&normpath(&self.path_expanduser(cwd)?));
            if roots.iter().any(|r| path_under_or_equal(&here, r)) {
                return Ok(true);
            }
        }
        Ok(false)
    }

    /// `pane_rule.pane_rule_hit`, less its catch (the caller reads any
    /// error as a hit).
    pub fn pane_rule_hit(&self, p: &Object, cwd: &str, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        let rec = match rec {
            None => return Ok(false),
            Some(r) => r,
        };
        match rec.step_type.as_str() {
            "shell" => {
                let command = rec.command.as_str();
                Ok(self.shell_allows(command)?
                    || {
                        let _g = frame();
                        shell_denies_from_outside(command)
                    }
                    // The search rule's own reason names the fix, so the glob
                    // check stands aside for a line that rule refuses.
                    || (self.shell_globs_secret(command, cwd)? && !self.search_refuses(command, cwd)?)
                    || self.shell_touches_asks(command, cwd)?)
            }
            "file_write" => self.path_touches_asks(&rec.path, cwd),
            "file_read" => {
                let path = rec.path.as_str();
                if web_door(path) || self.path_names_secret(path, cwd)? {
                    return Ok(true);
                }
                let tool = rec.tool_name.as_str();
                // Glob prints names only; Read opens only the one file its
                // path names; a search tool also searches its cwd.
                if tool == "Glob" {
                    return Ok(false);
                }
                if self.path_holds_secret(path, cwd)? {
                    return Ok(true);
                }
                let one_file = tool == "Read" || tool == "read";
                if !one_file {
                    let paths = {
                        let _g = frame();
                        search_paths(p)
                    };
                    match paths {
                        None => return Ok(true),
                        Some(ps) => {
                            // any(... for p in paths) runs a generator frame.
                            let _g = frame();
                            for x in &ps {
                                if self.path_holds_secret(x, cwd)? {
                                    return Ok(true);
                                }
                            }
                        }
                    }
                }
                Ok(!one_file && self.path_holds_secret(cwd, cwd)?)
            }
            "mcp" => {
                let mut strings = Vec::new();
                py_strings(&super::pyjson::Value::Obj(rec.arguments.clone()), 0, &mut strings);
                // any(... for v in _strings(...)) runs a generator frame.
                let _g = frame();
                for v in &strings {
                    if web_door(v)
                        || self.path_names_secret(v, cwd)?
                        || self.path_touches_asks(v, cwd)?
                        || self.shell_touches_asks(v, cwd)?
                    {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            _ => Ok(false),
        }
    }

    /// `floor_config._spellings`: each root as written and as the OS
    /// resolves it, once each.
    fn spellings(&self, input: Vec<String>) -> R<Vec<String>> {
        let _f = frame();
        let mut out: Vec<String> = Vec::new();
        for p in input {
            let e = self.path_expanduser(&p)?;
            let r = self.resolve(&self.path_expanduser(&p)?)?;
            for x in [e, r] {
                if !out.contains(&x) {
                    out.push(x);
                }
            }
        }
        Ok(out)
    }

    fn env_nonempty(&self, k: &str) -> Option<String> {
        self.env.get(k).filter(|v| !v.is_empty()).cloned()
    }

    /// `floor_config._roots` or `floor_config.opencode_config_roots`, in the
    /// frame the caller opened for it.
    fn compute_roots(&self, kind: Guard) -> R<Vec<String>> {
        let home = self.path_home()?;
        let mut out = Vec::new();
        for base in [self.env_nonempty("XDG_CONFIG_HOME"), Some(path_join(&home, ".config"))].into_iter().flatten() {
            out.push(path_join(&base, if kind == Guard::Floor { "coppice" } else { "opencode" }));
        }
        if kind == Guard::Floor {
            for base in [self.env_nonempty("XDG_DATA_HOME"), Some(path_join(&home, ".local/share"))].into_iter().flatten()
            {
                out.push(path_join(&base, "coppice"));
            }
            out.push(path_join(&home, ".opendaisugi/coppice"));
            out.extend(self.custom_data_dirs());
        }
        self.spellings(out)
    }

    /// `guard.roots()`: one frame, for the roots function or for the lambda
    /// of a fixed guard.
    fn guard_roots(&self, g: &GuardRoots) -> R<Vec<String>> {
        let _f = frame();
        match &g.fixed {
            Some(r) => Ok(r.clone()),
            None => self.compute_roots(g.kind),
        }
    }

    /// `floor_config._fixed`.
    fn fixed(&self, g: &GuardRoots) -> R<GuardRoots> {
        let _f = frame();
        Ok(GuardRoots { kind: g.kind, fixed: Some(self.guard_roots(g)?) })
    }

    /// `floor_config.custom_data_dirs`: the COPPICE_DATA_DIR the caller's
    /// pane names, when it is absolute.
    fn custom_data_dirs(&self) -> Vec<String> {
        let _f = frame();
        match self.env.get("COPPICE_DATA_DIR") {
            Some(v) if !v.is_empty() && isabs(v) => vec![path_str(&normpath(v))],
            _ => vec![],
        }
    }

    /// `floor_config.coppice_data_dirs`: the default data directory and any
    /// the caller's pane names, each as written and as resolved.
    pub(super) fn coppice_data_dirs(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut data = vec![path_join(&self.path_home()?, ".opendaisugi/coppice")];
        data.extend(self.custom_data_dirs());
        self.spellings(data)
    }

    /// `floor_config._expand_home`.
    pub(super) fn expand_home(&self, text: &str) -> R<String> {
        let _f = frame();
        let home = self.path_home()?;
        let text = if HOME_VARS.is_match(text) {
            HOME_VARS
                .compiled()
                .sub(text, |m, s| {
                    let name = m.group(s, 1).or_else(|| m.group(s, 2)).unwrap_or_default();
                    if name == "HOME" {
                        home.clone()
                    } else {
                        self.env.get(&name).cloned().unwrap_or_default()
                    }
                })
                .unwrap_or_else(|_| text.to_string())
        } else {
            text.to_string()
        };
        let chars: Vec<char> = text.chars().collect();
        let mut out = String::with_capacity(text.len());
        for (i, &ch) in chars.iter().enumerate() {
            if ch == '~'
                && (i == 0 || " \t'\"=:(".contains(chars[i - 1]))
                && (i + 1 == chars.len() || chars[i + 1] == '/')
            {
                out.push_str(&home);
                continue;
            }
            out.push(ch);
        }
        Ok(out)
    }

    /// `floor_config._in_roots`.
    fn in_roots(&self, p: &str, g: &GuardRoots) -> R<bool> {
        let _f = frame();
        for root in self.guard_roots(g)? {
            if p == root || p.starts_with(&format!("{}/", root.trim_end_matches('/'))) {
                return Ok(true);
            }
        }
        Ok(g.names(p))
    }

    /// `floor_config.path_in_floor`.
    fn path_in_floor(&self, raw: &str, cwd: &str, g: &GuardRoots) -> R<bool> {
        let _f = frame();
        if raw.is_empty() {
            return Ok(false);
        }
        let word = self.expand_home(raw)?;
        let typed = normpath(&self.expanduser(&word)?);
        if isabs(&typed) && self.in_roots(&typed, g)? {
            return Ok(true);
        }
        let base = if cwd.is_empty() { self.getwd()? } else { cwd.to_string() };
        if let Some(resolved) = self.resolve_path(&word, Some(&base))? {
            if self.in_roots(&resolved, g)? {
                return Ok(true);
            }
        }
        self.in_roots(&normpath(&join2(&base, &self.expanduser(&word)?)), g)
    }

    /// `floor_config._names_root_part`.
    fn names_root_part(&self, word: &str, g: &GuardRoots) -> R<bool> {
        let _f = frame();
        let parts = path_parts(&normpath(word));
        let roots = self.guard_roots(g)?;
        // any(r.name in parts for r in ...) runs a generator frame.
        let _g = frame();
        Ok(roots.iter().any(|root| {
            let name = path_name(root);
            parts.iter().any(|p| *p == name)
        }))
    }

    /// `floor_config._shell_hit`.
    fn shell_hit(&self, command: &str, cwd: &str, g: &GuardRoots) -> R<(bool, Vec<String>)> {
        let _f = frame();
        let d = match catch(self.decompose(command))? {
            Ok(d) => d,
            // A line the rule cannot split (RecursionError near the limit)
            // is a hit. The oracle's one exception, ImportError for a
            // missing parser, cannot happen here.
            Err(_) => return Ok((true, vec![])),
        };
        let mut messages = Vec::new();
        if !d.ok || d.commands.is_empty() {
            return Ok((false, messages));
        }
        let mut here: Option<String> = if cwd.is_empty() { None } else { Some(cwd.to_string()) };
        let mut cwds: Vec<Option<String>> = vec![here.clone()];
        let or_slash = |p: &Option<String>| match p {
            Some(s) if !s.is_empty() => s.clone(),
            _ => "/".to_string(),
        };
        for text in &d.commands {
            let raw = match shlex_split(text) {
                Ok(r) => r,
                Err(_) => {
                    // The words cannot be placed, so the cwd after them is
                    // unknown.
                    here = None;
                    if !cwds.contains(&here) {
                        cwds.push(here.clone());
                    }
                    continue;
                }
            };
            let values = git_message_values(&raw);
            messages.extend(values.iter().cloned());
            let mut tokens = Vec::with_capacity(raw.len());
            for t in &raw {
                tokens.push(self.expand_home(t)?);
            }
            let mut skip = values.clone();
            for (word, orig) in tokens.iter().zip(raw.iter()) {
                if let Some(k) = skip.iter().position(|s| s == orig) {
                    skip.remove(k);
                    continue;
                }
                let mut word = word.as_str();
                if word.starts_with('-') && word.contains('=') {
                    word = word.split_once('=').map(|x| x.1).unwrap_or("");
                    if orig.starts_with("--message=") {
                        continue;
                    }
                }
                if isabs(&self.expanduser(word)?) || here.as_deref().is_some_and(|h| !h.is_empty()) {
                    if self.path_in_floor(word, &or_slash(&here), g)? {
                        return Ok((true, messages));
                    }
                } else if self.names_root_part(word, g)? {
                    return Ok((true, messages));
                }
            }
            here = self.next_cwd(&tokens, here)?;
            if !cwds.contains(&here) {
                if cwds.len() >= MAX_CWDS {
                    // Past this many directories the gate stops following
                    // cd, as it does for a cd it cannot follow.
                    here = None;
                }
                if !cwds.contains(&here) {
                    cwds.push(here.clone());
                }
            }
        }
        for p in d.writes.iter().chain(d.reads.iter()) {
            let p = self.expand_home(p)?;
            for c in &cwds {
                let known = c.as_deref().is_some_and(|s| !s.is_empty());
                if (isabs(&self.expanduser(&p)?) || known) && self.path_in_floor(&p, &or_slash(c), g)? {
                    return Ok((true, messages));
                }
                if !known && !isabs(&self.expanduser(&p)?) && self.names_root_part(&p, g)? {
                    return Ok((true, messages));
                }
            }
        }
        Ok((false, messages))
    }

    /// `floor_config.text_names_floor`.
    fn text_names_floor(&self, text: &str, cwd: &str, g: &GuardRoots, free_text: bool) -> R<bool> {
        let _f = frame();
        let g = self.fixed(g)?;
        let expanded = self.expand_home(text)?;
        for r in self.guard_roots(&g)? {
            if expanded.contains(r.as_str()) {
                return Ok(true);
            }
        }
        if !cwd.is_empty() && self.path_in_floor(".", cwd, &g)? {
            return Ok(true);
        }
        let (hit, messages) = self.shell_hit(text, cwd, &g)?;
        if hit {
            return Ok(true);
        }
        if free_text {
            return Ok(false);
        }
        let mut exp = Vec::with_capacity(messages.len());
        for m in &messages {
            exp.push(self.expand_home(m)?);
        }
        Ok(g.text(&without_messages(&expanded, &exp)))
    }

    /// `floor_config._hit`, less its catch (the caller reads any error as a
    /// hit). `floor_config_hit` and `opencode_plugin_hit` each add a frame.
    pub fn floor_hit(&self, cwd: &str, rec: Option<&Record>, kind: Guard) -> R<bool> {
        let _outer = frame();
        let _f = frame();
        let rec = match rec {
            None => return Ok(false),
            Some(r) => r,
        };
        let g = GuardRoots { kind, fixed: None };
        match rec.step_type.as_str() {
            "file_write" => self.path_in_floor(&rec.path, cwd, &g),
            "shell" => self.text_names_floor(&rec.command, cwd, &g, false),
            "mcp" => {
                if !cwd.is_empty() && self.path_in_floor(".", cwd, &g)? {
                    return Ok(true);
                }
                let base = if cwd.is_empty() { "/" } else { cwd };
                let mut strings = Vec::new();
                py_strings(&super::pyjson::Value::Obj(rec.arguments.clone()), 0, &mut strings);
                // any(... for v in _strings(...)) runs a generator frame.
                let _g = frame();
                for v in &strings {
                    if self.path_in_floor(v, base, &g)? || self.text_names_floor(v, "", &g, true)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            _ => Ok(false),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rule_patterns_match_the_oracles() {
        assert!(ALLOW_TEXT.is_match("coppice --x agent 'allow'"));
        assert!(ALLOW_TEXT.is_match("coppice --x agent\u{a0}allow"));
        assert!(!ALLOW_TEXT.is_match("coppiceé agent allow"));
        assert!(!ALLOW_TEXT.is_match("coppiced agent allow"));
        assert!(WIRE_TEXT.is_match(r#"{"cmd": "agent.allow"}"#));
        assert!(WIRE_TEXT.is_match(r#"method=\"agent.allow\""#));
        // A deny is not an allow and passes, on the wire and as words.
        assert!(!WIRE_TEXT.is_match(r#"method=\"agent.deny\""#));
        assert!(!ALLOW_TEXT.is_match("coppice agent deny a1"));
        let words = |s: &str| s.split(' ').map(String::from).collect::<Vec<_>>();
        assert!(is_allow_words(&words("coppice --socket s agent allow a1")));
        assert!(!is_allow_words(&words("coppice --socket s agent deny a1")));
        assert!(web_door("cat ~/.local/share/coppice/web/token"));
        for secret in ["web/token", "web/ca/ca.key", "web/ca/leaf.key", "web/tls/tailscale.key", "voice/token"] {
            assert!(web_door(&format!("/home/user/.opendaisugi/coppice/{secret}")), "{secret}");
            assert!(web_door(&format!("cat {secret}")), "{secret}");
        }
        for public in ["web/ca/ca.crt", "web/ca/leaf.crt", "web/ca/meta.json", "web/tls/tailscale.crt", "web/web.json"] {
            assert!(!web_door(&format!("/home/user/.opendaisugi/coppice/{public}")), "{public}");
        }
        assert!(!web_door("myweb/token") && !web_door("web/tokens"));
        assert!(opencode_text("ls .opencode/plugins/x") && opencode_text("cat .opencode") && opencode_text("opencode.json"));
        assert!(!opencode_text("ls a.opencode/plugin") && !opencode_text("x.opencodez"));
        assert_eq!(
            git_message_values(&["git", "-C", "d", "commit", "-am", "hi", "--message=x"].map(String::from)),
            vec!["hi".to_string(), "x".to_string()]
        );
    }
}
