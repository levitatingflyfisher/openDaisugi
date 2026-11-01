//! The cards and tree routes: the arena of RK-7 and ST-5 and the delegation
//! tree on the floor. The queue, the tree and the answers are daisugi's.
//! The server runs the daisugi binary as a child process and never reads
//! or writes the rankings or the tree ledger (W3K-5). The twin of
//! internal/web/{daisugi,cards,tree}.go.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

use serde_json::json;

use super::{write_err, write_json, Server};
use crate::godec::Any;
use crate::web::http::{Request, Response};
use crate::web::upstream;

const DAISUGI_TIMEOUT: Duration = Duration::from_secs(10);
const DAISUGI_MAX_OUT: usize = 1 << 20;
const MAX_DIFF_BYTES: usize = 512 << 10;
const CARD_SORTS: [&str; 4] = ["reversibility", "impact", "date", "project"];

fn daisugi_bin() -> String {
    match std::env::var("COPPICE_DAISUGI_BIN") {
        Ok(b) if !b.is_empty() => b,
        _ => "daisugi".into(),
    }
}

fn daisugi_data_dir() -> String {
    std::env::var("COPPICE_DAISUGI_DATA_DIR").unwrap_or_default()
}

fn daisugi_args(args: Vec<String>) -> Vec<String> {
    daisugi_args_in(&daisugi_data_dir(), args)
}

/// Adds `--data-dir dir` to args; an empty dir is daisugi's own default.
fn daisugi_args_in(dir: &str, mut args: Vec<String>) -> Vec<String> {
    if !dir.is_empty() {
        args.push("--data-dir".into());
        args.push(dir.to_string());
    }
    args
}

fn data_label(d: &str) -> String {
    if d.is_empty() {
        "default".into()
    } else {
        d.to_string()
    }
}

fn data_dir_label() -> String {
    data_label(&daisugi_data_dir())
}

struct Ran {
    stdout: String,
    stderr: String,
    /// The exit code; -1 when the child did not exit by itself.
    code: i32,
    err: Option<String>,
}

/// Reads r to its end and keeps the first DAISUGI_MAX_OUT bytes.
fn read_capped(mut r: impl Read) -> Vec<u8> {
    let mut out = Vec::new();
    let mut buf = [0u8; 8192];
    loop {
        match r.read(&mut buf) {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                let room = DAISUGI_MAX_OUT.saturating_sub(out.len());
                out.extend_from_slice(&buf[..n.min(room)]);
            }
        }
    }
    out
}

/// Runs a command with a 10 s timeout and its output cut at 1 MiB.
fn run_cmd(mut cmd: Command) -> Ran {
    cmd.stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            return Ran { stdout: String::new(), stderr: String::new(), code: -1, err: Some(spawn_text(&cmd, &e)) }
        }
    };
    let out = child.stdout.take();
    let err = child.stderr.take();
    let (tx, rx) = mpsc::channel();
    let tx2 = tx.clone();
    std::thread::spawn(move || {
        let _ = tx.send((0, out.map(read_capped).unwrap_or_default()));
    });
    std::thread::spawn(move || {
        let _ = tx2.send((1, err.map(read_capped).unwrap_or_default()));
    });
    let deadline = Instant::now() + DAISUGI_TIMEOUT;
    let mut timed_out = false;
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break Some(s),
            Ok(None) => {}
            Err(_) => break None,
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            timed_out = true;
            break None;
        }
        std::thread::sleep(Duration::from_millis(5));
    };
    let mut bufs = [Vec::new(), Vec::new()];
    for _ in 0..2 {
        if let Ok((i, b)) = rx.recv_timeout(Duration::from_secs(2)) {
            bufs[i] = b;
        }
    }
    let stdout = String::from_utf8_lossy(&bufs[0]).into_owned();
    let stderr = String::from_utf8_lossy(&bufs[1]).into_owned();
    match status {
        Some(s) => match s.code() {
            Some(c) => Ran { stdout, stderr, code: c, err: None },
            None => Ran { stdout, stderr, code: -1, err: Some("signal: killed".into()) },
        },
        None if timed_out => Ran { stdout, stderr, code: -1, err: Some("timed out after 10 s".into()) },
        None => Ran { stdout, stderr, code: -1, err: Some("could not wait for the child".into()) },
    }
}

