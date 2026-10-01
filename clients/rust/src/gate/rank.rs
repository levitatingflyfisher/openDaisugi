//! owner_rule.py, rank_rule.py and tree_rule.py: the verbs that are the
//! owner's. A shell line that runs one is a hard deny. The line is read per
//! simple command, with the verbs as arguments of a daisugi command; a line
//! that does not decompose, a command shlex cannot split, and a word that
//! holds a command string are read by word order.

use super::frames::frame;
use super::pyjson::Value;
use super::record::Record;
use super::{catch, Runner, R};
use crate::interpreter_parse::shlex_split;

/// `rank_rule.REFUSAL`.
pub const RANK_REFUSAL: &str = "only the owner records a ranking vote. Record it yourself.";

/// `tree_rule.REFUSAL`.
pub const TREE_REFUSAL: &str = "only the operator or the starter changes the delegation tree. Run it yourself.";

/// `owner_rule.REFUSAL`.
pub const GATE_CHANGE_REFUSAL: &str = "only the operator changes how the gate enforces. Run it yourself.";

/// `owner_rule.COMMANDS`: every top-level command of the CLI.
const COMMANDS: &[&str] = &[
    "batch",
    "bench",
    "config",
    "conformance",
    "coppice",
    "dashboard",
    "distill-repeats",
    "gardener",
    "gate",
    "gateway",
    "gateway-report",
    "generate-envelope",
    "graft",
    "help",
    "hook",
    "install",
    "journal",
    "lora",
    "mcp",
    "metrics",
    "models",
    "modules",
    "onboard",
    "orchestrate",
    "pack",
    "pathways",
    "rank",
    "registry",
    "release",
    "route",
    "router",
    "run",
    "setup",
    "start",
    "status",
    "tend",
    "tiers",
    "tree",
    "verify",
    "viz",
    "voice",
    "weave",
];

/// `owner_rule.SUBCOMMANDS`.
fn subcommands(group: &str) -> Option<&'static [&'static str]> {
    match group {
        "gate" => Some(&[
            "arm",
            "audit",
            "check",
            "disarm",
            "init",
            "proposals",
            "register",
            "replay",
            "report",
            "serve",
            "settings",
            "status",
        ]),
        "graft" => Some(&["install", "remove", "status"]),
        "pack" => Some(&["bundle", "install", "list", "remove", "run", "status"]),
        "router" => Some(&["label", "status", "stop"]),
        "rank" => Some(&["choose", "fit", "queue", "record"]),
        "tree" => Some(&["answer", "check", "end", "root", "spawn", "status"]),
        _ => None,
    }
}

/// A verb is its command and subcommand words.
pub type Verbs = &'static [&'static [&'static str]];

/// `owner_rule.RANK_VERBS`.
pub const RANK_VERBS: Verbs = &[&["rank", "record"]];
/// `owner_rule.TREE_VERBS`.
pub const TREE_VERBS: Verbs = &[
    &["gate", "register"],
    &["gate", "init"],
    &["start"],
    &["tree", "root"],
    &["tree", "spawn"],
    &["tree", "end"],
    &["tree", "answer"],
];
/// `owner_rule.GATE_VERBS`.
pub const GATE_VERBS: Verbs = &[
    &["gate", "disarm"],
    &["gate", "arm"],
    &["gate", "serve"],
    &["install"],
    &["graft", "install"],
    &["graft", "remove"],
];

/// `owner_rule.LABEL_VERBS`.
pub const LABEL_VERBS: Verbs = &[&["router", "label"]];

/// `owner_rule.LABEL_REFUSAL`.
pub const LABEL_REFUSAL: &str = "only the operator labels a task's outcome. Label it yourself.";

/// `owner_rule.PACK_VERBS`.
pub const PACK_VERBS: Verbs = &[&["pack", "install"], &["pack", "remove"], &["pack", "bundle"]];

/// `owner_rule.PACK_REFUSAL`.
pub const PACK_REFUSAL: &str = "only the operator installs, removes or bundles a pack. Run it yourself.";

/// `owner_rule._WORD`: the ASCII characters a word is made of.
fn word_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || "_./:=+@%,-".contains(c)
}

