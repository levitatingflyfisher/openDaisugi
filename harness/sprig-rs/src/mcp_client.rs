//! The MCP client (the Go module's mcp_client.go): sprig starts the stdio
//! servers of --mcp-config, offers their tools to the model as
//! mcp__<server>__<tool>, and relays each call. A server is a process in a
//! group of its own. The group dies when the run ends, on a signal, and
//! with sprig.

use crate::gate::Tool;
use crate::gostr::trim_space;
use crate::json::{self, jmap, jmap_any, Dec, GoSlice, Map, Node, J, V};
use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::ffi::OsStr;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::process::CommandExt;
use std::process::{ChildStdin, Command, Stdio};
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::{Duration, Instant};

/// The most one server message may hold.
pub const MAX_MESSAGE: usize = 32 << 20;

/// How long one request waits for its answer, in milliseconds.
static CALL_TIMEOUT_MS: AtomicU64 = AtomicU64::new(30_000);

/// Sets the wait for an answer (tests only).
pub fn set_call_timeout(d: Duration) {
    CALL_TIMEOUT_MS.store(d.as_millis() as u64, Ordering::SeqCst);
}

fn call_timeout() -> Duration {
    Duration::from_millis(CALL_TIMEOUT_MS.load(Ordering::SeqCst))
}

/// One stdio server of a config.
#[derive(Debug, Clone, PartialEq)]
pub struct McpServerSpec {
    pub name: String,
    pub command: Vec<u8>,
    pub args: Vec<Vec<u8>>,
    /// Sorted by name; the key variables are left out.
    pub env: Vec<(Vec<u8>, Vec<u8>)>,
}

/// One tool as the model sees it.
#[derive(Debug, Clone, PartialEq)]
pub struct McpToolDef {
    /// mcp__<server>__<tool>
    pub name: String,
    pub description: String,
    pub schema: J,
}

const WANT: &str = "--mcp-config: want {\"mcpServers\": {name: {command, args, env}}}";

fn opt_str(d: &mut Dec, n: &Node, out: &mut Option<String>) {
    match &n.v {
        V::Null => *out = None,
        V::Str(s) => *out = Some(s.clone()),
        _ => d.type_err("value", "string"),
    }
}

/// Reads a --mcp-config value: inline JSON when it starts with "{", else
/// the path of a file. It returns the servers by name, and the JSON text
/// itself. Only stdio servers are taken.
pub fn parse_mcp_config(value: &[u8]) -> Result<(Vec<McpServerSpec>, Vec<u8>), Vec<u8>> {
    let mut text = trim_space(value).to_vec();
    if text.first() != Some(&b'{') {
        let b = crate::osx::read_file(value).map_err(|e| {
            [
                b"--mcp-config: cannot read ".as_slice(),
                value,
                b": ",
                &e,
            ]
            .concat()
        })?;
        text = trim_space(&b).to_vec();
    }
    let mut servers: Option<BTreeMap<String, Node>> = None;
    let r = json::unmarshal(&text, |d, n| {
        d.object(n, "", "", &["mcpServers"], |d, _, v| match &v.v {
            V::Null => servers = None,
            V::Obj(kv) => {
                let m = servers.get_or_insert_with(BTreeMap::new);
                for (k, x) in kv {
                    m.insert(k.clone(), x.clone());
                }
            }
            _ => d.type_err("value", "map[string]json.RawMessage"),
        })
    });
    let servers = match (r, servers) {
        (Ok(()), Some(s)) => s,
        _ => return Err(WANT.as_bytes().to_vec()),
    };
    let mut specs = Vec::new();
    for (name, node) in &servers {
        let quoted = crate::goerr::quote(name.as_bytes());
        let mut d = Dec::default();
        let (mut ty, mut cmd): (Option<String>, Option<String>) = (None, None);
        let mut args: GoSlice<String> = Default::default();
        let mut env: Option<BTreeMap<String, String>> = None;
        d.object(
            node,
            "",
            "",
            &["type", "command", "args", "env"],
            |d, i, v| match i {
                0 => opt_str(d, v, &mut ty),
                1 => opt_str(d, v, &mut cmd),
                2 => d.slice(v, &mut args, "[]string", |d, v, s| d.string(v, s)),
                _ => match &v.v {
                    V::Null => env = None,
                    V::Obj(kv) => {
                        let m = env.get_or_insert_with(BTreeMap::new);
                        for (k, x) in kv {
                            let mut s = String::new();
                            d.string(x, &mut s);
                            m.insert(k.clone(), s);
                        }
                    }
                    _ => d.type_err("value", "map[string]string"),
                },
            },
        );
        if d.err.is_some() {
            return Err(
                format!("--mcp-config: server {quoted}: want command, args and env").into_bytes(),
            );
        }
        let stdio = ty.as_deref().is_none_or(|t| t == "stdio");
        let Some(cmd) = cmd.filter(|_| stdio) else {
            return Err(format!(
                "--mcp-config: server {quoted} is not a stdio server; only command servers run"
            )
            .into_bytes());
        };
        if cmd.is_empty() {
            return Err(format!("--mcp-config: server {quoted} has an empty command").into_bytes());
        }
        let env = env
            .unwrap_or_default()
            .into_iter()
            .filter(|(k, _)| !crate::env::scrubbed(k.as_bytes()))
            .map(|(k, v)| (k.into_bytes(), v.into_bytes()))
            .collect();
        specs.push(McpServerSpec {
            name: name.clone(),
            command: cmd.into_bytes(),
            args: args
                .items()
                .iter()
                .map(|a| a.as_bytes().to_vec())
                .collect(),
            env,
        });
    }
    Ok((specs, text))
}

