//! `daisugi journal parse` and `daisugi journal ingest`: an agent
//! transcript read into episodes, and episodes journaled as verified
//! traces, with the flags, output, exit codes and files of the Python CLI.
//! The Go client's `journalparse.go` is the reference; rulings C-10, C-14
//! and C-16 say where it differs from Python.

use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use super::config;
use super::gatecmds::DECOMPOSE_OPT;
use super::gateroot::{self, join, path_str};
use super::journalcmd::{result_dump, JOURNAL_DATA_DIR};
use super::statuscmd::load_json;
use super::{exit, parse_args, Env, Opt, Res};
use crate::capture::infer_envelope;
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::{dumps_indent, py_type_name, Object, Value};
use crate::llm::{self, CallError, Client};
use crate::pathways::pmodel::decode_dict_text;
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;
use crate::tracejournal::{self, iso_seconds, trace_body};
use crate::transcript::{self, Fail};

const SPLIT_SYSTEM_PROMPT: &str = "You split a sequence of agent tool calls into logical sub-tasks.

Given a user message and a numbered list of tool calls, identify coherent sub-tasks.
Each sub-task should represent a focused unit of work.

Return a JSON object with a \"subtasks\" array. Each subtask has:
- \"start_index\": first tool call index (inclusive)
- \"end_index\": last tool call index (inclusive)
- \"task\": short description of this sub-task (imperative form)

Rules:
- Every tool call must belong to exactly one subtask.
- Subtasks must be contiguous and ordered by index.
- Aim for 5-15 tool calls per subtask.
";

/// journal parse's --model default.
pub(super) const SPLIT_DEFAULT_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

/// The seconds since the epoch, now.
fn now_secs() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0)
}

/// "YYYY-MM-DDTHH:MM:SSZ" of now, in UTC.
pub(super) fn now_iso() -> String {
    format!("{}Z", iso_seconds(now_secs()))
}

/// `click.Path(exists=True, dir_okay=False, readable=True)` on an
/// argument: the usage error click gives, or None.
fn click_file(name: &str, path: &str) -> Option<String> {
    match std::fs::metadata(path) {
        Err(_) => return Some(format!("Invalid value for '{name}': File '{path}' does not exist.")),
        Ok(m) if m.is_dir() => return Some(format!("Invalid value for '{name}': File '{path}' is a directory.")),
        Ok(_) => {}
    }
    let c = std::ffi::CString::new(path).ok()?;
    // SAFETY: access() reads a NUL-terminated path and nothing else.
    if unsafe { libc::access(c.as_ptr(), libc::R_OK) } != 0 {
        return Some(format!("Invalid value for '{name}': File '{path}' is not readable."));
    }
    None
}

/// `journal._TRACE_ID_RE`.
pub(super) fn trace_id_ok(id: &str) -> bool {
    !id.is_empty() && id.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
}

/// One episode's outcome (`ingest.EpisodeResult`), with what a real run
/// journals for it.
#[derive(Default)]
pub(super) struct Item {
    pub(super) id: String,
    pub(super) task: String,
    pub(super) status: String,
    pub(super) steps: usize,
    pub(super) violations: usize,
    /// The error of an ERROR episode.
    pub(super) err: Option<String>,
    pub(super) trace_id: String,
    pub(super) env: Object,
    pub(super) plan: Object,
    pub(super) result: Object,
    /// load_trace raised something other than not found: the oracle stops
    /// there with a traceback.
    pub(super) load_err: Option<(String, String)>,
}

impl Env {
    /// console.note: one line on stderr, silent under -q.
    pub(super) fn note(&mut self, s: &str) {
        if !self.quiet {
            self.errf(&format!("{s}\n"));
        }
    }

    pub(super) fn llm_client(&self) -> Client {
        Client::new(self.env.clone(), &self.home)
    }

    /// The gate part of `cli._echo_resolved`, which it works out before
    /// it resolves the backend.
    fn gate_state(&self) -> Result<String, String> {
        let data = self.data_home();
        if gateroot::is_disarmed(&join(&data, "gate")) {
            return Ok("disarmed".into());
        }
        config::gate_mode(&join(&data, "config.yaml")).map_err(|e| e.to_string())
    }

