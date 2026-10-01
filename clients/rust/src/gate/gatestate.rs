//! gate_state_rule.py: no agent writes the gate's own state (the gate
//! root, and each data dir's router, journal, tree, gateway, weave and
//! envelope cache). Each function opens the frames of the oracle's, so a
//! decomposition near the recursion limit raises here where it raises
//! there.

use super::envelope::Envelope;
use super::frames::frame;
use super::paths::{basename, isabs, join2, normpath, path_parent, path_str};
use super::predicate::Stage;
use super::pyjson::{Object, Value};
use super::record::{py_strings, Record};
use super::rules::fixed_floor_guard;
use super::{catch, Runner, R};
use crate::interpreter_parse::shlex_split;

/// `gate_state_rule.STATE_PARTS`.
const STATE_PARTS: &[&str] = &["gate", "router", "journal", "tree", "gateway", "weave", "envelope_cache.db"];

/// `gate_state_rule.READ_ONLY`.
const READ_ONLY: &[&str] = &[
    ":", "[", "b2sum", "basename", "cat", "cd", "cmp", "comm", "cut", "df", "diff", "dirname", "du", "echo",
    "egrep", "false", "fgrep", "find", "grep", "head", "hexdump", "jq", "ls", "md5sum", "nl", "od", "paste",
    "popd", "printf", "pushd", "pwd", "readlink", "realpath", "sha1sum", "sha256sum", "sha512sum", "stat",
    "strings", "tail", "test", "tr", "true", "wc",
];

/// `gate_state_rule.FIND_ACTIONS`.
const FIND_ACTIONS: &[&str] =
    &["-delete", "-exec", "-execdir", "-fls", "-fprint", "-fprint0", "-fprintf", "-ok", "-okdir"];

/// `gate_state_rule._BOUNDARY`.
const BOUNDARY: &[u8] = b"/ \t\n\r'\"`;&|()<>:,";

/// `gate_state_rule.refusal`.
pub fn gate_state_refusal(dir: &str) -> String {
    format!("the gate's own state is under {dir}; only the operator writes there.")
}

/// One protected path as written and its spellings.
struct Entry {
    name: String,
    forms: Vec<String>,
}

fn push_new(out: &mut Vec<String>, s: String) {
    if !out.contains(&s) {
        out.push(s);
    }
}

fn under(p: &str, top: &str) -> bool {
    p == top || p.starts_with(&format!("{}/", top.trim_end_matches('/')))
}

/// `gate_state_rule._names_text`.
fn names_text(text: &str, top: &str) -> bool {
    let _f = frame();
    let mut start = 0;
    while let Some(k) = text[start..].find(top) {
        let i = start + k;
        let j = i + top.len();
        if j == text.len() || BOUNDARY.contains(&text.as_bytes()[j]) {
            return true;
        }
        start = i + 1;
        while !text.is_char_boundary(start) {
            start += 1;
        }
    }
    false
}

/// `gate_state_rule._link_sources`.
fn link_sources(words: &[String]) -> Vec<String> {
    let _f = frame();
    let head = basename(&words[0]);
    let args = &words[1..];
    if head == "cp" {
        let mut linked = false;
        for w in args {
            if w == "--" {
                break;
            }
            if w == "--link" || w == "--symbolic-link" {
                linked = true;
            } else if w.starts_with('-') && !w.starts_with("--") && (w.contains('l') || w.contains('s')) {
                linked = true;
            }
        }
        if !linked {
            return vec![];
        }
    } else if head != "ln" {
        return vec![];
    }
    let mut out = Vec::new();
    let mut flags = true;
    for w in args {
        if flags && w == "--" {
            flags = false;
        } else if flags && w.starts_with('-') && w != "-" {
            continue;
        } else {
            out.push(w.clone());
        }
    }
    out
}

fn is_env_assign(tok: &str) -> bool {
    let b = tok.as_bytes();
    if b.is_empty() || !(b[0].is_ascii_alphabetic() || b[0] == b'_') {
        return false;
    }
    for &c in &b[1..] {
        if c == b'=' {
            return true;
        }
        if !(c.is_ascii_alphanumeric() || c == b'_') {
            return false;
        }
    }
    false
}

