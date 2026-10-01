//! `opendaisugi.pack.client`: start the worker in the pack's Python, send
//! one job, stream its progress, and turn every way it can end into an
//! Outcome. Nothing the worker does panics here.

use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::gate::pyjson::{dumps, Object, Value};
use crate::voice::resident::{exit_reason, frame, parse_line};

use super::{PROTOCOL, WORKER_FILE};

const LOAD_TIMEOUT: Duration = Duration::from_secs(300);
const STOP_GRACE: Duration = Duration::from_secs(2);
const STDERR_LINES: usize = 20;

/// `client.Outcome`: code 0 with a result, or 1 with an error.
#[derive(Debug)]
pub struct Outcome {
    pub code: i32,
    pub result: Option<Object>,
    pub error: Option<String>,
}

fn failed(msg: String) -> Outcome {
    Outcome {
        code: 1,
        result: None,
        error: Some(msg),
    }
}

/// `client.worker_argv`.
pub fn worker_argv(pack_dir: &Path, name: &str) -> Vec<String> {
    vec![
        pack_dir
            .join("venv/bin/python")
            .to_string_lossy()
            .into_owned(),
        "-I".into(),
        pack_dir
            .join("worker")
            .join(WORKER_FILE)
            .to_string_lossy()
            .into_owned(),
        "--pack".into(),
        name.into(),
    ]
}

/// `worker.request`: the frame of one job.
pub fn request(job: &str, args: &[String]) -> Vec<u8> {
    let list = Value::List(args.iter().map(|a| Value::Str(a.clone())).collect());
    let body = dumps(
        &Value::Obj(Object::new().with("job", job).with("args", list)),
        true,
    );
    frame(body.as_bytes())
}

/// `worker.reply_kind`.
pub fn reply_kind(o: Option<&Object>) -> Option<&'static str> {
    let o = o?;
    let is_str = |k: &str| matches!(o.get(k), Some(Value::Str(_)));
    if is_str("ready") {
        return Some("ready");
    }
    if is_str("progress") {
        return Some("progress");
    }
    if matches!(o.get("result"), Some(Value::Obj(_))) {
        return Some("result");
    }
    if is_str("error") {
        return Some("error");
    }
    None
}

