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
mod configcmd;
mod dashboardcmd;
pub mod envelope;
mod autotendcmd;
mod batchcmd;
mod gardencmd;
mod hookcapcmd;
mod hookcmd;
mod generatecmd;
mod gatecmds;
mod gatewaycmd;
mod graftcmd;
mod rankcmd;
mod treecmd;
mod voicecmd;
pub mod gateroot;
pub mod install;
mod installcmd;
mod installgw;
mod installrouter;
pub mod journal;
mod journalcmd;
mod journalparse;
pub mod lprobe;
#[cfg(feature = "mujoco")]
pub mod robotprobe;
mod mcpcmd;
mod modelscmd;
mod modulescmd;
mod onboardcmd;
mod packcmd;
mod orchestratecmd;
mod pathwayscmd;
mod registrycmd;
mod releasecmd;
mod routecmd;
mod routercmd;
mod routermeasure;
mod runcmd;
mod servecmd;
mod setupcmd;
mod startcmd;
mod statuscmd;
mod tendcmd;
mod verifycmd;
mod weaveattempts;
mod weavecmd;
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
    /// The root -q: progress notes are not printed.
    quiet: bool,
    /// The root --plain: no box drawing.
    plain: bool,
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
    /// Parsed, never listed in --help.
    pub hidden: bool,
}

impl Opt {
    pub const fn flag(names: &'static [&'static str], help: &'static str) -> Opt {
        Opt { names, neg: "", value: false, multiple: false, help, metavar: "", hidden: false }
    }

    pub const fn pair(names: &'static [&'static str], neg: &'static str, help: &'static str) -> Opt {
        Opt { names, neg, value: false, multiple: false, help, metavar: "", hidden: false }
    }

    pub const fn val(names: &'static [&'static str], metavar: &'static str, help: &'static str) -> Opt {
        Opt { names, neg: "", value: true, multiple: false, help, metavar, hidden: false }
    }

    /// A flag parsed but never listed in --help.
    pub const fn hidden(names: &'static [&'static str]) -> Opt {
        Opt { names, neg: "", value: false, multiple: false, help: "", metavar: "", hidden: true }
    }

    pub const fn many(names: &'static [&'static str], metavar: &'static str, help: &'static str) -> Opt {
        Opt { names, neg: "", value: true, multiple: true, help, metavar, hidden: false }
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
            None => {
                let mut msg = format!("No such option: {name}");
                if name.starts_with("--") {
                    let mut close = close_matches(&name, &long_names(opts));
                    if !close.is_empty() {
                        close.sort();
                        msg.push_str(&format!(" (Possible options: {})", close.join(", ")));
                    }
                }
                return Err(msg);
            }
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

/// click's `_long_opt`: every long option name, --help too.
fn long_names(opts: &[Opt]) -> Vec<String> {
    let mut out = vec!["--help".to_string()];
    for o in opts {
        out.extend(o.names.iter().filter(|n| n.starts_with("--")).map(|n| n.to_string()));
        if o.neg.starts_with("--") {
            out.push(o.neg.to_string());
        }
    }
    out
}

/// `difflib.get_close_matches(word, possibilities)`: at most three, each
/// with a ratio of 0.6 or more, best first.
pub(super) fn close_matches(word: &str, possibilities: &[String]) -> Vec<String> {
    let a: Vec<char> = word.chars().collect();
    let mut hits: Vec<(f64, &String)> = possibilities
        .iter()
        .filter_map(|x| {
            let b: Vec<char> = x.chars().collect();
            let r = seq_ratio(&b, &a);
            (r >= 0.6).then_some((r, x))
        })
        .collect();
    // heapq.nlargest(3, (score, x)): by score, then by x, both descending.
    hits.sort_by(|p, q| q.0.partial_cmp(&p.0).unwrap_or(std::cmp::Ordering::Equal).then_with(|| q.1.cmp(p.1)));
    hits.into_iter().take(3).map(|(_, x)| x.clone()).collect()
}

/// `difflib.SequenceMatcher(None, a, b).ratio()` for short sequences (no
/// junk, no autojunk): 2*M/T over the matching blocks.
pub(super) fn seq_ratio(a: &[char], b: &[char]) -> f64 {
    let t = a.len() + b.len();
    if t == 0 {
        return 1.0;
    }
    2.0 * matched(a, 0, a.len(), b, 0, b.len()) as f64 / t as f64
}

/// The matching blocks of a[alo:ahi] and b[blo:bhi], summed: the longest
/// match (first in a, then first in b), then each side of it.
fn matched(a: &[char], alo: usize, ahi: usize, b: &[char], blo: usize, bhi: usize) -> usize {
    let (mut bi, mut bj, mut bk) = (alo, blo, 0);
    for i in alo..ahi {
        for j in blo..bhi {
            let mut k = 0;
            while i + k < ahi && j + k < bhi && a[i + k] == b[j + k] {
                k += 1;
            }
            if k > bk {
                (bi, bj, bk) = (i, j, k);
            }
        }
    }
    if bk == 0 {
        return 0;
    }
    bk + matched(a, alo, bi, b, blo, bj) + matched(a, bi + bk, ahi, b, bj + bk, bhi)
}

const START_HERE: &str = "daisugi: a gate that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          gate this directory (audit mode) and watch it
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
    "help", "bench",
    "conformance", "coppice",
    "lora export", "models pin",
    "viz", "gate replay", "gate audit",
];

