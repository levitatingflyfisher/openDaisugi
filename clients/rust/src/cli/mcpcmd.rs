//! `daisugi mcp serve`: the MCP server the oracle's `mcp_server.py`
//! exposes, as a JSON-RPC 2.0 server over stdio of this binary's own (no
//! SDK). The Go client's `cli/mcpcmd.go` and `cli/mcptools.go` are the
//! reference; rulings K3-1 to K3-8 and K3-R-1 say where it differs from
//! Python.

use std::cell::RefCell;
use std::collections::BTreeMap;
use std::io::Write;
use std::rc::Rc;

use super::gateroot::path_str;
use super::runcmd::{prepare, PlanCheck};
use super::{exit, parse_args, Env, Opt, Res};
use crate::envgen::{self, cache::Cache, GenErr};
use crate::gate::py::text::{has_surrogate, repr};
use crate::gate::pyjson::{dumps, py_float_repr, Object, Value};
use crate::mcpwire::{self, Checked, Kind};
use crate::pathways::find::{select_matcher, Embedder, Matcher};
use crate::pathways::pathway::json_mode;
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::pathways::store::{Col, Store};
use crate::supervise::{self, approval::Always, DefaultApproval, DryRun, Executor, Supervisor};
use crate::tracejournal::Journal;

/// The JSON-RPC error code of a request this binary does not answer the
/// oracle's way (K3-3).
const REFUSAL_CODE: i64 = -32000;

const MCP_HELP: &str = "Usage: daisugi mcp [OPTIONS] COMMAND [ARGS]...

  Serve openDaisugi tools over MCP stdio.

Options:
  --help  Show this message and exit.

Commands:
  serve  Serve openDaisugi tools over MCP stdio.
";

/// The matcher in effect: the config key, and the matcher when one is
/// built for it (None: a key nothing builds).
pub(super) struct Pick {
    pub key: String,
    pub matcher: Option<Matcher>,
}

impl Env {
    /// The configured matcher, or the reason it cannot be used: the config
    /// is not one this binary reads, or the matcher is one it does not
    /// carry.
    pub(super) fn matcher_pick(&self, notes: &Rc<RefCell<String>>) -> Result<Pick, String> {
        let cfg = super::config::load(&super::gateroot::join(&self.data_home(), "config.yaml"))
            .map_err(|e| format!("the config file is not one this binary reads: {e}"))?;
        let pe = self.potion_env(notes);
        match select_matcher(&cfg.matcher_model, &pe) {
            Ok(m) => Ok(Pick {
                key: cfg.matcher_model,
                matcher: m,
            }),
            Err(nc) => Err(nc.0),
        }
    }

    pub(super) fn mcp_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(MCP_HELP);
            return Ok(());
        }
        if args[0] == "serve" {
            return self.mcp_serve(&args[1..]);
        }
        self.errf(&format!(
            "Usage: daisugi mcp [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi mcp --help' for help.\n\nError: No such command '{}'.\n",
            args[0]
        ));
        exit(2)
    }

    fn mcp_serve(&mut self, args: &[String]) -> Res {
        const CMD: &str = "mcp serve";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", ""),
            Opt::val(&["--model"], "TEXT", "Model used for envelope generation."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Serve openDaisugi tools over MCP stdio.", &opts);
        }
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let model = p.str("--model", envgen::DEFAULT_MODEL);
        // Daisugi(model=..., data_dir=...) makes the envelope cache.
        if let Err(e) = Cache::open(&format!("{data_dir}/envelope_cache.db")) {
            let why = match e {
                envgen::cache::CacheErr::Row(m) | envgen::cache::CacheErr::Sql(m) => m,
            };
            return self.fail(CMD, &why);
        }
        // What the command printed so far goes first; the server then
        // writes each line itself.
        self.flush();
        let mut s = Server::new(data_dir, model);
        let stdin = std::io::stdin();
        let mut lr = mcpwire::LineReader::new(stdin.lock());
        while let Some(line) = lr.next_line() {
            s.handle(self, &line);
        }
        Ok(())
    }
}

/// The state of one `daisugi mcp serve`: the facade's data directory and
/// model, and the stores it opens on first use.
struct Server {
    data_dir: String,
    model: String,
    initialized: bool,
    store: Option<Store>,
    journal: Option<Journal>,
    /// The whole-plan verify of verify_plan and run_plan; a test swaps it.
    check: PlanCheck,
    /// Where the lines go: stdout, or a test's list.
    sink: Option<Vec<String>>,
    /// The stale-embeddings warning has been given (Python gives it once
    /// per process).
    stale_warned: bool,
}

