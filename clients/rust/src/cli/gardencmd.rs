//! `daisugi gardener prune|merge|run|watch|status`: the pathway store's
//! life cycle, with the flags, output, exit codes and files of the Python
//! CLI.

use super::pathwayscmd::{py_int, DATA_DIR};
use super::{exit, parse_args, Env, Opt, Parsed, Res, Stop};
use crate::garden::{self, format_f, int_of, MergeConfig, MergeReport, PruneConfig, PruneReport};
use crate::gate::argparse::py_float;
use crate::gate::py::text::repr;
use crate::gate::pyjson::{dumps, dumps_indent, round, Object, Value};
use crate::pathways::store::Store;
use crate::pathways::PwErr;

const GARDENER_HELP: &str = "Usage: daisugi gardener [OPTIONS] COMMAND [ARGS]...

  Lifecycle management for compiled pathways (prune, merge, status).

Commands:
  prune   Evict stale / failure-dominated pathways.
  merge   Collapse near-duplicate pathways.
  run     Run the full gardener pipeline (prune + merge).
  watch   Cron-friendly one-shot gardener.
  status  Report current store size, pathway activation stats, failure ratios.
";

const DRY_RUN: Opt = Opt::flag(&["--dry-run"], "Report what would change; write nothing.");
const JSON: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

/// `time.time()`.
pub fn now_seconds() -> f64 {
    match std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
        Ok(d) => d.as_secs_f64(),
        Err(_) => 0.0,
    }
}

fn strs(ss: &[String]) -> Value {
    Value::List(ss.iter().map(|s| Value::Str(s.clone())).collect())
}

fn pairs(ps: &[(String, String)]) -> Value {
    Value::List(ps.iter().map(|(a, b)| Value::List(vec![Value::Str(a.clone()), Value::Str(b.clone())])).collect())
}

fn prune_json(r: &PruneReport, dry: Option<bool>) -> Object {
    let ids: Vec<String> = r.removed.iter().map(|(id, _)| id.clone()).collect();
    let mut o = Object::new();
    o.set("removed_ids", strs(&ids));
    o.set("kept_count", r.kept as i64);
    let mut reasons = Object::new();
    for (id, why) in &r.removed {
        reasons.set(id, why.as_str());
    }
    o.set("reasons", reasons);
    if let Some(d) = dry {
        o.set("dry_run", d);
    }
    o
}

impl Env {
    pub(super) fn gardener(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(GARDENER_HELP);
            return Ok(());
        }
        let (sub, rest) = (args[0].as_str(), &args[1..]);
        match sub {
            "prune" => self.gardener_prune(rest),
            "merge" => self.gardener_merge(rest),
            "run" => self.gardener_run(rest),
            "watch" => self.gardener_watch(rest),
            "status" => self.gardener_status(rest),
            _ => {
                self.errf(&format!(
                    "Usage: daisugi gardener [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi gardener --help' for help.\n\n\
                     Error: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// A FLOAT option: Python's `float()` of the text.
    pub(super) fn click_float(&mut self, cmd: &str, p: &Parsed, name: &str, def: f64) -> Result<f64, Stop> {
        if !p.has(name) {
            return Ok(def);
        }
        let raw = p.str(name, "");
        match py_float(&raw) {
            Some(f) => Ok(f),
            None => Err(self.usage_stop(cmd, format!("Invalid value for '{name}': {} is not a valid float.", repr(&raw)))),
        }
    }

    /// An INTEGER option: Python's `int()` of the text.
    pub(super) fn click_int(&mut self, cmd: &str, p: &Parsed, name: &str, def: i128) -> Result<i128, Stop> {
        if !p.has(name) {
            return Ok(def);
        }
        let raw = p.str(name, "");
        match py_int(&raw) {
            Some(n) => Ok(n),
            None => Err(self.usage_stop(cmd, format!("Invalid value for '{name}': {} is not a valid int.", repr(&raw)))),
        }
    }

    fn gardener_prune(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gardener prune";
        let opts = [
            DATA_DIR,
            Opt::val(&["--max-idle-days"], "FLOAT", "Evict a pathway idle longer than this."),
            Opt::val(&["--max-failure-ratio"], "FLOAT", "Evict a pathway failing more often than this."),
            Opt::val(&["--min-activations"], "INTEGER", "Activations before the failure ratio counts."),
            DRY_RUN,
            JSON,
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Evict stale / failure-dominated pathways.", &opts);
        }
        let cfg = PruneConfig {
            max_idle_days: self.click_float(CMD, &p, "--max-idle-days", 30.0)?,
            max_failure_ratio: self.click_float(CMD, &p, "--max-failure-ratio", 0.5)?,
            min_activations: self.click_int(CMD, &p, "--min-activations", 5)?,
        };
        let s = self.open_store(CMD, &p)?;
        let dry = p.flag("--dry-run");
        let rep = garden::prune(&s, &cfg, dry, now_seconds()).map_err(|e| self.pw_err(CMD, e))?;
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(prune_json(&rep, Some(dry))), 2, true)));
            return Ok(());
        }
        let verb = if dry { "would remove" } else { "removed" };
        let mut b = format!("{verb}: {} (kept: {})\n", rep.removed.len(), rep.kept);
        for (id, why) in &rep.removed {
            b.push_str(&format!("  {id} \u{2014} {why}\n"));
        }
        self.out(&b);
        Ok(())
    }

