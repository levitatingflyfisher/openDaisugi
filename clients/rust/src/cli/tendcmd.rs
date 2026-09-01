//! `daisugi tend` and `daisugi distill-repeats`: the distiller and the
//! gateway worklist, with the flags, output, exit codes and files of the
//! Python CLI. Every refusal comes before the first write.

use std::cell::RefCell;
use std::rc::Rc;

use super::gardencmd::now_seconds;
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::distill::repeats::{load_turns, rank_reuse};
use crate::distill::{load_embedder, prepare, Distiller, Options, Report, Sink, TendErr};
use crate::gate::py::text::repr;
use crate::llm::Client;
use crate::pathways::find::{select_matcher, Matcher, EMBEDDING_MODEL_VERSION};
use crate::pathways::potion;
use crate::pathways::store::{Col, Store};
use crate::pathways::PwErr;
use crate::tracejournal::{ensure_envelope_cache, Distillable, Journal};

/// The model tend asks by default.
pub const DEFAULT_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

/// How auto-tend reports a tend that raised: "tend failed: <exception
/// class name>: <message>".
pub fn tend_failure(msg: &str) -> String {
    if let Some((head, rest)) = msg.split_once(": ") {
        if !head.contains(' ') {
            let name = head.rsplit('.').next().unwrap_or(head);
            return format!("tend failed: {name}: {rest}");
        }
    }
    format!("tend failed: {msg}")
}

/// What the read checks of a tend found, before it writes.
pub struct TendState {
    matcher: Option<Matcher>,
    identity: String,
    client: Client,
    traces: Vec<Distillable>,
}

/// A tend report and how long the run took.
pub struct TendReport {
    pub rep: Report,
    pub seconds: f64,
}

/// Why the checks of a tend stopped it.
pub enum CheckStop {
    /// `Daisugi()` raised before tend ran: the CLI prints the message and
    /// exits 1, and auto-tend does not catch it.
    Raised(String),
    Stop(Stop),
}

impl From<Stop> for CheckStop {
    fn from(s: Stop) -> Self {
        CheckStop::Stop(s)
    }
}

/// `active_model_name` for a matcher key.
fn identity_of(m: &Option<Matcher>, key: &str) -> String {
    match m {
        Some(m) => m.identity().to_string(),
        None if key == "int8" => "all-MiniLM-L6-v2-int8".into(),
        None => key.to_string(),
    }
}

impl Env {
    /// The potion settings of this run; a download notice goes to `notes`.
    pub(super) fn potion_env(&self, notes: &Rc<RefCell<String>>) -> potion::Env {
        let n = notes.clone();
        let mut pe = potion::Env::from_process(self.env.clone(), self.home.clone());
        pe.notice = Box::new(move |s| {
            n.borrow_mut().push_str(s);
            n.borrow_mut().push('\n');
        });
        pe
    }

    fn drain_notes(&mut self, notes: &Rc<RefCell<String>>) {
        let text = std::mem::take(&mut *notes.borrow_mut());
        self.errf(&text);
    }

    /// Prints what the model client printed, then empties it.
    pub(super) fn drain_client(&mut self, c: &mut Client) {
        let out = std::mem::take(&mut c.stdout);
        let err = std::mem::take(&mut c.stderr);
        self.out(&out);
        self.errf(&err);
    }

    pub(super) fn tend(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tend";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(&["--model"], "TEXT", "Model used for template generalization + improvement."),
            Opt::val(&["--min-traces"], "INTEGER", "Minimum cluster size to distill."),
            Opt::val(&["--lookback-days"], "INTEGER", "How far back to scan the journal."),
            Opt::flag(&["--dry-run"], "Run the pipeline but do not store pathways."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Run the distiller. Scans successful traces and produces compiled pathways.", &opts);
        }
        let min_traces = self.click_int(CMD, &p, "--min-traces", 3)?;
        let lookback = self.click_int(CMD, &p, "--lookback-days", 30)?;
        let model = p.str("--model", DEFAULT_MODEL);
        let data_dir = p.str("--data-dir", &format!("{}/.opendaisugi", self.home));
        let r = self.run_tend(&data_dir, &model, min_traces, lookback, p.flag("--dry-run"))?;
        let mut b = format!(
            "tend complete: created={} updated={} skipped={} in {:.1}s\n",
            r.rep.created, r.rep.updated, r.rep.skipped, r.seconds
        );
        if !r.rep.pathways.is_empty() {
            b.push_str(&format!("  {} pathway(s): {}\n", r.rep.pathways.len(), r.rep.pathways.join(", ")));
        }
        for w in &r.rep.warnings {
            b.push_str(&format!("  warning: {w}\n"));
        }
        self.out(&b);
        Ok(())
    }

