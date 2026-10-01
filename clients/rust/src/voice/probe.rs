//! The voice-probe test instrument: one query of clients/voice_cases.py on
//! stdin, one JSON answer on stdout, as clients/voice_probe_oracle.py
//! answers it for the oracle. Not shipped.

use std::io::Read;
use std::os::unix::fs::PermissionsExt;

use num_bigint::BigInt;

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};
use crate::signing::{b64decode, b64encode};

use super::arm;
use super::audio::{to_wav_16k_mono, which};
use super::cleanup::{self, CleanupConfig, Transport};
use super::engine::{self, py_path_str};
use super::hardware;
use super::moonshine;
use super::parakeet;
use super::resident;
use super::ptt::{self, ClientErr, Stream, VoiceClient};
use super::server;
use super::wav;

fn o(kv: &[(&str, Value)]) -> Value {
    let mut ob = Object::new();
    for (k, v) in kv {
        ob.set(k, v.clone());
    }
    Value::Obj(ob)
}

fn s(v: impl Into<String>) -> Value {
    Value::Str(v.into())
}

fn err(class: &str, msg: &str) -> Value {
    o(&[("error", s(class)), ("message", s(msg))])
}

struct Q {
    q: Object,
    root: String,
}

impl Q {
    fn str(&self, k: &str) -> String {
        match self.q.get(k) {
            Some(Value::Str(x)) => x.clone(),
            _ => String::new(),
        }
    }

    fn opt(&self, k: &str) -> Option<String> {
        match self.q.get(k) {
            Some(Value::Str(x)) => Some(x.clone()),
            _ => None,
        }
    }

    fn num(&self, k: &str) -> f64 {
        match self.q.get(k) {
            Some(Value::Int(t)) => t.parse().unwrap_or(0.0),
            Some(Value::Float(f)) => *f,
            _ => 0.0,
        }
    }

    fn bytes(&self, k: &str) -> Vec<u8> {
        b64decode(&self.str(k)).unwrap_or_default()
    }

    fn armed(&self) -> String {
        format!("{}/armed", self.root)
    }

    fn lay(&self) -> std::io::Result<()> {
        let Some(Value::Obj(files)) = self.q.get("files") else { return Ok(()) };
        for (rel, spec) in files.iter() {
            let Value::Obj(spec) = spec else { continue };
            let path = format!("{}/{}", self.root, rel.trim_end_matches('/'));
            let mode = |def: u32| match spec.get("mode") {
                Some(Value::Int(t)) => t.parse().unwrap_or(def),
                _ => def,
            };
            if rel.ends_with('/') {
                std::fs::create_dir_all(&path)?;
                std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode(0o700)))?;
                continue;
            }
            if let Some(parent) = std::path::Path::new(&path).parent() {
                std::fs::create_dir_all(parent)?;
            }
            let data = match spec.get("b64") {
                Some(Value::Str(b)) => b64decode(b).unwrap_or_default(),
                _ => match spec.get("text") {
                    Some(Value::Str(t)) => t.as_bytes().to_vec(),
                    _ => vec![],
                },
            };
            std::fs::write(&path, data)?;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode(0o600)))?;
        }
        Ok(())
    }
}

/// `_tree`: every path under root with its mode, a file's text or base64.
fn tree(root: &str) -> Value {
    fn walk(dir: &std::path::Path, out: &mut Vec<std::path::PathBuf>) {
        let Ok(rd) = std::fs::read_dir(dir) else { return };
        for e in rd.flatten() {
            let p = e.path();
            out.push(p.clone());
            if std::fs::metadata(&p).map(|m| m.is_dir()).unwrap_or(false) {
                walk(&p, out);
            }
        }
    }
    let mut paths = vec![];
    walk(std::path::Path::new(root), &mut paths);
    paths.sort();
    let mut ob = Object::new();
    for p in paths {
        let rel = p.strip_prefix(root).map(|r| r.to_string_lossy().into_owned()).unwrap_or_default();
        let Ok(m) = std::fs::metadata(&p) else { continue };
        let mode = Value::Int((m.permissions().mode() & 0o777).to_string());
        if m.is_dir() {
            ob.set(&format!("{rel}/"), o(&[("mode", mode)]));
            continue;
        }
        let Ok(raw) = std::fs::read(&p) else { continue };
        match String::from_utf8(raw.clone()) {
            Ok(t) => ob.set(&rel, o(&[("mode", mode), ("text", s(t))])),
            Err(_) => ob.set(&rel, o(&[("mode", mode), ("b64", s(b64encode(&raw)))])),
        };
    }
    Value::Obj(ob)
}

