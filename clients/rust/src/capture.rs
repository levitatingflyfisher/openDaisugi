//! The oracle's capture conversion (`opendaisugi/hook.py`): the captured
//! sessions under a captures root listed, and one session turned into a
//! journal trace with an envelope inferred from what it did. The Go
//! client's `internal/capture` is the reference.

use std::collections::BTreeSet;

use num_bigint::BigInt;
use num_traits::FromPrimitive;

use crate::cli::gateroot::{join, path_str as pathlib_str};
use crate::gate::py::text::{lower, repr, strip};
use crate::gate::pyjson::{loads, py_decode_error, py_type_name, Object, PyDecodeFail, Value};
use crate::gate::pymodel::{decode_stream, universal_newlines};
use crate::pathways::pmodel::{take_model, Id, Mode};
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;

fn unreadable(why: impl Into<String>) -> PwErr {
    PwErr::Unreadable(why.into())
}

/// Why a capture is not read or converted.
#[derive(Debug)]
pub enum CapErr {
    /// An exception Python raises on this input and the command does not
    /// catch: its class name and `str(exc)`.
    Raised(String, String),
    /// A plan ActionPlan refuses in a way this binary words exactly: a NaN
    /// or an infinity in a step's data (`models.non_finite_error`).
    /// Python's ValidationError is a ValueError, so auto-tend skips the
    /// session with its text.
    InvalidPlan(String),
    /// Input this binary does not read the Python way, or an I/O error.
    Pw(PwErr),
}

impl From<PwErr> for CapErr {
    fn from(e: PwErr) -> Self {
        CapErr::Pw(e)
    }
}

fn raised(typ: &str, msg: impl Into<String>) -> CapErr {
    CapErr::Raised(typ.into(), msg.into())
}

/// One row of `list_sessions`.
#[derive(Clone)]
pub struct Session {
    pub id: String,
    pub calls: usize,
    pub first_at: Value,
    pub last_at: Value,
    pub path: String,
}

fn base_name(p: &str) -> &str {
    p.rsplit('/').next().unwrap_or(p)
}

/// `PurePosixPath(name).stem`.
pub fn stem(name: &str) -> &str {
    match name.rfind('.') {
        Some(i) if i > 0 && i < name.len() - 1 => &name[..i],
        _ => name,
    }
}

/// `open(path, encoding="utf-8")` iterated by line: the lines Python hands
/// out (universal newlines), and the exception it raises reading them,
/// after those lines, which a caller reads first.
fn lines(path: &str) -> Result<(Vec<String>, Option<CapErr>), CapErr> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        Err(e) if e.raw_os_error() == Some(libc::EISDIR) => {
            return Err(raised("IsADirectoryError", format!("[Errno 21] Is a directory: {}", repr(path))))
        }
        Err(e) => return Err(PwErr::Io(e).into()),
    };
    let (text, dec) = decode_stream(&raw);
    let text = universal_newlines(&text);
    let out = text.split_inclusive('\n').filter(|l| !l.is_empty()).map(|l| l.to_string()).collect();
    Ok((out, dec.map(|m| raised("UnicodeDecodeError", m))))
}

/// `json.loads(line)`: the value, None for a JSONDecodeError, or the other
/// exception it raises.
fn load(line: &str) -> Result<Option<Value>, CapErr> {
    match py_decode_error(line, 900) {
        Some(PyDecodeFail::Raised(m)) if m.starts_with("Exceeds the limit ") => return Err(raised("ValueError", m)),
        Some(PyDecodeFail::Raised(_)) => return Ok(None),
        Some(PyDecodeFail::TooDeep) => return Err(unreadable("a line nested deeper than this binary decodes").into()),
        None => {}
    }
    match loads(line) {
        Ok(v) => Ok(Some(v)),
        Err(_) => Err(unreadable("a line this binary does not decode").into()),
    }
}

