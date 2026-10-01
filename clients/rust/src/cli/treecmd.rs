//! `daisugi tree check|root|spawn|end|answer|status`: `tree.py` through its
//! commands. The Go client's `cli/treecmd.go` is the reference.

use num_bigint::BigInt;

use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Parsed, Res, Stop};
use crate::gate::pyjson::{dumps_indent, loads_py, Object, Value};
use crate::pathways::pmodel::{validate_model, Id, Mode, ValidationError};
use crate::tree::{self, Fail};

const TREE_HELP: &str = "Usage: daisugi tree [OPTIONS] COMMAND [ARGS]...

  The delegation tree: prove each edge, keep the budgets, ask the operator.

Options:
  --help  Show this message and exit.

Commands:
  check   Prove that the child's envelope fits inside the parent's.
  root    Make a root of the tree: the operator's envelope and budget.
  spawn   Prove the edge, reserve the child's budget and register its...
  run     Start a child of the tree on sprig, wait for it, and end it:...
  end     Record that a node ended; its parent gets the unspent budget...
  answer  Answer an ask: allow starts the child as proposed, marked not...
  status  Show the tree: each node's state, budgets and deadline, and...
";

/// `tree_run.read_request`: the request, or a line that says what is
/// wrong.
fn tree_run_request(data: &[u8]) -> Result<Object, String> {
    let Ok(text) = std::str::from_utf8(data) else {
        return Err("It could not be read as UTF-8 text.".into());
    };
    let doc = match loads_py(text, 900) {
        Ok(v) => v,
        Err(m) => return Err(format!("It did not parse: {}", m.split('\n').next().unwrap_or(""))),
    };
    let Value::Obj(doc) = doc else {
        return Err("It is not a JSON object.".into());
    };
    if doc.value("task").as_str().is_none_or(|t| crate::gate::py::text::strip(t).is_empty()) {
        return Err("task must be text.".into());
    }
    if doc.value("envelope").as_obj().is_none() {
        return Err("envelope must be an object.".into());
    }
    match doc.value("tools") {
        Value::List(l) if l.iter().all(|t| t.as_str().is_some()) => {}
        _ => return Err("tools must be a list of text.".into()),
    }
    match doc.value("max_turns") {
        Value::Null => {}
        Value::Int(t) if t.parse::<BigInt>().is_ok_and(|n| n >= BigInt::from(1)) => {}
        _ => return Err("max_turns must be a whole number of 1 or more.".into()),
    }
    Ok(doc)
}

const DATA_OPT: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");
const ROOT_OPT: Opt = Opt::val(&["--root"], "PATH", "Gate state directory (default: the data directory's gate).");
const Z3_OPT: Opt = Opt::val(&["--z3-timeout-ms"], "INTEGER", "Z3 budget for each proof, in ms.");
const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

/// `cli._envelope_why`: each error's place and message.
pub(super) fn envelope_why(e: &ValidationError) -> String {
    e.errs
        .iter()
        .map(|er| if er.loc.is_empty() { er.msg.clone() } else { format!("{}: {}", er.loc.join("."), er.msg) })
        .collect::<Vec<_>>()
        .join("; ")
}

