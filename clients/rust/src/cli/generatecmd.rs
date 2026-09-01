//! `daisugi generate-envelope`: one envelope for TASK from a model, as
//! `generate_envelope` makes it with no cache, pathway store or Tier-1
//! slot. The Go client's `cli/envelopecmd.go` is the reference.

use super::gatecmds::DECOMPOSE_OPT;
use super::gateroot::{self, join, path_str};
use super::{config, exit, parse_args, Env, Opt, Res};
use crate::envgen::{generate, GenErr, Options, DEFAULT_MODEL};
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::{dumps_indent, Object, Value};
use crate::llm;
use crate::pathways::pmodel::{validate_json, Id};

/// `defaults.DEFAULT_LOW_STAKES_ENVELOPE`.
const DEFAULT_LOW_STAKES: &str = r#"{"id": "env_default_low_stakes", "generated_by": "opendaisugi-library-default",
"task": "<default low-stakes envelope>", "permissions": {"file_read": ["**"], "file_write": ["/tmp/**", "./out/**"],
"network": false, "network_hosts": [], "shell": false, "shell_allowlist": [], "max_execution_time_s": 30,
"max_output_size_mb": 10}, "invariants": [], "postconditions": [],
"summary": "Default low-stakes envelope (dev/sandbox use)"}"#;

/// The name of the exception Python raises for an OS error.
fn os_name(e: &std::io::Error) -> &'static str {
    match e.raw_os_error() {
        Some(21) => "IsADirectoryError",
        Some(20) => "NotADirectoryError",
        Some(2) => "FileNotFoundError",
        Some(17) => "FileExistsError",
        Some(13) | Some(1) => "PermissionError",
        _ => "OSError",
    }
}

impl Env {
    /// `cli._tilde`: a path under the home directory as ~/..., once
    /// resolved.
    pub(super) fn tilde(&self, p: &str) -> String {
        let Ok(r) = gateroot::resolve(p) else { return p.to_string() };
        let home = self.home.trim_end_matches('/');
        if r == home {
            return "~/.".to_string();
        }
        match r.strip_prefix(&format!("{home}/")) {
            Some(rel) => format!("~/{rel}"),
            None => p.to_string(),
        }
    }

    /// The gate state `_echo_resolved(data_dir)` names.
    pub(super) fn gate_state_at(&self, data_dir: &str) -> Result<String, String> {
        if gateroot::is_disarmed(&join(data_dir, "gate")) {
            return Ok("disarmed".into());
        }
        config::gate_mode(&join(data_dir, "config.yaml")).map_err(|e| e.to_string())
    }

    /// `_echo_resolved` on the old backend name: resolve_backend raises
    /// LLMNotConfigured, which main() prints as one line, exit 1.
    pub(super) fn renamed_backend_at(&mut self, cmd: &str, data_dir: &str) -> Res {
        if !llm::renamed(&self.llm_client().backend()) {
            return Ok(());
        }
        if let Err(why) = self.gate_state_at(data_dir) {
            return self.refuse(cmd, &why);
        }
        self.errf(&format!("{}\n", llm::RENAMED_TEXT));
        exit(1)
    }

