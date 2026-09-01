//! `daisugi router status|stop`, `gateway-report`, and the Switchyard child
//! a gateway starts.

use std::cell::RefCell;
use std::rc::Rc;

use num_bigint::BigInt;

use super::config::{self, Config, ConfigErr};
use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::distill::load_embedder;
use crate::distill::repeats::{rank_reuse, Turn};
use crate::gate::py::text::{self, repr};
use crate::gate::pyjson::{self, py_repr, Object, Value};
use crate::gateway::meter::{Price, Prices};
use crate::gateway::pipeline::External;
use crate::gateway::report::{
    build_report, commas, external_records, format_f, format_share_table, load_journal, pad_repr, percent, share_table,
    Candidate, JRecord, JournalErr,
};
use crate::pathways::find::select_matcher;
use crate::switchyard::{self as sy, Auth, Handle, RouteErr};

/// The child a gateway started, and its state file.
pub struct SwitchyardChild {
    pub h: Handle,
    pub state: String,
}

impl SwitchyardChild {
    pub fn stop(&self) -> String {
        self.h.stop_own(&self.state)
    }
}

pub fn sy_config(cfg: &Config) -> sy::Config {
    sy::Config {
        route_id: cfg.switchyard_route_id.clone(),
        capable_model: cfg.switchyard_capable_model.clone(),
        efficient_model: cfg.switchyard_efficient_model.clone(),
        api_key_env: cfg.switchyard_api_key_env.clone(),
        llm_base_url: cfg.llm_base_url.clone(),
        llm_host_kind: cfg.llm_host_kind.clone(),
    }
}

const ROUTER_HELP: &str = "Usage: daisugi router [OPTIONS] COMMAND [ARGS]...

  The gateway's model chooser: NVIDIA NeMo Switchyard as a managed child,
  or the built-in rules router.

Commands:
  status  Show the router choice, the Switchyard binary, each running child, and recent turns.
  stop    Stop every switchyard-server that a gateway started and left running.
";

/// `str()` of a decoded JSON value.
fn py_str_of(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        other => py_repr(other).unwrap_or_default(),
    }
}

impl Env {
    /// `load_config(path)` as a command that has not caught its errors
    /// meets it: an invalid file ends the run with pydantic's error.
    pub(super) fn load_cfg(&mut self, cmd: &str, path: &str) -> Result<Config, Stop> {
        match config::load(path) {
            Ok(c) => Ok(c),
            Err(ConfigErr::Invalid) => {
                self.errf(&format!("daisugi {cmd}: pydantic_core._pydantic_core.ValidationError: {path} does not validate\n"));
                Err(Stop::Exit(1))
            }
            Err(e) => Err(self.refuse(cmd, &format!("{path} is not one this binary reads: {e}")).unwrap_err()),
        }
    }

    /// `_fail`: what, why and fix on stderr, then the exit code.
    pub(super) fn fail3(&mut self, what: &str, why: &str, fix: &str, code: i32) -> Stop {
        self.errf(&format!("{what}\n{why}\n{fix}\n"));
        Stop::Exit(code)
    }

