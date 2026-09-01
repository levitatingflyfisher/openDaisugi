//! `daisugi config`: every setting as daisugi will use it, and where it
//! came from (`config.resolved_config`). The Go client's `configcmd.go` is
//! the reference.

use super::config::{self, ConfigErr, Row, RowsErr};
use super::{parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::{len as py_len, strip};
use crate::gate::pyjson::{dumps_indent, Object, Value};

pub(super) const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

impl Env {
    /// Ends a command whose config.yaml load_config raises on: exit 1
    /// with the exception Python's traceback ends in, or the refusal when
    /// the file is outside the YAML this binary reads.
    pub(super) fn config_load_err(&mut self, cmd: &str, err: &RowsErr) -> Stop {
        match err {
            RowsErr::Attr(_) => {
                self.errf(&format!("daisugi {cmd}: {err}\n"));
                Stop::Exit(1)
            }
            RowsErr::Config(ConfigErr::Invalid) => {
                self.errf(&format!(
                    "daisugi {cmd}: pydantic_core._pydantic_core.ValidationError: the config file does not validate\n"
                ));
                Stop::Exit(1)
            }
            RowsErr::Config(e) => {
                self.errf(&format!("daisugi {cmd}: the config file is not one this binary reads: {e}. Nothing was changed.\n"));
                Stop::Exit(2)
            }
        }
    }

    pub(super) fn config_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "config";
        let opts = [JSON_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, "", "Show every setting as daisugi will use it, and where each one came from.", &opts);
        }
        let path = if self.home == "/" {
            "/.opendaisugi/config.yaml".to_string()
        } else {
            format!("{}/.opendaisugi/config.yaml", self.home)
        };
        let home = self.home.clone();
        let (mut rows, unknown, cfg) = match config::rows(&path, &home) {
            Ok(r) => r,
            Err(e) => return Err(self.config_load_err(CMD, &e)),
        };
        // llm_backend (resolved): the environment, then the file, then
        // what runs on this box.
        let env_backend = self.env.get("OPENDAISUGI_LLM_BACKEND").cloned().unwrap_or_default();
        let file_backend = cfg.llm_backend.as_deref().map(|b| strip(b).to_string()).unwrap_or_default();
        let (backend, source) = if !env_backend.is_empty() {
            (env_backend, "env")
        } else if !file_backend.is_empty() {
            (file_backend, "file")
        } else {
            let keyless = self.env.get("ANTHROPIC_API_KEY").is_none_or(|v| v.is_empty())
                && self.env.get("ANTHROPIC_AUTH_TOKEN").is_none_or(|v| v.is_empty());
            let b = if keyless && super::install::look_path(&self.env, "claude").is_some() { "claude-code" } else { "api" };
            (b.to_string(), "auto")
        };
        rows.push(Row::new("llm_backend (resolved)", &backend, source));
        let cwd = self.cwd();
        let eff = match config::effective_hook_mode(&self.home, &cwd) {
            Ok(e) => e,
            Err(_) => return self.refuse(CMD, "a Claude Code settings.json holds hooks this binary does not read yet"),
        };
        if !eff.mode.is_empty() {
            rows.push(Row::new("gate_mode (resolved)", &eff.mode, config::source_label(&eff)));
            if !eff.cwd_mode.is_empty() && !eff.global_mode.is_empty() {
                rows.push(Row::new("gate_mode (project)", &eff.cwd_mode, "project"));
                rows.push(Row::new("gate_mode (global)", &eff.global_mode, "global"));
                rows.push(Row::new(
                    "gate_mode (coexistence)",
                    "both fire; the verdict is the intersection — either one enforcing denies",
                    "info",
                ));
            }
        } else {
            let src = rows.iter().rev().find(|r| r.key == "gate_mode").map(|r| r.source.clone()).unwrap_or("default".into());
            rows.push(Row::new("gate_mode (resolved)", &cfg.gate_mode, &src));
        }
        if p.flag("--json") {
            let mut body = Object::new();
            for r in &rows {
                body.set(&r.key, Object::new().with("value", r.value.as_str()).with("source", r.source.as_str()));
            }
            let unk = Value::List(unknown.iter().map(|k| Value::Str(k.clone())).collect());
            body.set("_meta", Object::new().with("path", path.as_str()).with("unknown_keys_ignored", unk));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(body), 2, true)));
            return Ok(());
        }
        let width = rows.iter().map(|r| py_len(&r.key)).max().unwrap_or(0);
        let note = if std::fs::metadata(&path).is_err() { "  (not present)" } else { "" };
        let mut b = format!("config file: {path}{note}\n");
        for r in &rows {
            b.push_str(&format!("  {}{}  {}  ({})\n", r.key, " ".repeat(width - py_len(&r.key)), r.value, r.source));
        }
        if !unknown.is_empty() {
            b.push_str(&format!("  unknown keys ignored: {}\n", unknown.join(", ")));
        }
        self.out(&b);
        Ok(())
    }
}
