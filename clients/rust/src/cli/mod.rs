//! The `daisugi` command: the gate commands and the gate half of
//! `install`, with the flags, files and output of the Python CLI
//! (`opendaisugi.cli`). A command or flag it does not carry says so in one
//! line and exits 2, changing nothing. The Go client's `cli` package is
//! the reference.
//!
//! The CLI never runs Python. `daisugi gate check`, the hook entry an
//! installed hook runs, answers every call through the gate, decided in a
//! child process of this binary (`gate::hook`); a call the gate cannot
//! decide is denied with a reason.

pub mod config;
pub mod envelope;
mod autotendcmd;
mod gardencmd;
mod gatecmds;
pub mod gateroot;
pub mod install;
mod installcmd;
pub mod journal;
mod pathwayscmd;
mod tendcmd;
pub mod words;
pub mod yaml;

use std::collections::HashMap;
use std::io::{BufRead, Write};

/// The build's version: DAISUGI_VERSION when the build sets it
/// (scripts/release.sh does), else the git describe of the checkout
/// (build.rs reads it), else "unknown".
pub const VERSION: &str = env!("DAISUGI_VERSION");

/// One run's world.
pub struct Env {
    pub args: Vec<String>,
    pub env: HashMap<String, String>,
    pub home: String,
    out: Vec<u8>,
    err: Vec<u8>,
    /// Set while auto-tend runs tend: a failure prints as "tend failed:
    /// ..." and auto-tend goes on.
    tend_failed: bool,
    /// Set when tend's Daisugi() raised: auto-tend does not catch that
    /// one.
    raised: bool,
}

/// How a command ended.
pub enum Stop {
    /// Exit with this code; any message is already written.
    Exit(i32),
    /// click's UsageError: exit 2 with "Error: <msg>".
    Usage(String),
}

pub type Res = Result<(), Stop>;

fn exit(code: i32) -> Res {
    Err(Stop::Exit(code))
}

impl Env {
    fn out(&mut self, s: &str) {
        self.out.extend_from_slice(s.as_bytes());
    }

    fn errf(&mut self, s: &str) {
        self.err.extend_from_slice(s.as_bytes());
    }

    /// Writes what the command printed so far, so a prompt shows before
    /// the answer is read.
    fn flush(&mut self) {
        let _ = std::io::stdout().write_all(&self.out);
        let _ = std::io::stdout().flush();
        let _ = std::io::stderr().write_all(&self.err);
        self.out.clear();
        self.err.clear();
    }

    /// The one line a command or flag this binary does not carry prints.
    fn not_yet(&mut self, what: &str) -> Res {
        self.errf(&format!("{what} is not in this binary yet.\n"));
        exit(2)
    }

    /// A command whose input holds something this binary cannot yet read
    /// the way Python does. Exits 2 before anything is written.
    fn refuse(&mut self, cmd: &str, why: &str) -> Res {
        self.errf(&format!("daisugi {cmd}: {why}. Nothing was changed.\n"));
        exit(2)
    }

    /// An error Python meets too (it raises): exit 1 with the reason.
    fn fail(&mut self, cmd: &str, why: &str) -> Res {
        self.errf(&format!("daisugi {cmd}: {why}\n"));
        exit(1)
    }

    fn usage(&mut self, cmd: &str, msg: String) -> Res {
        self.errf(&format!("Usage: daisugi {cmd} [OPTIONS]\nTry 'daisugi {cmd} --help' for help.\n\nError: {msg}\n"));
        exit(2)
    }

    /// Says when a write went through a symlink to another file.
    fn note_file(&mut self, path: &str, followed: &Option<String>) {
        if let Some(f) = followed {
            self.errf(&format!("note: {path} is a symlink; wrote {f}.\n"));
        }
    }