fn spawn_text(cmd: &Command, e: &std::io::Error) -> String {
    let name = cmd.get_program().to_string_lossy().into_owned();
    let why = match e.kind() {
        std::io::ErrorKind::NotFound => "no such file or directory".to_string(),
        _ => e.to_string(),
    };
    format!("fork/exec {name}: {why}")
}

fn run_daisugi(args: Vec<String>) -> Ran {
    let mut c = Command::new(daisugi_bin());
    c.args(args);
    run_cmd(c)
}

fn first_line(s: &str) -> String {
    for l in s.split('\n') {
        let l = l.trim();
        if !l.is_empty() {
            return l.to_string();
        }
    }
    String::new()
}

/// What a choice id, an attempt id or an ask id looks like. It never starts
/// with a dash, so it cannot read as a flag.
fn id_ok(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 128 {
        return false;
    }
    let first = b[0].is_ascii_alphanumeric() || b[0] == b'_';
    first
        && b[1..]
            .iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b':' | b'-'))
}

fn s(v: &str) -> String {
    v.to_string()
}

fn why_of(r: &Ran) -> String {
    match &r.err {
        Some(e) => e.clone(),
        None => first_line(&r.stderr),
    }
}

impl Server {
    /// Runs a daisugi command that prints one JSON object; answers the
    /// request with 502 when it cannot.
    fn daisugi_json(&self, resp: &mut Response, what: &str, args: Vec<String>) -> Option<Vec<(String, Any)>> {
        self.daisugi_json_in(Some(resp), &daisugi_data_dir(), what, args)
    }

    /// `daisugi_json` for one data dir. With no response a failure is
    /// logged and not answered: the caller goes on to the next dir.
    fn daisugi_json_in(
        &self,
        resp: Option<&mut Response>,
        dir: &str,
        what: &str,
        args: Vec<String>,
    ) -> Option<Vec<(String, Any)>> {
        let r = run_daisugi(daisugi_args_in(dir, args));
        if r.err.is_some() || r.code != 0 {
            let why = why_of(&r);
            crate::web::log("WARN", &format!("web: {what} failed"), &[("why", &why), ("data_dir", &data_label(dir))]);
            if let Some(resp) = resp {
                write_json(resp, 502, &crate::gojson::map(vec![("error", json!(format!("{what} failed: {why}")))]));
            }
            return None;
        }
        if r.stdout.trim() != "null" {
            if let Ok(Any::Map(m)) = crate::godec::decode_map(r.stdout.as_bytes()) {
                return Some(m);
            }
        }
        if let Some(resp) = resp {
            write_json(
                resp,
                502,
                &crate::gojson::map(vec![("error", json!(format!("{what} failed: its output is not JSON")))]),
            );
        }
        None
    }

    /// The data directories the floor reads cards from, in order: the
    /// configured one (empty for daisugi's default), then the extra ones the
    /// server owns that exist and are not the first again.
    fn data_sources(&self) -> Vec<String> {
        let first = daisugi_data_dir();
        let mut out = vec![first.clone()];
        for d in &self.opts.extra_data_dirs {
            let d = d.to_string_lossy().into_owned();
            if d.is_empty() || d == first || out.contains(&d) || !Path::new(&d).is_dir() {
                continue;
            }
            out.push(d);
        }
        out
    }

