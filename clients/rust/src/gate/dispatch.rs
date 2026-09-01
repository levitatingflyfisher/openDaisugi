//! `verifier_dispatch.verify_via`: when config.yaml names a compiled
//! verifier client, the oracle's verdict is joined with that client's, run
//! on the conformance wire protocol. The client may only tighten; a client
//! that fails leaves the oracle's verdict alone. A client is found as the
//! oracle's gate finds it: OPENDAISUGI_<NAME>_CLIENT, else
//! daisugi-conform-<name> on PATH, never through a source checkout.

use super::check::Violation;
use super::envelope::Envelope;
use super::paths::os_error;
use super::py::text::{encode_utf8_strict, splitlines};
use super::pymodel::universal_newlines;
use super::pyjson::{any_json, canonical_json, dumps, loads, LoadError, Object, Value};
use super::record::Record;
use super::sha256::hexdigest;
use super::check::step_dict;
use super::{now_float, undecided, Runner, R};
use std::io::{Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

/// `bench.options.VERIFIER_CLIENTS`, less python (never dispatched).
struct Spec {
    argv: &'static [&'static str],
    build_steps: &'static [&'static str],
    build_cwd: &'static str,
}

fn spec(name: &str) -> Option<Spec> {
    Some(match name {
        "rust" => Spec {
            argv: &["clients/rust/target/release/conform"],
            build_steps: &["cargo build --release"],
            build_cwd: "clients/rust",
        },
        "go" => Spec {
            argv: &["clients/go/conform"],
            build_steps: &["go build -o conform ./cmd/conform"],
            build_cwd: "clients/go",
        },
        "typescript" => Spec {
            argv: &["node", "clients/ts/dist/conform.js"],
            build_steps: &["npm install", "npm run build"],
            build_cwd: "clients/ts",
        },
        "lean" => Spec {
            argv: &["clients/lean/.lake/build/bin/conform"],
            build_steps: &["lake build"],
            build_cwd: "clients/lean",
        },
        _ => return None,
    })
}

fn access(p: &Path, mode: i32) -> bool {
    use std::os::unix::ffi::OsStrExt;
    let c = match std::ffi::CString::new(p.as_os_str().as_bytes()) {
        Ok(c) => c,
        Err(_) => return false,
    };
    unsafe { libc::access(c.as_ptr(), mode) == 0 }
}

/// `shutil.which(name, path=path)`: the first executable file of that name
/// on the search path (`os.environ["PATH"]`, else the system's default
/// path, when `path` is None).
pub fn py_which(name: &str, path: Option<&str>) -> Option<PathBuf> {
    use std::os::unix::ffi::OsStrExt;
    let search: Vec<u8> = match path {
        Some(p) => super::py::text::fsencode(p).ok()?,
        None => match std::env::var_os("PATH") {
            Some(p) => p.as_bytes().to_vec(),
            None => b"/bin:/usr/bin".to_vec(),
        },
    };
    if search.is_empty() {
        return None;
    }
    let mut seen: Vec<&[u8]> = Vec::new();
    for dir in search.split(|&b| b == b':') {
        if seen.contains(&dir) {
            continue;
        }
        seen.push(dir);
        let f = if dir.is_empty() {
            PathBuf::from(name)
        } else {
            Path::new(std::ffi::OsStr::from_bytes(dir)).join(name)
        };
        if f.exists() && access(&f, libc::F_OK | libc::X_OK) && !f.is_dir() {
            return Some(f);
        }
    }
    None
}

/// `bench.options.gate_client_argv(spec)`: the client named by
/// OPENDAISUGI_<NAME>_CLIENT, else daisugi-conform-<name> on PATH; never a
/// source checkout. `None` when this box has none.
fn client_argv(r: &Runner, name: &str, s: &Spec) -> Option<Vec<String>> {
    let var = format!("OPENDAISUGI_{}_CLIENT", name.to_uppercase());
    let found = match r.env.get(&var).filter(|v| !v.is_empty()) {
        Some(v) => v.clone(),
        None => py_which(&format!("daisugi-conform-{name}"), r.env.get("PATH").map(String::as_str))?
            .to_string_lossy()
            .into_owned(),
    };
    let fp = PathBuf::from(std::ffi::OsStr::from_bytes(&super::py::text::fsencode(&found).ok()?));
    if !fp.is_file() {
        return None;
    }
    if s.argv[0] == "node" {
        let node = py_which("node", r.env.get("PATH").map(String::as_str))?;
        return Some(vec![node.to_string_lossy().into_owned(), found]);
    }
    if access(&fp, libc::X_OK) {
        Some(vec![found])
    } else {
        None
    }
}

