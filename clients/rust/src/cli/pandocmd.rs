//! `daisugi pando`: Pando R2, the conversation memory, over the store under
//! the data home (ADR-0023). The verbs live in the pando crate, which opens
//! no socket; this file gives it the data home and the model behind tend.
//! The Go client's `cli/pandocmd.go` is the twin.

use std::io::Read;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use pando::chat::Summarizer;
use pando::compactor::{CallError, CallUsage, ModelSummarizer, COMPACT_TIMEOUT, DEFAULT_MAX_TOKENS};
use pando::ctx::Ctx;

use super::{exit, Env, Res};

impl Env {
    pub(super) fn pando_cmd(&mut self, args: &[String]) -> Res {
        let data_home = PathBuf::from(self.data_home());
        let home = PathBuf::from(
            self.env
                .get("HOME")
                .cloned()
                .unwrap_or_else(|| self.home.clone()),
        );
        let mut input = std::io::BufReader::new(std::io::stdin());
        let (mut out, mut err) = (Vec::new(), Vec::new());
        let code = {
            let this: &Env = self;
            let mk = move |profile: &str| this.pando_summarizer(profile);
            let hroot = crate::hermesdb::root(
                this.env.get("HERMES_HOME").map(String::as_str).unwrap_or(""),
                &home.to_string_lossy(),
            );
            let hmk = move |profile: &str| -> Result<Box<dyn pando::live::Source>, String> {
                Ok(Box::new(crate::hermesdb::HermesSource::new(hroot.clone(), profile)))
            };
            let genv = |k: &str| this.env.get(k).cloned().unwrap_or_default();
            pando::cli::run(
                args,
                &mut pando::cli::CliEnv {
                    stdin: &mut input,
                    out: &mut out,
                    err: &mut err,
                    data_home,
                    home,
                    new_summarizer: Some(&mk),
                    now: None,
                    embed: crate::pathways::lexical::vector,
                    progress: Some(std::sync::Arc::new(std::sync::Mutex::new(std::io::stderr()))),
                    hermes_source: Some(&hmk),
                    getenv: Some(&genv),
                },
            )
        };
        self.out(&String::from_utf8_lossy(&out));
        self.errf(&String::from_utf8_lossy(&err));
        if code != 0 {
            return exit(code);
        }
        Ok(())
    }

    /// The model behind `tend` and `import`. An empty profile is Claude Code
    /// through `claude -p` (haiku); any other name is an agent profile asked
    /// over HTTP.
    fn pando_summarizer(&self, profile: &str) -> Result<Box<dyn Summarizer>, String> {
        let env = self.env.clone();
        if profile.is_empty() || profile == crate::agentprof::DEFAULT {
            let path = env.get("PATH").cloned().unwrap_or_default();
            let mut m = ModelSummarizer::new(Box::new(move |_: &Ctx, system: &str, prompt: &str| {
                claude_call(&path, &env, "haiku", system, prompt)
            }));
            m.timeout = COMPACT_TIMEOUT;
            return Ok(Box::new(m));
        }
        let table = crate::agentprof::configured_table(&self.data_home());
        let profiles = crate::agentprof::resolve(table.as_ref(), &|k| self.env.get(k).cloned());
        let p = profiles.lookup(Some(profile))?;
        let (model, base, key) = (
            p.model.clone().unwrap_or_default(),
            p.base_url.clone().unwrap_or_default(),
            p.key.clone().unwrap_or_default(),
        );
        let max_tokens = p.max_tokens.unwrap_or(DEFAULT_MAX_TOKENS) as i64;
        // timeout_s caps the call; the HTTP client waits at least that long.
        let timeout_s = p.timeout_s;
        let thinking = p.thinking.clone();
        let wire = p.wire.clone();
        let client = self.llm_client();
        let mut m = ModelSummarizer::new(Box::new(move |_: &Ctx, system: &str, prompt: &str| {
            let msgs = vec![
                ("system".to_string(), system.to_string()),
                ("user".to_string(), prompt.to_string()),
            ];
            let get = |k: &str| env.get(k).cloned();
            let mut t = crate::llm::wire::timeout(&get);
            if let Some(s) = timeout_s {
                t = t.max(s as f64);
            }
            let opts = thinking_opts(thinking.as_deref(), &wire, max_tokens);
            match client.complete_with(&model, &base, &key, &msgs, &opts, t) {
                // A reply with no text that stopped for length spent all its
                // tokens on reasoning. It is a call that gave no line: the
                // compactor counts it as a fallback. A separate reasoning
                // field is never read.
                Ok(r) if r.text.trim().is_empty() && r.cut => Ok((
                    String::new(),
                    CallUsage {
                        stop: "length",
                        ..Default::default()
                    },
                )),
                Ok(r) => Ok((r.text, CallUsage::default())),
                Err(crate::llm::CallError::Model(msg)) => Err(CallError {
                    detail: msg.clone(),
                    msg,
                    ran: true,
                    ..Default::default()
                }),
                Err(crate::llm::CallError::Os(e)) => Err(e.to_string().into()),
                Err(crate::llm::CallError::Unported(m)) => Err(m.into()),
            }
        }));
        m.timeout = timeout_s.map_or(COMPACT_TIMEOUT, |s| Duration::from_secs(s as u64));
        Ok(Box::new(m))
    }
}