impl Runner {
    /// `gate_state_rule.data_dirs`.
    fn state_data_dirs(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut dirs = Vec::new();
        if basename(&self.root) == "gate" {
            dirs.push(path_parent(&self.root));
        }
        dirs.extend(self.guarded_data_dirs()?);
        let mut out = Vec::new();
        for d in dirs {
            push_new(&mut out, normpath(&self.expanduser(&d)?));
        }
        Ok(out)
    }

    /// `gate_state_rule.protected_dirs`.
    fn protected_dirs(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out = vec![normpath(&self.expanduser(&self.root)?)];
        for d in self.state_data_dirs()? {
            for part in STATE_PARTS {
                push_new(&mut out, format!("{d}/{part}"));
            }
        }
        Ok(out)
    }

    /// `gate_state_rule._named_roots`.
    fn state_named_roots(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out = self.state_data_dirs()?;
        let gate = normpath(&self.expanduser(&self.root)?);
        if basename(&gate) != "gate" {
            push_new(&mut out, gate);
        }
        Ok(out)
    }

    /// `gate_state_rule._entries`.
    fn state_entries(&self, paths: Vec<String>) -> R<Vec<Entry>> {
        let _f = frame();
        let mut out = Vec::with_capacity(paths.len());
        for p in paths {
            let forms = self.state_spell(&p)?;
            out.push(Entry { name: p, forms });
        }
        Ok(out)
    }

    /// `gate_state_rule._spell`.
    fn state_spell(&self, p: &str) -> R<Vec<String>> {
        let _f = frame();
        let mut out = vec![p.to_string()];
        push_new(&mut out, self.realpath(p)?);
        Ok(out)
    }

    /// `gate_state_rule._placed`.
    fn state_placed(&self, raw: &str, cwd: &str) -> R<Vec<String>> {
        let _f = frame();
        let word = self.expand_home(raw)?;
        let mut out = Vec::new();
        let typed = normpath(&self.expanduser(&word)?);
        if isabs(&typed) {
            out.push(typed);
        }
        let base = if cwd.is_empty() { self.getwd()? } else { cwd.to_string() };
        if let Some(resolved) = self.resolve_path(&word, Some(&base))? {
            push_new(&mut out, resolved);
        }
        push_new(&mut out, normpath(&join2(&base, &self.expanduser(&word)?)));
        Ok(out)
    }

    /// `gate_state_rule._hit_of`.
    fn state_hit_of(&self, spellings: &[String], entries: &[Entry], above: bool) -> Option<String> {
        let _f = frame();
        for e in entries {
            for f in &e.forms {
                for s in spellings {
                    if under(s, f) || (above && under(f, s)) {
                        return Some(e.name.clone());
                    }
                }
            }
        }
        None
    }

    /// `gate_state_rule._names`.
    fn state_names(&self, command: &str, cwd: &str, forms: &[String]) -> R<bool> {
        let _f = frame();
        let g = fixed_floor_guard(forms.iter().map(|f| path_str(f)).collect());
        let expanded = self.expand_home(command)?;
        for f in forms {
            if names_text(&expanded, f) {
                return Ok(true);
            }
        }
        if !cwd.is_empty() && self.path_in_floor(".", cwd, &g)? {
            return Ok(true);
        }
        Ok(self.shell_hit(command, cwd, &g)?.0)
    }

