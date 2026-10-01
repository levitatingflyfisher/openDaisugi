//! The gate's ask files. The gate writes asks/<id>.json and waits for
//! answers/<id>.json; agent.allow and agent.deny write the answer the same
//! way the phone does.

use std::path::Path;

use serde_json::{json, Value};

use crate::gojson;
use crate::state::{TIER_PERMANENT, TIER_UNDOABLE};

#[derive(Debug)]
pub enum AnswerError {
    NoAsk,
    BadDecision,
    BadScope,
    PermanentTask,
    NeedsName(String),
    Io(String),
}

impl std::fmt::Display for AnswerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            AnswerError::NoAsk => f.write_str("no pending ask"),
            AnswerError::BadDecision => f.write_str("decision must be allow or deny"),
            AnswerError::BadScope => f.write_str("scope must be once or task"),
            AnswerError::PermanentTask => {
                f.write_str("a permanent ask cannot be allowed for the task. Allow it once")
            }
            AnswerError::NeedsName(n) => {
                write!(f, "this cannot be undone. Type the pane name to allow: {n}")
            }
            AnswerError::Io(e) => f.write_str(e),
        }
    }
}

/// One answer to one ask.
pub struct Reply {
    pub tool_use_id: String,
    pub decision: String,
    pub reason: String,
    pub scope: String,
    pub confirm: String,
    pub tier: String,
    pub name: String,
    pub by: String,
    pub who_from: String,
}

fn worst_tier(a: &str, b: &str) -> &'static str {
    if a == TIER_UNDOABLE && b == TIER_UNDOABLE {
        TIER_UNDOABLE
    } else {
        TIER_PERMANENT
    }
}

/// The permanent rule: a permanent ask needs confirm equal to the name, and
/// may not be allowed for the task.
pub fn check_allow(tier: &str, name: &str, confirm: &str, scope: &str) -> Result<(), AnswerError> {
    if tier == TIER_UNDOABLE {
        return Ok(());
    }
    if scope == "task" {
        return Err(AnswerError::PermanentTask);
    }
    let confirm = confirm.trim();
    if name.is_empty() || confirm.is_empty() || confirm != name {
        return Err(AnswerError::NeedsName(name.into()));
    }
    Ok(())
}

/// The file stem of an ask id: every character outside [A-Za-z0-9._-]
/// becomes "_", dots are trimmed from both ends, cut to 128, or "none".
pub fn safe_id(raw: &str) -> String {
    let s: String = raw
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '.' || c == '_' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect();
    let s = s.trim_matches('.');
    let s: String = s.chars().take(128).collect();
    if s.is_empty() {
        "none".into()
    } else {
        s
    }
}

fn io_err(e: std::io::Error) -> AnswerError {
    AnswerError::Io(e.to_string())
}

