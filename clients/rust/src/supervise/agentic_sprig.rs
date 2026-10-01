//! `AgenticExecutor._run_sprig_step`: an agentic step run as the sprig
//! binary in its workspace, with the gate in `--gate-cmd` pinned to the
//! gate root and the session, and the wall in `--tools`. The texts of a
//! failed run are the oracle's own. The Go `supervise.runSprig` is the
//! reference.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::time::{Duration, Instant};

use super::{ms, ExecResult};
use crate::gate::py::text::{decode_utf8_replace, head, repr, strip};
use crate::gate::pyjson::{loads_py, Object, Value};

/// The host tools sprig has a tool for, and its name for each. Any other
/// requested tool is left out of sprig's wall.
pub fn sprig_tool(host: &str) -> Option<&'static str> {
    Some(match host {
        "Read" => "read",
        "Write" => "write",
        "Edit" => "edit",
        "Bash" => "bash",
        _ => return None,
    })
}

/// The failed step when a word of sprig's gate command would hold
/// whitespace: sprig splits `--gate-cmd` at whitespace.
pub const SPRIG_SPACE_TEXT: &str = "the sprig gate command has a word with a space in it, and sprig \
splits that command at spaces. Use a TMPDIR path and a daisugi path with no spaces.";

/// What one sprig run needs from the executor.
pub struct SprigRun<'a> {
    pub sprig: &'a str,
    pub self_path: &'a str,
    pub model: &'a str,
    pub env: &'a HashMap<String, String>,
    pub workspace: &'a str,
    pub root: &'a str,
    pub session: &'a str,
}

/// `_sprig_exit_text`: the exit code (or the signal) and the last line
/// sprig wrote to stderr.
pub fn exit_text(rc: i32, stderr: &[u8]) -> String {
    if rc < 0 {
        return format!("agentic sub-agent failed: sprig was killed by signal {}", -rc);
    }
    let text = format!("agentic sub-agent failed: sprig exited with code {rc}");
    let decoded = decode_utf8_replace(stderr);
    match decoded.split('\n').map(strip).filter(|s| !s.is_empty()).last() {
        Some(last) => format!("{text}: {}", head(last, 500)),
        None => text,
    }
}

/// Runs sprig for the step and returns the step's result.
pub fn run(
    r: &SprigRun,
    step: &Object,
    allowed: &[String],
    timeout_s: u64,
    max_output_bytes: usize,
    started: Instant,
) -> Result<ExecResult, String> {
    let fail = |msg: String| -> Result<ExecResult, String> {
        Ok(ExecResult {
            rc: 1,
            stdout: super::agentic::truncate_output(&msg, max_output_bytes),
            duration_ms: ms(started),
            ..Default::default()
        })
    };
    // The gate is pinned by its own command (--root, --session), never by
    // the payload sprig sends.
    let gate = [
        r.self_path.to_string(),
        "gate".into(),
        "check".into(),
        "--mode".into(),
        "enforce".into(),
        "--root".into(),
        r.root.into(),
        "--session".into(),
        r.session.into(),
        "--captures-root".into(),
        format!("{}/captures", r.root),
    ]
    .join(" ");
    let mut wall: Vec<&str> = Vec::new();
    for t in allowed {
        if let Some(name) = sprig_tool(t) {
            if !wall.contains(&name) {
                wall.push(name);
            }
        }
    }
    let mut argv: Vec<String> = vec![
        "--json".into(),
        "--gate".into(),
        "--gate-cmd".into(),
        gate,
        "--tools".into(),
        wall.join(","),
        "--model".into(),
        r.model.into(),
        "--session-dir".into(),
        format!("{}/sessions", r.root),
        "--session".into(),
        r.session.into(),
    ];
    if let Value::Int(t) = step.value("max_turns") {
        argv.push("--max-turns".into());
        argv.push(t.clone());
    }
    argv.push("-".into());
    let prompt = super::str_of(step, "prompt");
    let stdin = crate::gate::py::text::fsencode(&prompt).unwrap_or_else(|_| prompt.clone().into_bytes());

    let start_text = format!("agentic sub-agent failed: cannot start sprig ({})", r.sprig);
    let bin = if r.sprig.contains('/') {
        Some(r.sprig.to_string())
    } else {
        crate::cli::install::look_path(r.env, r.sprig)
    };
    let Some(bin) = bin else {
        return fail(start_text);
    };
    let ran = match run_group(&bin, &argv, r.workspace, r.env, stdin, timeout_s) {
        Ok(x) => x,
        Err(_) => return fail(start_text),
    };
    if ran.timed_out {
        return fail(format!(
            "agentic sub-agent failed: sprig ran past {timeout_s}s and was killed"
        ));
    }
    if ran.rc != 0 {
        return fail(exit_text(ran.rc, &ran.stderr));
    }
    let raw = decode_utf8_replace(&ran.stdout);
    let parsed = match loads_py(&raw, 900) {
        Ok(v) => v,
        Err(_) => {
            return fail(format!(
                "agentic sub-agent returned unparseable output: {}",
                repr(head(&raw, 300))
            ))
        }
    };
    let answer = match &parsed {
        Value::Obj(o) => match o.value("answer") {
            Value::Str(s) => Some(s.clone()),
            _ => None,
        },
        _ => None,
    };
    let Some(answer) = answer else {
        return fail(format!(
            "agentic sub-agent reply has no answer: {}",
            repr(head(&raw, 300))
        ));
    };
    Ok(ExecResult {
        rc: 0,
        stdout: super::agentic::truncate_output(&answer, max_output_bytes),
        duration_ms: ms(started),
        ..Default::default()
    })
}

