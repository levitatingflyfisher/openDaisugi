//! The MCP protocol as the oracle's server speaks it over stdio: the lines
//! it reads, the messages it takes, and the JSON it writes. The oracle's
//! server is the mcp SDK's FastMCP; this module copies what that SDK
//! answers, byte for byte, without taking the SDK. The Go client's
//! `mcpwire` package is the reference.
//!
//! A line is read with universal newlines (\n, \r\n and a lone \r end it)
//! and decoded as UTF-8 with each bad byte run replaced, as Python's
//! `TextIOWrapper(errors="replace")` reads stdin. A line that is not a
//! JSON-RPC request or notification gets the SDK's error notification. A
//! request with an id and a method the client may send is checked the way
//! the SDK checks it; a request it rejects gets -32602.

mod tools_gen;

use std::collections::HashMap;
use std::io::BufRead;
use std::sync::OnceLock;

pub use tools_gen::*;

use crate::gate::pyjson::{any_json, loads, py_decode_error, Object, PyDecodeFail, Value};
use crate::pathways::pmodel::{self, Field, Loc, Mode, Model, Schema, ValidationError};

/// Reads stdin as the oracle's stdio transport does.
pub struct LineReader<R: BufRead> {
    r: R,
}

impl<R: BufRead> LineReader<R> {
    pub fn new(r: R) -> Self {
        LineReader { r }
    }

    fn byte(&mut self) -> Option<u8> {
        let b = *self.r.fill_buf().ok()?.first()?;
        self.r.consume(1);
        Some(b)
    }

    fn peek(&mut self) -> Option<u8> {
        self.r.fill_buf().ok()?.first().copied()
    }

    /// The next line, its end made "\n", or None at the end of input. A
    /// last line with no end is returned as it is.
    pub fn next_line(&mut self) -> Option<String> {
        let mut buf = vec![];
        loop {
            let Some(c) = self.byte() else {
                if buf.is_empty() {
                    return None;
                }
                return Some(decode(&buf));
            };
            match c {
                b'\n' => return Some(decode(&buf) + "\n"),
                b'\r' => {
                    // A \r\n is one end; the byte after a \r is read before
                    // the line is given, as Python's newline decoder waits
                    // for it.
                    if self.peek() == Some(b'\n') {
                        self.byte();
                    }
                    return Some(decode(&buf) + "\n");
                }
                _ => buf.push(c),
            }
        }
    }
}

fn decode(b: &[u8]) -> String {
    crate::gate::py::text::decode_utf8_replace(b)
}

/// What a line holds for the server.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// A line the SDK cannot take as a request or notification: not JSON,
    /// not an object, or a response. It gets `ERROR_NOTIFICATION`.
    Invalid,
    /// It has an id: it gets one reply.
    Request,
    /// No id (or one the SDK does not read as an id): it gets nothing.
    Notification,
}

/// One line, classified.
#[derive(Debug, Clone)]
pub struct Message {
    pub kind: Kind,
    /// A `Value::Int` or `Value::Str`; Null for anything but a request.
    pub id: Value,
    pub method: String,
    /// The params object as the SDK passes it on, NaN and the infinities
    /// made None (`model_dump(mode="json")`). None when absent or null.
    pub params: Option<Object>,
}

fn invalid() -> Message {
    Message {
        kind: Kind::Invalid,
        id: Value::Null,
        method: String::new(),
        params: None,
    }
}

/// Reads a line as `JSONRPCMessage.model_validate_json` does and says what
/// the SDK does with it.
pub fn classify(line: &str) -> Message {
    let Ok(Value::Obj(o)) = pmodel::parse_json("JSONRPCMessage", line) else {
        return invalid();
    };
    if o.get("jsonrpc") != Some(&Value::Str("2.0".into())) {
        return invalid();
    }
    let Value::Str(method) = o.value("method") else {
        return invalid();
    };
    let params = match o.get("params") {
        None | Some(Value::Null) => None,
        Some(Value::Obj(p)) => match any_json(&Value::Obj(p.clone())) {
            Value::Obj(p) => Some(p),
            _ => None,
        },
        // A response, an error reply or anything else leads to the same
        // error notification.
        Some(_) => return invalid(),
    };
    let method = method.clone();
    match o.get("id") {
        Some(id @ (Value::Int(_) | Value::Str(_))) => Message {
            kind: Kind::Request,
            id: id.clone(),
            method,
            params,
        },
        _ => Message {
            kind: Kind::Notification,
            id: Value::Null,
            method,
            params,
        },
    }
}

