//! sprig-mcp: sprig's tools over the Model Context Protocol, JSON-RPC 2.0
//! on stdio. Every tools/call runs through the same gated executor.

use crate::gate::{Executor, ToolCall};
use crate::json::{encode, jmap, Map, J};
use std::io::{BufRead, Write};

pub const PROTOCOL_VERSION: &str = "2024-11-05";

pub struct Server {
    pub exec: Executor,
}

impl Server {
    /// The reply to one message, or None for a notification.
    pub fn handle(&self, raw: &[u8]) -> Option<Vec<u8>> {
        let mut method = String::new();
        let mut jsonrpc = String::new();
        let mut id: Option<Vec<u8>> = None;
        let mut params: Option<Vec<u8>> = None;
        let r = crate::json::unmarshal(raw, |d, n| {
            d.object(
                n,
                "rpcRequest",
                "mcp.rpcRequest",
                &["jsonrpc", "id", "method", "params"],
                |d, i, v| match i {
                    0 => d.string(v, &mut jsonrpc),
                    1 => id = Some(raw[v.start..v.end].to_vec()),
                    2 => d.string(v, &mut method),
                    _ => params = Some(raw[v.start..v.end].to_vec()),
                },
            )
        });
        if r.is_err() {
            return Some(rpc_error(None, -32700, "parse error"));
        }
        let notification = id.as_ref().is_none_or(|x| x.is_empty());
        match method.as_str() {
            "notifications/initialized" => None,
            "initialize" => Some(reply(id, initialize_result())),
            "tools/list" => Some(reply(id, self.tools_list())),
            "tools/call" => Some(reply(id, self.tools_call(params.as_deref().unwrap_or(b"")))),
            _ if notification => None,
            _ => Some(rpc_error(
                id,
                -32601,
                &format!("method not found: {method}"),
            )),
        }
    }

    /// Newline-delimited JSON-RPC in, one reply line per request out.
    /// Ok at the end of input; Err with Go's text on a read or write error.
    pub fn serve(&self, r: &mut dyn BufRead, w: &mut dyn Write) -> Result<(), String> {
        loop {
            let mut line = Vec::new();
            let got = r.read_until(b'\n', &mut line);
            let msg = crate::gostr::trim_space(&line);
            if !msg.is_empty() {
                if let Some(resp) = self.handle(msg) {
                    let mut out = resp;
                    out.push(b'\n');
                    w.write_all(&out).and_then(|_| w.flush()).map_err(|e| {
                        format!("write /dev/stdout: {}", crate::goerr::io_reason(&e))
                    })?;
                }
            }
            match got {
                Ok(0) => return Ok(()),
                Ok(_) if line.last() != Some(&b'\n') => return Ok(()),
                Ok(_) => {}
                Err(e) => return Err(format!("read /dev/stdin: {}", crate::goerr::io_reason(&e))),
            }
        }
    }
}

fn id_value(id: Option<Vec<u8>>) -> J {
    match id {
        Some(raw) if !raw.is_empty() => J::Raw(raw),
        _ => J::Null,
    }
}

fn reply(id: Option<Vec<u8>>, result: J) -> Vec<u8> {
    encode(&jmap(vec![
        ("jsonrpc", J::s("2.0")),
        ("id", id_value(id)),
        ("result", result),
    ]))
}

fn rpc_error(id: Option<Vec<u8>>, code: i64, msg: &str) -> Vec<u8> {
    encode(&jmap(vec![
        ("jsonrpc", J::s("2.0")),
        ("id", id_value(id)),
        (
            "error",
            jmap(vec![("code", J::Int(code)), ("message", J::s(msg))]),
        ),
    ]))
}

fn initialize_result() -> J {
    jmap(vec![
        ("protocolVersion", J::s(PROTOCOL_VERSION)),
        ("capabilities", jmap(vec![("tools", jmap(vec![]))])),
        (
            "serverInfo",
            jmap(vec![("name", J::s("sprig")), ("version", J::s("0.1.0"))]),
        ),
    ])
}

/// The input schema and description sprig gives each of its tools.
fn tool_schema(name: &str) -> Option<(J, &'static str)> {
    let obj = |required: &[&str], fields: &[&str]| {
        let props = fields
            .iter()
            .map(|f| (*f, jmap(vec![("type", J::s("string"))])))
            .collect();
        jmap(vec![
            ("type", J::s("object")),
            ("properties", jmap(props)),
            (
                "required",
                J::Arr(required.iter().map(|r| J::s(r)).collect()),
            ),
        ])
    };
    Some(match name {
        "read" => (obj(&["path"], &["path"]), "Return a file's contents."),
        "write" => (
            obj(&["path", "content"], &["path", "content"]),
            "Write content to a file (creates or overwrites).",
        ),
        "edit" => (
            obj(&["path", "old", "new"], &["path", "old", "new"]),
            "Replace a unique occurrence of old with new in a file.",
        ),
        "bash" => (
            obj(&["cmd"], &["cmd"]),
            "Run a shell command and return its output.",
        ),
        _ => return None,
    })
}

