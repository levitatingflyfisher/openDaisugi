//! The foreman's four tools reach the model through a small MCP server
//! that only relays. `coppice foreman tools --sock S` is that server. It
//! speaks MCP on stdio, and for each tool call it opens the socket S,
//! sends one request line and reads one response line. It holds no state,
//! never dials the public socket, and runs no tool itself. The twin of
//! Go's internal/foreman tools.go.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::Duration;

use serde_json::{json, Map, Value};

use crate::gojson;

/// The four tools, in the order the model sees them.
pub const TOOL_NAMES: [&str; 4] = ["zoom", "date", "spawn", "status"];

/// The longest request or response line on a tool socket.
pub const MAX_TOOL_LINE: usize = 8 << 20;

const MCP_PROTOCOL: &str = "2024-11-05";

const TOOLS_USAGE: &str = "usage: coppice foreman tools [--standing | --sock SOCKET]";

/// One request line on a tool socket.
#[derive(Clone, Debug, Default)]
pub struct ToolRequest {
    pub tool: String,
    pub args: Map<String, Value>,
}

impl ToolRequest {
    /// The line as Go's json.Marshal writes the struct.
    pub fn encode(&self) -> String {
        gojson::marshal(&gojson::obj(vec![
            ("tool", json!(self.tool)),
            ("args", sorted(&Value::Object(self.args.clone()))),
        ]))
    }

    /// Reads a request line. Go's decoder takes a missing or null args as
    /// none.
    pub fn decode(line: &[u8]) -> Result<ToolRequest, String> {
        let v: Value = serde_json::from_slice(line).map_err(|e| e.to_string())?;
        let Value::Object(m) = v else {
            return Err("the request is not an object".into());
        };
        let tool = match m.get("tool") {
            None | Some(Value::Null) => String::new(),
            Some(Value::String(s)) => s.clone(),
            Some(_) => return Err("tool is not a string".into()),
        };
        let args = match m.get("args") {
            None | Some(Value::Null) => Map::new(),
            Some(Value::Object(a)) => a.clone(),
            Some(_) => return Err("args is not an object".into()),
        };
        Ok(ToolRequest { tool, args })
    }
}

/// The one response line.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct ToolResponse {
    pub ok: bool,
    pub result: String,
    pub error: String,
}

impl ToolResponse {
    /// The line as Go's json.Marshal writes the struct, empty fields left
    /// out.
    pub fn encode(&self) -> String {
        let mut p = vec![("ok", json!(self.ok))];
        if !self.result.is_empty() {
            p.push(("result", json!(self.result)));
        }
        if !self.error.is_empty() {
            p.push(("error", json!(self.error)));
        }
        gojson::marshal(&gojson::obj(p))
    }

    pub fn err(msg: impl Into<String>) -> ToolResponse {
        ToolResponse {
            ok: false,
            result: String::new(),
            error: msg.into(),
        }
    }
}

/// v as Go writes a value it decoded into any: every object's keys sorted,
/// as a map[string]any, and every number a float64.
pub fn sorted(v: &Value) -> Value {
    match v {
        Value::Number(n) => match n.as_f64() {
            Some(f) if f.is_finite() => gojson::float(f),
            _ => v.clone(),
        },
        Value::Object(m) => {
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            let mut out = Map::new();
            for k in keys {
                out.insert(k.clone(), sorted(&m[k]));
            }
            Value::Object(out)
        }
        Value::Array(a) => Value::Array(a.iter().map(sorted).collect()),
        x => x.clone(),
    }
}

fn object_schema(required: Option<&[&str]>, props: Vec<(&str, Value)>) -> Value {
    gojson::map(vec![
        ("type", json!("object")),
        ("properties", gojson::map(props)),
        (
            "required",
            match required {
                None => Value::Null,
                Some(r) => json!(r),
            },
        ),
    ])
}

