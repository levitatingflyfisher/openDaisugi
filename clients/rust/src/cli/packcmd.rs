//! `daisugi pack ...` and `daisugi lora train` (cli.pack_app and
//! cli.lora_train_cmd on its pack path). The Go client's
//! `cli/packcmd.go` is the other port.

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::pack::manage::{self, Ctx, Io};
use crate::pack::{Catalog, CATALOG_ENV};

const PACK_HELP: &str = "Usage: daisugi pack [OPTIONS] COMMAND [ARGS]...

  The ML packs: a pinned Python with locked packages, for the jobs that
  stay in Python.

Options:
  --help  Show this message and exit.

Commands:
  bundle   Fetch a pack's Python and wheels into OUT, for pack install --offline.
  install  Install a pack: the pinned Python, a virtual environment, the locked wheels.
  list     List the packs and which are installed.
  remove   Delete a pack's directory, and nothing else.
  run      Run one job in a pack's worker.
  status   Check the installed packs against their pins.
";

const LORA_HELP: &str = "Usage: daisugi lora [OPTIONS] COMMAND [ARGS]...

  LoRA training: the trainer runs in the train pack.

Options:
  --help  Show this message and exit.

Commands:
  train   Train a LoRA adapter (python -m opendaisugi.lora.train's arguments).
  export  Emit (task → envelope JSON) pairs from the journal as JSONL for fine-tuning.
";

const LORA_TRAIN_HELP: &str =
    "Usage: daisugi lora train --jsonl JSONL --output OUTPUT [TRAINER OPTIONS]

  Train a LoRA adapter in the train pack (daisugi pack install train).
  Every option is the trainer's (python -m opendaisugi.lora.train). The
  base model is --base-model, else the choice daisugi models use recorded
  in --data-dir, else the default for this hardware.
";

/// Each line goes out at once, so a long job's progress streams.
struct EnvIo<'e>(&'e mut Env);

impl Io for EnvIo<'_> {
    fn out(&mut self, line: &str) {
        self.0.out(&format!("{line}\n"));
        self.0.flush();
    }
    fn err(&mut self, line: &str) {
        self.0.errf(&format!("{line}\n"));
        self.0.flush();
    }
}

const DATA_DIR: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");

/// `worker.flag_value`.
fn flag_value(args: &[String], flag: &str) -> Option<String> {
    let mut got = None;
    let eq = format!("{flag}=");
    for (i, a) in args.iter().enumerate() {
        if a == flag && i + 1 < args.len() {
            got = Some(args[i + 1].clone());
        } else if let Some(v) = a.strip_prefix(&eq) {
            got = Some(v.to_string());
        }
    }
    got
}

impl Env {
    fn pack_catalog(&mut self, cmd: &str) -> Result<Catalog, super::Stop> {
        let path = self.env.get(CATALOG_ENV).cloned().filter(|p| !p.is_empty());
        Catalog::load(path.as_deref()).map_err(|e| match self.fail(cmd, &e) {
            Err(s) => s,
            Ok(()) => super::Stop::Exit(1),
        })
    }

    fn pack_with(&mut self, cmd: &str, data_dir: String, f: impl FnOnce(&mut Ctx) -> i32) -> Res {
        let cat = self.pack_catalog(cmd)?;
        let system = self
            .env
            .get(manage::SYSTEM_ENV)
            .cloned()
            .filter(|v| !v.is_empty());
        let system_dir = Some(system.unwrap_or_else(|| manage::SYSTEM_PACKS.to_string()));
        let mut io = EnvIo(self);
        let mut ctx = Ctx {
            data_dir,
            cat,
            env: None,
            io: &mut io,
            system_dir,
        };
        match f(&mut ctx) {
            0 => Ok(()),
            code => exit(code),
        }
    }

    fn pack_default_dir(&self) -> String {
        self.data_home()
    }