/// The class Python raises for a file error.
fn os_class(e: &std::io::Error) -> &'static str {
    match e.raw_os_error() {
        Some(libc::ENOENT) => "FileNotFoundError",
        Some(libc::EEXIST) => "FileExistsError",
        Some(libc::ENOTDIR) => "NotADirectoryError",
        Some(libc::EISDIR) => "IsADirectoryError",
        Some(libc::EACCES) | Some(libc::EPERM) => "PermissionError",
        _ => "OSError",
    }
}

fn pins() -> Value {
    let names: Vec<Value> = engine::FASTER_WHISPER_MODEL_NAMES.iter().map(|n| s(*n)).collect();
    let (mut tr, mut ow, mut mw) = (Object::new(), Object::new(), Object::new());
    for (n, t, w, m) in engine::FIXTURES {
        tr.set(n, *t);
        ow.set(n, *w);
        mw.set(n, *m);
    }
    let mut pmodels = Object::new();
    for (name, m) in parakeet::PARAKEET_MODELS {
        let file = |f: &parakeet::ParakeetFile| Value::List(vec![s(f.name), s(f.sha256), Value::Int(f.size.to_string())]);
        pmodels.set(name, o(&[("dir", s(m.dir)), ("source", file(&m.source)), ("quantized", file(&m.quantized))]));
    }
    let (mut models, mut sizes) = (Object::new(), Object::new());
    for (name, files) in moonshine::MOONSHINE_MODELS {
        let rows = files.iter().map(|f| Value::List(vec![s(f.name), s(f.sha256), Value::Int(f.size.to_string())])).collect();
        models.set(name, Value::List(rows));
        sizes.set(name, Value::Int(moonshine::moonshine_size_mb(name).to_string()));
    }
    o(&[
        ("faster_whisper_model_names", Value::List(names)),
        ("faster_whisper_test_model", s(engine::FASTER_WHISPER_TEST_MODEL)),
        ("fixture_transcripts", Value::Obj(tr)),
        ("fixture_observed_wer", Value::Obj(ow)),
        ("fixture_max_wer", Value::Obj(mw)),
        ("fixture_mean_max_wer", Value::Float(engine::FIXTURE_MEAN_MAX_WER)),
        ("whisper_cpp_binary", s(engine::WHISPER_CPP_BINARY)),
        ("whisper_cpp_auto_language", s(engine::WHISPER_CPP_AUTO_LANGUAGE)),
        ("whisper_cpp_timeout_s", Value::Float(engine::WHISPER_CPP_TIMEOUT_S)),
        ("faster_whisper_revisions", o(&moonshine::FASTER_WHISPER_REVISIONS.iter().map(|(k, v)| (*k, s(*v))).collect::<Vec<_>>())),
        (
            "faster_whisper_sizes",
            o(&moonshine::FASTER_WHISPER_SIZES.iter().map(|(k, v)| (*k, Value::Int(v.to_string()))).collect::<Vec<_>>()),
        ),
        ("moonshine_binary", s(moonshine::MOONSHINE_BINARY)),
        ("moonshine_timeout_s", Value::Float(moonshine::MOONSHINE_TIMEOUT_S)),
        ("moonshine_default_model", s(moonshine::MOONSHINE_DEFAULT_MODEL)),
        ("moonshine_dir_arch", s(moonshine::MOONSHINE_DIR_ARCH)),
        ("moonshine_base_url", s(moonshine::MOONSHINE_BASE_URL)),
        ("moonshine_revision", s(moonshine::MOONSHINE_REVISION)),
        ("moonshine_models", Value::Obj(models)),
        ("moonshine_size_mb", Value::Obj(sizes)),
        ("parakeet_binary", s(parakeet::PARAKEET_BINARY)),
        ("parakeet_quantize_binary", s(parakeet::PARAKEET_QUANTIZE_BINARY)),
        ("parakeet_default_model", s(parakeet::PARAKEET_DEFAULT_MODEL)),
        ("parakeet_base_url", s(parakeet::PARAKEET_BASE_URL)),
        ("parakeet_quant", s(parakeet::PARAKEET_QUANT)),
        ("parakeet_quant_arches", Value::List(parakeet::PARAKEET_QUANT_ARCHES.iter().map(|a| s(*a)).collect())),
        ("parakeet_models", Value::Obj(pmodels)),
        ("valid_engine_names", s(engine::VALID_ENGINE_NAMES)),
        (
            "hardware",
            o(&[
                ("parakeet_min_ram_gb", Value::Float(hardware::PARAKEET_MIN_RAM_GB)),
                ("parakeet_min_cpus", Value::Int(hardware::PARAKEET_MIN_CPUS.to_string())),
                ("moonshine_min_ram_gb", Value::Float(hardware::MOONSHINE_MIN_RAM_GB)),
            ]),
        ),
        (
            "resident",
            o(&[
                ("protocol", s(resident::PROTOCOL)),
                ("max_frame_bytes", Value::Int(resident::MAX_FRAME_BYTES.to_string())),
                ("load_timeout_s", Value::Float(resident::LOAD_TIMEOUT_S)),
                ("clip_timeout_s", Value::Float(resident::CLIP_TIMEOUT_S)),
                ("stop_grace_s", Value::Float(resident::STOP_GRACE_S)),
            ]),
        ),
        ("cleanup_prompt", s(cleanup::CLEANUP_PROMPT)),
    ])
}