    fn gardener_merge(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gardener merge";
        let opts = [
            DATA_DIR,
            Opt::val(&["--similarity"], "FLOAT", "Cosine similarity at which two pathways merge."),
            DRY_RUN,
            JSON,
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Collapse near-duplicate pathways.", &opts);
        }
        let cfg = MergeConfig { similarity_threshold: self.click_float(CMD, &p, "--similarity", 0.92)?, ..Default::default() };
        let s = self.open_store(CMD, &p)?;
        let dry = p.flag("--dry-run");
        let rep = garden::merge(&s, &cfg, dry).map_err(|e| self.pw_err(CMD, e))?;
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("merged_pairs", pairs(&rep.merged_pairs));
            o.set("kept_ids", strs(&rep.kept_ids));
            o.set("removed_ids", strs(&rep.removed_ids));
            o.set("dry_run", dry);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let verb = if dry { "would merge" } else { "merged" };
        let mut b = format!("{verb}: {} pair(s)\n", rep.merged_pairs.len());
        for (w, l) in &rep.merged_pairs {
            b.push_str(&format!("  {w}  <-  {l}\n"));
        }
        self.out(&b);
        Ok(())
    }

    /// `gardener.run_gardener`: prune, then merge, each with its defaults.
    fn run_gardener(&mut self, cmd: &str, s: &Store, dry: bool) -> Result<(PruneReport, MergeReport), Stop> {
        let pr = garden::prune(s, &PruneConfig::default(), dry, now_seconds()).map_err(|e| self.pw_err(cmd, e))?;
        let mr = garden::merge(s, &MergeConfig::default(), dry).map_err(|e| self.pw_err(cmd, e))?;
        Ok((pr, mr))
    }

    fn gardener_run(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gardener run";
        let opts = [DATA_DIR, DRY_RUN, JSON];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Run the full gardener pipeline (prune + merge).", &opts);
        }
        let s = self.open_store(CMD, &p)?;
        let dry = p.flag("--dry-run");
        let (pr, mr) = self.run_gardener(CMD, &s, dry)?;
        if p.flag("--json") {
            let mut m = Object::new();
            m.set("merged_pairs", pairs(&mr.merged_pairs));
            m.set("kept_ids", strs(&mr.kept_ids));
            let mut o = Object::new();
            o.set("prune", prune_json(&pr, None));
            o.set("merge", m);
            o.set("dry_run", dry);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let verb = if dry { "would" } else { "" };
        self.out(&format!(
            "prune: {verb} removed {}, kept {}\nmerge: {verb} merged {} pair(s)\n",
            pr.removed.len(),
            pr.kept,
            mr.merged_pairs.len()
        ));
        Ok(())
    }

