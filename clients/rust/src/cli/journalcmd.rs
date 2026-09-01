//! `daisugi journal stats|replay|search`, and the group that also holds
//! `parse` and `ingest` (journalparse.rs): the trace journal with the
//! flags, output, exit codes and files of the Python CLI. The Go client's
//! `journalcmd.go` is the reference; rulings C-14 and C-15 say where it
//! differs from Python.

use std::time::Instant;

use super::statuscmd::{load_json, py_fixed};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{any_json, dumps_indent, Object, Value};
use crate::pathways::find::{select_matcher, Embedder};
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::pathways::store::Col;
use crate::pathways::PwErr;
use crate::tracejournal::{Journal, Record};
use crate::violation::Violation;

const JOURNAL_HELP: &str = "Usage: daisugi journal [OPTIONS] COMMAND [ARGS]...

  Inspect and query the trace journal.

Commands:
  search  Search journal traces by task (the lexical and potion matchers).
  replay  Re-run verify() on a stored trace and report drift.
  stats   Print aggregate stats from the journal index.
  parse   Parse an agent transcript into episodes.
  ingest  Ingest parsed episodes into the journal.
";

pub(super) const JOURNAL_DATA_DIR: Opt = Opt::val(&["--data-dir"], "PATH", "Root data directory containing journal/");

/// Python's `True` or `False`.
pub(super) fn py_bool(b: bool) -> &'static str {
    if b {
        "True"
    } else {
        "False"
    }
}

/// A violation dump: stage, message, detail and suggested_remediation.
pub(super) fn violation_dump(v: &Violation) -> Value {
    Value::Obj(
        Object::new()
            .with("stage", v.stage)
            .with("message", v.message.as_str())
            .with("detail", v.detail.clone().map(Value::Obj).unwrap_or(Value::Null))
            .with("suggested_remediation", v.remediation.clone().map(Value::Str).unwrap_or(Value::Null)),
    )
}

/// A `VerificationResult` dump of a verify() outcome.
pub(super) fn result_dump(ok: bool, violations: &[Violation], warnings: &[String], env_id: &Value, plan_id: &Value, ms: f64) -> Object {
    Object::new()
        .with("ok", ok)
        .with("violations", Value::List(violations.iter().map(violation_dump).collect()))
        .with("warnings", Value::List(warnings.iter().map(|w| Value::Str(w.clone())).collect()))
        .with("envelope_id", env_id.clone())
        .with("plan_id", plan_id.clone())
        .with("duration_ms", ms)
        .with("client", "python")
        .with("fallback", Value::Null)
        .with("client_verdict", Value::Null)
}

/// What `verify(plan, envelope, z3_timeout_ms=500)` of a stored trace
/// found.
struct Replayed {
    ok: bool,
    violations: Vec<Violation>,
    unmodeled: bool,
    dump: Object,
}

