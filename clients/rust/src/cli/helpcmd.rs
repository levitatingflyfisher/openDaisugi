//! `daisugi help [--all]`: the start-here text, or every command grouped,
//! as the oracle prints them. The Go client's `cli/helpcmd.go` is the twin.

use super::helpall_gen::HELP_ALL;
use super::{parse_args, Env, Opt, Res, START_HERE};

/// `clients/cli_cases.HELP_NOT_PORTED`: the commands of the oracle's help
/// --all this binary does not carry, a top-level command or "group sub".
/// Their lines are left out (ruling HP-1).
const HELP_NOT_PORTED: &[&str] = &["bench", "conformance", "coppice", "gate audit"];

/// The oracle's help --all text with the lines of the commands this binary
/// does not carry left out.
pub fn help_all_for_port(text: &str) -> String {
    let mut out = String::new();
    for line in text.split_inclusive('\n') {
        let words: Vec<&str> = line.split_whitespace().collect();
        if line.starts_with("    ") && words.len() >= 2 {
            let pair = format!("{} {}", words[0], words[1]);
            if HELP_NOT_PORTED.contains(&words[0]) || HELP_NOT_PORTED.contains(&pair.as_str()) {
                continue;
            }
        } else if line.starts_with("  ") && !words.is_empty() && HELP_NOT_PORTED.contains(&words[0]) {
            continue;
        }
        out.push_str(line);
    }
    out
}

impl Env {
    pub(super) fn help_cmd(&mut self, args: &[String]) -> Res {
        let opts = [Opt::flag(&["--all"], "List every command, grouped.")];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("help", m),
        };
        if p.help {
            return self.cmd_help("help", "", "Show the short start-here text, or every command with --all.", &opts);
        }
        if p.flag("--all") {
            self.out(&help_all_for_port(HELP_ALL));
        } else {
            self.out(START_HERE);
        }
        Ok(())
    }
}