    pub(super) fn handle_cards(&self, req: &Request, resp: &mut Response) {
        let mut sort = req.query("sort").unwrap_or_default();
        if sort.is_empty() {
            sort = "reversibility".into();
        }
        if !CARD_SORTS.contains(&sort.as_str()) {
            write_err(resp, 400, "Sort by reversibility, impact, date or project.");
            return;
        }
        let mut doc: Option<Vec<(String, Any)>> = None;
        let mut cards_out: Vec<Any> = Vec::new();
        let mut seen: Vec<String> = Vec::new();
        for (i, dir) in self.data_sources().iter().enumerate() {
            let args = vec![s("rank"), s("queue"), s("--json"), s("--sort"), sort.clone()];
            let d = if i == 0 {
                self.daisugi_json_in(Some(resp), dir, "daisugi rank queue", args)
            } else {
                self.daisugi_json_in(None, dir, "daisugi rank queue", args)
            };
            let Some(d) = d else {
                if i == 0 {
                    return;
                }
                continue;
            };
            let decayed = d.iter().find(|(k, _)| k == "decayed").and_then(|(_, v)| match v {
                Any::F64(n) => Some(*n),
                _ => None,
            });
            let cards = d.iter().find(|(k, _)| k == "cards").map(|(_, v)| v.clone());
            match &mut doc {
                None => doc = Some(d),
                Some(first) => {
                    if let Some(n) = decayed {
                        let prev = first
                            .iter()
                            .find(|(k, _)| k == "decayed")
                            .and_then(|(_, v)| if let Any::F64(p) = v { Some(*p) } else { None })
                            .unwrap_or(0.0);
                        set(first, "decayed", Any::F64(prev + n));
                    }
                }
            }
            if let Some(Any::Arr(cs)) = cards {
                for c in cs {
                    let Any::Map(mut m) = c else { continue };
                    let id = m.iter().find(|(k, _)| k == "choice_id").and_then(|(_, v)| if let Any::Str(x) = v { Some(x.clone()) } else { None }).unwrap_or_default();
                    if !id.is_empty() && seen.contains(&id) {
                        continue;
                    }
                    seen.push(id);
                    set(&mut m, "data_dir", Any::Str(data_label(dir)));
                    cards_out.push(Any::Map(m));
                }
            }
        }
        let Some(mut doc) = doc else { return };
        set(&mut doc, "cards", Any::Arr(cards_out));
        set(&mut doc, "source", Any::Str(s("daisugi rank queue")));
        set(&mut doc, "data_dir", Any::Str(data_dir_label()));
        write_any_map(resp, 200, doc);
    }

    pub(super) fn handle_tree(&self, resp: &mut Response) {
        // The configured data dir and the steward's work dir: a steward
        // branch run puts its asks in the work dir. A node or an ask shows
        // once, whichever dir holds it.
        let mut doc: Option<Vec<(String, Any)>> = None;
        let mut lists: [(&str, &str, Vec<Any>); 2] = [("nodes", "session", vec![]), ("asks", "ask_id", vec![])];
        let mut seen: Vec<String> = Vec::new();
        for (i, dir) in self.data_sources().iter().enumerate() {
            let args = vec![s("tree"), s("status"), s("--json")];
            let d = if i == 0 {
                self.daisugi_json_in(Some(resp), dir, "daisugi tree status", args)
            } else {
                self.daisugi_json_in(None, dir, "daisugi tree status", args)
            };
            let Some(d) = d else {
                if i == 0 {
                    return;
                }
                continue;
            };
            let skipped = d.iter().find(|(k, _)| k == "skipped").and_then(|(_, v)| match v {
                Any::F64(n) => Some(*n),
                _ => None,
            });
            for (key, id_key, out) in lists.iter_mut() {
                let items = d.iter().find(|(k, _)| k == *key).map(|(_, v)| v.clone());
                let Some(Any::Arr(items)) = items else { continue };
                for it in items {
                    let Any::Map(mut m) = it else { continue };
                    let id = m
                        .iter()
                        .find(|(k, _)| k == *id_key)
                        .and_then(|(_, v)| if let Any::Str(x) = v { Some(x.clone()) } else { None })
                        .unwrap_or_default();
                    let tag = format!("{key}/{id}");
                    if !id.is_empty() && seen.contains(&tag) {
                        continue;
                    }
                    seen.push(tag);
                    set(&mut m, "data_dir", Any::Str(data_label(dir)));
                    out.push(Any::Map(m));
                }
            }
            match &mut doc {
                None => doc = Some(d),
                Some(first) => {
                    if let Some(n) = skipped {
                        let prev = first
                            .iter()
                            .find(|(k, _)| k == "skipped")
                            .and_then(|(_, v)| if let Any::F64(p) = v { Some(*p) } else { None })
                            .unwrap_or(0.0);
                        set(first, "skipped", Any::F64(prev + n));
                    }
                }
            }
        }
        let Some(mut doc) = doc else { return };
        for (key, _, out) in lists {
            set(&mut doc, key, Any::Arr(out));
        }
        set(&mut doc, "source", Any::Str(s("daisugi tree status")));
        write_any_map(resp, 200, doc);
    }

