//! `daisugi rank fit|choose|queue|record`: `rank.py` through its commands.
//! The Go client's `cli/rankcmd.go` is the reference.

use std::collections::HashMap;

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::pyjson::{dumps_indent, loads_py, Object, Value};
use crate::rank::{self, Ranking};

const RANK_HELP: &str = "Usage: daisugi rank [OPTIONS] COMMAND [ARGS]...

  Rank N attempts at one task, record the choice, and review it later.

Options:
  --help  Show this message and exit.

Commands:
  fit     Rank the attempts: eliminate, then quality, then the judges'...
  choose  Rank the attempts, take the leader, and open a card when there...
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
    if r.comparisons.is_none() {
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
    rank::fit(r, comps, owner, warnings)
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
            let why = crate::gate::resident::utf8_error_text(&raw);
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
            let row = rank::opened_row(&r, &res, t, None);
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
        let p = match parse_args(&args[1..], &OPTS, n) {
            Ok(p) => p,
            Err(m) => return self.usage_args(cmd, names, &m),
        };
        if p.help {
            return self.cmd_help(cmd, &format!(" {names}"), summary, &OPTS);
        }
        if p.args.is_empty() {
            return self.usage_args(cmd, names, "Missing argument 'CHOICE'.");
        }
        if n == 2 && p.args.len() < 2 {
            return self.usage_args(cmd, names, "Missing argument 'ATTEMPT'.");
        }
        let pick = if n == 2 { Some(p.args[1].clone()) } else { None };
        let data_dir = self.rank_data_dir(&p);
        self.rank_answer(cmd, &data_dir, &p.args[0].clone(), pick, p.flag("--json"))
    }

    /// `cli._rank_answer`.
    fn rank_answer(&mut self, cmd: &str, data_dir: &str, cid: &str, pick: Option<String>, json: bool) -> Res {
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
        if json {
            let mut sw = Object::new();
            sw.set("cost", sc.cost).set("undo_steps", sc.undo_steps).set("text", sc.text.as_str());
            let mut o = Object::new();
            o.set("choice_id", cid).set("event", "overridden").set("chosen", chosen.as_str()).set("pick", pk.as_str());
            o.set("switch", sw);
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
        } else {
            self.out(&format!("Recorded on {cid}: switch from {chosen} to {pk}.\n"));
            self.out(&format!("What switching will do ({}): {}\n", sc.cost, sc.text));
            self.out("The pick is recorded; the work is not changed yet.\n");
        }
        Ok(())
    }
}
