//! `agentic_executor.AgenticExecutor`: an agentic step run as `claude -p`
//! in its workspace, under the call-time gate in enforce mode. The step's
//! child envelope (the caller's own when it names none) is proved inside
//! the caller's envelope before anything starts, registered in a fresh
//! gate root outside the workspace under a session the executor picks,
//! and the sub-agent's tool wall is computed from it. The Go
//! `supervise.Agentic` is the reference.

use std::time::Instant;

use super::executors::Executor;
use super::{ms, str_of, ExecResult};
use crate::gate::py::text::{head, repr};
use crate::gate::pyjson::{loads_py, py_repr, py_type_name, Object, Value};
use crate::llm::{CallError, Client};

/// `verify._AGENTIC_TOOL_CAPABILITIES`: the permission a host tool needs.
fn capability(tool: &str) -> Option<&'static str> {
    Some(match tool {
        "Bash" => "shell",
        "Read" | "Glob" | "Grep" => "file_read",
        "Write" | "Edit" | "MultiEdit" => "file_write",
        "WebFetch" | "WebSearch" => "network",
        _ => return None,
    })
}

/// `tree.DEFAULT_TIMEOUT_MS`, the edge proof's Z3 budget.
const EDGE_TIMEOUT_MS: u32 = 2000;

/// `executor.truncate_output`.
pub fn truncate_output(text: &str, max_bytes: usize) -> String {
    let b = text.as_bytes();
    if b.len() <= max_bytes {
        return text.to_string();
    }
    format!(
        "{}\n... [truncated]",
        String::from_utf8_lossy(&b[..max_bytes])
    )
}

/// The executor for the `agentic` step type.
pub struct Agentic {
    /// The caller's envelope, the authorization ceiling.
    pub envelope: Object,
    /// The sub-agent's model ("haiku").
    pub model: String,
    /// Runs `claude -p`.
    pub claude: Client,
    /// This binary: the gate hook runs `<self> gate check`.
    pub self_path: String,
    /// Where the gate root is made (`tempfile.gettempdir()`).
    pub temp_dir: String,
}

