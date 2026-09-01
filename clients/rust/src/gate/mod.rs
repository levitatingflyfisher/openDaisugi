//! A port of `opendaisugi.gate`: one hook payload in, the host's verdict
//! contract out, with the same shadow log, session tree, captures mirror
//! and coppice report the Python gate writes. It runs no Python.
//!
//! A call the port cannot decide exactly as the Python gate would is
//! denied, with exit 2 and a reason, in the contract of the call's host
//! format. That is decided before anything is written, so an undecided
//! call leaves no log line.

pub mod argparse;
pub mod ask;
pub mod check;
pub mod checkpoints;
pub mod config;
pub mod decide;
pub mod dispatch;
pub mod effects;
pub mod envelope;
pub mod frames;
pub mod globs;
pub mod hook;
pub mod llm;
pub mod logs;
pub mod py;
pub mod paths;
pub mod predicate;
pub mod pydantic;
pub mod pymodel;
pub mod pyjson;
pub mod pystr;
pub mod record;
pub mod rules;
pub mod search;
pub mod sha256;
pub mod state_report;
pub mod urls;

use decide::Decision;
use effects::PERMANENT;
use paths::{exists, path_join, path_str};
use py::text::{as_surrogate, fsdecode, has_surrogate};
use pyjson::{dumps, loads, Object, Value};
use pystr::py_strip;
use record::join_of;
use std::collections::HashMap;
use std::ffi::OsString;
use std::sync::{mpsc, OnceLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

pub use py::PyErr;

/// Why a step of the gate did not return a value: a Python exception the
/// oracle raises at the same point, or a case the port cannot decide.
/// Python catches its exceptions at fixed places, and so does the port
/// (`catch`). An undecided case is never caught: it denies the call.
#[derive(Debug, Clone)]
pub enum Fault {
    Raised(PyErr),
    Undecided(String),
}

impl From<PyErr> for Fault {
    fn from(e: PyErr) -> Self {
        if e.is("Undecided") {
            Fault::Undecided(e.msg)
        } else {
            Fault::Raised(e)
        }
    }
}

pub type R<T> = Result<T, Fault>;

pub fn undecided<T>(why: impl Into<String>) -> R<T> {
    Err(Fault::Undecided(why.into()))
}

/// Raises a Python exception.
pub fn raise<T>(e: PyErr) -> R<T> {
    Err(Fault::Raised(e))
}

/// Python's `try: ... except Exception as e:` around a step: the value, or
/// the exception it raised. An undecided case passes through.
pub fn catch<T>(r: R<T>) -> R<Result<T, PyErr>> {
    match r {
        Ok(v) => Ok(Ok(v)),
        Err(Fault::Raised(e)) => Ok(Err(e)),
        Err(Fault::Undecided(w)) => Err(Fault::Undecided(w)),
    }
}

/// A best-effort step after the verdict is final (the session tree, a
/// checkpoint, the captures mirror, a report): Python swallows its
/// exceptions, and a case the port cannot decide there must not change a
/// verdict already given, so both end the step.
pub fn best_effort<T>(r: R<T>) -> Option<T> {
    r.ok()
}

/// `gate.MAX_PAYLOAD_BYTES` and `gate.MAX_SHELL_COMMAND_CHARS`: past them
/// the gate denies an input unread.
pub const MAX_PAYLOAD_BYTES: usize = 16 * 1024 * 1024;
pub const MAX_SHELL_COMMAND_CHARS: usize = 256 * 1024;

/// What the gate process emits.
#[derive(Debug, Clone, Default)]
pub struct GateResult {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub exit: i32,
    /// False when the port could not decide the call; `why` says why.
    pub native: bool,
    pub why: String,
}

/// One call's inputs.
#[derive(Debug, Clone)]
pub struct Runner {
    pub env: HashMap<String, String>,
    pub home: String,
    wd: OnceLock<Option<String>>,
    /// realpath answers for the length of one call (see `paths::realpath`):
    /// the answer, and the deepest symlink nesting it took.
    real_cache: std::sync::Arc<std::sync::Mutex<HashMap<String, (Result<String, PyErr>, i64)>>>,
    pub root: String,
    pub default_root: String,
    pub mode: String,
    pub fmt: String,
    pub session: Option<String>,
    pub captures: Option<String>,
    pub verify_timeout_s: f64,
    pub ask: bool,
    pub ask_timeout_s: f64,
    pub checkpoints: bool,
    pub t0: Instant,
}

/// `n` random bytes as hex, from the kernel.
pub fn random_hex(n: usize) -> R<String> {
    use std::io::Read;
    let mut b = vec![0u8; n];
    match std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut b)) {
        Ok(()) => Ok(b.iter().map(|x| format!("{x:02x}")).collect()),
        Err(_) => undecided("no randomness"),
    }
}