/// The tools/list result.
pub fn tool_list() -> Value {
    let int = || gojson::map(vec![("type", json!("integer"))]);
    let s = || gojson::map(vec![("type", json!("string"))]);
    let boolean = || gojson::map(vec![("type", json!("boolean"))]);
    let tools = vec![
        gojson::map(vec![
            ("name", json!("zoom")),
            ("description", json!("Open a node of the chat's memory: give id and n. id is the first message, n the count: a power of two. n = 1 gives the message word for word; a larger node gives its two halves as lines. A long node comes in pages: give offset (a byte) and size to read the next page. A bad id answers with the root ids. Or give label, a worker's label: you get the newest 32 KiB of that worker's own log, once the worker has ended.")),
            ("inputSchema", object_schema(None, vec![("id", int()), ("n", int()), ("offset", int()), ("size", int()), ("label", s())])),
        ]),
        gojson::map(vec![
            ("name", json!("date")),
            ("description", json!("When message id was logged, in UTC.")),
            ("inputSchema", object_schema(Some(&["id"][..]), vec![("id", int())])),
        ]),
        gojson::map(vec![
            ("name", json!("spawn")),
            ("description", json!("Start a worker on a project with a brief. It returns at once with a label. The worker's report comes back later as a message that starts with the label in brackets. A profile that names a pool puts the worker on a machine with a free slot, or in the queue (the answer says which); background: true makes it a job for an idle machine, only on a pool.")),
            (
                "inputSchema",
                object_schema(
                    Some(&["project", "brief"][..]),
                    vec![("project", s()), ("brief", s()), ("profile", s()), ("model", s()), ("background", boolean())],
                ),
            ),
        ]),
        gojson::map(vec![
            ("name", json!("status")),
            ("description", json!("The workers: label, project, state, and when each started.")),
            ("inputSchema", object_schema(None, vec![])),
        ]),
    ];
    gojson::map(vec![("tools", Value::Array(tools))])
}

/// The id of a JSON-RPC message as Go's RawMessage keeps it, or null.
fn raw_id(v: Option<&Value>) -> Value {
    match v {
        None => Value::Null,
        Some(x) => x.clone(),
    }
}

fn rpc_reply(id: Value, result: Value) -> String {
    gojson::marshal(&gojson::map(vec![
        ("jsonrpc", json!("2.0")),
        ("id", id),
        ("result", result),
    ]))
}

fn rpc_error(id: Value, code: i64, msg: &str) -> String {
    gojson::marshal(&gojson::map(vec![
        ("jsonrpc", json!("2.0")),
        ("id", id),
        (
            "error",
            gojson::map(vec![("code", json!(code)), ("message", json!(msg))]),
        ),
    ]))
}

/// Sends one request to the tool socket and returns the response.
pub fn relay(sock: &Path, req: &ToolRequest) -> ToolResponse {
    let Ok(mut conn) = UnixStream::connect(sock) else {
        return ToolResponse::err("the foreman's tools are closed: this turn is over");
    };
    let _ = conn.set_read_timeout(Some(Duration::from_secs(120)));
    let _ = conn.set_write_timeout(Some(Duration::from_secs(120)));
    if let Err(e) = conn.write_all(format!("{}\n", req.encode()).as_bytes()) {
        return ToolResponse::err(format!(
            "the foreman's tools did not take the call: {}",
            crate::sys::go_errno_text(&e)
        ));
    }
    let mut line = Vec::new();
    let mut r = BufReader::with_capacity(64 * 1024, (&conn).take(MAX_TOOL_LINE as u64 + 2));
    let _ = r.read_until(b'\n', &mut line);
    if line.is_empty() {
        return ToolResponse::err("the foreman's tools gave no answer");
    }
    if line.len() > MAX_TOOL_LINE {
        return ToolResponse::err("the answer is too long");
    }
    let Ok(v) = serde_json::from_slice::<Value>(line.trim_ascii()) else {
        return ToolResponse::err("the foreman's tools gave an answer that is not JSON");
    };
    let Value::Object(m) = v else {
        return ToolResponse::err("the foreman's tools gave an answer that is not JSON");
    };
    let s = |k: &str| m.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    ToolResponse {
        ok: m.get("ok").and_then(Value::as_bool).unwrap_or(false),
        result: s("result"),
        error: s("error"),
    }
}

