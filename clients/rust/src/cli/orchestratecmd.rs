//! `daisugi orchestrate PROMPT`: decompose, size, supervised execute,
//! synthesize, with the flags, output, exit codes and files of the Python
//! CLI. The Go client's `cli/orchestratecmd.go` is the reference.

use std::cell::RefCell;
use std::rc::Rc;

use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::envgen::{self, GenErr};
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::{any_json, dumps, dumps_indent, Object, Value};
use crate::orchestrate::{self, DecomposeErr, DEFAULT_DECOMPOSE_MODEL};
use crate::pathways::find::{select_matcher, Cache as FindCache, FindErr};
use crate::pathways::pmodel::Id;
use crate::pathways::store::Store;
use crate::pathways::PwErr;
use crate::supervise;

/// Why an orchestration stopped after the data directory was set up.
enum OrchErr {
    Gen(GenErr),
    Decompose(DecomposeErr),
}

impl Env {
    /// click.INT on an option: the value, or click's usage error.
    fn orch_int(&mut self, name: &str, v: &str) -> Result<i64, Stop> {
        match super::pathwayscmd::py_int(v) {
            Some(n) => Ok(n.clamp(i64::MIN as i128, i64::MAX as i128) as i64),
            None => {
                let _ = self.usage_args("orchestrate", "PROMPT", &format!("Invalid value for {name}: {} is not a valid integer.", repr(v)));
                Err(Stop::Exit(2))
            }
        }
    }

    /// `_echo_resolved(data_dir)`: the backend, the gate's state and the
    /// data directory, on stderr.
    pub(super) fn echo_resolved_at(&mut self, data_dir: &str) -> Result<(), String> {
        let gate = self.gate_state_at(data_dir)?;
        let backend = self.llm_client().backend();
        let shown = self.tilde(data_dir);
        self.note(&format!("backend: {backend} · gate: {gate} · data: {shown}"));
        Ok(())
    }

    /// `orchestrate_cmd`'s exception handling.
    fn orchestrate_err(&mut self, cmd: &str, e: OrchErr) -> Res {
        let unported = |s: &mut Env, why: &str| -> Res {
            // The data directory is set up by now (K2-6): say so, not
            // "Nothing was changed."
            s.errf(&format!(
                "daisugi {cmd}: {why}: is not in this binary yet. The envelope cache, pathway store and journal were made; no step ran.\n"
            ));
            exit(2)
        };
        match e {
            OrchErr::Decompose(DecomposeErr::Decomposition { msg, .. }) => Err(self.fail3(
                "Tried to decompose the prompt into a plan.",
                &msg,
                "Rephrase the prompt, or check the backend with `daisugi config`.",
                1,
            )),
            OrchErr::Decompose(DecomposeErr::NotConfigured(msg)) => {
                self.errf(&format!("{}\n", strip(&msg)));
                exit(1)
            }
            OrchErr::Decompose(DecomposeErr::Unported(why)) | OrchErr::Gen(GenErr::Unported(why)) => unported(self, &why),
            OrchErr::Gen(GenErr::Py(pe)) => match pe.class.as_str() {
                "EnvelopeGenerationError" | "ModelCallError" | "ModelLadderExhausted" => {
                    Err(self.fail3("Tried to generate the envelope.", &pe.msg, "Check the backend with `daisugi config`.", 1))
                }
                "LLMNotConfigured" | "LowStakesNotConfigured" | "TaskTooLongError" => {
                    self.errf(&format!("{}\n", strip(&pe.msg)));
                    exit(1)
                }
                _ => Err(self.fail3(
                    "Tried to orchestrate the prompt.",
                    &format!("{}: {}", pe.class, pe.msg),
                    "Run again with DAISUGI_DEBUG=1 and file the traceback as a bug.",
                    1,
                )),
            },
            OrchErr::Gen(GenErr::Inherit(ie)) => self.fail(cmd, &format!("EnvelopeInheritanceError: {ie}")),
            OrchErr::Gen(GenErr::CacheRow(m)) | OrchErr::Gen(GenErr::Other(m)) => self.fail(cmd, &m),
        }
    }