/// `time.time()`.
pub fn now_float() -> f64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}

/// The environment variable the outer process sets on its worker with the
/// width of the terminal on its own stdout, when it has one. Python reads
/// that width for argparse's text; the worker's stdout is a pipe.
pub const TTY_ENV: &str = "DAISUGI_GATE_TTY_COLUMNS";

/// Answers one call, with `env` as the whole environment.
pub fn run(argv: &[String], stdin: &[u8], env: HashMap<String, String>) -> GateResult {
    let os: Vec<OsString> = argv.iter().map(OsString::from).collect();
    answer(argv, &os, stdin, &env)
}

/// `gate._fmt_from_argv`: the first `--format X` or `--format=X`, as
/// written, else claude.
fn fmt_from_argv(argv: &[String]) -> String {
    for (i, a) in argv.iter().enumerate() {
        if a == "--format" && i + 1 < argv.len() {
            return argv[i + 1].clone();
        }
        if let Some(v) = a.strip_prefix("--format=") {
            return v.to_string();
        }
    }
    "claude".into()
}

/// The argv as Python reads it (UTF-8, undecodable bytes as surrogates).
fn py_argv(args: &[OsString]) -> Vec<String> {
    use std::os::unix::ffi::OsStrExt;
    args.iter().map(|a| fsdecode(a.as_bytes())).collect()
}

/// How long the outer process waits for its worker: the verify budget,
/// plus the ask budget with --ask, plus 3 s (at most 3,600 s of budget;
/// 13 s when argparse refuses the argv). The installer gives the host hook
/// at least the verify budget plus 5 s (the ask budget too with --ask),
/// and a host that times a hook out lets the call through, so the worker
/// must end, and deny, first.
pub fn child_deadline_s(args: &[OsString]) -> f64 {
    let a = match argparse::parse(&py_argv(args), 80) {
        Ok(a) => a,
        Err(_) => return 13.0,
    };
    let mut budget = a.verify_timeout;
    if a.ask {
        budget += a.ask_timeout.max(0.0);
    }
    if budget.is_nan() || budget > 3600.0 {
        budget = 3600.0;
    }
    budget.max(1.0) + 3.0
}

/// The host format a deny of this argv takes, read as argparse reads it
/// (an abbreviated or repeated --format included); for an argv argparse
/// refuses, the format `_fmt_from_argv` guesses, as the oracle's escape
/// does.
pub fn deny_format(args: &[OsString]) -> String {
    let argv = py_argv(args);
    match argparse::parse(&argv, 80) {
        Ok(a) => a.fmt,
        Err(_) => fmt_from_argv(&argv),
    }
}

/// `run` for this process: its own argv and environment, each read as
/// Python reads it (UTF-8, with undecodable bytes kept as surrogates).
pub fn run_process(args: Vec<OsString>, stdin: &[u8]) -> GateResult {
    use std::os::unix::ffi::OsStrExt;
    let mut env = HashMap::new();
    for (k, v) in std::env::vars_os() {
        env.insert(fsdecode(k.as_bytes()), fsdecode(v.as_bytes()));
    }
    let argv: Vec<String> = args.iter().map(|a| fsdecode(a.as_bytes())).collect();
    answer(&argv, &args, stdin, &env)
}

fn answer(argv: &[String], os: &[OsString], stdin: &[u8], env: &HashMap<String, String>) -> GateResult {
    let native = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| run_native(argv, stdin, env)));
    let why = match native {
        Ok(Ok(mut res)) => {
            res.native = true;
            return res;
        }
        Ok(Err(Fault::Undecided(why))) => why,
        Ok(Err(Fault::Raised(e))) => format!("unexpected: an uncaught {}: {}", e.kind, e.msg),
        Err(_) => "unexpected: a panic in the port".to_string(),
    };
    undecided_deny(os, &why)
}

/// The deny for a call the port cannot decide.
fn undecided_deny(os: &[OsString], why: &str) -> GateResult {
    let mut out = backstop_deny(os, &format!("the Rust gate cannot decide this call: {why}"));
    out.native = false;
    out.why = why.to_string();
    out
}