    /// `typer.confirm(text, default=False)` on a piped stdin.
    fn confirm(&mut self, text: &str) -> Result<bool, Stop> {
        let stdin = std::io::stdin();
        let mut lock = stdin.lock();
        loop {
            self.out(&format!("{text} [y/N]: "));
            self.flush();
            let mut line = String::new();
            let n = lock.read_line(&mut line).unwrap_or(0);
            if n == 0 {
                self.errf("Aborted.\n");
                return Err(Stop::Exit(1));
            }
            match line.trim_end_matches(['\r', '\n']).trim().to_lowercase().as_str() {
                "y" | "yes" => return Ok(true),
                "n" | "no" | "" => return Ok(false),
                _ => {}
            }
            self.out("Error: invalid input\n");
        }
    }
}

/// One command-line option, spelled as the Python CLI (typer over click)
/// spells it.
#[derive(Clone)]
pub struct Opt {
    pub names: &'static [&'static str],
    pub neg: &'static str,
    pub value: bool,
    pub multiple: bool,
    pub help: &'static str,
    pub metavar: &'static str,
}

impl Opt {
    pub const fn flag(names: &'static [&'static str], help: &'static str) -> Opt {
        Opt { names, neg: "", value: false, multiple: false, help, metavar: "" }
    }

    pub const fn pair(names: &'static [&'static str], neg: &'static str, help: &'static str) -> Opt {
        Opt { names, neg, value: false, multiple: false, help, metavar: "" }
    }

    pub const fn val(names: &'static [&'static str], metavar: &'static str, help: &'static str) -> Opt {
        Opt { names, neg: "", value: true, multiple: false, help, metavar }
    }

    pub const fn many(names: &'static [&'static str], metavar: &'static str, help: &'static str) -> Opt {
        Opt { names, neg: "", value: true, multiple: true, help, metavar }
    }
}

/// What `parse_args` read.
#[derive(Default)]
pub struct Parsed {
    pub vals: HashMap<String, Vec<String>>,
    pub flags: HashMap<String, bool>,
    pub args: Vec<String>,
    pub help: bool,
}

impl Parsed {
    pub fn str(&self, name: &str, def: &str) -> String {
        self.vals.get(name).and_then(|v| v.last()).cloned().unwrap_or_else(|| def.to_string())
    }

    pub fn has(&self, name: &str) -> bool {
        self.vals.get(name).is_some_and(|v| !v.is_empty())
    }

    pub fn flag(&self, name: &str) -> bool {
        self.flags.get(name).copied().unwrap_or(false)
    }

    pub fn flag_set(&self, name: &str) -> bool {
        self.flags.contains_key(name)
    }

    pub fn list(&self, name: &str) -> Vec<String> {
        self.vals.get(name).cloned().unwrap_or_default()
    }
}

/// Reads args as click does: options anywhere, "--opt=value" or "--opt
/// value" (the next word is the value even when it starts with a dash),
/// "--" ending the options, and --help anywhere. `Err` is click's
/// UsageError text.
pub fn parse_args(args: &[String], opts: &[Opt], max_args: usize) -> Result<Parsed, String> {
    let mut p = Parsed::default();
    let find = |name: &str| -> Option<(&Opt, bool)> {
        for o in opts {
            if o.names.contains(&name) {
                return Some((o, true));
            }
            if !o.neg.is_empty() && o.neg == name {
                return Some((o, false));
            }
        }
        None
    };
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        if a == "--" {
            p.args.extend_from_slice(&args[i + 1..]);
            break;
        }
        if a == "--help" {
            p.help = true;
            i += 1;
            continue;
        }
        if !a.starts_with('-') || a == "-" {
            p.args.push(a.clone());
            i += 1;
            continue;
        }
        let (name, mut value, has_eq) = if a.starts_with("--") {
            match a.split_once('=') {
                Some((n, v)) => (n.to_string(), v.to_string(), true),
                None => (a.clone(), String::new(), false),
            }
        } else {
            (a.clone(), String::new(), false)
        };
        let (o, positive) = match find(&name) {
            Some(x) => x,
            None => return Err(format!("No such option: {name}")),
        };
        let key = o.names[0].to_string();
        if !o.value {
            if has_eq {
                return Err(format!("Option '{name}' does not take a value."));
            }
            let b = (positive || o.neg.is_empty()) && !(!o.neg.is_empty() && name == o.neg);
            p.flags.insert(key, b);
            i += 1;
            continue;
        }
        if !has_eq {
            if i + 1 >= args.len() {
                return Err(format!("Option '{name}' requires an argument."));
            }
            i += 1;
            value = args[i].clone();
        }
        let e = p.vals.entry(key).or_default();
        if o.multiple {
            e.push(value);
        } else {
            *e = vec![value];
        }
        i += 1;
    }
    if !p.help && p.args.len() > max_args {
        return Err(format!("Got unexpected extra argument(s) ({})", p.args[max_args..].join(" ")));
    }
    Ok(p)
}

