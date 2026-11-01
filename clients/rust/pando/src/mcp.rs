//! The read tools and the stdio MCP server named pando. The twin of Go's
//! pando mcp.go. The server has no write tool.

use std::io::{BufRead, Write};

use serde_json::{Map, Value};

use super::branch::{branch_lines, has_main, recall_tree, Scope};
use super::chat::{Chat, ZOOM_PAGE_BYTES};
use super::recall::{format_hit, Embed, RECALL_MAX};

/// The longest request line the MCP server reads.
pub const MAX_TOOL_LINE: usize = 8 << 20;

const MCP_PROTOCOL: &str = "2024-11-05";

/// The version the server reports.
pub const VERSION: &str = "1";

pub type Args = Map<String, Value>;

/// Reads a whole-number argument of a tool call.
pub fn int_arg(args: &Args, name: &str) -> Result<i64, String> {
    let Some(v) = args.get(name) else {
        return Err(format!("{name} is missing"));
    };
    match v.as_f64() {
        Some(f) if f == f.trunc() && f.abs() <= (1u64 << 53) as f64 => Ok(f as i64),
        _ => Err(format!("{name} must be a whole number")),
    }
}

/// Reads a text argument. A missing one is "" unless need is set.
pub fn str_arg(args: &Args, name: &str, need: bool) -> Result<String, String> {
    match args.get(name) {
        None | Some(Value::Null) => {
            if need {
                Err(format!("{name} is missing"))
            } else {
                Ok(String::new())
            }
        }
        Some(Value::String(s)) => Ok(s.clone()),
        Some(_) => Err(format!("{name} must be text")),
    }
}

/// The text the model reads for the zoom tool.
pub const ZOOM_DESCRIPTION: &str = "Open a node of the chat's memory: give id and n. id is the first message, n the count: a power of two. n = 1 gives the message word for word; a larger node gives its two halves as lines. A long node comes in pages: give offset (a byte) and size to read the next page. A bad id answers with the root ids.";

/// What the zoom tool says about child stores.
pub const ZOOM_CHILD_TEXT: &str = " A worker's report names its child store and the root ids of its transcript: give child, the worker's label, with an id and an n from those roots to read inside that transcript. Without child the id is in the chat.";

/// The text the model reads for the date tool.
pub const DATE_DESCRIPTION: &str = "When message id was logged, in UTC.";

/// The text the model reads for the recall tool.
pub const RECALL_DESCRIPTION: &str = "Find where a word or a topic was said: give a query. You get node ids with a score and one line each, best first. Then zoom into an id to read it. k is how many (default 8, at most 32).";

/// What the recall tool says about branches.
pub const RECALL_BRANCH_TEXT: &str = " A hit in a branch below has an id like trellis:12+4: zoom it with branch trellis, id 12 and n 4.";

/// What the zoom and date tools say about branches.
pub const ZOOM_BRANCH_TEXT: &str = " Give branch, a path of labels such as trellis or trellis/ui, to read inside a branch below; the id is then in that branch.";

/// Answers the zoom tool over one chat.
pub fn tool_zoom(c: &Chat, args: &Args) -> Result<String, String> {
    let id = int_arg(args, "id")?;
    let n = int_arg(args, "n")?;
    let (mut offset, mut size) = (0, 0);
    if args.contains_key("offset") {
        offset = int_arg(args, "offset")?;
    }
    if args.contains_key("size") {
        size = int_arg(args, "size")?;
    }
    c.zoom_page(id, n, offset, size.min(ZOOM_PAGE_BYTES as i64 + 1))
}

/// Answers the zoom tool of the pando server over c's store: see
/// tool_zoom_scope.
pub fn tool_zoom_child(c: &Chat, args: &Args) -> Result<String, String> {
    let sc = Scope { store: c.dir().to_path_buf(), dir: c.dir().to_path_buf(), ..Default::default() };
    tool_zoom_scope(&sc, args)
}

/// Answers the zoom tool inside a scope: branch (a path of labels from the
/// scope) picks a branch below, child a worker's child store of it. Neither
/// ever leaves the scope.
pub fn tool_zoom_scope(sc: &Scope, args: &Args) -> Result<String, String> {
    let c = scoped_chat(sc, args, true)?;
    tool_zoom(&c, args)
}