    /// `gate_state_rule._shell_scan`.
    fn state_shell_scan(&self, command: &str) -> R<(bool, Vec<String>)> {
        let _f = frame();
        let d = self.decompose(command)?;
        if !d.ok {
            return Ok((false, vec![]));
        }
        let mut links = Vec::new();
        for text in &d.commands {
            let words = match shlex_split(text) {
                Ok(w) => w,
                Err(_) => return Ok((false, vec![])),
            };
            let mut i = 0;
            while i < words.len() && is_env_assign(&words[i]) {
                i += 1;
            }
            if i == words.len() {
                continue;
            }
            let head = basename(&words[i]);
            if crate::dialect_gen::WRITERS.iter().any(|(n, _)| *n == head) {
                links.extend(link_sources(&words[i..]));
                continue;
            }
            if !READ_ONLY.contains(&head) {
                return Ok((false, vec![]));
            }
            if head == "find" && words[i + 1..].iter().any(|w| FIND_ACTIONS.contains(&w.as_str())) {
                return Ok((false, vec![]));
            }
        }
        Ok((true, links))
    }

    /// `gate_state_rule._shell`.
    fn state_shell(
        &self,
        command: &str,
        cwd: &str,
        entries: &[Entry],
        named: &[Entry],
        env: &Envelope,
    ) -> R<Option<String>> {
        let _f = frame();
        let mut hit_root = None;
        for n in named {
            if self.state_names(command, cwd, &n.forms)? {
                hit_root = Some(n.name.clone());
                break;
            }
        }
        let Some(hit_root) = hit_root else { return Ok(None) };
        let step = Object::new().with("type", "shell").with("command", command);
        let stage = Stage::new(self, env, Value::Null, false);
        let Some(writes) = stage.step_write_paths(&step)? else { return Ok(Some(hit_root)) };
        let (known, links) = self.state_shell_scan(command)?;
        if !known {
            return Ok(Some(hit_root));
        }
        for w in writes.iter().chain(links.iter()) {
            if !isabs(&self.expanduser(w)?) && cwd.is_empty() {
                return Ok(Some(hit_root));
            }
            let placed = self.state_placed(w, cwd)?;
            if let Some(got) = self.state_hit_of(&placed, entries, true) {
                return Ok(Some(got));
            }
        }
        Ok(None)
    }

    fn gate_state_body(&self, cwd: &str, rec: Option<&Record>, env: &Envelope) -> R<Option<String>> {
        let Some(rec) = rec else { return Ok(None) };
        match rec.step_type.as_str() {
            "file_write" => {
                let entries = self.state_entries(self.protected_dirs()?)?;
                let placed = self.state_placed(&rec.path, cwd)?;
                Ok(self.state_hit_of(&placed, &entries, true))
            }
            "shell" => {
                let entries = self.state_entries(self.protected_dirs()?)?;
                let named = self.state_entries(self.state_named_roots()?)?;
                self.state_shell(&rec.command, cwd, &entries, &named, env)
            }
            "mcp" => {
                let entries = self.state_entries(self.protected_dirs()?)?;
                if !cwd.is_empty() {
                    let placed = self.state_placed(".", cwd)?;
                    if let Some(got) = self.state_hit_of(&placed, &entries, false) {
                        return Ok(Some(got));
                    }
                }
                let base = if cwd.is_empty() { "/" } else { cwd };
                let mut strings = Vec::new();
                py_strings(&Value::Obj(rec.arguments.clone()), 0, &mut strings);
                for v in &strings {
                    let placed = self.state_placed(v, base)?;
                    if let Some(got) = self.state_hit_of(&placed, &entries, false) {
                        return Ok(Some(got));
                    }
                }
                Ok(None)
            }
            _ => Ok(None),
        }
    }

    /// `gate_state_rule.gate_state_hit`: the protected path a tool call
    /// writes, as written, or None. Any error is a hit on the gate root.
    pub fn gate_state_hit(&self, cwd: &str, rec: Option<&Record>, env: &Envelope) -> R<Option<String>> {
        let _f = frame();
        match catch(self.gate_state_body(cwd, rec, env))? {
            Ok(v) => Ok(v),
            Err(_) => self.first_protected(),
        }
    }

    /// `gate_state_rule._first_protected`.
    fn first_protected(&self) -> R<Option<String>> {
        let _f = frame();
        Ok(Some(match catch(self.protected_dirs())? {
            Ok(v) => v[0].clone(),
            Err(_) => self.root.clone(),
        }))
    }
}