    /// Whether `PathwayStore.find(prompt)` would warn of stale embeddings,
    /// read without writing. A store that is not there does not warn.
    fn find_warning(&self, db: &str, prompt: &str, key: &str, pe: &crate::pathways::potion::Env) -> Result<bool, String> {
        if std::fs::metadata(db).is_err() {
            return Ok(false);
        }
        let s = Store::open_read_only(db).map_err(|e| e.to_string())?;
        match s.find(prompt, key, pe, None, &mut FindCache::default()) {
            Ok(r) => Ok(!r.warning.is_empty()),
            Err(FindErr::NotCarried(nc)) => Err(nc.0),
            Err(FindErr::Err(PwErr::Unreadable(why))) => Err(why),
            Err(FindErr::Err(_)) => Ok(false),
        }
    }

    pub(super) fn orchestrate_cmd(&mut self, args: &[String]) -> Res {
        self.orchestrate_with(args, crate::pathways::verify::verify)
    }

    pub(super) fn orchestrate_with(&mut self, args: &[String], check: orchestrate::PlanCheck) -> Res {
        const CMD: &str = "orchestrate";
        let opts = [
            Opt::val(&["--envelope", "-e"], "PATH", "Envelope YAML (authorization boundary). If omitted, one is generated for the prompt."),
            Opt::val(&["--budget", "-b"], "INTEGER", "Approximate token budget for the run (gates routing during execution). Omit for unbudgeted."),
            Opt::flag(&["--strict-budget"], "Stop when the budget is exhausted instead of downgrading to a cheaper model."),
            Opt::flag(&["--deterministic-synthesis"], "Assemble the final answer from step outputs deterministically instead of with an LLM."),
            Opt::val(&["--max-parallel"], "INTEGER", "Run independent steps in the same dependency level concurrently, up to this many at once (1 = sequential, the default)."),
            Opt::val(&["--model"], "TEXT", "Model used to decompose the prompt (and generate the envelope if none is given)."),
            Opt::val(&["--llm"], "TEXT", "LLM backend: api | claude-code. Default: auto-detect."),
            Opt::val(&["--stakes"], "TEXT", "Stakes for a generated envelope: low|medium|high."),
            Opt::flag(&["--cost"], "Show a cost figure for the run."),
            Opt::val(&["--data-dir"], "PATH", "Daisugi data dir (pathway store + journal)."),
            Opt::flag(&["--json"], "Emit the orchestration result as JSON."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "PROMPT", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " PROMPT", "Run PROMPT end to end: decompose → size → supervised execute → synthesize.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "PROMPT", "Missing argument 'PROMPT'.");
        }
        let prompt = p.args[0].clone();
        let budget = if p.has("--budget") { Some(self.orch_int("'--budget' / '-b'", &p.str("--budget", ""))?) } else { None };
        let parallel = if p.has("--max-parallel") { self.orch_int("'--max-parallel'", &p.str("--max-parallel", ""))? } else { 1 };
        let stakes = p.str("--stakes", "medium");
        if !["low", "medium", "high"].contains(&stakes.as_str()) {
            self.errf(&format!("Invalid --stakes {}; choose from ['high', 'low', 'medium'].\n", repr(&stakes)));
            return exit(2);
        }
        if p.has("--llm") {
            let v = p.str("--llm", "");
            self.check_llm_flag(&v)?;
            self.env.insert("OPENDAISUGI_LLM_BACKEND".into(), v);
        }
        if parallel > 1 {
            return self.not_yet("daisugi orchestrate --max-parallel above 1");
        }
        let model = p.str("--model", DEFAULT_DECOMPOSE_MODEL);
        let data_dir = path_str(&p.str("--data-dir", &join(&self.home, ".opendaisugi")));
        self.renamed_backend_at(CMD, &data_dir)?;
        let client = self.llm_client();
        if let Err(why) = client.check(&model) {
            return self.refuse(CMD, &why);
        }
        let notes = Rc::new(RefCell::new(String::new()));
        let pe = self.potion_env(&notes);
        let cfg = match super::config::load(&format!("{}/.opendaisugi/config.yaml", self.home)) {
            Ok(c) => c,
            Err(e) => return self.refuse(CMD, &format!("the config file is not one this binary reads: {e}")),
        };
        let key = cfg.matcher_model.clone();
        match select_matcher(&key, &pe) {
            Ok(Some(_)) => {}
            Ok(None) => return self.refuse(CMD, &format!("matcher_model {} is not a built embedder", repr(&key))),
            Err(nc) => return self.refuse(CMD, &nc.0),
        }
        // Read the envelope before anything is said, so a refusal changes
        // nothing and says one line.
        let env_path = p.str("--envelope", "");
        let (mut env, mut env_missing, mut env_err) = (None, false, String::new());
        if p.has("--envelope") {
            if std::fs::metadata(&env_path).is_err() {
                env_missing = true;
            } else {
                match Self::load_model_yaml(&env_path, Id::Envelope) {
                    Ok(o) => env = Some(o),
                    Err(Err(why)) => return self.refuse(CMD, &why),
                    Err(Ok(perr)) => env_err = perr,
                }
            }
        }
        if let Some(e) = &env {
            let ev = Value::Obj(e.clone());
            if let Err(err) = crate::pathways::verify::check_envelope(&ev) {
                return self.refuse(CMD, &err.to_string());
            }
            let why = crate::pathways::verify::stage2_refusal(&ev);
            if !why.is_empty() {
                return self.refuse(CMD, &why);
            }
        }
        // A stored pathway embedded under another model makes find warn (a
        // UserWarning this binary does not print): refused here, before
        // anything is written.
        match self.find_warning(&join(&data_dir, "pathways.db"), &prompt, &key, &pe) {
            Err(why) => return self.refuse(CMD, &why),
            Ok(true) => {
                return self.refuse(CMD, "the pathway store warns of stale embeddings, a warning this binary does not print yet")
            }
            Ok(false) => {}
        }
        self.drain_notes(&notes);
        if let Err(why) = self.echo_resolved_at(&data_dir) {
            return self.refuse(CMD, &why);
        }
        if env_missing {
            return Err(self.fail3(&format!("Tried to read the envelope {env_path}."), "The file does not exist.", "Check the path.", 2));
        }
        if !env_err.is_empty() {
            return Err(self.fail3(
                &format!("Tried to read the envelope {env_path}."),
                &format!("It did not parse: {env_err}"),
                "Fix the file and run again.",
                2,
            ));
        }
        // Daisugi(model=..., data_dir=...): the envelope cache, then the
        // pathway store and the journal the run reads and writes.
        let cache = match envgen::cache::Cache::open(&join(&data_dir, "envelope_cache.db")) {
            Ok(c) => c,
            Err(envgen::cache::CacheErr::Row(m) | envgen::cache::CacheErr::Sql(m)) => return self.fail(CMD, &m),
        };
        let store = Store::open(&path_str(&join(&data_dir, "pathways.db"))).map_err(|e| self.pw_err(CMD, e))?;
        let j = self.open_journal(CMD, &data_dir)?;
        let client = Rc::new(RefCell::new(client));
        let env = match env {
            Some(e) => e,
            None => {
                let o = envgen::Options {
                    task: prompt.clone(),
                    models: vec![model.clone()],
                    single: true,
                    stakes: stakes.clone(),
                    thinking: "standard".into(),
                    cache: Some(cache),
                    store: Some(&store),
                    matcher_key: key.clone(),
                    potion: Some(&pe),
                    journal: Some(&j),
                    max_retries: 3,
                    max_task_chars: 4000,
                    ..Default::default()
                };
                let g = envgen::generate(&o, &mut client.borrow_mut());
                self.drain_notes(&notes);
                match g.envelope {
                    Ok(e) => e,
                    Err(e) => return self.orchestrate_err(CMD, OrchErr::Gen(e)),
                }
            }
        };
        let ev = Value::Obj(env.clone());
        if let Err(err) = crate::pathways::verify::check_envelope(&ev) {
            return self.orchestrate_err(
                CMD,
                OrchErr::Decompose(DecomposeErr::Unported(format!("the generated envelope does not read: {err}"))),
            );
        }
        let why = crate::pathways::verify::stage2_refusal(&ev);
        if !why.is_empty() {
            // A generated envelope: the data directory is set up (K2-6).
            return self.orchestrate_err(CMD, OrchErr::Decompose(DecomposeErr::Unported(why)));
        }
        let fallback = match env.value("fallback") {
            Value::Obj(f) if f.value("strategy").as_str() == Some("tier2_recompute") => {
                Some(supervise::recompute(self.llm_client(), env.clone(), 500))
            }
            _ => None,
        };
        let json = p.flag("--json");
        let res = orchestrate::run(orchestrate::Options {
            llm: client.clone(),
            prompt: prompt.clone(),
            env,
            budget,
            strict_budget: p.flag("--strict-budget"),
            synth_llm: !p.flag("--deterministic-synthesis"),
            store: Some(&store),
            matcher_key: key.clone(),
            potion: Some(&pe),
            journal: Some(&j),
            decompose_model: model.clone(),
            z3_timeout_ms: 500,
            step_timeout_s: 180,
            ladder: orchestrate::build_ladder(""),
            fallback,
            shell_env: self.env.clone(),
            check,
        });
        self.drain_notes(&notes);
        let res = match res {
            Ok(r) => r,
            Err(DecomposeErr::Decomposition { no_steps: true, .. }) => {
                if json {
                    let o = Object::new().with("status", "no-steps").with("prompt", prompt.as_str());
                    self.out(&format!("{}\n", dumps(&Value::Obj(o), false)));
                } else {
                    self.out("This prompt has no steps to run.\n");
                    self.out(
                        "daisugi orchestrate runs multi-step tasks under a verified envelope. \
                         For a plain question, ask your agent directly.\n",
                    );
                }
                return Ok(());
            }
            Err(e) => return self.orchestrate_err(CMD, OrchErr::Decompose(e)),
        };
        if let Some(e) = &res.log_error {
            return self.fail(CMD, e);
        }
        let status = res.session.status.clone();
        if json {
            let sizings: Vec<Value> = res.sizings.iter().map(|s| Value::Obj(s.dump())).collect();
            let steps: Vec<Value> = res
                .session
                .steps
                .iter()
                .map(|o| {
                    Value::Obj(
                        Object::new()
                            .with("step_id", o.step_id.as_str())
                            .with("status", o.status.as_str())
                            .with("rc", o.rc.map(|n| supervise::py_int(n as i64)).unwrap_or(Value::Null))
                            .with("error", o.error.clone().map(Value::Str).unwrap_or(Value::Null)),
                    )
                })
                .collect();
            let payload = Object::new()
                .with("prompt", prompt.as_str())
                .with("status", status.as_str())
                .with("final_answer", res.answer.as_str())
                .with("reused_pathway", res.reused)
                .with("used_llm_synthesis", res.used_llm)
                .with("budget", Value::Obj(res.budget.dump()))
                .with("sizings", Value::List(sizings))
                .with("steps", Value::List(steps))
                .with("plan", any_json(&Value::Obj(res.plan.clone())));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(payload), 2, true)));
        } else {
            self.echo(&format!("{}\n\n", res.answer));
            let reused = if res.reused { ", reused pathway" } else { "" };
            self.echo(&format!("— orchestration ({status}{reused}) —\n"));
            if status != supervise::SUCCEEDED {
                for o in &res.session.steps {
                    if (o.status == supervise::FAILED || o.status == supervise::ABORTED || o.status == "rejected_halted")
                        && o.error.as_ref().is_some_and(|e| !e.is_empty())
                    {
                        self.echo_err(&format!("  {}: {} — {}\n", o.step_id, o.status, o.error.clone().unwrap_or_default()));
                    }
                }
            }
            for s in &res.sizings {
                let down = if s.downgraded { "  [downgraded]" } else { "" };
                self.echo(&format!("  {}: difficulty={:.2} → {} ({}){down}\n", s.step_id, s.difficulty, s.tier, s.model));
            }
            let b = &res.budget;
            let mut spent = b.spent.to_string();
            if let Some(t) = b.total {
                spent.push_str(&format!("/{t}"));
            }
            self.echo(&format!("  budget: {spent} tokens across {} model call(s)\n", b.step_count));
            if p.flag("--cost") {
                match b.measured {
                    Some(m) => self.echo(&format!("  cost:   ${m:.4} (exact — Claude Code accounting)\n")),
                    None => self.echo(&format!("  cost:   ~${:.4} (estimated)\n", b.approx_f)),
                }
            }
        }
        if status != supervise::SUCCEEDED {
            return exit(1);
        }
        Ok(())
    }
}