enum Line {
    Data(Vec<u8>),
    Eof,
    TooBig,
}

/// Reads one newline-ended message, at most MAX_MESSAGE bytes (the newline
/// counts).
fn read_line(br: &mut impl BufRead) -> Line {
    let mut buf: Vec<u8> = Vec::new();
    loop {
        let chunk = match br.fill_buf() {
            Ok(c) if !c.is_empty() => c,
            _ => return Line::Eof,
        };
        let (take, done) = match chunk.iter().position(|&c| c == b'\n') {
            Some(i) => (i + 1, true),
            None => (chunk.len(), false),
        };
        buf.extend_from_slice(&chunk[..take]);
        br.consume(take);
        if buf.len() > MAX_MESSAGE {
            return Line::TooBig;
        }
        if done {
            return Line::Data(trim_space(&buf).to_vec());
        }
    }
}

/// One running server.
pub struct McpServer {
    stdin: Arc<Mutex<Option<ChildStdin>>>,
    rx: mpsc::Receiver<Line>,
    pgid: i32,
    /// Set when the group had no member left and was forgotten.
    gone: Arc<AtomicBool>,
    /// Why the server is gone; empty while it runs.
    dead: RefCell<String>,
    next: Cell<i64>,
}

impl McpServer {
    fn kill(&self, why: &str) {
        if self.dead.borrow().is_empty() {
            *self.dead.borrow_mut() = why.to_string();
        }
        self.close();
    }

    fn close(&self) {
        // The group goes first, unless it was forgotten (its id may name
        // another process by then). A writer blocked on a full pipe holds
        // the lock; if the lock is busy the stuck thread keeps the pipe,
        // which closes when the server is gone.
        if !self.gone.load(Ordering::SeqCst) {
            crate::procgroup::kill_group(self.pgid, crate::procgroup::SIGKILL);
        }
        if let Ok(mut g) = self.stdin.try_lock() {
            g.take();
        }
    }

    /// Writes one line, waiting no longer than the call timeout for the
    /// server to take it.
    fn send(&self, msg: &J) -> Result<(), ()> {
        let mut line = json::encode(msg);
        line.push(b'\n');
        let (tx, rx) = mpsc::channel();
        let stdin = self.stdin.clone();
        std::thread::spawn(move || {
            let ok = match stdin.lock() {
                Ok(mut g) => match g.as_mut() {
                    Some(w) => w.write_all(&line).and_then(|_| w.flush()).is_ok(),
                    None => false,
                },
                Err(_) => false,
            };
            let _ = tx.send(ok);
        });
        match rx.recv_timeout(call_timeout()) {
            Ok(true) => Ok(()),
            _ => Err(()),
        }
    }