/// The methods of the SDK's ClientRequest.
const CLIENT_METHODS: &[&str] = &[
    "ping",
    "initialize",
    "completion/complete",
    "logging/setLevel",
    "prompts/get",
    "prompts/list",
    "resources/list",
    "resources/templates/list",
    "resources/read",
    "resources/subscribe",
    "resources/unsubscribe",
    "tools/call",
    "tools/list",
    "tasks/get",
    "tasks/result",
    "tasks/list",
    "tasks/cancel",
];

/// The client methods this binary checks and answers. A request for
/// another client method is refused.
pub const CARRIED: &[&str] = &[
    "ping",
    "initialize",
    "logging/setLevel",
    "prompts/get",
    "prompts/list",
    "resources/list",
    "resources/templates/list",
    "tools/call",
    "tools/list",
    "resources/read",
    "resources/subscribe",
    "resources/unsubscribe",
    "completion/complete",
    "tasks/get",
    "tasks/result",
    "tasks/list",
    "tasks/cancel",
];

/// `ClientRequest.model_validate` for a request.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Checked {
    /// The SDK takes it.
    Ok,
    /// The SDK answers -32602.
    Invalid,
    /// A client method, or a form of one, this binary does not check.
    Unported,
}

fn str_or_int(v: &Value) -> bool {
    matches!(v, Value::Str(_) | Value::Int(_) | Value::Bool(_))
}

/// The key is absent, null or a str.
fn opt_str(o: &Object, k: &str) -> bool {
    matches!(o.get(k), None | Some(Value::Null) | Some(Value::Str(_)))
}