    /// The edits of each attempt on a card. The paths come from daisugi's
    /// own record of the card, and only a worktree under the branch
    /// directory or under a task's worktree is read (W3K-6).
    pub(super) fn handle_card_diff(&self, resp: &mut Response, id: &str) {
        if !id_ok(id) {
            write_err(resp, 404, "There is no such card.");
            return;
        }
        let mut r = run_daisugi(Vec::new());
        for dir in self.data_sources() {
            r = run_daisugi(daisugi_args_in(&dir, vec![s("rank"), s("show"), s("--choice"), s(id), s("--json")]));
            if !(r.err.is_none() && r.code == 1) {
                break; // found, or failed for a reason other than "no such card"
            }
        }
        if r.err.is_none() && r.code == 1 {
            write_err(resp, 404, &first_line(&r.stderr));
            return;
        }
        if r.err.is_some() || r.code != 0 {
            write_json(
                resp,
                502,
                &crate::gojson::map(vec![("error", json!(format!("daisugi rank show failed: {}", why_of(&r))))]),
            );
            return;
        }
        let places = where_of(&r.stdout);
        let roots = self.worktree_roots();
        let mut diffs: Vec<(String, Any)> = Vec::new();
        let mut notes: Vec<(String, Any)> = Vec::new();
        for (attempt, place) in places {
            if place.path.is_empty() {
                notes.push((attempt, Any::Str(s("no diff: this attempt has no worktree"))));
                continue;
            }
            if !under_any(&place.path, &roots) {
                notes.push((attempt, Any::Str(s("no diff: the alternative is outside the floor's worktrees"))));
                continue;
            }
            match worktree_diff(&place) {
                Ok(d) => diffs.push((attempt, Any::Str(d))),
                Err(note) => notes.push((attempt, Any::Str(note))),
            }
        }
        let doc = vec![
            (s("choice_id"), Any::Str(s(id))),
            (s("diffs"), Any::Map(diffs)),
            (s("notes"), Any::Map(notes)),
            (s("source"), Any::Str(s("daisugi rank show"))),
        ];
        write_any_map(resp, 200, doc);
    }

    /// The places a diff may be read from: the branch directory of each
    /// data dir, and the worktree of every task.
    fn worktree_roots(&self) -> Vec<PathBuf> {
        let mut roots = Vec::new();
        for mut data in self.data_sources() {
            if data.is_empty() {
                if let Ok(h) = std::env::var("HOME") {
                    if !h.is_empty() {
                        data = format!("{h}/.opendaisugi");
                    }
                }
            }
            if !data.is_empty() {
                roots.push(Path::new(&data).join("tree").join("branches"));
            }
        }
        if let Ok(msg) = upstream::call(&self.opts.dial, vec![("cmd", json!("task.list"))]) {
            if let Some(Any::Arr(tasks)) = msg.get("result").and_then(|r| r.get("tasks")) {
                for t in tasks {
                    let wt = t.str_of("worktree");
                    if !wt.is_empty() {
                        roots.push(PathBuf::from(wt));
                    }
                }
            }
        }
        roots
    }

    /// Refuses, and answers, any request that is not the operator's: a
    /// lock-screen deny token, a pane process, a local peer the server
    /// cannot place. True when the request may go on.
    fn operator_only(&self, req: &Request, resp: &mut Response, deny_only: &Option<String>) -> bool {
        if deny_only.is_some() {
            write_err(resp, 403, "This card can only deny its own ask.");
            return false;
        }
        if let Some(hello) = crate::web::peer::peer_hello(req.remote, req.local) {
            if !crate::web::peer::place_allows(&self.opts.dial, hello) {
                crate::web::log(
                    "WARN",
                    "web: refused an answer from a pane",
                    &[("remote", &req.remote.to_string()), ("path", &req.path)],
                );
                write_err(resp, 403, crate::web::peer::PANE_REFUSAL);
                return false;
            }
        }
        true
    }