    /// Sends one request and waits for its answer. A server that ends,
    /// breaks the format, or stays silent past the call timeout is dead
    /// from then on. The result is the raw JSON of the answer's result.
    fn request(&self, method: &str, params: J) -> Result<Vec<u8>, String> {
        if !self.dead.borrow().is_empty() {
            return Err(self.dead.borrow().clone());
        }
        let id = self.next.get() + 1;
        self.next.set(id);
        let msg = jmap(vec![
            ("jsonrpc", J::s("2.0")),
            ("id", J::Int(id)),
            ("method", J::s(method)),
            ("params", params),
        ]);
        if self.send(&msg).is_err() {
            self.kill("the server is not running");
            return Err("the server is not running".into());
        }
        let deadline = Instant::now() + call_timeout();
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            let line = match self.rx.recv_timeout(left) {
                Ok(Line::Data(l)) => l,
                Ok(Line::TooBig) => {
                    let why = "the server broke the protocol: a message is larger than 32 MiB";
                    self.kill(why);
                    return Err(why.into());
                }
                Ok(Line::Eof) | Err(mpsc::RecvTimeoutError::Disconnected) => {
                    self.kill("the server ended");
                    return Err("the server ended".into());
                }
                Err(mpsc::RecvTimeoutError::Timeout) => {
                    let why = "the server gave no answer in 30 s";
                    self.kill(why);
                    return Err(why.into());
                }
            };
            let Ok(n) = json::parse(&line) else { continue };
            let mut d = Dec::default();
            let mut id_raw: Option<Vec<u8>> = None;
            let mut m = String::new();
            let mut result: Option<Vec<u8>> = None;
            let mut err_msg: Option<String> = None;
            d.object(&n, "", "", &["id", "method", "result", "error"], |d, i, v| match i {
                0 => id_raw = Some(line[v.start..v.end].to_vec()),
                1 => d.string(v, &mut m),
                2 => result = Some(line[v.start..v.end].to_vec()),
                _ => match &v.v {
                    V::Null => err_msg = None,
                    V::Obj(_) => {
                        let mut text = String::new();
                        d.object(v, "", "", &["code", "message"], |d, i, x| {
                            if i == 0 {
                                let mut c = 0;
                                d.int(x, &mut c);
                            } else {
                                d.string(x, &mut text);
                            }
                        });
                        err_msg = Some(text);
                    }
                    _ => d.type_err("value", "struct"),
                },
            });
            let Some(id_raw) = id_raw.filter(|_| d.err.is_none()) else {
                continue; // a notification, or noise
            };
            if !m.is_empty() {
                // A request from the server: sprig answers none.
                let reply = jmap(vec![
                    ("jsonrpc", J::s("2.0")),
                    ("id", J::Raw(id_raw)),
                    (
                        "error",
                        jmap(vec![
                            ("code", J::Int(-32601)),
                            ("message", J::s("method not found")),
                        ]),
                    ),
                ]);
                // The reply goes through the timed send, so a server that
                // reads nothing cannot hold the run past the call timeout.
                if self.send(&reply).is_err() {
                    self.kill("the server is not running");
                    return Err("the server is not running".into());
                }
                continue;
            }
            if id_raw != id.to_string().into_bytes() {
                continue;
            }
            if let Some(e) = err_msg {
                return Err(e);
            }
            return Ok(result.unwrap_or_default());
        }
    }
}

/// The servers of a run and the tools they offer.
#[derive(Default)]
pub struct McpSet {
    servers: Vec<Rc<McpServer>>,
    pub defs: Vec<McpToolDef>,
    tools: Vec<(String, Rc<McpServer>, String)>,
}

impl Drop for McpSet {
    fn drop(&mut self) {
        self.close();
    }
}

fn name_usable(full: &str) -> bool {
    (1..=64).contains(&full.len())
        && full
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}

