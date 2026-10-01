//! The gate's ask files. The gate writes asks/<id>.json and waits for
//! answers/<id>.json; agent.allow and agent.deny write the answer the same
//! way the phone does.

use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
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
    let raw = std::fs::read(root.join("asks").join(format!("{id}.json")))
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
    let dir = root.join("answers");
    std::fs::create_dir_all(&dir).map_err(io_err)?;
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).map_err(io_err)?;
    let tmp = dir.join(format!(
        ".answer-{}-{}",
        std::process::id(),
        crate::server::Server::now_seconds().to_bits()
    ));
    let res = (|| {
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)?;
        f.write_all(body.as_bytes())?;
        drop(f);
        std::fs::rename(&tmp, dir.join(format!("{id}.json")))
    })();
    if res.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    res.map_err(io_err)
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
}