impl Env {
    pub(super) fn tree_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            self.out(TREE_HELP);
            return exit(2);
        }
        match args[0].as_str() {
            "--help" => {
                self.out(TREE_HELP);
                Ok(())
            }
            "check" => self.tree_check(&args[1..]),
            "root" => self.tree_root(&args[1..]),
            "spawn" => self.tree_spawn(&args[1..]),
            "run" => self.tree_run(&args[1..]),
            "end" => self.tree_end(&args[1..]),
            "answer" => self.tree_answer(&args[1..]),
            "status" => self.tree_status(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi tree [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi tree --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    fn tree_dirs(&self, p: &Parsed) -> (String, String) {
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let gate_root = path_str(&p.str("--root", &join(&data_dir, "gate")));
        (data_dir, gate_root)
    }

    /// `cli._tree_envelope`: an envelope file (JSON); a bad one exits 2.
    fn tree_envelope(&mut self, cmd: &str, path: &str) -> Result<Object, Stop> {
        let what = format!("Tried to read the envelope in {path}.");
        let fix = "Fix the file and run again.";
        if !std::fs::metadata(path).is_ok_and(|m| m.is_file()) {
            return Err(self.fail3(&what, "There is no such file.", "Fix the path and run again.", 2));
        }
        let Ok(raw) = std::fs::read(path) else {
            return Err(self.fail3(&what, "It could not be read as UTF-8 text.", fix, 2));
        };
        let Ok(text) = String::from_utf8(raw) else {
            return Err(self.fail3(&what, "It could not be read as UTF-8 text.", fix, 2));
        };
        let doc = match loads_py(&text, 900) {
            Ok(v) => v,
            Err(m) if m == "unsupported" || m.starts_with("maximum recursion") || m.starts_with("Exceeds the limit") => {
                return Err(self.refuse(cmd, &format!("{path} holds JSON this binary does not read")).unwrap_err())
            }
            Err(m) => {
                return Err(self.fail3(&what, &format!("It did not parse: {}", m.split('\n').next().unwrap_or("")), fix, 2))
            }
        };
        if doc.as_obj().is_none() {
            return Err(self.fail3(&what, "It is not a JSON object.", fix, 2));
        }
        match validate_model(Id::Envelope, &doc, Mode::Python) {
            Ok(Value::Obj(o)) => Ok(o),
            Ok(_) => Err(self.fail3(&what, "It is not a JSON object.", fix, 2)),
            Err(e) => Err(self.fail3(&what, &format!("It is not a valid envelope: {}", envelope_why(&e)), fix, 2)),
        }
    }

    fn tree_fail(&mut self, cmd: &str, f: Fail) -> Res {
        match f {
            Fail::Tree(t) => Err(self.fail3(&t.what, &t.why, &t.fix, t.code)),
            Fail::Io(e) => self.fail(cmd, &e),
        }
    }

    /// An int option, or None when absent; click's usage error otherwise.
    fn tree_count(&mut self, cmd: &str, args: &str, p: &Parsed, name: &str) -> Result<Option<BigInt>, Stop> {
        if !p.has(name) {
            return Ok(None);
        }
        let v = p.str(name, "");
        match super::pathwayscmd::py_int(&v) {
            Some(n) => Ok(Some(BigInt::from(n))),
            None => {
                let _ = self.usage_args(
                    cmd,
                    args,
                    &format!("Invalid value for {name}: {} is not a valid integer.", crate::gate::py::text::repr(&v)),
                );
                Err(Stop::Exit(2))
            }
        }
    }

    fn tree_timeout(&mut self, cmd: &str, args: &str, p: &Parsed) -> Result<u32, Stop> {
        Ok(match self.tree_count(cmd, args, p, "--z3-timeout-ms")? {
            None => tree::DEFAULT_TIMEOUT_MS,
            Some(n) => u32::try_from(n).unwrap_or(u32::MAX),
        })
    }

    /// `daisugi tree run`: the one starter. stdin is {task, envelope,
    /// tools, max_turns}; it spawns the child, starts sprig with the
    /// reservation, waits, ends it, and prints {status, session, answer,
    /// tokens_used, turns_used, reasons, ask_id}.
    fn tree_run(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree run";
        const ARGS: &str = "[-]";
        let opts = [
            Opt::val(&["--parent"], "TEXT", "The parent's session id."),
            Opt::val(&["--session"], "TEXT", "The child's session id (default: one minted from the parent)."),
            Opt::val(&["--profile"], "TEXT", "The agent profile the child runs on."),
            Opt::val(&["--model"], "TEXT", "The model id (free text)."),
            Opt::val(&["--tokens"], "INTEGER", "Tokens to reserve for the child."),
            Opt::val(&["--turns"], "INTEGER", "Turns to reserve for the child."),
            Opt::val(&["--parent-tokens-used"], "INTEGER", "The tokens the parent used itself so far."),
            Opt::val(&["--parent-turns-used"], "INTEGER", "The turns the parent used itself so far."),
            Opt::val(&["--keys-fd"], "INTEGER", "Read the resolved agent profiles, with keys, from this descriptor."),
            Z3_OPT,
            DATA_OPT,
            ROOT_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, ARGS, &m),
        };
        if p.help {
            return self.cmd_help(CMD, " [-]", "Start a child of the tree on sprig, wait for it, and end it: the one starter.", &opts);
        }
        for need in ["--parent", "--profile"] {
            if !p.has(need) {
                return self.usage_args(CMD, ARGS, &format!("Missing option '{need}'."));
            }
        }
        let mut counts = std::collections::HashMap::new();
        for name in ["--tokens", "--turns", "--parent-tokens-used", "--parent-turns-used", "--keys-fd"] {
            let n = self.tree_count(CMD, ARGS, &p, name)?;
            counts.insert(name, n);
        }
        let timeout = self.tree_timeout(CMD, ARGS, &p)?;
        let what = "Tried to read the tree run request on stdin.";
        let fix = "Fix the request and run again.";
        if p.args.first().is_some_and(|a| a != "-") {
            return Err(self.fail3(what, "The only argument is -, for stdin.", fix, 2));
        }
        let mut data = vec![];
        let _ = std::io::Read::read_to_end(&mut std::io::stdin(), &mut data);
        let req = match tree_run_request(&data) {
            Ok(r) => r,
            Err(why) => return Err(self.fail3(what, &why, fix, 2)),
        };
        let envelope = match validate_model(Id::Envelope, req.value("envelope"), Mode::Python) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => return Err(self.fail3(what, "envelope must be an object.", fix, 2)),
            Err(e) => return Err(self.fail3(what, &format!("envelope is not a valid envelope: {}", envelope_why(&e)), fix, 2)),
        };
        let mut bundle = vec![];
        if let Some(fd) = counts["--keys-fd"].clone() {
            let read = i32::try_from(fd.clone()).ok().filter(|n| *n >= 0 && unsafe { libc::fcntl(*n, libc::F_GETFD) } >= 0);
            let Some(n) = read else {
                return Err(self.fail3(
                    "Tried to read the agent profiles.",
                    &format!("Descriptor {fd} cannot be read."),
                    "Pass the descriptor the profiles are on.",
                    2,
                ));
            };
            use std::os::fd::FromRawFd;
            let mut f = unsafe { std::fs::File::from_raw_fd(n) };
            if std::io::Read::read_to_end(&mut f, &mut bundle).is_err() {
                return Err(self.fail3(
                    "Tried to read the agent profiles.",
                    &format!("Descriptor {fd} cannot be read."),
                    "Pass the descriptor the profiles are on.",
                    2,
                ));
            }
        }
        let profiles = crate::agentprof::from_bundle(&bundle);
        let (data_dir, gate_root) = self.tree_dirs(&p);
        let me = match self.self_path() {
            Ok(m) => m,
            Err(e) => return self.fail(CMD, &e),
        };
        let cwd = match std::env::current_dir() {
            Ok(d) => d.to_string_lossy().to_string(),
            Err(e) => return self.fail(CMD, &e.to_string()),
        };
        let starter = crate::supervise::treerun::Starter {
            self_path: me,
            temp_dir: super::weavecmd::gettempdir(&self.env),
            sprig: self.env.get("DAISUGI_SPRIG").cloned().unwrap_or_else(|| "sprig".into()),
            env: self.env.clone(),
        };
        let tools: Vec<String> = match req.value("tools") {
            Value::List(l) => l.iter().filter_map(|t| t.as_str().map(String::from)).collect(),
            _ => vec![],
        };
        let max_turns = match req.value("max_turns") {
            Value::Int(t) => t.parse().ok(),
            _ => None,
        };
        let timeout_s = envelope
            .value("permissions")
            .as_obj()
            .and_then(|p| match p.value("max_execution_time_s") {
                Value::Int(t) => t.parse::<u64>().ok(),
                _ => None,
            })
            .unwrap_or(30);
        let opts = crate::supervise::treerun::NodeOpts {
            parent: p.str("--parent", ""),
            session: if p.has("--session") { Some(p.str("--session", "")) } else { None },
            envelope,
            profiles,
            profile_name: Some(p.str("--profile", "")),
            model: p.str("--model", ""),
            tokens: counts["--tokens"].clone(),
            turns: counts["--turns"].clone(),
            parent_tokens_used: counts["--parent-tokens-used"].clone(),
            parent_turns_used: counts["--parent-turns-used"].clone(),
            task: req.value("task").as_str().unwrap_or("").to_string(),
            tools,
            tools_repr: crate::gate::pyjson::py_repr(req.value("tools")).unwrap_or_default(),
            max_turns,
            cwd,
            timeout_s,
            default_model: "haiku".into(),
            timeout_ms: timeout,
        };
        let node = match starter.run_node(&data_dir, &gate_root, opts) {
            Ok(n) => n,
            Err(f) => return self.tree_fail(CMD, f),
        };
        self.out(&format!("{}\n", crate::gate::pyjson::dumps(&Value::Obj(node.doc()), true)));
        match node.status {
            "started" => Ok(()),
            "asked" => exit(3),
            _ => exit(1),
        }
    }

    fn tree_check(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree check";
        let opts = [Z3_OPT, JSON_OPT];
        let p = match parse_args(args, &opts, 2) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "PARENT CHILD", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " PARENT CHILD", "Prove that the child's envelope fits inside the parent's.", &opts);
        }
        if p.args.len() < 2 {
            let missing = if p.args.len() == 1 { "CHILD" } else { "PARENT" };
            return self.usage_args(CMD, "PARENT CHILD", &format!("Missing argument '{missing}'."));
        }
        let timeout = self.tree_timeout(CMD, "PARENT CHILD", &p)?;
        let parent = self.tree_envelope(CMD, &p.args[0])?;
        let child = self.tree_envelope(CMD, &p.args[1])?;
        let res = tree::edge_ok(Some(&parent), Some(&child), timeout);
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(res.doc()), 2, true)));
        } else {
            self.out(&res.text());
        }
        if res.holds {
            Ok(())
        } else {
            exit(1)
        }
    }

    fn tree_root(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree root";
        let opts = [
            Opt::val(&["--session"], "TEXT", "The root's session id."),
            Opt::val(&["--tokens"], "INTEGER", "The root's token budget."),
            Opt::val(&["--turns"], "INTEGER", "The root's turn budget."),
            DATA_OPT,
            ROOT_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ENVELOPE", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " ENVELOPE", "Make a root of the tree: the operator's envelope and budget. Only the operator runs this.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "ENVELOPE", "Missing argument 'ENVELOPE'.");
        }
        if !p.has("--session") {
            return self.usage_args(CMD, "ENVELOPE", "Missing option '--session'.");
        }
        let tokens = self.tree_count(CMD, "ENVELOPE", &p, "--tokens")?;
        let turns = self.tree_count(CMD, "ENVELOPE", &p, "--turns")?;
        let env = self.tree_envelope(CMD, &p.args[0])?;
        let (data_dir, gate_root) = self.tree_dirs(&p);
        let session = p.str("--session", "");
        match tree::make_root(&data_dir, &gate_root, &env, &session, tokens, turns) {
            Ok(path) => {
                self.out(&format!("root {session} registered → {path}\n"));
                Ok(())
            }
            Err(f) => self.tree_fail(CMD, f),
        }
    }

    fn tree_spawn(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree spawn";
        let opts = [
            Opt::val(&["--parent"], "TEXT", "The parent's session id."),
            Opt::val(&["--session"], "TEXT", "The child's session id."),
            Opt::val(&["--tokens"], "INTEGER", "Tokens to reserve for the child."),
            Opt::val(&["--turns"], "INTEGER", "Turns to reserve for the child."),
            Z3_OPT,
            DATA_OPT,
            ROOT_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ENVELOPE", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " ENVELOPE", "Prove the edge, reserve the child's budget and register its envelope.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "ENVELOPE", "Missing argument 'ENVELOPE'.");
        }
        for need in ["--parent", "--session"] {
            if !p.has(need) {
                return self.usage_args(CMD, "ENVELOPE", &format!("Missing option '{need}'."));
            }
        }
        let tokens = self.tree_count(CMD, "ENVELOPE", &p, "--tokens")?;
        let turns = self.tree_count(CMD, "ENVELOPE", &p, "--turns")?;
        let timeout = self.tree_timeout(CMD, "ENVELOPE", &p)?;
        let env = self.tree_envelope(CMD, &p.args[0])?;
        let (data_dir, gate_root) = self.tree_dirs(&p);
        let parent = p.str("--parent", "");
        let session = p.str("--session", "");
        let res = match tree::spawn(&data_dir, &gate_root, &env, &parent, &session, tokens, turns, timeout) {
            Ok(r) => r,
            Err(f) => return self.tree_fail(CMD, f),
        };
        if res.status == "started" {
            self.out(&format!("started {session} under {parent} → {}\n", res.path));
            if let Some(d) = &res.inherited {
                self.out(&format!("{session} takes the deadline of {parent}: {}.\n", tree::q(d)));
            }
            return Ok(());
        }
        self.errf(&format!("not started: the edge from {parent} to {session} is refused: {}\n", res.reasons.join("; ")));
        if res.status == "asked" {
            self.errf(&format!(
                "{parent} has had {} proposals refused, so this one goes to the operator as ask {}.\n",
                tree::ASK_AFTER,
                res.ask_id
            ));
            self.errf(&format!("The operator answers it with: daisugi tree answer {} allow|deny\n", res.ask_id));
            return exit(3);
        }
        exit(1)
    }

    fn tree_end(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree end";
        let opts = [
            Opt::val(&["--tokens-used"], "INTEGER", "Tokens it used."),
            Opt::val(&["--turns-used"], "INTEGER", "Turns it used."),
            DATA_OPT,
            ROOT_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "SESSION", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " SESSION",
                "Record that a node ended; its parent gets the unspent budget back and its envelope is unregistered.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "SESSION", "Missing argument 'SESSION'.");
        }
        let tokens = self.tree_count(CMD, "SESSION", &p, "--tokens-used")?;
        let turns = self.tree_count(CMD, "SESSION", &p, "--turns-used")?;
        let (data_dir, gate_root) = self.tree_dirs(&p);
        let session = p.args[0].clone();
        let node = match tree::end(&data_dir, &gate_root, &session, tokens, turns) {
            Ok(n) => n,
            Err(f) => return self.tree_fail(CMD, f),
        };
        self.out(&format!("ended {session}; its envelope is unregistered.\n"));
        if let Some(parent) = &node.parent {
            for (axis, reserved, used) in [("tokens", &node.tokens, &node.tokens_used), ("turns", &node.turns, &node.turns_used)] {
                if let (Some(r), Some(u)) = (reserved, used) {
                    let back = if u > r { BigInt::default() } else { r - u };
                    self.out(&format!("{axis}: used {u} of {r}; {back} go back to {parent}.\n"));
                }
            }
        }
        Ok(())
    }

    fn tree_answer(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree answer";
        let opts = [DATA_OPT, ROOT_OPT];
        let p = match parse_args(args, &opts, 2) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ASK_ID VERDICT", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " ASK_ID VERDICT",
                "Answer an ask: allow starts the child as proposed, marked not proved. Only the operator runs this.",
                &opts,
            );
        }
        if p.args.len() < 2 {
            let missing = if p.args.len() == 1 { "VERDICT" } else { "ASK_ID" };
            return self.usage_args(CMD, "ASK_ID VERDICT", &format!("Missing argument '{missing}'."));
        }
        let (data_dir, gate_root) = self.tree_dirs(&p);
        let id = p.args[0].clone();
        let (ask, path) = match tree::answer(&data_dir, &gate_root, &id, &p.args[1]) {
            Ok(r) => r,
            Err(f) => return self.tree_fail(CMD, f),
        };
        let parent = ask.value("parent").as_str().unwrap_or("").to_string();
        match path {
            None => self.out(&format!("denied {id}; {parent} may propose again.\n")),
            Some(path) => self.out(&format!(
                "allowed {id}: started {} under {parent}, not proved → {path}\n",
                ask.value("session").as_str().unwrap_or("")
            )),
        }
        Ok(())
    }

    fn tree_status(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tree status";
        let opts = [DATA_OPT, JSON_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, "", "Show the tree: each node's state, budgets and deadline, and the open asks. Reads only.", &opts);
        }
        let (data_dir, _) = self.tree_dirs(&p);
        let doc = tree::status_doc(&data_dir);
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(doc), 2, true)));
        } else {
            self.out(&tree::status_text(&doc));
        }
        Ok(())
    }
}