fn scoped_chat(sc: &Scope, args: &Args, child: bool) -> Result<Chat, String> {
    if sc.closed {
        return Err(sc.why.clone());
    }
    let br = str_arg(args, "branch", false)?;
    let mut dir = sc.below(&br)?;
    if child {
        let label = str_arg(args, "child", false)?;
        if !label.is_empty() {
            let d = super::child::child_dir(&dir, &label)?;
            if !std::fs::symlink_metadata(&d).is_ok_and(|m| m.is_dir()) {
                return Err(format!("no worker transcript is named {}", crate::sys::go_quote(&label)));
            }
            dir = d;
        }
    }
    if !has_main(&dir) {
        if let Err(e) = std::fs::metadata(&dir) {
            return Err(crate::fsutil::path_err("stat", &dir, &e));
        }
        return Ok(Chat::empty_read_only(&dir));
    }
    Chat::open_read_only(&dir)
}

/// Answers the date tool inside a scope, with an optional branch.
pub fn tool_date_scope(sc: &Scope, args: &Args) -> Result<String, String> {
    let c = scoped_chat(sc, args, false)?;
    tool_date(&c, args)
}

/// Answers the recall tool over the scope and every branch below it.
pub fn tool_recall_scope(sc: &Scope, args: &Args, embed: Embed) -> Result<String, String> {
    if sc.closed {
        return Err(sc.why.clone());
    }
    let q = str_arg(args, "query", true)?;
    let mut k = 8;
    if args.contains_key("k") {
        k = int_arg(args, "k")?;
    }
    let hits = recall_tree(&sc.dir, &q, k.clamp(1, RECALL_MAX as i64) as usize, embed)?;
    if hits.is_empty() {
        return Ok("no match".into());
    }
    Ok(hits.iter().map(format_hit).collect::<Vec<_>>().join("\n"))
}

/// Answers the view tool: the scope's own view, with its branch lines.
pub fn tool_view_scope(sc: &Scope) -> Result<String, String> {
    if sc.closed {
        return Err(sc.why.clone());
    }
    let t = if has_main(&sc.dir) { super::live::view_context_of(&sc.dir)? } else { String::new() };
    if t.is_empty() {
        let b = branch_lines(&sc.dir);
        if !b.is_empty() {
            return Ok(b);
        }
        return Ok("The memory holds no message yet.".into());
    }
    Ok(t)
}

/// Answers the date tool.
pub fn tool_date(c: &Chat, args: &Args) -> Result<String, String> {
    let id = int_arg(args, "id")?;
    c.date(id)
}

/// The text the model reads for the view tool.
pub const VIEW_DESCRIPTION: &str = "Show the view again: the summary lines of every chat, fine near now and coarse in the past, then the newest messages that have no summary yet. No arguments. Use recall to search and zoom to read.";

/// Answers the view tool.
pub fn tool_view(c: &Chat, _args: &Args) -> Result<String, String> {
    let t = c.view_context();
    if t.is_empty() {
        return Ok("The memory holds no message yet.".into());
    }
    Ok(t)
}

/// Answers the recall tool: lines "id<TAB>score<TAB>line".
pub fn tool_recall(c: &Chat, args: &Args, embed: Embed) -> Result<String, String> {
    let q = str_arg(args, "query", true)?;
    let mut k = 8;
    if args.contains_key("k") {
        k = int_arg(args, "k")?;
    }
    let hits = c.recall(&q, k.clamp(1, RECALL_MAX as i64) as usize, embed);
    if hits.is_empty() {
        return Ok("no match".into());
    }
    Ok(hits.iter().map(format_hit).collect::<Vec<_>>().join("\n"))
}

fn obj(pairs: Vec<(&str, Value)>) -> Value {
    let mut m = Map::new();
    for (k, v) in pairs {
        m.insert(k.to_string(), v);
    }
    Value::Object(m)
}

fn schema(required: &[&str], props: Vec<(&str, Value)>) -> Value {
    obj(vec![
        ("type", Value::from("object")),
        ("properties", obj(props)),
        (
            "required",
            Value::Array(required.iter().map(|s| Value::from(*s)).collect()),
        ),
    ])
}

