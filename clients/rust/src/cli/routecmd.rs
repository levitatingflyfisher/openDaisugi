//! `daisugi route`: `RouteAdvisor.advise` for one task.

use std::cell::RefCell;
use std::rc::Rc;

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::{lower, repr, strip};
use crate::gate::pyjson::{self, Object, Value};
use crate::gateway::report::format_f;
use crate::gateway::route::{estimate_difficulty, DEFAULT_CHEAP_MODEL, DEFAULT_FRONTIER_MODEL};
use crate::pathways::find::{select_matcher, Cache, FindErr};
use crate::pathways::store::Store;
use crate::pathways::PwErr;

/// Each carried backend's reuse threshold, as `_search.active_threshold`
/// gives it.
fn threshold_of(key: &str) -> Option<f64> {
    match key {
        "potion" => Some(0.59),
        "all-MiniLM-L6-v2" => Some(0.55),
        "lexical" => Some(0.25),
        "int8" => Some(0.55),
        _ => None,
    }
}

struct Advice {
    tier: &'static str,
    model: String,
    reason: String,
    pathway: String,
    pairing: bool,
}

impl Env {
    pub(super) fn route(&mut self, args: &[String]) -> Res {
        const CMD: &str = "route";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory (pathway store)."),
            Opt::val(&["--cheap-model"], "TEXT", "Model recommended for easy tasks."),
            Opt::val(&["--frontier-model"], "TEXT", "Model recommended for hard tasks."),
            Opt::val(&["--threshold"], "FLOAT", "Pathway-match threshold (0-1); default: active backend's."),
            Opt::val(&["--harness"], "TEXT", "Host harness: claude-code, codex, ollama/local, hermes, openclaw."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " TASK", "Recommend the cheapest viable model/tier for a task.", &opts);
        }
        if p.args.is_empty() {
            return self.usage(CMD, "Missing argument 'task'.".into());
        }
        let task = p.args[0].clone();
        let has_threshold = p.has("--threshold");
        let mut threshold = self.click_float(CMD, &p, "--threshold", 0.0)?;
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let cheap = p.str("--cheap-model", DEFAULT_CHEAP_MODEL);
        let frontier = p.str("--frontier-model", DEFAULT_FRONTIER_MODEL);
        let harness = lower(strip(&p.str("--harness", "claude-code")));
        let advisor_tool = matches!(harness.as_str(), "claude-code" | "claude" | "anthropic");
        let db = format!("{data_dir}/pathways.db");
        let has_store = std::fs::metadata(&db).is_ok();
        let cfg = match super::config::load(&super::gateroot::join(&self.data_home(), "config.yaml")) {
            Ok(c) => c,
            Err(e) => return self.refuse(CMD, &format!("the config file is not one this binary reads: {e}")),
        };
        if !has_threshold {
            match threshold_of(&cfg.matcher_model) {
                Some(t) => threshold = t,
                None => {
                    self.errf(&format!("matcher_model={} is not a built embedder.\n", repr(&cfg.matcher_model)));
                    return exit(1);
                }
            }
        }
        let d = estimate_difficulty(&task);
        let mut advice: Option<Advice> = None;
        if has_store {
            let notes = Rc::new(RefCell::new(String::new()));
            let pe = self.potion_env(&notes);
            match select_matcher(&cfg.matcher_model, &pe) {
                Err(nc) => return self.refuse(CMD, &nc.0),
                Ok(None) => {
                    self.errf(&format!("matcher_model={} is not a built embedder.\n", repr(&cfg.matcher_model)));
                    return exit(1);
                }
                Ok(Some(_)) => {}
            }
            let s = Store::open(&db).map_err(|e| self.pw_err(CMD, e))?;
            let mut cache = Cache::default();
            let r = s.find(&task, &cfg.matcher_model, &pe, Some(threshold), &mut cache);
            self.drain_notes(&notes);
            match r {
                Err(FindErr::Err(PwErr::Unreadable(why))) => return self.refuse(CMD, &why),
                Err(FindErr::NotCarried(nc)) => return self.refuse(CMD, &nc.0),
                // A lookup that fails is no match: routing never breaks on
                // the store.
                Err(_) => {}
                Ok(r) => {
                    // The stale-embeddings UserWarning find prints, as
                    // Python's warnings module prints it under
                    // PYTHONWARNINGS; a filter not modelled is refused.
                    if !r.warning.is_empty() {
                        let pyw = self.env.get("PYTHONWARNINGS").cloned().unwrap_or_default();
                        match super::pywarn::user_warning_shown(&pyw, &r.warning) {
                            None => {
                                return self.refuse(
                                    CMD,
                                    &format!("the pathway store warns of stale embeddings under PYTHONWARNINGS={}, a filter this binary does not read the oracle's way", crate::gate::py::text::repr(&pyw)),
                                )
                            }
                            Some(true) => self.errf(&format!("UserWarning: {}\n", r.warning)),
                            Some(false) => {}
                        }
                    }
                    if let Some((pw, sim)) = r.matched {
                        let id = pw.id().to_string();
                        advice = Some(Advice {
                            tier: "tier0-pathway",
                            model: String::new(),
                            reason: format!(
                                "reuse distilled pathway {id} (similarity {}); re-verified against its envelope \u{2014} \
                                 near-zero LLM cost, provably in policy",
                                format_f(sim, 2)
                            ),
                            pathway: id,
                            pairing: false,
                        });
                    }
                }
            }
        }
        let a = advice.unwrap_or_else(|| {
            if d < 0.5 {
                Advice {
                    tier: "tier1-cheap",
                    model: cheap,
                    reason: format!("novel but low-difficulty ({}); cheap model suffices", format_f(d, 2)),
                    pathway: String::new(),
                    pairing: false,
                }
            } else if !advisor_tool {
                Advice {
                    tier: "tier2-frontier",
                    model: frontier,
                    reason: format!("novel and high-difficulty ({}); use the frontier model", format_f(d, 2)),
                    pathway: String::new(),
                    pairing: false,
                }
            } else {
                Advice {
                    tier: "tier2-frontier",
                    model: frontier,
                    reason: format!(
                        "novel and high-difficulty ({}); use the frontier \u{2014} or pair a cheap executor with an Opus \
                         advisor (Anthropic advisor tool, beta advisor-tool-2026-03-01) for comparable quality at lower cost",
                        format_f(d, 2)
                    ),
                    pathway: String::new(),
                    pairing: true,
                }
            }
        });
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("tier", a.tier);
            o.set("model", a.model.as_str());
            o.set("reason", a.reason.as_str());
            o.set("difficulty", d);
            o.set("pathway_id", if a.pathway.is_empty() { Value::Null } else { Value::Str(a.pathway.clone()) });
            o.set("advisor_pairing", a.pairing);
            self.out(&format!("{}\n", pyjson::dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let mut line = format!("route: {}", a.tier);
        if !a.model.is_empty() {
            line.push_str(&format!("  \u{2192}  {}", a.model));
        }
        self.out(&format!("{line}\n"));
        self.out(&format!("  difficulty: {}\n", format_f(d, 2)));
        if !a.pathway.is_empty() {
            self.out(&format!("  pathway:    {}\n", a.pathway));
        }
        self.out(&format!("  why:        {}\n", a.reason));
        Ok(())
    }
}