pub fn check(m: &Message) -> Checked {
    use Checked::*;
    let method = m.method.as_str();
    if !CLIENT_METHODS.contains(&method) {
        return Invalid;
    }
    if !CARRIED.contains(&method) {
        return Unported;
    }
    let Some(p) = &m.params else {
        if [
            "initialize",
            "logging/setLevel",
            "prompts/get",
            "tools/call",
            "resources/read",
            "resources/subscribe",
            "resources/unsubscribe",
            "completion/complete",
            "tasks/get",
            "tasks/result",
            "tasks/cancel",
        ]
        .contains(&method)
        {
            return Invalid;
        }
        return Ok;
    };
    // RequestParams: an optional task and _meta with an optional
    // progressToken. A tool call's task (TaskMetadata: an optional int
    // ttl) is read and then not used, as the oracle's server does.
    match p.get("task") {
        None | Some(Value::Null) => {}
        Some(_) if method != "tools/call" => return Unported,
        Some(Value::Obj(t)) => match t.get("ttl") {
            None | Some(Value::Null) | Some(Value::Int(_)) => {}
            // pydantic's lax int takes some floats and strings.
            Some(_) => return Unported,
        },
        Some(_) => return Invalid,
    }
    match p.get("_meta") {
        None | Some(Value::Null) => {}
        Some(Value::Obj(meta)) => {
            if let Some(tok) = meta.get("progressToken") {
                if !tok.is_null() && !str_or_int(tok) {
                    return Invalid;
                }
            }
        }
        Some(_) => return Invalid,
    }
    let is_obj = |v: Option<&Value>| matches!(v, None | Some(Value::Null) | Some(Value::Obj(_)));
    match method {
        "tools/list" | "resources/list" | "resources/templates/list" | "prompts/list" => {
            if !opt_str(p, "cursor") {
                return Invalid;
            }
        }
        "resources/read" | "resources/subscribe" | "resources/unsubscribe" => {
            let Value::Str(u) = p.value("uri") else { return Invalid };
            if let Err(c) = norm_uri(u) {
                return c;
            }
        }
        "tasks/get" | "tasks/result" | "tasks/cancel" => {
            if !matches!(p.value("taskId"), Value::Str(_)) {
                return Invalid;
            }
        }
        "tasks/list" => {
            if !opt_str(p, "cursor") {
                return Invalid;
            }
        }
        "completion/complete" => {
            let Value::Obj(r) = p.value("ref") else { return Invalid };
            let ok_ref = match r.value("type") {
                Value::Str(t) if t == "ref/prompt" => matches!(r.value("name"), Value::Str(_)) && opt_str(r, "title"),
                Value::Str(t) if t == "ref/resource" => matches!(r.value("uri"), Value::Str(_)),
                _ => false,
            };
            if !ok_ref {
                return Invalid;
            }
            let Value::Obj(a) = p.value("argument") else { return Invalid };
            if !matches!(a.value("name"), Value::Str(_)) || !matches!(a.value("value"), Value::Str(_)) {
                return Invalid;
            }
            match p.get("context") {
                None | Some(Value::Null) => {}
                Some(Value::Obj(c)) => match c.get("arguments") {
                    None | Some(Value::Null) => {}
                    Some(Value::Obj(args)) => {
                        if args.iter().any(|(_, v)| !matches!(v, Value::Str(_))) {
                            return Invalid;
                        }
                    }
                    Some(_) => return Invalid,
                },
                Some(_) => return Invalid,
            }
        }
        "tools/call" => {
            if !matches!(p.value("name"), Value::Str(_)) || !is_obj(p.get("arguments")) {
                return Invalid;
            }
        }
        "prompts/get" => {
            if !matches!(p.value("name"), Value::Str(_)) {
                return Invalid;
            }
            match p.get("arguments") {
                None | Some(Value::Null) => {}
                Some(Value::Obj(a)) => {
                    if a.iter().any(|(_, v)| !matches!(v, Value::Str(_))) {
                        return Invalid;
                    }
                }
                Some(_) => return Invalid,
            }
        }
        "logging/setLevel" => {
            let levels = [
                "debug",
                "info",
                "notice",
                "warning",
                "error",
                "critical",
                "alert",
                "emergency",
            ];
            if !matches!(p.value("level"), Value::Str(l) if levels.contains(&l.as_str())) {
                return Invalid;
            }
        }
        "initialize" => {
            if !str_or_int(p.value("protocolVersion")) {
                return Invalid;
            }
            let Value::Obj(caps) = p.value("capabilities") else {
                return Invalid;
            };
            for k in ["experimental", "sampling", "elicitation", "roots", "tasks"] {
                if !is_obj(caps.get(k)) {
                    return Invalid;
                }
            }
            let Value::Obj(info) = p.value("clientInfo") else {
                return Invalid;
            };
            if !matches!(info.value("name"), Value::Str(_))
                || !matches!(info.value("version"), Value::Str(_))
            {
                return Invalid;
            }
            if !opt_str(info, "title") || !opt_str(info, "websiteUrl") {
                return Invalid;
            }
        }
        _ => {}
    }
    Ok
}

/// The version the server answers initialize with.
pub fn protocol(requested: &Value) -> &'static str {
    if let Value::Str(s) = requested {
        if let Some(v) = SUPPORTED_PROTOCOLS.iter().find(|v| *v == s) {
            return v;
        }
    }
    LATEST_PROTOCOL
}

/// The line the SDK writes for a line it cannot take.
pub const ERROR_NOTIFICATION: &str = "{\"method\":\"notifications/message\",\"params\":{\"level\":\"error\",\
\"logger\":\"mcp.server.exception_handler\",\"data\":\"Internal Server Error\"},\"jsonrpc\":\"2.0\"}";

/// A result line.
pub fn reply(id: &Value, result: &str) -> String {
    format!(
        "{{\"jsonrpc\":\"2.0\",\"id\":{},\"result\":{result}}}",
        compact(id)
    )
}

/// An error line; a data of None is left out.
pub fn error_reply(id: &Value, code: i64, message: &str, data: Option<&str>) -> String {
    let mut e = Object::new();
    e.set("code", Value::Int(code.to_string()));
    e.set("message", message);
    if let Some(d) = data {
        e.set("data", d);
    }
    format!(
        "{{\"jsonrpc\":\"2.0\",\"id\":{},\"error\":{}}}",
        compact(id),
        compact(&Value::Obj(e))
    )
}