    /// `cli._echo_resolved(DEFAULT_DATA_DIR)`: the backend, the gate's
    /// state and the data directory, on stderr.
    fn echo_resolved(&mut self, cmd: &str) -> Res {
        let gate = match self.gate_state() {
            Ok(g) => g,
            Err(why) => return self.refuse(cmd, &why),
        };
        let backend = self.llm_client().backend();
        let shown = self.tilde(&self.data_home());
        self.note(&format!("backend: {backend} · gate: {gate} · data: {shown}"));
        Ok(())
    }

    /// `cli._check_llm_flag`: a value that names no backend is one line,
    /// exit 2; the old name gets the line that names api.
    pub(super) fn check_llm_flag(&mut self, v: &str) -> Res {
        match v {
            "api" | "claude-code" => return Ok(()),
            "litellm" => self.errf(&format!("{}\n", llm::RENAMED_TEXT)),
            _ => self.errf(&format!("Invalid --llm value {}. Must be 'api' or 'claude-code'.\n", repr(v))),
        }
        exit(2)
    }

    /// `_echo_resolved` on the old backend name: resolve_backend raises
    /// LLMNotConfigured, which main() prints as one line, exit 1.
    fn renamed_backend(&mut self, cmd: &str) -> Res {
        if !llm::renamed(&self.llm_client().backend()) {
            return Ok(());
        }
        if let Err(why) = self.gate_state() {
            return self.refuse(cmd, &why);
        }
        self.errf(&format!("{}\n", llm::RENAMED_TEXT));
        exit(1)
    }

    /// `_call_split_llm` on the claude-code backend: `claude -p
    /// --model=haiku [DAISUGI_CLAUDE_ARGS]` with the prompt on stdin, 60 s,
    /// in a fresh directory. Any failure is None: the episode stays whole.
    pub(super) fn split_claude(&self, content: &str, neutral: &mut Option<String>) -> transcript::R<Option<Value>> {
        let prompt = format!("[system]\n{SPLIT_SYSTEM_PROMPT}\n\n[user]\n{content}");
        let Some(bin) = super::install::look_path(&self.env, "claude") else { return Ok(None) };
        let mut args = vec!["-p".to_string(), "--model=haiku".to_string()];
        let raw = strip(self.env.get("DAISUGI_CLAUDE_ARGS").map(String::as_str).unwrap_or(""));
        if !raw.is_empty() {
            if let Ok(extra) = crate::interpreter_parse::shlex_split(raw) {
                args.extend(extra);
            }
        }
        if neutral.is_none() {
            match mkdtemp(self.env.get("TMPDIR").map(String::as_str).unwrap_or("")) {
                Ok(d) => *neutral = Some(d),
                Err(e) => return Err(Fail::Unreadable(format!("the split's working directory: {e}"))),
            }
        }
        let dir = neutral.clone().unwrap_or_default();
        let child = Command::new(&bin)
            .args(&args)
            .current_dir(&dir)
            .env_clear()
            .envs(&self.env)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn();
        let Ok(mut child) = child else { return Ok(None) };
        let mut stdin = child.stdin.take();
        let input = prompt.into_bytes();
        let writer = std::thread::spawn(move || {
            if let Some(s) = stdin.as_mut() {
                let _ = s.write_all(&input);
            }
        });
        let mut pipe = child.stdout.take();
        let reader = std::thread::spawn(move || {
            let mut b = vec![];
            if let Some(p) = pipe.as_mut() {
                let _ = p.read_to_end(&mut b);
            }
            b
        });
        let ok = super::statuscmd::wait_timeout(child, Duration::from_secs(60)).is_some_and(|s| s.success());
        let _ = writer.join();
        let out = reader.join().unwrap_or_default();
        if !ok {
            return Ok(None);
        }
        let Ok(text) = String::from_utf8(out) else {
            return Err(Fail::Unreadable("claude wrote text that is not UTF-8".into()));
        };
        // text=True reads the output with universal newlines.
        let text = text.replace("\r\n", "\n").replace('\r', "\n");
        let text = strip(&text);
        let (start, end) = match (text.find('{'), text.rfind('}')) {
            (Some(a), Some(b)) if b > a => (a, b),
            _ => return Ok(None),
        };
        let body = &text[start..=end];
        let obj = match load_json(body) {
            Ok(Value::Obj(o)) => Some(o),
            Ok(_) => None,
            Err(_) => {
                let d = decode_dict_text(body);
                if !d.refuse.is_empty() {
                    return Err(Fail::Unreadable(format!("claude wrote a reply that holds {}", d.refuse)));
                }
                d.dict
            }
        };
        let Some(obj) = obj else { return Ok(None) };
        Ok(Some(obj.get("subtasks").cloned().unwrap_or(Value::List(vec![]))))
    }

