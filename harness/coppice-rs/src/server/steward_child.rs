//! A worker's whole transcript is kept as a small Pando store of its own,
//! beside the steward's: <store>/children/<label>/ (ADR-0023, addendum A).
//! It is never merged into the steward's tree and has no view in it. The
//! worker's report lands in the steward's log as one work message; that
//! message names the child store and its root ids, and zoom with
//! child=<label> goes down from there to the word. The twin of Go's
//! server/steward_child.go.

use std::collections::HashMap;
use std::time::{Duration, SystemTime};

use serde_json::{Map, Value};

use super::steward_chat::{StewardChat, ERR_NO_CHAT};
use super::Server;
use crate::steward::chat as fchat;
use crate::steward::ctx::Ctx;

/// Caps the text of a tool call or its result in a child store, as the
/// importer caps them.
const CHILD_TOOL_BYTES: usize = 2000;

struct ChildMessage {
    kind: &'static str,
    text: String,
    at: Option<SystemTime>,
}

/// Reads a sprig session file: the brief, what the worker said, and each
/// tool call with its result. In a child store the kind "steward" is the
/// worker's own words. A verdict row is the warden's and is left out.
fn child_messages_of(path: &str) -> Result<Vec<ChildMessage>, String> {
    let data = std::fs::read(path).map_err(|e| crate::sys::go_path_err("open", std::path::Path::new(path), &e))?;
    struct Call {
        name: String,
        input: String,
        at: Option<SystemTime>,
    }
    let mut out = Vec::new();
    let mut calls: HashMap<String, Call> = HashMap::new();
    let mut order: Vec<String> = Vec::new();
    let at_of = |v: &Value| -> Option<SystemTime> {
        v.get("ts")
            .and_then(|t| t.as_f64())
            .map(|f| SystemTime::UNIX_EPOCH + Duration::from_secs(f as i64 as u64))
    };
    for ln in data.split(|c| *c == b'\n') {
        let Ok(v) = serde_json::from_slice::<Value>(ln) else {
            continue;
        };
        if !v.is_object() {
            continue;
        }
        let at = at_of(&v);
        let text = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or("").to_string();
        match v.get("type").and_then(|t| t.as_str()).unwrap_or("") {
            "prompt" => {
                let t = text("text");
                if !t.trim().is_empty() {
                    out.push(ChildMessage { kind: fchat::KIND_USER, text: t, at });
                }
            }
            "assistant" => {
                let t = text("text");
                if !t.trim().is_empty() {
                    out.push(ChildMessage { kind: fchat::KIND_STEWARD, text: t, at });
                }
            }
            "tool_call" => {
                let id = text("toolUseId");
                let mut detail = text("detail");
                if detail.is_empty() {
                    detail = v.get("input").map_or("null".to_string(), |i| i.to_string());
                }
                calls.insert(
                    id.clone(),
                    Call {
                        name: text("name"),
                        input: crate::steward::fsutil::clip(&detail, CHILD_TOOL_BYTES).to_string(),
                        at,
                    },
                );
                order.push(id);
            }
            "tool_result" => {
                let id = text("toolUseId");
                let Some(c) = calls.remove(&id) else { continue };
                let ok = v.get("ok").and_then(|b| b.as_bool()).unwrap_or(false);
                let sum = text("summary");
                let state = if ok { "ok" } else { "failed" };
                out.push(ChildMessage {
                    kind: fchat::KIND_TOOL,
                    text: format!(
                        "{}({})\n{}: {}",
                        c.name,
                        c.input,
                        state,
                        crate::steward::fsutil::clip(&sum, CHILD_TOOL_BYTES)
                    ),
                    at: c.at,
                });
            }
            _ => {}
        }
    }
    // A call with no result (the worker ended inside it) is kept as it is.
    for id in order {
        if let Some(c) = calls.remove(&id) {
            out.push(ChildMessage {
                kind: fchat::KIND_TOOL,
                text: format!("{}({})\nno result", c.name, c.input),
                at: c.at,
            });
        }
    }
    Ok(out)
}

impl Server {
    /// Writes the transcript of a worker into its child store and returns
    /// the line the work message carries: the store's name and its root
    /// ids. A child that already holds messages is not written again. It
    /// returns None when the worker left no transcript; the report then
    /// stands alone.
    pub(super) fn record_child(&self, fc: &StewardChat, label: &str, session_file: &str) -> Option<String> {
        self.record_child_in(fc, label, "", session_file)
    }