    fn gardener_watch(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gardener watch";
        let opts = [
            DATA_DIR,
            Opt::val(&["--min-interval"], "INTEGER", "Skip if the last run is newer than this many seconds."),
            Opt::flag(&["--force"], "Ignore the min-interval check."),
            DRY_RUN,
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Cron-friendly one-shot gardener. Skips if last run is within --min-interval.", &opts);
        }
        let min_interval = self.click_int(CMD, &p, "--min-interval", 3600)?;
        let dir = p.str("--data-dir", &format!("{}/.opendaisugi", self.home));
        let stamp = format!("{dir}/.gardener-last-run");
        let now = now_seconds();
        let last = read_stamp(&stamp).map_err(|e| self.pw_err(CMD, e))?;
        let elapsed = now - last;
        if !p.flag("--force") && elapsed < min_interval as f64 {
            let mut o = Object::new();
            o.set("skipped", true);
            o.set("reason", "min_interval_not_elapsed");
            o.set("elapsed_s", round(elapsed, 1));
            o.set("min_interval_s", Value::Int(min_interval.to_string()));
            self.out(&format!("{}\n", dumps(&Value::Obj(o), true)));
            return Ok(());
        }
        let s = self.open_store(CMD, &p)?;
        let dry = p.flag("--dry-run");
        let (pr, mr) = self.run_gardener(CMD, &s, dry)?;
        if !dry {
            let write = std::fs::create_dir_all(&dir).and_then(|_| std::fs::write(&stamp, format_f(now, 3)));
            if let Err(e) = write {
                return Err(self.pw_err(CMD, PwErr::Io(e)));
            }
        }
        let mut pj = Object::new();
        pj.set("removed", pr.removed.len() as i64);
        pj.set("kept", pr.kept as i64);
        let mut mj = Object::new();
        mj.set("merged", mr.merged_pairs.len() as i64);
        let mut o = Object::new();
        o.set("skipped", false);
        o.set("ran_at", now);
        o.set("dry_run", dry);
        o.set("prune", pj);
        o.set("merge", mj);
        self.out(&format!("{}\n", dumps(&Value::Obj(o), true)));
        Ok(())
    }

    fn gardener_status(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gardener status";
        let opts = [DATA_DIR, JSON];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Report current store size, pathway activation stats, failure ratios.", &opts);
        }
        let s = self.open_store(CMD, &p)?;
        let all = s.read_all(&|_| false).map_err(|e| self.pw_err(CMD, e))?;
        if p.flag("--json") {
            let items: Vec<Value> = all
                .iter()
                .map(|pw| {
                    let mut x = Object::new();
                    for k in ["id", "hit_count", "failure_count", "last_activation_at"] {
                        x.set(k, pw.obj.value(k).clone());
                    }
                    Value::Obj(x)
                })
                .collect();
            let mut o = Object::new();
            o.set("count", all.len() as i64);
            o.set("pathways", Value::List(items));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let mut b = format!("count: {}\n", all.len());
        for pw in &all {
            let (h, f) = (int_of(pw.obj.value("hit_count")), int_of(pw.obj.value("failure_count")));
            let total = h + f;
            let ratio = if total == 0 { 0.0 } else { garden::true_div(f, total) };
            b.push_str(&format!("  {}  hits={h}  fails={f}  fail_ratio={}\n", pw.id(), format_f(ratio, 2)));
        }
        self.out(&b);
        Ok(())
    }
}

/// The last-run stamp as the oracle reads it: a float, or 0 when the file
/// is absent or its text is not one.
pub fn read_stamp(path: &str) -> Result<f64, PwErr> {
    let raw = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(0.0),
        Err(e) => return Err(PwErr::Io(e)),
    };
    let text = match String::from_utf8(raw) {
        Ok(t) => t,
        Err(_) => return Err(PwErr::Invalid("UnicodeDecodeError: the stamp file is not UTF-8".into())),
    };
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    Ok(py_float(crate::gate::py::text::strip(&text)).unwrap_or(0.0))
}