/// `main()`'s printing: stdout then stderr, each with print()'s newline.
/// stdout is strict UTF-8 and stderr backslash-escapes what it cannot
/// encode; a stdout print that raises skips the stderr print.
fn printed(stdout: &str, stderr: &str, exit: i32) -> GateResult {
    let mut res = GateResult { exit, ..Default::default() };
    if !stdout.is_empty() {
        if has_surrogate(stdout) {
            return res;
        }
        res.stdout = format!("{stdout}\n").into_bytes();
    }
    if !stderr.is_empty() {
        res.stderr = format!("{}\n", backslashreplace(stderr)).into_bytes();
    }
    res
}

/// The text with each lone surrogate written as `\udXXX`, as a stream with
/// errors="backslashreplace" writes it.
fn backslashreplace(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match as_surrogate(c) {
            Some(u) => out.push_str(&format!("\\u{u:04x}")),
            None => out.push(c),
        }
    }
    out
}

fn run_native(argv: &[String], stdin: &[u8], env: &HashMap<String, String>) -> R<GateResult> {
    let t0 = Instant::now();
    let tty = env.get(TTY_ENV).and_then(|v| v.parse::<i64>().ok()).map(|c| (c, 24));
    let mut env = env.clone();
    env.remove(TTY_ENV);
    let width = argparse::terminal_columns(
        env.get("COLUMNS").map(String::as_str),
        env.get("LINES").map(String::as_str),
        tty,
    ) - 2;
    let args = match argparse::parse(argv, width) {
        Ok(a) => a,
        Err(argparse::Exit::Help(text)) => {
            // print_help writes the text as is; a SystemExit(0) follows.
            return Ok(GateResult { stdout: text.into_bytes(), exit: 0, ..Default::default() });
        }
        Err(argparse::Exit::Error(text)) => {
            // parser.error prints the usage and the error, and run_argv
            // turns its SystemExit(2) into a deny: a block body on stdout
            // for the formats that read stdout (the format guessed from the
            // raw argv), else exit 2.
            let fmt = fmt_from_argv(argv);
            let mut res = if fmt == "hermes" || fmt == "openclaw" {
                let d = Decision {
                    allow: false,
                    would_deny: true,
                    reason: "gate escape: 2".into(),
                    ..Default::default()
                };
                outcome(&d, &fmt)?
            } else {
                printed("", "openDaisugi gate: DENIED (fail-closed on error): 2", 2)
            };
            let mut err = backslashreplace(&text).into_bytes();
            err.extend(res.stderr);
            res.stderr = err;
            return Ok(res);
        }
    };
    if env.get("OPENDAISUGI_CONFORMANCE_RECORD").is_some_and(|v| !v.is_empty()) {
        return undecided("conformance recording is on");
    }
    let home = env.get("HOME").cloned().unwrap_or_default();
    let mut r = Runner {
        env: env.clone(),
        home,
        wd: OnceLock::new(),
        real_cache: Default::default(),
        root: String::new(),
        default_root: String::new(),
        mode: String::new(),
        fmt: args.fmt.clone(),
        session: args.session.clone(),
        captures: args.captures_root.as_deref().map(path_str),
        verify_timeout_s: args.verify_timeout,
        ask: args.ask,
        ask_timeout_s: args.ask_timeout,
        checkpoints: args.checkpoints,
        t0,
    };
    // DEFAULT_GATE_ROOT is worked out when the module is imported: an
    // error there ends the Python gate before it reads its input.
    let home_dir = match catch(r.path_home())? {
        Ok(h) => h,
        Err(e) => return undecided(format!("Path.home() raises at import: {}", e.msg)),
    };
    r.default_root = path_join(&path_join(&home_dir, ".opendaisugi"), "gate");
    r.root = match &args.root {
        Some(x) => path_str(x),
        None => r.default_root.clone(),
    };
    let t = r.verify_timeout_s;
    if !t.is_finite() || (t > 0.0 && t < 1.0) {
        // The verifier thread's join would raise, or would race a budget
        // this small.
        return undecided("a --verify-timeout that is not finite, or under one second");
    }
    r.mode = match &args.mode {
        Some(m) => m.clone(),
        None => {
            // resolve_gate_mode runs in run_argv, before gate_and_contract.
            let _f = frames::at(5);
            r.config_gate_mode()?
        }
    };
    r.decide(stdin)
}

impl Runner {
    pub fn elapsed_ms(&self) -> f64 {
        self.t0.elapsed().as_nanos() as f64 / 1e6
    }