/// A number as Python orders it: an exact int, a float, or nan.
enum Num {
    Nan,
    Float(f64),
    Int(BigInt),
}

fn numeric(v: &Value) -> Option<Num> {
    match v {
        Value::Float(f) if f.is_nan() => Some(Num::Nan),
        Value::Float(f) => Some(Num::Float(*f)),
        Value::Int(t) => t.parse::<BigInt>().ok().map(Num::Int),
        Value::Bool(b) => Some(Num::Int(BigInt::from(*b as i64))),
        _ => None,
    }
}

/// `a < b` for an int and a float, exactly.
fn int_below_float(a: &BigInt, y: f64) -> bool {
    if y.is_infinite() {
        return y > 0.0;
    }
    let f = y.floor();
    let fb = BigInt::from_f64(f).unwrap_or_default();
    if y == f {
        *a < fb
    } else {
        *a <= fb
    }
}

/// `a < b` for a float and an int, exactly.
fn float_below_int(x: f64, b: &BigInt) -> bool {
    if x.is_infinite() {
        return x < 0.0;
    }
    BigInt::from_f64(x.floor()).unwrap_or_default() < *b
}

/// Python's `a < b` for the values a captured_at can hold: numbers (bool
/// is an int) and str compare; any other pair raises TypeError. Two lists
/// compare item by item, which this binary does not model.
fn less(a: &Value, b: &Value) -> Result<bool, CapErr> {
    if let (Some(x), Some(y)) = (numeric(a), numeric(b)) {
        return Ok(match (x, y) {
            (Num::Nan, _) | (_, Num::Nan) => false,
            (Num::Float(x), Num::Float(y)) => x < y,
            (Num::Int(x), Num::Int(y)) => x < y,
            (Num::Int(x), Num::Float(y)) => int_below_float(&x, y),
            (Num::Float(x), Num::Int(y)) => float_below_int(x, &y),
        });
    }
    if let (Value::Str(x), Value::Str(y)) = (a, b) {
        // UTF-8 byte order is code point order.
        return Ok(x < y);
    }
    if matches!((a, b), (Value::List(_), Value::List(_))) {
        return Err(unreadable("two lists of captured_at to order").into());
    }
    Err(raised(
        "TypeError",
        format!("'<' not supported between instances of '{}' and '{}'", py_type_name(a), py_type_name(b)),
    ))
}

/// `list.sort(key=..., reverse=True)` on keys: the indexes in their sorted
/// order. CPython reverses the list, sorts it stably, and reverses it
/// again. Below 64 items its sort is one run found by count_run and
/// extended by binary insertion, which fixes where a key that compares
/// false both ways (nan) lands; that is followed here exactly. From 64
/// items on, it merges runs; a nan there is refused, and without one any
/// stable sort gives the same order.
fn sort_desc(keys: &[Value]) -> Result<Vec<usize>, CapErr> {
    let n = keys.len();
    let mut idx: Vec<usize> = (0..n).rev().collect();
    let mut failed: Option<CapErr> = None;
    let lt = |i: usize, j: usize, failed: &mut Option<CapErr>| -> bool {
        if failed.is_some() {
            return false;
        }
        match less(&keys[i], &keys[j]) {
            Ok(r) => r,
            Err(e) => {
                *failed = Some(e);
                false
            }
        }
    };
    if n < 64 {
        if n > 1 {
            // count_run
            let mut run = 2;
            if lt(idx[1], idx[0], &mut failed) {
                while run < n && lt(idx[run], idx[run - 1], &mut failed) {
                    run += 1;
                }
                idx[..run].reverse();
            } else {
                while run < n && !lt(idx[run], idx[run - 1], &mut failed) {
                    run += 1;
                }
            }
            // binarysort from the end of the run
            let mut start = run;
            while start < n && failed.is_none() {
                let pivot = idx[start];
                let (mut l, mut r) = (0, start);
                while l < r {
                    let p = l + ((r - l) >> 1);
                    if lt(pivot, idx[p], &mut failed) {
                        r = p;
                    } else {
                        l = p + 1;
                    }
                }
                idx.copy_within(l..start, l + 1);
                idx[l] = pivot;
                start += 1;
            }
        }
    } else {
        if keys.iter().any(|k| matches!(numeric(k), Some(Num::Nan))) {
            return Err(unreadable(format!("a captured_at of nan among {n} sessions")).into());
        }
        // Keys that do not all compare (a str among numbers, a list, a
        // dict) raise where the merge first meets them; which pair that is
        // is not modelled here.
        let all_num = keys.iter().all(|k| numeric(k).is_some());
        let all_str = keys.iter().all(|k| matches!(k, Value::Str(_)));
        if !all_num && !all_str {
            let why = format!("captured_at values of types that do not compare among {n} sessions");
            return Err(unreadable(why).into());
        }
        let mut out = idx.clone();
        out.sort_by(|&a, &b| {
            if lt(a, b, &mut failed) {
                std::cmp::Ordering::Less
            } else if lt(b, a, &mut failed) {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Equal
            }
        });
        idx = out;
    }
    if let Some(e) = failed {
        return Err(e);
    }
    idx.reverse();
    Ok(idx)
}