impl Env {
    pub(super) fn journal(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(JOURNAL_HELP);
            return Ok(());
        }
        let (sub, rest) = (args[0].as_str(), &args[1..]);
        match sub {
            "stats" => self.journal_stats(rest),
            "replay" => self.journal_replay(rest),
            "search" => self.journal_search(rest),
            "parse" => self.journal_parse(rest),
            "ingest" => self.journal_ingest(rest),
            _ => {
                self.errf(&format!(
                    "Usage: daisugi journal [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi journal --help' for help.\n\n\
                     Error: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `Journal(data_dir=...)`: the directories and the index made.
    pub(super) fn open_journal(&mut self, cmd: &str, data_dir: &str) -> Result<Journal, Stop> {
        Journal::open(data_dir).map_err(|e| self.pw_err(cmd, e))
    }

    /// Ends a command on a `load_trace` that raised: exit 1 naming the
    /// exception.
    pub(super) fn load_err(&mut self, cmd: &str, typ: &str, msg: &str) -> Stop {
        self.errf(&format!("daisugi {cmd}: {typ}: {msg}\n"));
        Stop::Exit(1)
    }

    fn journal_stats(&mut self, args: &[String]) -> Res {
        const CMD: &str = "journal stats";
        let opts = [JOURNAL_DATA_DIR, Opt::flag(&["--json"], "Emit JournalStats as JSON.")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Print aggregate stats from the journal index.", &opts);
        }
        let dir = self.data_dir_of(&p);
        let j = self.open_journal(CMD, &dir)?;
        let st = j.stats().map_err(|e| self.pw_err(CMD, e))?;
        let avg = st.avg_ms.unwrap_or(0.0);
        if p.flag("--json") {
            let o = Object::new()
                .with("total", Value::Int(st.total.to_string()))
                .with("passed", Value::Int(st.passed.to_string()))
                .with("failed", Value::Int(st.failed.to_string()))
                .with("avg_duration_ms", avg);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        self.out(&format!(
            "total: {}\npassed: {}\nfailed: {}\navg duration (ms): {}\n",
            st.total,
            st.passed,
            st.failed,
            py_fixed(avg, 2)
        ));
        Ok(())
    }

    /// `Journal.replay`'s verify() of a stored trace.
    fn replay_verify(&mut self, cmd: &str, rec: &Record) -> Result<Replayed, Stop> {
        let t0 = Instant::now();
        let out = crate::pathways::verify::verify(&Value::Obj(rec.plan.clone()), &Value::Obj(rec.envelope.clone()), None, 500);
        let ms = t0.elapsed().as_secs_f64() * 1000.0;
        let out = match out {
            Ok(o) => o,
            // verify() raises: the oracle's replay ends with a traceback.
            Err(PwErr::Invalid(why)) => return Err(self.fail(cmd, &why).unwrap_err()),
            Err(e) => return Err(self.pw_err(cmd, e)),
        };
        let ok = out.violations.is_empty();
        let dump = result_dump(ok, &out.violations, &out.warnings, rec.envelope.value("id"), rec.plan.value("id"), ms);
        Ok(Replayed { ok, violations: out.violations, unmodeled: out.warnings_unmodeled, dump })
    }

    fn journal_replay(&mut self, args: &[String]) -> Res {
        const CMD: &str = "journal replay";
        let opts = [JOURNAL_DATA_DIR, Opt::flag(&["--json"], "Emit ReplayResult as JSON.")];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " TRACE_ID", "Re-run verify() on a stored trace and report drift.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "TRACE_ID", "TRACE_ID");
        }
        let id = p.args[0].clone();
        let dir = self.data_dir_of(&p);
        let j = self.open_journal(CMD, &dir)?;
        let rec = match j.load_trace(&id).map_err(|e| self.pw_err(CMD, e))? {
            Ok(r) => r,
            Err(le) if le.typ == "FileNotFoundError" => {
                self.out(&format!("Trace not found: {id}\n"));
                return exit(2);
            }
            Err(le) => return Err(self.load_err(CMD, &le.typ, &le.msg)),
        };
        let r = self.replay_verify(CMD, &rec)?;
        let orig_ok = rec.result.value("ok") == &Value::Bool(true);
        let drift = orig_ok != r.ok;
        if p.flag("--json") {
            if r.unmodeled || r.violations.iter().any(|v| v.detail.is_none()) {
                // A violation's detail outside the permissions and DAG
                // stages is not worded by this binary's verifier (C-14).
                return self.refuse(CMD, "the replayed result holds a violation whose detail this binary does not write yet");
            }
            let o = Object::new()
                .with("trace_id", any_json(&rec.id))
                .with("original_ok", orig_ok)
                .with("replayed_ok", r.ok)
                .with("drift", drift)
                .with("original_result", any_json(&Value::Obj(rec.result.clone())))
                .with("replayed_result", Value::Obj(r.dump));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            let trace_id = match super::statuscmd::py_str_plain(&rec.id) {
                Some(s) => s,
                None => return self.refuse(CMD, "the trace's id is not a string"),
            };
            let mut b = String::new();
            if drift {
                b.push_str(&format!("{trace_id}: DRIFT detected\n"));
                b.push_str(&format!("  original: ok={}\n", py_bool(orig_ok)));
                b.push_str(&format!("  replayed: ok={}\n", py_bool(r.ok)));
                if !r.violations.is_empty() {
                    b.push_str("  new violations:\n");
                    for v in &r.violations {
                        b.push_str(&format!("    - [{}] {}\n", v.stage, v.message));
                    }
                }
            } else {
                b.push_str(&format!("{trace_id}: no drift (ok={})\n", py_bool(orig_ok)));
            }
            self.out(&b);
        }
        if drift {
            return exit(1);
        }
        Ok(())
    }

    fn journal_search(&mut self, args: &[String]) -> Res {
        const CMD: &str = "journal search";
        let opts = [
            JOURNAL_DATA_DIR,
            Opt::val(&["--limit"], "INTEGER", "Maximum number of results."),
            Opt::flag(&["--json"], "Emit result rows as JSON array."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " QUERY", "Search journal traces by task (the lexical and potion matchers).", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "QUERY", "QUERY");
        }
        let limit = self.click_int(CMD, &p, "--limit", 10)?;
        let query = p.args[0].clone();
        // The matcher is the one ~/.opendaisugi/config.yaml names. A
        // matcher this binary does not carry is refused before Journal()
        // makes anything; an error Python raises comes after it, as in
        // Python.
        let cfg = super::config::load(&super::gateroot::join(&self.home, ".opendaisugi/config.yaml"));
        if let Err(e @ super::config::ConfigErr::Unsupported) = &cfg {
            return Err(self.config_load_err(CMD, &super::config::RowsErr::Config(e.clone())));
        }
        let mut emb: Option<Embedder> = None;
        let mut unbuilt = false;
        if let Ok(c) = &cfg {
            let notes = std::rc::Rc::new(std::cell::RefCell::new(String::new()));
            let pe = self.potion_env(&notes);
            match select_matcher(&c.matcher_model, &pe) {
                Err(nc) => return self.refuse(CMD, &nc.0),
                Ok(None) => unbuilt = true,
                Ok(Some(m)) => {
                    // A potion model that cannot be had is refused here,
                    // before Journal() makes anything; Python raises after
                    // it.
                    let loaded = crate::distill::load_embedder(&m, &pe);
                    self.drain_notes(&notes);
                    match loaded {
                        Ok(e) => emb = Some(e),
                        Err(why) => return self.refuse(CMD, &why),
                    }
                }
            }
        }
        let dir = self.data_dir_of(&p);
        let j = self.open_journal(CMD, &dir)?;
        let cfg = match cfg {
            Ok(c) => c,
            Err(e) => return Err(self.config_load_err(CMD, &super::config::RowsErr::Config(e))),
        };
        if unbuilt {
            self.errf(&format!(
                "matcher_model={} is not a built embedder. Built: 'all-MiniLM-L6-v2' with torch, 'potion' with no \
                 torch, 'lexical' with no model, 'int8' with onnx and no torch.\n",
                repr(&cfg.matcher_model)
            ));
            return exit(1);
        }
        let emb = emb.expect("set for a built matcher");
        let rows = j.list_recent(10_000).map_err(|e| self.pw_err(CMD, e))?;
        let mut traces = Vec::with_capacity(rows.len());
        let mut tasks = Vec::with_capacity(rows.len());
        for r in &rows {
            match trace_row(r) {
                Ok((t, task)) => {
                    traces.push(t);
                    tasks.push(task);
                }
                Err(why) => return self.refuse(CMD, why),
            }
        }
        let mut results: Vec<&Object> = vec![];
        if !traces.is_empty() {
            let q = emb.encode(&query);
            let qn = dot(&q, &q).sqrt();
            let scores: Vec<f64> = tasks
                .iter()
                .map(|task| {
                    let c = emb.encode(task);
                    let mut denom = pairwise(&c).sqrt() * qn;
                    if denom == 0.0 {
                        denom = 1e-9;
                    }
                    (dot(&c, &q) / denom).clamp(-1.0, 1.0)
                })
                .collect();
            let mut order: Vec<usize> = (0..scores.len()).collect();
            // argsort(-scores): equal scores keep their order here; numpy's
            // SIMD sort may order them otherwise (C-15).
            order.sort_by(|&a, &b| (-scores[a]).partial_cmp(&-scores[b]).unwrap_or(std::cmp::Ordering::Equal));
            for i in py_slice(order.len(), limit) {
                results.push(&traces[order[i]]);
            }
        }
        if p.flag("--json") {
            let out = Value::List(results.iter().map(|t| Value::Obj((*t).clone())).collect());
            self.out(&format!("{}\n", dumps_indent(&out, 2, true)));
            return Ok(());
        }
        if results.is_empty() {
            self.out("(no matching traces)\n");
            return Ok(());
        }
        let mut b = String::new();
        for t in results {
            let status = if t.value("ok") == &Value::Bool(true) { "ok" } else { "FAIL" };
            b.push_str(&format!(
                "{}  [{status}]  {}\n",
                t.value("id").as_str().unwrap_or(""),
                t.value("task").as_str().unwrap_or("")
            ));
        }
        self.out(&b);
        Ok(())
    }
}

/// The indexes of `seq[:limit]` for a sequence of n items.
fn py_slice(n: usize, limit: i128) -> std::ops::Range<usize> {
    let n = n as i128;
    let end = if limit < 0 { (n + limit).max(0) } else { limit.min(n) };
    0..end as usize
}

fn dot(a: &[f64], b: &[f64]) -> f64 {
    a.iter().zip(b).map(|(x, y)| x * y).sum()
}

/// numpy's pairwise sum of the squares of v, as `np.linalg.norm(c,
/// axis=1)` sums a row.
fn pairwise(v: &[f64]) -> f64 {
    let n = v.len();
    if n < 8 {
        return v.iter().map(|x| x * x).fold(0.0, |s, x| s + x);
    }
    if n <= 128 {
        let mut r = [0.0f64; 8];
        let mut i = 0;
        while i < n - n % 8 {
            for k in 0..8 {
                r[k] += v[i + k] * v[i + k];
            }
            i += 8;
        }
        let mut s = ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]));
        while i < n {
            s += v[i] * v[i];
            i += 1;
        }
        return s;
    }
    let mut n2 = n / 2;
    n2 -= n2 % 8;
    pairwise(&v[..n2]) + pairwise(&v[n2..])
}

/// `Trace(...)` of one `list_recent` row, dumped in JSON mode, and its
/// task.
fn trace_row(r: &[Col]) -> Result<(Object, String), &'static str> {
    let text = |i: usize| -> Result<String, &'static str> {
        match &r[i] {
            Col::Text(s) => Ok(s.clone()),
            _ => Err("a journal row holds a value that is not text"),
        }
    };
    let (id, created, task, plan_id, env_id) = (text(0)?, text(1)?, text(2)?, text(3)?, text(4)?);
    let ok = match &r[5] {
        Col::Int(x) => *x != 0,
        _ => return Err("a journal row's ok is not an integer"),
    };
    let dur = match &r[6] {
        Col::Real(x) => *x,
        Col::Int(x) => *x as f64,
        _ => return Err("a journal row's duration is not a number"),
    };
    let vtext = match &r[7] {
        Col::Text(s) => s.clone(),
        _ => return Err("a journal row's violations are not text"),
    };
    let list = match load_json(&vtext) {
        Ok(Value::List(l)) => l,
        Ok(_) => return Err("a journal row's violations are not a list"),
        Err(_) => return Err("a journal row's violations are not JSON this binary reads"),
    };
    let mut violations = Vec::with_capacity(list.len());
    for x in &list {
        if !matches!(x, Value::Obj(_)) {
            return Err("a journal row's violation is not an object");
        }
        match validate_model(Id::Violation, x, Mode::Python) {
            Ok(v) => violations.push(any_json(&v)),
            Err(_) => return Err("a journal row's violation does not validate"),
        }
    }
    let t = Object::new()
        .with("id", id)
        .with("created_at", created)
        .with("task", task.as_str())
        .with("plan_id", plan_id)
        .with("envelope_id", env_id)
        .with("ok", ok)
        .with("duration_ms", dur)
        .with("violations", Value::List(violations));
    Ok((t, task))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_slice_is_pythons() {
        assert_eq!(py_slice(5, 10), 0..5);
        assert_eq!(py_slice(5, 2), 0..2);
        assert_eq!(py_slice(5, -2), 0..3);
        assert_eq!(py_slice(5, -9), 0..0);
        assert_eq!(py_slice(5, 0), 0..0);
    }

    #[test]
    fn pairwise_sums_as_numpy_does() {
        let v: Vec<f64> = (0..300).map(|i| (i as f64 * 0.37).sin()).collect();
        let plain: f64 = v.iter().map(|x| x * x).sum();
        assert!((pairwise(&v) - plain).abs() < 1e-9);
        assert_eq!(pairwise(&[3.0, 4.0]), 25.0);
    }
}