fn node_to_j(n: &Node) -> J {
    match &n.v {
        V::Null => J::Null,
        V::Bool(b) => J::Bool(*b),
        V::Num(t) => J::Raw(t.clone().into_bytes()),
        V::Str(s) => J::s(s),
        V::Arr(xs) => J::Arr(xs.iter().map(node_to_j).collect()),
        V::Obj(kv) => {
            let mut m: BTreeMap<&str, J> = BTreeMap::new();
            for (k, x) in kv {
                m.insert(k, node_to_j(x));
            }
            J::Obj(m.into_iter().map(|(k, v)| (k.to_string(), v)).collect())
        }
    }
}

#[derive(Default)]
struct RawTool {
    name: String,
    description: String,
    schema: Option<J>,
}

impl McpSet {
    /// Starts every server, runs the handshake and lists the tools. A
    /// server that fails to start fails the run: a turn must not go on
    /// with a tool set the operator did not ask for. Notes go to `note`.
    pub fn start(specs: &[McpServerSpec], note: &mut dyn Write) -> Result<McpSet, String> {
        let mut set = McpSet::default();
        for spec in specs {
            set.add(spec, note)
                .map_err(|e| format!("mcp server {}: {e}", spec.name))?;
        }
        Ok(set)
    }

    fn add(&mut self, spec: &McpServerSpec, note: &mut dyn Write) -> Result<(), String> {
        let s = start_server(spec)?;
        self.servers.push(s.clone());
        s.request(
            "initialize",
            jmap(vec![
                ("protocolVersion", J::s("2024-11-05")),
                ("capabilities", jmap(vec![])),
                (
                    "clientInfo",
                    jmap(vec![("name", J::s("sprig")), ("version", J::s("0"))]),
                ),
            ]),
        )?;
        let _ = s.send(&jmap(vec![
            ("jsonrpc", J::s("2.0")),
            ("method", J::s("notifications/initialized")),
        ]));
        let mut cursor = String::new();
        for _page in 0..10 {
            let mut params = vec![];
            if !cursor.is_empty() {
                params.push(("cursor", J::s(&cursor)));
            }
            let raw = s.request("tools/list", jmap(params))?;
            let (tools, next) = parse_tool_list(&raw)
                .ok_or_else(|| "tools/list gave an unreadable answer".to_string())?;
            for t in tools {
                let full = format!("mcp__{}__{}", spec.name, t.name);
                if t.name.is_empty() {
                    let _ = writeln!(
                        note,
                        "sprig: mcp server {}: a tool with no name skipped",
                        spec.name
                    );
                    continue;
                }
                if self.defs.iter().any(|d| d.name == full) {
                    let _ = writeln!(
                        note,
                        "sprig: mcp tool {full} skipped: the name is already taken"
                    );
                    continue;
                }
                if !name_usable(&full) {
                    let _ = writeln!(note, "sprig: mcp tool {full} skipped: the name is not usable");
                    continue;
                }
                let schema = t
                    .schema
                    .unwrap_or_else(|| jmap(vec![("type", J::s("object"))]));
                self.defs.push(McpToolDef {
                    name: full.clone(),
                    description: t.description,
                    schema,
                });
                self.tools.push((full, s.clone(), t.name));
            }
            if next.is_empty() {
                break;
            }
            cursor = next;
        }
        Ok(())
    }

    /// The executor tools, keyed by their full names.
    pub fn tools(&self) -> Vec<(String, Box<dyn Tool>)> {
        self.tools
            .iter()
            .map(|(full, server, remote)| {
                (
                    full.clone(),
                    Box::new(McpTool {
                        server: server.clone(),
                        remote: remote.clone(),
                    }) as Box<dyn Tool>,
                )
            })
            .collect()
    }

    /// Ends the servers: stdin closed, each group killed.
    pub fn close(&self) {
        for s in &self.servers {
            s.close();
        }
    }
}