/// Answers one MCP message, or None for a notification.
fn handle(sock: &Path, raw: &[u8]) -> Option<String> {
    let Ok(Value::Object(req)) = serde_json::from_slice::<Value>(raw) else {
        return Some(rpc_error(Value::Null, -32700, "parse error"));
    };
    let id = raw_id(req.get("id"));
    let method = req.get("method").and_then(Value::as_str).unwrap_or("");
    match method {
        "notifications/initialized" => None,
        "initialize" => Some(rpc_reply(
            id,
            gojson::map(vec![
                ("protocolVersion", json!(MCP_PROTOCOL)),
                (
                    "capabilities",
                    gojson::map(vec![("tools", gojson::map(vec![]))]),
                ),
                (
                    "serverInfo",
                    gojson::map(vec![("name", json!("foreman")), ("version", json!("1"))]),
                ),
            ]),
        )),
        "tools/list" => Some(rpc_reply(id, tool_list())),
        "tools/call" => {
            let params = req.get("params");
            let name = params
                .and_then(|p| p.get("name"))
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();
            let args = params
                .and_then(|p| p.get("arguments"))
                .and_then(Value::as_object)
                .cloned()
                .unwrap_or_default();
            let (text, is_err) = if !TOOL_NAMES.contains(&name.as_str()) {
                (
                    format!(
                        "no tool named {}. The tools are {}.",
                        crate::sys::go_quote(&name),
                        TOOL_NAMES.join(", ")
                    ),
                    true,
                )
            } else {
                let resp = relay(sock, &ToolRequest { tool: name, args });
                if resp.ok {
                    (resp.result, false)
                } else {
                    (resp.error, true)
                }
            };
            Some(rpc_reply(
                id,
                gojson::map(vec![
                    (
                        "content",
                        json!([gojson::map(vec![
                            ("type", json!("text")),
                            ("text", json!(text))
                        ])]),
                    ),
                    ("isError", json!(is_err)),
                ]),
            ))
        }
        _ => {
            if matches!(req.get("id"), None | Some(Value::Null)) && !req.contains_key("id") {
                return None;
            }
            Some(rpc_error(
                id,
                -32601,
                &format!("method not found: {method}"),
            ))
        }
    }
}

/// The pane foreman's tool socket for a coppice data dir: tools.sock in
/// the log dir.
pub fn standing_sock(coppice_data_dir: &Path) -> PathBuf {
    super::log_dir(coppice_data_dir).join("tools.sock")
}