    /// Every refusal of a tend run, made before anything is written: the
    /// matcher, the model, the journal's traces and refinements and the
    /// store's rows. `extra` is how many traces the caller adds first
    /// (auto-tend's conversions). A matcher the binary does not carry is
    /// refused only when the run would load its embedder: a stale row to
    /// re-embed, or enough traces to cluster.
    #[allow(clippy::too_many_arguments)]
    pub(super) fn tend_check(
        &mut self,
        data_dir: &str,
        model: &str,
        min_traces: i128,
        lookback: i128,
        dry: bool,
        extra: usize,
        started: f64,
        notes: &Rc<RefCell<String>>,
    ) -> Result<TendState, CheckStop> {
        const CMD: &str = "tend";
        let cfg = match super::config::load(&format!("{}/.opendaisugi/config.yaml", self.home)) {
            Ok(c) => c,
            Err(e) => return Err(self.refuse(CMD, &format!("the config file is not one this binary reads: {e}")).unwrap_err().into()),
        };
        let pe = self.potion_env(notes);
        let (matcher, not_carried) = match select_matcher(&cfg.matcher_model, &pe) {
            Ok(Some(m)) => (Some(m), None),
            // Daisugi() resolves the matcher's threshold before it makes
            // anything, and raises for a key nothing builds.
            Ok(None) => return Err(CheckStop::Raised(format!("matcher_model={} is not a built embedder.", repr(&cfg.matcher_model)))),
            Err(nc) => (None, Some(nc.0)),
        };
        let identity = identity_of(&matcher, &cfg.matcher_model);
        let client = Client::new(self.env.clone(), &self.home);
        if let Err(why) = client.check(model) {
            return Err(self.refuse(CMD, &why).unwrap_err().into());
        }
        if std::fs::metadata(data_dir).map(|m| !m.is_dir()).unwrap_or(false) {
            self.errf(&format!("daisugi {CMD}: FileExistsError: {data_dir} exists and is not a directory\n"));
            return Err(Stop::Exit(1).into());
        }
        let since = started - lookback as f64 * 86_400.0;
        let mut traces = vec![];
        if std::fs::metadata(format!("{data_dir}/journal/index.db")).is_ok() {
            // Read only: an old journal is migrated when tend runs, after
            // every refusal.
            let j = Journal::open_read_only(data_dir).map_err(|e| self.pw_err(CMD, e))?;
            traces = j.list_successful(Some(since)).map_err(|e| self.pw_err(CMD, e))?;
            prepare(&j, &traces).map_err(|e| self.pw_err(CMD, e))?;
        }
        let mut stale = false;
        let db = format!("{data_dir}/pathways.db");
        if !dry && std::fs::metadata(&db).is_ok() {
            // Read only, as the journal: the store is migrated after every
            // refusal.
            let s = Store::open_read_only(&db).map_err(|e| self.pw_err(CMD, e))?;
            if let Err(e @ PwErr::Unreadable(_)) = s.read_all(&|_| false) {
                return Err(self.pw_err(CMD, e).into());
            }
            let rows = s.provenance().map_err(|e| self.pw_err(CMD, e))?;
            let is = |c: &Col, v: &str| matches!(c, Col::Text(t) if t == v);
            stale = rows.iter().any(|r| !is(&r[2], &identity) || !is(&r[3], EMBEDDING_MODEL_VERSION));
        }
        if let Some(why) = not_carried {
            if stale || (traces.len() + extra) as i128 >= min_traces {
                return Err(self.refuse(CMD, &why).unwrap_err().into());
            }
        }
        Ok(TendState { matcher, identity, client, traces })
    }