fn parse_tool_list(raw: &[u8]) -> Option<(Vec<RawTool>, String)> {
    let mut tools: GoSlice<RawTool> = Default::default();
    let mut next = String::new();
    json::unmarshal(raw, |d, n| {
        d.object(n, "", "", &["tools", "nextCursor"], |d, i, v| {
            if i == 0 {
                d.slice(v, &mut tools, "[]struct", |d, v, t| {
                    d.object(
                        v,
                        "",
                        "",
                        &["name", "description", "inputSchema"],
                        |d, i, x| match i {
                            0 => d.string(x, &mut t.name),
                            1 => d.string(x, &mut t.description),
                            _ => match &x.v {
                                V::Null => t.schema = None,
                                V::Obj(_) => t.schema = Some(node_to_j(x)),
                                _ => d.type_err("value", "map[string]interface {}"),
                            },
                        },
                    )
                });
            } else {
                d.string(v, &mut next);
            }
        })
    })
    .ok()?;
    let items = tools
        .items()
        .iter()
        .map(|t| RawTool {
            name: t.name.clone(),
            description: t.description.clone(),
            schema: t.schema.clone(),
        })
        .collect();
    Some((items, next))
}

/// Starts the process in a group of its own, with SIGKILL as its
/// parent-death signal.
fn start_server(spec: &McpServerSpec) -> Result<Rc<McpServer>, String> {
    let name = &spec.command;
    let q = crate::goerr::quote(name);
    let path = if name.contains(&b'/') {
        name.clone()
    } else {
        match crate::osx::look_path(name) {
            Ok(p) => p,
            Err(e) if e.not_found => return Err(format!("the command {q} was not found")),
            Err(_) => return Err(format!("the command {q} cannot start")),
        }
    };
    let mut cmd = Command::new(OsStr::from_bytes(&path));
    cmd.arg0(OsStr::from_bytes(name));
    for a in &spec.args {
        cmd.arg(OsStr::from_bytes(a));
    }
    cmd.env_clear();
    for (k, v) in crate::env::child_env(None) {
        cmd.env(OsStr::from_bytes(&k), OsStr::from_bytes(&v));
    }
    for (k, v) in &spec.env {
        cmd.env(OsStr::from_bytes(k), OsStr::from_bytes(v));
    }
    cmd.stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    cmd.process_group(0);
    unsafe {
        cmd.pre_exec(|| {
            crate::procgroup::prctl(
                crate::procgroup::PR_SET_PDEATHSIG,
                crate::procgroup::SIGKILL as u64,
                0,
                0,
                0,
            );
            Ok(())
        });
    }
    let mut child = cmd
        .spawn()
        .map_err(|_| format!("the command {q} cannot start"))?;
    drop(cmd);
    let pgid = child.id() as i32;
    crate::procgroup::add(pgid);
    let stdin = child.stdin.take();
    let stdout = child.stdout.take();
    let gone = Arc::new(AtomicBool::new(false));
    let gone_w = gone.clone();
    std::thread::spawn(move || {
        let _ = child.wait();
        if crate::procgroup::forget_if_empty(pgid) {
            gone_w.store(true, Ordering::SeqCst);
        }
    });
    let (tx, rx) = mpsc::sync_channel(16);
    if let Some(out) = stdout {
        std::thread::spawn(move || {
            let mut br = BufReader::with_capacity(64 << 10, out);
            loop {
                let line = read_line(&mut br);
                let stop = !matches!(line, Line::Data(_));
                if tx.send(line).is_err() || stop {
                    return;
                }
            }
        });
    }
    Ok(Rc::new(McpServer {
        stdin: Arc::new(Mutex::new(stdin)),
        rx,
        pgid,
        gone,
        dead: RefCell::new(String::new()),
        next: Cell::new(0),
    }))
}

/// One remote tool as an executor tool.
struct McpTool {
    server: Rc<McpServer>,
    remote: String,
}

