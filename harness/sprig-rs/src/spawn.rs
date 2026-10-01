//! The spawn tool: start a child agent through the tree command (daisugi
//! tree run), wait for it and return its answer, as the Go module's
//! spawn.go does. The parent is always sprig's own --session; the
//! resolved agent profiles go on a fresh pipe as the command's
//! descriptor 3; a child's branch use counts toward --max-tokens.

use crate::gate::Tool;
use crate::json::{jmap, Any, Map, J};
use std::cell::Cell;
use std::ffi::OsStr;
use std::io::{Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::rc::Rc;

pub struct SpawnTool {
    /// The tree command, split at whitespace.
    pub command: Vec<Vec<u8>>,
    /// sprig's own session: the parent of every child.
    pub session: Vec<u8>,
    /// The resolved profiles with keys; None sends none.
    pub keys: Option<Vec<u8>>,
    /// sprig's own tokens and model turns so far, without its children's.
    pub own_use: Rc<Cell<(i64, i64)>>,
    /// The child's tools when the input names none: the parent's wall in
    /// host names.
    pub default_tools: Vec<String>,
    /// The tokens the children used, their branches included.
    pub branch: Rc<Cell<i64>>,
}

/// sprig's tool name in the host's words.
pub fn host_tool_name(name: &str) -> Option<&'static str> {
    match name {
        "read" => Some("Read"),
        "write" => Some("Write"),
        "edit" => Some("Edit"),
        "bash" => Some("Bash"),
        "spawn" => Some("Spawn"),
        _ => None,
    }
}

/// A wall in host names.
pub fn host_tools(wall: &[String]) -> Vec<String> {
    wall.iter()
        .filter_map(|n| host_tool_name(n).map(str::to_string))
        .collect()
}

const MAX_COUNT: f64 = 9007199254740992.0;

/// A whole number from 0 to 2**53 in a tool input.
fn count(input: &Map, key: &str) -> Result<Option<i64>, Vec<u8>> {
    match input.get(key) {
        None | Some(Any::Null) => Ok(None),
        Some(Any::Num(f)) if *f == f.trunc() && *f >= 0.0 && *f <= MAX_COUNT => Ok(Some(*f as i64)),
        _ => Err(format!("spawn: {key} must be a whole number from 0 to 2**53").into_bytes()),
    }
}

/// An optional text field of a tool input.
fn text(input: &Map, key: &str) -> Result<String, Vec<u8>> {
    match input.get(key) {
        None | Some(Any::Null) => Ok(String::new()),
        Some(Any::Str(s)) => Ok(s.clone()),
        _ => Err(format!("spawn: {key} must be text").into_bytes()),
    }
}

/// What tree run prints, read field by field from a plain decode.
#[derive(Default)]
struct Reply {
    status: String,
    answer: String,
    tokens_used: Option<i64>,
    reasons: Vec<String>,
    ask_id: String,
}

fn read_reply(data: &[u8]) -> Option<Reply> {
    let root = crate::json::parse(data).ok()?;
    let mut d = crate::json::Dec::default();
    let any = d.any(&root);
    if d.err.is_some() {
        return None;
    }
    let Any::Map(o) = any else { return None };
    let s = |k: &str| match o.get(k) {
        Some(Any::Str(s)) => s.clone(),
        _ => String::new(),
    };
    let mut r = Reply {
        status: s("status"),
        answer: s("answer"),
        ask_id: s("ask_id"),
        ..Default::default()
    };
    if let Some(Any::Num(f)) = o.get("tokens_used") {
        if *f == f.trunc() && *f >= 0.0 && *f <= MAX_COUNT {
            r.tokens_used = Some(*f as i64);
        }
    }
    if let Some(Any::Arr(xs)) = o.get("reasons") {
        for x in xs {
            if let Any::Str(s) = x {
                r.reasons.push(s.clone());
            }
        }
    }
    if r.status.is_empty() {
        return None;
    }
    Some(r)
}

extern "C" {
    fn fcntl(fd: i32, cmd: i32, ...) -> i32;
    fn dup2(old: i32, new: i32) -> i32;
}

// F_DUPFD_CLOEXEC is 1030 on Linux.
const F_DUPFD_CLOEXEC: i32 = 1030;