    /// `_start_switchyard_for_gateway`: a healthy child and the target pair
    /// to meter by, or the run ends.
    pub(super) fn start_switchyard_for_gateway(
        &mut self,
        data_dir: &str,
        own: Option<&str>,
        port: i64,
    ) -> Result<(External, String, SwitchyardChild), Stop> {
        let path = self.env.get("PATH").cloned().unwrap_or_default();
        let binary = sy::look_path(sy::BINARY_NAME, &path);
        if let Some(problem) = sy::prerequisite_problem(binary.as_deref()) {
            self.errf(&format!("{problem}\n"));
            return Err(Stop::Exit(3));
        }
        let cfg = self.load_cfg("gateway", &format!("{data_dir}/config.yaml"))?;
        let mut prices = Prices::new();
        let files = sy::child_files(data_dir, port);
        let (dir, name) = files.config.rsplit_once('/').map(|(a, b)| (a.to_string(), b.to_string())).unwrap_or_default();
        let config_path;
        let mut auth: Option<Auth> = None;
        if let Some(own) = own {
            let own = path_str(own);
            let text = match std::fs::read(&own) {
                Ok(raw) => match crate::gate::pymodel::decode_utf8_strict(&raw) {
                    Ok(t) => t,
                    Err(msg) => {
                        return Err(self.fail3(
                            &format!("cannot read {own}: {msg}"),
                            "the gateway copies your Switchyard config before it starts the child.",
                            "check the path you gave to --switchyard-config.",
                            1,
                        ));
                    }
                },
                Err(e) => {
                    return Err(self.fail3(
                        &format!("cannot read {own}: {}", sy::py_os_error(&e, &own)),
                        "the gateway copies your Switchyard config before it starts the child.",
                        "check the path you gave to --switchyard-config.",
                        1,
                    ))
                }
            };
            config_path = sy::write_config(&dir, &text, &name).map_err(|e| self.fail("gateway", &e.to_string()).unwrap_err())?;
        } else {
            let Some(targets) = sy::targets_from_config(&sy_config(&cfg)) else {
                return Err(self.fail3(
                    "no efficient model is set for Switchyard.",
                    "the stage router needs a cheaper tier to choose.",
                    "run: daisugi install --gateway --router switchyard --efficient-model <id>",
                    1,
                ));
            };
            let env = self.env.clone();
            let text = match sy::render(&targets, &cfg.switchyard_route_id, &|n: &str| env.get(n).is_some_and(|v| !v.is_empty()))
            {
                Ok(t) => t,
                Err(why) => {
                    return Err(self.fail3(
                        &format!("cannot write the Switchyard config: {why}"),
                        "the values in config.yaml do not make a valid route.",
                        "fix switchyard_* in config.yaml, or run daisugi install again.",
                        1,
                    ))
                }
            };
            config_path = sy::write_config(&dir, &text, &name).map_err(|e| self.fail("gateway", &e.to_string()).unwrap_err())?;
            auth = Some(sy::auth_modes(&targets));
            if targets.efficient_local {
                prices.insert(targets.efficient_id.clone(), Price::default());
            }
        }
        let (capable, efficient) = match sy::route_targets(&config_path, &cfg.switchyard_route_id) {
            Ok(p) => p,
            Err(RouteErr::Meter(m)) => {
                return Err(self.fail3(
                    &format!("cannot meter this Switchyard config: {m}"),
                    "the gateway books each turn by the two tiers of the route it sends.",
                    "name a stage_router route with id switchyard_route_id, or pass a --switchyard-config that has one.",
                    1,
                ))
            }
            Err(RouteErr::NotRead(nr)) => {
                return Err(self.refuse("gateway", &format!("cannot read {config_path}: {nr}")).unwrap_err());
            }
        };
        if auth.is_none() {
            auth = sy::auth_from_toml(&config_path, &cfg.switchyard_route_id);
        }
        let state = sy::state_path(data_dir, port);
        let (h, msg) = sy::start(&sy::StartOptions {
            config_path: config_path.clone(),
            binary: binary.unwrap_or_default(),
            routing_log: files.routing_log,
            log_path: files.log,
            state_path: state.clone(),
            route_id: cfg.switchyard_route_id.clone(),
            port,
            auth: auth.clone(),
            wait: std::time::Duration::from_secs(10),
            kill_grace: std::time::Duration::from_secs(2),
        });
        self.out(&format!("  switchyard: {msg}\n"));
        let Some(h) = h else { return Err(Stop::Exit(3)) };
        self.out(&format!("  switchyard config: {config_path}\n"));
        self.out(&format!("  capable tier: {capable}   efficient tier: {efficient}\n"));
        if let Some(a) = &auth {
            self.out(&format!("  capable auth: {}\n", a.capable));
            self.out(&format!("  efficient auth: {}\n", a.efficient));
        }
        let ext = External { route_id: cfg.switchyard_route_id.clone(), capable_target: capable, efficient_target: efficient, prices };
        let base = h.base_url();
        Ok((ext, base, SwitchyardChild { h, state }))
    }

