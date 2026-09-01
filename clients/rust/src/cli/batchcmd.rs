//! `daisugi batch prove` (`opendaisugi/batch.py`): a declared batch
//! proved before any item runs. The Go client's `cli/batchcmd.go` is the
//! reference.

use super::gateroot::path_str;
use super::registrycmd::read_text;
use super::{exit, parse_args, Env, Opt, Res};
use crate::batch;
use crate::gate::pyjson::Value;
use crate::pathways::pmodel::{validate_json, Id};

const BATCH_HELP: &str = "Usage: daisugi batch [OPTIONS] COMMAND [ARGS]...

  Within-instance batch compilation: prove a declared batch before it runs.

Commands:
  prove  Statically prove a declared batch before any iteration.
";

impl Env {
    pub(super) fn batch_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(BATCH_HELP);
            return Ok(());
        }
        if args[0] == "prove" {
            return self.batch_prove(&args[1..]);
        }
        self.errf(&format!(
            "Usage: daisugi batch [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi batch --help' for help.\n\nError: No such command '{}'.\n",
            args[0]
        ));
        exit(2)
    }

    fn batch_prove(&mut self, args: &[String]) -> Res {
        const CMD: &str = "batch prove";
        let opts = [Opt::val(
            &["--envelope", "-e"],
            "PATH",
            "Envelope JSON to prove the footprint against.",
        )];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "DECLARATION", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " DECLARATION",
                "Statically prove a declared batch before any iteration.",
                &opts,
            );
        }
        let Some(decl_path) = p.args.first() else {
            return self.usage_args(CMD, "DECLARATION", "Missing argument 'declaration'.");
        };
        if !p.has("--envelope") {
            return self.usage_args(CMD, "DECLARATION", "Missing option '--envelope' / '-e'.");
        }
        let text = match read_text(&path_str(decl_path)) {
            Ok(t) => t,
            Err(e) => return self.raise(CMD, e),
        };
        let decl = match batch::validate_text(&text) {
            Ok(d) => d,
            Err(e) => return self.raise(CMD, e.into()),
        };
        let etext = match read_text(&path_str(&p.str("--envelope", ""))) {
            Ok(t) => t,
            Err(e) => return self.raise(CMD, e),
        };
        let env = match validate_json(Id::Envelope, &etext) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => unreachable!("a model validates to an object"),
            Err(e) => return self.raise(CMD, e.into()),
        };
        let cls = batch::classify(&decl);
        if !cls.batchable {
            self.echo_err(&format!(
                "NOT BATCHABLE — program contains non-batchable step kind(s): {}\n",
                cls.kinds()
            ));
            return exit(1);
        }
        let fw: Vec<String> = match env
            .value("permissions")
            .as_obj()
            .map(|p| p.value("file_write"))
        {
            Some(Value::List(l)) => l
                .iter()
                .filter_map(|g| g.as_str().map(str::to_string))
                .collect(),
            _ => vec![],
        };
        let proof = match batch::prove(&decl, &fw) {
            Ok(pr) => pr,
            Err(e) => return self.raise(CMD, e),
        };
        for w in &proof.writes {
            self.echo(&format!("  write: {w}\n"));
        }
        let irr = batch::irreversible(&proof.writes);
        if proof.ok && irr.is_empty() {
            self.echo(&format!(
                "PROVABLE — {} write(s), all inside the envelope and the declared footprint F; one proof covers all N.\n",
                proof.writes.len()
            ));
            return Ok(());
        }
        if !proof.ok {
            self.echo_err(&format!("UNPROVABLE — {}\n", proof.reason));
        }
        if !irr.is_empty() {
            self.echo_err(&format!(
                "IRREVERSIBLE TARGETS (cannot enter a batch): {}\n",
                batch::list_repr(&irr)
            ));
        }
        exit(1)
    }
}