    pub(super) fn handle_card_answer(&self, req: &Request, resp: &mut Response) {
        let Some((_name, deny_only)) = self.guard_answer(req, resp) else {
            return;
        };
        let body = &req.body[..req.body.len().min(16 << 10)];
        let Some(b) = decode_fields(body, &CARD_TY) else {
            write_err(resp, 400, "The request body is not JSON.");
            return;
        };
        if !self.operator_only(req, resp, &deny_only) {
            return;
        }
        let (choice, outcome, pick) = (&b[0], &b[1], &b[2]);
        if !id_ok(choice) {
            write_err(resp, 400, "choice_id is not a card id.");
            return;
        }
        let args = match outcome.as_str() {
            "keep" | "drop" => vec![s("rank"), s("record"), s("drop"), choice.clone(), s("--json")],
            "switch" => {
                if !id_ok(pick) {
                    write_err(resp, 400, "A switch names the attempt to pick.");
                    return;
                }
                vec![s("rank"), s("record"), s("pick"), choice.clone(), pick.clone(), s("--json")]
            }
            _ => {
                write_err(resp, 400, "outcome is keep, drop or switch.");
                return;
            }
        };
        let mut r = run_daisugi(Vec::new());
        let mut answered_in = String::new();
        for dir in self.data_sources() {
            r = run_daisugi(daisugi_args_in(&dir, args.clone()));
            answered_in = dir;
            if !(r.err.is_none() && r.code == 1) {
                break; // this dir holds the card, or failed for another reason
            }
        }
        if r.err.is_some() || r.code < 0 || r.code > 1 {
            let why = why_of(&r);
            crate::web::log("WARN", "web: a card answer failed", &[("why", &why)]);
            write_json(
                resp,
                502,
                &crate::gojson::map(vec![("error", json!(format!("daisugi rank record failed: {why}")))]),
            );
            return;
        }
        if r.code == 1 {
            write_err(resp, 404, r.stderr.trim());
            return;
        }
        let mut reply = reply_map(&r.stdout);
        // The card is closed, so its branch worktrees can go (W3K-7). They
        // are in the data dir that held the card.
        let p = run_daisugi(daisugi_args_in(&answered_in, vec![s("tree"), s("prune-branches"), s("--json")]));
        if p.err.is_none() && p.code == 0 {
            let pruned = match crate::godec::decode_any(p.stdout.as_bytes()) {
                Ok(v) => v,
                Err(_) => Any::Str(p.stdout.trim().to_string()),
            };
            set(&mut reply, "pruned", pruned);
        }
        write_any_map(resp, 200, reply);
    }

    pub(super) fn handle_tree_answer(&self, req: &Request, resp: &mut Response) {
        let Some((_name, deny_only)) = self.guard_answer(req, resp) else {
            return;
        };
        let body = &req.body[..req.body.len().min(16 << 10)];
        let Some(b) = decode_fields(body, &TREE_TY) else {
            write_err(resp, 400, "The request body is not JSON.");
            return;
        };
        if !self.operator_only(req, resp, &deny_only) {
            return;
        }
        if !id_ok(&b[0]) {
            write_err(resp, 400, "ask_id is not an ask id.");
            return;
        }
        if b[1] != "allow" && b[1] != "deny" {
            write_err(resp, 400, "decision is allow or deny.");
            return;
        }
        // The ask lives in the dir that holds it: try each, and stop at the one
        // that knows it (exit 1 is "no such ask").
        let mut r = run_daisugi(Vec::new());
        for dir in self.data_sources() {
            r = run_daisugi(daisugi_args_in(&dir, vec![s("tree"), s("answer"), b[0].clone(), b[1].clone()]));
            if !(r.err.is_none() && r.code == 1) {
                break;
            }
        }
        if r.err.is_some() || r.code < 0 || r.code > 1 {
            write_json(
                resp,
                502,
                &crate::gojson::map(vec![("error", json!(format!("daisugi tree answer failed: {}", why_of(&r))))]),
            );
            return;
        }
        if r.code == 1 {
            write_err(resp, 404, r.stderr.trim());
            return;
        }
        write_any_map(resp, 200, reply_map(&r.stdout));
    }
}

/// The command's JSON object, else {"output": its text}.
fn reply_map(stdout: &str) -> Vec<(String, Any)> {
    if stdout.trim() != "null" {
        if let Ok(Any::Map(m)) = crate::godec::decode_map(stdout.as_bytes()) {
            return m;
        }
    }
    vec![(s("output"), Any::Str(stdout.trim().to_string()))]
}

fn set(m: &mut Vec<(String, Any)>, k: &str, v: Any) {
    if let Some(slot) = m.iter_mut().find(|(key, _)| key == k) {
        slot.1 = v;
    } else {
        m.push((k.to_string(), v));
    }
}

fn write_any_map(resp: &mut Response, status: u16, m: Vec<(String, Any)>) {
    resp.set("Content-Type", "application/json");
    resp.status = status;
    resp.body = format!("{}\n", Any::Map(m).marshal()).into_bytes();
}