const ROOT_HELP: &str = "Usage: daisugi [OPTIONS] COMMAND [ARGS]...

  Runtime assurance for agent actions. This is the daisugi Rust binary:
  the everyday commands, the gate commands, the gate and gateway halves of
  install, the pathway store, the journal, the garden, the gateway, the
  registry, batch proofs and release signing.
  gate check answers every call natively.

Options:
  --version        Show the opendaisugi version and exit.
  --plain          No color, no box drawing; greppable output.
  -q, --quiet      Results only; no progress notes.
  -v, --verbose    Show tracebacks and detail.
  --no-color       Disable color (same as NO_COLOR=1).
  --help           Show this message and exit.

Start here:
  start            Gate this directory (audit by default) and open the live view.
  status           Show day-one readiness: are token savings live and are actions verified?
  config           Show every setting as daisugi will use it, and where each one came from.
  dashboard        The live floor: the module map with gauges read from the stores.

View:
  modules          Show the module wiring: what is active, available, or an open slot.
  metrics          Prometheus metrics: the exposition text, or a /metrics endpoint.

Gate:
  gate check       Answer one tool-call hook: the verdict for the payload on stdin.
  gate init        Register a reviewable starter envelope for this session.
  gate register    Register the envelope the gate checks this session's calls against.
  gate arm         Remove the disarm marker; the gate resumes evaluating calls.
  gate disarm      Kill switch: the gate allows everything until re-armed.
  gate status      Show armed/disarmed state, the verdict mode, and the envelopes.
  gate report      Summarize the audit log: what an enforcing gate would have denied.
  gate settings    Print the Claude Code hooks-settings JSON that wires in the gate.
  gate proposals   List recorded envelope-edit proposals.
  gate serve       Run the resident gate in the foreground (Ctrl-C to stop).
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

Gateway:
  gateway          Run the token-saving gateway: a local proxy any harness points at.
  gateway-report   Calibrate the gateway on a recorded day: routing and reuse.
  router status    Show the router choice, the Switchyard binary and each child.
  router stop      Stop every switchyard-server a gateway left running.
  route            Recommend the cheapest viable model or tier for a task.

Envelopes:
  generate-envelope  Generate a safety envelope for a task from a model.

Run:
  orchestrate      Run a prompt end to end: decompose, size, supervised execute, synthesize.
  run              Execute a plan against an envelope under runtime supervision.
  verify           Verify an action plan against a safety envelope.

Setup:
  onboard          Turn existing agent transcripts into verified traces and pathways.
  tiers setup      Detect hardware, recommend a local model, and optionally qualify and wire it.
  models list      List the curated models, the default for this box, and the one in use.
  models search    Search the Hugging Face API for models, filtered by size and license.
  models use       Record the model the garden uses. Any id, path or GGUF file is accepted.
  pack list        List the ML packs and which are installed.
  pack install     Install a pack: a pinned Python, a venv, the locked wheels.
  pack run         Run one job (selftest, train, vla-chunk) in a pack's worker.
  lora train       Train a LoRA adapter in the train pack.
  mcp serve        Serve the openDaisugi tools over MCP stdio.

Registry and release:
  registry init    Clone a registry repo to a local directory.
  registry pull    git pull and materialize new pathway bundles into the local cache.
  registry publish Sign + commit + push a local pathway as a bundle to the registry.
  registry status  Show the local clone's diagnostic info.
  registry pull-and-tend  Pull new bundles from the registry, then run tend.
  batch prove      Statically prove a declared batch before any iteration.
  release keygen   Generate an ed25519 release-signing keypair.
  release sign     Build a SHA-256 manifest over ARTIFACTS and sign it.
  release verify   Verify a release: trusted signature AND intact artifacts.