/// How a tool call ended.
enum ToolErr {
    /// An exception the tool raised: FastMCP writes its text.
    Tool(String),
    /// Input this binary does not answer the oracle's way.
    Refuse(String),
}

type TR<T> = Result<T, ToolErr>;

fn refuse<T>(why: impl Into<String>) -> TR<T> {
    Err(ToolErr::Refuse(why.into()))
}

/// The first line of an error, for a refusal's text.
fn short(s: &str) -> String {
    s.split('\n').next().unwrap_or("").to_string()
}

/// The tools whose return is a list or None: FastMCP wraps it as
/// {"result": ...}.
const LIST_TOOLS: &[&str] = &[
    "find_pathway",
    "list_pathways",
    "receipts_for_run",
    "recent_runs",
];

fn str_or_null(s: Option<&str>) -> Value {
    s.map(|x| Value::Str(x.into())).unwrap_or(Value::Null)
}

fn int_arg(a: &Object, k: &str) -> Option<i64> {
    match a.value(k) {
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

/// A tool's model run on a value: pydantic's error text is what the tool
/// raises.
fn validate(id: Id, v: &Value) -> TR<Object> {
    match validate_model(id, v, Mode::Python) {
        Ok(Value::Obj(o)) => Ok(o),
        Ok(_) => refuse("a model that did not validate to a mapping"),
        Err(e) => match e.unreadable() {
            Some(why) => refuse(&why),
            None => Err(ToolErr::Tool(e.text())),
        },
    }
}

impl Server {
    fn new(data_dir: String, model: String) -> Server {
        Server {
            data_dir,
            model,
            initialized: false,
            store: None,
            journal: None,
            check: crate::pathways::verify::verify,
            sink: None,
            stale_warned: false,
        }
    }

    /// Writes one line and flushes it, so the client reads each reply as
    /// it is made.
    fn write(&mut self, line: &str) {
        if let Some(lines) = &mut self.sink {
            lines.push(line.to_string());
            return;
        }
        let out = std::io::stdout();
        let mut lock = out.lock();
        let _ = lock.write_all(line.as_bytes());
        let _ = lock.write_all(b"\n");
        let _ = lock.flush();
    }

    /// Answers a request this binary does not answer the oracle's way.
    fn refuse(&mut self, id: &Value, what: &str) {
        self.write(&mcpwire::error_reply(
            id,
            REFUSAL_CODE,
            &format!("{what} is not in this binary yet."),
            None,
        ));
    }

    fn handle(&mut self, e: &mut Env, line: &str) {
        let m = mcpwire::classify(line);
        match m.kind {
            Kind::Invalid => {
                self.write(mcpwire::ERROR_NOTIFICATION);
                return;
            }
            Kind::Notification => {
                if m.method == "notifications/initialized" {
                    self.initialized = true;
                }
                return;
            }
            Kind::Request => {}
        }
        match mcpwire::check(&m) {
            Checked::Invalid => {
                self.write(&mcpwire::invalid_params(&m.id));
                return;
            }
            Checked::Unported => {
                self.refuse(
                    &m.id,
                    &format!("daisugi mcp: the request {} in this form", m.method),
                );
                return;
            }
            Checked::Ok => {}
        }
        if m.method != "initialize" && m.method != "ping" && !self.initialized {
            self.write(&mcpwire::invalid_params(&m.id));
            return;
        }
        let params = m.params.clone().unwrap_or_default();
        match m.method.as_str() {
            "initialize" => {
                let proto = mcpwire::protocol(params.value("protocolVersion"));
                self.write(&mcpwire::reply(&m.id, &mcpwire::initialize_result(proto)));
                self.initialized = true;
            }
            "ping" => self.write(&mcpwire::reply(&m.id, "{}")),
            "tools/list" => self.write(&mcpwire::reply(&m.id, mcpwire::TOOLS_LIST)),
            "resources/list" => self.write(&mcpwire::reply(&m.id, "{\"resources\":[]}")),
            "resources/templates/list" => {
                self.write(&mcpwire::reply(&m.id, "{\"resourceTemplates\":[]}"))
            }
            "prompts/list" => self.write(&mcpwire::reply(&m.id, "{\"prompts\":[]}")),
            "prompts/get" => {
                let name = params.value("name").as_str().unwrap_or("").to_string();
                self.write(&mcpwire::error_reply(
                    &m.id,
                    0,
                    &format!("Unknown prompt: {name}"),
                    None,
                ));
            }
            // The oracle's server registers no handler for these.
            "logging/setLevel" | "resources/subscribe" | "resources/unsubscribe" | "completion/complete"
            | "tasks/get" | "tasks/result" | "tasks/list" | "tasks/cancel" => {
                self.write(&mcpwire::error_reply(&m.id, -32601, "Method not found", None))
            }
            "resources/read" => {
                let uri = mcpwire::norm_uri(params.value("uri").as_str().unwrap_or("")).unwrap_or_default();
                self.write(&mcpwire::error_reply(&m.id, 0, &format!("Unknown resource: {uri}"), None))
            }
            "tools/call" => {
                let name = params.value("name").as_str().unwrap_or("").to_string();
                let args = params
                    .value("arguments")
                    .as_obj()
                    .cloned()
                    .unwrap_or_default();
                // A panic is taken as a refusal: the server answers the
                // request and goes on, as the oracle's does.
                let r = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    self.call(e, &name, &args)
                }));
                match r {
                    Ok(Ok(result)) => self.write(&mcpwire::reply(&m.id, &result)),
                    Ok(Err(why)) => self.refuse(&m.id, &format!("daisugi mcp: {name} with {why}")),
                    Err(_) => self.refuse(
                        &m.id,
                        &format!("daisugi mcp: {name} with input this binary does not handle"),
                    ),
                }
            }
            _ => self.refuse(&m.id, &format!("daisugi mcp: the request {}", m.method)),
        }
    }

    /// FastMCP's call_tool for one tool: the result's JSON, or a refusal's
    /// reason.
    fn call(&mut self, e: &mut Env, name: &str, args: &Object) -> Result<String, String> {
        let Some(model) = mcpwire::args(name) else {
            return Ok(mcpwire::tool_result(
                &[format!("Unknown tool: {name}")],
                None,
                true,
            ));
        };
        let Some(pre) = mcpwire::pre_parse(model, args) else {
            return Err("an argument nested deeper than this binary reads".into());
        };
        if has_surrogate(&dumps(&Value::Obj(pre.clone()), false)) {
            return Err("an argument that holds a lone surrogate".into());
        }
        let a = match mcpwire::validate(model, &Value::Obj(pre)) {
            Ok(Value::Obj(a)) => a,
            Ok(_) => return Err("arguments that did not validate to a mapping".into()),
            Err(verr) => {
                return Ok(mcpwire::tool_result(
                    &[format!("Error executing tool {name}: {}", verr.text())],
                    None,
                    true,
                ))
            }
        };
        let notes = Rc::new(RefCell::new(String::new()));
        let out = match name {
            "envelope_for" => self.envelope_for(e, &a, &notes),
            "find_pathway" => self.find_pathway(e, &a, &notes),
            "recall" => self.recall(e, &a, &notes),
            "recall_answer" => self.recall_answer(e, &a, &notes),
            "verify_plan" => self.verify_plan(&a),
            "verify_completed_step" => self.verify_completed_step(&a),
            "list_pathways" => self.list_pathways(),
            "pathway_stats" => self.pathway_stats(),
            "run_plan" => self.run_plan(e, &a),
            "receipts_for_run" => self.receipts_for_run(&a),
            "recent_runs" => self.recent_runs(&a),
            "delegate" => self.delegate(e, &a),
            _ => {
                return Ok(mcpwire::tool_result(
                    &[format!("Unknown tool: {name}")],
                    None,
                    true,
                ))
            }
        };
        // A download notice is the log's, on stderr.
        let text = std::mem::take(&mut *notes.borrow_mut());
        if !text.is_empty() {
            eprint!("{text}");
        }
        let out = match out {
            Ok(v) => v,
            Err(ToolErr::Tool(t)) => {
                return Ok(mcpwire::tool_result(
                    &[format!("Error executing tool {name}: {t}")],
                    None,
                    true,
                ))
            }
            Err(ToolErr::Refuse(why)) => return Err(why),
        };
        // _convert_to_content: a list is one text block per item, None
        // none.
        let texts: Vec<String> = match &out {
            Value::Null => vec![],
            Value::List(l) => l.iter().map(mcpwire::indent).collect(),
            other => vec![mcpwire::indent(other)],
        };
        let mut structured = json_mode(&out);
        if LIST_TOOLS.contains(&name) {
            structured = Value::Obj(Object::new().with("result", structured));
        }
        Ok(mcpwire::tool_result(&texts, Some(&structured), false))
    }

    /// The facade's lazy `PathwayStore(data_dir/pathways.db)`.
    fn store(&mut self) -> TR<&Store> {
        if self.store.is_none() {
            match Store::open(&format!("{}/pathways.db", self.data_dir)) {
                Ok(s) => self.store = Some(s),
                Err(err) => {
                    return refuse(format!("a pathway store this binary does not open ({err})"))
                }
            }
        }
        Ok(self.store.as_ref().expect("opened above"))
    }

    /// The facade's lazy `Journal(data_dir)`.
    fn journal(&mut self) -> TR<&Journal> {
        if self.journal.is_none() {
            match Journal::open(&self.data_dir) {
                Ok(j) => self.journal = Some(j),
                Err(err) => return refuse(format!("a journal this binary does not open ({err})")),
            }
        }
        Ok(self.journal.as_ref().expect("opened above"))
    }

    /// The matcher in effect, or a refusal when it is not one this binary
    /// builds.
    fn built_matcher(e: &Env, notes: &Rc<RefCell<String>>) -> TR<(String, Matcher)> {
        let pick = match e.matcher_pick(notes) {
            Ok(p) => p,
            Err(why) => return refuse(why),
        };
        match pick.matcher {
            Some(m) => Ok((pick.key, m)),
            None => refuse(format!(
                "matcher_model {}, which is not a built embedder here",
                repr(&pick.key)
            )),
        }
    }

    fn pathway_stats(&mut self) -> TR<Value> {
        let (n, hits) = match self.store()?.stats() {
            Ok(x) => x,
            Err(err) => {
                return refuse(format!("a pathway store this binary does not read ({err})"))
            }
        };
        let hits =
            match hits {
                Col::Int(i) => Value::Int(i.to_string()),
                Col::Real(f) => Value::Float(f),
                _ => return refuse(
                    "a pathway store this binary does not read (a hit count that is not a number)",
                ),
            };
        Ok(Value::Obj(
            Object::new()
                .with("count", Value::Int(n.to_string()))
                .with("total_hits", hits),
        ))
    }

    fn list_pathways(&mut self) -> TR<Value> {
        let all = match self.store()?.read_all(&|_| false) {
            Ok(a) => a,
            Err(err) => {
                return refuse(format!("a pathway store this binary does not read ({err})"))
            }
        };
        let out = all
            .iter()
            .map(|p| {
                Value::Obj(
                    Object::new()
                        .with("id", p.id())
                        .with("task_description", p.task())
                        .with("hit_count", p.obj.value("hit_count").clone())
                        .with("distilled_at", p.obj.value("distilled_at").clone()),
                )
            })
            .collect();
        Ok(Value::List(out))
    }

    /// The delegate tool: the router's worker reads the
    /// file and answers; each quote is checked against the file.
    fn delegate(&mut self, e: &mut Env, a: &Object) -> TR<Value> {
        let s = |k: &str| match a.value(k) {
            Value::Str(v) => v.clone(),
            _ => String::new(),
        };
        let c = e.llm_client();
        let env = e.env.clone();
        let get = move |k: &str| env.get(k).cloned();
        let call = |m: &str, b: &str, msgs: &[(String, String)], o: &crate::llm::wire::Opts, t: f64| {
            c.complete_at(m, b, msgs, o, t)
        };
        match crate::delegate::run(&s("path"), &s("question"), &s("mode"), &self.data_dir, &get, &call) {
            Ok(o) => Ok(Value::Obj(o)),
            Err(crate::delegate::Unsupported(why)) => refuse(why),
        }
    }

    fn recent_runs(&mut self, a: &Object) -> TR<Value> {
        let Some(limit) = int_arg(a, "limit") else {
            return refuse("a limit outside what SQLite takes");
        };
        let rows = match self.journal()?.list_recent(limit) {
            Ok(r) => r,
            Err(err) => return refuse(format!("a journal this binary does not read ({err})")),
        };
        let bad = || refuse("a journal row this binary does not read as a Trace");
        let mut out = vec![];
        for r in rows {
            let (Col::Text(id), Col::Text(created), Col::Text(task)) = (&r[0], &r[1], &r[2]) else {
                return bad();
            };
            let (Col::Text(_), Col::Text(_), Col::Int(ok)) = (&r[3], &r[4], &r[5]) else {
                return bad();
            };
            let dur = match r[6] {
                Col::Real(f) => f,
                Col::Int(i) => i as f64,
                _ => return bad(),
            };
            if !matches!(&r[7], Col::Text(v) if v == "[]") {
                return refuse(
                    "a journal row with violations, which this binary does not read as a Trace yet",
                );
            }
            out.push(Value::Obj(
                Object::new()
                    .with("run_id", id.as_str())
                    .with("task", task.as_str())
                    .with("ok", *ok != 0)
                    .with("duration_ms", dur)
                    .with("created_at", created.as_str()),
            ));
        }
        Ok(Value::List(out))
    }

    fn receipts_for_run(&mut self, a: &Object) -> TR<Value> {
        let run_id = a.value("run_id").as_str().unwrap_or("").to_string();
        let rows = match self.journal()?.receipts(&run_id) {
            Ok(r) => r,
            Err(err) => return refuse(format!("a journal this binary does not read ({err})")),
        };
        let out = rows
            .into_iter()
            .map(|r| {
                Value::Obj(
                    Object::new()
                        .with("step_id", r.step_id)
                        .with("run_id", r.run_id)
                        .with("timestamp", r.timestamp)
                        .with("evidence_hash", r.evidence_hash)
                        .with("verify_result", r.verify_result)
                        .with("verify_details", r.verify_details)
                        .with("model_id", r.model_id),
                )
            })
            .collect();
        Ok(Value::List(out))
    }

    fn envelope_for(&mut self, e: &mut Env, a: &Object, notes: &Rc<RefCell<String>>) -> TR<Value> {
        let task = a.value("task").as_str().unwrap_or("").to_string();
        let stakes = a.value("stakes").as_str().unwrap_or("").to_string();
        if !["low", "medium", "high"].contains(&stakes.as_str()) {
            return Err(ToolErr::Tool(format!(
                "stakes must be low|medium|high, got {}",
                repr(&stakes)
            )));
        }
        let (key, _) = Self::built_matcher(e, notes)?;
        let mut c = e.llm_client();
        if stakes != "low" {
            if let Err(why) = c.check(&self.model) {
                return refuse(why);
            }
        }
        // generate_envelope's arguments: the pathway store and the journal
        // are made before it runs.
        self.store()?;
        self.journal()?;
        let cache = match Cache::open(&format!("{}/envelope_cache.db", self.data_dir)) {
            Ok(c) => c,
            Err(_) => return refuse("an envelope cache this binary does not open"),
        };
        let pe = e.potion_env(notes);
        let o = envgen::Options {
            task,
            context: a.value("context").as_str().map(String::from),
            cache: Some(cache),
            store: self.store.as_ref(),
            matcher_key: key,
            potion: Some(&pe),
            journal: self.journal.as_ref(),
            stakes,
            models: vec![self.model.clone()],
            single: true,
            thinking: "standard".into(),
            max_retries: 3,
            max_task_chars: 4000,
            ..Default::default()
        };
        let g = envgen::generate(&o, &mut c);
        self.stale_warning(e, &g.find_warning, notes);
        match g.envelope {
            Ok(env) => Ok(json_mode(&Value::Obj(env))),
            Err(GenErr::Py(pe)) => Err(ToolErr::Tool(pe.msg)),
            Err(GenErr::Inherit(_)) => refuse("an inheritance check this binary does not word"),
            Err(GenErr::CacheRow(m)) | Err(GenErr::Unported(m)) | Err(GenErr::Other(m)) => {
                refuse(short(&m))
            }
        }
    }

    /// The stale-embeddings UserWarning find prints, once per server, as
    /// Python's warnings module prints it to the server's stderr under
    /// PYTHONWARNINGS. A filter this binary does not model prints nothing:
    /// the server's stderr is its log, which no client reads (RF-11).
    fn stale_warning(&mut self, e: &Env, warning: &str, notes: &Rc<RefCell<String>>) {
        if warning.is_empty() || self.stale_warned {
            return;
        }
        self.stale_warned = true;
        let pyw = e.env.get("PYTHONWARNINGS").cloned().unwrap_or_default();
        if super::pywarn::user_warning_shown(&pyw, warning) == Some(true) {
            notes.borrow_mut().push_str(&format!("UserWarning: {warning}\n"));
        }
    }

    fn find_pathway(&mut self, e: &mut Env, a: &Object, notes: &Rc<RefCell<String>>) -> TR<Value> {
        let (key, _) = Self::built_matcher(e, notes)?;
        let pe = e.potion_env(notes);
        let task = a.value("task").as_str().unwrap_or("").to_string();
        let mut cache = Default::default();
        let r = match self.store()?.find(&task, &key, &pe, None, &mut cache) {
            Ok(r) => {
                self.stale_warning(e, &r.warning, notes);
                r
            }
            Err(crate::pathways::find::FindErr::NotCarried(nc)) => return refuse(nc.0),
            Err(crate::pathways::find::FindErr::Err(err)) => {
                return refuse(format!(
                    "a pathway store this binary does not read ({})",
                    short(&err.to_string())
                ))
            }
        };
        match r.matched {
            None => Ok(Value::Null),
            Some((p, sim)) => Ok(Value::Obj(
                Object::new()
                    .with("similarity", sim)
                    .with("pathway", json_mode(&Value::Obj(p.full().obj))),
            )),
        }
    }

    fn recall(&mut self, e: &mut Env, a: &Object, notes: &Rc<RefCell<String>>) -> TR<Value> {
        self.store()?;
        let env = validate(Id::Envelope, a.value("envelope"))?;
        let z3 = match int_arg(a, "z3_timeout_ms") {
            // Z3 reads a timeout of 0 or less as none, and the oracle
            // passes what it is given (K3-6).
            Some(n) if n > 0 && n <= i32::MAX as i64 => n as u32,
            _ => return refuse("a z3_timeout_ms that is not a positive 32-bit count"),
        };
        let (key, _) = Self::built_matcher(e, notes)?;
        let pe = e.potion_env(notes);
        let mut c = e.llm_client();
        let task = a.value("task").as_str().unwrap_or("").to_string();
        let store = self.store.as_ref().expect("opened above");
        let r = match crate::gateway::recall::recall(
            store,
            &key,
            &pe,
            &task,
            &Value::Obj(env),
            z3,
            Some(&mut c),
            &self.model,
        ) {
            Ok(r) => r,
            Err(why) => return refuse(short(&why)),
        };
        self.stale_warning(e, &r.warning, notes);
        let prov = match r.provenance {
            Some(p) => Value::Obj(
                Object::new()
                    .with("pathway_id", p.pathway_id)
                    .with("similarity", p.similarity)
                    .with("tier", p.tier)
                    .with("source_trace_count", p.source_trace_count as i64)
                    .with("distilled_at", p.distilled_at)
                    .with("hit_count", p.hit_count),
            ),
            None => Value::Null,
        };
        Ok(Value::Obj(
            Object::new()
                .with("hit", r.hit)
                .with("reason", str_or_null(r.reason))
                .with("plan", json_mode(&r.plan))
                .with("provenance", prov),
        ))
    }

    fn recall_answer(&mut self, e: &mut Env, a: &Object, notes: &Rc<RefCell<String>>) -> TR<Value> {
        let max_age = match a.value("max_age_seconds") {
            Value::Float(f) => *f,
            Value::Int(t) => t.parse::<f64>().unwrap_or(f64::NAN),
            _ => f64::NAN,
        };
        if !max_age.is_finite() {
            return Err(ToolErr::Tool(format!(
                "max_age_seconds must be a finite number, got {}",
                py_float_repr(max_age)
            )));
        }
        let entries = match crate::gateway::answers::load_answers(&format!(
            "{}/gateway/answers.jsonl",
            self.data_dir
        )) {
            Ok(x) => x,
            Err(err) => {
                return refuse(format!(
                    "an answer store this binary does not read ({})",
                    short(&err.to_string())
                ))
            }
        };
        let ground = a.value("current_ground_hash").as_str().map(String::from);
        // The embedder is loaded only when there is an answer to match.
        let mut emb = Embedder::Lexical;
        let mut threshold = 0.0;
        if entries
            .iter()
            .any(|x| x.signature.truthy() && x.answer.truthy())
        {
            let (_, m) = Self::built_matcher(e, notes)?;
            let pe = e.potion_env(notes);
            emb = match crate::distill::load_embedder(&m, &pe) {
                Ok(x) => x,
                Err(why) => return refuse(short(&why)),
            };
            threshold = m.threshold();
        }
        let now = crate::tracejournal::runs::now();
        let task = a.value("task").as_str().unwrap_or("").to_string();
        let r = match crate::gateway::recall::recall_answer(
            &task,
            &entries,
            now,
            &emb,
            threshold,
            max_age,
            ground.as_deref(),
        ) {
            Ok(r) => r,
            Err(why) => return refuse(short(&why)),
        };
        let prov = match r.provenance {
            Some(p) => Value::Obj(
                Object::new()
                    .with("similarity", p.similarity)
                    .with("age_seconds", p.age_seconds)
                    .with("created_at", p.created_at)
                    .with("ground_hash", p.ground_hash),
            ),
            None => Value::Null,
        };
        Ok(Value::Obj(
            Object::new()
                .with("hit", r.hit)
                .with("reason", str_or_null(r.reason))
                .with("answer", r.answer)
                .with("provenance", prov),
        ))
    }

    fn verify_plan(&mut self, a: &Object) -> TR<Value> {
        let plan = validate(Id::ActionPlan, a.value("plan"))?;
        let env = validate(Id::Envelope, a.value("envelope"))?;
        match prepare(&plan, &env, self.check) {
            Ok(p) => Ok(Value::Obj(p.verification)),
            Err(why) => refuse(why),
        }
    }

    fn verify_completed_step(&mut self, a: &Object) -> TR<Value> {
        let raw = a.value("envelope").as_obj().cloned().unwrap_or_default();
        if raw.get("permissions").is_none() {
            return Err(ToolErr::Tool(
                "envelope dict is missing 'permissions'; pass an explicit Permission block (all-default is not a safe fallback)"
                    .into(),
            ));
        }
        let task = raw
            .get("task")
            .cloned()
            .unwrap_or(Value::Str(String::new()));
        let plan = validate(
            Id::ActionPlan,
            &Value::Obj(
                Object::new()
                    .with("source", "stage2-mcp")
                    .with("task", task)
                    .with("steps", Value::List(vec![a.value("step").clone()])),
            ),
        )?;
        let env = validate(Id::Envelope, &Value::Obj(raw))?;
        let step = match plan.value("steps") {
            Value::List(l) => l
                .first()
                .and_then(|s| s.as_obj())
                .cloned()
                .unwrap_or_default(),
            _ => Object::new(),
        };
        match crate::pathways::verify::stage2_violations(&step, &env) {
            Ok(vs) => Ok(Value::Obj(
                Object::new().with("violations", Value::List(vs)),
            )),
            Err(why) => refuse(why),
        }
    }

    fn run_plan(&mut self, e: &mut Env, a: &Object) -> TR<Value> {
        let plan = validate(Id::ActionPlan, a.value("plan"))?;
        let env = validate(Id::Envelope, a.value("envelope"))?;
        if e.env.contains_key("OPENDAISUGI_MCP_RUN_TIMEOUT") {
            return refuse("OPENDAISUGI_MCP_RUN_TIMEOUT set");
        }
        let dry = matches!(a.value("dry_run"), Value::Bool(true));
        let pre = match prepare(&plan, &env, self.check) {
            Ok(p) => p,
            Err(why) => return refuse(why),
        };
        self.journal()?;
        let fallback = match env.value("fallback") {
            Value::Obj(f) if f.value("strategy").as_str() == Some("tier2_recompute") => {
                Some(supervise::recompute(e.llm_client(), env.clone(), 500))
            }
            _ => None,
        };
        let mut executors: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
        if dry {
            if let Value::List(steps) = plan.value("steps") {
                for s in steps {
                    if let Some(t) = s.as_obj().and_then(|o| o.value("type").as_str()) {
                        executors.insert(t.to_string(), Box::new(DryRun));
                    }
                }
            }
        }
        if executors.is_empty() {
            for (k, ex) in supervise::default_executors(e.env.clone()) {
                executors.insert(k, ex);
            }
        }
        // The real approval gate on a live run; stdin is the MCP stream,
        // never a terminal, so no prompt is read from it.
        let approval: Box<dyn supervise::approval::Approver> = if dry {
            Box::new(Always)
        } else {
            Box::new(DefaultApproval {
                env: e.env.clone(),
                terminal: false,
            })
        };
        let j = self.journal.as_ref().expect("opened above");
        let mut sup = Supervisor::new(executors, approval, Some(j));
        sup.fallback = fallback;
        let sess = sup.run(&plan, &env, pre.verification);
        if let Some(err) = sup.log_err.take() {
            return refuse(format!(
                "a journal write this binary does not make the oracle's way ({})",
                short(&err)
            ));
        }
        drop(sup);
        let rows = match j.receipts(&sess.id) {
            Ok(r) => r,
            Err(err) => {
                return refuse(format!(
                    "a journal this binary does not read ({})",
                    short(&err.to_string())
                ))
            }
        };
        let receipts = rows
            .into_iter()
            .map(|r| {
                Value::Obj(
                    Object::new()
                        .with("step_id", r.step_id)
                        .with("timestamp", r.timestamp)
                        .with("evidence_hash", r.evidence_hash)
                        .with("verify_result", r.verify_result)
                        .with("model_id", r.model_id),
                )
            })
            .collect();
        Ok(Value::Obj(
            Object::new()
                .with("run_id", sess.id.as_str())
                .with("status", sess.status.as_str())
                .with("integrity_passed", sess.integrity_passed)
                .with("failed_step_id", sess.failed_step_id.clone())
                .with("receipts", Value::List(receipts)),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pathways::verify::Outcome;
    use crate::pathways::PwErr;

    const INIT: &str = r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}"#;

    fn session(lines: &[&str], check: Option<PlanCheck>) -> Vec<String> {
        let home = crate::supervise::tests::scratch("mcp");
        let mut e = Env {
            args: vec![],
            env: [
                ("HOME".to_string(), home.clone()),
                ("PATH".to_string(), "/usr/bin:/bin".to_string()),
            ]
            .into(),
            home: home.clone(),
            out: vec![],
            err: vec![],
            tend_failed: false,
            raised: false,
            quiet: false,
            plain: false,
        };
        let mut s = Server::new(format!("{home}/.opendaisugi"), envgen::DEFAULT_MODEL.into());
        s.sink = Some(vec![]);
        if let Some(c) = check {
            s.check = c;
        }
        for l in lines {
            s.handle(&mut e, &format!("{l}\n"));
        }
        let _ = std::fs::remove_dir_all(home);
        s.sink.take().unwrap_or_default()
    }

    fn verify_unknown(_: &Value, _: &Value, _: Option<bool>, _: u32) -> Result<Outcome, PwErr> {
        Ok(Outcome {
            violations: vec![],
            timeouts: vec!["Z3 returned unknown".into()],
            warnings: vec![],
            warnings_unmodeled: false,
        })
    }

    /// A Z3 check that does not finish makes verify_plan answer not ok,
    /// with the z3 violation (fail closed), where the oracle's lenient
    /// verify keeps it as a warning (K3-5).
    #[test]
    fn verify_plan_with_a_z3_unknown_fails_closed() {
        let call = r#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"verify_plan","arguments":{"plan":{"source":"s","task":"t","steps":[{"id":"a","type":"shell","command":"ls"}]},"envelope":{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["ls"]}}}}}"#;
        let out = session(&[INIT, call], Some(verify_unknown));
        let last = out.last().unwrap();
        assert!(
            last.contains(r#""ok":false"#)
                && last.contains(r#""stage":"z3""#)
                && last.contains("Z3 returned unknown"),
            "{last}"
        );
    }

    /// A request this binary does not answer the oracle's way gets a
    /// JSON-RPC error that says so, and the server goes on (K3-3).
    #[test]
    fn an_unported_method_is_refused_and_the_server_goes_on() {
        let out = session(
            &[
                INIT,
                r#"{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"http://a/%41"}}"#,
                r#"{"jsonrpc":"2.0","id":3,"method":"ping"}"#,
            ],
            None,
        );
        assert_eq!(out.len(), 3, "{out:?}");
        assert!(
            out[1].contains(r#""code":-32000"#) && out[1].contains("is not in this binary yet."),
            "{}",
            out[1]
        );
        assert_eq!(out[2], r#"{"jsonrpc":"2.0","id":3,"result":{}}"#);
    }

    /// An argument whose JSON text holds a lone surrogate is refused, not
    /// answered another way.
    #[test]
    fn an_argument_with_a_lone_surrogate_is_refused() {
        let call = r#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"verify_plan","arguments":{"plan":"{\"a\": \"\\ud800\"}","envelope":{}}}}"#;
        let out = session(&[INIT, call], None);
        assert!(
            out[1].contains("lone surrogate") && out[1].contains("-32000"),
            "{}",
            out[1]
        );
    }
}
