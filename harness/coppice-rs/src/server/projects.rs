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
    ])
}

impl Server {
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
