//! `voice/ptt.py`: push-to-talk as a state machine (keys in, actions out),
//! `record_and_send`, and the HTTP client of the voice server.

use std::time::Duration;

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};
use crate::gateway::pyops;

use super::coppice::py_str;
use super::wav::write_wav_pcm;

/// What a client call can end in: a sentence for a person
/// (VoiceServerError), or a failure the oracle does not catch.
#[derive(Debug)]
pub enum ClientErr {
    Server(String),
    Other(String),
}

pub trait Stream {
    fn start(&mut self) -> Result<(), String>;
    fn stop(&mut self) -> Result<(), String>;
    /// The next chunk, and whether more is waiting.
    fn read(&mut self, frames: usize) -> Result<(Vec<u8>, bool), String>;
}

pub trait VoiceClient {
    fn transcribe(&mut self, wav: &[u8]) -> Result<Value, ClientErr>;
    fn deliver(&mut self, pane: &str, text_: &str, mode: &str) -> Result<Value, ClientErr>;
}

/// One press and release: the text and the delivery answer.
pub struct PttResult {
    pub text: Value,
    pub reply: Value,
}

fn get(v: &Value, k: &str, def: Value) -> Result<Value, ClientErr> {
    match v {
        Value::Obj(o) => Ok(o.get(k).cloned().unwrap_or(def)),
        other => Err(ClientErr::Other(format!("'{}' object has no attribute 'get'", pyjson::py_type_name(other)))),
    }
}

fn skipped(reason: &str) -> Value {
    let mut o = Object::new();
    o.set("delivered", "skipped");
    o.set("reason", reason);
    Value::Obj(o)
}

/// `ptt.record_and_send`.
pub fn record_and_send(
    frames: &[Vec<u8>],
    pane: &str,
    client: &mut dyn VoiceClient,
    sample_rate: i64,
    print: &mut dyn FnMut(String),
) -> Result<PttResult, ClientErr> {
    if frames.is_empty() {
        print("No audio was captured. Record for longer before tapping space again.".into());
        return Ok(PttResult { text: Value::Str(String::new()), reply: skipped("no audio was captured") });
    }
    let wav = write_wav_pcm(&frames.concat(), sample_rate).map_err(|e| ClientErr::Other(e.msg))?;
    let result = client.transcribe(&wav)?;
    let text_ = get(&result, "text", Value::Str(String::new()))?;
    let raw = get(&result, "raw_text", text_.clone())?;
    if !pyops::py_equal(&raw, &text_) {
        print(format!("Heard: {}", py_str(&raw)));
    }
    if !text_.truthy() {
        print("No speech was heard. Record for longer before tapping space again.".into());
        return Ok(PttResult { text: text_, reply: skipped("no speech was heard") });
    }
    print(py_str(&text_));
    let reply = client.deliver(pane, &py_str(&text_), "preview")?;
    let mut line = py_str(&get(&reply, "delivered", Value::Str("?".into()))?);
    let reason = get(&reply, "reason", Value::Null)?;
    if reason.truthy() {
        line = format!("{line}. {}", py_str(&reason));
    }
    print(line);
    Ok(PttResult { text: text_, reply })
}

/// `ptt.run_ptt`: space starts a recording and stops it, q quits. A cycle
/// the server fails prints its sentence and the session goes on.
pub fn run_ptt(
    pane: &str,
    client: &mut dyn VoiceClient,
    keys: &mut dyn FnMut() -> Option<char>,
    open: &mut dyn FnMut() -> Result<Box<dyn Stream>, String>,
    chunk_frames: usize,
    print: &mut dyn FnMut(String),
) -> Result<Vec<PttResult>, String> {
    let mut results = vec![];
    let mut recording = false;
    let mut stream: Option<Box<dyn Stream>> = None;
    let mut frames: Vec<Vec<u8>> = vec![];
    while let Some(ch) = keys() {
        if ch == 'q' {
            if recording {
                if let Some(s) = stream.as_mut() {
                    s.stop()?;
                }
            }
            break;
        }
        if ch == ' ' && !recording {
            recording = true;
            frames.clear();
            let mut s = open()?;
            s.start()?;
            stream = Some(s);
        } else if ch == ' ' && recording {
            recording = false;
            let s = stream.as_mut().expect("a recording has a stream");
            let mut more = true;
            while more {
                let (chunk, m) = s.read(chunk_frames)?;
                more = m;
                if !chunk.is_empty() {
                    frames.push(chunk);
                }
            }
            s.stop()?;
            match record_and_send(&frames, pane, client, 16000, print) {
                Ok(r) => results.push(r),
                Err(ClientErr::Server(m)) => print(m),
                Err(ClientErr::Other(m)) => return Err(m),
            }
        }
    }
    Ok(results)
}

