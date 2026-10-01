//! `daisugi gate replay CAPTURES --envelope ENV [--json]`: a captured
//! session through the gate, offline, in audit terms
//! (`gate.replay_captures`). Nothing runs and nothing is written. The Go
//! client's `cli/replaycmd.go` is the twin.

use super::journal::{from_records, JournalErr};
use super::runcmd::load_yaml;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{canonical_json, py_str, py_type_name, Object, Value};
use crate::gate::replay::{replay, ReplayErr};
use crate::pathways::pmodel::{validate_model, Id, Mode};

const CMD: &str = "gate replay";
const ARG: &str = "CAPTURES_JSONL";

/// How the envelope file read when it gave no envelope.
enum EnvErr {
    /// The last line of the oracle's traceback.
    Crash(String),
    Refused(String),
}

/// `Envelope(**yaml.safe_load(path.read_text()))` as the replay command
/// reads it.
fn replay_envelope(path: &str) -> Result<Object, EnvErr> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(EnvErr::Crash(format!(
                "FileNotFoundError: [Errno 2] No such file or directory: {}",
                repr(path)
            )))
        }
        Err(e) => return Err(EnvErr::Refused(format!("{path} cannot be read: {e}"))),
    };
    let text = String::from_utf8(raw).map_err(|_| EnvErr::Refused(format!("{path} is not UTF-8")))?;
    let v = match load_yaml(path, &text) {
        Ok(v) => v,
        Err(Err(why)) => return Err(EnvErr::Refused(why)),
        Err(Ok(e)) => return Err(EnvErr::Crash(format!("{}: {}", crate::pyyaml::qualified(&e), e.msg))),
    };
    if !matches!(v, Value::Obj(_)) {
        return Err(EnvErr::Crash(format!(
            "TypeError: opendaisugi.models.Envelope() argument after ** must be a mapping, not {}",
            py_type_name(&v)
        )));
    }
    match validate_model(Id::Envelope, &v, Mode::Python) {
        Ok(Value::Obj(o)) => Ok(o),
        Ok(_) => Err(EnvErr::Refused(format!("{path} does not hold a mapping"))),
        Err(e) => Err(EnvErr::Crash(format!(
            "pydantic_core._pydantic_core.ValidationError: {}",
            e.text().split('\n').next().unwrap_or("")
        ))),
    }
}

impl Env {
    pub(super) fn gate_replay(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--envelope"], "PATH", "Envelope to evaluate against (JSON/YAML)."),
            Opt::flag(&["--json"], "Emit the full report as JSON."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, ARG, &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                &format!(" {ARG}"),
                "Replay a captured session through the gate offline (nothing executes).",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, ARG, &format!("Missing argument '{ARG}'."));
        }
        if !p.has("--envelope") {
            return self.usage_args(CMD, ARG, "Missing option '--envelope'.");
        }
        let env = match replay_envelope(&p.str("--envelope", "")) {
            Ok(o) => o,
            Err(EnvErr::Refused(w)) => return self.refuse(CMD, &w),
            Err(EnvErr::Crash(c)) => {
                self.errf(&format!("Traceback (most recent call last): ...\n{c}\n"));
                return exit(1);
            }
        };
        let recs = match replay(&p.args[0], env, self.env.clone()) {
            Ok(r) => r,
            Err(ReplayErr::Refused(w)) => return self.refuse(CMD, &w),
            Err(ReplayErr::Crash(c)) => {
                self.errf(&format!("Traceback (most recent call last): ...\n{c}\n"));
                return exit(1);
            }
        };
        let rep = match from_records(recs) {
            Ok(r) => r,
            Err(e @ JournalErr::Crash(_)) => return self.fail(CMD, &e.to_string()),
            Err(e) => return self.refuse(CMD, &e.to_string()),
        };
        if p.flag("--json") {
            self.out(&format!("{}\n", rep.json()));
            return Ok(());
        }
        let mut b = format!(
            "calls={} allowed={} would_deny={} false_positive_candidates={}\n",
            rep.records.len(),
            rep.records.len() - rep.denied.len(),
            rep.denied.len(),
            rep.fp.len()
        );
        let fp: Vec<String> = rep.fp.iter().map(|i| canonical_json(&Value::Obj(rep.denied[*i].clone()))).collect();
        for r in &rep.denied {
            // `r in rep["false_positive_candidates"]`: equality, not identity.
            let mark = if fp.contains(&canonical_json(&Value::Obj(r.clone()))) { " [FP-candidate]" } else { "" };
            let line = (|| -> crate::gate::R<String> {
                let tool = py_str(r.value("tool_name"))?;
                let detail = match r.get("detail") {
                    Some(Value::Str(s)) => repr(s),
                    Some(other) => crate::gate::pyjson::py_repr(other)?,
                    None => repr(""),
                };
                let reason = py_str(r.value("reason"))?;
                Ok(format!("  DENY{mark} {tool} {detail}: {reason}\n"))
            })();
            match line {
                Ok(l) => b.push_str(&l),
                Err(_) => return self.refuse(CMD, "a record holds a value this binary does not print"),
            }
        }
        self.out(&b);
        Ok(())
    }
}