/// Runs `coppice foreman tools [--standing | --sock S]`, the MCP stdio
/// relay. data_dir is the dir `coppice --data-dir` named, or None. With no
/// --sock, or with --standing, it dials the standing socket of that data
/// dir, else of $COPPICE_DATA_DIR (every pane has it), else of the default
/// data dir.
pub fn tools_main(
    data_dir: Option<&Path>,
    argv: &[String],
    input: &mut dyn Read,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    let (mut sock, mut standing) = (String::new(), false);
    let mut k = 0;
    while k < argv.len() {
        if argv[k] == "--sock" && k + 1 < argv.len() {
            sock = argv[k + 1].clone();
            k += 2;
            continue;
        }
        if argv[k] == "--standing" {
            standing = true;
            k += 1;
            continue;
        }
        let _ = writeln!(errw, "{TOOLS_USAGE}");
        return 1;
    }
    if !sock.is_empty() && standing {
        let _ = writeln!(errw, "{TOOLS_USAGE}");
        return 1;
    }
    let sock = if sock.is_empty() {
        let dir = match data_dir {
            Some(d) if !d.as_os_str().is_empty() => d.to_path_buf(),
            _ => match std::env::var_os("COPPICE_DATA_DIR").filter(|v| !v.is_empty()) {
                Some(d) => PathBuf::from(d),
                None => crate::datahome::default().join("coppice"),
            },
        };
        standing_sock(&dir)
    } else {
        PathBuf::from(sock)
    };
    let mut br = BufReader::with_capacity(64 * 1024, input);
    loop {
        let mut line = Vec::new();
        let res = br.read_until(b'\n', &mut line);
        let msg = line.trim_ascii();
        if !msg.is_empty() {
            if let Some(resp) = handle(&sock, msg) {
                if out
                    .write_all(format!("{resp}\n").as_bytes())
                    .and_then(|_| out.flush())
                    .is_err()
                {
                    return 1;
                }
            }
        }
        match res {
            Ok(0) => return 0,
            Ok(_) if !line.ends_with(b"\n") => return 0,
            Ok(_) => {}
            Err(_) => return 1,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;
    use std::sync::{Arc, Mutex};

    fn fake_socket(
        tag: &str,
        answer: impl Fn(&ToolRequest) -> ToolResponse + Send + 'static,
    ) -> (PathBuf, Arc<Mutex<Vec<ToolRequest>>>) {
        let dir = std::env::temp_dir().join(format!("ts-{}-{tag}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("t.sock");
        let ln = UnixListener::bind(&path).unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let s2 = seen.clone();
        std::thread::spawn(move || {
            for c in ln.incoming() {
                let Ok(mut c) = c else { return };
                let mut line = Vec::new();
                let _ = BufReader::new(&c).read_until(b'\n', &mut line);
                let req = ToolRequest::decode(line.trim_ascii()).unwrap_or_default();
                let resp = answer(&req);
                s2.lock().unwrap().push(req);
                let _ = c.write_all(format!("{}\n", resp.encode()).as_bytes());
            }
        });
        (path, seen)
    }

    fn mcp(sock: &Path, lines: &[&str]) -> Vec<Value> {
        let input = lines.join("\n") + "\n";
        let (mut out, mut errb) = (Vec::new(), Vec::new());
        let argv = vec!["--sock".to_string(), sock.to_string_lossy().into_owned()];
        assert_eq!(
            tools_main(None, &argv, &mut input.as_bytes(), &mut out, &mut errb),
            0
        );
        String::from_utf8(out)
            .unwrap()
            .lines()
            .map(|l| serde_json::from_str(l).unwrap())
            .collect()
    }

    #[test]
    fn the_relay_offers_exactly_four_tools() {
        let (sock, _) = fake_socket("four", |_| ToolResponse {
            ok: true,
            ..Default::default()
        });
        let got = mcp(
            &sock,
            &[
                r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}"#,
                r#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#,
                r#"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#,
            ],
        );
        assert_eq!(got.len(), 2);
        let names: Vec<&str> = got[1]["result"]["tools"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| t["name"].as_str().unwrap())
            .collect();
        assert_eq!(names, ["zoom", "date", "spawn", "status"]);
        // The zoom tool takes a label and requires nothing: the server checks.
        let zoom = &got[1]["result"]["tools"][0]["inputSchema"];
        assert!(zoom["required"].is_null());
        assert!(zoom["properties"]["label"].is_object());
    }

    #[test]
    fn the_relay_passes_a_call_to_the_socket_and_back() {
        let (sock, seen) = fake_socket("pass", |r| {
            if r.tool == "date" {
                return ToolResponse::err("no message 9");
            }
            ToolResponse {
                ok: true,
                result: "the text".into(),
                ..Default::default()
            }
        });
        let got = mcp(
            &sock,
            &[
                r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":4}}}"#,
                r#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"date","arguments":{"id":9}}}"#,
                r#"{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"cmd":"ls"}}}"#,
            ],
        );
        let text = |i: usize| {
            let r = &got[i]["result"];
            (
                r["content"][0]["text"].as_str().unwrap().to_string(),
                r["isError"].as_bool().unwrap(),
            )
        };
        assert_eq!(text(0), ("the text".to_string(), false));
        assert_eq!(text(1), ("no message 9".to_string(), true));
        assert!(text(2).1 && text(2).0.contains("no tool named"));
        let seen = seen.lock().unwrap();
        assert_eq!(seen.len(), 2);
        assert_eq!(seen[0].tool, "zoom");
        assert_eq!(seen[0].args["n"].as_f64(), Some(4.0));
    }

    #[test]
    fn a_call_after_the_turn_is_over_says_so() {
        let got = mcp(
            Path::new("/nonexistent/gone.sock"),
            &[
                r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{}}}"#,
            ],
        );
        let r = &got[0]["result"];
        assert!(r["isError"].as_bool().unwrap());
        assert!(r["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("this turn is over"));
    }

    #[test]
    fn standing_finds_the_socket_of_the_data_dir() {
        let dir = std::env::temp_dir().join(format!("sd-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(super::super::log_dir(&dir)).unwrap();
        let ln = UnixListener::bind(standing_sock(&dir)).unwrap();
        std::thread::spawn(move || {
            for c in ln.incoming() {
                let Ok(mut c) = c else { return };
                let mut line = Vec::new();
                let _ = BufReader::new(&c).read_until(b'\n', &mut line);
                let _ = c.write_all(b"{\"ok\":true,\"result\":\"from the standing socket\"}\n");
            }
        });
        let call = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"status\"}}\n";
        for argv in [vec!["--standing".to_string()], vec![]] {
            let (mut out, mut errb) = (Vec::new(), Vec::new());
            assert_eq!(
                tools_main(Some(&dir), &argv, &mut call.as_bytes(), &mut out, &mut errb),
                0
            );
            assert!(String::from_utf8(out)
                .unwrap()
                .contains("from the standing socket"));
        }
        let (mut out, mut errb) = (Vec::new(), Vec::new());
        let argv = vec![
            "--standing".to_string(),
            "--sock".to_string(),
            "x".to_string(),
        ];
        assert_eq!(
            tools_main(Some(&dir), &argv, &mut "".as_bytes(), &mut out, &mut errb),
            1
        );
        assert!(String::from_utf8(errb).unwrap().contains("usage:"));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