static CARD_TY: crate::godec::StructTy = crate::godec::StructTy {
    name: "",
    text: "struct",
    fields: &[
        ("choice_id", crate::godec::Ty::Str),
        ("outcome", crate::godec::Ty::Str),
        ("pick", crate::godec::Ty::Str),
    ],
};

static TREE_TY: crate::godec::StructTy = crate::godec::StructTy {
    name: "",
    text: "struct",
    fields: &[
        ("ask_id", crate::godec::Ty::Str),
        ("decision", crate::godec::Ty::Str),
        ("reason", crate::godec::Ty::Str),
    ],
};

/// Go's Decoder.Decode of the first value of body into a struct of string
/// fields. The values come back in the order of the fields. None for any
/// error.
fn decode_fields(body: &[u8], ty: &'static crate::godec::StructTy) -> Option<Vec<String>> {
    let first = super::first_value(body)?;
    let (g, err) = crate::godec::decode(first, &crate::godec::Ty::Struct(ty));
    if err.is_some() {
        return None;
    }
    Some(ty.fields.iter().map(|(n, _)| g.f(n).s()).collect())
}

/// Where an attempt's edits are: the worktree path, and the base commit and
/// repository that daisugi recorded for it when it made the worktree. The
/// floor never reads them from the worktree.
#[derive(Default, Clone)]
struct Place {
    path: String,
    base: String,
    repo: String,
}

/// From `rank show --json`, each attempt's worktree place of the card's
/// opened row, in the row's order. An attempt with another kind of place
/// maps to the empty place.
fn where_of(show_json: &str) -> Vec<(String, Place)> {
    let mut out: Vec<(String, Place)> = Vec::new();
    let Ok(doc) = crate::godec::decode_map(show_json.as_bytes()) else {
        return out;
    };
    let Some(Any::Arr(choices)) = doc.get("choices") else {
        return out;
    };
    for c in choices {
        if c.str_of("event") != "opened" {
            continue;
        }
        let Some(facts) = c.get("facts") else { continue };
        let Some(Any::Arr(order)) = facts.get("order") else { continue };
        for id in order {
            let Any::Str(id) = id else { continue };
            let mut place = Place::default();
            if let Some(w) = facts.get("where").and_then(|w| w.get(id)) {
                if w.str_of("kind") == "worktree" {
                    place = Place { path: w.str_of("path"), base: w.str_of("base"), repo: w.str_of("repo") };
                }
            }
            match out.iter_mut().find(|(k, _)| k == id) {
                Some(slot) => slot.1 = place,
                None => out.push((id.clone(), place)),
            }
        }
    }
    out
}

/// Whether path, with symlinks resolved, is inside one of roots, also
/// resolved. A root that is itself the path does not count.
fn under_any(path: &str, roots: &[PathBuf]) -> bool {
    if !Path::new(path).is_absolute() {
        return false;
    }
    let Ok(p) = std::fs::canonicalize(path) else {
        return false;
    };
    for root in roots {
        let Ok(rr) = std::fs::canonicalize(root) else {
            continue;
        };
        if let Ok(rel) = p.strip_prefix(&rr) {
            if !rel.as_os_str().is_empty() {
                return true;
            }
        }
    }
    false
}

fn git_out(dir: &str, args: &[&str]) -> Option<String> {
    // No file system monitor and no hooks directory: a config a branch child
    // wrote must not run a program of its own. No submodule check either: it
    // would run the nested repository's own config.
    let mut c = Command::new("git");
    c.args([
        "-c",
        "core.fsmonitor=false",
        "-c",
        "core.hooksPath=/dev/null",
        "-c",
        "diff.ignoreSubmodules=all",
        "-c",
        "submodule.recurse=false",
        "-C",
    ])
    .arg(dir)
    .args(args);
    let r = run_cmd(c);
    if r.err.is_some() || r.code != 0 {
        None
    } else {
        Some(r.stdout)
    }
}

