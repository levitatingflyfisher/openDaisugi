//! The oracle's capture conversion (`opendaisugi/hook.py`): the captured
//! sessions under a captures root listed, and one session turned into a
//! journal trace with an envelope inferred from what it did. The Go
//! client's `internal/capture` is the reference.

use std::collections::BTreeSet;

use crate::gate::py::text::{lower, strip};
use crate::gate::pyjson::{loads_py, Object, Value};
use crate::pathways::pmodel::{take_model, Id, Mode};
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;

fn unreadable(why: impl Into<String>) -> PwErr {
    PwErr::Unreadable(why.into())
}

/// Why a session is not converted.
pub enum Skip {
    /// A plan ActionPlan refuses in a way this binary words exactly: a NaN
    /// or an infinity in a step's data (`models.non_finite_error`).
    /// Python's ValidationError is a ValueError, so auto-tend skips the
    /// session with its text.
    InvalidPlan(String),
}

/// One row of `list_sessions`.
#[derive(Clone)]
pub struct Session {
    pub id: String,
    pub calls: usize,
    pub last_at: Value,
    pub path: String,
}

fn base_name(p: &str) -> &str {
    p.rsplit('/').next().unwrap_or(p)
}

/// The file's lines, each with its line break, as `readlines` of a text
/// file gives them (universal newlines).
fn lines(path: &str) -> Result<Vec<String>, PwErr> {
    let raw = std::fs::read(path).map_err(PwErr::Io)?;
    let text = String::from_utf8(raw).map_err(|_| unreadable(format!("{} is not UTF-8", base_name(path))))?;
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    Ok(text.split_inclusive('\n').filter(|l| !l.is_empty()).map(|l| l.to_string()).collect())
}

/// A bool, an int or a finite float as a float, for ordering.
fn number(v: &Value) -> Option<f64> {
    match v {
        Value::Int(t) => t.parse::<f64>().ok(),
        Value::Float(f) if f.is_finite() => Some(*f),
        Value::Bool(b) => Some(*b as i64 as f64),
        _ => None,
    }
}

/// `list_sessions(root)`: every *.jsonl with a readable first and last
/// line, newest last_at first.
pub fn list_sessions(root: &str) -> Result<Vec<Session>, PwErr> {
    match std::fs::metadata(root) {
        Ok(m) if m.is_dir() => {}
        Ok(_) => return Ok(vec![]),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(vec![]),
        Err(e) => return Err(PwErr::Io(e)),
    }
    let mut names: Vec<String> = vec![];
    for e in std::fs::read_dir(root).map_err(PwErr::Io)? {
        let e = e.map_err(PwErr::Io)?;
        let name = e.file_name().to_string_lossy().into_owned();
        // pathlib's glob "*" takes a name that starts with a dot too.
        if name.ends_with(".jsonl") {
            names.push(format!("{}/{name}", root.trim_end_matches('/')));
        }
    }
    names.sort();
    let mut rows = vec![];
    for f in names {
        if std::fs::metadata(&f).map(|m| m.is_dir()).unwrap_or(true) {
            continue;
        }
        let ls = lines(&f)?;
        let nonblank: Vec<&String> = ls.iter().filter(|l| !strip(l).is_empty()).collect();
        if nonblank.is_empty() {
            continue;
        }
        let (first, last) = (nonblank[0], nonblank[nonblank.len() - 1]);
        let (fv, lv) = match (loads_py(first, 900), loads_py(last, 900)) {
            (Ok(a), Ok(b)) => (a, b),
            _ => continue,
        };
        let lo = match (fv, lv) {
            (Value::Obj(_), Value::Obj(lo)) => lo,
            _ => return Err(unreadable(format!("a line of {} is not a JSON object", base_name(&f)))),
        };
        let last_at = lo.get("captured_at").cloned().unwrap_or(Value::Null);
        if !last_at.is_null() && number(&last_at).is_none() {
            return Err(unreadable(format!("captured_at in {} is not a number", base_name(&f))));
        }
        let id = base_name(&f).strip_suffix(".jsonl").unwrap_or("").to_string();
        rows.push(Session { id, calls: nonblank.len(), last_at, path: f.clone() });
    }
    let key = |v: &Value| number(v).filter(|n| *n != 0.0).unwrap_or(0.0);
    rows.sort_by(|a, b| key(&b.last_at).partial_cmp(&key(&a.last_at)).unwrap_or(std::cmp::Ordering::Equal));
    Ok(rows)
}