    pub(super) fn router(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            // typer's no_args_is_help: the help, then exit 2.
            self.out(ROUTER_HELP);
            return exit(2);
        }
        if args[0] == "--help" {
            self.out(ROUTER_HELP);
            return Ok(());
        }
        match args[0].as_str() {
            "status" => self.router_status(&args[1..]),
            "stop" => self.router_stop(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi router [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi router --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    fn load_journal_for(&mut self, cmd: &str, data_dir: &str) -> Result<Vec<JRecord>, Stop> {
        match load_journal(&format!("{data_dir}/gateway/turns.jsonl")) {
            Ok(r) => Ok(r),
            Err(e) => Err(self.journal_err(cmd, e)),
        }
    }

    fn journal_err(&mut self, cmd: &str, e: JournalErr) -> Stop {
        match e {
            JournalErr::Unread(why) => self.refuse(cmd, &why).unwrap_err(),
            JournalErr::Io(e) => self.fail(cmd, &e.to_string()).unwrap_err(),
        }
    }

    fn router_status(&mut self, args: &[String]) -> Res {
        const CMD: &str = "router status";
        let opts = [Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."), Opt::flag(&["--json"], "Machine-readable JSON output.")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Show the router choice, the Switchyard binary, each running child, and recent turns.", &opts);
        }
        let data_dir = path_str(&p.str("--data-dir", &format!("{}/.opendaisugi", self.home)));
        let cfg = self.load_cfg(CMD, &format!("{data_dir}/config.yaml"))?;
        let path = self.env.get("PATH").cloned().unwrap_or_default();
        let binary = sy::look_path(sy::BINARY_NAME, &path);
        let version = sy::version(binary.as_deref());
        struct Child {
            obj: Object,
            running: bool,
            auth: Option<Object>,
            host: String,
            path: String,
        }
        let mut children = vec![];
        let mut unreadable = vec![];
        for sf in sy::list_states(&data_dir) {
            let Some(st) = sf.state else {
                unreadable.push(sf.path);
                continue;
            };
            let running = sy::child_is_running(st.pid);
            let healthy = running && sy::probe_health(&st.host, st.port);
            let auth = match st.obj.value("auth") {
                Value::Obj(a) => Some(a.clone()),
                _ => None,
            };
            let mut route_id = st.obj.value("route_id").clone();
            if !route_id.truthy() {
                route_id = Value::Str(cfg.switchyard_route_id.clone());
            }
            let mut o = Object::new();
            o.set("pid", st.obj.value("pid").clone());
            o.set("host", st.host.as_str());
            o.set("port", st.obj.value("port").clone());
            o.set("running", running);
            o.set("healthy", healthy);
            o.set("config_path", st.obj.value("config_path").clone());
            o.set("route_id", route_id);
            o.set("auth", auth.clone().map(Value::Obj).unwrap_or(Value::Null));
            o.set("state_file", sf.path.as_str());
            children.push(Child { obj: o, running, auth, host: st.host.clone(), path: sf.path });
        }
        let next = sy::targets_from_config(&sy_config(&cfg)).map(|t| sy::auth_modes(&t));
        let records = self.load_journal_for(CMD, &data_dir)?;
        let mut recent = match external_records(&records) {
            Ok(r) => r,
            Err(e) => return Err(self.journal_err(CMD, e)),
        };
        if recent.len() > 10 {
            recent = recent.split_off(recent.len() - 10);
        }
        let share = match share_table(&records) {
            Ok(s) => s,
            Err(e) => return Err(self.journal_err(CMD, e)),
        };
        let weeks = super::routermeasure::weekly(&data_dir);
        let (dstate, dlines) = match super::routermeasure::delegate_state(&data_dir, &self.env) {
            Ok(x) => x,
            Err(why) => return self.refuse(CMD, &why),
        };
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("router", cfg.gateway_router.as_str());
            o.set("binary", binary.clone().map(Value::Str).unwrap_or(Value::Null));
            o.set("version", version.clone().map(Value::Str).unwrap_or(Value::Null));
            o.set("children", Value::List(children.iter().map(|c| Value::Obj(c.obj.clone())).collect()));
            o.set("unreadable_state_files", Value::List(unreadable.iter().map(|u| Value::Str(u.clone())).collect()));
            o.set(
                "next_start_auth",
                match &next {
                    Some(a) => {
                        let mut x = Object::new();
                        x.set("capable", a.capable.as_str());
                        x.set("efficient", a.efficient.as_str());
                        Value::Obj(x)
                    }
                    None => Value::Null,
                },
            );
            let rows: Vec<Value> = share
                .iter()
                .map(|r| {
                    let mut x = Object::new();
                    x.set("model", r.model.as_str());
                    x.set("turns", r.turns as i64);
                    x.set("share", r.share);
                    x.set("input_tokens", Value::Int(r.input.to_string()));
                    x.set("output_tokens", Value::Int(r.output.to_string()));
                    x.set("frontier_tokens_saved", Value::Int(r.saved.to_string()));
                    Value::Obj(x)
                })
                .collect();
            o.set("targets", Value::List(rows));
            let rt: Vec<Value> = recent
                .iter()
                .map(|r| {
                    let mut x = Object::new();
                    x.set("task", r.task.as_str());
                    x.set("model", r.model.clone());
                    x.set("downgraded", r.downgraded);
                    Value::Obj(x)
                })
                .collect();
            o.set("recent_turns", Value::List(rt));
            o.set("weeks", Value::List(weeks.iter().map(|w| Value::Obj(w.object())).collect()));
            o.set("escalation_built", false);
            o.set("delegate", Value::Obj(dstate));
            self.out(&format!("{}\n", pyjson::dumps(&Value::Obj(o), true)));
            return Ok(());
        }
        self.out(&format!("configured router: {}\n", cfg.gateway_router));
        match &binary {
            Some(b) => self.out(&format!("  binary:  {b}\n")),
            None => self.out(&format!("  binary:  not found. Install it with: {}\n", sy::INSTALL_CMD)),
        }
        if let Some(v) = &version {
            self.out(&format!("  version: {v}\n"));
        }
        let live = children.iter().any(|c| c.running);
        if live {
            self.out("running router: switchyard\n");
        }
        for c in children.iter().filter(|c| !c.running) {
            self.out(&format!(
                "  stale state file: {}. Pid {} is not a running switchyard-server. Clear it with: daisugi router stop\n",
                c.path,
                py_str_of(c.obj.value("pid"))
            ));
        }
        for c in children.iter().filter(|c| c.running) {
            let status = if c.obj.value("healthy") == &Value::Bool(true) { "healthy" } else { "not answering" };
            self.out(&format!(
                "  child:   pid {} on {}:{}, {status}, route {}\n",
                py_str_of(c.obj.value("pid")),
                c.host,
                py_str_of(c.obj.value("port")),
                py_str_of(c.obj.value("route_id"))
            ));
            self.out(&format!("    config: {}\n", py_str_of(c.obj.value("config_path"))));
            match &c.auth {
                Some(a) if !a.is_empty() => {
                    let Some(capable) = a.get("capable") else {
                        self.errf("Traceback (most recent call last): ...\nKeyError: 'capable'\n");
                        return exit(1);
                    };
                    self.out(&format!("    capable auth:   {}\n", py_str_of(capable)));
                    let Some(efficient) = a.get("efficient") else {
                        self.errf("Traceback (most recent call last): ...\nKeyError: 'efficient'\n");
                        return exit(1);
                    };
                    self.out(&format!("    efficient auth: {}\n", py_str_of(efficient)));
                }
                _ => self.out("    auth: not recorded at start\n"),
            }
        }
        if !live {
            self.out("  child:   not running. Start it with: daisugi gateway --router switchyard\n");
            if let Some(n) = &next {
                self.out(&format!("  capable auth at next start:   {}\n", n.capable));
                self.out(&format!("  efficient auth at next start: {}\n", n.efficient));
            }
        }
        for u in &unreadable {
            self.out(&format!("  unreadable state file: {u}. Remove it by hand, or run daisugi router stop.\n"));
        }
        if !share.is_empty() {
            self.out("  targets:\n");
            for ln in format_share_table(&share) {
                self.out(&format!("  {ln}\n"));
            }
        }
        self.out(&format!("  recent turns, last {}:\n", recent.len()));
        for r in &recent {
            let joined = text::split(&r.task).join(" ");
            let label: String = joined.chars().take(60).collect();
            let saved = if r.downgraded { "saving" } else { "no saving" };
            self.out(&format!("    {}  {}  {saved}\n", pad_repr(&label, 62), r.model.as_str().unwrap_or_default()));
        }
        self.out("by week (UTC), newest first; escalation is not built yet:\n");
        if weeks.is_empty() {
            self.out("  no turns and no delegations recorded\n");
        }
        for w in &weeks {
            self.out(&format!("  {}\n", w.line()));
        }
        for ln in &dlines {
            self.out(&format!("{ln}\n"));
        }
        Ok(())
    }

    fn router_stop(&mut self, args: &[String]) -> Res {
        const CMD: &str = "router stop";
        let opts = [Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Stop every switchyard-server that a gateway started and left running.", &opts);
        }
        let data_dir = path_str(&p.str("--data-dir", &format!("{}/.opendaisugi", self.home)));
        let states = sy::list_states(&data_dir);
        if states.is_empty() {
            self.out("no switchyard-server state file; nothing to stop\n");
            return Ok(());
        }
        for sf in states {
            let msg = sy::stop(&sf.path);
            self.out(&format!("{msg}\n"));
        }
        Ok(())
    }

    pub(super) fn gateway_report(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gateway-report";
        let opts = [Opt::val(&["--data-dir"], "PATH", "Daisugi data directory (reads <data-dir>/gateway/turns.jsonl).")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Calibrate the gateway on a real recorded day: realized routing + potential reuse.", &opts);
        }
        let data_dir = path_str(&p.str("--data-dir", &format!("{}/.opendaisugi", self.home)));
        let records = self.load_journal_for(CMD, &data_dir)?;
        if records.is_empty() {
            self.out("no turns recorded yet \u{2014} run `daisugi gateway` to start journaling.\n");
            return Ok(());
        }
        let share = match share_table(&records) {
            Ok(s) => s,
            Err(e) => return Err(self.journal_err(CMD, e)),
        };
        if !share.is_empty() {
            self.out("Switchyard targets, measured. Each turn is booked by the target it names:\n");
            for ln in format_share_table(&share) {
                self.out(&format!("{ln}\n"));
            }
            self.out("\n");
        }
        let mut cands = vec![];
        if records.iter().any(|r| !r.signature.is_empty()) {
            let mut turns = vec![];
            for r in &records {
                let t: BigInt = &r.input + &r.cache_read + &r.cache_creation + &r.output;
                let Some(tokens) = crate::gateway::report::to_i128(&t) else {
                    return self.refuse(CMD, "a gateway turn's token count is past the range this binary adds up");
                };
                turns.push(Turn { signature: r.signature.clone(), task: r.task.clone(), tokens, dollars: r.actual });
            }
            let cfg = match config::load(&format!("{}/.opendaisugi/config.yaml", self.home)) {
                Ok(c) => c,
                Err(e) => return self.refuse(CMD, &format!("the config file is not one this binary reads: {e}")),
            };
            let notes = Rc::new(RefCell::new(String::new()));
            let pe = self.potion_env(&notes);
            let m = match select_matcher(&cfg.matcher_model, &pe) {
                Ok(Some(m)) => m,
                Ok(None) => {
                    self.errf(&format!("matcher_model={} is not a built embedder.\n", repr(&cfg.matcher_model)));
                    return exit(1);
                }
                Err(nc) => return self.refuse(CMD, &nc.0),
            };
            let emb = load_embedder(&m, &pe);
            self.drain_notes(&notes);
            let emb = match emb {
                Ok(e) => e,
                Err(why) => {
                    self.errf(&format!("{why}\n"));
                    return exit(2);
                }
            };
            for c in rank_reuse(&turns, &emb, m.threshold(), &mut |_| false) {
                cands.push(Candidate { count: c.count, tokens: BigInt::from(c.tokens), dollars: c.dollars });
            }
            self.drain_notes(&notes);
        }
        let r = build_report(&records, &cands);
        let s = &r.summary;
        let f2 = |x: f64| format_f(x, 2);
        let mut b = String::new();
        b.push_str("Routing (realized) \u{2014} measured from turns already run:\n");
        b.push_str(&format!("  turns:                 {}\n", s.turns));
        b.push_str(&format!("  downgraded turns:      {}\n", s.downgraded));
        b.push_str(&format!("  frontier tokens saved: {}\n", commas(&s.frontier_saved)));
        b.push_str(&format!("  dollars saved:         ${}\n", f2(s.dollars_saved)));
        b.push_str(&format!("  blended multiplier:    {}x\n", f2(s.blended)));
        b.push_str(&format!("  local-rung turns:      {}\n", s.local_turns));
        b.push('\n');
        b.push_str("Prompt cache (measured) \u{2014} the provider's own usage split:\n");
        b.push_str(&format!("  cache read tokens:     {}\n", commas(&s.cache_read)));
        b.push_str(&format!("  cache write tokens:    {}\n", commas(&s.cache_creation)));
        b.push_str(&format!("  cache hit rate:        {} of all input\n", percent(s.cache_hit_rate)));
        b.push('\n');
        b.push_str("Reuse opportunity (ceiling) \u{2014} NOT measured, assumes perfect fresh reuse:\n");
        b.push_str(&format!("  repeat clusters:       {}\n", r.repeat_clusters));
        b.push_str(&format!("  recoverable tokens:    {}\n", commas(&r.recoverable_tokens)));
        b.push_str(&format!("  recoverable dollars:   ${}\n", f2(r.recoverable_dollars)));
        b.push('\n');
        b.push_str("Combined (ceiling) \u{2014} realized routing plus the reuse ceiling above:\n");
        b.push_str(&format!("  frontier tokens saved: {}\n", commas(&r.combined_saved)));
        b.push_str(&format!("  blended multiplier:    {}x\n", f2(r.combined_multiplier)));
        b.push('\n');
        b.push_str(
            "note: the Reuse and Combined figures are a CEILING, assuming every repeat-after-the-first is served from a \
             perfect, fresh cache \u{2014} only Routing above is measured.\n",
        );
        self.out(&b);
        Ok(())
    }
}