fn look_path(bin: &str, path: &str) -> Option<PathBuf> {
    path.split(':')
        .filter(|d| !d.is_empty())
        .map(|d| PathBuf::from(d).join(bin))
        .find(|p| {
            use std::os::unix::fs::PermissionsExt;
            std::fs::metadata(p).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
        })
}

/// Asks Claude Code for one answer: `claude -p` in an empty dir, no tools,
/// no saved session.
fn claude_call(
    path: &str,
    env: &std::collections::HashMap<String, String>,
    model: &str,
    system: &str,
    prompt: &str,
) -> Result<(String, CallUsage), CallError> {
    let Some(bin) = look_path("claude", path) else {
        return Err(CallError::from(
            "tend needs Claude Code (claude) on PATH, or --profile NAME of an agent profile".to_string(),
        ));
    };
    let dir = std::env::temp_dir().join(format!("pando-tend-{}-{}", std::process::id(), Instant::now().elapsed().as_nanos()));
    std::fs::create_dir_all(&dir).map_err(|e| CallError::from(e.to_string()))?;
    let res = (|| {
        let start = Instant::now();
        let mut child = Command::new(&bin)
            .args(["-p", "--model", model, "--tools", "", "--no-session-persistence", "--system-prompt", system])
            .current_dir(&dir)
            .env_clear()
            .envs(env)
            .env(pando::live::SKIP_VAR, "1") // a model call of ours, not the owner's chat
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|e| CallError::from(e.to_string()))?;
        let mut stdin = child.stdin.take();
        let p = prompt.to_string();
        std::thread::spawn(move || {
            use std::io::Write;
            if let Some(s) = stdin.as_mut() {
                let _ = s.write_all(p.as_bytes());
            }
        });
        let (mut so, mut se) = (child.stdout.take().unwrap(), child.stderr.take().unwrap());
        let t_out = std::thread::spawn(move || {
            let mut b = Vec::new();
            let _ = so.read_to_end(&mut b);
            b
        });
        let t_err = std::thread::spawn(move || {
            let mut b = Vec::new();
            let _ = se.read_to_end(&mut b);
            b
        });
        let limit = 2 * COMPACT_TIMEOUT;
        let status = loop {
            match child.try_wait() {
                Ok(Some(s)) => break Ok(s),
                Ok(None) if start.elapsed() > limit => {
                    let _ = child.kill();
                    break child.wait();
                }
                Ok(None) => std::thread::sleep(Duration::from_millis(20)),
                Err(e) => break Err(e),
            }
        };
        let (out, errb) = (t_out.join().unwrap_or_default(), t_err.join().unwrap_or_default());
        match status {
            Ok(s) if s.success() => Ok((String::from_utf8_lossy(&out).trim().to_string(), CallUsage::default())),
            other => {
                let mut msg = String::from_utf8_lossy(&errb).trim().to_string();
                if msg.len() > 400 {
                    let mut at = msg.len() - 400;
                    while !msg.is_char_boundary(at) {
                        at += 1;
                    }
                    msg = msg[at..].to_string();
                }
                let why = match other {
                    // Go writes "exit status N"; newer Rust writes "exit status: N".
                    Ok(s) => match s.code() {
                        Some(c) => format!("exit status {c}"),
                        None => format!("{s}"),
                    },
                    Err(e) => e.to_string(),
                };
                Err(CallError {
                    msg: format!("claude -p failed after {}s: {why}", start.elapsed().as_secs()),
                    detail: msg.clone(),
                    stderr: msg,
                    ran: true,
                })
            }
        }
    })();
    let _ = std::fs::remove_dir_all(&dir);
    res
}

/// The request options for a profile's thinking value. On the openai wire
/// it is reasoning_effort (off is "none"). On the anthropic wire off sends
/// nothing and the others turn on extended thinking with a token budget;
/// max_tokens grows by the budget so the summary keeps the tokens the
/// profile gave it.
pub fn thinking_opts(level: Option<&str>, wire: &str, max_tokens: i64) -> crate::llm::wire::Opts {
    let mut o = crate::llm::wire::Opts {
        max_tokens: Some(max_tokens),
        ..Default::default()
    };
    let Some(level) = level else { return o };
    if wire == "anthropic" {
        let budget = match level {
            "low" => 1024,
            "medium" => 4096,
            "high" => 16000,
            _ => 0,
        };
        if budget > 0 {
            o.thinking_budget = budget;
            o.max_tokens = Some(max_tokens + budget);
        }
        return o;
    }
    o.reasoning_effort = if level == "off" { "none".into() } else { level.into() };
    o
}

#[cfg(test)]
mod thinking_tests {
    use super::thinking_opts;

    #[test]
    fn thinking_per_wire() {
        for (level, wire, effort, budget, max) in [
            ("off", "openai", "none", 0, 1024),
            ("low", "openai", "low", 0, 1024),
            ("high", "openai", "high", 0, 1024),
            ("off", "anthropic", "", 0, 1024),
            ("low", "anthropic", "", 1024, 2048),
            ("medium", "anthropic", "", 4096, 5120),
            ("high", "anthropic", "", 16000, 17024),
        ] {
            let o = thinking_opts(Some(level), wire, 1024);
            assert_eq!((o.reasoning_effort.as_str(), o.thinking_budget, o.max_tokens), (effort, budget, Some(max)), "{level}/{wire}");
        }
        assert_eq!(thinking_opts(None, "openai", 7).max_tokens, Some(7));
    }
}