/// A session's records: each non-blank line's JSON, lines that do not
/// parse skipped. A record whose shape the conversion would raise on is
/// refused.
pub fn records(path: &str) -> Result<Vec<Object>, PwErr> {
    let mut out = vec![];
    let name = base_name(path);
    for l in lines(path)? {
        let l = strip(&l);
        if l.is_empty() {
            continue;
        }
        let o = match loads_py(l, 900) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => return Err(unreadable(format!("a record of {name} is not a JSON object"))),
            Err(_) => continue,
        };
        if !matches!(o.get("step_type"), Some(Value::Str(_))) {
            return Err(unreadable(format!("a record of {name} has no step_type")));
        }
        for k in ["command", "path", "url", "mcp_server", "mcp_tool"] {
            if let Some(v) = o.get(k) {
                if v.truthy() && !matches!(v, Value::Str(_)) {
                    return Err(unreadable(format!("{k} in a record of {name} is not text")));
                }
            }
        }
        if let Some(v) = o.get("arguments") {
            if v.truthy() && !matches!(v, Value::Obj(_)) {
                return Err(unreadable(format!("arguments in a record of {name} is not an object")));
            }
        }
        out.push(o);
    }
    Ok(out)
}

fn str_of<'a>(o: &'a Object, k: &str) -> &'a str {
    match o.get(k) {
        Some(Value::Str(s)) => s,
        _ => "",
    }
}

/// `pathlib.PurePosixPath`'s root and parts.
fn posix_path(s: &str) -> (&'static str, Vec<&str>) {
    let root = if s.starts_with("//") && !s.starts_with("///") {
        "//"
    } else if s.starts_with('/') {
        "/"
    } else {
        ""
    };
    (root, s.split('/').filter(|p| !p.is_empty() && *p != ".").collect())
}

fn path_str(root: &str, parts: &[&str]) -> String {
    let s = format!("{root}{}", parts.join("/"));
    if s.is_empty() {
        ".".into()
    } else {
        s
    }
}

/// `hook._glob_for_path`.
fn glob_for_path(path: &str) -> String {
    if path.is_empty() {
        return "**".into();
    }
    let (root, parts) = posix_path(path);
    let parent: &[&str] = if parts.is_empty() { &parts } else { &parts[..parts.len() - 1] };
    if !root.is_empty() {
        let p = if parts.is_empty() { "/".to_string() } else { path_str(root, parent) };
        return format!("{}/**", p.trim_end_matches('/'));
    }
    let p = path_str(root, parent);
    if p.is_empty() || p == "." {
        return "./**".into();
    }
    format!("./{}/**", p.trim_end_matches('/'))
}

const MAX_OBSERVE_DEPTH: usize = 4;

/// `hook._observed_effects`.
fn observed_effects(parser: &mut ShellParser, command: &str, decompose: bool) -> (Vec<String>, Vec<String>, Vec<String>) {
    let (mut heads, mut reads, mut writes) = (vec![], vec![], vec![]);
    fn collect(
        p: &mut ShellParser,
        cmd: &str,
        depth: usize,
        decompose: bool,
        out: &mut (&mut Vec<String>, &mut Vec<String>, &mut Vec<String>),
    ) {
        if depth > MAX_OBSERVE_DEPTH {
            return;
        }
        let mut simple = None;
        if decompose {
            let d = p.decompose(cmd);
            if d.ok {
                out.1.extend(d.reads);
                out.2.extend(d.writes);
                simple = Some(d.commands);
            }
        }
        let simple = simple.unwrap_or_else(|| vec![cmd.to_string()]);
        for s in simple {
            if let Some(h) = crate::verify::extract_shell_head(strip(&s)) {
                if !h.is_empty() {
                    out.0.push(h);
                }
            }
            if let Some(pl) = crate::interpreter_parse::parse_interpreter(&s) {
                if !pl.opaque {
                    for inner in &pl.inner_commands {
                        collect(p, inner, depth + 1, decompose, out);
                    }
                }
            }
        }
    }
    collect(parser, command, 0, decompose, &mut (&mut heads, &mut reads, &mut writes));
    (heads, reads, writes)
}

fn url_host(url: &str) -> Option<&str> {
    let rest = url.strip_prefix("http://").or_else(|| url.strip_prefix("https://"))?;
    let end = rest.find('/').unwrap_or(rest.len());
    (end > 0).then(|| &rest[..end])
}

fn sorted(set: BTreeSet<String>) -> Value {
    Value::List(set.into_iter().map(Value::Str).collect())
}