/// `op_resident`: a resident engine built, started, run through the
/// query's steps, then stopped.
fn resident_op(q: &Q) -> Value {
    let mut env = moonshine::ModelEnv::process();
    if q.q.get("load_timeout_s").is_some() {
        env.timeouts.load_s = q.num("load_timeout_s");
    }
    if q.q.get("clip_timeout_s").is_some() {
        env.timeouts.clip_s = q.num("clip_timeout_s");
    }
    let built = if q.str("engine") == "moonshine" {
        moonshine::new_moonshine(&q.str("model"), &env, &mut |_| {})
    } else {
        parakeet::new_parakeet(&q.str("model"), &env, &mut |_| {})
    };
    let e = match built.and_then(|e| e.start().map(|_| e)) {
        Ok(e) => e,
        Err(x) => return o(&[("start", err(x.class(), &x.msg))]),
    };
    let mut steps = vec![];
    let list = match q.q.get("steps") {
        Some(Value::List(l)) => l.clone(),
        _ => vec![],
    };
    for step in list {
        let Value::Obj(step) = step else { continue };
        if step.get("wait").is_some() {
            let sq = Q { q: step.clone(), root: String::new() };
            steps.push(o(&[("state", s(e.wait_ready(sq.num("wait"))))]));
            continue;
        }
        let clip = match step.get("clip") {
            Some(Value::Str(b)) => b64decode(b).unwrap_or_default(),
            _ => vec![],
        };
        steps.push(match e.transcribe(&clip, None) {
            Ok(t) => o(&[("text", s(t.text)), ("duration_s", Value::Float(t.duration_s))]),
            Err(engine::TranscribeError::Engine(x)) => err(x.class(), &x.msg),
            Err(engine::TranscribeError::Other(m)) => err("RuntimeError", &m),
        });
    }
    e.stop();
    o(&[("start", s("ok")), ("steps", Value::List(steps))])
}

struct Scripted {
    spec: Object,
    calls: Vec<Value>,
}

impl Transport for Scripted {
    fn complete(&mut self, model: &str, system: &str, user: &str) -> Result<(String, BigInt, BigInt), String> {
        self.calls.push(o(&[("model", s(model)), ("system", s(system)), ("user", s(user))]));
        if self.spec.get("raise").map(|v| v.truthy()).unwrap_or(false) {
            return Err("the transport failed".into());
        }
        let t = match self.spec.get("text") {
            Some(Value::Str(t)) => t.clone(),
            _ => String::new(),
        };
        let int = |k: &str| match self.spec.get("usage") {
            Some(Value::Obj(u)) => match u.get(k) {
                Some(Value::Int(t)) => t.parse().unwrap_or_default(),
                _ => BigInt::from(0),
            },
            _ => BigInt::from(0),
        };
        Ok((t, int("input_tokens"), int("output_tokens")))
    }
}

struct PStream {
    chunks: Vec<Value>,
    events: std::rc::Rc<std::cell::RefCell<Vec<Value>>>,
}

impl Stream for PStream {
    fn start(&mut self) -> Result<(), String> {
        self.events.borrow_mut().push(s("start"));
        Ok(())
    }