pub fn token_file(data_dir: &str) -> String {
    format!("{data_dir}/coppice/web/token")
}

pub fn armed_dir(data_dir: &str) -> String {
    format!("{data_dir}/voice/armed")
}

/// `server._read_token`: Ok(None) when missing, unreadable or blank; Err
/// when the file is not UTF-8 (the oracle raises there).
pub fn read_token(path: &str) -> Result<Option<String>, String> {
    let Ok(raw) = std::fs::read(path) else { return Ok(None) };
    let t = String::from_utf8(raw).map_err(|e| {
        let b = e.as_bytes();
        let pos = e.utf8_error().valid_up_to();
        format!("'utf-8' codec can't decode byte 0x{:02x} in position {pos}: invalid start byte", b[pos])
    })?;
    let s = text::strip(&t);
    Ok((!s.is_empty()).then(|| s.to_string()))
}

/// `ptt.HttpVoiceClient`.
pub struct HttpClient {
    url: String,
    token: String,
    timeout: Duration,
}

impl HttpClient {
    /// Reads the token file under data_dir; refuses with a sentence when it
    /// cannot.
    pub fn new(server_url: &str, data_dir: &str, timeout: Duration) -> Result<HttpClient, ClientErr> {
        let tf = token_file(data_dir);
        match read_token(&tf) {
            Err(e) => Err(ClientErr::Other(e)),
            Ok(None) => Err(ClientErr::Server(format!(
                "No token file at {tf}. Run coppice web token on the box, or pass --token-file."
            ))),
            Ok(Some(token)) => Ok(HttpClient { url: server_url.trim_end_matches('/').to_string(), token, timeout }),
        }
    }

    fn unreachable(&self) -> ClientErr {
        ClientErr::Server(format!("Could not reach the voice server at {}. Run daisugi voice serve first.", self.url))
    }

    /// One POST, over TCP or TLS, with no proxy.
    fn post(&self, path: &str, ctype: &str, body: &[u8], deliver: bool) -> Result<Value, ClientErr> {
        let headers = [("Content-Type", ctype.to_string()), ("Authorization", format!("Bearer {}", self.token))];
        let url = format!("{}{path}", self.url);
        let ans = match crate::llm::http::post(&url, &headers, body, self.timeout.as_secs_f64(), &crate::netproxy::Httpx::none()) {
            Ok(a) => a,
            Err(crate::llm::http::Fail::Timeout) => return Err(ClientErr::Other("timed out".into())),
            Err(crate::llm::http::Fail::Unported(w)) => return Err(ClientErr::Other(w)),
            Err(_) => return Err(self.unreachable()),
        };
        let status = ans.status;
        let parsed = pyops::loads_text(&ans.text);
        if (200..300).contains(&status) {
            return parsed.map_err(|_| ClientErr::Other("the answer is not JSON".into()));
        }
        let obj = match parsed {
            Ok(Value::Obj(o)) => Some(o),
            _ => None,
        };
        if deliver {
            if let Some(o) = &obj {
                if o.get("delivered").is_some() {
                    return Ok(Value::Obj(o.clone()));
                }
            }
        }
        let mut msg = format!("The voice server answered {status}.");
        if let Some(o) = obj {
            let m = o.get("message").filter(|v| v.truthy()).or_else(|| o.get("error").filter(|v| v.truthy()));
            if let Some(m) = m {
                msg = py_str(m);
            }
        }
        Err(ClientErr::Server(msg))
    }
}

impl VoiceClient for HttpClient {
    fn transcribe(&mut self, wav: &[u8]) -> Result<Value, ClientErr> {
        self.post("/transcribe", "audio/wav", wav, false)
    }

    fn deliver(&mut self, pane: &str, text_: &str, mode: &str) -> Result<Value, ClientErr> {
        let mut o = Object::new();
        o.set("pane", pane);
        o.set("text", text_);
        o.set("mode", mode);
        self.post("/deliver", "application/json", pyjson::dumps(&Value::Obj(o), true).as_bytes(), true)
    }
}