/// True when the path is the linked worktree git made for it in the recorded
/// repository: not a symlink, its `.git` a regular file, its git directory
/// exactly `<repo>/worktrees/<name>`. The common directory alone also admits
/// the repository's main git directory and a sibling's.
fn owns_worktree(p: &Place) -> bool {
    if std::fs::symlink_metadata(&p.path).map(|m| m.file_type().is_symlink()).unwrap_or(true) {
        return false;
    }
    if !std::fs::symlink_metadata(format!("{}/.git", p.path)).map(|m| m.file_type().is_file()).unwrap_or(false) {
        return false;
    }
    let name = Path::new(&p.path).file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    // With extensions.worktreeConfig on, git reads a config.worktree the child
    // can write, and no -c guard switches that off.
    if std::fs::symlink_metadata(format!("{}/worktrees/{name}/config.worktree", p.repo)).is_ok() {
        return false;
    }
    // A fenced child's own object store that daisugi has not folded away yet:
    // the worktree's HEAD and index may name objects only that store holds,
    // and this reader never reads a store the child wrote.
    if std::fs::symlink_metadata(format!("{}/worktrees/{name}/objects", p.repo)).is_ok() {
        return false;
    }
    let same = |a: &str, b: &str| match (std::fs::canonicalize(a.trim()), std::fs::canonicalize(b)) {
        (Ok(x), Ok(y)) => x == y,
        _ => false,
    };
    let gd_ok = git_out(&p.path, &["rev-parse", "--path-format=absolute", "--git-dir"])
        .is_some_and(|g| same(&g, &format!("{}/worktrees/{name}", p.repo)));
    gd_ok
        && git_out(&p.path, &["rev-parse", "--path-format=absolute", "--git-common-dir"]).is_some_and(|c| same(&c, &p.repo))
}

/// `git diff` of a branch worktree against the base commit daisugi recorded
/// for it. A base that is not a full object id is never given to git, and
/// neither is a worktree whose repository is not the one recorded: the child
/// can rewrite the worktree's .git pointer.
fn worktree_diff(p: &Place) -> Result<String, String> {
    let is_id = (p.base.len() == 40 || p.base.len() == 64)
        && p.base.bytes().all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c));
    if !is_id || p.repo.is_empty() {
        return Err(s("no diff: daisugi recorded no base commit for this worktree"));
    }
    if !owns_worktree(p) {
        return Err(s("no diff: the worktree no longer belongs to its repository"));
    }
    let diff_args = ["diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--no-renames", &p.base, "--"];
    let Some(mut out) = git_out(&p.path, &diff_args) else {
        return Err(s("no diff: git could not read the worktree"));
    };
    if out.len() > MAX_DIFF_BYTES {
        let mut cut = MAX_DIFF_BYTES;
        while !out.is_char_boundary(cut) {
            cut -= 1;
        }
        out.truncate(cut);
        out.push_str("\n[cut at 512 KiB]\n");
    }
    Ok(out)
}