    /// Runs `f` under the `--verify-timeout` budget on a thread of its own,
    /// with `base` Python frames on its stack, as the Python gate runs its
    /// verifier in a worker thread. `None` when the budget ran out; the
    /// worker is then abandoned (the process ends soon after).
    pub fn run_bounded<T: Send + 'static>(
        &self,
        base: i64,
        f: impl FnOnce(&Runner) -> R<T> + Send + 'static,
    ) -> R<Option<T>> {
        let (tx, rx) = mpsc::channel();
        let me = self.clone();
        let spawned = std::thread::Builder::new().stack_size(256 << 20).spawn(move || {
            let _f = frames::at(base);
            let out = f(&me);
            let _ = tx.send(out);
        });
        if spawned.is_err() {
            return undecided("the gate could not start a thread");
        }
        let budget = if self.verify_timeout_s > 0.0 { self.verify_timeout_s } else { 0.0 };
        match rx.recv_timeout(Duration::from_secs_f64(budget)) {
            Ok(r) => r.map(Some),
            Err(mpsc::RecvTimeoutError::Timeout) => Ok(None),
            Err(mpsc::RecvTimeoutError::Disconnected) => undecided("unexpected: a panic in a native check"),
        }
    }

    /// Runs `f` to its end on a thread of its own with a large stack and
    /// `base` Python frames on it: a step the oracle runs with no deadline.
    pub fn run_unbounded<T: Send + 'static>(
        &self,
        base: i64,
        f: impl FnOnce(&Runner) -> R<T> + Send + 'static,
    ) -> R<T> {
        let me = self.clone();
        let spawned = std::thread::Builder::new().stack_size(256 << 20).spawn(move || {
            let _f = frames::at(base);
            f(&me)
        });
        match spawned {
            Err(_) => undecided("the gate could not start a thread"),
            Ok(h) => match h.join() {
                Ok(r) => r,
                Err(_) => undecided("unexpected: a panic in a native check"),
            },
        }
    }

    fn is_disarmed(&self) -> R<bool> {
        let _f = frames::frame();
        exists(&path_join(&self.root, "DISARMED"))
    }

    /// `gate.gate_and_contract`: the verdict, its log lines, and the host
    /// outcome.
    fn decide(&self, stdin: &[u8]) -> R<GateResult> {
        let _f = frames::at(6);
        let d = match catch(self.decide_body(stdin))? {
            Ok(d) => d,
            Err(e) => {
                // The mode-selected failure policy.
                if self.mode == "enforce" {
                    self.deny(&format!("gate I/O error (denied fail-closed): {}", e.msg))
                } else {
                    Decision {
                        allow: true,
                        would_deny: true,
                        reason: format!("gate I/O error (shadow mode allows): {}", e.msg),
                        elapsed_ms: self.elapsed_ms(),
                        tier: PERMANENT.into(),
                        ..Default::default()
                    }
                }
            }
        };
        outcome(&d, &self.fmt)
    }

    fn decide_body(&self, stdin: &[u8]) -> R<Decision> {
        if self.is_disarmed()? {
            let d = Decision {
                allow: true,
                reason: "gate disarmed by operator (marker file present)".into(),
                elapsed_ms: self.elapsed_ms(),
                tier: PERMANENT.into(),
                ..Default::default()
            };
            self.log_shadow(&Value::Null, &d, &Value::Null, None)?;
            return Ok(d);
        }
        let too_big = stdin.len() > MAX_PAYLOAD_BYTES;
        let text = if too_big { String::new() } else { String::from_utf8_lossy(stdin).into_owned() };
        let mut payload: Option<Value> = None;
        if !py_strip(&text).is_empty() {
            match loads(&text) {
                // JSON null is Python's None, which reads as no payload.
                Ok(Value::Null) | Err(_) => {}
                Ok(v) => payload = Some(v),
            }
        }
        let obj = match &payload {
            Some(Value::Obj(o)) => Some(o),
            _ => None,
        };
        let payload_session = obj.map(|p| p.value("session_id").clone()).unwrap_or(Value::Null);
        let session_id = match &self.session {
            Some(s) if !s.is_empty() => Value::Str(s.clone()),
            _ => payload_session.clone(),
        };
        let env = self.load_envelope(&session_id)?;
        // Only the predicate stage needs Z3, and a context takes about
        // 9 ms to make: start it now, on a thread, when that stage may run.
        if env.as_ref().is_some_and(|e| !e.invariants.is_empty() || !e.postconditions.is_empty()) {
            crate::z3py::prewarm();
        }
        let mut d = match (&env, &payload, obj) {
            (None, _, _) => self.deny(
                "no envelope registered for this session — run `daisugi gate register <envelope.json>` to \
                 authorize it, or `daisugi gate disarm` to switch the gate off",
            ),
            _ if too_big => {
                self.deny(&format!("hook payload is larger than {MAX_PAYLOAD_BYTES} bytes; the gate does not read it"))
            }
            (_, None, _) => self.deny("hook payload was not parseable JSON"),
            (_, _, None) => self.deny("hook payload is not a JSON object"),
            (Some(env), _, Some(p)) => self.evaluate_call(p, env)?,
        };
        if self.ask && self.mode == "enforce" && d.would_deny && !d.pane_rule {
            if let Some(p) = obj {
                d = self.maybe_ask(p, d, &session_id)?;
            }
        }
        d.elapsed_ms = d.elapsed_ms.max(0.0);
        let join = obj.map(join_of).unwrap_or_default();
        self.log_shadow(&session_id, &d, &payload_session, Some(&join))?;
        if let Some(p) = obj {
            let tree = self.log_tree(p, &d, &session_id)?;
            if self.checkpoints && d.allow {
                self.maybe_checkpoint(p, &session_id)?;
            }
            if let (Some(c), true) = (&self.captures, d.allow) {
                self.capture(c, p)?;
            }
            self.report_state(p, &d, &session_id, tree)?;
        }
        Ok(d)
    }
}