    /// The `pack` group.
    pub(super) fn pack(&mut self, args: &[String]) -> Res {
        let Some(first) = args.first() else {
            self.out(PACK_HELP);
            return exit(2);
        };
        let sub = first.as_str();
        if sub == "--help" {
            self.out(PACK_HELP);
            return Ok(());
        }
        if sub == "run" {
            return self.pack_run(&args[1..]);
        }
        const OFFLINE: Opt = Opt::val(
            &["--offline"],
            "DIR",
            "Install from a bundle (a directory or a .tar).",
        );
        const FORCE: Opt = Opt::flag(&["--force"], "Install again over an installed pack.");
        let (shape, max, summary, opts): (&str, usize, &str, Vec<Opt>) = match sub {
            "list" => (
                "",
                0,
                "List the packs and which are installed.",
                vec![DATA_DIR],
            ),
            "status" => (
                " [NAME]",
                1,
                "Check the installed packs against their pins.",
                vec![DATA_DIR],
            ),
            "install" => (
                " NAME",
                1,
                "Install a pack: the pinned Python, a virtual environment, the locked wheels.",
                vec![OFFLINE, FORCE, DATA_DIR],
            ),
            "remove" => (
                " NAME",
                1,
                "Delete a pack's directory, and nothing else.",
                vec![DATA_DIR],
            ),
            "bundle" => (
                " NAME OUT",
                2,
                "Fetch a pack's Python and wheels into OUT, for pack install --offline.",
                vec![DATA_DIR],
            ),
            _ => {
                self.errf(&format!(
                    "Usage: daisugi pack [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi pack --help' for help.\n\nError: No such command '{sub}'.\n"
                ));
                return exit(2);
            }
        };
        let cmd = format!("pack {sub}");
        let p = match parse_args(&args[1..], &opts, max) {
            Ok(p) => p,
            Err(m) => return self.usage_args(&cmd, shape.trim(), &m),
        };
        if p.help {
            return self.cmd_help(&cmd, shape, summary, &opts);
        }
        let need = shape.matches(' ').count() - shape.matches('[').count();
        if p.args.len() < need {
            let missing = shape
                .split_whitespace()
                .nth(p.args.len())
                .unwrap_or("NAME")
                .to_string();
            return self.usage_args(
                &cmd,
                shape.trim(),
                &format!("Missing argument '{missing}'."),
            );
        }
        let data_dir = path_str(&p.str("--data-dir", &self.pack_default_dir()));
        let a = p.args.clone();
        let offline = p.has("--offline").then(|| p.str("--offline", ""));
        let force = p.flag("--force");
        self.pack_with(&cmd, data_dir, |c| match sub {
            "list" => manage::list(c),
            "status" => manage::status(c, a.first().map(String::as_str)),
            "install" => manage::install(c, &a[0], offline.as_deref(), force),
            "remove" => manage::remove(c, &a[0]),
            _ => manage::bundle(c, &a[0], &a[1]),
        })
    }

    /// `pack run [--data-dir P] NAME JOB [ARGS]...`: options only before
    /// NAME; every word after JOB goes to the job.
    fn pack_run(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pack run";
        const SHAPE: &str = "NAME JOB [ARGS]...";
        let mut data_dir = self.pack_default_dir();
        let mut i = 0;
        while i < args.len() && args[i].starts_with('-') && args[i] != "-" {
            let a = args[i].as_str();
            if a == "--help" {
                return self.cmd_help(
                    CMD,
                    " NAME JOB [ARGS]...",
                    "Run one job in a pack's worker. Options go before NAME; every word after JOB goes to the job.",
                    &[DATA_DIR],
                );
            } else if a == "--data-dir" {
                if i + 1 >= args.len() {
                    return self.usage_args(
                        CMD,
                        SHAPE,
                        "Option '--data-dir' requires an argument.",
                    );
                }
                data_dir = args[i + 1].clone();
                i += 2;
                continue;
            } else if let Some(v) = a.strip_prefix("--data-dir=") {
                data_dir = v.to_string();
            } else if a == "--" {
                i += 1;
                break;
            } else {
                return self.usage_args(CMD, SHAPE, &format!("No such option: {a}"));
            }
            i += 1;
        }
        let rest = &args[i..];
        if rest.is_empty() {
            return self.usage_args(CMD, SHAPE, "Missing argument 'NAME'.");
        }
        if rest.len() < 2 {
            return self.usage_args(CMD, SHAPE, "Missing argument 'JOB'.");
        }
        let (name, job, jargs) = (rest[0].clone(), rest[1].clone(), rest[2..].to_vec());
        self.pack_with(CMD, path_str(&data_dir), |c| {
            manage::run(c, &name, &job, &jargs)
        })
    }

    /// The `lora` group: train runs in the train pack; export reads the
    /// journal.
    pub(super) fn lora(&mut self, args: &[String]) -> Res {
        let Some(first) = args.first() else {
            self.out(LORA_HELP);
            return exit(2);
        };
        match first.as_str() {
            "--help" => {
                self.out(LORA_HELP);
                Ok(())
            }
            "train" => self.lora_train(&args[1..]),
            "export" => self.lora_export(&args[1..]),
            sub => {
                self.errf(&format!(
                    "Usage: daisugi lora [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi lora --help' for help.\n\nError: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// MC-R-6 picks the base model when --base-model is not given.
    fn lora_train(&mut self, args: &[String]) -> Res {
        if args.iter().any(|a| a == "--help" || a == "-h") {
            self.out(LORA_TRAIN_HELP);
            return Ok(());
        }
        let data_dir =
            path_str(&flag_value(args, "--data-dir").unwrap_or_else(|| self.pack_default_dir()));
        let mut args = args.to_vec();
        if flag_value(&args, "--base-model").is_none() {
            let base = match super::modelscmd::in_use(&data_dir) {
                Some(b) => b,
                None => {
                    let cat = self.models_catalog("lora train")?;
                    let hw = self.models_hardware();
                    cat.default_model(hw.ram_gb, hw.vram_gb)
                }
            };
            args.splice(0..0, ["--base-model".to_string(), base]);
        }
        self.pack_with("lora train", data_dir, |c| manage::lora_train(c, &args))
    }
}