    /// record_child for a worker of a branch: its transcript is kept under
    /// that branch, where the branch's lead and everyone above see it.
    pub(super) fn record_child_in(
        &self,
        fc: &StewardChat,
        label: &str,
        branch: &str,
        session_file: &str,
    ) -> Option<String> {
        if fc.store.as_os_str().is_empty() || session_file.is_empty() || !pando::child::valid_child(label) {
            return None;
        }
        let msgs = child_messages_of(session_file).ok().filter(|m| !m.is_empty());
        let msgs = msgs?;
        let base = if branch.is_empty() {
            fc.store.clone()
        } else {
            match pando::branch::ensure_branch(&fc.store, branch, &self.cfg.steward.clock.0) {
                Ok(d) => d,
                Err(e) => {
                    self.note_once(&format!("The transcript of {label} could not be kept: {e}"));
                    return None;
                }
            }
        };
        let mut c = match pando::child::open_child(
            &base,
            label,
            fchat::Config {
                now: self.cfg.steward.clock.0.clone(),
                ..Default::default()
            },
        ) {
            Ok(c) => c,
            Err(e) => {
                self.note_once(&format!("The transcript of {label} could not be kept: {e}"));
                return None;
            }
        };
        if c.count() == 0 {
            for m in &msgs {
                let at = m.at.unwrap_or_else(|| self.fm_now());
                if let (_, Some(e)) = c.append_at(m.kind, &m.text, Some(at)) {
                    self.note_once(&format!("The transcript of {label} could not be kept: {e}"));
                    break;
                }
            }
        }
        fc.mark_child(&c.dir().to_string_lossy());
        let prefix = format!("{label}:");
        let roots: Vec<String> = c
            .child_roots(label)
            .into_iter()
            .map(|r| r.strip_prefix(&prefix).unwrap_or(&r).to_string())
            .collect();
        c.close();
        if !branch.is_empty() {
            return Some(format!(
                "(transcript: child store {label} in branch {branch}, roots {}; zoom with branch={} child={})",
                roots.join(", "),
                crate::sys::go_quote(branch),
                crate::sys::go_quote(label)
            ));
        }
        Some(format!(
            "(transcript: child store {label}, roots {}; zoom with child={})",
            roots.join(", "),
            crate::sys::go_quote(label)
        ))
    }

    /// Builds the nodes of the child stores that were written since the
    /// last call, with the steward's own summarizer and its rule.
    pub(super) fn tend_kids(&self, fc: &StewardChat, ctx: &Ctx) -> Result<(), String> {
        let Some(sum) = fc.sum_ref() else { return Ok(()) };
        let kids = std::mem::take(&mut *fc.kids.lock().unwrap_or_else(|e| e.into_inner()));
        for (k, dir) in kids.iter().enumerate() {
            let (_, err) = pando::child::tend_dir(
                ctx,
                std::path::Path::new(dir),
                sum,
                0,
                self.cfg.steward.clock.0.clone(),
            );
            if let Some(e) = err {
                fc.kids
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .extend(kids[k..].iter().cloned());
                return Err(e);
            }
        }
        Ok(())
    }

    /// Answers zoom with child=<label>: the id and n are inside that
    /// worker's own store. The store is opened read only, by a name that
    /// cannot leave this store's children dir, so a zoom never reads
    /// another store.
    pub(super) fn zoom_child(
        &self,
        fc: &StewardChat,
        label: &str,
        args: &Map<String, Value>,
    ) -> Result<String, String> {
        if fc.store.as_os_str().is_empty() {
            return Err(ERR_NO_CHAT.into());
        }
        let mut args = args.clone();
        if !args.contains_key("child") && !label.is_empty() {
            args.insert("child".into(), Value::from(label));
        }
        let sc = pando::branch::Scope { store: fc.store.clone(), dir: fc.store.clone(), ..Default::default() };
        pando::mcp::tool_zoom_scope(&sc, &args)
    }
}

impl StewardChat {
    /// Says a child store (by its dir) has nodes to build, and wakes the
    /// compactor.
    pub(super) fn mark_child(&self, dir: &str) {
        self.kids
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(dir.to_string());
        self.wake();
    }

    /// The compactor's summarizer, when the chat has one.
    pub(super) fn sum_ref(&self) -> Option<&crate::steward::compactor::ModelSummarizer> {
        self.sum.as_deref()
    }
}