impl Tool for McpTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let args = Some(input.clone().unwrap_or_default());
        let raw = match self.server.request(
            "tools/call",
            jmap(vec![
                ("name", J::s(&self.remote)),
                ("arguments", jmap_any(&args)),
            ]),
        ) {
            Ok(r) => r,
            Err(e) => return (Vec::new(), Some(e.into_bytes())),
        };
        let unreadable = || {
            (
                Vec::new(),
                Some(b"the tool gave an unreadable result".to_vec()),
            )
        };
        let mut content: GoSlice<(String, String)> = Default::default();
        let mut is_error = false;
        let r = json::unmarshal(&raw, |d, n| {
            d.object(n, "", "", &["content", "isError"], |d, i, v| {
                if i == 0 {
                    d.slice(v, &mut content, "[]struct", |d, v, c| {
                        d.object(v, "", "", &["type", "text"], |d, i, x| {
                            if i == 0 {
                                d.string(x, &mut c.0)
                            } else {
                                d.string(x, &mut c.1)
                            }
                        })
                    });
                } else {
                    d.boolean(v, &mut is_error);
                }
            })
        });
        if r.is_err() {
            return unreadable();
        }
        let parts: Vec<String> = content
            .items()
            .iter()
            .map(|(t, text)| {
                if t == "text" {
                    text.clone()
                } else {
                    format!("[{t} content left out]")
                }
            })
            .collect();
        let text = parts.join("\n").into_bytes();
        if is_error {
            (Vec::new(), Some(text))
        } else {
            (text, None)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The tests that move the call timeout run one at a time.
    static SERIAL: Mutex<()> = Mutex::new(());

    /// A shell server that answers the handshake and the tool list, then
    /// does `tail` on the first tool call.
    fn sh_server(tail: &str) -> McpServerSpec {
        let script = format!(
            "read l; echo '{{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{{}}}}'; read l; read l; \
echo '{{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{{\"tools\":[{{\"name\":\"t\"}}]}}}}'; read l; {tail}"
        );
        McpServerSpec {
            name: "h".into(),
            command: b"sh".to_vec(),
            args: vec![b"-c".to_vec(), script.into_bytes()],
            env: Vec::new(),
        }
    }

    #[test]
    fn config_takes_stdio_servers_by_name_and_drops_key_variables() {
        let (specs, _) = parse_mcp_config(
            br#"{"mcpServers":{"b":{"command":"x"},"a":{"command":"y","args":["1"],"env":{"K":"v","ANTHROPIC_API_KEY":"no"}}}}"#,
        )
        .unwrap();
        assert_eq!(specs.len(), 2);
        assert_eq!((specs[0].name.as_str(), specs[1].name.as_str()), ("a", "b"));
        assert_eq!(specs[0].env, vec![(b"K".to_vec(), b"v".to_vec())]);
        for bad in [
            r#"{}"#,
            r#"{"mcpServers":{"a":{"type":"sse","url":"u"}}}"#,
            r#"{"mcpServers":{"a":{}}}"#,
            r#"{"mcpServers":{"a":{"command":""}}}"#,
        ] {
            assert!(parse_mcp_config(bad.as_bytes()).is_err(), "{bad}");
        }
    }

    #[test]
    fn a_silent_server_is_a_tool_result_then_dead() {
        let _g = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        crate::procgroup::begin();
        set_call_timeout(Duration::from_millis(1500));
        let set = McpSet::start(&[sh_server("sleep 30")], &mut std::io::sink()).unwrap();
        let tools = set.tools();
        let (out, err) = tools[0].1.run(&None);
        set_call_timeout(Duration::from_secs(30));
        assert!(out.is_empty());
        let err = String::from_utf8(err.unwrap()).unwrap();
        assert!(err.contains("no answer"), "{err}");
        let (_, err) = tools[0].1.run(&None);
        assert!(String::from_utf8(err.unwrap()).unwrap().contains("no answer"));
        crate::procgroup::kill_own();
    }

    #[test]
    fn a_message_over_32_mib_kills_the_server() {
        let _g = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        crate::procgroup::begin();
        let tail = "head -c 33554500 /dev/zero | tr '\\0' x; echo";
        let set = McpSet::start(&[sh_server(tail)], &mut std::io::sink()).unwrap();
        let (_, err) = set.tools()[0].1.run(&None);
        let err = String::from_utf8(err.unwrap()).unwrap();
        assert!(err.contains("32 MiB"), "{err}");
        crate::procgroup::kill_own();
    }
}
