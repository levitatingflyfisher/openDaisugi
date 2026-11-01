//! `daisugi rank fit|choose|queue|record`: `rank.py` through its commands.
//! The Go client's `cli/rankcmd.go` is the reference.

use std::collections::HashMap;

use super::wardenroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::warden::pyjson::{dumps_indent, loads_py, Object, Value};
use crate::rank::{self, Ranking};

const RANK_HELP: &str = "Usage: daisugi rank [OPTIONS] COMMAND [ARGS]...

  Rank N attempts at one task, record the choice, and review it later.

Options:
  --help  Show this message and exit.

Commands:
  fit     Rank the attempts: eliminate, then quality, then the judges'...
  choose  Rank the attempts, take the leader, and open a card when there...
  show    Show a ranking as the journal holds it: votes, choices, card,...
  judge   Ask a judge model to compare the next pair, in both orders, and...
  judges  Show how far each judge agrees with the owner, and its current...
  pivot   Show what switching a card to another attempt would do; with...
  queue   List the open cards: what was chosen, why, and what switching...
  record  Record the owner's answer on a card.
";

const RANK_RECORD_HELP: &str = "Usage: daisugi rank record [OPTIONS] COMMAND [ARGS]...

  Record the owner's answer on a card. Only the owner runs these.

Options:
  --help  Show this message and exit.

Commands:
  pick  Pick an attempt on a card: the chosen one confirms it, another...
  drop  Drop a card: the chosen attempt is kept, recorded as confirmed.
";