    /// `Daisugi(...).tend(min_traces, lookback_days)`: the checks first,
    /// with nothing written; then the envelope cache, the journal and the
    /// store opened (and made), and the distiller run.
    pub(super) fn run_tend(&mut self, data_dir: &str, model: &str, min_traces: i128, lookback: i128, dry: bool) -> Result<TendReport, Stop> {
        const CMD: &str = "tend";
        let started = now_seconds();
        let notes = Rc::new(RefCell::new(String::new()));
        let st = match self.tend_check(data_dir, model, min_traces, lookback, dry, 0, started, &notes) {
            Ok(st) => st,
            Err(CheckStop::Raised(msg)) => {
                self.errf(&format!("{msg}\n"));
                self.raised = true;
                return Err(Stop::Exit(1));
            }
            Err(CheckStop::Stop(s)) => return Err(s),
        };
        let TendState { matcher, identity, mut client, traces } = st;
        // From here on the run writes, so input this binary cannot read
        // stops it without the refusal's "Nothing was changed.".
        ensure_envelope_cache(&format!("{data_dir}/envelope_cache.db")).map_err(|e| self.after_writes(CMD, e))?;
        let j = Journal::open(data_dir).map_err(|e| self.after_writes(CMD, e))?;
        let prepared = prepare(&j, &traces).map_err(|e| self.after_writes(CMD, e))?;
        let store = if dry {
            None
        } else {
            Some(Store::open(&format!("{data_dir}/pathways.db")).map_err(|e| self.after_writes(CMD, e))?)
        };
        let pe = self.potion_env(&notes);
        let threshold = matcher.as_ref().map(|m| m.threshold()).unwrap_or(0.0);
        let rep = {
            let mut d = Distiller {
                prepared,
                sink: match &store {
                    Some(s) => Sink::Disk(s),
                    None => Sink::Mem(vec![]),
                },
                matcher,
                pe: &pe,
                emb: Default::default(),
                llm: &mut client,
                opt: Options {
                    model: model.to_string(),
                    min_traces,
                    lookback_days: lookback,
                    threshold,
                    validation_split: 0.6,
                    structure_weight: 0.5,
                    identity,
                    z3_timeout_ms: 500,
                },
                now: &now_seconds,
            };
            d.tend(&traces)
        };
        self.drain_notes(&notes);
        self.drain_client(&mut client);
        match rep {
            Ok(rep) => Ok(TendReport { rep, seconds: now_seconds() - started }),
            Err(TendErr::Pw(e)) => Err(self.after_writes(CMD, e)),
            Err(TendErr::Os(e)) => Err(self.pw_err(CMD, PwErr::Io(e))),
        }
    }

    /// An error once a run has written: as `pw_err`, except that input
    /// this binary cannot read stops the run (exit 2) with no claim that
    /// nothing changed. The checks before the first write read the same
    /// input, so no case reaches this.
    fn after_writes(&mut self, cmd: &str, e: PwErr) -> Stop {
        match e {
            PwErr::Unreadable(why) => {
                self.errf(&format!("daisugi {cmd}: {why}. The run stopped after its first write.\n"));
                Stop::Exit(2)
            }
            other => self.pw_err(cmd, other),
        }
    }

    pub(super) fn distill_repeats(&mut self, args: &[String]) -> Res {
        const CMD: &str = "distill-repeats";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory (reads <data-dir>/gateway/turns.jsonl)."),
            Opt::val(&["--top"], "INTEGER", "Maximum number of rows to print."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Rank repeated gateway asks into a reuse worklist.", &opts);
        }
        let top = self.click_int(CMD, &p, "--top", 20)?;
        let data_dir = p.str("--data-dir", &format!("{}/.opendaisugi", self.home));
        // Skipped lines are logged on the opendaisugi logger, which prints
        // nothing by default.
        let turns = load_turns(&format!("{data_dir}/gateway/turns.jsonl")).map_err(|e| self.pw_err(CMD, e))?;
        if turns.is_empty() {
            self.out("no turns recorded yet \u{2014} run `daisugi gateway` to start journaling.\n");
            return Ok(());
        }
        let mut cands = vec![];
        if turns.iter().any(|t| !t.signature.is_empty()) {
            let cfg = match super::config::load(&format!("{}/.opendaisugi/config.yaml", self.home)) {
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
            let db = format!("{data_dir}/pathways.db");
            let store = if std::fs::metadata(&db).is_ok() { Some(Store::open(&db).map_err(|e| self.pw_err(CMD, e))?) } else { None };
            let key = cfg.matcher_model.clone();
            let mut cache = crate::pathways::find::Cache::default();
            let mut find = |task: &str| -> bool {
                match &store {
                    Some(s) => matches!(s.find(task, &key, &pe, None, &mut cache), Ok(r) if r.matched.is_some()),
                    None => false,
                }
            };
            cands = rank_reuse(&turns, &emb, m.threshold(), &mut find);
            self.drain_notes(&notes);
        }
        if cands.is_empty() {
            self.out("no repeated asks found yet.\n");
            return Ok(());
        }
        let mut b = format!("{:>4}  {:>4}  {:>10}  {:>9}  reusable?  task\n", "rank", "occ", "tokens", "dollars");
        // candidates[:top], a negative top counting from the end.
        let n = cands.len() as i128;
        let n = if top < 0 { (n + top).max(0) } else { top.min(n) } as usize;
        for (i, c) in cands[..n].iter().enumerate() {
            b.push_str(&format!("{}\n", c.row(i + 1)));
        }
        self.out(&b);
        Ok(())
    }
}