/// The tools/list result of the pando server.
pub fn read_tools() -> Vec<Value> {
    scope_tools(false)
}

/// The tools/list result of a pando server. A worker of a branch gets no
/// view tool.
pub fn scope_tools(worker: bool) -> Vec<Value> {
    let int = || obj(vec![("type", Value::from("integer"))]);
    let strp = || obj(vec![("type", Value::from("string"))]);
    let mut tools = vec![
        obj(vec![
            ("name", Value::from("zoom")),
            ("description", Value::from(format!("{ZOOM_DESCRIPTION}{ZOOM_CHILD_TEXT}{ZOOM_BRANCH_TEXT}"))),
            (
                "inputSchema",
                schema(
                    &["id", "n"],
                    vec![
                        ("id", int()),
                        ("n", int()),
                        ("offset", int()),
                        ("size", int()),
                        ("child", strp()),
                        ("branch", strp()),
                    ],
                ),
            ),
        ]),
        obj(vec![
            ("name", Value::from("date")),
            ("description", Value::from(format!("{DATE_DESCRIPTION}{ZOOM_BRANCH_TEXT}"))),
            ("inputSchema", schema(&["id"], vec![("id", int()), ("branch", strp())])),
        ]),
        obj(vec![
            ("name", Value::from("recall")),
            ("description", Value::from(format!("{RECALL_DESCRIPTION}{RECALL_BRANCH_TEXT}"))),
            (
                "inputSchema",
                schema(&["query"], vec![("query", strp()), ("k", int())]),
            ),
        ]),
    ];
    if !worker {
        tools.push(obj(vec![
            ("name", Value::from("view")),
            ("description", Value::from(VIEW_DESCRIPTION)),
            ("inputSchema", schema(&[], vec![])),
        ]));
    }
    tools
}

fn esc(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
}

/// A value as Go's json.Marshal writes a map[string]any: keys sorted, HTML
/// characters escaped.
pub fn marshal(v: &Value) -> String {
    let mut out = String::new();
    put(&mut out, v);
    out
}

fn put(out: &mut String, v: &Value) {
    match v {
        Value::Null => out.push_str("null"),
        Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
        Value::Number(n) => out.push_str(&n.to_string()),
        Value::String(s) => esc(out, s),
        Value::Array(a) => {
            out.push('[');
            for (i, x) in a.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                put(out, x);
            }
            out.push(']');
        }
        Value::Object(m) => {
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            out.push('{');
            for (i, k) in keys.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                esc(out, k);
                out.push(':');
                put(out, &m[*k]);
            }
            out.push('}');
        }
    }
}

/// A JSON-RPC result line.
pub fn rpc_reply(id: Option<&Value>, result: Value) -> String {
    marshal(&obj(vec![
        ("jsonrpc", Value::from("2.0")),
        ("id", id.cloned().unwrap_or(Value::Null)),
        ("result", result),
    ]))
}

/// A JSON-RPC error line.
pub fn rpc_error(id: Option<&Value>, code: i64, msg: &str) -> String {
    marshal(&obj(vec![
        ("jsonrpc", Value::from("2.0")),
        ("id", id.cloned().unwrap_or(Value::Null)),
        (
            "error",
            obj(vec![("code", Value::from(code)), ("message", Value::from(msg))]),
        ),
    ]))
}

/// Runs the stdio server named pando over one chat: zoom, date, recall and
/// view. It has no write tool. It returns when in ends.
pub fn serve_mcp(
    c: &Chat,
    embed: Embed,
    input: &mut dyn BufRead,
    out: &mut dyn Write,
) -> Result<(), String> {
    let sc = Scope { store: c.dir().to_path_buf(), dir: c.dir().to_path_buf(), ..Default::default() };
    serve_scope(&sc, embed, input, out)
}