const START_HERE: &str = "daisugi: a gate that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          gate this directory (shadow mode) and watch it
  daisugi start --enforce                the same, but out-of-envelope calls are denied
  daisugi orchestrate \"list three risks\" run a prompt end to end under a verified plan
  daisugi status                         is this machine ready; what is installed
  daisugi dashboard                      the live view of sessions, verdicts, and cost

More
  daisugi config                         every setting, with its source
  daisugi help --all                     every command, grouped
";

/// Every other command of the Python CLI, so --help shows what this binary
/// does not carry.
const NOT_IN_BINARY: &[&str] = &[
    "start", "status", "dashboard", "orchestrate", "config", "help", "journal", "batch", "bench",
    "conformance", "coppice", "gateway", "gateway-report", "generate-envelope",
    "lora", "mcp", "metrics", "models", "modules", "onboard", "registry", "release", "route", "router", "run", "setup",
    "tiers", "verify", "viz", "voice", "gate replay", "gate audit", "gate serve",
];

const ROOT_HELP: &str = "Usage: daisugi [OPTIONS] COMMAND [ARGS]...

  Runtime assurance for agent actions. This is the daisugi Rust binary:
  the gate commands, the gate half of install, the pathway store and the
  garden. gate check answers every call natively.

Options:
  --version        Show the opendaisugi version and exit.
  --plain          No color, no box drawing; greppable output.
  -q, --quiet      Results only; no progress notes.
  -v, --verbose    Show tracebacks and detail.
  --no-color       Disable color (same as NO_COLOR=1).
  --help           Show this message and exit.

Gate:
  gate check       Answer one tool-call hook: the verdict for the payload on stdin.
  gate init        Register a reviewable starter envelope for this session.
  gate register    Register the envelope the gate checks this session's calls against.
  gate arm         Remove the disarm marker; the gate resumes evaluating calls.
  gate disarm      Kill switch: the gate allows everything until re-armed.
  gate status      Show armed/disarmed state, the verdict mode, and the envelopes.
  gate report      Summarize the shadow log: what an enforcing gate would have denied.
  gate settings    Print the Claude Code hooks-settings JSON that wires in the gate.
  gate proposals   List recorded envelope-edit proposals.
  install          Wire the gate into agent harnesses (--gate, --harness).

Pathways:
  pathways list    List all compiled pathways.
  pathways show    Show a compiled pathway in detail.
  pathways stats   Summarize stored pathways (count, total hits).
  pathways delete  Delete a compiled pathway.
  pathways export  Export a pathway: json, skill, mermaid, md or smtlib.
  pathways import  Import a pathway bundle, re-verify it, and admit it.

Garden:
  gardener prune   Evict stale / failure-dominated pathways.
  gardener merge   Collapse near-duplicate pathways.
  gardener run     Run the full gardener pipeline (prune + merge).
  gardener watch   Cron-friendly one-shot gardener.
  gardener status  Report store size, activation stats, failure ratios.
  tend             Distil successful journal traces into pathways.
  hook auto-tend   Turn captured sessions into traces, then tend.
  distill-repeats  Rank repeated gateway asks into a reuse worklist.

