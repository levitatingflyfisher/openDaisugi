//! `tiers setup --endpoint` and `--remote`: the HTTP backend is pinned, a
//! failed probe says why, and the served context window is read from
//! llama-server's own fields. The box that found the bug had a `claude`
//! binary on PATH, so auto-detection chose claude-code, which ignores the
//! endpoint. These tests put a stub `claude` on PATH and serve the
//! endpoint from a localhost server. No model, no network.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

const VALID: &str = r#"{"generated_by":"model","task":"t","permissions":{"file_read":["/work/**"],"file_write":[],"network":false,"shell":false,"shell_allowlist":[],"max_execution_time_s":30,"max_output_size_mb":10},"invariants":[],"postconditions":[]}"#;

const LLAMA_MODELS: &str = r#"{"object":"list","data":[{"id":"gemma","object":"model","meta":{"n_vocab":262144,"n_ctx":16384,"n_ctx_train":262144}}]}"#;

/// One directory per test, under target/ (on disk: /tmp is RAM on some boxes).
fn scratch(tag: &str) -> PathBuf {
    let dir = PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!("setup-endpoint-{}-{tag}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Ends the server thread when the test ends: it sets the stop flag and
/// joins, so no thread outlives its test.
struct Guard {
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl Drop for Guard {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

/// The most connections a server takes before it ends by itself.
const MAX_CONNECTIONS: usize = 64;

/// A server: `routes` maps a path to (status, body). Returns the port, the
/// paths it was asked for in order, and a guard that stops and joins the
/// thread when dropped. The thread also ends by itself after
/// `MAX_CONNECTIONS` connections.
fn serve(routes: Vec<(&'static str, u16, String)>) -> (u16, Arc<Mutex<Vec<String>>>, Guard) {
    let l = TcpListener::bind("127.0.0.1:0").unwrap();
    l.set_nonblocking(true).unwrap();
    let port = l.local_addr().unwrap().port();
    let stop = Arc::new(AtomicBool::new(false));
    let stopped = stop.clone();
    let hits = Arc::new(Mutex::new(vec![]));
    let seen = hits.clone();
    let routes: HashMap<&'static str, (u16, String)> = routes.into_iter().map(|(p, s, b)| (p, (s, b))).collect();
    let handle = std::thread::spawn(move || {
        let mut taken = 0;
        while taken < MAX_CONNECTIONS && !stopped.load(Ordering::SeqCst) {
            let mut s = match l.accept() {
                Ok((s, _)) => s,
                Err(_) => {
                    std::thread::sleep(std::time::Duration::from_millis(5));
                    continue;
                }
            };
            taken += 1;
            s.set_nonblocking(false).unwrap();
            s.set_read_timeout(Some(std::time::Duration::from_secs(5))).unwrap();
            let mut buf = vec![];
            let mut b = [0u8; 4096];
            let head_end = loop {
                let k = s.read(&mut b).unwrap_or(0);
                if k == 0 {
                    break None;
                }
                buf.extend_from_slice(&b[..k]);
                if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                    break Some(i + 4);
                }
            };
            let Some(head_end) = head_end else { continue };
            let head = String::from_utf8_lossy(&buf[..head_end]).into_owned();
            let want: usize = head
                .lines()
                .find_map(|l| l.to_ascii_lowercase().strip_prefix("content-length:").map(|v| v.trim().parse().unwrap_or(0)))
                .unwrap_or(0);
            while buf.len() < head_end + want {
                let k = s.read(&mut b).unwrap_or(0);
                if k == 0 {
                    break;
                }
                buf.extend_from_slice(&b[..k]);
            }
            let path = head.split_whitespace().nth(1).unwrap_or("").to_string();
            seen.lock().unwrap().push(path.clone());
            let (status, body) = routes.get(path.as_str()).cloned().unwrap_or((404, "{}".into()));
            let reply = format!(
                "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                body.len()
            );
            let _ = s.write_all(reply.as_bytes());
        }
    });
    (port, hits, Guard { stop, handle: Some(handle) })
}

fn chat(message: &str, finish: &str) -> String {
    format!(r#"{{"choices":[{{"message":{message},"finish_reason":"{finish}"}}]}}"#)
}

fn content(text: &str) -> String {
    format!(r#"{{"role":"assistant","content":{}}}"#, serde_json::to_string(text).unwrap())
}

/// Runs `daisugi tiers setup ARGS` with a stub `claude` first on PATH.
fn setup(tag: &str, extra_env: &[(&str, &str)], args: &[&str]) -> (i32, String, String) {
    let home = scratch(tag);
    let bin = home.join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    // A child process writes the stub, so this process never holds a write
    // handle to it (ETXTBSY on exec).
    let mut sh = Command::new("/bin/sh");
    sh.args(["-c", "printf '#!/bin/sh\\nexit 1\\n' > \"$1\" && chmod 755 \"$1\"", "sh"]).arg(bin.join("claude"));
    assert!(sh.status().unwrap().success());
    let data = home.join("d");
    let mut c = Command::new(env!("CARGO_BIN_EXE_daisugi"));
    c.args(["tiers", "setup", "--data-dir"])
        .arg(&data)
        .args(args)
        .env_clear()
        .env("HOME", &home)
        .env("PATH", &bin)
        .current_dir(&home)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (k, v) in extra_env {
        c.env(k, v);
    }
    let out = c.output().unwrap();
    (
        out.status.code().unwrap_or(-1),
        String::from_utf8_lossy(&out.stdout).into_owned(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    )
}

fn count(hits: &Arc<Mutex<Vec<String>>>, path: &str) -> usize {
    hits.lock().unwrap().iter().filter(|p| *p == path).count()
}

#[test]
fn endpoint_ignores_claude_on_path() {
    let (port, hits, _guard) = serve(vec![("/v1/chat/completions", 200, chat(&content(VALID), "stop"))]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (code, out, err) = setup("ignores", &[], &["--endpoint", &ep, "--model", "m"]);
    assert_eq!(code, 0, "{out}{err}");
    assert_eq!(count(&hits, "/v1/chat/completions"), 3, "{out}{err}");
    assert!(out.contains("PASSED") && out.contains("3/3"), "{out}");
}

#[test]
fn endpoint_refuses_forced_claude_code() {
    let (port, hits, _guard) = serve(vec![("/v1/chat/completions", 200, chat(&content(VALID), "stop"))]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (code, out, err) =
        setup("forced", &[("OPENDAISUGI_LLM_BACKEND", "claude-code")], &["--endpoint", &ep, "--model", "m"]);
    assert_ne!(code, 0, "{out}{err}");
    assert!(err.contains("--endpoint cannot be used with the claude-code backend"), "{err}");
    assert!(hits.lock().unwrap().is_empty());
}

#[test]
fn a_failed_probe_says_why() {
    let (port, _, _guard) = serve(vec![("/v1/chat/completions", 500, "boom".into())]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (code, out, err) = setup("reason", &[], &["--endpoint", &ep, "--model", "m"]);
    assert_eq!(code, 0, "{out}{err}");
    assert!(out.contains("FAILED") && out.contains("boom") && out.contains("ALL attempts errored"), "{out}");
    let (_, js, _) = setup("reason-json", &[], &["--endpoint", &ep, "--model", "m", "--json"]);
    let v: serde_json::Value = serde_json::from_str(&js).unwrap();
    let o = v["qualification"]["outcomes"].as_array().unwrap();
    assert_eq!(o.len(), 3);
    assert_eq!(o[0]["outcome"], "error");
    assert!(o[0]["reason"].as_str().unwrap().contains("boom"));
    assert!(!o[0]["task"].as_str().unwrap().is_empty());
}

#[test]
fn declined_and_cut_reasons() {
    let (port, _, _guard) = serve(vec![("/v1/chat/completions", 200, chat(&content("nope"), "stop"))]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (_, js, _) = setup("declined", &[], &["--endpoint", &ep, "--model", "m", "--json"]);
    let v: serde_json::Value = serde_json::from_str(&js).unwrap();
    assert!(v["qualification"]["outcomes"].as_array().unwrap().iter().all(|o| o["outcome"] == "declined"), "{js}");
    let cut = chat(r#"{"role":"assistant","content":"","reasoning_content":"x"}"#, "length");
    let (port, _, _guard) = serve(vec![("/v1/chat/completions", 200, cut)]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (_, out, _) = setup("cut", &[], &["--endpoint", &ep, "--model", "m"]);
    assert!(out.contains("token budget"), "{out}");
}

#[test]
fn endpoint_reports_the_served_context() {
    let (port, _, _guard) = serve(vec![
        ("/v1/models", 200, LLAMA_MODELS.into()),
        ("/v1/chat/completions", 200, chat(&content(VALID), "stop")),
    ]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (_, out, _) = setup("ctx", &[], &["--endpoint", &ep, "--model", "m"]);
    assert!(out.contains("Context window: 16384 tokens") && !out.contains("262144"), "{out}");
    let (_, js, _) = setup("ctx-json", &[], &["--endpoint", &ep, "--model", "m", "--json"]);
    let v: serde_json::Value = serde_json::from_str(&js).unwrap();
    assert_eq!(v["qualification"]["context_window"], 16384);
}

#[test]
fn progress_goes_to_stderr_and_json_stdout_stays_clean() {
    let (port, _, _guard) = serve(vec![("/v1/chat/completions", 200, chat(&content(VALID), "stop"))]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    let (_, out, err) = setup("progress", &[], &["--endpoint", &ep, "--model", "m", "--json"]);
    assert!(err.contains(&format!("qualifying m at {ep}: 3 probes, up to 300 s each\n")), "{err}");
    assert!(err.contains("probe 1/3: valid") && err.contains("probe 3/3: valid"), "{err}");
    serde_json::from_str::<serde_json::Value>(&out).expect("stdout is one JSON document");
    let (bad, _, _g2) = serve(vec![("/v1/chat/completions", 500, "boom".into())]);
    let (_, _, err) = setup("progress-bad", &[], &["--endpoint", &format!("http://127.0.0.1:{bad}/v1"), "--model", "m"]);
    assert!(err.contains("probe 1/3: error: ") && err.contains("boom"), "{err}");
}

#[test]
fn endpoint_context_is_of_the_named_model() {
    let models = r#"{"data":[{"id":"small","meta":{"n_ctx":4096}},{"id":"big","meta":{"n_ctx":32768}}]}"#;
    let (port, _, _guard) = serve(vec![
        ("/v1/models", 200, models.into()),
        ("/v1/chat/completions", 200, chat(&content(VALID), "stop")),
    ]);
    let ep = format!("http://127.0.0.1:{port}/v1");
    for (name, want) in [("big", 32768), ("openai/big", 32768), ("other", 4096)] {
        let (_, out, _) = setup(&format!("named-{}", name.replace('/', "-")), &[], &["--endpoint", &ep, "--model", name]);
        assert!(out.contains(&format!("Context window: {want} tokens")), "{name}: {out}");
    }
}

#[test]
fn remote_context_from_models_meta() {
    let (port, hits, _guard) = serve(vec![("/v1/models", 200, LLAMA_MODELS.into())]);
    let host = format!("127.0.0.1:{port}");
    let (_, out, err) = setup("remote-meta", &[], &["--remote", &host, "--kind", "openai"]);
    assert!(out.contains("16384") && !out.contains("262144"), "{out}{err}");
    assert_eq!(count(&hits, "/props"), 0);
}

#[test]
fn remote_context_falls_back_to_props() {
    let (port, hits, _guard) = serve(vec![
        ("/v1/models", 200, r#"{"data":[{"id":"m","meta":{"n_ctx_train":262144}}]}"#.into()),
        ("/props", 200, r#"{"default_generation_settings":{"n_ctx":8192}}"#.into()),
    ]);
    let host = format!("127.0.0.1:{port}");
    let (_, out, err) = setup("remote-props", &[], &["--remote", &host, "--kind", "openai"]);
    assert!(out.contains("8192") && !out.contains("262144"), "{out}{err}");
    assert_eq!(count(&hits, "/props"), 1);
}

#[test]
fn remote_context_is_never_the_training_length() {
    let (port, _, _guard) = serve(vec![("/v1/models", 200, r#"{"data":[{"id":"m","n_ctx_train":4096}]}"#.into())]);
    let host = format!("127.0.0.1:{port}");
    let (_, out, _) = setup("remote-train", &[], &["--remote", &host, "--kind", "openai"]);
    assert!(!out.contains("4096"), "{out}");
}