/// The SDK's reply to a request it does not take.
pub fn invalid_params(id: &Value) -> String {
    error_reply(id, -32602, "Invalid request parameters", Some(""))
}

/// The initialize reply's result.
pub fn initialize_result(protocol: &str) -> String {
    format!(
        "{{\"protocolVersion\":{},\"capabilities\":{CAPABILITIES},\"serverInfo\":{{\"name\":{},\"version\":{}}},\"instructions\":{}}}",
        compact(&Value::Str(protocol.into())),
        compact(&Value::Str(SERVER_NAME.into())),
        compact(&Value::Str(SERVER_VERSION.into())),
        compact(&Value::Str(INSTRUCTIONS.into())),
    )
}

/// A CallToolResult: the text blocks, the structured content (None: left
/// out) and whether it is an error.
pub fn tool_result(texts: &[String], structured: Option<&Value>, is_error: bool) -> String {
    let mut b = String::from("{\"content\":[");
    for (i, t) in texts.iter().enumerate() {
        if i > 0 {
            b.push(',');
        }
        b.push_str(&format!(
            "{{\"type\":\"text\",\"text\":{}}}",
            compact(&Value::Str(t.clone()))
        ));
    }
    b.push(']');
    if let Some(s) = structured {
        b.push_str(&format!(",\"structuredContent\":{}", compact(s)));
    }
    b.push_str(if is_error {
        ",\"isError\":true}"
    } else {
        ",\"isError\":false}"
    });
    b
}

// ---------------------------------------------------------------------------
// pydantic's JSON
// ---------------------------------------------------------------------------

/// pydantic's `model_dump_json` of a value: no spaces, text as it is (only
/// quotes, backslashes and control characters escaped), pydantic's float
/// text, and NaN and the infinities as null.
pub fn compact(v: &Value) -> String {
    let mut b = String::new();
    write(&mut b, v, "", 0, false);
    b
}

/// `pydantic_core.to_json(v, indent=2)`: the text FastMCP puts in a tool
/// result's text block. NaN and the infinities are written as NaN,
/// Infinity and -Infinity.
pub fn indent(v: &Value) -> String {
    let mut b = String::new();
    write(&mut b, v, "  ", 0, true);
    b
}

fn float_text(f: f64, nan_text: bool) -> String {
    if f.is_nan() {
        return if nan_text { "NaN" } else { "null" }.into();
    }
    if f.is_infinite() {
        return match (nan_text, f > 0.0) {
            (false, _) => "null",
            (true, true) => "Infinity",
            (true, false) => "-Infinity",
        }
        .into();
    }
    crate::cli::envelope::float_json(f)
}

fn newline(b: &mut String, unit: &str, level: usize) {
    if unit.is_empty() {
        return;
    }
    b.push('\n');
    for _ in 0..level {
        b.push_str(unit);
    }
}

fn write(b: &mut String, v: &Value, unit: &str, level: usize, nan_text: bool) {
    match v {
        Value::Null => b.push_str("null"),
        Value::Bool(true) => b.push_str("true"),
        Value::Bool(false) => b.push_str("false"),
        Value::Int(t) => b.push_str(t),
        Value::Float(f) => b.push_str(&float_text(*f, nan_text)),
        Value::Str(s) => write_string(b, s),
        Value::List(l) | Value::Tuple(l) => {
            if l.is_empty() {
                b.push_str("[]");
                return;
            }
            b.push('[');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                newline(b, unit, level + 1);
                write(b, e, unit, level + 1, nan_text);
            }
            newline(b, unit, level);
            b.push(']');
        }
        Value::Obj(o) => {
            if o.is_empty() {
                b.push_str("{}");
                return;
            }
            b.push('{');
            for (i, (k, x)) in o.iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                newline(b, unit, level + 1);
                write_string(b, k);
                b.push_str(if unit.is_empty() { ":" } else { ": " });
                write(b, x, unit, level + 1, nan_text);
            }
            newline(b, unit, level);
            b.push('}');
        }
    }
}

