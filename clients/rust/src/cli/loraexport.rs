//! `daisugi lora export OUTPUT`: (task → envelope JSON) pairs from the
//! journal's successful traces as JSONL (`lora.dataset.emit_jsonl`). The
//! Go client's `cli/loraexport.go` is the twin.

use super::gateroot::{mkdir_all, parent, path_str};
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::{len, repr, strip};
use crate::gate::pyjson::{dumps, dumps_indent, Object, Value};
use crate::pathways::pathway::dump_json;

impl Env {
    pub(super) fn lora_export(&mut self, args: &[String]) -> Res {
        const CMD: &str = "lora export";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", ""),
            Opt::val(&["--format"], "TEXT", "Output format: alpaca (instruction/input/output) or chat (messages)."),
            Opt::val(&["--days"], "INTEGER", "Only include traces from the last N days."),
            Opt::val(&["--min-task-chars"], "INTEGER", "Skip traces with tasks shorter than this many characters."),
            Opt::val(&["--system-prompt"], "TEXT", "System prompt injected into chat-format examples."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "OUTPUT", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " OUTPUT", "Emit (task → envelope JSON) pairs from the journal as JSONL for fine-tuning.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "OUTPUT", "Missing argument 'output'.");
        }
        let days = if p.has("--days") { Some(self.click_int(CMD, &p, "--days", 0)?) } else { None };
        let min_chars = self.click_int(CMD, &p, "--min-task-chars", 10)?;
        let format = p.str("--format", "alpaca");
        if format != "alpaca" && format != "chat" {
            self.errf(&format!("Unknown format {}; expected 'alpaca' or 'chat'.\n", repr(&format)));
            return exit(2);
        }
        let since = days.map(|d| {
            let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|x| x.as_secs_f64()).unwrap_or(0.0);
            now - d as f64 * 86400.0
        });
        let dir = self.data_dir_of(&p);
        let j = self.open_journal(CMD, &dir)?;
        let out = path_str(&p.args[0]);
        // emit_jsonl makes the parent first, then lists the traces.
        let par = parent(&out);
        if !par.is_empty() && par != "." {
            if let Err(e) = mkdir_all(&par) {
                return self.fail(CMD, &e.to_string());
            }
        }
        let rows = j.list_successful(since).map_err(|e| self.pw_err(CMD, e))?;
        let (mut written, mut skipped_empty, mut skipped_load) = (0i64, 0i64, 0i64);
        let mut b = String::new();
        for row in &rows {
            let rec = match j.load_trace(&row.trace_id).map_err(|e| self.pw_err(CMD, e))? {
                Ok(r) => r,
                Err(_) => {
                    skipped_load += 1;
                    continue;
                }
            };
            let task = match &rec.task {
                Value::Str(t) => strip(t).to_string(),
                Value::Null => String::new(),
                _ => return self.refuse(CMD, "a trace's task is not text"),
            };
            if (len(&task) as i128) < min_chars {
                skipped_empty += 1;
                continue;
            }
            let env_json = dump_json(&Value::Obj(rec.envelope.clone()));
            let mut payload = Object::new();
            if format == "alpaca" {
                payload.set("instruction", task.as_str());
                payload.set("input", "");
                payload.set("output", env_json.as_str());
            } else {
                let mut msgs = vec![];
                let sp = p.str("--system-prompt", "");
                let msg = |role: &str, content: &str| {
                    let mut m = Object::new();
                    m.set("role", role);
                    m.set("content", content);
                    Value::Obj(m)
                };
                if !sp.is_empty() {
                    msgs.push(msg("system", &sp));
                }
                msgs.push(msg("user", &task));
                msgs.push(msg("assistant", &env_json));
                payload.set("messages", Value::List(msgs));
            }
            b.push_str(&dumps(&Value::Obj(payload), true));
            b.push('\n');
            written += 1;
        }
        if let Err(e) = super::gateroot::write_file(&out, &b) {
            return self.fail(CMD, &e.to_string());
        }
        let stats = Object::new()
            .with("total", Value::Int(rows.len().to_string()))
            .with("written", Value::Int(written.to_string()))
            .with("skipped_empty_task", Value::Int(skipped_empty.to_string()))
            .with("skipped_load_error", Value::Int(skipped_load.to_string()))
            .with("output_path", Value::Str(out.clone()))
            .with("format", Value::Str(format.clone()));
        self.out(&format!("{}\n", dumps_indent(&Value::Obj(stats), 2, true)));
        Ok(())
    }
}