/// The deny the outer process gives when its worker did not answer: the
/// call's host format read as argparse reads it (the last `--format` wins,
/// a unique prefix counts), in that format's contract.
pub fn backstop_deny(args: &[OsString], why: &str) -> GateResult {
    let fmt = deny_format(args);
    let d = Decision {
        allow: false,
        would_deny: true,
        reason: format!("gate internal error (denied fail-closed): {why}"),
        ..Default::default()
    };
    outcome(&d, &fmt).unwrap_or_else(|_| GateResult {
        stderr: format!("openDaisugi gate: DENIED — {}\n", d.reason).into_bytes(),
        exit: 2,
        ..Default::default()
    })
}

/// `gate._outcome`, printed as `main()` prints it.
fn outcome(d: &Decision, f: &str) -> R<GateResult> {
    let deny_now = !d.allow;
    Ok(match f {
        "claude" | "pi" | "opencode" => {
            if deny_now {
                printed("", &format!("openDaisugi gate: DENIED — {}", d.reason), 2)
            } else if let Some(edit) = &d.updated_input {
                // An operator edited the call before allowing it: only
                // claude's hookSpecificOutput carries an edit.
                if f == "claude" {
                    let body = Object::new().with(
                        "hookSpecificOutput",
                        Object::new()
                            .with("hookEventName", "PreToolUse")
                            .with("permissionDecision", "allow")
                            .with("permissionDecisionReason", d.reason.as_str())
                            .with("updatedInput", edit.clone()),
                    );
                    printed(&dumps(&Value::Obj(body), true), "", 0)
                } else {
                    printed(
                        "",
                        &format!(
                            "openDaisugi gate: DENIED: {}. The operator edit cannot be carried on the {} format. \
                             It has no updatedInput channel. Denied fail-closed rather than running the original input.",
                            d.reason,
                            py::text::repr(f)
                        ),
                        2,
                    )
                }
            } else {
                printed(&stdout_for(f, false, ""), "", 0)
            }
        }
        "hermes" | "openclaw" => {
            if d.updated_input.is_some() {
                let why = format!(
                    "{}. The operator edit cannot be carried on the {} format. It has no updatedInput channel. \
                     Denied fail-closed rather than running the original input",
                    d.reason,
                    py::text::repr(f)
                );
                printed(&stdout_for(f, true, &why), "", 0)
            } else {
                printed(&stdout_for(f, deny_now, &d.reason), "", 0)
            }
        }
        _ => printed(
            "",
            &format!(
                "openDaisugi gate: DENIED: unknown host format {}. Use --format claude, pi, opencode, hermes, or openclaw.",
                py::text::repr(f)
            ),
            2,
        ),
    })
}

/// `hook.stdout_for_format`.
fn stdout_for(f: &str, block: bool, reason: &str) -> String {
    match f {
        "hermes" if block => dumps(
            &Value::Obj(Object::new().with("decision", "block").with("action", "block").with("reason", reason)),
            true,
        ),
        "openclaw" if block => {
            dumps(&Value::Obj(Object::new().with("block", true).with("blockReason", reason)), true)
        }
        "hermes" | "openclaw" => "{}".into(),
        _ => r#"{"continue": true}"#.into(),
    }
}

#[cfg(test)]
mod tests;