fn str_of(o: &Object, k: &str) -> String {
    o.get(k)
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

fn wait_code(child: &mut Child) -> i32 {
    use std::os::unix::process::ExitStatusExt;
    match child.wait() {
        Ok(st) => st.code().unwrap_or_else(|| -st.signal().unwrap_or(0)),
        Err(_) => 0,
    }
}

fn head_chars(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// `client.run_job`. `env` None is this process's environment.
pub fn run_job(
    pack_dir: &Path,
    name: &str,
    job: &str,
    args: &[String],
    on_progress: &mut dyn FnMut(&str),
    env: &Option<Vec<(String, String)>>,
) -> Outcome {
    let argv = worker_argv(pack_dir, name);
    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(vars) = env {
        cmd.env_clear().envs(vars.iter().map(|(k, v)| (k, v)));
    }
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(_) => {
            return failed(format!(
                "pack {name}: cannot start the worker ({})",
                argv[0]
            ))
        }
    };
    let tail = Arc::new(Mutex::new(Vec::<String>::new()));
    let stderr = child.stderr.take().expect("piped");
    let t2 = tail.clone();
    let err_thread = std::thread::spawn(move || {
        let mut r = BufReader::new(stderr);
        let mut buf = vec![];
        loop {
            buf.clear();
            match r.read_until(b'\n', &mut buf) {
                Ok(0) | Err(_) => break,
                Ok(_) => {
                    let text = String::from_utf8_lossy(&buf);
                    let text = text.strip_suffix('\n').unwrap_or(&text).to_string();
                    if !text.trim().is_empty() {
                        let mut t = t2.lock().unwrap();
                        t.push(text);
                        if t.len() > STDERR_LINES {
                            t.remove(0);
                        }
                    }
                }
            }
        }
    });
    let stdout = child.stdout.take().expect("piped");
    let (tx, rx) = mpsc::channel::<Vec<u8>>();
    std::thread::spawn(move || {
        let mut r = BufReader::new(stdout);
        loop {
            let mut buf = vec![];
            match r.read_until(b'\n', &mut buf) {
                Ok(0) | Err(_) => break,
                Ok(_) => {
                    if tx.send(buf).is_err() {
                        break;
                    }
                }
            }
        }
    });
    let mut stdin = child.stdin.take();

    let dead = |child: &mut Child, err_thread: std::thread::JoinHandle<()>| -> Outcome {
        // Read stderr to its end before naming the last line.
        let (done_tx, done_rx) = mpsc::channel::<()>();
        std::thread::spawn(move || {
            let _ = err_thread.join();
            let _ = done_tx.send(());
        });
        let _ = done_rx.recv_timeout(Duration::from_secs(5));
        let rc = wait_code(child);
        let t = tail.lock().unwrap().clone();
        failed(format!("pack {name}: the worker {}", exit_reason(rc, &t)))
    };
    let kill = |child: &mut Child| {
        let _ = child.kill();
        let _ = child.wait();
    };
    let not_a_reply = |child: &mut Child, raw: &[u8]| -> Outcome {
        let _ = child.kill();
        let _ = child.wait();
        let text = String::from_utf8_lossy(raw);
        failed(format!(
            "pack {name}: the worker wrote a line that is not a reply: {}",
            head_chars(crate::gate::py::text::strip(&text), 80)
        ))
    };

    let first = match rx.recv_timeout(LOAD_TIMEOUT) {
        Ok(b) => b,
        Err(mpsc::RecvTimeoutError::Disconnected) => return dead(&mut child, err_thread),
        Err(mpsc::RecvTimeoutError::Timeout) => {
            kill(&mut child);
            return failed(format!(
                "pack {name}: the worker was not ready in {} s",
                LOAD_TIMEOUT.as_secs()
            ));
        }
    };
    let obj = parse_line(&first);
    if reply_kind(obj.as_ref()) != Some("ready") {
        return not_a_reply(&mut child, &first);
    }
    let got = str_of(obj.as_ref().unwrap(), "ready");
    if got != PROTOCOL {
        kill(&mut child);
        return failed(format!(
            "pack {name}: the worker speaks {got}, not {PROTOCOL}. Install it again: daisugi pack install {name} --force"
        ));
    }
    if let Some(w) = stdin.as_mut() {
        let _ = w.write_all(&request(job, args));
        let _ = w.flush();
    }
    let stop = |child: &mut Child, stdin: &mut Option<std::process::ChildStdin>| {
        drop(stdin.take());
        let deadline = std::time::Instant::now() + STOP_GRACE;
        loop {
            match child.try_wait() {
                Ok(Some(_)) => return,
                Ok(None) if std::time::Instant::now() < deadline => {
                    std::thread::sleep(Duration::from_millis(10))
                }
                _ => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return;
                }
            }
        }
    };
    while let Ok(raw) = rx.recv() {
        let obj = parse_line(&raw);
        match reply_kind(obj.as_ref()) {
            Some("progress") => on_progress(&str_of(obj.as_ref().unwrap(), "progress")),
            Some("result") => {
                stop(&mut child, &mut stdin);
                let res = obj.unwrap().get("result").and_then(|v| v.as_obj()).cloned();
                return Outcome {
                    code: 0,
                    result: res,
                    error: None,
                };
            }
            Some("error") => {
                stop(&mut child, &mut stdin);
                let why = str_of(obj.as_ref().unwrap(), "error");
                return failed(format!("pack {name}: {job}: {why}"));
            }
            _ => return not_a_reply(&mut child, &raw),
        }
    }
    drop(stdin.take());
    dead(&mut child, err_thread)
}

/// The pack's directory for a name.
pub fn pack_path(data_dir: &str, name: &str) -> PathBuf {
    Path::new(data_dir).join("packs").join(name)
}