/// Runs the pando server over a scope: the store, or a branch and
/// everything below it. A worker's scope has no view tool. A closed scope
/// lists no tool.
pub fn serve_scope(
    sc: &Scope,
    embed: Embed,
    input: &mut dyn BufRead,
    out: &mut dyn Write,
) -> Result<(), String> {
    let mut line = Vec::new();
    loop {
        line.clear();
        let n = input.read_until(b'\n', &mut line).map_err(|e| e.to_string())?;
        let msg = String::from_utf8_lossy(&line);
        let msg = msg.trim();
        if !msg.is_empty() {
            if let Some(resp) = mcp_handle(sc, embed, msg) {
                writeln!(out, "{resp}").map_err(|e| e.to_string())?;
                out.flush().map_err(|e| e.to_string())?;
            }
        }
        if n == 0 {
            return Ok(());
        }
    }
}

/// What a closed pando server answers to a call.
pub const CLOSED_TEXT: &str = "The memory is closed to this process: it is not a chat of the owner's.";

fn no_embed(_: &str) -> Vec<f64> {
    Vec::new()
}

/// The pando server of a process that is not the owner's chat (a steward
/// worker, a model call of daisugi's own). It lists no tool and answers
/// every call with CLOSED_TEXT, so the store stays out of that process.
pub fn serve_mcp_closed(input: &mut dyn BufRead, out: &mut dyn Write) -> Result<(), String> {
    let sc = Scope { closed: true, why: CLOSED_TEXT.into(), ..Default::default() };
    serve_scope(&sc, no_embed, input, out)
}

fn mcp_handle(sc: &Scope, embed: Embed, raw: &str) -> Option<String> {
    let req: Value = match serde_json::from_str(raw) {
        Ok(v @ Value::Object(_)) => v,
        _ => return Some(rpc_error(None, -32700, "parse error")),
    };
    let id = req.get("id");
    let method = req.get("method").and_then(|m| m.as_str()).unwrap_or("");
    match method {
        "notifications/initialized" => None,
        "initialize" => Some(rpc_reply(
            id,
            obj(vec![
                ("protocolVersion", Value::from(MCP_PROTOCOL)),
                ("capabilities", obj(vec![("tools", obj(vec![]))])),
                (
                    "serverInfo",
                    obj(vec![("name", Value::from("pando")), ("version", Value::from(VERSION))]),
                ),
            ]),
        )),
        "tools/list" => {
            let tools = if sc.closed { Vec::new() } else { scope_tools(sc.worker) };
            Some(rpc_reply(id, obj(vec![("tools", Value::Array(tools))])))
        }
        "tools/call" => {
            if sc.closed {
                let why = if sc.why.is_empty() { CLOSED_TEXT.to_string() } else { sc.why.clone() };
                return Some(rpc_reply(
                    id,
                    obj(vec![
                        (
                            "content",
                            Value::Array(vec![obj(vec![("type", Value::from("text")), ("text", Value::from(why))])]),
                        ),
                        ("isError", Value::from(true)),
                    ]),
                ));
            }
            let params = req.get("params");
            let name = params
                .and_then(|p| p.get("name"))
                .and_then(|n| n.as_str())
                .unwrap_or("");
            let args = params
                .and_then(|p| p.get("arguments"))
                .and_then(|a| a.as_object())
                .cloned()
                .unwrap_or_default();
            // Each call reads the store as it is now: reads take no lock, and
            // a server that runs for hours must see what was logged since it
            // started.
            let res = match name {
                "zoom" => tool_zoom_scope(sc, &args),
                "date" => tool_date_scope(sc, &args),
                "recall" => tool_recall_scope(sc, &args, embed),
                "view" if !sc.worker => tool_view_scope(sc),
                _ => {
                    let names = if sc.worker { "zoom, date, recall" } else { "zoom, date, recall, view" };
                    return Some(rpc_error(
                        id,
                        -32601,
                        &format!("no tool named {}. The tools are {names}.", crate::sys::go_quote(name)),
                    ));
                }
            };
            let (text, is_err) = match res {
                Ok(t) => (t, false),
                Err(e) => (e, true),
            };
            Some(rpc_reply(
                id,
                obj(vec![
                    (
                        "content",
                        Value::Array(vec![obj(vec![
                            ("type", Value::from("text")),
                            ("text", Value::from(text)),
                        ])]),
                    ),
                    ("isError", Value::from(is_err)),
                ]),
            ))
        }
        _ => {
            if id.is_none() {
                return None;
            }
            Some(rpc_error(id, -32601, &format!("method not found: {method}")))
        }
    }
}