/// Python's `format(x, "g")`: six significant digits, no trailing zeros.
fn fmt_g(x: f64) -> String {
    if x == 0.0 {
        return if x.is_sign_negative() { "-0".into() } else { "0".into() };
    }
    if !x.is_finite() {
        return if x.is_nan() { "nan".into() } else if x > 0.0 { "inf".into() } else { "-inf".into() };
    }
    let e = format!("{:.5e}", x);
    let (mant, exp) = e.split_once('e').unwrap_or((&e, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    if (-4..6).contains(&exp) {
        let decimals = (5 - exp).max(0) as usize;
        let s = format!("{:.*}", decimals, x);
        if s.contains('.') {
            s.trim_end_matches('0').trim_end_matches('.').to_string()
        } else {
            s
        }
    } else {
        let m = if mant.contains('.') { mant.trim_end_matches('0').trim_end_matches('.') } else { mant };
        format!("{m}e{}{:02}", if exp < 0 { '-' } else { '+' }, exp.abs())
    }
}

/// Why a client's answer is not used.
struct ClientFailure(String);

impl Runner {
    /// `gate._dispatch_verify`: the verdict (ok, violations) of the client
    /// config.yaml names, python being the oracle alone, and the oracle's
    /// Z3-timeout warnings, which a client never clears.
    pub fn verify_dispatch(&self, rec: &Record, env: &Envelope) -> R<(bool, Vec<Violation>, Vec<String>)> {
        let client = self.verifier_client()?;
        if client == "python" {
            let (vs, timeouts) = self.verify_record(rec, env, 0)?;
            return Ok((vs.is_empty(), vs, timeouts));
        }
        // verify_via runs the oracle one frame deeper.
        let t0 = Instant::now();
        let (vs, timeouts) = self.verify_record(rec, env, 1)?;
        let oracle_s = t0.elapsed().as_secs_f64();
        let (ok, vs) = self.verify_via(&client, rec, &step_dict(rec), env, vs, oracle_s, self.verify_timeout_s * 0.5)?;
        Ok((ok, vs, timeouts))
    }

    /// `verify_via(client, plan, envelope, timeout_s=budget)`, after the
    /// oracle has run: its violations, and how long it took.
    #[allow(clippy::too_many_arguments)]
    pub fn verify_via(
        &self,
        client: &str,
        rec: &Record,
        step: &Value,
        env: &Envelope,
        oracle: Vec<Violation>,
        oracle_s: f64,
        budget_s: f64,
    ) -> R<(bool, Vec<Violation>)> {
        let left = budget_s - oracle_s;
        let mut verdict: Option<Object> = None;
        let failure = match spec(client) {
            None => format!("no client named {}", super::py::text::repr(client)),
            Some(s) => match client_argv(self, client, &s) {
                None => format!("not built; cd {} && {}", s.build_cwd, s.build_steps.join(" && ")),
                Some(_) if left <= 0.0 => {
                    format!("no time left after the oracle, which took {oracle_s:.2} s of {} s", fmt_g(budget_s))
                }
                Some(argv) => {
                    let case = verify_case(rec, step, env, &oracle)?;
                    match run_client(&argv, &case, left)? {
                        Ok(v) => {
                            verdict = Some(v);
                            String::new()
                        }
                        Err(ClientFailure(m)) => m,
                    }
                }
            },
        };
        let _ = rec;
        let v = match verdict {
            None => {
                self.record_dispatch(client, false, &failure);
                return Ok((oracle.is_empty(), oracle));
            }
            Some(v) => v,
        };
        self.record_dispatch(client, true, "");
        let client_ok = matches!(v.value("ok"), Value::Bool(true));
        let oracle_ok = oracle.is_empty();
        let mut out = oracle;
        if let Value::List(items) = v.value("violations") {
            for item in items {
                let o = item.as_obj().cloned().unwrap_or_default();
                let stage = o.value("stage").as_str().unwrap_or("unknown").to_string();
                let sv = o.value("step").clone();
                out.push(Violation {
                    stage,
                    message: format!("the {client} verifier client refused this step, {}", step_detail(step, &sv)),
                    detail: Some(Object::new().with("step", sv).with("client", client)),
                    remediation: None,
                });
            }
        }
        Ok((oracle_ok && client_ok, out))
    }

    /// `_record_dispatch`: best effort.
    fn record_dispatch(&self, client: &str, ok: bool, error: &str) {
        let dir = Path::new(&self.root).join("verifier");
        let _ = (|| -> std::io::Result<()> {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new().recursive(true).mode(0o700).create(&dir)?;
            let body = Object::new()
                .with("client", client)
                .with("ok", ok)
                .with("error", error)
                .with("at", now_float());
            std::fs::write(dir.join("last_dispatch.json"), dumps(&Value::Obj(body), true))
        })();
    }
}

/// `_step_detail(plan, step_id)`.
fn step_detail(step: &Value, step_id: &Value) -> String {
    let o = step.as_obj().cloned().unwrap_or_default();
    if !super::predicate::py_eq(o.value("id"), step_id) {
        return "no matching step in the plan".into();
    }
    for attr in ["command", "path", "url", "skill_id", "tool"] {
        if let Some(Value::Str(s)) = o.get(attr) {
            if !s.is_empty() {
                return format!("{attr}={s}");
            }
        }
    }
    format!("step type {}", o.value("type").as_str().unwrap_or("unknown"))
}

/// `conformance.make_verify_case(plan, envelope, options, oracle)` as the
/// canonical JSON line the client reads. A lone surrogate anywhere makes
/// the case id's UTF-8 encoding raise, as it does in the oracle.
fn verify_case(rec: &Record, step: &Value, env: &Envelope, oracle: &[Violation]) -> R<String> {
    let _ = rec;
    let mut step_json = step.as_obj().cloned().unwrap_or_default();
    for k in ["metadata", "arguments"] {
        if let Some(v) = step_json.get(k).cloned() {
            step_json.set(k, any_json(&v));
        }
    }
    let plan = Object::new()
        .with("id", "plan_case")
        .with("source", "call-time-gate")
        .with("task", env.task.as_str())
        .with("steps", Value::List(vec![Value::Obj(step_json)]));
    let mut envelope = env.dump.clone();
    envelope.set("id", "env_case");
    for k in ["invariants", "postconditions"] {
        if let Value::List(items) = envelope.value(k).clone() {
            let items: Vec<Value> = items
                .into_iter()
                .map(|it| match it {
                    Value::Obj(mut o) => {
                        let e = any_json(o.value("expr"));
                        o.set("expr", e);
                        Value::Obj(o)
                    }
                    other => other,
                })
                .collect();
            envelope.set(k, Value::List(items));
        }
    }
    let violations: Vec<Value> = oracle
        .iter()
        .map(|v| {
            let step = v.detail.as_ref().map(|d| d.value("step").clone()).unwrap_or(Value::Null);
            Value::Obj(Object::new().with("stage", v.stage.as_str()).with("step", step))
        })
        .collect();
    let mut body = Object::new()
        .with("kind", "verify")
        .with("v", 1i64)
        .with("plan", plan)
        .with("envelope", envelope)
        .with("options", Object::new().with("strict", Value::Null).with("z3_timeout_ms", 500i64))
        .with("expect", Object::new().with("ok", oracle.is_empty()).with("violations", Value::List(violations)));
    let bytes = encode_utf8_strict(&canonical_json(&Value::Obj(body.clone())))?;
    body.set("id", hexdigest(&bytes)[..16].to_string());
    Ok(canonical_json(&Value::Obj(body)))
}

/// `_run_client(argv, case, timeout_s)`: the client's verdict for the case.
fn run_client(argv: &[String], case_line: &str, timeout_s: f64) -> R<Result<Object, ClientFailure>> {
    Ok(match run_client_inner(argv, case_line, timeout_s) {
        Err(Ok(e)) => Err(e),
        Err(Err(undecided_why)) => return undecided(undecided_why),
        Ok(v) => Ok(v),
    })
}

type Outcome<T> = Result<T, Result<ClientFailure, String>>;

fn fail<T>(m: impl Into<String>) -> Outcome<T> {
    Err(Ok(ClientFailure(m.into())))
}

fn run_client_inner(argv: &[String], case_line: &str, timeout_s: f64) -> Outcome<Object> {
    let case_id = match loads(case_line) {
        Ok(Value::Obj(o)) => o.value("id").clone(),
        _ => Value::Null,
    };
    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .env_remove(super::TTY_ENV);
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return fail(format!("could not start: {}", os_error(&e, &argv[0]).msg)),
    };
    let input = format!("{case_line}\n").into_bytes();
    let mut stdin = child.stdin.take();
    let writer = std::thread::spawn(move || {
        if let Some(s) = stdin.as_mut() {
            let _ = s.write_all(&input);
        }
        drop(stdin);
    });
    let mut out = child.stdout.take();
    let mut err = child.stderr.take();
    let reader = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(o) = out.as_mut() {
            let _ = o.read_to_end(&mut b);
        }
        b
    });
    let ereader = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(e) = err.as_mut() {
            let _ = e.read_to_end(&mut b);
        }
        b
    });
    let deadline = Instant::now() + Duration::from_secs_f64(timeout_s.max(0.0));
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break s,
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return fail(format!("timed out after {} s", fmt_g(timeout_s)));
                }
                std::thread::sleep(Duration::from_millis(2));
            }
            Err(e) => return fail(format!("could not start: {e}")),
        }
    };
    let _ = writer.join();
    let stdout = reader.join().unwrap_or_default();
    let stderr = ereader.join().unwrap_or_default();
    let code = status.code().unwrap_or_else(|| {
        use std::os::unix::process::ExitStatusExt;
        -status.signal().unwrap_or(0)
    });
    if code != 0 {
        let e = String::from_utf8_lossy(&stderr).into_owned();
        return fail(format!("exited {code}: {}", e.trim()));
    }
    let text = universal_newlines(&String::from_utf8_lossy(&stdout));
    let mut matches: Vec<Object> = Vec::new();
    for line in splitlines(&text) {
        if super::py::text::strip(line).is_empty() {
            continue;
        }
        let verdict = match loads(line) {
            Ok(v) => v,
            // json.loads raises RecursionError, which is no ValueError.
            Err(LoadError::Recursion) => return Err(Err("a client verdict nested past Python's limit".into())),
            Err(_) => return fail("wrote a non-JSON verdict"),
        };
        if !well_formed(&verdict) {
            return fail("wrote a malformed verdict");
        }
        let o = verdict.as_obj().cloned().unwrap_or_default();
        if super::predicate::py_eq(o.value("id"), &case_id) && o.get("id").is_some() {
            matches.push(o);
        }
    }
    if matches.is_empty() {
        return fail("produced no verdict for the case");
    }
    if matches.len() > 1 {
        return fail(format!("wrote {} verdicts for one case", matches.len()));
    }
    let v = matches.remove(0);
    if v.get("ok").is_none() {
        return fail("error verdict");
    }
    let has_violations = v.get("violations").is_some_and(|x| x.truthy());
    if matches!(v.value("ok"), Value::Bool(true)) && has_violations {
        return fail("inconsistent verdict");
    }
    Ok(v)
}

/// `_well_formed(verdict)`.
fn well_formed(v: &Value) -> bool {
    let o = match v {
        Value::Obj(o) => o,
        _ => return false,
    };
    if o.get("id").is_none() {
        return false;
    }
    if let Some(ok) = o.get("ok") {
        if !matches!(ok, Value::Bool(_)) {
            return false;
        }
    }
    match o.get("violations") {
        None => true,
        Some(Value::List(l)) => l.iter().all(|x| matches!(x, Value::Obj(d) if matches!(d.get("stage"), Some(Value::Str(_))))),
        Some(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn g_format_is_pythons() {
        for (x, want) in [(2.5, "2.5"), (2.0, "2"), (0.1, "0.1"), (1234567.0, "1.23457e+06"), (0.00001, "1e-05")] {
            assert_eq!(fmt_g(x), want, "{x}");
        }
    }
}