    /// `_call_split_llm` on the api backend: one `llm_client.complete`
    /// with json_object, the reply's text read by json.loads, and
    /// `body.get("subtasks", [])`. Every failure fails the parse.
    pub(super) fn split_api(&self, model: &str, content: &str) -> transcript::R<Option<Value>> {
        let msgs = vec![("system".to_string(), SPLIT_SYSTEM_PROMPT.to_string()), ("user".to_string(), content.to_string())];
        let text = match self.llm_client().complete_json(model, &msgs) {
            Ok(t) => t,
            Err(CallError::Model(m)) => return Err(Fail::Parse(m)),
            Err(CallError::Unported(why)) => return Err(Fail::Unreadable(why)),
            Err(CallError::Os(e)) => return Err(Fail::Unreadable(e.to_string())),
        };
        let v = match load_json(&text) {
            Ok(v) => v,
            Err(super::statuscmd::JsonErr::Raised(m)) => return Err(Fail::Parse(m)),
            Err(super::statuscmd::JsonErr::Unread) => {
                return Err(Fail::Unreadable("a split reply this binary does not read as json.loads does".into()))
            }
        };
        match v {
            Value::Obj(o) => Ok(Some(o.get("subtasks").cloned().unwrap_or(Value::List(vec![])))),
            other => Err(Fail::Parse(format!("'{}' object has no attribute 'get'", py_type_name(&other)))),
        }
    }