    pub(super) fn generate_envelope(&mut self, args: &[String]) -> Res {
        const CMD: &str = "generate-envelope";
        let opts = [
            Opt::val(&["--model"], "TEXT", "LLM model, provider/model: anthropic/..., openai/... or ollama/..."),
            Opt::val(&["--data-dir"], "PATH", "Root data directory. Unused by this command but accepted for consistency."),
            Opt::flag(&["--json"], "Emit JSON instead of YAML."),
            Opt::val(&["--stakes"], "TEXT", "Stakes level: low (uses default), medium (cache), high (always fresh)."),
            Opt::val(&["--low-stakes-envelope"], "PATH", "Path to a JSON Envelope file; used when --stakes low is set."),
            Opt::val(&["--thinking-budget"], "TEXT", "Thinking budget: light, standard, deep (mapped per provider)."),
            Opt::val(&["--llm"], "TEXT", "LLM backend: api | claude-code. Default: auto-detect."),
            DECOMPOSE_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, " TASK", "Generate a safety envelope for TASK via an LLM.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "TASK", "TASK");
        }
        let task = p.args[0].clone();
        let model = p.str("--model", DEFAULT_MODEL);
        let data_dir = path_str(&p.str("--data-dir", &join(&self.home, ".opendaisugi")));
        if p.has("--llm") {
            let v = p.str("--llm", "");
            self.check_llm_flag(&v)?;
            self.env.insert("OPENDAISUGI_LLM_BACKEND".into(), v);
        }
        let stakes = p.str("--stakes", "medium");
        if !["low", "medium", "high"].contains(&stakes.as_str()) {
            self.errf(&format!("Invalid --stakes value {}. Must be one of: high, low, medium\n", repr(&stakes)));
            return exit(2);
        }
        let thinking = p.str("--thinking-budget", "standard");
        if !["light", "standard", "deep"].contains(&thinking.as_str()) {
            self.errf(&format!("Invalid --thinking-budget value {}. Must be one of: deep, light, standard\n", repr(&thinking)));
            return exit(2);
        }
        self.renamed_backend_at(CMD, &data_dir)?;
        let mut c = self.llm_client();
        if stakes != "low" {
            // A call this binary does not make the oracle's way is refused
            // before anything is said or asked.
            if let Err(why) = c.check(&model) {
                return self.refuse(CMD, &why);
            }
        }
        let gate = match self.gate_state_at(&data_dir) {
            Ok(g) => g,
            Err(why) => return self.refuse(CMD, &why),
        };
        let shown = self.tilde(&data_dir);
        self.note(&format!("backend: {} · gate: {gate} · data: {shown}", c.backend()));
        let mut o = Options { task, models: vec![model], stakes: stakes.clone(), thinking, ..Default::default() };
        if stakes == "low" {
            let text = if p.has("--low-stakes-envelope") {
                let path = p.str("--low-stakes-envelope", "");
                let raw = match std::fs::read(&path) {
                    Ok(b) => b,
                    Err(e) => return self.fail(CMD, &format!("{}: {e}", os_name(&e))),
                };
                match String::from_utf8(raw) {
                    Ok(t) => t,
                    Err(_) => return self.fail(CMD, &format!("UnicodeDecodeError: {path} is not UTF-8")),
                }
            } else {
                DEFAULT_LOW_STAKES.to_string()
            };
            match validate_json(Id::Envelope, &text) {
                Ok(Value::Obj(env)) => o.low_stakes = Some(env),
                Ok(_) => return self.fail(CMD, "pydantic_core._pydantic_core.ValidationError: not an object"),
                Err(e) => return self.fail(CMD, &format!("pydantic_core._pydantic_core.ValidationError: {}", e.text())),
            }
        }
        let env = match generate(&o, &mut c).envelope {
            Ok(env) => env,
            Err(GenErr::Py(e)) => {
                match e.class.as_str() {
                    "TaskTooLongError" => {
                        self.errf(&format!("{}\n", e.msg));
                        return exit(2);
                    }
                    "EnvelopeGenerationError" | "ModelCallError" => {
                        self.errf(&format!("Envelope generation failed: {}\n", e.msg));
                        return exit(2);
                    }
                    // main() prints an OpenDaisugiError as its text, exit 1.
                    "LLMNotConfigured" => {
                        self.errf(&format!("{}\n", strip(&e.msg)));
                        return exit(1);
                    }
                    _ => {}
                }
                return self.fail(CMD, &format!("{}: {}", e.class, e.msg));
            }
            Err(GenErr::Unported(why)) => return self.refuse(CMD, &format!("{why} is not in this binary yet")),
            Err(GenErr::Inherit(ie)) => return self.fail(CMD, &format!("EnvelopeInheritanceError: {ie}")),
            Err(GenErr::CacheRow(m)) | Err(GenErr::Other(m)) => return self.fail(CMD, &m),
        };
        let decompose = if p.flag_set("--allow-shell-decomposition") {
            p.flag("--allow-shell-decomposition")
        } else {
            self.decompose_default(&join(&data_dir, "gate"), CMD)?
        };
        let mut env: Object = env;
        if decompose {
            if let Some(Value::Obj(perms)) = env.get_mut("permissions") {
                perms.set("shell_allow_decomposition", true);
            }
        }
        let env = Value::Obj(env);
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&env, 2, true)));
            return Ok(());
        }
        match crate::pathways::yamldump::safe_dump(&env) {
            Ok(text) => {
                self.out(&format!("{}\n", text.trim_end_matches([' ', '\t', '\n', '\r', '\x0b', '\x0c'])));
                Ok(())
            }
            Err(why) => self.refuse(CMD, &format!("the envelope holds a value this binary does not write as YAML: {why:?}")),
        }
    }
}