/// serde_json's string: ", \ and the control characters below 0x20
/// escaped, everything else as UTF-8.
fn write_string(b: &mut String, s: &str) {
    b.push('"');
    for c in s.chars() {
        match c {
            '"' => b.push_str("\\\""),
            '\\' => b.push_str("\\\\"),
            '\n' => b.push_str("\\n"),
            '\r' => b.push_str("\\r"),
            '\t' => b.push_str("\\t"),
            '\u{8}' => b.push_str("\\b"),
            '\u{c}' => b.push_str("\\f"),
            c if (c as u32) < 0x20 => b.push_str(&format!("\\u{:04x}", c as u32)),
            c => b.push(c),
        }
    }
    b.push('"');
}

// ---------------------------------------------------------------------------
// The tools' arguments
// ---------------------------------------------------------------------------

/// Nesting well under where `json.loads` would raise RecursionError.
const PRE_PARSE_DEPTH: usize = 900;

fn field(name: &'static str, schema: Schema, default: Option<fn() -> Value>) -> Field {
    Field {
        name,
        schema,
        default,
        omit_none: false,
    }
}

fn model(name: &'static str, fields: Vec<Field>) -> Model {
    Model {
        name,
        fields,
        finite: false,
    }
}

fn str_s() -> Schema {
    Schema::Str
}

fn opt_str_s() -> Schema {
    Schema::Nullable(Box::new(Schema::Str))
}

fn dict_s() -> Schema {
    Schema::Dict(Box::new(Schema::Any))
}

/// Each tool's argument model (FastMCP's `<tool>Arguments`).
pub fn args(tool: &str) -> Option<&'static Model> {
    static M: OnceLock<HashMap<&'static str, Model>> = OnceLock::new();
    M.get_or_init(|| {
        let mut m = HashMap::new();
        m.insert(
            "envelope_for",
            model(
                "envelope_forArguments",
                vec![
                    field("task", str_s(), None),
                    field("stakes", str_s(), Some(|| Value::Str("medium".into()))),
                    field("context", opt_str_s(), Some(|| Value::Null)),
                ],
            ),
        );
        m.insert(
            "find_pathway",
            model("find_pathwayArguments", vec![field("task", str_s(), None)]),
        );
        m.insert(
            "recall",
            model(
                "recallArguments",
                vec![
                    field("task", str_s(), None),
                    field("envelope", dict_s(), None),
                    field(
                        "z3_timeout_ms",
                        Schema::Int,
                        Some(|| Value::Int("500".into())),
                    ),
                ],
            ),
        );
        m.insert(
            "recall_answer",
            model(
                "recall_answerArguments",
                vec![
                    field("task", str_s(), None),
                    field("current_ground_hash", opt_str_s(), Some(|| Value::Null)),
                    field(
                        "max_age_seconds",
                        Schema::Float,
                        Some(|| Value::Float(604800.0)),
                    ),
                ],
            ),
        );
        m.insert(
            "verify_plan",
            model(
                "verify_planArguments",
                vec![
                    field("plan", dict_s(), None),
                    field("envelope", dict_s(), None),
                ],
            ),
        );
        m.insert(
            "verify_completed_step",
            model(
                "verify_completed_stepArguments",
                vec![
                    field("step", dict_s(), None),
                    field("envelope", dict_s(), None),
                ],
            ),
        );
        m.insert("list_pathways", model("list_pathwaysArguments", vec![]));
        m.insert("pathway_stats", model("pathway_statsArguments", vec![]));
        m.insert(
            "run_plan",
            model(
                "run_planArguments",
                vec![
                    field("plan", dict_s(), None),
                    field("envelope", dict_s(), None),
                    field("dry_run", Schema::Bool, Some(|| Value::Bool(true))),
                ],
            ),
        );
        m.insert(
            "receipts_for_run",
            model(
                "receipts_for_runArguments",
                vec![field("run_id", str_s(), None)],
            ),
        );
        m.insert(
            "recent_runs",
            model(
                "recent_runsArguments",
                vec![field(
                    "limit",
                    Schema::Int,
                    Some(|| Value::Int("20".into())),
                )],
            ),
        );
        m.insert(
            "delegate",
            model(
                "delegateArguments",
                vec![
                    field("path", str_s(), None),
                    field("question", str_s(), None),
                    field("mode", str_s(), Some(|| Value::Str("bulk_read".into()))),
                ],
            ),
        );
        m
    })
    .get(tool)
}