    pub(super) fn journal_parse(&mut self, args: &[String]) -> Res {
        const CMD: &str = "journal parse";
        let opts = [
            Opt::val(&["-o", "--output"], "PATH", "Path to write the episodes YAML/JSON file."),
            Opt::val(&["--format"], "TEXT", "Parser format (default: claude-code)."),
            Opt::val(&["--min-tools"], "INTEGER", "Merge episodes below this tool-call threshold."),
            Opt::val(&["--max-tools"], "INTEGER", "LLM-split episodes above this tool-call threshold."),
            Opt::val(&["--model"], "TEXT", "Model for LLM splitting (rarely needed)."),
            Opt::flag(&["--json"], "Write JSON instead of YAML."),
            Opt::val(&["--llm"], "TEXT", "LLM backend: api | claude-code. Default: auto-detect."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " TRANSCRIPT", "Parse an agent transcript into episodes.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "TRANSCRIPT", "TRANSCRIPT");
        }
        let path = p.args[0].clone();
        if let Some(m) = click_file("transcript", &path) {
            return self.usage(CMD, m);
        }
        let min_tools = self.click_int(CMD, &p, "--min-tools", 3)?;
        let max_tools = self.click_int(CMD, &p, "--max-tools", 30)?;
        if !p.has("-o") {
            return self.usage(CMD, "Missing option '-o' / '--output'.".into());
        }
        let output = p.str("-o", "");
        if p.has("--llm") {
            let v = p.str("--llm", "");
            self.check_llm_flag(&v)?;
            self.env.insert("OPENDAISUGI_LLM_BACKEND".into(), v);
        }
        self.renamed_backend(CMD)?;
        let format = p.str("--format", "claude-code");
        if format != "claude-code" && format != "codex" {
            self.echo_resolved(CMD)?;
            self.errf(&format!("Unknown parser format: {}. Available: ['claude-code', 'codex']\n", repr(&format)));
            return exit(2);
        }
        // The transcript is read before the note Python prints first, so a
        // refusal is the run's one line. An exception the parse raises is
        // printed after the note, as Python prints it.
        let raw = match std::fs::read(&path) {
            Ok(r) => r,
            Err(e) => return Err(self.pw_err(CMD, PwErr::Io(e))),
        };
        let eps = match transcript::read(&raw, &format) {
            Ok(e) => e,
            Err(Fail::Unreadable(why)) => return self.refuse(CMD, &why),
            Err(Fail::Parse(m)) => {
                self.echo_resolved(CMD)?;
                self.errf(&format!("Parse error: {m}\n"));
                return exit(2);
            }
        };
        let eps = transcript::merge_small(eps, min_tools);
        // Splitting asks a model. On the api backend a proxy setting the
        // binary does not read as httpx does is refused before any call.
        let model = p.str("--model", SPLIT_DEFAULT_MODEL);
        let via_api = self.llm_client().backend() != "claude-code";
        if via_api && eps.iter().any(|ep| ep.steps() as i128 > max_tools) {
            if let Err(why) = self.llm_client().check(&model) {
                return self.refuse(CMD, &why);
            }
        }
        self.echo_resolved(CMD)?;
        let mut neutral: Option<String> = None;
        let split = {
            let me = &*self;
            let neutral = &mut neutral;
            transcript::split_large(eps, max_tools, &mut |content: &str| {
                if via_api {
                    me.split_api(&model, content)
                } else {
                    me.split_claude(content, neutral)
                }
            })
        };
        if let Some(d) = &neutral {
            let _ = std::fs::remove_dir_all(d);
        }
        let finalized = split.and_then(|eps| Ok((eps.len(), transcript::finalize(&eps)?)));
        let (count, episodes) = match finalized {
            Ok(e) => e,
            Err(Fail::Parse(m)) => {
                self.errf(&format!("Parse error: {m}\n"));
                return exit(2);
            }
            Err(Fail::Unreadable(why)) => return self.refuse(CMD, &why),
        };
        let payload = Value::Obj(
            Object::new()
                .with("source", format.as_str())
                .with("source_file", path_str(&path))
                .with("parsed_at", now_iso())
                .with("episodes", Value::List(episodes)),
        );
        let text = if p.flag("--json") {
            dumps_indent(&payload, 2, true)
        } else {
            match crate::pathways::yamldump::safe_dump(&payload) {
                Ok(t) => t,
                Err(w) => return self.refuse(CMD, &w.0),
            }
        };
        if let Err(e) = std::fs::write(&output, text) {
            return Err(self.pw_err(CMD, PwErr::Io(e)));
        }
        self.out(&format!("Parsed {count} episodes to {}\n", path_str(&output)));
        Ok(())
    }

    pub(super) fn journal_ingest(&mut self, args: &[String]) -> Res {
        const CMD: &str = "journal ingest";
        let opts = [
            JOURNAL_DATA_DIR,
            Opt::flag(&["--dry-run"], "Show what would be ingested without LLM calls."),
            DECOMPOSE_OPT,
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " EPISODES_FILE", "Ingest parsed episodes into the journal.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "EPISODES_FILE", "EPISODES_FILE");
        }
        let path = p.args[0].clone();
        if let Some(m) = click_file("episodes_file", &path) {
            return self.usage(CMD, m);
        }
        let dry = p.flag("--dry-run");
        if self.env.contains_key("OPENDAISUGI_CONFORMANCE_RECORD") && !dry {
            return self.not_yet("daisugi journal ingest with OPENDAISUGI_CONFORMANCE_RECORD set");
        }
        let raw = match std::fs::read(&path) {
            Ok(r) => r,
            Err(e) => return Err(self.pw_err(CMD, PwErr::Io(e))),
        };
        let Ok(text) = String::from_utf8(raw) else {
            return self.refuse(CMD, "the episodes file is not UTF-8");
        };
        let v = match load_episodes(&text) {
            Ok(Ok(v)) => v,
            Ok(Err(e)) => {
                self.errf(&format!("Invalid YAML: {}\n", e.msg));
                return exit(2);
            }
            Err(why) => return self.refuse(CMD, &why),
        };
        let obj = match v {
            Value::Obj(o) => o,
            other => {
                self.errf(&format!(
                    "Invalid episodes file: opendaisugi.parsers.ParseResult() argument after ** must be a mapping, not {}\n",
                    py_type_name(&other)
                ));
                return exit(2);
            }
        };
        let pr = match transcript::validate(&obj) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => return self.refuse(CMD, "the episodes file did not validate to a mapping"),
            Err(e) => {
                if e.unreadable_step() {
                    return self.refuse(CMD, "the episodes file holds a step this binary does not read");
                }
                self.errf(&format!("Invalid episodes file: {}\n", e.text()));
                return exit(2);
            }
        };
        let data_dir = self.data_dir_of(&p);
        let mut decompose = p.flag("--allow-shell-decomposition");
        if !p.flag_set("--allow-shell-decomposition") {
            match config::load(&join(&data_dir, "config.yaml")) {
                Ok(c) => decompose = c.shell_allow_decomposition,
                Err(e) => return Err(self.config_load_err(CMD, &config::RowsErr::Config(e))),
            }
        }
        let source_file = pr.value("source_file").as_str().unwrap_or("").to_string();
        let sum = crate::gate::sha256::hexdigest(source_file.as_bytes());
        let episodes = match pr.value("episodes") {
            Value::List(l) => l.clone(),
            _ => vec![],
        };
        // Everything is worked out before the first write, so input this
        // binary cannot journal the way Python does changes nothing. A
        // trace already ingested is found by its YAML body, as load_trace
        // finds it.
        let traces_dir = format!("{data_dir}/journal/traces");
        let mut parser = ShellParser::new();
        let mut items: Vec<Item> = vec![];
        for x in &episodes {
            let Value::Obj(ep) = x else { continue };
            let steps = match ep.value("steps") {
                Value::List(l) => l.clone(),
                _ => vec![],
            };
            let mut it = Item {
                id: ep.value("id").as_str().unwrap_or("").to_string(),
                task: ep.value("task").as_str().unwrap_or("").to_string(),
                steps: steps.len(),
                ..Default::default()
            };
            it.trace_id = format!("import-{}-{}", &sum[..8], it.id);
            match tracejournal::load_trace(&traces_dir, &it.trace_id) {
                Ok(Ok(_)) => {
                    it.status = "SKIP".into();
                    items.push(it);
                    continue;
                }
                Ok(Err(le)) if le.typ == "FileNotFoundError" => {}
                // load_trace raises: the oracle stops at this episode.
                Ok(Err(le)) => {
                    it.load_err = Some((le.typ, le.msg));
                    items.push(it);
                    break;
                }
                Err(e) => return Err(self.pw_err(CMD, e)),
            }
            self.ingest_one(CMD, &mut it, &steps, decompose, dry, &mut parser)?;
            items.push(it);
        }
        let j = self.open_journal(CMD, &data_dir)?;
        for it in items.iter_mut() {
            if let Some((typ, msg)) = &it.load_err {
                // load_trace raised: the oracle's asyncio.run ends with it.
                let (typ, msg) = (typ.clone(), msg.clone());
                return Err(self.load_err(CMD, &typ, &msg));
            }
            if dry || (it.status != "OK" && it.status != "FAIL") {
                continue;
            }
            if !trace_id_ok(&it.trace_id) {
                it.status = "ERROR".into();
                it.violations = 0;
                it.err = Some(format!(
                    "Invalid trace_id {}: must contain only alphanumeric characters, hyphens, underscores, and dots",
                    repr(&it.trace_id)
                ));
                continue;
            }
            if let Err(e) = j.log(&it.task, &it.env, &it.plan, &it.result, &it.trace_id, &now_iso()) {
                if let PwErr::Unreadable(why) = &e {
                    return self.refuse(CMD, why);
                }
                it.status = "ERROR".into();
                it.violations = 0;
                it.err = Some(match e {
                    PwErr::Invalid(t) => t.split_once(": ").map(|(_, m)| m.to_string()).unwrap_or(t),
                    other => other.to_string(),
                });
            }
        }
        self.ingest_report(&items, &path, dry, p.flag("--json"))
    }