/// Runs the tree command: stdout, stderr and Go's error text.
fn run_tree(
    argv: &[Vec<u8>],
    stdin: Vec<u8>,
    keys: Option<&[u8]>,
) -> (Vec<u8>, Vec<u8>, Option<String>) {
    let name = &argv[0];
    let path = if name.contains(&b'/') {
        name.clone()
    } else {
        match crate::osx::look_path(name) {
            Ok(p) => p,
            Err(e) => return (Vec::new(), Vec::new(), Some(e.text)),
        }
    };
    let mut cmd = Command::new(OsStr::from_bytes(&path));
    cmd.arg0(OsStr::from_bytes(name));
    for a in &argv[1..] {
        cmd.arg(OsStr::from_bytes(a));
    }
    cmd.env_clear();
    for (k, v) in crate::env::child_env(None) {
        cmd.env(OsStr::from_bytes(&k), OsStr::from_bytes(&v));
    }
    cmd.stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // The tree command dies with sprig: it stays in sprig's group, and its
    // parent-death signal is SIGTERM.
    unsafe {
        cmd.pre_exec(|| {
            crate::procgroup::prctl(
                crate::procgroup::PR_SET_PDEATHSIG,
                crate::procgroup::SIGTERM as u64,
                0,
                0,
                0,
            );
            Ok(())
        });
    }
    let mut pipe = None;
    if let Some(k) = keys {
        let Ok((r, w)) = std::io::pipe() else {
            return (Vec::new(), Vec::new(), Some("cannot make a pipe".into()));
        };
        use std::os::fd::AsRawFd;
        // Above 10, close on exec: the child gets it only as descriptor 3.
        let high = unsafe { fcntl(r.as_raw_fd(), F_DUPFD_CLOEXEC, 10) };
        drop(r);
        if high < 0 {
            return (Vec::new(), Vec::new(), Some("cannot make a pipe".into()));
        }
        unsafe {
            cmd.pre_exec(move || {
                if dup2(high, 3) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        pipe = Some((high, w, k.to_vec()));
    }
    let child = cmd.spawn();
    let close_high = |p: &Option<(i32, std::io::PipeWriter, Vec<u8>)>| {
        if let Some((high, _, _)) = p {
            unsafe {
                libc_close(*high);
            }
        }
    };
    let mut child = match child {
        Ok(c) => c,
        Err(e) => {
            close_high(&pipe);
            return (
                Vec::new(),
                Vec::new(),
                Some(
                    String::from_utf8_lossy(&crate::goerr::path_error("fork/exec", &path, &e))
                        .into_owned(),
                ),
            );
        }
    };
    close_high(&pipe);
    let writer = pipe.map(|(_, mut w, data)| {
        std::thread::spawn(move || {
            let _ = w.write_all(&data);
        })
    });
    let feeder = child.stdin.take().map(|mut s| {
        std::thread::spawn(move || {
            let _ = s.write_all(&stdin);
        })
    });
    let so = child.stdout.take();
    let se = child.stderr.take();
    let pid = child.id() as i32;
    let err_t = std::thread::spawn(move || read_capped(se, pid));
    let (out, over_out) = read_capped(so, pid);
    let (errb, over_err) = err_t.join().unwrap_or_default();
    if let Some(f) = feeder {
        let _ = f.join();
    }
    let status = child.wait();
    if let Some(w) = writer {
        let _ = w.join();
    }
    if over_out || over_err {
        return (Vec::new(), Vec::new(), Some(TOO_MUCH.into()));
    }
    let err = match status {
        Ok(st) if st.success() => None,
        Ok(st) => Some(crate::goerr::exit_text(st)),
        Err(_) => Some("wait failed".into()),
    };
    (out, errb, err)
}

/// The most the tree command may write on stdout, and on stderr.
const MAX_TREE_OUTPUT: usize = 16 << 20;
/// What run_tree reports for output past the cap.
const TOO_MUCH: &str = "\x00too much";

/// Reads a pipe to its end, at most MAX_TREE_OUTPUT bytes; past that it
/// kills the tree command and says so.
fn read_capped<R: Read>(r: Option<R>, pid: i32) -> (Vec<u8>, bool) {
    let mut b = Vec::new();
    let Some(mut r) = r else {
        return (b, false);
    };
    let mut chunk = [0u8; 65536];
    loop {
        match r.read(&mut chunk) {
            Ok(0) => return (b, false),
            Ok(n) if b.len() + n > MAX_TREE_OUTPUT => {
                crate::procgroup::kill_pid(pid);
                return (b, true);
            }
            Ok(n) => b.extend_from_slice(&chunk[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(_) => return (b, false),
        }
    }
}

extern "C" {
    #[link_name = "close"]
    fn libc_close(fd: i32) -> i32;
}

impl Tool for SpawnTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let (mut out, mut err) = match self.spawn(input.as_ref().cloned().unwrap_or_default()) {
            Ok(out) => (out, None),
            Err(e) => (Vec::new(), Some(e)),
        };
        // Every key the bundle holds reads [key] in the result: a child's
        // reasons or the tree command's stderr may echo one.
        for k in bundle_keys(self.keys.as_deref().unwrap_or_default()) {
            out = crate::env::redact(&out, &k);
            err = err.map(|e| crate::env::redact(&e, &k));
        }
        (out, err)
    }
}

/// Every non-empty key of a profile bundle, in the order of the profile
/// names (Go's bundleKeys).
fn bundle_keys(bundle: &[u8]) -> Vec<Vec<u8>> {
    let mut doc = crate::json::Map::new();
    let parsed = crate::json::unmarshal(bundle, |d, n| {
        if let crate::json::Any::Map(m) = d.any(n) {
            doc = m;
        }
    });
    if parsed.is_err() {
        return vec![];
    }
    let mut out = vec![];
    for entry in doc.values() {
        if let crate::json::Any::Map(e) = entry {
            if let Some(crate::json::Any::Str(k)) = e.get("key") {
                if !k.is_empty() {
                    out.push(k.clone().into_bytes());
                }
            }
        }
    }
    out
}

impl SpawnTool {
    fn spawn(&self, input: Map) -> Result<Vec<u8>, Vec<u8>> {
        let task = text(&input, "task")?;
        if crate::gostr::trim_space(task.as_bytes()).is_empty() {
            return Err(b"spawn: task must be text".to_vec());
        }
        let Some(Any::Map(env)) = input.get("envelope") else {
            return Err(b"spawn: envelope must be an object".to_vec());
        };
        let mut profile = text(&input, "profile")?;
        if profile.is_empty() {
            profile = "default".into();
        }
        let model = text(&input, "model")?;
        let mut tools = self.default_tools.clone();
        match input.get("tools") {
            None | Some(Any::Null) => {}
            Some(Any::Arr(xs)) => {
                tools = Vec::new();
                for x in xs {
                    let Any::Str(name) = x else {
                        return Err(b"spawn: tools must be a list of tool names".to_vec());
                    };
                    tools.push(host_tool_name(name).map_or(name.clone(), str::to_string));
                }
            }
            Some(_) => return Err(b"spawn: tools must be a list of tool names".to_vec()),
        }
        let tokens = count(&input, "tokens")?;
        let turns = count(&input, "turns")?;
        let max_turns = count(&input, "max_turns")?;

        let mut argv = self.command.clone();
        let mut push = |a: &[u8]| argv.push(a.to_vec());
        push(b"--parent");
        push(&self.session);
        push(b"--profile");
        push(profile.as_bytes());
        if !model.is_empty() {
            push(b"--model");
            push(model.as_bytes());
        }
        if let Some(n) = tokens {
            push(b"--tokens");
            push(n.to_string().as_bytes());
        }
        if let Some(n) = turns {
            push(b"--turns");
            push(n.to_string().as_bytes());
        }
        let (own_tokens, own_turns) = self.own_use.get();
        push(b"--parent-tokens-used");
        push(own_tokens.to_string().as_bytes());
        push(b"--parent-turns-used");
        push(own_turns.to_string().as_bytes());
        if self.keys.is_some() {
            push(b"--keys-fd");
            push(b"3");
        }
        push(b"-");

        let mut req = vec![
            ("task", J::s(&task)),
            ("envelope", J::from(&Any::Map(env.clone()))),
            ("tools", J::Arr(tools.iter().map(J::s).collect())),
        ];
        if let Some(n) = max_turns {
            req.push(("max_turns", J::Int(n)));
        }
        let stdin = crate::json::encode(&jmap(req));
        let (out, errb, run_err) = run_tree(&argv, stdin, self.keys.as_deref());
        if run_err.as_deref() == Some(TOO_MUCH) {
            return Err(b"spawn: the tree command wrote more than 16 MiB".to_vec());
        }
        let Some(reply) = read_reply(crate::gostr::trim_space(&out)) else {
            let mut reason = crate::gate::gate_reason(&errb, b"");
            if reason.is_empty() {
                if let Some(e) = run_err {
                    reason = e.into_bytes();
                }
            }
            return Err([b"spawn: the tree command failed: ".as_slice(), &reason].concat());
        };
        match reply.tokens_used {
            Some(n) => self.branch.set(crate::agent::sat_add(self.branch.get(), n)),
            None if reply.status == "ran" || reply.status == "failed" => {
                // A child with no report is charged its whole reservation.
                if let Some(n) = tokens {
                    self.branch.set(crate::agent::sat_add(self.branch.get(), n));
                }
            }
            None => {}
        }
        Ok(match reply.status.as_str() {
            "ran" => reply.answer.into_bytes(),
            "asked" => format!("sent to the operator as ask {}", reply.ask_id).into_bytes(),
            "refused" => format!("refused: {}", reply.reasons.join("; ")).into_bytes(),
            _ => format!("failed: {}", reply.reasons.join("; ")).into_bytes(),
        })
    }
}