/// `owner_rule.words`: backslashes and quotes dropped, then the runs of
/// word characters. Any other character, non-ASCII included, ends a word.
fn words(text: &str) -> Vec<String> {
    let text: String = text.replace("\\\n", "").chars().filter(|c| !matches!(c, '\\' | '\'' | '"')).collect();
    let mut out = Vec::new();
    let mut cur = String::new();
    for c in text.chars() {
        if word_char(c) {
            cur.push(c);
        } else if !cur.is_empty() {
            out.push(std::mem::take(&mut cur));
        }
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

/// `owner_rule.is_head`.
fn is_head(w: &str) -> bool {
    let last = w.rsplit('/').next().unwrap_or(w);
    last == "daisugi" || last == "daisugi-py" || last == "opendaisugi" || last.starts_with("opendaisugi.")
}

/// `owner_rule.scan_words`: a head word, then each word of a verb, in that
/// order.
fn scan_words(text: &str, verbs: Verbs) -> bool {
    let ws = words(text);
    for verb in verbs {
        let mut want = 0usize;
        for w in &ws {
            if want == 0 {
                if is_head(w) {
                    want = 1;
                }
            } else if w == verb[want - 1] {
                want += 1;
                if want > verb.len() {
                    return true;
                }
            }
        }
    }
    false
}

/// `owner_rule.command_path`.
fn command_path(args: &[String]) -> Vec<&str> {
    let pos: Vec<&str> = args.iter().map(String::as_str).filter(|a| !a.starts_with('-')).collect();
    for (i, w) in pos.iter().enumerate() {
        if COMMANDS.contains(w) {
            if let Some(subs) = subcommands(w) {
                if let Some(x) = pos[i + 1..].iter().find(|x| subs.contains(x)) {
                    return vec![w, x];
                }
            }
            return vec![w];
        }
    }
    vec![]
}

/// `owner_rule._simple_hit`.
fn simple_hit(simple: &str, verbs: Verbs) -> bool {
    let tokens = match shlex_split(simple) {
        Ok(t) => t,
        Err(_) => return scan_words(simple, verbs),
    };
    for (i, tok) in tokens.iter().enumerate() {
        if is_head(tok) {
            let path = command_path(&tokens[i + 1..]);
            if !path.is_empty() && verbs.iter().any(|v| *v == path.as_slice()) {
                return true;
            }
        } else if tok.chars().any(|c| !word_char(c)) && scan_words(tok, verbs) {
            return true;
        }
    }
    // `owner_rule._SHLEX_GAPS`: forms shlex splits differently from the
    // shell are also read by word order.
    if ["\\\n", "$'", "$\""].iter().any(|f| simple.contains(f)) {
        return scan_words(simple, verbs);
    }
    false
}

impl Runner {
    /// `owner_rule._simple_commands`: the simple commands of a shell line,
    /// or None when it does not decompose or the parser raises.
    fn simple_commands(&self, command: &str) -> R<Option<Vec<String>>> {
        let _f = frame();
        Ok(match catch(self.decompose(command))? {
            Ok(d) if d.ok && !d.commands.is_empty() => Some(d.commands),
            _ => None,
        })
    }

    /// `owner_rule.runs_verb`.
    pub fn runs_verb(&self, command: &str, verbs: Verbs) -> R<bool> {
        let _f = frame();
        let simples = match self.simple_commands(command)? {
            None => return Ok(scan_words(command, verbs)),
            Some(s) => s,
        };
        Ok(simples.iter().any(|s| simple_hit(s, verbs)))
    }

    /// `owner_rule.shell_hit`: a shell call whose command is a string that
    /// runs one of `verbs`. Any error is a hit.
    fn shell_verb_hit(&self, rec: Option<&Record>, verbs: Verbs) -> R<bool> {
        let _f = frame();
        let got = catch((|| -> R<bool> {
            match rec {
                Some(r) if r.step_type == "shell" => match r.raw("command") {
                    Value::Str(c) => self.runs_verb(c, verbs),
                    _ => Ok(false),
                },
                _ => Ok(false),
            }
        })())?;
        Ok(got.unwrap_or(true))
    }

    /// `rank_rule.rank_record_hit`.
    pub fn rank_record_hit(&self, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        self.shell_verb_hit(rec, RANK_VERBS)
    }

    /// `tree_rule.tree_write_hit`.
    pub fn tree_write_hit(&self, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        self.shell_verb_hit(rec, TREE_VERBS)
    }

    /// `owner_rule.gate_change_hit`.
    pub fn gate_change_hit(&self, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        self.shell_verb_hit(rec, GATE_VERBS)
    }

    /// `owner_rule.label_hit`.
    pub fn label_hit(&self, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        self.shell_verb_hit(rec, LABEL_VERBS)
    }

    /// `owner_rule.pack_hit`.
    pub fn pack_hit(&self, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        self.shell_verb_hit(rec, PACK_VERBS)
    }
}

#[cfg(test)]
mod owner_rule_tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::OnceLock;
    use std::time::Instant;

    fn runner() -> Runner {
        Runner {
            env: HashMap::new(),
            home: "/home/user".into(),
            wd: OnceLock::new(),
            real_cache: Default::default(),
            root: String::new(),
            default_root: String::new(),
            mode: "enforce".into(),
            fmt: "claude".into(),
            session: None,
            captures: None,
            verify_timeout_s: 1.0,
            ask: false,
            ask_timeout_s: 0.0,
            checkpoints: false,
            t0: Instant::now(),
            resident: false,
            caller: Default::default(),
        }
    }

    #[test]
    fn a_pack_change_is_the_operators() {
        let r = runner();
        for line in [
            "daisugi pack install train",
            "daisugi pack remove train",
            "daisugi pack bundle train --out /x",
            "cd /w && daisugi pack install train --offline",
        ] {
            assert!(r.runs_verb(line, PACK_VERBS).unwrap(), "{line}");
            assert!(!r.runs_verb(line, GATE_VERBS).unwrap(), "{line}");
        }
        for line in [
            "daisugi pack list",
            "daisugi pack status train",
            "daisugi pack run train selftest",
            "daisugi lora train --jsonl a --output b",
            "daisugi pack status gate disarm",
        ] {
            assert!(!r.runs_verb(line, PACK_VERBS).unwrap(), "{line}");
            assert!(!r.runs_verb(line, GATE_VERBS).unwrap(), "{line}");
        }
    }

    #[test]
    fn the_tree_verbs_read_per_command() {
        let r = runner();
        for line in [
            "daisugi gate register env.json",
            "daisugi --root /g gate --x register e.json",
            "daisugi tree spawn e.json --parent top --session kid",
            "daisugi tree status; daisugi gate register e.json",
            "echo daisugi tree end",
            "daisugi gate init --force",
            "daisugi start claude",
            "daisugi-py start",
            "daisugi status && npm start (",
        ] {
            assert!(r.runs_verb(line, TREE_VERBS).unwrap(), "{line}");
        }
        for line in [
            "daisugi tree check p.json c.json",
            "daisugi register gate",
            "daisugi trees spawn",
            "daisugi tree check end.json root.json",
            "daisugi registry init /r",
            "npm start",
            "daisugi status && npm start",
            "daisugi help start",
        ] {
            assert!(!r.runs_verb(line, TREE_VERBS).unwrap(), "{line}");
        }
    }

    #[test]
    fn the_gate_verbs_are_the_operators() {
        let r = runner();
        for line in [
            "daisugi gate disarm",
            "daisugi install --uninstall",
            "sh -c 'daisugi gate disarm'",
            "x=$(daisugi gate disarm)",
            "uv run --no-sync python -m opendaisugi.cli gate serve",
        ] {
            assert!(r.runs_verb(line, GATE_VERBS).unwrap(), "{line}");
        }
        for line in ["daisugi gate status", "daisugi status && npm install", "daisugi pathways show disarm"] {
            assert!(!r.runs_verb(line, GATE_VERBS).unwrap(), "{line}");
        }
        assert!(r.runs_verb("daisugi rank record", RANK_VERBS).unwrap());
        assert!(!r.runs_verb("daisugi status && rank record", RANK_VERBS).unwrap());
    }
}