/// The fields annotated exactly str: `pre_parse_json` leaves their values
/// alone.
const STR_ONLY: &[&str] = &["task", "stakes", "run_id", "path", "question", "mode"];

/// `FuncMetadata.pre_parse_json`: a str value given for a field not
/// annotated str is read with `json.loads`, and kept when that gives a
/// list, a dict or None. None when a value holds text `json.loads` reads
/// into a form this binary does not take (nesting deeper than it reads).
pub fn pre_parse(m: &Model, args: &Object) -> Option<Object> {
    let mut out = Object::with_capacity(args.len());
    for (k, v) in args.iter() {
        out.set(k, v.clone());
        let Value::Str(s) = v else { continue };
        if STR_ONLY.contains(&k.as_str()) || !m.fields.iter().any(|f| f.name == k) {
            continue;
        }
        match py_decode_error(s, PRE_PARSE_DEPTH) {
            Some(PyDecodeFail::TooDeep) => return None,
            Some(PyDecodeFail::Raised(_)) => continue,
            None => {}
        }
        let parsed = match loads(s) {
            Ok(p) => p,
            Err(crate::gate::pyjson::LoadError::Recursion) => return None,
            Err(_) => continue,
        };
        if matches!(
            parsed,
            Value::Str(_) | Value::Int(_) | Value::Float(_) | Value::Bool(_)
        ) {
            continue;
        }
        out.set(k, parsed);
    }
    Some(out)
}