impl Server {
    fn tools_list(&self) -> J {
        let tools = self
            .exec
            .tool_names()
            .iter()
            .map(|n| {
                let (schema, desc) =
                    tool_schema(n).unwrap_or((jmap(vec![("type", J::s("object"))]), ""));
                jmap(vec![
                    ("name", J::s(n)),
                    ("description", J::s(desc)),
                    ("inputSchema", schema),
                ])
            })
            .collect();
        jmap(vec![("tools", J::Arr(tools))])
    }

    fn tools_call(&self, params: &[u8]) -> J {
        let mut name = String::new();
        let mut args: Option<Map> = None;
        let _ = crate::json::unmarshal(params, |d, n| {
            d.object(
                n,
                "",
                "struct { Name string; Arguments map[string]interface {} }",
                &["name", "arguments"],
                |d, i, v| match i {
                    0 => d.string(v, &mut name),
                    _ => d.map_any(v, &mut args),
                },
            )
        });
        let input = Some(args.unwrap_or_default());
        let res = self.exec.execute(&ToolCall {
            id: String::new(),
            name,
            input,
        });
        let (text, is_err) = if !res.allowed {
            (
                [b"REFUSED by the gate: ".as_slice(), &res.reason].concat(),
                true,
            )
        } else if let Some(e) = &res.err {
            let mut t = [b"error: ".as_slice(), e].concat();
            if !res.output.is_empty() {
                t.push(b'\n');
                t.extend_from_slice(&res.output);
            }
            (t, true)
        } else {
            (res.output.clone(), false)
        };
        jmap(vec![
            (
                "content",
                J::Arr(vec![jmap(vec![
                    ("type", J::s("text")),
                    ("text", J::Str(text)),
                ])]),
            ),
            ("isError", J::Bool(is_err)),
        ])
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::AllowAll;

    fn server() -> Server {
        Server {
            exec: Executor::new(crate::tools::default_tools(), Box::new(AllowAll)),
        }
    }

    fn h(s: &Server, raw: &str) -> Option<String> {
        s.handle(raw.as_bytes())
            .map(|b| String::from_utf8(b).unwrap())
    }

    #[test]
    fn requests_follow_go() {
        let s = server();
        assert_eq!(
            h(&s, "{bad"),
            Some(
                r#"{"error":{"code":-32700,"message":"parse error"},"id":null,"jsonrpc":"2.0"}"#
                    .into()
            )
        );
        assert_eq!(h(&s, r#"{"jsonrpc":"2.0","method":"x"}"#), None);
        assert_eq!(
            h(
                &s,
                r#"{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}"#
            ),
            None
        );
        assert_eq!(
            h(&s, r#"{"jsonrpc":"2.0","id": { "a" : [1, 2] , "b":"<x>"},"method":"x/y"}"#),
            Some(r#"{"error":{"code":-32601,"message":"method not found: x/y"},"id":{"a":[1,2],"b":"\u003cx\u003e"},"jsonrpc":"2.0"}"#.into())
        );
        assert_eq!(
            h(&s, r#"{"jsonrpc":"2.0","id":null,"method":"initialize"}"#),
            Some(r#"{"id":null,"jsonrpc":"2.0","result":{"capabilities":{"tools":{}},"protocolVersion":"2024-11-05","serverInfo":{"name":"sprig","version":"0.1.0"}}}"#.into())
        );
        assert_eq!(
            h(&s, r#"{"jsonrpc":5,"id":1,"method":"initialize"}"#).unwrap()[..30].to_string(),
            r#"{"error":{"code":-32700,"messa"#
        );
        let list = h(&s, r#"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#).unwrap();
        assert!(list.starts_with(r#"{"id":2,"jsonrpc":"2.0","result":{"tools":[{"description":"Run a shell command and return its output.","inputSchema":{"properties":{"cmd":{"type":"string"}},"required":["cmd"],"type":"object"},"name":"bash"}"#));
        let call = h(
            &s,
            r#"{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"cmd":"echo hi; exit 2"}}}"#,
        );
        assert_eq!(
            call.unwrap(),
            r#"{"id":3,"jsonrpc":"2.0","result":{"content":[{"text":"error: exit status 2\nhi\n","type":"text"}],"isError":true}}"#
        );
        let call = h(&s, r#"{"jsonrpc":"2.0","id":4,"method":"tools/call"}"#);
        assert_eq!(
            call.unwrap(),
            r#"{"id":4,"jsonrpc":"2.0","result":{"content":[{"text":"REFUSED by the gate: unknown tool: ","type":"text"}],"isError":true}}"#
        );
        let _ = (
            Map::new(),
            J::Null,
            encode(&jmap(vec![])),
            ToolCall::default(),
        );
    }

    #[test]
    fn serve_reads_lines() {
        let s = server();
        let input = "\n  \r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"zz\"}\r\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"zz\"}";
        let mut out = Vec::new();
        s.serve(&mut input.as_bytes(), &mut out).unwrap();
        let text = String::from_utf8(out).unwrap();
        assert_eq!(text.lines().count(), 2);
        assert!(text.ends_with("\"id\":2,\"jsonrpc\":\"2.0\"}\n"));
    }
}