    /// `_process_episode` up to the journal write: the envelope inferred
    /// from the episode's steps, the plan verified against it.
    pub(super) fn ingest_one(
        &mut self,
        cmd: &str,
        it: &mut Item,
        steps: &[Value],
        decompose: bool,
        dry: bool,
        parser: &mut ShellParser,
    ) -> Res {
        let mut records = vec![];
        for x in steps {
            let Value::Obj(s) = x else { continue };
            let rec = match s.value("type").as_str() {
                Some("shell") => Object::new().with("step_type", "shell").with("command", s.value("command").clone()),
                Some("file_read") => Object::new().with("step_type", "file_read").with("path", s.value("path").clone()),
                Some("file_write") => Object::new().with("step_type", "file_write").with("path", s.value("path").clone()),
                Some("network") => Object::new().with("step_type", "network").with("url", s.value("url").clone()),
                Some("mcp") => Object::new()
                    .with("step_type", "mcp")
                    .with("mcp_server", s.value("server").clone())
                    .with("mcp_tool", s.value("tool").clone()),
                _ => continue,
            };
            records.push(rec);
        }
        let env = infer_envelope(parser, &records, &it.task, decompose).map_err(|e| self.pw_err(cmd, e))?;
        let plan_id = match crate::gate::random_hex(4) {
            Ok(h) => h,
            Err(_) => return self.fail(cmd, "OSError: no random bytes"),
        };
        let plan = Object::new()
            .with("id", format!("plan_{plan_id}"))
            .with("source", "claude-code-import")
            .with("task", it.task.as_str())
            .with("steps", Value::List(steps.to_vec()));
        let v = match journal_result(&env, &plan, &it.task, &format!("episode {}", it.id)) {
            Ok(v) => v,
            Err(PwErr::Unreadable(why)) => return self.refuse(cmd, &format!("the plan of {}: {why}", it.id)),
            Err(PwErr::Invalid(why)) => {
                return self.refuse(cmd, &format!("episode {}: the verifier raised ({why})", it.id))
            }
            Err(e) => return Err(self.pw_err(cmd, e)),
        };
        it.violations = v.violations;
        it.status = if v.ok { "OK" } else { "FAIL" }.into();
        if dry {
            return Ok(());
        }
        it.result = v.result.map_err(|e| self.pw_err(cmd, e))?;
        it.env = env;
        it.plan = plan;
        Ok(())
    }