Journal:
  journal search   Search journal traces by task (the lexical and potion matchers).
  journal replay   Re-run verify() on a stored trace and report drift.
  journal stats    Print aggregate stats from the journal index.
  journal parse    Parse an agent transcript into episodes.
  journal ingest   Ingest parsed episodes into the journal.

Not yet in this binary (use the Python CLI, opendaisugi.cli):
";

const GATE_HELP: &str = "Usage: daisugi gate [OPTIONS] COMMAND [ARGS]...

  Call-time tool gate (ADR-0007): verify each live tool call against a
  registered envelope. Audit by default; --mode enforce denies.

Commands:
  check      Answer one hook payload on stdin with the host's verdict.
  init       Generate and register a reviewable starter envelope.
  register   Register the envelope the gate checks calls against.
  disarm     Kill switch: the gate allows everything until re-armed.
  arm        Remove the disarm marker; the gate resumes evaluating calls.
  status     Show armed state, the verdict mode, and the envelopes.
  report     Summarize the audit log.
  settings   Print the Claude Code hooks-settings JSON for the gate.
  proposals  List recorded envelope-edit proposals.
  serve      Run the resident gate in the foreground (Ctrl-C to stop).

Not yet in this binary: replay, audit.
";

const CHECK_HELP: &str = "Usage: daisugi gate check --mode audit|enforce [OPTIONS] < payload.json

  Read one hook payload from stdin and emit the host's verdict contract.
  On the Claude Code path a deny is exit code 2 with the reason on stderr.
  This is the command an installed hook runs.

Options:
  --mode audit|enforce      audit observes and logs; enforce denies.
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
    // audit.
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
    let mut e = Env { args: args.clone(), env, home: String::new(), out: vec![], err: vec![], tend_failed: false, raised: false, quiet: false, plain: false };
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
                "-q" | "--quiet" => {
                    self.quiet = true;
                    i += 1;
                }
                "--plain" => {
                    self.plain = true;
                    i += 1;
                }
                "-v" | "--verbose" | "--no-color" => i += 1,
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
        // Every verify this command runs asks a model for an llm_check, as
        // the oracle's evaluator does.
        crate::gate::llm::set_command_check(crate::gate::llm::CommandCheck {
            env: self.env.clone(),
            home: self.home.clone(),
        });
        match args[0].as_str() {
            "gate" => return self.gate(&args[1..]),
            "install" => return self.install(&args[1..]),
            "pathways" => return self.pathways(&args[1..]),
            "gardener" => return self.gardener(&args[1..]),
            "tend" => return self.tend(&args[1..]),
            "hook" => return self.hook(&args[1..]),
            "distill-repeats" => return self.distill_repeats(&args[1..]),
            "gateway" => return self.gateway_cmd(&args[1..]),
            "gateway-report" => return self.gateway_report(&args[1..]),
            "router" => return self.router(&args[1..]),
            "graft" => return self.graft(&args[1..]),
            "rank" => return self.rank_cmd(&args[1..]),
            "tree" => return self.tree_cmd(&args[1..]),
            "route" => return self.route(&args[1..]),
            "config" => return self.config_cmd(&args[1..]),
            "status" => return self.status_cmd(&args[1..]),
            "start" => return self.start(&args[1..]),
            "journal" => return self.journal(&args[1..]),
            "generate-envelope" => return self.generate_envelope(&args[1..]),
            "run" => return self.run_cmd(&args[1..]),
            "verify" => return self.verify_cmd(&args[1..]),
            "weave" => return self.weave_cmd(&args[1..]),
            "orchestrate" => return self.orchestrate_cmd(&args[1..]),
            "mcp" => return self.mcp_cmd(&args[1..]),
            "onboard" => return self.onboard(&args[1..]),
            "tiers" => return self.tiers(&args[1..]),
            "models" => return self.models(&args[1..]),
            "pack" => return self.pack(&args[1..]),
            "lora" => return self.lora(&args[1..]),
            "setup" => return self.setup_moved(&args[1..]),
            "modules" => return self.modules_cmd(&args[1..]),
            "dashboard" => return self.dashboard_cmd(&args[1..]),
            "metrics" => return self.metrics_cmd(&args[1..]),
            "registry" => return self.registry_cmd(&args[1..]),
            "batch" => return self.batch_cmd(&args[1..]),
            "release" => return self.release_cmd(&args[1..]),
            "voice" => return self.voice_cmd(&args[1..]),
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
            "serve" => self.gate_serve(rest),
            "replay" | "audit" => self.not_yet(&format!("daisugi gate {sub}")),
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
        for o in opts.iter().filter(|o| !o.hidden) {
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
