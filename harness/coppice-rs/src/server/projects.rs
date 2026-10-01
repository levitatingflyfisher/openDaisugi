//! project.list: the operator's pinned projects, then the directories
//! panes last started in, and the default harness.

use std::collections::HashSet;
use std::sync::Arc;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::config;
use crate::gojson;
use crate::proto::{self, Request, Response};

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "project.list",
        Arc::new(|s, c, r| s.handle_project_list(c, r)),
    );
    let _ = s.handle(
        "project.remove",
        Arc::new(|s, c, r| s.handle_project_remove(c, r)),
    );
}

/// Go's filepath.Base: the last element after trailing slashes go, "/"
/// for a path of slashes only, "." for an empty one.
fn base_name(path: &str) -> String {
    if path.is_empty() {
        return ".".into();
    }
    let trimmed = path.trim_end_matches('/');
    if trimmed.is_empty() {
        return "/".into();
    }
    trimmed.rsplit('/').next().unwrap_or(trimmed).to_string()
}

fn project_row(path: &str, pinned: bool) -> Value {
    gojson::map(vec![
        ("path", json!(path)),
        ("name", json!(base_name(path))),
        ("pinned", json!(pinned)),
        (
            "missing",
            json!(!std::fs::metadata(path).is_ok_and(|m| m.is_dir())),
        ),
    ])
}

impl Server {
    /// Takes one directory out of the project list: the pinned list in
    /// coppice.toml and the recent list. The role check is in
    /// operator_verb: only the operator edits the list. A named web token
    /// may not edit the config either, the same as floor.chat.
    fn handle_project_remove(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        if c.token_named() {
            return proto::err_resp(
                &r.id,
                proto::ERR_UNAUTHORIZED,
                super::messages::CONFIG_REFUSAL,
            );
        }
        let path = r.str("path").unwrap_or_default();
        if path.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "project.remove needs path. Pass the project directory.",
            );
        }
        if !path.starts_with('/') {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "project.remove needs an absolute path. Pass the full project directory.",
            );
        }
        let given = crate::sys::clean(&path);
        let resolved = std::fs::canonicalize(&given)
            .map(|x| x.to_string_lossy().into_owned())
            .unwrap_or_else(|_| given.clone());
        // The raw string as sent matches too, so a hand-edited "/src/x/" pin
        // can be removed.
        let same = |p: &str| p == path || p == given || p == resolved;
        let mut found = false;
        match config::load() {
            Err(e) => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_INTERNAL,
                    &format!("cannot read {}: {e}", config::path().display()),
                )
            }
            Ok(Some(mut cfg)) => {
                let before = cfg.projects.len();
                cfg.projects.retain(|p| !same(p));
                if cfg.projects.len() != before {
                    found = true;
                    if let Err(e) = config::save(&cfg) {
                        return proto::err_resp(
                            &r.id,
                            proto::ERR_INTERNAL,
                            &format!("cannot write {}: {e}", config::path().display()),
                        );
                    }
                }
            }
            Ok(None) => {}
        }
        {
            let _g = self.label_mu.lock().unwrap_or_else(|e| e.into_inner());
            let dirs = self.recent_dirs();
            let left: Vec<String> = dirs.iter().filter(|d| !same(d)).cloned().collect();
            if left.len() != dirs.len() {
                found = true;
                self.save_recent_dirs(&left);
            }
        }
        if !found {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{resolved} is not a project. Run: coppice project list"),
            );
        }
        proto::ok_resp(&r.id, gojson::map(vec![("removed", json!(resolved))]))
    }

    fn handle_project_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let cfg = config::load().ok().flatten();
        let pinned = cfg.as_ref().map(|c| c.projects.clone()).unwrap_or_default();
        let pinned_set: HashSet<&String> = pinned.iter().collect();
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        for p in &pinned {
            if seen.insert(p.clone()) {
                out.push(project_row(p, true));
            }
        }
        for d in self.recent_dirs() {
            if !pinned_set.contains(&d) {
                out.push(project_row(&d, false));
            }
        }
        let def = cfg.map(|c| c.default).unwrap_or_default();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("projects", Value::Array(out)),
                ("default", json!(def)),
            ]),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::base_name;

    #[test]
    fn base_name_is_gos() {
        for (p, want) in [
            ("/a/b", "b"),
            ("/a/b/", "b"),
            ("/", "/"),
            ("//", "/"),
            ("", "."),
            ("a", "a"),
        ] {
            assert_eq!(base_name(p), want, "{p:?}");
        }
    }
}