/// `SPRIG_DRAIN_S`: how long the output is read after the timeout kill.
pub const SPRIG_DRAIN_S: u64 = 2;

/// What `run_group` saw.
pub struct Ran {
    pub rc: i32,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub timed_out: bool,
}

/// Runs bin in its own process group and kills the whole group at the
/// timeout, so a tool or gate process it started dies with it. Err when
/// the program cannot start.
pub fn run_group(
    bin: &str,
    args: &[String],
    dir: &str,
    env: &HashMap<String, String>,
    stdin: Vec<u8>,
    timeout_s: u64,
) -> std::io::Result<Ran> {
    use std::os::unix::process::{CommandExt, ExitStatusExt};
    use std::process::{Command, Stdio};
    let mut cmd = Command::new(bin);
    cmd.args(args)
        .current_dir(dir)
        .env_clear()
        .envs(env)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    let mut child = cmd.spawn()?;
    let pgid = child.id() as i32;
    let mut sin = child.stdin.take();
    let feeder = std::thread::spawn(move || {
        if let Some(s) = sin.as_mut() {
            let _ = s.write_all(&stdin);
        }
    });
    let reader = |p: Option<Box<dyn Read + Send>>| {
        std::thread::spawn(move || {
            let mut b = Vec::new();
            if let Some(mut p) = p {
                let _ = p.read_to_end(&mut b);
            }
            b
        })
    };
    let out = reader(child.stdout.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let err = reader(child.stderr.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let limit = Duration::from_secs(timeout_s);
    let t0 = Instant::now();
    let mut timed_out = false;
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break Some(s),
            Ok(None) if t0.elapsed() >= limit => {
                timed_out = true;
                unsafe {
                    libc::kill(-pgid, libc::SIGKILL);
                }
                break child.wait().ok();
            }
            Ok(None) => std::thread::sleep(Duration::from_millis(5)),
            Err(_) => break None,
        }
    };
    // A process sprig left can hold the pipes open after sprig exits: read
    // to EOF until the step's own time is up, then kill the group. As in
    // the oracle (communicate with a timeout), that is a timeout too.
    while !(out.is_finished() && err.is_finished()) {
        if t0.elapsed() >= limit {
            timed_out = true;
            unsafe {
                libc::kill(-pgid, libc::SIGKILL);
            }
            break;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    // A process that left the group can still hold a pipe: after the kill,
    // wait for the rest of the output a short, fixed time, then stop. The
    // reader threads are left to end when that process does.
    if timed_out {
        let drain = Instant::now();
        while !(out.is_finished() && err.is_finished()) {
            if drain.elapsed() >= Duration::from_secs(SPRIG_DRAIN_S) {
                return Ok(Ran { rc: -1, stdout: Vec::new(), stderr: Vec::new(), timed_out });
            }
            std::thread::sleep(Duration::from_millis(5));
        }
    }
    let _ = feeder.join();
    let stdout = out.join().unwrap_or_default();
    let stderr = err.join().unwrap_or_default();
    let rc = match status {
        Some(s) => match (s.code(), s.signal()) {
            (Some(c), _) => c,
            (None, Some(sig)) => -sig,
            _ => -1,
        },
        None => -1,
    };
    Ok(Ran { rc, stdout, stderr, timed_out })
}