Not yet in this binary (use the Python CLI, opendaisugi.cli):
";

const GATE_HELP: &str = "Usage: daisugi gate [OPTIONS] COMMAND [ARGS]...

  Call-time tool gate (ADR-0007): verify each live tool call against a
  registered envelope. Shadow by default; --mode enforce denies.

Commands:
  check      Answer one hook payload on stdin with the host's verdict.
  init       Generate and register a reviewable starter envelope.
  register   Register the envelope the gate checks calls against.
  disarm     Kill switch: the gate allows everything until re-armed.
  arm        Remove the disarm marker; the gate resumes evaluating calls.
  status     Show armed state, the verdict mode, and the envelopes.
  report     Summarize the shadow log.
  settings   Print the Claude Code hooks-settings JSON for the gate.
  proposals  List recorded envelope-edit proposals.

Not yet in this binary: replay, audit, serve.
";

const CHECK_HELP: &str = "Usage: daisugi gate check --mode shadow|enforce [OPTIONS] < payload.json

  Read one hook payload from stdin and emit the host's verdict contract.
  On the Claude Code path a deny is exit code 2 with the reason on stderr.
  This is the command an installed hook runs.

Options:
  --mode shadow|enforce     shadow observes and logs; enforce denies.
  --root PATH               Gate state directory (default ~/.opendaisugi/gate).
  --format NAME             Host contract: claude | pi | opencode | hermes | openclaw.
  --verify-timeout SECONDS  Inner verifier budget; running out of it denies.
  --captures-root PATH      Also mirror each call into this captures directory.
  --session ID              Check against this registered session's envelope.
";

/// Runs one command and ends the process with its code.
pub fn main() -> ! {
    use std::os::unix::ffi::OsStrExt;
    let raw: Vec<std::ffi::OsString> = std::env::args_os().collect();
    // The hook entry comes first: the gate reads its own argv and
    // environment, and a refusal here would be exit 2, a deny, even in
    // shadow.
    let root_flags = ["--plain", "-q", "--quiet", "-v", "--verbose", "--no-color"];
    let mut i = 1;
    while i < raw.len() && root_flags.iter().any(|f| raw[i].as_bytes() == f.as_bytes()) {
        i += 1;
    }
    if raw.len() >= i + 2 && raw[i].as_bytes() == b"gate" && raw[i + 1].as_bytes() == b"check" {
        let rest: Vec<std::ffi::OsString> = raw[i + 2..].to_vec();
        if rest.iter().any(|a| a.as_bytes() == b"--help") {
            let _ = std::io::stdout().write_all(CHECK_HELP.as_bytes());
            let _ = std::io::stdout().flush();
            std::process::exit(0);
        }
        crate::gate::hook::main(rest, &["gate", "check"]);
    }
    let mut args = vec![];
    for a in &raw[1..] {
        match a.to_str() {
            Some(s) => args.push(s.to_string()),
            None => {
                let _ = std::io::stderr().write_all(b"daisugi: an argument is not valid UTF-8. Nothing was changed.\n");
                std::process::exit(2);
            }
        }
    }
    let mut env = HashMap::new();
    for (k, v) in std::env::vars_os() {
        if let (Some(k), Some(v)) = (k.to_str(), v.to_str()) {
            env.insert(k.to_string(), v.to_string());
        }
    }
    let mut e = Env { args: args.clone(), env, home: String::new(), out: vec![], err: vec![], tend_failed: false, raised: false };
    let code = match e.run(&args) {
        Ok(()) => 0,
        Err(Stop::Exit(c)) => c,
        Err(Stop::Usage(m)) => {
            e.errf(&format!("Error: {m}\n"));
            2
        }
    };
    e.flush();
    std::process::exit(code)
}

impl Env {
    /// `Path.home()`: $HOME as pathlib prints it.
    fn home_dir(&self) -> Result<String, String> {
        match self.env.get("HOME") {
            Some(h) if h.starts_with('/') => Ok(gateroot::path_str(h)),
            _ => Err("HOME is unset or not an absolute path".into()),
        }
    }

