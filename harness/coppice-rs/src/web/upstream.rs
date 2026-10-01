//! The only door to the coppice server: a unix socket the caller names.
//! A Session is one client's connection; Call runs one request on a fresh
//! connection and waits for the reply that echoes its id.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::Value;

use crate::godec::Any;

/// The longest reply line read from the server.
pub const MAX_LINE_BYTES: usize = 4 << 20;
/// The longest request line, counting its newline.
pub const MAX_REQUEST_BYTES: usize = 1 << 20;

/// The socket path.
#[derive(Clone, Debug)]
pub struct Dialer {
    pub path: PathBuf,
}

/// One connection to the server, with its lines read on a thread.
pub struct Session {
    up: UnixStream,
    writer: Mutex<UnixStream>,
    pub lines: Receiver<Vec<u8>>,
    pub err: Arc<Mutex<Option<String>>>,
}

impl Dialer {
    /// Opens a session.
    pub fn open(&self) -> Result<Session, String> {
        let up = UnixStream::connect(&self.path).map_err(|e| {
            format!(
                "dial unix {}: connect: {}",
                self.path.display(),
                crate::sys::go_errno_text(&e)
            )
        })?;
        let reader = up.try_clone().map_err(|e| e.to_string())?;
        let writer = up.try_clone().map_err(|e| e.to_string())?;
        let (tx, rx) = mpsc::sync_channel::<Vec<u8>>(64);
        let err = Arc::new(Mutex::new(None));
        let err2 = err.clone();
        std::thread::spawn(move || {
            let mut r = BufReader::new(reader);
            loop {
                let mut line = Vec::new();
                match r
                    .by_ref()
                    .take(MAX_LINE_BYTES as u64 + 1)
                    .read_until(b'\n', &mut line)
                {
                    Ok(0) => break,
                    Ok(_) => {
                        if line.last() == Some(&b'\n') {
                            line.pop();
                        } else if line.len() > MAX_LINE_BYTES {
                            *err2.lock().unwrap_or_else(|e| e.into_inner()) =
                                Some("bufio.Scanner: token too long".into());
                            break;
                        }
                        if line.last() == Some(&b'\r') {
                            line.pop();
                        }
                        if tx.send(line).is_err() {
                            break;
                        }
                    }
                    Err(e) => {
                        *err2.lock().unwrap_or_else(|e| e.into_inner()) = Some(e.to_string());
                        break;
                    }
                }
            }
        });
        Ok(Session {
            up,
            writer: Mutex::new(writer),
            lines: rx,
            err,
        })
    }
}

impl Session {
    /// Writes one line; the newline is added here. A line at or over the
    /// server's limit is refused before anything is written.
    pub fn send(&self, line: &[u8]) -> Result<(), String> {
        let n = line.len() + 1;
        if n >= MAX_REQUEST_BYTES {
            return Err(format!(
                "that request line is {n} bytes, at or over the server's {MAX_REQUEST_BYTES} byte limit"
            ));
        }
        let mut out = line.to_vec();
        out.push(b'\n');
        let mut w = self.writer.lock().unwrap_or_else(|e| e.into_inner());
        w.write_all(&out).map_err(|e| e.to_string())
    }

    /// Ends the session; the reader thread then ends its channel.
    pub fn close(&self) {
        let _ = self.up.shutdown(std::net::Shutdown::Both);
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        self.close();
    }
}

/// A refusal from the server: its code and message.
#[derive(Debug)]
pub enum CallError {
    Refused { code: String, message: String },
    Failed(String),
}

impl std::fmt::Display for CallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CallError::Refused { code, message } => {
                if code.is_empty() {
                    f.write_str("coppice-server refused the request")
                } else {
                    write!(f, "{code}: {message}")
                }
            }
            CallError::Failed(s) => f.write_str(s),
        }
    }
}

/// The refusal in an {"ok":false} reply.
pub fn refusal(msg: &Any) -> CallError {
    let body = msg.get("error");
    let code = body.map(|b| b.str_of("code")).unwrap_or_default();
    let message = body.map(|b| b.str_of("message")).unwrap_or_default();
    if code.is_empty() && message.is_empty() {
        return CallError::Refused {
            code: "internal".into(),
            message: "coppice-server refused and said nothing".into(),
        };
    }
    CallError::Refused { code, message }
}

static CALL_SEQ: AtomicU64 = AtomicU64::new(0);

/// Runs one request on a fresh connection and returns the reply that
/// echoes its id, read as Go reads it into a map[string]any.
pub fn call(d: &Dialer, req: Vec<(&str, Value)>) -> Result<Any, CallError> {
    let s = d.open().map_err(CallError::Failed)?;
    let id = format!("api-{}", CALL_SEQ.fetch_add(1, Ordering::SeqCst) + 1);
    let mut pairs = req;
    pairs.retain(|(k, _)| *k != "id");
    pairs.push(("id", Value::String(id.clone())));
    let body = crate::gojson::marshal(&crate::gojson::map(pairs));
    s.send(body.as_bytes()).map_err(CallError::Failed)?;
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let left = deadline.saturating_duration_since(Instant::now());
        match s.lines.recv_timeout(left) {
            Err(RecvTimeoutError::Timeout) => {
                return Err(CallError::Failed(
                    "coppice-server did not answer in 5 s".into(),
                ))
            }
            Err(RecvTimeoutError::Disconnected) => {
                let e = s.err.lock().unwrap_or_else(|e| e.into_inner()).clone();
                return Err(CallError::Failed(
                    e.unwrap_or_else(|| "coppice-server closed the connection".into()),
                ));
            }
            Ok(line) => {
                let Ok(msg) = crate::godec::decode_map(&line) else {
                    continue;
                };
                match msg.get("id") {
                    Some(Any::Str(x)) if *x == id => {}
                    _ => continue,
                }
                if !matches!(msg.get("ok"), Some(Any::Bool(true))) {
                    return Err(refusal(&msg));
                }
                return Ok(msg);
            }
        }
    }
}