/// A tool's arguments validated by its model, as pydantic does in Python
/// mode.
pub fn validate(m: &Model, v: &Value) -> Result<Value, ValidationError> {
    let (out, errs) = m.validate(v, &Loc::Root, Mode::Python);
    if errs.is_empty() {
        Ok(out)
    } else {
        Err(ValidationError {
            title: m.name.into(),
            errs,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Lines end at \n, \r\n and a lone \r; bad UTF-8 is replaced run by
    /// run; a last line with no end is kept.
    #[test]
    fn lines_are_universal_newlines() {
        let mut r = LineReader::new(&b"a\nb\r\nc\rd\xff\xfe\ne"[..]);
        let mut got = vec![];
        while let Some(l) = r.next_line() {
            got.push(l);
        }
        assert_eq!(got, ["a\n", "b\n", "c\n", "d\u{fffd}\u{fffd}\n", "e"]);
    }

    /// Only a JSON integer or a string is an id; anything else makes the
    /// message a notification, which gets no reply.
    #[test]
    fn classify_ids() {
        for (line, kind) in [
            (r#"{"jsonrpc":"2.0","id":1,"method":"ping"}"#, Kind::Request),
            (
                r#"{"jsonrpc":"2.0","id":"x","method":"ping"}"#,
                Kind::Request,
            ),
            (
                r#"{"jsonrpc":"2.0","id":1.0,"method":"ping"}"#,
                Kind::Notification,
            ),
            (
                r#"{"jsonrpc":"2.0","id":true,"method":"ping"}"#,
                Kind::Notification,
            ),
            (r#"{"jsonrpc":"2.0","method":"ping"}"#, Kind::Notification),
            (r#"{"jsonrpc":"1.0","id":1,"method":"ping"}"#, Kind::Invalid),
            (r#"{"jsonrpc":"2.0","id":1,"result":{}}"#, Kind::Invalid),
            (
                r#"{"jsonrpc":"2.0","id":1,"method":"x","params":1}"#,
                Kind::Invalid,
            ),
            ("[1]", Kind::Invalid),
            (
                r#"{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":"\ud800"}}"#,
                Kind::Invalid,
            ),
        ] {
            assert_eq!(classify(line).kind, kind, "{line}");
        }
    }

    /// pydantic's float text, and NaN as null (a line) or NaN (a text
    /// block).
    #[test]
    fn floats_as_pydantic_writes_them() {
        for (f, want) in [
            (1e16, "1e+16"),
            (1e-5, "0.00001"),
            (1.5e-7, "1.5e-7"),
            (100.0, "100.0"),
            (1e-6, "1e-6"),
            (0.1, "0.1"),
        ] {
            assert_eq!(compact(&Value::Float(f)), want, "{f}");
        }
        let o = Value::Obj(
            Object::new()
                .with("a", f64::NAN)
                .with("b", "é\u{0}\u{2028}"),
        );
        assert_eq!(compact(&o), "{\"a\":null,\"b\":\"é\\u0000\u{2028}\"}");
        assert_eq!(
            indent(&o),
            "{\n  \"a\": NaN,\n  \"b\": \"é\\u0000\u{2028}\"\n}"
        );
    }

    /// A str given for a dict field is read as JSON; a str field is left
    /// alone; nesting past what the binary reads is refused.
    #[test]
    fn pre_parse_reads_json_text() {
        let m = args("verify_plan").unwrap();
        let a = Object::new()
            .with("plan", "{\"a\": 1}")
            .with("envelope", "5");
        let out = pre_parse(m, &a).unwrap();
        assert!(matches!(out.value("plan"), Value::Obj(_)));
        assert_eq!(out.value("envelope"), &Value::Str("5".into()));
        let deep = format!("{}{}", "[".repeat(1000), "]".repeat(1000));
        assert!(pre_parse(m, &Object::new().with("plan", deep.as_str())).is_none());
        let m = args("find_pathway").unwrap();
        let out = pre_parse(m, &Object::new().with("task", "[1]")).unwrap();
        assert_eq!(out.value("task"), &Value::Str("[1]".into()));
    }
}

/// pydantic's `AnyUrl` on a URI: the URL as it serializes, or why it is
/// not one (`Invalid`: the SDK answers -32602; `Unported`: a form this
/// binary does not normalize the way the URL parser does). The Go
/// client's `mcpwire.NormURI` is the twin.
pub fn norm_uri(u: &str) -> Result<String, Checked> {
    let b = u.as_bytes();
    let Some(i) = u.find(':') else { return Err(Checked::Invalid) };
    if i == 0 || !b[0].is_ascii_alphabetic() {
        return Err(Checked::Invalid);
    }
    if !b[1..i].iter().all(|c| c.is_ascii_alphanumeric() || b"+-.".contains(c)) {
        return Err(Checked::Invalid);
    }
    if b.iter().any(|&c| c <= 0x20 || c >= 0x7f || b"%\\[]".contains(&c)) {
        return Err(Checked::Unported);
    }
    let scheme = u[..i].to_ascii_lowercase();
    let rest = &u[i + 1..];
    let port = match scheme.as_str() {
        "http" | "ws" => Some("80"),
        "https" | "wss" => Some("443"),
        "ftp" => Some("21"),
        _ => None,
    };
    if let Some(port) = port {
        let Some(rest) = rest.strip_prefix("//") else { return Err(Checked::Unported) };
        let end = rest.find(|c| "/?#".contains(c)).unwrap_or(rest.len());
        let (hostport, tail) = rest.split_at(end);
        if hostport.contains('@') {
            return Err(Checked::Unported);
        }
        let (host, p) = match hostport.split_once(':') {
            Some((h, p)) => (h, Some(p)),
            None => (hostport, None),
        };
        if host.is_empty() {
            return Err(Checked::Invalid);
        }
        if host.as_bytes()[0].is_ascii_digit() {
            return Err(Checked::Unported);
        }
        let mut host = host.to_ascii_lowercase();
        if let Some(p) = p {
            if p.is_empty() || !p.bytes().all(|c| c.is_ascii_digit()) || p.len() > 5 {
                return Err(Checked::Unported);
            }
            if p != port {
                host = format!("{host}:{p}");
            }
        }
        let path = tail.split(|c| c == '?' || c == '#').next().unwrap_or("");
        if path.split('/').any(|seg| seg == "." || seg == "..") {
            // The URL parser resolves dot segments.
            return Err(Checked::Unported);
        }
        let tail = if tail.starts_with('/') { tail.to_string() } else { format!("/{tail}") };
        return Ok(format!("{scheme}://{host}{tail}"));
    }
    if scheme == "file" && !rest.starts_with("///") {
        return Err(Checked::Unported);
    }
    Ok(format!("{scheme}:{rest}"))
}