const OPTS: [Opt; 2] =
    [Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."), Opt::flag(&["--json"], "Machine-readable JSON output.")];

use crate::rank::now;

fn code_of(code: i32) -> Res {
    if code == 0 {
        Ok(())
    } else {
        exit(code)
    }
}

/// `cli._rank_fit`: the journal stands in for what the file lacks.
fn fit_of(r: &Ranking, data_dir: &str) -> Object {
    let mut warnings = vec![];
    let mut comps = vec![];
    if r.comparisons.as_ref().map_or(true, |c| c.is_empty()) {
        let (c, bad) = rank::journal_comparisons(data_dir, &r.id);
        comps = c;
        if bad > 0 {
            warnings.push(format!("{bad} rows of the comparison journal did not read; skipped"));
        }
    }
    let mut owner = vec![];
    if r.owner.is_none() {
        let by_id: HashMap<String, &rank::Attempt> = r.attempts.iter().map(|a| (a.id.clone(), a)).collect();
        owner = rank::owner_answers(data_dir, &r.id, &by_id, &mut warnings);
    }
    let mut weights = None;
    if r.comparisons.as_ref().map_or(true, |c| c.is_empty()) {
        // The journal supplies the votes: each judge weighs by its record (W3K-2).
        let mut judges: std::collections::BTreeSet<String> = r.policy.judges.iter().cloned().collect();
        judges.extend(comps.iter().map(|c| c.judge.clone()));
        if !judges.is_empty() {
            let ow: &[rank::Pair] = match &r.owner {
                Some(o) => o,
                None => &owner,
            };
            let judges: Vec<String> = judges.into_iter().collect();
            weights = Some(rank::weights_for(data_dir, &judges, &rank::seed_of(r, &comps, ow)).0);
        }
    }
    rank::fit_w(r, comps, owner, warnings, weights.as_ref())
}

impl Env {
    pub(super) fn rank_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            self.out(RANK_HELP);
            return exit(2);
        }
        match args[0].as_str() {
            "--help" => {
                self.out(RANK_HELP);
                Ok(())
            }
            "fit" => self.rank_fit(&args[1..], false),
            "choose" => self.rank_fit(&args[1..], true),
            "show" => self.rank_show(&args[1..]),
            "judge" => self.rank_judge(&args[1..]),
            "judges" => self.rank_judges(&args[1..]),
            "pivot" => self.rank_pivot(&args[1..]),
            "queue" => self.rank_queue(&args[1..]),
            "record" => self.rank_record(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi rank [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi rank --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    fn rank_data_dir(&self, p: &super::Parsed) -> String {
        path_str(&p.str("--data-dir", &self.data_home()))
    }

    /// `cli._rank_read`.
    fn rank_read(&mut self, cmd: &str, path: &str) -> Result<Ranking, Res> {
        let raw = match std::fs::read(path) {
            Ok(r) => r,
            Err(e) => return Err(self.refuse(cmd, &format!("{path} cannot be read: {e}"))),
        };
        let Ok(text) = String::from_utf8(raw.clone()) else {
            let why = crate::warden::resident::utf8_error_text(&raw);
            return Err(Err(self.fail3(
                &format!("Tried to read the attempts in {path}."),
                &format!("It did not parse: {why}"),
                "Fix the file and run again.",
                2,
            )));
        };
        let doc = match loads_py(&text, 900) {
            Ok(v) => v,
            Err(m) if m == "unsupported" || m.starts_with("maximum recursion") || m.starts_with("Exceeds the limit") => {
                return Err(self.refuse(cmd, &format!("{path} holds JSON this binary does not read")))
            }
            Err(m) => {
                return Err(Err(self.fail3(
                    &format!("Tried to read the attempts in {path}."),
                    &format!("It did not parse: {}", m.split('\n').next().unwrap_or("")),
                    "Fix the file and run again.",
                    2,
                )))
            }
        };
        rank::parse(&doc).map_err(|why| {
            Err(self.fail3(&format!("Tried to rank the attempts in {path}."), &why, "Fix the file and run again.", 2))
        })
    }

    fn rank_fit(&mut self, args: &[String], choose: bool) -> Res {
        let (cmd, summary) = if choose {
            ("rank choose", "Rank the attempts, take the leader, and open a card when there was a choice.")
        } else {
            ("rank fit", "Rank the attempts: eliminate, then quality, then the judges' votes, then cost.")
        };
        let p = match parse_args(args, &OPTS, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(cmd, "ATTEMPTS_PATH", &m),
        };
        if p.help {
            return self.cmd_help(cmd, " ATTEMPTS_PATH", summary, &OPTS);
        }
        if p.args.is_empty() {
            return self.usage_args(cmd, "ATTEMPTS_PATH", "Missing argument 'ATTEMPTS_PATH'.");
        }
        let path = p.args[0].clone();
        if let Some(m) = Self::click_path("'attempts_path'", &path) {
            return self.usage_args(cmd, "ATTEMPTS_PATH", &m);
        }
        let r = match self.rank_read(cmd, &path) {
            Ok(r) => r,
            Err(stop) => return stop,
        };
        let data_dir = self.rank_data_dir(&p);
        let res = fit_of(&r, &data_dir);
        let code = rank::exit_code(res.value("status").as_str().unwrap_or(""));
        if !choose {
            if p.flag("--json") {
                self.out(&format!("{}\n", dumps_indent(&Value::Obj(res), 2, true)));
            } else {
                self.out(&rank::fit_text(&res));
            }
            return code_of(code);
        }
        let t = now();
        let mut rows = rank::sweep(&data_dir, t);
        let mut cid: Option<String> = None;
        let mut recorded = false;
        if matches!(res.value("order"), Value::List(l) if l.len() > 1) {
            let row = rank::opened_row(&r, &res, t, None, None);
            let id = row.value("choice_id").as_str().unwrap_or("").to_string();
            if !rank::read_cards(&data_dir).iter().any(|c| c.id() == id) {
                rows.push(row);
                recorded = true;
            }
            cid = Some(id);
        }
        if let Err(e) = rank::append_rows(&data_dir, &rows) {
            return self.fail(cmd, &e.to_string());
        }
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("ranking", res.clone()).set("choice_id", cid.clone()).set("recorded", recorded);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&rank::fit_text(&res));
            match &cid {
                None => self.out("No choice to record: fewer than two attempts survived.\n"),
                Some(c) if recorded => {
                    self.out(&format!("Chose {}; card {c} is open.\n", res.value("leader").as_str().unwrap_or("")))
                }
                Some(c) => self.out(&format!("Choice {c} is already recorded.\n")),
            }
        }
        code_of(code)
    }

    fn rank_queue(&mut self, args: &[String]) -> Res {
        const CMD: &str = "rank queue";
        let opts = [Opt::val(&["--sort"], "TEXT", "reversibility, impact, date or project."), OPTS[0].clone(), OPTS[1].clone()];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "List the open cards: what was chosen, why, and what switching would do.", &opts);
        }
        let by = p.str("--sort", "reversibility");
        if !rank::SORTS.contains(&by.as_str()) {
            self.errf("Error: --sort must be reversibility, impact, date or project.\n");
            return exit(2);
        }
        let (views, decayed) = rank::queue(&self.rank_data_dir(&p), now(), &by);
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(rank::queue_json(&views, decayed)), 2, true)));
        } else {
            self.out(&rank::queue_text(&views, decayed));
        }
        Ok(())
    }

    fn rank_record(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            self.out(RANK_RECORD_HELP);
            return exit(2);
        }
        let (cmd, summary, n) = match args[0].as_str() {
            "--help" => {
                self.out(RANK_RECORD_HELP);
                return Ok(());
            }
            "pick" => ("rank record pick", "Pick an attempt on a card: the chosen one confirms it, another overrides it.", 2),
            "drop" => ("rank record drop", "Drop a card: the chosen attempt is kept, recorded as confirmed.", 1),
            other => {
                self.errf(&format!(
                    "Usage: daisugi rank record [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi rank record --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                return exit(2);
            }
        };
        let names = if n == 2 { "CHOICE ATTEMPT" } else { "CHOICE" };
        let opts: Vec<Opt> = if n == 2 {
            vec![OPTS[0].clone(), OPTS[1].clone(), Opt::flag(&["--apply"], "Then pivot: change the work to the pick.")]
        } else {
            vec![OPTS[0].clone(), OPTS[1].clone()]
        };
        let p = match parse_args(&args[1..], &opts, n) {
            Ok(p) => p,
            Err(m) => return self.usage_args(cmd, names, &m),
        };
        if p.help {
            return self.cmd_help(cmd, &format!(" {names}"), summary, &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(cmd, names, "Missing argument 'CHOICE'.");
        }
        if n == 2 && p.args.len() < 2 {
            return self.usage_args(cmd, names, "Missing argument 'ATTEMPT'.");
        }
        let pick = if n == 2 { Some(p.args[1].clone()) } else { None };
        let data_dir = self.rank_data_dir(&p);
        self.rank_answer(cmd, &data_dir, &p.args[0].clone(), pick, p.flag("--json"), n == 2 && p.flag("--apply"))
    }

    /// `cli._rank_answer`.
    fn rank_answer(&mut self, cmd: &str, data_dir: &str, cid: &str, pick: Option<String>, json: bool, apply: bool) -> Res {
        let t = now();
        let rows = rank::sweep(data_dir, t);
        if let Err(e) = rank::append_rows(data_dir, &rows) {
            return self.fail(cmd, &e.to_string());
        }
        let cards = rank::read_cards(data_dir);
        let what = format!("Tried to record an answer on choice {cid}.");
        let Some(card) = cards.iter().find(|c| c.id() == cid) else {
            return Err(self.fail3(&what, &format!("There is no choice {cid}."), "Run daisugi rank queue to see the open cards.", 1));
        };
        if let Some(a) = &card.answer {
            let why = format!(
                "It is answered already: {} ({}).",
                a.value("event").as_str().unwrap_or(""),
                a.value("how").as_str().unwrap_or("")
            );
            return Err(self.fail3(&what, &why, "An answer stands; record a new choice to change the work.", 1));
        }
        let chosen = card.chosen();
        let rid = card.opened.value("ranking_id").clone();
        let surv: Vec<String> = card.survivors().into_iter().map(|(id, _)| id).collect();
        if let Some(pk) = &pick {
            if !surv.contains(pk) {
                let why = format!("{pk} is not an attempt that survived; the survivors are {}.", rank::join(&surv));
                return Err(self.fail3(&what, &why, "Pick one of them and run again.", 2));
            }
        }
        if pick.is_none() || pick.as_deref() == Some(chosen.as_str()) {
            let mut row = Object::new();
            row.set("choice_id", cid).set("ranking_id", rid).set("event", "confirmed");
            row.set("how", if pick.is_none() { "drop" } else { "answer" }).set("ts", t);
            if let Err(e) = rank::append_rows(data_dir, &[row]) {
                return self.fail(cmd, &e.to_string());
            }
            if json {
                let mut o = Object::new();
                o.set("choice_id", cid).set("event", "confirmed").set("chosen", chosen.as_str());
                self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            } else {
                self.out(&format!("Kept {chosen} on {cid}; recorded as confirmed.\n"));
            }
            return Ok(());
        }
        let pk = pick.unwrap_or_default();
        let sc = rank::switch_cost(card, data_dir, Some(&pk));
        let mut row = Object::new();
        row.set("choice_id", cid).set("ranking_id", rid).set("event", "overridden").set("pick", pk.as_str());
        row.set("how", "answer").set("ts", t);
        if let Err(e) = rank::append_rows(data_dir, &[row]) {
            return self.fail(cmd, &e.to_string());
        }
        let mut pivot: Option<Object> = None;
        if apply {
            let plan = match crate::rank_pivot::plan(card, data_dir, &pk) {
                Ok(p) => p,
                Err(why) => return self.fail(cmd, &why),
            };
            pivot = Some(crate::rank_pivot::apply(&plan, data_dir));
        }
        let applied = pivot.as_ref().map(|p| matches!(p.value("applied"), Value::Bool(true)));
        if json {
            let mut sw = Object::new();
            sw.set("cost", sc.cost).set("undo_steps", sc.undo_steps).set("text", sc.text.as_str());
            let mut o = Object::new();
            o.set("choice_id", cid).set("event", "overridden").set("chosen", chosen.as_str()).set("pick", pk.as_str());
            o.set("switch", sw);
            if let Some(pv) = &pivot {
                o.set("pivot", pv.clone());
            }
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&format!("Recorded on {cid}: switch from {chosen} to {pk}.\n"));
            self.out(&format!("What switching will do ({}): {}\n", sc.cost, sc.text));
            match (&pivot, applied) {
                (None, _) => self.out("The pick is recorded; the work is not changed yet.\n"),
                (Some(pv), Some(true)) => {
                    let n = match pv.value("undone") {
                        Value::List(l) => l.len(),
                        _ => 0,
                    };
                    self.out(&format!("The pick is recorded and applied: undid {n} steps.\n"))
                }
                (Some(pv), _) => {
                    let why = match pv.value("follow_up").as_str().filter(|f| !f.is_empty()) {
                        Some(f) => f.to_string(),
                        None => pv.value("why").as_str().unwrap_or("").to_string(),
                    };
                    self.out(&format!("The pick is recorded; the work is not changed: {why}\n"))
                }
            }
        }
        if applied == Some(false) {
            return exit(1);
        }
        Ok(())
    }

    fn rank_show(&mut self, args: &[String]) -> Res {
        const CMD: &str = "rank show";
        let opts = [
            Opt::val(&["--ranking"], "TEXT", "A ranking id."),
            Opt::val(&["--choice"], "TEXT", "A choice id."),
            OPTS[0].clone(),
            OPTS[1].clone(),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Show a ranking as the journal holds it: votes, choices, card, owner answers.", &opts);
        }
        let ranking = p.has("--ranking").then(|| p.str("--ranking", ""));
        let choice = p.has("--choice").then(|| p.str("--choice", ""));
        if ranking.is_some() == choice.is_some() {
            self.errf("Error: Give --ranking or --choice, not both.\n");
            return exit(2);
        }
        match rank::show_doc(&self.rank_data_dir(&p), ranking.as_deref(), choice.as_deref(), now()) {
            Err(why) => {
                self.out(&format!("{why}\n"));
                exit(1)
            }
            Ok(doc) => {
                if p.flag("--json") {
                    self.out(&format!("{}\n", dumps_indent(&Value::Obj(doc), 2, true)));
                } else {
                    self.out(&rank::show_text(&doc));
                }
                Ok(())
            }
        }
    }

    fn rank_judges(&mut self, args: &[String]) -> Res {
        const CMD: &str = "rank judges";
        let p = parse_args(args, &OPTS, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Show how far each judge agrees with the owner, and its current weight.", &OPTS);
        }
        let trust = rank::judge_trust(&self.rank_data_dir(&p), rank::TRUST_FLOOR, rank::TRUST_MIN_CARDS);
        let weights = rank::judge_weights(&trust, "");
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("judges", rank::trust_doc(&trust)).set("weights", rank::weights_obj(&weights));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&rank::trust_text(&trust, &weights));
        }
        Ok(())
    }

    fn rank_pivot(&mut self, args: &[String]) -> Res {
        const CMD: &str = "rank pivot";
        let opts = [
            Opt::val(&["--choice"], "TEXT", "The card's choice id. [required]"),
            Opt::val(&["--to"], "TEXT", "The attempt to switch to. [required]"),
            Opt::flag(&["--apply"], "Change the work. Only the owner."),
            OPTS[0].clone(),
            OPTS[1].clone(),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Show what switching a card to another attempt would do; with --apply, do it.", &opts);
        }
        for name in ["--choice", "--to"] {
            if !p.has(name) {
                return self.usage_args(CMD, "", &format!("Missing option '{name}'."));
            }
        }
        let data_dir = self.rank_data_dir(&p);
        let (choice, to) = (p.str("--choice", ""), p.str("--to", ""));
        let cards = rank::read_cards(&data_dir);
        let Some(card) = cards.iter().find(|c| c.id() == choice) else {
            self.out(&format!("There is no choice {choice}.\n"));
            return exit(1);
        };
        let plan = match crate::rank_pivot::plan(card, &data_dir, &to) {
            Ok(pl) => pl,
            Err(why) => {
                self.errf(&format!("Error: {why}\n"));
                return exit(2);
            }
        };
        let result = p.flag("--apply").then(|| crate::rank_pivot::apply(&plan, &data_dir));
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("plan", plan.clone()).set("result", result.clone().map(Value::Obj).unwrap_or(Value::Null));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&crate::rank_pivot::plan_text(&plan));
            if let Some(r) = &result {
                if matches!(r.value("applied"), Value::Bool(true)) {
                    self.out("Applied.\n");
                } else {
                    let why = r.value("why").as_str().unwrap_or("").to_string();
                    self.out(&format!("Not applied: {why}\n"));
                }
            }
        }
        if let Some(r) = &result {
            if !matches!(r.value("applied"), Value::Bool(true)) {
                return exit(1);
            }
        }
        Ok(())
    }

    fn rank_judge(&mut self, args: &[String]) -> Res {
        const CMD: &str = "rank judge";
        let opts = [
            Opt::val(&["--judge"], "TEXT", "PROFILE/MODEL to ask."),
            Opt::flag(&["--loop"], "Repeat until the judges are exhausted."),
            OPTS[0].clone(),
            OPTS[1].clone(),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ATTEMPTS_PATH", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " ATTEMPTS_PATH",
                "Ask a judge model to compare the next pair, in both orders, and record the vote.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "ATTEMPTS_PATH", "Missing argument 'ATTEMPTS_PATH'.");
        }
        let path = p.args[0].clone();
        if let Some(m) = Self::click_path("'attempts_path'", &path) {
            return self.usage_args(CMD, "ATTEMPTS_PATH", &m);
        }
        let r = match self.rank_read(CMD, &path) {
            Ok(r) => r,
            Err(stop) => return stop,
        };
        let doc = std::fs::read(&path)
            .ok()
            .and_then(|b| String::from_utf8(b).ok())
            .and_then(|t| loads_py(&t, 900).ok())
            .unwrap_or(Value::Null);
        let judge = p.has("--judge").then(|| p.str("--judge", ""));
        if judge.is_none() && r.policy.judges.is_empty() {
            self.errf("policy.judges names no judge.\n");
            return exit(3);
        }
        let data_dir = self.rank_data_dir(&p);
        let table = crate::agentprof::configured_table(&self.data_home());
        let profiles = crate::agentprof::resolve(table.as_ref(), &|k| self.env.get(k).cloned());
        let client = self.llm_client();
        let env = self.env.clone();
        let call = move |model: &str, base: &str, key: &str, msgs: &[(String, String)]| -> Result<String, (String, String)> {
            let get = |k: &str| env.get(k).cloned();
            let t = crate::llm::wire::timeout(&get);
            match client.complete_with(model, base, key, msgs, &crate::llm::wire::Opts::default(), t) {
                Ok(reply) => Ok(reply.text),
                Err(crate::llm::CallError::Model(m)) => Err(("ModelCallError".into(), m)),
                Err(crate::llm::CallError::Os(e)) => Err(("OSError".into(), e.to_string())),
                Err(crate::llm::CallError::Unported(m)) => Err(("Error".into(), m)),
            }
        };
        let env2 = self.env.clone();
        let getenv = move |k: &str| env2.get(k).cloned();
        let mut recorded = 0i64;
        let mut last;
        loop {
            last = match crate::rank_judge::judge_once(
                &|| crate::rank_judge::parse_doc(&doc),
                &data_dir,
                &profiles,
                &getenv,
                judge.as_deref(),
                now(),
                &call,
            ) {
                Ok(l) => l,
                Err(why) => return self.fail(CMD, &why),
            };
            if !last.recorded {
                break;
            }
            recorded += 1;
            let next_none = last.fit.as_ref().map(|f| f.value("next").is_null()).unwrap_or(true);
            if !p.flag("--loop") || next_none {
                break;
            }
        }
        let stop: Value = if !last.recorded {
            last.reason.as_str().into()
        } else {
            last.fit.as_ref().map(|f| f.value("stop").clone()).unwrap_or(Value::Null)
        };
        if p.flag("--json") {
            let strs = |v: &Option<Vec<String>>| match v {
                Some(l) => Value::List(l.iter().map(|s| Value::Str(s.clone())).collect()),
                None => Value::Null,
            };
            let mut lastv = Object::new();
            lastv.set("pair", strs(&last.pair)).set("judge", last.judge.clone()).set("outcomes", strs(&last.outcomes));
            let mut o = Object::new();
            o.set("recorded", recorded).set("stop", stop).set("why", last.why.as_str()).set("last", lastv);
            o.set("fit", last.fit.clone().map(Value::Obj).unwrap_or(Value::Null));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&format!("Recorded {recorded} comparison(s). {}\n", last.why));
        }
        if recorded > 0 {
            return Ok(());
        }
        exit(if last.reason == "failed" { 1 } else { 3 })
    }
}