/// `tempfile.mkdtemp(prefix=prefix, dir=dir)`: a fresh directory, 0700.
fn mkdtemp(dir: &str, prefix: &str) -> std::io::Result<String> {
    use std::io::Read;
    use std::os::unix::fs::DirBuilderExt;
    const NAMES: &[u8] = b"abcdefghijklmnopqrstuvwxyz0123456789_";
    for _ in 0..100 {
        let mut b = [0u8; 8];
        std::fs::File::open("/dev/urandom")?.read_exact(&mut b)?;
        let suffix: String = b
            .iter()
            .map(|x| NAMES[*x as usize % NAMES.len()] as char)
            .collect();
        let d = format!("{dir}/{prefix}{suffix}");
        match std::fs::DirBuilder::new().mode(0o700).create(&d) {
            Ok(()) => return Ok(d),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::from(std::io::ErrorKind::AlreadyExists))
}

/// `int(v)` on a decoded JSON value, for its error only: Ok when int()
/// takes it, or when it is None (left out of the sum).
fn py_int(v: &Value) -> Result<(), String> {
    match v {
        Value::Null | Value::Bool(_) | Value::Int(_) | Value::Float(_) => Ok(()),
        Value::Str(s) => {
            let t = s.trim().replace('_', "");
            let t = t
                .strip_prefix('+')
                .or_else(|| t.strip_prefix('-'))
                .unwrap_or(&t);
            if !t.is_empty() && t.chars().all(|c| c.is_ascii_digit()) {
                Ok(())
            } else {
                Err(format!(
                    "invalid literal for int() with base 10: {}",
                    repr(s)
                ))
            }
        }
        other => Err(format!(
            "int() argument must be a string, a bytes-like object or a real number, not '{}'",
            py_type_name(other)
        )),
    }
}

impl Executor for Agentic {
    fn run(
        &mut self,
        step: &Object,
        timeout_s: u64,
        max_output_bytes: usize,
    ) -> Result<ExecResult, String> {
        let started = Instant::now();
        let fail = |msg: String| -> Result<ExecResult, String> {
            Ok(ExecResult {
                rc: 1,
                stdout: truncate_output(&msg, max_output_bytes),
                duration_ms: ms(started),
                ..Default::default()
            })
        };
        let workspace = str_of(step, "workspace");
        if !std::fs::metadata(&workspace).is_ok_and(|m| m.is_dir()) {
            return fail(format!(
                "agentic workspace '{workspace}' does not exist or is not a directory"
            ));
        }
        // The edge: the sub-agent's envelope must fit inside the caller's,
        // strict and fail closed, before anything starts. A step with no
        // child envelope restates the caller's own, which fits.
        let child = match step.value("child_envelope") {
            Value::Obj(c) => c.clone(),
            _ => self.envelope.clone(),
        };
        let edge = crate::tree::edge_ok(Some(&self.envelope), Some(&child), EDGE_TIMEOUT_MS);
        let registered = match (edge.holds, edge.child) {
            (true, Some(c)) => c,
            _ => {
                return fail(format!(
                    "the child envelope is refused: {}",
                    edge.reasons.join("; ")
                ))
            }
        };
        let mut registered = registered.clone();
        registered.set("parent_envelope", self.envelope.value("id").clone());

        let tools: Vec<Value> = match step.value("tools") {
            Value::List(l) => l.clone(),
            _ => vec![],
        };
        let perms = registered
            .value("permissions")
            .as_obj()
            .cloned()
            .unwrap_or_default();
        let allowed: Vec<String> = tools
            .iter()
            .filter_map(|t| t.as_str())
            .filter(|t| capability(t).is_some_and(|c| perms.value(c).truthy()))
            .map(|t| t.to_string())
            .collect();
        if allowed.is_empty() {
            let requested = py_repr(step.value("tools")).unwrap_or_default();
            return fail(format!(
                "no requested tool is backed by the envelope (requested {requested}); nothing to delegate"
            ));
        }

        // The gate root is made outside the workspace: the sub-agent must
        // not be able to rewrite its own hook settings or envelope.
        let root = mkdtemp(&self.temp_dir, "daisugi-agentic-gate-").map_err(|e| e.to_string())?;
        // The executor, not the sub-agent, picks the session the envelope
        // binds to, and pins the gate to it.
        let session = format!(
            "agentic-{}",
            crate::cli::gateroot::safe_session_id(&str_of(step, "id"))
        );
        let (_, _, written) = crate::cli::envelope::register(&registered, &session, &root);
        written.map_err(|e| e.to_string())?;
        let settings = crate::cli::install::settings_json(
            &self.self_path,
            &crate::cli::install::HookOptions {
                mode: "enforce".into(),
                root: root.clone(),
                format: "claude".into(),
                captures_root: Some(format!("{root}/captures")),
                session: Some(session.clone()),
                ask: false,
            },
        );
        let wall = allowed.join(" ");
        let mut extra: Vec<&str> = vec![
            "--output-format",
            "json",
            "--settings",
            &settings,
            "--allowedTools",
            &wall,
        ];
        let turns = match step.value("max_turns") {
            Value::Int(t) => Some(t.clone()),
            _ => None,
        };
        if let Some(t) = &turns {
            extra.push("--max-turns");
            extra.push(t);
        }
        let prompt = str_of(step, "prompt");
        let raw = match self.claude.sync_text_in(
            Some(&workspace),
            &prompt,
            &self.model,
            timeout_s as f64,
            &extra,
        ) {
            Ok(r) => r,
            Err(CallError::Model(m)) => return fail(format!("agentic sub-agent failed: {m}")),
            Err(CallError::Os(e)) => return fail(format!("agentic sub-agent failed: {e}")),
            Err(CallError::Unported(w)) => return Err(w),
        };
        let parsed = match loads_py(&raw, 900) {
            Ok(v) => v,
            Err(_) => {
                return fail(format!(
                    "agentic sub-agent returned unparseable output: {}",
                    repr(head(&raw, 300))
                ));
            }
        };
        let Value::Obj(obj) = parsed else {
            // obj.get raises AttributeError out of the executor.
            return Err(format!(
                "'{}' object has no attribute 'get'",
                py_type_name(&parsed)
            ));
        };
        // The meter sums the token counts with int(); a count int()
        // refuses raises out of the executor.
        if let Value::Obj(usage) = obj.value("usage") {
            for f in [
                "input_tokens",
                "output_tokens",
                "cache_creation_input_tokens",
                "cache_read_input_tokens",
            ] {
                py_int(usage.value(f))?;
            }
        }
        let result = match obj.value("result") {
            Value::Str(s) => s.clone(),
            v if v.truthy() => py_repr(v).unwrap_or_default(),
            _ => String::new(),
        };
        if obj.value("is_error").truthy() {
            return fail(format!(
                "agentic sub-agent reported is_error: {}",
                head(&result, 500)
            ));
        }
        Ok(ExecResult {
            rc: 0,
            stdout: truncate_output(&result, max_output_bytes),
            duration_ms: ms(started),
            ..Default::default()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    /// Removes its directories when dropped, so a test leaves no files.
    struct Gone(Vec<String>);

    impl Drop for Gone {
        fn drop(&mut self) {
            for d in &self.0 {
                let _ = std::fs::remove_dir_all(d);
            }
        }
    }

    fn tmp(name: &str) -> String {
        use std::sync::atomic::{AtomicU64, Ordering};
        static N: AtomicU64 = AtomicU64::new(0);
        let d = std::env::temp_dir().join(format!(
            "agentic-test-{}-{}-{name}",
            std::process::id(),
            N.fetch_add(1, Ordering::SeqCst)
        ));
        std::fs::create_dir_all(&d).unwrap();
        d.to_string_lossy().to_string()
    }

    fn envelope(id: &str, read: &[&str], shell: bool) -> Object {
        let reads = Value::List(read.iter().map(|r| Value::Str(r.to_string())).collect());
        let input = Object::new()
            .with("id", id)
            .with("generated_by", "test")
            .with("task", "t")
            .with(
                "permissions",
                Object::new().with("file_read", reads).with("shell", shell),
            );
        crate::cli::envelope::validate(&input)
            .ok()
            .expect("a valid envelope")
    }

    /// A `claude` that writes its argv, one word a line, to `args` and
    /// prints `out`; the client finds it on PATH.
    fn agent(dir: &str, out: &str) -> Client {
        let bin = format!("{dir}/claude");
        let script =
            format!("#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done > {dir}/args\ncat > /dev/null\nprintf '%s' '{out}'\n");
        std::fs::write(&bin, script).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut env = HashMap::new();
        env.insert("PATH".to_string(), format!("{dir}:/usr/bin:/bin"));
        Client::new(env, dir)
    }

    fn step(ws: &str, tools: &[&str], child: Option<Object>) -> Object {
        let ts = Value::List(tools.iter().map(|t| Value::Str(t.to_string())).collect());
        let mut s = Object::new()
            .with("id", "g")
            .with("depends_on", Value::List(vec![]))
            .with("type", "agentic")
            .with("prompt", "fix it")
            .with("workspace", ws)
            .with("tools", ts)
            .with("max_turns", Value::Null);
        if let Some(c) = child {
            s.set("child_envelope", c);
        }
        s
    }

    #[test]
    fn runs_under_the_gate() {
        let (dir, ws, temp) = (tmp("bin"), tmp("ws"), tmp("t"));
        let _gone = Gone(vec![dir.clone(), ws.clone(), temp.clone()]);
        let parent = envelope("env_parent", &[&format!("{ws}/**")], true);
        let child = envelope("env_child", &[&format!("{ws}/**")], false);
        let mut a = Agentic {
            envelope: parent,
            model: "haiku".into(),
            claude: agent(
                &dir,
                r#"{"type": "result", "is_error": false, "result": "done"}"#,
            ),
            self_path: "/opt/daisugi".into(),
            temp_dir: temp.clone(),
        };
        let r = a
            .run(&step(&ws, &["Read", "Bash"], Some(child)), 30, 1000)
            .unwrap();
        assert_eq!((r.rc, r.stdout.as_str()), (0, "done"));
        let args = std::fs::read_to_string(format!("{dir}/args")).unwrap();
        let words: Vec<&str> = args.trim_end().split('\n').collect();
        assert_eq!(&words[..2], &["-p", "--model=haiku"]);
        assert_eq!(&words[words.len() - 2..], &["--allowedTools", "Read"]);
        let roots: Vec<_> = std::fs::read_dir(&temp)
            .unwrap()
            .map(|e| e.unwrap().path())
            .collect();
        assert_eq!(roots.len(), 1);
        let root = roots[0].to_string_lossy().to_string();
        assert!(root.contains("/daisugi-agentic-gate-"));
        assert!(args.contains(&format!(
            "/opt/daisugi gate check --mode enforce --root {root}"
        )));
        assert!(args.contains(&format!(
            "--captures-root {root}/captures --session agentic-g"
        )));
        let f = format!("{root}/envelopes/agentic-g.json");
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(&f).unwrap().permissions().mode() & 0o777,
            0o600
        );
        let text = std::fs::read_to_string(&f).unwrap();
        assert!(
            text.contains(r#""id": "env_child""#)
                && text.contains(r#""parent_envelope": "env_parent""#)
        );
    }

    #[test]
    fn fails_closed() {
        let (dir, ws, temp) = (tmp("bin"), tmp("ws"), tmp("t"));
        let _gone = Gone(vec![dir.clone(), ws.clone(), temp.clone()]);
        let parent = envelope("env_parent", &[&format!("{ws}/**")], true);
        let mut a = Agentic {
            envelope: parent,
            model: "haiku".into(),
            claude: agent(
                &dir,
                r#"{"type": "result", "is_error": true, "result": "no"}"#,
            ),
            self_path: "/opt/daisugi".into(),
            temp_dir: temp,
        };
        let narrow = envelope("env_c", &[&format!("{ws}/**")], false);
        let cases = [
            (
                step(&format!("{ws}/missing"), &["Read"], None),
                format!("agentic workspace '{ws}/missing' does not exist or is not a directory"),
            ),
            (step(&ws, &["Read"], Some(envelope("env_c", &["/**"], false))), "the child envelope is refused: ".into()),
            (
                step(&ws, &["Bash"], Some(narrow)),
                "no requested tool is backed by the envelope (requested ['Bash']); nothing to delegate".into(),
            ),
            (step(&ws, &["Read"], None), "agentic sub-agent reported is_error: no".into()),
        ];
        for (s, want) in cases {
            let r = a.run(&s, 30, 1000).unwrap();
            assert_eq!(r.rc, 1);
            assert!(
                r.stdout.starts_with(&want),
                "{} does not start with {want}",
                r.stdout
            );
        }
    }
}
