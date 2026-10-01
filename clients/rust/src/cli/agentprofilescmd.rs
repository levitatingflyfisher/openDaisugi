//! `daisugi agent-profiles bundle`: the operator's table, written to a
//! descriptor. The Go client's `cli/agentprofilescmd.go` is the reference.

use std::io::Write;

use super::{exit, parse_args, Env, Opt, Res};

const HELP: &str = "Usage: daisugi agent-profiles [OPTIONS] COMMAND [ARGS]...

  The operator's agent profiles.

Options:
  --help  Show this message and exit.

Commands:
  bundle  Write the resolved agent profiles to a descriptor.
  list    List the agent profiles: name, wire, base URL, model, and whether the key is set.
";

impl Env {
    pub(super) fn agent_profiles_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            self.out(HELP);
            return exit(2);
        }
        match args[0].as_str() {
            "--help" => {
                self.out(HELP);
                Ok(())
            }
            "bundle" => self.agent_profiles_bundle(&args[1..]),
            "list" => self.agent_profiles_list(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi agent-profiles [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi agent-profiles --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `cli.agent_profiles_bundle_cmd`: resolve the table (each key_env is
    /// read once), print one line to stderr for each profile left out, and
    /// write the bundle to the descriptor. Nothing goes to stdout.
    fn agent_profiles_bundle(&mut self, args: &[String]) -> Res {
        const CMD: &str = "agent-profiles bundle";
        let opts = [Opt::val(&["--fd"], "INTEGER", "The descriptor to write the bundle to.")];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, "", "Write the resolved agent profiles to a descriptor.", &opts);
        }
        if !p.has("--fd") {
            return self.usage(CMD, "Missing option '--fd'.".into());
        }
        let raw = p.str("--fd", "");
        let Some(fd) = super::pathwayscmd::py_int(&raw) else {
            return self.usage(
                CMD,
                format!("Invalid value for '--fd': {} is not a valid int.", crate::gate::py::text::repr(&raw)),
            );
        };
        if (0..3).contains(&fd) {
            return Err(self.fail3(
                "Tried to write the agent profiles.",
                &format!("Descriptor {fd} is standard input, output or error."),
                "Pass a descriptor of 3 or more.",
                2,
            ));
        }
        let table = crate::agentprof::configured_table(&self.data_home());
        let profiles = crate::agentprof::resolve(table.as_ref(), &|k| self.env.get(k).cloned());
        let (data, notes) = crate::agentprof::operator_bundle(&profiles);
        for line in &notes {
            self.errf(&format!("{line}\n"));
        }
        let wrote = i32::try_from(fd).ok().filter(|n| *n >= 0 && unsafe { libc::fcntl(*n, libc::F_GETFD) } >= 0).is_some_and(|n| {
            use std::os::fd::FromRawFd;
            let mut f = unsafe { std::fs::File::from_raw_fd(n) };
            f.write_all(&data).and_then(|()| f.flush()).is_ok()
        });
        if !wrote {
            return Err(self.fail3(
                "Tried to write the agent profiles.",
                &format!("Descriptor {fd} cannot be written."),
                "Pass the descriptor the profiles go to.",
                2,
            ));
        }
        Ok(())
    }

    /// `cli.agent_profiles_list_cmd`: the valid profiles with no key, one
    /// line to stderr for each entry left out.
    fn agent_profiles_list(&mut self, args: &[String]) -> Res {
        const CMD: &str = "agent-profiles list";
        let opts = [Opt::flag(&["--json"], "Machine-readable JSON output.")];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "List the agent profiles: name, wire, base URL, model, and whether the key is set.",
                &opts,
            );
        }
        let table = crate::agentprof::configured_table(&self.data_home());
        let profiles = crate::agentprof::resolve(table.as_ref(), &|k| self.env.get(k).cloned());
        let rows = crate::agentprof::list_rows(&profiles);
        for (n, why) in &profiles.invalid {
            self.errf(&format!("agent profile '{n}' is left out: {why}\n"));
        }
        if p.flag("--json") {
            use crate::gate::pyjson::{canonical_json_ascii, Value};
            self.out(&format!("{}\n", canonical_json_ascii(&Value::List(rows))));
            return Ok(());
        }
        for r in &rows {
            let crate::gate::pyjson::Value::Obj(o) = r else { continue };
            let text = |k: &str| match o.get(k) {
                Some(crate::gate::pyjson::Value::Str(s)) if !s.is_empty() => s.clone(),
                _ => "-".to_string(),
            };
            let key = match o.get("key_set") {
                Some(crate::gate::pyjson::Value::Bool(true)) => "key set",
                _ => "key not set",
            };
            self.out(&format!(
                "{}\t{}\t{}\t{}\t{}\n",
                text("name"),
                text("wire"),
                text("base_url"),
                text("model"),
                key
            ));
        }
        Ok(())
    }
}
