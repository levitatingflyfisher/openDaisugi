//! `daisugi hook list` and `daisugi hook to-trace`: the captured sessions
//! listed, and one session made a journal trace, with the flags, output,
//! exit codes and files of the Python CLI. The Go client's `hookcapcmd.go`
//! is the reference.

use super::autotendcmd::convert_capture;
use super::config;
use super::gardencmd::now_seconds;
use super::gatecmds::DECOMPOSE_OPT;
use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Parsed, Res};
use crate::capture::{list_sessions, stem, CapErr};
use crate::distill::token_hex;
use crate::gate::pyjson::{dumps, py_str, Object, Value};
use crate::shell_decompose::ShellParser;
use crate::tracejournal::{iso_seconds, Journal};

const CAPTURES_ROOT: Opt = Opt::val(&["--captures-root"], "PATH", "");

/// `format(s, "<n")` on a str: padded with spaces to n code points.
fn ljust(s: &str, n: usize) -> String {
    let k = s.chars().count();
    if k < n {
        format!("{s}{}", " ".repeat(n - k))
    } else {
        s.to_string()
    }
}

impl Env {
    /// The --captures-root value as pathlib prints it.
    fn captures_root(&self, p: &Parsed) -> String {
        if p.has("--captures-root") {
            return path_str(&p.str("--captures-root", ""));
        }
        join(&self.data_home(), "captures")
    }

    /// An exception Python does not catch ends the command: the class
    /// name and its text, exit 1.
    fn py_raise(&mut self, cmd: &str, typ: &str, msg: &str) -> Res {
        self.errf(&format!("daisugi {cmd}: {typ}: {msg}\n"));
        exit(1)
    }

    /// `daisugi hook list`: the captured sessions with their call counts,
    /// newest first.
    pub(super) fn hook_list(&mut self, args: &[String]) -> Res {
        const CMD: &str = "hook list";
        let opts = [CAPTURES_ROOT, Opt::flag(&["--json"], "Machine-readable JSON output.")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "List captured sessions with call counts.", &opts);
        }
        let sessions = match list_sessions(&self.captures_root(&p)) {
            Ok(s) => s,
            Err(CapErr::Raised(typ, msg)) => return self.py_raise(CMD, &typ, &msg),
            Err(e) => return Err(self.cap_err(CMD, e)),
        };
        if p.flag("--json") {
            let rows: Vec<Value> = sessions
                .iter()
                .map(|s| {
                    Value::Obj(
                        Object::new()
                            .with("session_id", s.id.as_str())
                            .with("calls", Value::Int(s.calls.to_string()))
                            .with("first_at", s.first_at.clone())
                            .with("last_at", s.last_at.clone()),
                    )
                })
                .collect();
            self.out(&format!("{}\n", dumps(&Value::List(rows), true)));
            return Ok(());
        }
        if sessions.is_empty() {
            self.out("(no captured sessions)\n");
            return Ok(());
        }
        let mut b = format!("{}  {:>6}  last_at\n", ljust("session_id", 40), "calls");
        for s in &sessions {
            let Ok(at) = py_str(&s.last_at) else {
                return self.refuse(CMD, "a captured_at whose str() this binary does not write");
            };
            b.push_str(&format!("{}  {:>6}  {at}\n", ljust(&s.id, 40), s.calls));
        }
        self.out(&b);
        Ok(())
    }

    /// `daisugi hook to-trace`: one captured session made a journal trace,
    /// verified against the envelope inferred from what it did.
    pub(super) fn hook_to_trace(&mut self, args: &[String]) -> Res {
        const CMD: &str = "hook to-trace";
        let opts = [
            CAPTURES_ROOT,
            Opt::val(&["--data-dir"], "PATH", ""),
            Opt::val(&["--task"], "TEXT", "Override the task description; default uses the session id."),
            DECOMPOSE_OPT,
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " SESSION_ID", "Convert a captured session into a journal trace.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "SESSION_ID", "SESSION_ID");
        }
        let id = p.args[0].clone();
        let name = format!("{id}.jsonl");
        let path =
            if name.starts_with('/') { path_str(&name) } else { join(&self.captures_root(&p), &path_str(&name)) };
        if std::fs::metadata(&path).is_err() {
            self.errf(&format!("error: no capture at {path}\n"));
            return exit(1);
        }
        let data_dir = self.data_dir_of(&p);
        let mut decompose = p.flag("--allow-shell-decomposition");
        if !p.flag_set("--allow-shell-decomposition") {
            match config::load(&join(&data_dir, "config.yaml")) {
                Ok(c) => decompose = c.shell_allow_decomposition,
                Err(e) => return Err(self.config_load_err(CMD, &config::RowsErr::Config(e))),
            }
        }
        let mut task = p.str("--task", "");
        if task.is_empty() {
            task = format!("captured session {}", stem(path.rsplit('/').next().unwrap_or(&path)));
        }
        // Everything is read and checked before the journal is made, so a
        // capture this binary cannot convert changes nothing. Python makes
        // the journal before it reads the capture, so an exception it raises
        // leaves the journal made.
        let mut parser = ShellParser::new();
        let (conv, raised) = match convert_capture(&mut parser, &path, &id, &task, decompose) {
            Ok(c) => (Some(c), None),
            Err(CapErr::Raised(typ, msg)) => (None, Some((typ, msg))),
            Err(CapErr::InvalidPlan(text)) => {
                (None, Some(("pydantic_core._pydantic_core.ValidationError".into(), text)))
            }
            Err(e) => return Err(self.cap_err(CMD, e)),
        };
        let hex = token_hex().map_err(|e| self.pw_err(CMD, e))?;
        let j = Journal::open(&data_dir).map_err(|e| self.pw_err(CMD, e))?;
        let Some(c) = conv else {
            let (typ, msg) = raised.unwrap_or_default();
            return self.py_raise(CMD, &typ, &msg);
        };
        let created = format!("{}Z", iso_seconds(now_seconds() as i64));
        let trace_id = format!("{}-{hex}", &created[..10]);
        j.log(&c.task, &c.env, &c.plan, &c.result, &trace_id, &created).map_err(|e| self.pw_err(CMD, e))?;
        j.mark_converted(&id, &trace_id, now_seconds()).map_err(|e| self.pw_err(CMD, e))?;
        self.out(&format!("{trace_id}\n"));
        Ok(())
    }
}