/// `hook.infer_envelope`: an envelope admitting every captured call,
/// stakes low, with a fresh id.
pub fn infer_envelope(parser: &mut ShellParser, recs: &[Object], task: &str, decompose: bool) -> Result<Object, PwErr> {
    let (mut heads, mut reads, mut writes, mut hosts, mut mcp) =
        (BTreeSet::new(), BTreeSet::new(), BTreeSet::new(), BTreeSet::new(), BTreeSet::new());
    let mut network = false;
    for r in recs {
        match str_of(r, "step_type") {
            "shell" => {
                let cmd = strip(str_of(r, "command"));
                if cmd.is_empty() {
                    continue;
                }
                let (h, rs, ws) = observed_effects(parser, cmd, decompose);
                heads.extend(h);
                for p in rs {
                    if p != "/dev/null" && p != "/dev/stdin" {
                        reads.insert(glob_for_path(&p));
                    }
                }
                for p in ws {
                    if !["/dev/null", "/dev/stdout", "/dev/stderr"].contains(&p.as_str()) {
                        writes.insert(glob_for_path(&p));
                    }
                }
            }
            "file_read" => {
                reads.insert(glob_for_path(str_of(r, "path")));
            }
            "file_write" => {
                writes.insert(glob_for_path(str_of(r, "path")));
            }
            "network" => {
                network = true;
                if let Some(h) = url_host(str_of(r, "url")) {
                    hosts.insert(lower(h));
                }
            }
            "mcp" => {
                let (server, tool) = (str_of(r, "mcp_server"), str_of(r, "mcp_tool"));
                if !server.is_empty() && !tool.is_empty() && !(server == "opencode" && tool == "apply_patch") {
                    mcp.insert(format!("{server}/{tool}"));
                }
            }
            _ => {}
        }
    }
    let mut perm = Object::new();
    perm.set("shell", !heads.is_empty());
    perm.set("shell_allowlist", sorted(heads));
    perm.set("shell_allow_decomposition", decompose);
    perm.set("mcp_allowlist", sorted(mcp));
    perm.set("file_read", sorted(reads));
    perm.set("file_write", sorted(writes));
    perm.set("network", network);
    perm.set("network_hosts", sorted(hosts));
    perm.set("max_execution_time_s", Value::Int("60".into()));
    perm.set("max_output_size_mb", Value::Int("20".into()));
    let mut env = Object::new();
    env.set("generated_by", "opendaisugi.hook.infer_envelope");
    env.set("task", task);
    env.set("permissions", perm);
    env.set("stakes", "low");
    match take_model(Id::Envelope, Value::Obj(env), Mode::Python) {
        Ok(Value::Obj(o)) => Ok(o),
        Ok(_) => Err(unreadable("the inferred envelope is not a mapping")),
        Err(e) => Err(unreadable(e.text())),
    }
}

/// `ActionPlan(source="hook-capture", task, _records_to_steps(...))`.
pub fn plan(recs: &[Object], task: &str) -> Result<Result<Object, Skip>, PwErr> {
    let mut steps = vec![];
    let mut prev = String::new();
    for (i, r) in recs.iter().enumerate() {
        let sid = format!("s{i}");
        let deps = if prev.is_empty() { vec![] } else { vec![Value::Str(prev.clone())] };
        let mut st = Object::new();
        st.set("id", sid.as_str());
        st.set("depends_on", Value::List(deps));
        let t = str_of(r, "step_type");
        let made = match t {
            "shell" => {
                st.set("type", "shell");
                st.set("command", str_of(r, "command"));
                true
            }
            "file_read" => {
                st.set("type", "file_read");
                st.set("path", str_of(r, "path"));
                true
            }
            "file_write" => {
                st.set("type", "file_write");
                st.set("path", str_of(r, "path"));
                st.set("content", "");
                true
            }
            "network" => {
                st.set("type", "network");
                st.set("url", str_of(r, "url"));
                true
            }
            "mcp" => {
                st.set("type", "mcp");
                st.set("server", str_of(r, "mcp_server"));
                st.set("tool", str_of(r, "mcp_tool"));
                let args = match r.get("arguments") {
                    Some(v @ Value::Obj(_)) if v.truthy() => v.clone(),
                    _ => Value::Obj(Object::new()),
                };
                st.set("arguments", args);
                true
            }
            _ => false,
        };
        if made {
            steps.push(Value::Obj(st));
        }
        prev = sid;
    }
    let mut p = Object::new();
    p.set("source", "hook-capture");
    p.set("task", task);
    p.set("steps", Value::List(steps));
    match take_model(Id::ActionPlan, Value::Obj(p), Mode::Python) {
        Ok(Value::Obj(o)) => Ok(Ok(o)),
        Ok(_) => Err(unreadable("the plan is not a mapping")),
        Err(e) => {
            if e.errs.iter().all(|x| x.typ == "finite_number") {
                return Ok(Err(Skip::InvalidPlan(e.text())));
            }
            Err(unreadable(e.text()))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn globs_for_paths_are_the_hooks() {
        assert_eq!(glob_for_path(""), "**");
        assert_eq!(glob_for_path("a.txt"), "./**");
        assert_eq!(glob_for_path("sub//dir/./b.txt"), "./sub/dir/**");
        assert_eq!(glob_for_path("/"), "/**");
        assert_eq!(glob_for_path("/work/src/a.py"), "/work/src/**");
        assert_eq!(glob_for_path("/x"), "/**");
        assert_eq!(glob_for_path("//x/y"), "//x/**");
    }
}