    fn stop(&mut self) -> Result<(), String> {
        self.events.borrow_mut().push(s("stop"));
        Ok(())
    }

    fn read(&mut self, frames: usize) -> Result<(Vec<u8>, bool), String> {
        self.events.borrow_mut().push(s(format!("read {frames}")));
        if self.chunks.is_empty() {
            return Ok((vec![], false));
        }
        let Value::List(c) = self.chunks.remove(0) else { return Ok((vec![], false)) };
        let b = match &c[0] {
            Value::Str(t) => b64decode(t).unwrap_or_default(),
            _ => vec![],
        };
        Ok((b, c[1].truthy()))
    }
}

struct PClient {
    transcribes: Vec<Value>,
    delivers: Vec<Value>,
    events: std::rc::Rc<std::cell::RefCell<Vec<Value>>>,
}

fn take(list: &mut Vec<Value>) -> Result<Value, ClientErr> {
    let Value::Obj(a) = list.remove(0) else { return Err(ClientErr::Other("bad script".into())) };
    if let Some(e) = a.get("error") {
        return Err(ClientErr::Server(super::coppice::py_str(e)));
    }
    Ok(a.get("reply").cloned().unwrap_or(Value::Null))
}

impl VoiceClient for PClient {
    fn transcribe(&mut self, wav: &[u8]) -> Result<Value, ClientErr> {
        self.events.borrow_mut().push(o(&[("transcribe", s(b64encode(wav)))]));
        take(&mut self.transcribes)
    }

    fn deliver(&mut self, pane: &str, text_: &str, mode: &str) -> Result<Value, ClientErr> {
        self.events.borrow_mut().push(o(&[("deliver", Value::List(vec![s(pane), s(text_), s(mode)]))]));
        take(&mut self.delivers)
    }
}

impl Q {
    fn list(&self, k: &str) -> Vec<Value> {
        match self.q.get(k) {
            Some(Value::List(l)) => l.clone(),
            _ => vec![],
        }
    }

    fn ptt(&self) -> Result<Value, String> {
        let events = std::rc::Rc::new(std::cell::RefCell::new(Vec::<Value>::new()));
        let mut printed = vec![];
        let mut streams = self.list("streams");
        let mut client = PClient { transcribes: self.list("transcribe"), delivers: self.list("deliver"), events: events.clone() };
        let keys: Vec<char> = self.str("keys").chars().collect();
        let mut i = 0;
        let mut next_key = || {
            let k = keys.get(i).copied();
            i += 1;
            k
        };
        let ev = events.clone();
        let mut open = || -> Result<Box<dyn Stream>, String> {
            ev.borrow_mut().push(s("open"));
            let chunks = if streams.is_empty() {
                vec![]
            } else {
                match streams.remove(0) {
                    Value::List(l) => l,
                    _ => vec![],
                }
            };
            Ok(Box::new(PStream { chunks, events: ev.clone() }))
        };
        let frames = match self.q.get("chunk_frames") {
            Some(Value::Int(t)) => t.parse().unwrap_or(1600),
            _ => 1600,
        };
        let mut print = |line: String| printed.push(s(line));
        let results = ptt::run_ptt(&self.str("pane"), &mut client, &mut next_key, &mut open, frames, &mut print)?;
        let rs: Vec<Value> = results.into_iter().map(|r| Value::List(vec![r.text, r.reply])).collect();
        let ev = events.borrow().clone();
        Ok(o(&[("results", Value::List(rs)), ("printed", Value::List(printed)), ("events", Value::List(ev))]))
    }
}

