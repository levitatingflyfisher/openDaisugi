//! The part of `floor/coppice_backend.py` and `floor/registry.py` deliver
//! uses: pick coppice, then prompt_pane through it.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::os::unix::net::UnixStream;
use std::time::Duration;

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

/// Why a send failed: no pane backend (503), or anything else (500).
#[derive(Debug)]
pub enum SendError {
    FloorNotAvailable(String),
    Other(String),
}

/// registry._FIX["coppice"].
const COPPICE_FIX: &str =
    "build it: cd harness/coppice && mkdir -p build && go build -o build/coppice ./cmd/coppice, then run `coppice server start`";

#[derive(Clone)]
pub struct FloorConfig {
    pub backend: String,
    pub coppice_socket: Option<String>,
}

/// One call's failure: a socket error (OSError), or a reply problem
/// (CoppiceError), or a reply that is not an object.
enum CallErr {
    Os(String),
    Coppice(String),
    Other(String),
}

struct Coppice {
    sock: String,
    seq: u64,
}

/// `coppice_backend.default_socket_path`.
pub fn default_socket(home: &str) -> String {
    match std::env::var("XDG_RUNTIME_DIR") {
        Ok(rt) if !rt.is_empty() => format!("{rt}/coppice/server.sock"),
        _ => format!("{home}/.opendaisugi/coppice/server.sock"),
    }
}

impl Coppice {
    fn is_socket(&self) -> bool {
        match std::fs::symlink_metadata(&self.sock) {
            Ok(m) => m.file_type().is_socket() && m.uid() == unsafe { libc::getuid() },
            Err(_) => false,
        }
    }

    fn call(&mut self, cmd: &str, timeout: Duration, fields: Option<Object>) -> Result<Object, CallErr> {
        self.seq += 1;
        let mut req = Object::new();
        req.set("id", self.seq.to_string());
        req.set("cmd", cmd);
        if let Some(f) = fields {
            for (k, v) in f.iter() {
                req.set(k, v.clone());
            }
        }
        let mut conn = UnixStream::connect(&self.sock).map_err(|e| CallErr::Os(e.to_string()))?;
        let _ = conn.set_read_timeout(Some(timeout));
        let _ = conn.set_write_timeout(Some(timeout));
        let line = format!("{}\n", pyjson::dumps(&Value::Obj(req), true));
        conn.write_all(line.as_bytes()).map_err(|e| CallErr::Os(e.to_string()))?;
        let mut reply = vec![];
        let mut r = BufReader::new(conn);
        match r.read_until(b'\n', &mut reply) {
            Err(e) if reply.is_empty() => {
                if matches!(e.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut) {
                    return Err(CallErr::Os(e.to_string()));
                }
                return Err(CallErr::Coppice(format!("coppice closed the connection on {cmd}")));
            }
            Ok(0) => return Err(CallErr::Coppice(format!("coppice closed the connection on {cmd}"))),
            _ => {}
        }
        let v = match String::from_utf8(reply).ok().and_then(|t| pyjson::loads_py(&t, 900).ok()) {
            Some(v) => v,
            None => return Err(CallErr::Coppice("coppice sent a reply that is not JSON".into())),
        };
        let Value::Obj(o) = v else { return Err(CallErr::Other("the coppice reply is not an object".into())) };
        if !o.get("ok").map(|v| v.truthy()).unwrap_or(false) {
            return Err(CallErr::Coppice("coppice answered ok: false".into()));
        }
        match o.get("result") {
            Some(Value::Obj(r)) if !r.is_empty() => Ok(r.clone()),
            _ => Ok(Object::new()),
        }
    }

    fn available(&mut self) -> bool {
        self.is_socket() && self.call("server.status", Duration::from_millis(300), None).is_ok()
    }

    /// `registry.prompt_pane` with no foreman and no wait.
    fn prompt_pane(&mut self, pane: &str, text_: &str) -> Result<&'static str, SendError> {
        let mut kind = "pty".to_string();
        match self.call("pane.list", Duration::from_secs(5), None) {
            Ok(res) => {
                if let Some(Value::List(rows)) = res.get("panes") {
                    for r in rows {
                        let Value::Obj(row) = r else { return Err(SendError::Other("a pane row is not an object".into())) };
                        let Some(id) = row.get("id") else { return Err(SendError::Other("a pane row has no id".into())) };
                        if py_str(id) == pane {
                            kind = row.get("kind").map(py_str).unwrap_or_else(|| "pty".into());
                            break;
                        }
                    }
                }
            }
            Err(CallErr::Other(m)) => return Err(SendError::Other(m)),
            Err(_) => {}
        }
        let mut f = Object::new();
        f.set("pane", pane);
        f.set("text", text_);
        let (cmd, timeout, word) = if kind == "headless" {
            f.set("wait", false);
            f.set("timeout_ms", 60000i64);
            ("agent.prompt", Duration::from_secs(61), "prompted")
        } else {
            f.set("enter", true);
            ("pane.send_text", Duration::from_secs(5), "typed")
        };
        match self.call(cmd, timeout, Some(f)) {
            Ok(_) => Ok(word),
            Err(CallErr::Os(m)) | Err(CallErr::Coppice(m)) | Err(CallErr::Other(m)) => Err(SendError::Other(m)),
        }
    }
}

/// `str(v)` for a JSON value.
pub fn py_str(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        Value::Null => "None".into(),
        Value::Bool(true) => "True".into(),
        Value::Bool(false) => "False".into(),
        Value::Int(t) => t.clone(),
        Value::Float(f) => pyjson::float_repr(*f),
        other => pyjson::dumps(other, false),
    }
}

/// `registry.pick_backend` then `prompt_pane`, for this binary: coppice,
/// named or under auto; herdr and tmux are not carried (ruling VO-3).
pub fn send_through_floor(cfg: &FloorConfig, home: &str, pane: &str, text_: &str) -> Result<&'static str, SendError> {
    let name = cfg.backend.as_str();
    match name {
        "coppice" | "auto" => {}
        "herdr" | "tmux" => {
            return Err(SendError::FloorNotAvailable(format!(
                "{name} is not available: this daisugi delivers through coppice only. Set floor.backend to coppice or auto."
            )))
        }
        _ => {
            return Err(SendError::FloorNotAvailable(format!(
                "no pane backend named {}. Choose one of: coppice, herdr, tmux.",
                text::repr(name)
            )))
        }
    }
    let sock = match &cfg.coppice_socket {
        Some(s) if !s.is_empty() => s.clone(),
        _ => default_socket(home),
    };
    let mut c = Coppice { sock, seq: 0 };
    if !c.available() {
        if name == "auto" {
            return Err(SendError::FloorNotAvailable(
                "no pane backend is available. coppice: coppice did not answer. This daisugi delivers through coppice only. Start one: `coppice server start`.".into(),
            ));
        }
        return Err(SendError::FloorNotAvailable(format!("coppice is not available: coppice did not answer. {COPPICE_FIX}.")));
    }
    c.prompt_pane(pane, text_)
}