/// `list_sessions(root)`: every *.jsonl with a readable first and last
/// line, newest last_at first.
pub fn list_sessions(root: &str) -> Result<Vec<Session>, CapErr> {
    match std::fs::metadata(root) {
        Ok(m) if m.is_dir() => {}
        // Path.glob under a file finds nothing.
        Ok(_) => return Ok(vec![]),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound || e.raw_os_error() == Some(libc::ENOTDIR) => {
            return Ok(vec![])
        }
        Err(e) => return Err(PwErr::Io(e).into()),
    }
    let base = pathlib_str(root);
    let mut names: Vec<String> = vec![];
    for e in std::fs::read_dir(root).map_err(PwErr::Io)? {
        let e = e.map_err(PwErr::Io)?;
        let Ok(name) = e.file_name().into_string() else {
            return Err(unreadable("a capture's file name is not UTF-8").into());
        };
        // pathlib's glob "*" takes a name that starts with a dot too.
        if name.ends_with(".jsonl") {
            names.push(join(&base, &name));
        }
    }
    names.sort();
    let mut rows = vec![];
    for f in names {
        let (ls, err) = lines(&f)?;
        if let Some(e) = err {
            return Err(e);
        }
        let nonblank: Vec<&String> = ls.iter().filter(|l| !strip(l).is_empty()).collect();
        if nonblank.is_empty() {
            continue;
        }
        let Some(fv) = load(nonblank[0])? else { continue };
        let Some(lv) = load(nonblank[nonblank.len() - 1])? else { continue };
        let no_get = |v: &Value| raised("AttributeError", format!("'{}' object has no attribute 'get'", py_type_name(v)));
        let Value::Obj(fo) = &fv else { return Err(no_get(&fv)) };
        let Value::Obj(lo) = &lv else { return Err(no_get(&lv)) };
        let first_at = fo.get("captured_at").cloned().unwrap_or(Value::Null);
        let last_at = lo.get("captured_at").cloned().unwrap_or(Value::Null);
        let id = stem(base_name(&f)).to_string();
        rows.push(Session { id, calls: nonblank.len(), first_at, last_at, path: f.clone() });
    }
    let keys: Vec<Value> =
        rows.iter().map(|r| if r.last_at.truthy() { r.last_at.clone() } else { Value::Int("0".into()) }).collect();
    let order = sort_desc(&keys)?;
    Ok(order.into_iter().map(|k| rows[k].clone()).collect())
}