#[cfg(test)]
mod worktree_tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn g(dir: &str, args: &[&str]) -> String {
        let out = Command::new("git")
            .args(["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"])
            .args(args)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_SYSTEM", "/dev/null")
            .output()
            .expect("git runs");
        assert!(out.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&out.stderr));
        String::from_utf8_lossy(&out.stdout).into_owned()
    }

    fn fixture(tag: &str) -> (String, Place) {
        let root = std::env::temp_dir().join(format!("cards-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        let root = root.display().to_string();
        let repo = format!("{root}/repo");
        std::fs::create_dir_all(&repo).unwrap();
        g(&repo, &["init", "-q", "-b", "main"]);
        std::fs::write(format!("{repo}/a.txt"), "one\n").unwrap();
        g(&repo, &["add", "-A"]);
        g(&repo, &["commit", "-qm", "x"]);
        let wt = format!("{root}/wt");
        g(&repo, &["worktree", "add", "-q", "--detach", &wt]);
        std::fs::write(format!("{wt}/a.txt"), "two\n").unwrap();
        let base = g(&repo, &["rev-parse", "HEAD"]).trim().to_string();
        let place = Place { path: wt, base, repo: format!("{repo}/.git") };
        (root, place)
    }

    fn script(root: &str, marker: &str) -> String {
        let sc = format!("{root}/clean.sh");
        std::fs::write(&sc, format!("#!/bin/sh\necho x >> {marker}\ncat\n")).unwrap();
        std::fs::set_permissions(&sc, std::fs::Permissions::from_mode(0o755)).unwrap();
        sc
    }

    fn touch(f: &str, text: &str, sec: u64) {
        std::fs::write(f, text).unwrap();
        let t = std::time::UNIX_EPOCH + std::time::Duration::from_secs(sec);
        std::fs::File::options().write(true).open(f).unwrap().set_modified(t).unwrap();
    }

    #[test]
    fn a_nested_repository_the_child_made_runs_no_program_under_the_floors_diff() {
        let (root, p) = fixture("nested");
        let marker = format!("{root}/ran-nested");
        let sc = script(&root, &marker);
        let sub = format!("{}/sub", p.path);
        std::fs::create_dir_all(&sub).unwrap();
        g(&sub, &["init", "-q"]);
        std::fs::write(format!("{sub}/s.txt"), "aaaa\n").unwrap();
        std::fs::write(format!("{sub}/.gitattributes"), "* filter=x\n").unwrap();
        g(&sub, &["add", "-A"]);
        g(&sub, &["commit", "-qm", "n"]);
        g(&sub, &["config", "filter.x.clean", &sc]);
        g(&p.path, &["add", "sub"]);
        touch(&format!("{sub}/s.txt"), "bbbb\n", 1);
        g(&p.path, &["diff", "--name-only", &p.base]);
        assert!(Path::new(&marker).exists(), "the control: an unguarded diff runs the nested filter");
        let _ = std::fs::remove_file(&marker);
        touch(&format!("{sub}/s.txt"), "cccc\n", 7);
        assert!(worktree_diff(&p).unwrap().contains("+two"));
        assert!(!Path::new(&marker).exists(), "the nested filter ran under the floor's diff");
    }

    #[test]
    fn a_worktree_with_a_config_worktree_is_refused() {
        let (root, p) = fixture("wtconfig");
        g(&format!("{}/..", p.repo), &["config", "extensions.worktreeConfig", "true"]);
        let marker = format!("{root}/ran-wtconfig");
        let sc = script(&root, &marker);
        let gd = g(&p.path, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        std::fs::write(format!("{gd}/config.worktree"), format!("[filter \"x\"]\n\tclean = {sc}\n")).unwrap();
        std::fs::write(format!("{}/.gitattributes", p.path), "* filter=x\n").unwrap();
        touch(&format!("{}/a.txt", p.path), "two\n", 3);
        g(&p.path, &["diff", "HEAD"]);
        assert!(Path::new(&marker).exists(), "the control: git runs the child's config.worktree");
        let _ = std::fs::remove_file(&marker);
        let err = worktree_diff(&p).unwrap_err();
        assert!(err.contains("no longer belongs to its repository"), "{err}");
        assert!(!Path::new(&marker).exists());
    }

    #[test]
    fn a_child_that_committed_into_its_own_store_still_has_a_diff() {
        let (_, p) = fixture("ownstore");
        let gd = g(&p.path, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        let own = format!("{gd}/objects");
        std::fs::create_dir_all(&own).unwrap();
        for args in [vec!["add", "-A"], vec!["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"]] {
            let o = Command::new("git")
                .args(["-C", &p.path])
                .args(&args)
                .env("GIT_OBJECT_DIRECTORY", &own)
                .env("GIT_ALTERNATE_OBJECT_DIRECTORIES", format!("{}/objects", p.repo))
                .output()
                .unwrap();
            assert!(o.status.success());
        }
        std::fs::write(format!("{}/a.txt", p.path), "three\n").unwrap();
        // Until daisugi folds the child's store away, the floor reads nothing.
        assert!(worktree_diff(&p).unwrap_err().contains("no longer belongs to its repository"));
        // The fold daisugi runs once the child is gone (branch_git.fold).
        let name = Path::new(&gd).file_name().unwrap().to_string_lossy().to_string();
        let git_dir = format!("--git-dir={}", p.repo);
        let head = format!("worktrees/{name}/HEAD");
        for args in [
            vec![git_dir.as_str(), "-c", "core.logAllRefUpdates=false", "update-ref", "--no-deref", head.as_str(), p.base.as_str()],
            vec![git_dir.as_str(), "read-tree", p.base.as_str()],
        ] {
            let o = Command::new("git").args(&args).env("GIT_INDEX_FILE", format!("{gd}/index")).output().unwrap();
            assert!(o.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&o.stderr));
        }
        let _ = std::fs::remove_dir_all(format!("{gd}/logs"));
        std::fs::remove_dir_all(&own).unwrap();
        assert!(worktree_diff(&p).unwrap().contains("+three"));
    }
}