fn answer(q: &Q) -> Result<Value, String> {
    let op = q.str("op");
    Ok(match op.as_str() {
        "pins" => pins(),
        "prereq" => {
            let w = which(engine::WHISPER_CPP_BINARY).is_some();
            let m = which(moonshine::MOONSHINE_BINARY).is_some();
            let p = which(parakeet::PARAKEET_BINARY).is_some();
            o(&[
                ("faster_whisper_available", Value::Bool(false)),
                ("ffmpeg_available", Value::Bool(which("ffmpeg").is_some())),
                ("espeak_available", Value::Bool(which("espeak-ng").is_some() || which("espeak").is_some())),
                ("whisper_cpp_available", Value::Bool(w)),
                ("moonshine_available", Value::Bool(m)),
                ("parakeet_available", Value::Bool(p)),
                ("ok", Value::Bool(w || m || p)),
            ])
        }
        "engine_args" => {
            let args =
                engine::whisper_cpp_args(&q.str("binary"), &py_path_str(&q.str("model")), &q.str("wav_path"), q.opt("language").as_deref());
            o(&[("args", Value::List(args.into_iter().map(Value::Str).collect()))])
        }
        "engine_text" => o(&[("text", s(engine::whisper_cpp_text(&q.str("stdout"))))]),
        "pick_engine" => {
            if let Some(v) = q.opt("moonshine_base_url") {
                std::env::set_var(moonshine::MOONSHINE_BASE_URL_ENV, v);
            }
            if let Some(v) = q.opt("xdg_cache_home") {
                std::env::set_var("XDG_CACHE_HOME", v);
            }
            if let Some(v) = q.opt("parakeet_base_url") {
                std::env::set_var(parakeet::PARAKEET_BASE_URL_ENV, v);
            }
            if let Some(v) = q.opt("hardware") {
                std::env::set_var(hardware::HARDWARE_ENV, v);
            }
            let (eng, model) = (q.opt("voice_engine"), q.opt("voice_model"));
            let mut said: Vec<Value> = vec![];
            let env = moonshine::ModelEnv::process();
            let picked = engine::pick_engine(
                eng.as_deref().unwrap_or("faster-whisper"),
                model.as_deref().unwrap_or("tiny.en"),
                &env,
                &mut |line: &str| said.push(s(line)),
            );
            let mut ans = match picked {
                Ok(e) => {
                    let mut a = o(&[("engine", s(e.name))]);
                    if let Value::Obj(ob) = &mut a {
                        if e.name == "moonshine" || e.name == "parakeet" {
                            ob.set("model", s(e.model()));
                        }
                        if e.name == "moonshine" {
                            ob.set("arch", s(e.arch.clone()));
                        }
                        if !e.argv.is_empty() {
                            ob.set("argv", Value::List(e.argv.iter().cloned().map(Value::Str).collect()));
                        }
                    }
                    a
                }
                Err(e) => err(e.class(), &e.msg),
            };
            if !said.is_empty() {
                if let Value::Obj(ob) = &mut ans {
                    ob.set("said", Value::List(said));
                }
            }
            ans
        }
        "moonshine_args" => {
            let args = moonshine::moonshine_args(&q.str("binary"), &py_path_str(&q.str("model")), &q.str("arch"));
            o(&[("args", Value::List(args.into_iter().map(Value::Str).collect()))])
        }
        "parakeet_quant_refusal" => match parakeet::parakeet_quant_refusal(&q.str("name"), &q.str("arch")) {
            Some(why) => o(&[("refusal", s(&why))]),
            None => o(&[("refusal", Value::Null)]),
        },
        "parakeet_args" => {
            let args = parakeet::parakeet_args(&q.str("binary"), &py_path_str(&q.str("model")));
            o(&[("args", Value::List(args.into_iter().map(Value::Str).collect()))])
        }
        "resident" => resident_op(&q),
        "choose_engine" => {
            let ram_gb = match q.q.get("ram_gb") {
                Some(Value::Null) | None => None,
                _ => Some(q.num("ram_gb")),
            };
            let hw = hardware::VoiceHardware { ram_gb, cpus: q.num("cpus") as i64, vram_gb: q.num("vram_gb") };
            let installed: Vec<String> = match q.q.get("installed") {
                Some(Value::List(l)) => l.iter().filter_map(|v| if let Value::Str(x) = v { Some(x.clone()) } else { None }).collect(),
                _ => vec![],
            };
            let refs: Vec<&str> = installed.iter().map(|x| x.as_str()).collect();
            let fw = matches!(q.q.get("faster_whisper"), Some(Value::Bool(true)));
            // Usable on a pinned arch, or where a file that matches the pin is on disk.
            let arch = match q.q.get("arch") {
                Some(Value::Str(a)) => a.clone(),
                _ => "x86_64".to_string(),
            };
            let on_disk = matches!(q.q.get("q4k_on_disk"), Some(Value::Bool(true)));
            let ok = parakeet::parakeet_quant_refusal("v2", &arch).is_none() || on_disk;
            let (e, m, line) = hardware::choose_engine(&hw, &refs, fw, ok);
            let order = hardware::hardware_order(&hw).into_iter().map(s).collect();
            o(&[("order", Value::List(order)), ("engine", s(e)), ("model", s(m)), ("line", s(line))])
        }
        "resident_line" => {
            let ob = resident::parse_line(&q.bytes("line_b64"));
            let kind = match resident::reply_kind(ob.as_ref()) {
                Some(k) => s(k),
                None => Value::Null,
            };
            o(&[("ready", Value::Bool(resident::is_ready(ob.as_ref()))), ("kind", kind)])
        }
        "fetch_file" => {
            let size = match q.q.get("size") {
                Some(Value::Int(t)) => t.parse::<u64>().ok(),
                _ => None,
            };
            let dest = py_path_str(&q.str("dest"));
            let mut ans = match moonshine::fetch_file(&q.str("url"), &q.str("sha256"), &dest, size, &moonshine::ModelEnv::process()) {
                Ok(()) => o(&[("ok", Value::Bool(true))]),
                Err(e) if e.verify => o(&[("error", s("verify"))]),
                Err(_) => o(&[("error", s("download"))]),
            };
            let dir = std::path::Path::new(&dest).parent().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
            let mut files = Object::new();
            for (n, d) in moonshine::fetch_listing(&dir) {
                files.set(&n, s(d));
            }
            if let Value::Obj(ob) = &mut ans {
                ob.set("files", Value::Obj(files));
            }
            ans
        }
        "transcribe" => match engine::new_whisper_cpp(&py_path_str(&q.str("model"))) {
            Err(e) => err(e.class(), &e.msg),
            Ok(e) => match e.transcribe(&q.bytes("wav_b64"), q.opt("language").as_deref()) {
                Err(engine::TranscribeError::Engine(x)) => err(x.class(), &x.msg),
                Err(engine::TranscribeError::Other(m)) => err("RuntimeError", &m),
                Ok(t) => {
                    let seg = o(&[("start", Value::Float(0.0)), ("end", Value::Float(t.duration_s)), ("text", s(t.text.clone()))]);
                    o(&[("text", s(t.text)), ("segments", Value::List(vec![seg])), ("duration_s", Value::Float(t.duration_s))])
                }
            },
        },
        "wav_duration" => match wav::wav_duration_s(&q.bytes("wav_b64")) {
            Ok(d) => o(&[("seconds", Value::Float(d))]),
            Err(_) => o(&[("error", s("unreadable"))]),
        },
        "to_wav" => match to_wav_16k_mono(&q.bytes("raw_b64")) {
            Ok(w) => o(&[("wav_b64", s(b64encode(&w)))]),
            Err(e) if e.bad_audio() => o(&[("error", s("bad_audio"))]),
            Err(_) => o(&[("error", s("internal"))]),
        },
        "multipart" => match arm::extract_multipart_audio(&q.bytes("body_b64"), &q.str("content_type")) {
            Ok((p, t)) => o(&[("payload_b64", s(b64encode(&p))), ("part_type", s(t))]),
            Err(m) => err("ValueError", &m),
        },
        "content_length" => {
            let n = match q.q.get("value") {
                None | Some(Value::Null) => Some(BigInt::from(0)),
                Some(Value::Str(v)) => super::pynum::py_int(v),
                _ => None,
            };
            match n.filter(|n| *n >= BigInt::from(0)) {
                Some(n) => o(&[("length", Value::Int(n.to_string()))]),
                None => o(&[("length", Value::Null)]),
            }
        }
        "is_loopback" => o(&[("loopback", Value::Bool(server::is_loopback(&q.str("address"))))]),
        "armed_name" => o(&[("name", s(arm::armed_name(&q.str("pane_key"))))]),
        "arm" => {
            q.lay().map_err(|e| e.to_string())?;
            let now_raw = q.q.get("now").cloned();
            let mut ans = match arm::arm(&q.str("pane_key"), q.num("minutes"), &q.armed(), q.num("now"), now_raw.clone()) {
                Ok(e) => {
                    let entry = o(&[
                        ("pane_key", s(e.pane_key)),
                        ("armed_at", now_raw.unwrap_or(Value::Float(e.armed_at))),
                        ("expires_at", Value::Float(e.expires_at)),
                    ]);
                    o(&[("entry", entry)])
                }
                Err(e) => o(&[("error", s(os_class(&e)))]),
            };
            if let Value::Obj(a) = &mut ans {
                a.set("tree", tree(&q.root));
            }
            ans
        }
        "disarm" => {
            q.lay().map_err(|e| e.to_string())?;
            let mut ans = match arm::disarm(&q.str("pane_key"), &q.armed()) {
                Ok(r) => o(&[("removed", Value::Bool(r))]),
                Err(e) => o(&[("error", s(os_class(&e)))]),
            };
            if let Value::Obj(a) = &mut ans {
                a.set("tree", tree(&q.root));
            }
            ans
        }
        "is_armed" => {
            q.lay().map_err(|e| e.to_string())?;
            o(&[("armed", Value::Bool(arm::is_armed(&q.str("pane_key"), &q.armed(), q.num("now"))))])
        }
        "deliver" => {
            q.lay().map_err(|e| e.to_string())?;
            let mut calls = vec![];
            let pane = q.str("pane");
            let res = arm::deliver(&pane, &q.str("text"), &q.str("mode"), &q.armed(), q.opt("pane_key").as_deref(), q.num("now"), |t| {
                calls.push(s("factory"));
                calls.push(s(format!("send {pane} {t}")));
                Ok::<(), ()>(())
            })
            .map_err(|_| "send failed".to_string())?;
            o(&[
                ("delivered", s(res.delivered)),
                ("reason", res.reason.map(Value::Str).unwrap_or(Value::Null)),
                ("calls", Value::List(calls)),
            ])
        }
        "cleanup" => {
            q.lay().map_err(|e| e.to_string())?;
            let mut cfg = CleanupConfig { on: false, model: None, base_url: None, data_dir: format!("{}/data", q.root) };
            if let Some(Value::Obj(c)) = q.q.get("config") {
                cfg.on = c.get("voice_cleanup").map(|v| v.truthy()).unwrap_or(false);
                if let Some(Value::Str(m)) = c.get("voice_cleanup_model") {
                    cfg.model = Some(m.clone());
                }
            }
            let spec = match q.q.get("transport") {
                Some(Value::Obj(t)) => t.clone(),
                _ => Object::new(),
            };
            let mut tr = Scripted { spec, calls: vec![] };
            let now_secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0);
            let r = cleanup::clean_transcript(&q.str("text"), &cfg, &mut tr, now_secs)?;
            let mut journal = vec![];
            if let Ok(raw) = std::fs::read_to_string(format!("{}/gateway/turns.jsonl", cfg.data_dir)) {
                for ln in text::splitlines(&raw) {
                    let mut v = pyjson::loads_py(ln, 900)?;
                    if let Value::Obj(ob) = &mut v {
                        let c = ob.get("created_at").cloned().unwrap_or(Value::Null);
                        ob.set("created_at", if c.truthy() { s("<created_at>") } else { c });
                    }
                    journal.push(v);
                }
            }
            o(&[
                ("text", s(r.text)),
                ("cleaned", Value::Bool(r.cleaned)),
                ("reason", r.reason.map(Value::Str).unwrap_or(Value::Null)),
                ("calls", Value::List(tr.calls)),
                ("journal", Value::List(journal)),
            ])
        }
        "ptt" => q.ptt()?,
        "client" => {
            q.lay().map_err(|e| e.to_string())?;
            let res = ptt::HttpClient::new(&q.str("url"), &format!("{}/data", q.root), std::time::Duration::from_secs(5)).and_then(|mut c| {
                if q.str("call") == "transcribe" {
                    c.transcribe(&q.bytes("wav_b64"))
                } else {
                    c.deliver(&q.str("pane"), &q.str("text"), &q.str("mode"))
                }
            });
            match res {
                Ok(v) => o(&[("reply", v)]),
                Err(ClientErr::Server(m)) => err("VoiceServerError", &m),
                Err(ClientErr::Other(m)) => return Err(m),
            }
        }
        other => return Err(format!("unknown op {other}")),
    })
}

/// Runs one probe query; the process exit code.
pub fn main() -> i32 {
    let mut raw = vec![];
    if std::io::stdin().read_to_end(&mut raw).is_err() {
        return 2;
    }
    let Ok(t) = String::from_utf8(raw) else { return 2 };
    let Ok(Value::Obj(q)) = pyjson::loads_py(&t, 900) else { return 2 };
    let q = Q { q, root: std::env::var("VOICE_PROBE_DIR").unwrap_or_default() };
    match answer(&q) {
        Ok(v) => {
            println!("{}", pyjson::dumps(&v, false));
            0
        }
        Err(m) => {
            eprintln!("{m}");
            1
        }
    }
}