    fn ingest_report(&mut self, items: &[Item], path: &str, dry: bool, as_json: bool) -> Res {
        let count = |s: &str| items.iter().filter(|it| it.status == s).count();
        let (passed, failed, skipped, errored) = (count("OK"), count("FAIL"), count("SKIP"), count("ERROR"));
        let int = |n: usize| Value::Int(n.to_string());
        if as_json {
            let eps: Vec<Value> = items
                .iter()
                .map(|it| {
                    Value::Obj(
                        Object::new()
                            .with("episode_id", it.id.as_str())
                            .with("task", it.task.as_str())
                            .with("status", it.status.as_str())
                            .with("steps", int(it.steps))
                            .with("violations", int(it.violations))
                            .with("error", it.err.clone().map(Value::Str).unwrap_or(Value::Null)),
                    )
                })
                .collect();
            let o = Object::new()
                .with("total", int(items.len()))
                .with("passed", int(passed))
                .with("failed", int(failed))
                .with("skipped", int(skipped))
                .with("errored", int(errored))
                .with("episodes", Value::List(eps));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            let mut b = String::new();
            for it in items {
                let mut detail = format!("{} steps", it.steps);
                if it.violations > 0 {
                    detail.push_str(&format!(", {} violations", it.violations));
                }
                if let Some(e) = &it.err {
                    detail = e.clone();
                }
                b.push_str(&format!("{}  {:<7} \"{}\" ({detail})\n", it.id, it.status, it.task));
            }
            b.push('\n');
            let (verb, note, pass, fail) = if dry {
                ("Previewed", " (dry run — nothing written)", "would pass", "would fail")
            } else {
                ("Ingested", "", "passed", "failed")
            };
            let full = path_str(path);
            let name = full.rsplit('/').next().unwrap_or(&full);
            b.push_str(&format!("{verb} {} episodes from {name}{note}\n", items.len()));
            b.push_str(&format!("  {passed} {pass} verification\n"));
            b.push_str(&format!("  {failed} {fail} verification\n"));
            b.push_str(&format!("  {skipped} skipped (already in journal)\n"));
            if errored > 0 {
                b.push_str(&format!("  {errored} errored\n"));
            }
            self.out(&b);
        }
        if errored > 0 {
            return exit(1);
        }
        Ok(())
    }
}