    fn run(&mut self, args: &[String]) -> Res {
        // Root options come before the command.
        let mut i = 0;
        while i < args.len() {
            match args[i].as_str() {
                "--version" => {
                    self.out(&format!("{VERSION}\n"));
                    return Ok(());
                }
                "--help" => return self.root_help(),
                "--plain" | "-q" | "--quiet" | "-v" | "--verbose" | "--no-color" => i += 1,
                _ => break,
            }
        }
        let args = &args[i..];
        if args.is_empty() {
            self.out(START_HERE);
            return Ok(());
        }
        if args[0].starts_with('-') {
            return self.usage("", format!("No such option: {}", args[0]));
        }
        let home = match self.home_dir() {
            Ok(h) => h,
            Err(why) => {
                let cmd = args[0].clone();
                return self.refuse(&cmd, &why);
            }
        };
        self.home = home;
        match args[0].as_str() {
            "gate" => return self.gate(&args[1..]),
            "install" => return self.install(&args[1..]),
            "pathways" => return self.pathways(&args[1..]),
            "gardener" => return self.gardener(&args[1..]),
            "tend" => return self.tend(&args[1..]),
            "hook" => return self.hook(&args[1..]),
            "distill-repeats" => return self.distill_repeats(&args[1..]),
            _ => {}
        }
        if NOT_IN_BINARY.contains(&args[0].as_str()) {
            return self.not_yet(&format!("daisugi {}", args[0]));
        }
        self.errf(&format!(
            "Usage: daisugi [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi --help' for help.\n\nError: No such command '{}'.\n",
            args[0]
        ));
        exit(2)
    }

    fn root_help(&mut self) -> Res {
        self.out(ROOT_HELP);
        let mut names: Vec<&str> = NOT_IN_BINARY.to_vec();
        names.sort();
        let mut line = " ".to_string();
        for n in names {
            if line.len() + n.len() + 2 > 78 {
                self.out(&format!("{line}\n"));
                line = " ".into();
            }
            line.push_str(&format!(" {n},"));
        }
        let line = line.strip_suffix(',').unwrap_or(&line).to_string();
        self.out(&format!("{line}\n"));
        Ok(())
    }

    fn gate(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(GATE_HELP);
            return Ok(());
        }
        let (sub, rest) = (args[0].as_str(), &args[1..]);
        match sub {
            // `gate check` after a root option or another spelling reaches
            // here only when main did not route it; it never does.
            "check" => self.not_yet("daisugi gate check in this position"),
            "init" => self.gate_init(rest),
            "register" => self.gate_register(rest),
            "disarm" => self.gate_disarm(rest),
            "arm" => self.gate_arm(rest),
            "status" => self.gate_status(rest),
            "report" => self.gate_report(rest),
            "settings" => self.gate_settings(rest),
            "proposals" => self.gate_proposals(rest),
            "replay" | "audit" | "serve" => self.not_yet(&format!("daisugi gate {sub}")),
            _ => {
                self.errf(&format!(
                    "Usage: daisugi gate [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi gate --help' for help.\n\nError: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// A command's help from its options.
    fn cmd_help(&mut self, cmd: &str, args: &str, summary: &str, opts: &[Opt]) -> Res {
        self.out(&format!("Usage: daisugi {cmd} [OPTIONS]{args}\n\n  {summary}\n\nOptions:\n"));
        for o in opts {
            let mut name = o.names.join(", ");
            if !o.neg.is_empty() {
                name.push_str(&format!(" / {}", o.neg));
            }
            if o.value {
                name.push_str(&format!(" {}", o.metavar));
            }
            self.out(&format!("  {name:<34} {}\n", o.help));
        }
        self.out(&format!("  {:<34} {}\n", "--help", "Show this message and exit."));
        Ok(())
    }
}