/// Records one decision, echoing the nonce of the ask it answers. It writes
/// nothing for a bad decision or scope, a missing ask, or an allow that
/// breaks the permanent rule.
pub fn answer(root: &Path, r: &Reply) -> Result<(), AnswerError> {
    if r.decision != "allow" && r.decision != "deny" {
        return Err(AnswerError::BadDecision);
    }
    if r.decision == "allow" && !r.scope.is_empty() && r.scope != "once" && r.scope != "task" {
        return Err(AnswerError::BadScope);
    }
    let id = safe_id(&r.tool_use_id);
    // GR-R-10: the ask is read as the gate reads it, through no link and
    // only from a file this user owns that group and other cannot write. A
    // linked or shared ask is as good as gone.
    let raw = crate::server::gatefs::read(root, &["asks"], &format!("{id}.json"), None)
        .map_err(|_| AnswerError::NoAsk)?;
    let text = String::from_utf8_lossy(&raw);
    let nonce =
        crate::proto::struct_string_field(&text, "nonce").map_err(|_| AnswerError::NoAsk)?;
    if nonce.is_empty() {
        return Err(AnswerError::NoAsk);
    }
    if r.decision == "allow" {
        // The tier field is any: only a string counts.
        let file_tier = crate::proto::struct_string_field(&text, "tier").unwrap_or_default();
        check_allow(
            worst_tier(&r.tier, &file_tier),
            &r.name,
            &r.confirm,
            &r.scope,
        )?;
    }
    let scope = if r.scope.is_empty() || r.decision == "deny" {
        "once".to_string()
    } else {
        r.scope.clone()
    };
    let (by, from) = if r.by.is_empty() || r.who_from.is_empty() {
        ("local".to_string(), "none".to_string())
    } else {
        (r.by.clone(), r.who_from.clone())
    };
    let body = gojson::marshal(&gojson::map(vec![
        ("toolUseId", json!(r.tool_use_id)),
        ("decision", json!(r.decision)),
        ("reason", json!(r.reason)),
        ("updatedInput", Value::Null),
        ("nonce", json!(nonce)),
        ("scope", json!(scope)),
        ("by", json!(by)),
        ("whoFrom", json!(from)),
    ]));
    // GR-R-10: answers/ is checked with no link followed before anything is
    // written, and the answer is made by openat and renamed inside that
    // directory, so a link planted at answers/ or at the answer's name never
    // carries the write elsewhere.
    crate::server::gatefs::write(root, &["answers"], &format!("{id}.json"), body.as_bytes()).map_err(io_err)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn safe_id_matches_the_gate() {
        assert_eq!(safe_id("toolu_01/../x"), "toolu_01_.._x");
        assert_eq!(safe_id("..."), "none");
        assert_eq!(safe_id(".a."), "a");
    }

    #[test]
    fn a_permanent_ask_needs_the_name() {
        assert!(check_allow("undoable", "", "", "task").is_ok());
        assert!(matches!(
            check_allow("permanent", "p", "", ""),
            Err(AnswerError::NeedsName(_))
        ));
        assert!(matches!(
            check_allow("x", "p", "p", "task"),
            Err(AnswerError::PermanentTask)
        ));
        assert!(check_allow("permanent", "p", " p ", "once").is_ok());
    }

    // GR-R-10: the answer writer checks answers/ with no link followed before
    // it writes, and reads the ask as the gate does. Each test plants one
    // link and needs the refusal; before this change each of them wrote
    // through it. Twins: the Go tests in internal/web/ask_test.go.

    use std::os::unix::fs::{symlink, PermissionsExt};

    fn scratch(tag: &str) -> std::path::PathBuf {
        let base = std::env::var_os("TMPDIR")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| Path::new(env!("CARGO_MANIFEST_DIR")).join("target").join("test-tmp"));
        let d = base.join(format!("coppice-rs-ask-{}-{tag}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::set_permissions(&d, std::fs::Permissions::from_mode(0o700)).unwrap();
        d
    }

    fn write_ask(root: &Path) -> std::path::PathBuf {
        let dir = root.join("asks");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        let p = dir.join("toolu_01.json");
        std::fs::write(&p, "{\"toolUseId\": \"toolu_01\", \"nonce\": \"n1\"}").unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o600)).unwrap();
        p
    }

    fn deny() -> Reply {
        Reply {
            tool_use_id: "toolu_01".into(),
            decision: "deny".into(),
            reason: String::new(),
            scope: String::new(),
            confirm: String::new(),
            tier: String::new(),
            name: String::new(),
            by: String::new(),
            who_from: String::new(),
        }
    }

    #[test]
    fn answer_writes_nothing_through_a_symlinked_answers_dir() {
        let root = scratch("dir");
        write_ask(&root);
        let elsewhere = scratch("dir-else");
        symlink(&elsewhere, root.join("answers")).unwrap();
        assert!(answer(&root, &deny()).is_err());
        assert_eq!(std::fs::read_dir(&elsewhere).unwrap().count(), 0);
    }

    #[test]
    fn answer_replaces_a_link_at_the_answer_name_and_writes_nothing_through_it() {
        let root = scratch("name");
        write_ask(&root);
        std::fs::create_dir(root.join("answers")).unwrap();
        std::fs::set_permissions(root.join("answers"), std::fs::Permissions::from_mode(0o700)).unwrap();
        let victim = scratch("name-victim").join("victim");
        std::fs::write(&victim, "keep").unwrap();
        symlink(&victim, root.join("answers/toolu_01.json")).unwrap();
        answer(&root, &deny()).unwrap();
        assert_eq!(std::fs::read_to_string(&victim).unwrap(), "keep");
        let m = std::fs::symlink_metadata(root.join("answers/toolu_01.json")).unwrap();
        assert!(m.file_type().is_file(), "the link was not replaced");
    }

    #[test]
    fn answer_reads_no_ask_that_is_linked_shared_or_hard_linked() {
        for name in ["symlink", "group writable", "hard link"] {
            let root = scratch(&format!("ask-{}", name.replace(' ', "-")));
            let p = write_ask(&root);
            match name {
                "symlink" => {
                    let real = root.join("real.json");
                    std::fs::rename(&p, &real).unwrap();
                    symlink(&real, &p).unwrap();
                }
                "group writable" => std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o620)).unwrap(),
                _ => std::fs::hard_link(&p, root.join("second")).unwrap(),
            }
            assert!(matches!(answer(&root, &deny()), Err(AnswerError::NoAsk)), "{name}");
            assert!(!root.join("answers").exists(), "{name}: an answer directory was made");
        }
    }
}