/// What `verify(plan, envelope)` found, as a trace stores it.
pub(super) struct Verified {
    pub(super) ok: bool,
    pub(super) violations: usize,
    /// The result a trace stores, or why this binary cannot store it: a
    /// violation detail or a warning it does not word, or a trace body of
    /// the task it cannot write.
    pub(super) result: Result<Object, PwErr>,
}

/// `verify(plan, envelope)` as a trace stores its result. `what` names the
/// input in a refusal. An error is the verifier's own.
pub(super) fn journal_result(env: &Object, plan: &Object, task: &str, what: &str) -> Result<Verified, PwErr> {
    let t0 = Instant::now();
    let out = crate::pathways::verify::verify(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, 500)?;
    let ms = t0.elapsed().as_secs_f64() * 1000.0;
    let ok = out.violations.is_empty();
    let n = out.violations.len();
    let result = if let Some(v) = out.violations.iter().find(|v| v.detail.is_none()) {
        Err(PwErr::Unreadable(format!(
            "{what} would be journaled with a violation ({}), whose detail this binary does not write yet",
            v.message
        )))
    } else if out.warnings_unmodeled {
        Err(PwErr::Unreadable(format!("{what} may carry a verifier warning this binary does not word yet")))
    } else {
        let r = result_dump(ok, &out.violations, &out.warnings, env.value("id"), plan.value("id"), ms);
        // The body must be one the binary can write.
        trace_body(task, env, plan, &r, "2000-01-01-00000000", "2000-01-01T00:00:00Z").map(|_| r)
    };
    Ok(Verified { ok, violations: n, result })
}

/// `tempfile.mkdtemp(prefix="opendaisugi-claude-")` in `tmpdir`, or in
/// /tmp when it is empty.
fn mkdtemp(tmpdir: &str) -> std::io::Result<String> {
    use std::os::unix::fs::DirBuilderExt;
    let base = if tmpdir.is_empty() { "/tmp" } else { tmpdir };
    for _ in 0..100 {
        let hex = crate::gate::random_hex(4).map_err(|_| std::io::Error::other("no random bytes"))?;
        let d = format!("{base}/opendaisugi-claude-{hex}");
        match std::fs::DirBuilder::new().mode(0o700).create(&d) {
            Ok(()) => return Ok(d),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::from(std::io::ErrorKind::AlreadyExists))
}

/// `yaml.safe_load(episodes_file.read_text())`: the value, or the
/// yaml.YAMLError the oracle prints (`Ok(Err(exc))`).
fn load_episodes(text: &str) -> Result<Result<Value, crate::pyyaml::Exc>, String> {
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    match crate::pyyaml::load(&text) {
        Err(crate::pyyaml::Fail::Unsupported(why)) => {
            Err(format!("the episodes file holds YAML this binary does not read ({why})"))
        }
        Err(crate::pyyaml::Fail::Exc(e)) if e.kind == "ValueError" => {
            Err("the episodes file makes yaml raise a ValueError, which the oracle does not catch".into())
        }
        Err(crate::pyyaml::Fail::Exc(e)) => Ok(Err(e)),
        Ok(v) => match crate::pyyaml::to_json(&v) {
            Some(j) => Ok(Ok(j)),
            None => Err("the episodes file holds a date or a key that is not text".into()),
        },
    }
}