/// A session's records as `captures_to_trace` reads them: each non-blank
/// line's JSON, lines that are not JSON skipped. An exception the
/// conversion raises on a record (one that is not a JSON object, or has no
/// step_type) is `Raised`; a record whose values it would read another
/// way than this binary is unreadable.
pub fn records(path: &str) -> Result<Vec<Object>, CapErr> {
    let (ls, err) = lines(path)?;
    let mut vals = vec![];
    for l in &ls {
        let l = strip(l);
        if l.is_empty() {
            continue;
        }
        if let Some(v) = load(l)? {
            vals.push(v);
        }
    }
    if let Some(e) = err {
        return Err(e);
    }
    let name = base_name(path);
    let mut out = Vec::with_capacity(vals.len());
    for v in vals {
        let o = match v {
            Value::Obj(o) => o,
            Value::List(_) => return Err(raised("TypeError", "list indices must be integers or slices, not str")),
            Value::Str(_) => return Err(raised("TypeError", "string indices must be integers, not 'str'")),
            other => {
                return Err(raised("TypeError", format!("'{}' object is not subscriptable", py_type_name(&other))))
            }
        };
        // A step_type that is not a str equals no step type: the record
        // adds nothing, as in Python.
        if o.get("step_type").is_none() {
            return Err(raised("KeyError", "'step_type'"));
        }
        for k in ["command", "path", "url", "mcp_server", "mcp_tool"] {
            if let Some(v) = o.get(k) {
                if v.truthy() && !matches!(v, Value::Str(_)) {
                    return Err(unreadable(format!("{k} in a record of {name} is not text")).into());
                }
            }
        }
        if let Some(v) = o.get("arguments") {
            if v.truthy() && !matches!(v, Value::Obj(_)) {
                return Err(unreadable(format!("arguments in a record of {name} is not an object")).into());
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
pub fn plan(recs: &[Object], task: &str) -> Result<Object, CapErr> {
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
        Ok(Value::Obj(o)) => Ok(o),
        Ok(_) => Err(unreadable("the plan is not a mapping").into()),
        Err(e) => {
            if e.errs.iter().all(|x| x.typ == "finite_number") {
                return Err(CapErr::InvalidPlan(e.text()));
            }
            Err(unreadable(e.text()).into())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn numbers_order_exactly_as_python_orders_them() {
        let int = |t: &str| Value::Int(t.into());
        // 2**53 + 1 is above the float 2**53, though as a float it equals it.
        assert!(less(&Value::Float(9007199254740992.0), &int("9007199254740993")).unwrap());
        assert!(!less(&int("9007199254740993"), &Value::Float(9007199254740992.0)).unwrap());
        assert!(less(&int("2"), &Value::Float(2.5)).unwrap());
        assert!(!less(&int("3"), &Value::Float(2.5)).unwrap());
        assert!(less(&Value::Float(f64::NEG_INFINITY), &int(&"9".repeat(400))).unwrap());
        assert!(!less(&Value::Float(f64::NAN), &int("1")).unwrap());
        assert!(matches!(less(&Value::Str("a".into()), &int("1")), Err(CapErr::Raised(t, _)) if t == "TypeError"));
    }

    #[test]
    fn sessions_sort_as_list_sort_reverse_does() {
        let f = Value::Float;
        // CPython keeps equal keys in their order, and a nan where its run
        // and binary insertion put it.
        assert_eq!(sort_desc(&[f(1.0), f(3.0), f(3.0), f(2.0)]).unwrap(), vec![1, 2, 3, 0]);
        assert_eq!(sort_desc(&[f(1.0), f(f64::NAN), f(2.0)]).unwrap(), vec![0, 1, 2]);
    }

    #[test]
    fn many_sessions_with_keys_that_do_not_compare_are_refused() {
        let mut keys: Vec<Value> = (0..64).map(|i| Value::Int(i.to_string())).collect();
        keys[10] = Value::Str("a".into());
        assert!(matches!(sort_desc(&keys), Err(CapErr::Pw(PwErr::Unreadable(_)))));
        keys[10] = Value::Int("10".into());
        assert_eq!(sort_desc(&keys).unwrap()[0], 63);
    }

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
