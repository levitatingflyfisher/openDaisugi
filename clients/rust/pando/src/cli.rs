//! daisugi pando: the verbs over a store. The twin of Go's pando cli.go.
//! The crate opens no socket; its one outside call is the summarizer, which
//! the caller builds.

use std::io::{BufRead, Write};
use std::path::{Path, PathBuf};
use std::time::SystemTime;

use super::child::{child_dir, list_children, redo_children, tend_children_calls};
use super::chat::{flat, redo_end, Chat, Clock, Config, RedoSet, Summarizer, ERR_LOCKED, KIND_NOTE, KIND_USER};
use super::ctx::Ctx;
use super::fsutil;
use super::import::{
    accept, describe, plural, read_import_state, scan, totals, ImportOpts, ImportSession, Importer,
    SRC_WEB,
};
use super::import_web::{parse_map, read_proposal, scan_web, write_proposal, PathMap};
use super::migrate::migrate;
use super::recall::{format_hit, Embed};
use super::branch::{
    add_route, branch_lines, branch_rows, branch_tree, close_branch, has_main, recall_tree, run_branch_hook, scope_of,
    sub_stores, SubStore, BRANCH_VAR,
};
use super::store::{list_stores, store_dir, DEFAULT_STORE};
use super::tend::{redo_calls, tend_calls, TendRun, ERR_ALL_CLIPPED};

/// Builds the model behind tend and import. The profile is the agent
/// profile's name, or "" for the default.
pub type NewSummarizer<'a> = &'a dyn Fn(&str) -> Result<Box<dyn Summarizer>, String>;

/// What a command line needs from the process around it.
pub struct CliEnv<'a> {
    pub stdin: &'a mut dyn BufRead,
    pub out: &'a mut dyn Write,
    pub err: &'a mut dyn Write,
    /// The data home (the daisugi rule), where pando/<store> lives, unless
    /// --data-dir names another.
    pub data_home: PathBuf,
    /// $HOME, for the transcripts the importer reads.
    pub home: PathBuf,
    pub new_summarizer: Option<NewSummarizer<'a>>,
    /// The clock. None is the system clock.
    pub now: Option<Clock>,
    /// The matcher behind recall (PD-R-5).
    pub embed: Embed,
    /// Where tend prints one line for each node built, live; None prints
    /// none. The binary gives the process's stderr.
    pub progress: Option<super::tend::ProgressSink>,
    /// Builds the source that reads the Hermes profiles; "" names all of
    /// them. None means this build cannot read Hermes.
    pub hermes_source: Option<&'a dyn Fn(&str) -> Result<Box<dyn super::live::Source>, String>>,
    /// Reads the process environment (DAISUGI_PANDO_PROFILE).
    pub getenv: Option<&'a dyn Fn(&str) -> String>,
}

impl CliEnv<'_> {
    fn now(&self) -> SystemTime {
        match &self.now {
            Some(f) => f(),
            None => SystemTime::now(),
        }
    }
}

/// The help of daisugi pando.
pub const USAGE: &str = "Usage: daisugi pando [OPTIONS] COMMAND [ARGS]...

  Pando R2, the conversation memory: a verbatim log, a summary tree and a
  view, in one store on disk. A store is a dir, <data home>/pando/<store>.
  Stores are separate on purpose: a command names one, never two.

Options:
  --store NAME     The store: work (the default) or home, or any a-z0-9- name.
  --data-dir DIR   The data home that holds pando/<store>.
  --json           Print JSON where the command has a list or a record.
  --branch PATH    Read inside a branch of the store (trellis, trellis/ui).
  -q, --quiet      With tend: do not print a line for each node built.
  --help           Show this message and exit.

Commands:
  stores   List the stores.
  status   Show a store: messages, nodes to build, the view's size.
  log      Print the messages, one per line. --day YYYY-MM-DD for one day.
  zoom     Open a node: ID N. N=1 is the message word for word.
  date     Print when message ID was logged, in UTC.
  view     Print the view: the summary lines a turn would see.
  recall   Find where a word was said: QUERY [-k N]. It gives ids, then zoom.
  append   Add a message: TEXT [--kind user|note]. \"-\" reads standard input.
  tend     Build the summary nodes that are not built yet [--profile NAME]
           [--max N] [--yes] [--dry-run] [-q]. With no --profile it runs Claude
           Code (paid); over 25 nodes it asks for --profile or --yes first.
           --max caps the model calls of the run, a re-ask for a shorter line
           included. Each node built prints one line to stderr; -q hides them.
           --redo-clipped builds again the nodes that hold a clipped copy of
           their source (the model gave no text), and the nodes above them.
           --redo-level N builds again every node at level N and above; with
           both, the run does the union. Both take --profile, --max, --yes
           and --dry-run, and they ask first when they may make over 25 calls.
  import   Bring old chats in: --from claude|codex|web|hermes|all [--dir PATH]
           (all is claude and codex; hermes is read on its own)
           [--web PATH] [--since DATE] [--tools] [--map OLD=NEW] [--dry-run]
           [--propose FILE | --accept FILE] [--tend [--profile NAME]
           [--max N] [--yes]]. --tend builds the summary nodes at once.
           --dir reads Claude Code chats from PATH, not ~/.claude/projects.
           --from hermes logs the Hermes chats now, with no queue: --hermes-profile
           NAME for one profile, --all (or neither) for all; it only reads state.db.
  migrate  Move the old coppice steward log into pando/work [--from DIR].
  serve    Run a read-only MCP server named pando on stdin and stdout.
  context  Print the view with a short note on the tools, as a chat's first
           turn gets it. It prints nothing when the store holds no message.
  hook     A Claude Code hook: reads the event on stdin. SessionStart prints the
           view as context; UserPromptSubmit and Stop log the new messages of the
           transcript. It never fails the chat: errors go to <store>/hook.log.
  profile  Show or set the agent profile that builds nodes in the background:
           [NAME | none]. It is kept in <data home>/pando/profile; the
           DAISUGI_PANDO_PROFILE variable overrides it.
  branches List the branches of the store: long-running agents' parts of the
           tree, each with its state and newest summary.
  close    Close a branch: BRANCH (a path such as trellis or trellis/ui). Its
           messages stay and recall finds them; the view above no longer names it.
  routes   List the session map: which sessions and dirs belong to which branch.
  unroute  Take a session (or dir:PATH) off the session map: ID. Its next lines
           go to the store again; lines already logged stay where they are.
  follow   Keep the store live: log new Claude Code and Hermes messages as they
           come and build the summary nodes in the background [--profile NAME]
           [--interval SECONDS] [--once] [--claude-dir DIR] [--hermes-profile NAME | --all].
           With no profile (flag or pando.profile) it only logs, and says once that
           the nodes wait. A model outage delays a node and never clips it.
";

#[derive(Default)]
struct Flags {
    store: String,
    data_dir: String,
    day: String,
    kind: String,
    profile: String,
    from: String,
    since: String,
    dir: String,
    web: String,
    propose: String,
    accept: String,
    child: String,
    branch: String,
    session: String,
    mig_from: String,
    offset: i64,
    size: i64,
    k: i64,
    max: usize,
    as_json: bool,
    dry: bool,
    tools: bool,
    tend: bool,
    yes: bool,
    quiet: bool,
    once: bool,
    all: bool,
    hermes_profile: String,
    claude_dir: String,
    interval: i64,
    redo: bool,
    redo_level: Option<i64>,
    help: bool,
    maps: Vec<String>,
    words: Vec<String>,
}

macro_rules! wl {
    ($w:expr, $($a:tt)*) => {{ let _ = writeln!($w, $($a)*); }};
}

fn usage_err(env: &mut CliEnv, msg: &str) -> i32 {
    wl!(env.err, "{msg}");
    wl!(env.err, "Try 'daisugi pando --help' for help.");
    2
}

/// Runs daisugi pando with the words after "pando". It returns the exit
/// code: 0 done, 1 a store or id that is not there or a failed step, 2
/// usage.
pub fn run(argv: &[String], env: &mut CliEnv) -> i32 {
    let mut f = Flags {
        store: DEFAULT_STORE.into(),
        k: 8,
        ..Default::default()
    };
    let mut i = 0;
    while i < argv.len() {
        let a = argv[i].as_str();
        let val = |i: &mut usize| -> Option<String> {
            if *i + 1 >= argv.len() {
                return None;
            }
            *i += 1;
            Some(argv[*i].clone())
        };
        macro_rules! need {
            ($dst:expr) => {
                match val(&mut i) {
                    Some(v) => $dst = v,
                    None => {
                        wl!(env.err, "{a} needs a value");
                        return 2;
                    }
                }
            };
        }
        macro_rules! need_int {
            () => {
                match val(&mut i) {
                    None => {
                        wl!(env.err, "{a} needs a number");
                        return 2;
                    }
                    Some(v) => match v.parse::<i64>() {
                        Ok(n) if n >= 0 => n,
                        _ => {
                            wl!(env.err, "{a} needs a number, not {}", crate::sys::go_quote(&v));
                            return 2;
                        }
                    },
                }
            };
        }
        match a {
            "--store" => need!(f.store),
            "--data-dir" => need!(f.data_dir),
            "--day" => need!(f.day),
            "--kind" => need!(f.kind),
            "--profile" => need!(f.profile),
            "--from" => {
                need!(f.from);
                f.mig_from = f.from.clone();
            }
            "--since" => need!(f.since),
            "--web" => need!(f.web),
            "--propose" => need!(f.propose),
            "--accept" => need!(f.accept),
            "--child" => need!(f.child),
            "--branch" => need!(f.branch),
            "--session" => need!(f.session),
            "--hermes-profile" => need!(f.hermes_profile),
            "--claude-dir" => need!(f.claude_dir),
            "--interval" => f.interval = need_int!(),
            "--once" => f.once = true,
            "--all" => f.all = true,
            "--dir" => need!(f.dir),
            "--map" => {
                let m;
                need!(m);
                f.maps.push(m);
            }
            "--offset" => f.offset = need_int!(),
            "--size" => f.size = need_int!(),
            "-k" => f.k = need_int!(),
            "--max" => f.max = need_int!() as usize,
            "--json" => f.as_json = true,
            "--dry-run" => f.dry = true,
            "--tools" => f.tools = true,
            "--tend" => f.tend = true,
            "--redo-clipped" => f.redo = true,
            "--redo-level" => f.redo_level = Some(need_int!()),
            "--yes" | "-y" => f.yes = true,
            "--quiet" | "-q" => f.quiet = true,
            "--help" | "-h" => f.help = true,
            _ => {
                if a.starts_with("--") {
                    return usage_err(env, &format!("No such option: {a}"));
                }
                f.words.push(a.to_string());
            }
        }
        i += 1;
    }
    if f.help || f.words.is_empty() {
        let _ = write!(env.out, "{USAGE}");
        return if f.help { 0 } else { 2 };
    }
    let verb = f.words[0].clone();
    let args: Vec<String> = f.words[1..].to_vec();
    let arity: (usize, Option<usize>) = match verb.as_str() {
        "stores" | "status" | "log" | "view" | "tend" | "import" | "migrate" | "serve" | "hook" | "follow"
        | "context" => (0, Some(0)),
        "profile" => (0, Some(1)),
        "zoom" => (2, Some(2)),
        "date" | "recall" | "close" | "unroute" => (1, Some(1)),
        "branches" | "routes" => (0, Some(0)),
        "append" => (1, None),
        _ => return usage_err(env, &format!("No such command '{verb}'.")),
    };
    if args.len() < arity.0 || arity.1.is_some_and(|m| args.len() > m) {
        return usage_err(
            env,
            &format!("daisugi pando {verb} takes {} argument(s).", arity.0),
        );
    }
    if f.all && verb != "follow" && !(verb == "import" && f.from == "hermes") {
        return usage_err(env, "--all goes with follow or import --from hermes");
    }
    if f.all && !f.hermes_profile.is_empty() {
        return usage_err(env, "--all and --hermes-profile exclude each other");
    }
    if f.redo && verb != "tend" {
        return usage_err(env, "--redo-clipped goes with tend");
    }
    if f.redo_level.is_some() && verb != "tend" {
        return usage_err(env, "--redo-level goes with tend");
    }
    let data = if f.data_dir.is_empty() {
        env.data_home.clone()
    } else {
        PathBuf::from(&f.data_dir)
    };
    let raw_mark = env.getenv.map(|g| g(BRANCH_VAR)).unwrap_or_default();
    let branch_mark = raw_mark.trim().to_string();
    let marked = !branch_mark.is_empty() || super::live::skip_on(env.getenv);
    if verb == "stores" {
        if marked {
            wl!(env.err, "{}", marked_refusal(&raw_mark, &verb));
            return 1;
        }
        return cli_stores(env, &data, &f);
    }
    if verb == "profile" {
        return cli_profile(env, &data, &args);
    }
    let dir = match store_dir(&data, &f.store) {
        Ok(d) => d,
        Err(e) => {
            if verb == "hook" {
                return 0; // a hook never fails the chat
            }
            return usage_err(env, &e.to_string());
        }
    };
    if verb == "hook" {
        // The event is read first in every case: the parent writes it to our
        // standard input and must not meet a closed pipe.
        let mut input = String::new();
        let _ = env.stdin.read_to_string(&mut input);
        let ev = super::live::read_hook_event(&input);
        if super::live::skip_on(env.getenv) {
            // Not the owner's chat: no view, nothing logged. The session
            // goes on the skip list, so the follow service and the importer
            // leave its transcript alone.
            // A spawner that names its own list (DAISUGI_PANDO_SKIPLIST, as
            // coppice does under a --data-dir of its own) is listed there too.
            let list = super::live::skip_list_path(&dir);
            super::live::skip_session(&list, &ev);
            if let Some(g) = env.getenv {
                let named = g(super::live::SKIP_LIST_VAR);
                if !named.is_empty() && std::path::Path::new(&named) != list {
                    super::live::skip_session(std::path::Path::new(&named), &ev);
                }
            }
            return 0;
        }
        let skip = super::live::skip_dirs(&dir, &env.home, env.getenv);
        let out = if !branch_mark.is_empty() {
            // A long-running agent: its view is its branch, and what it says
            // goes into its branch, never into the store above.
            run_branch_hook(&dir, &branch_mark, &ev, &skip, env.now.clone())
        } else {
            // The owner's own chat claims its session for the store at its
            // start, before any branch can map it (G5).
            if ev.event == "SessionStart" {
                let _ = super::branch::claim_root(&dir, &[&ev.session_id, &ev.transcript_path]);
            }
            super::live::run_hook(&dir, &ev, &skip, env.now.clone())
        };
        let _ = write!(env.out, "{out}");
        return 0;
    }
    let sc = scope_of(&dir, env.getenv);
    if verb == "serve" && sc.closed {
        // A worker with no branch, or a branch that cannot be read: the
        // server lists no tool and does not open the store.
        if let Err(e) = super::mcp::serve_scope(&sc, env.embed, env.stdin, env.out) {
            wl!(env.err, "{e}");
            return 1;
        }
        return 0;
    }
    if marked {
        // A process in a branch reads its branch and below, and writes
        // nothing by hand: its hooks and the follow service log it.
        if matches!(
            verb.as_str(),
            "stores" | "migrate" | "append" | "tend" | "import" | "follow" | "profile" | "close" | "routes" | "unroute"
        ) {
            wl!(env.err, "{}", marked_refusal(&branch_mark, &verb));
            return 1;
        }
        if verb == "context" && (sc.closed || sc.worker) {
            return 0; // no view for a worker, and never the store above
        }
        if sc.closed {
            wl!(env.err, "{}", sc.why);
            return 1;
        }
        if verb == "context" && !f.session.is_empty() && !sc.worker {
            // The Hermes plugin names its session: the follow service then
            // logs its rows into this branch.
            let key = format!("hermes:{}", f.session);
            let _ = add_route(&dir, &sc.branch, &[&key]);
        }
    }
    if verb == "routes" || verb == "unroute" {
        return cli_routes(env, &dir, &f, &verb, &args);
    }
    if verb != "migrate" && verb != "append" && verb != "import" && verb != "follow" && verb != "close" && !dir.is_dir() {
        wl!(env.err, "no store {} at {}", f.store, dir.display());
        return 1;
    }
    if verb == "close" {
        if let Err(e) = close_branch(&dir, &args[0], &env.now) {
            wl!(env.err, "{e}");
            return 1;
        }
        wl!(env.out, "closed: {}", args[0]);
        return 0;
    }
    match verb.as_str() {
        "migrate" => return cli_migrate(env, &data, &f),
        "append" => return cli_append(env, &dir, &f, &args),
        "tend" if f.redo || f.redo_level.is_some() => return cli_redo(env, &dir, &f),
        "tend" => return cli_tend(env, &dir, &f),
        "import" if f.from == "hermes" => return cli_import_hermes(env, &dir, &f),
        "import" => return cli_import(env, &data, &dir, &f),
        "follow" => return cli_follow(env, &data, &dir, &f),
        _ => {}
    }
    let mut root = match sc.below(&f.branch) {
        Ok(d) => d,
        Err(e) => {
            if !f.branch.is_empty() {
                return usage_err(env, &e);
            }
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if !f.child.is_empty() {
        root = match child_dir(&root, &f.child) {
            Ok(d) => d,
            Err(e) => return usage_err(env, &e),
        };
        if !root.is_dir() {
            wl!(env.err, "no child {} in store {}", f.child, f.store);
            return 1;
        }
    }
    if verb == "branches" {
        return cli_branches(env, &root, &f);
    }
    if verb == "context" && f.child.is_empty() {
        // The view text needs the view, the tree and the last messages only.
        match super::live::view_context_of(&root) {
            Ok(text) => {
                let _ = write!(env.out, "{text}");
                return 0;
            }
            Err(e) => {
                if !has_main(&root) {
                    let _ = write!(env.out, "{}", branch_lines(&root));
                    return 0;
                }
                wl!(env.err, "{e}");
                return 1;
            }
        }
    }
    if verb == "serve" {
        let mut sc = sc.clone();
        sc.dir = root.clone();
        if let Err(e) = super::mcp::serve_scope(&sc, env.embed, env.stdin, env.out) {
            wl!(env.err, "{e}");
            return 1;
        }
        return 0;
    }
    let c = match Chat::open_read_only(&root) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if !c.notice().is_empty() {
        wl!(env.err, "note: {}", c.notice());
    }
    cli_read(env, &c, &root, &f, &verb, &args)
}

/// A string as Go writes it with HTML escaping off.
fn js(s: &str) -> String {
    fsutil::json_str(s)
}

fn print_json(env: &mut CliEnv, line: String) -> i32 {
    let _ = writeln!(env.out, "{line}");
    0
}

fn cli_stores(env: &mut CliEnv, data: &Path, f: &Flags) -> i32 {
    let names = match list_stores(data) {
        Ok(n) => n,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if f.as_json {
        let items: Vec<String> = names.iter().map(|n| js(n)).collect();
        return print_json(env, format!("[{}]", items.join(",")));
    }
    for n in names {
        wl!(env.out, "{n}");
    }
    0
}

fn cli_read(env: &mut CliEnv, c: &Chat, dir: &Path, f: &Flags, verb: &str, args: &[String]) -> i32 {
    match verb {
        "status" => {
            if f.as_json {
                let mut s = String::from("{");
                if !f.child.is_empty() {
                    s.push_str(&format!("\"child\":{},", js(&f.child)));
                }
                s.push_str(&format!(
                    "\"clipped\":{},\"messages\":{},\"not_in_view\":{},\"notice\":{},\"path\":{},\"pending\":{},\"store\":{},\"view_bytes\":{}}}",
                    c.clipped_count(),
                    c.count(),
                    c.not_in_view(),
                    js(c.notice()),
                    js(&dir.to_string_lossy()),
                    c.pending(),
                    js(&f.store),
                    c.view_size()
                ));
                return print_json(env, s);
            }
            let _ = write!(
                env.out,
                "store: {}\npath: {}\nmessages: {}\npending nodes: {}\nnot in the view yet: {}\nview bytes: {}\nclipped nodes: {}\n",
                f.store,
                dir.display(),
                c.count(),
                c.pending(),
                c.not_in_view(),
                c.view_size(),
                c.clipped_count()
            );
            if f.child.is_empty() {
                wl!(env.out, "children: {}", list_children(dir).len());
                let n = branch_tree(dir).len();
                if n > 0 {
                    wl!(env.out, "branches: {n}");
                }
            }
            if !c.notice().is_empty() {
                wl!(env.out, "notice: {}", c.notice());
            }
        }
        "log" => {
            let mut msgs = Vec::new();
            if !f.day.is_empty() {
                match c.day(&f.day) {
                    Ok(m) => msgs = m,
                    Err(e) => {
                        wl!(env.err, "{e}");
                        return 1;
                    }
                }
            } else {
                for i in 0..c.count() {
                    match c.message(i) {
                        Ok(m) => msgs.push(m),
                        Err(e) => {
                            wl!(env.err, "{e}");
                            return 1;
                        }
                    }
                }
            }
            for m in msgs {
                if f.as_json {
                    let _ = write!(env.out, "{}", m.encode());
                    continue;
                }
                wl!(env.out, "{}\t{}\t{}\t{}", m.i, m.kind, m.date, flat(&m.text));
            }
        }
        "view" => {
            let ents: Vec<(String, String)> = c
                .view_lines()
                .into_iter()
                .map(|id| {
                    let text = c.node(id).map_or(super::chat::UNBUILT, |n| n.text.as_str());
                    (id.name(), flat(text))
                })
                .collect();
            if f.as_json {
                let rows: Vec<String> = ents
                    .iter()
                    .map(|(i, t)| format!("{{\"id\":{},\"text\":{}}}", js(i), js(t)))
                    .collect();
                return print_json(env, format!("[{}]", rows.join(",")));
            }
            for (i, t) in ents {
                wl!(env.out, "{i}\t{t}");
            }
            // The branches below, as context, the hook and the view tool give them.
            if f.child.is_empty() {
                let _ = write!(env.out, "{}", branch_lines(dir));
            }
        }
        "zoom" => {
            let (e1, e2) = (args[0].parse::<i64>(), args[1].parse::<i64>());
            let (Ok(id), Ok(n)) = (e1, e2) else {
                return usage_err(
                    env,
                    &format!(
                        "zoom needs two numbers, ID and N, not {} {}",
                        crate::sys::go_quote(&args[0]),
                        crate::sys::go_quote(&args[1])
                    ),
                );
            };
            let text = match c.zoom_page(id, n, f.offset, f.size) {
                Ok(t) => t,
                Err(e) => {
                    wl!(env.err, "{e}");
                    return 1;
                }
            };
            if f.as_json {
                return print_json(env, format!("{{\"text\":{}}}", js(&text)));
            }
            let _ = write!(env.out, "{text}");
            if !text.ends_with('\n') {
                wl!(env.out, "");
            }
        }
        "date" => {
            let Ok(id) = args[0].parse::<i64>() else {
                return usage_err(
                    env,
                    &format!("date needs a message id, not {}", crate::sys::go_quote(&args[0])),
                );
            };
            let d = match c.date(id) {
                Ok(d) => d,
                Err(e) => {
                    wl!(env.err, "{e}");
                    return 1;
                }
            };
            if f.as_json {
                return print_json(env, format!("{{\"date\":{}}}", js(&d)));
            }
            wl!(env.out, "{d}");
        }
        "recall" => {
            let hits = if f.child.is_empty() {
                match recall_tree(dir, &args[0], f.k as usize, env.embed) {
                    Ok(h) => h,
                    Err(e) => {
                        wl!(env.err, "{e}");
                        return 1;
                    }
                }
            } else {
                c.recall(&args[0], f.k as usize, env.embed)
            };
            if f.as_json {
                let rows: Vec<String> = hits
                    .iter()
                    .map(|h| {
                        format!(
                            "{{\"id\":{},\"score\":{},\"line\":{}}}",
                            js(&h.id),
                            go_float(h.score),
                            js(&h.line)
                        )
                    })
                    .collect();
                return print_json(env, format!("[{}]", rows.join(",")));
            }
            for h in &hits {
                wl!(env.out, "{}", format_hit(h));
            }
        }
        "context" => {
            if !super::live::skip_on(env.getenv) {
                let _ = write!(env.out, "{}", c.view_context());
            }
        }
        _ => {}
    }
    0
}

/// A float as Go's encoder writes it: the shortest digits, an exponent only
/// below 1e-6 or from 1e21.
fn go_float(f: f64) -> String {
    let abs = f.abs();
    if abs != 0.0 && (abs < 1e-6 || abs >= 1e21) {
        let s = format!("{:e}", f);
        // 1e-7 -> 1e-07
        if let Some((m, e)) = s.split_once('e') {
            let (sign, d) = match e.strip_prefix('-') {
                Some(d) => ("-", d),
                None => ("+", e),
            };
            return format!("{m}e{sign}{:0>2}", d);
        }
        return s;
    }
    let s = format!("{f}");
    s
}

fn cli_append(env: &mut CliEnv, dir: &Path, f: &Flags, args: &[String]) -> i32 {
    let kind = if f.kind.is_empty() { KIND_USER } else { f.kind.as_str() };
    if kind != KIND_USER && kind != KIND_NOTE {
        return usage_err(
            env,
            &format!("--kind takes user or note, not {}", crate::sys::go_quote(kind)),
        );
    }
    let mut text = args.join(" ");
    if text == "-" {
        let mut b = Vec::new();
        if let Err(e) = env.stdin.read_to_end(&mut b) {
            wl!(env.err, "{e}");
            return 1;
        }
        text = String::from_utf8_lossy(&b).trim_end_matches('\n').to_string();
    }
    if text.trim().is_empty() {
        return usage_err(env, "append needs some text");
    }
    let mut c = match Chat::open(Config {
        dir: dir.to_path_buf(),
        now: env.now.clone(),
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if !c.notice().is_empty() {
        wl!(env.err, "note: {}", c.notice());
    }
    let (ids, err) = c.append(kind, &text);
    c.close();
    if let Some(e) = err {
        wl!(env.err, "{e}");
        return 1;
    }
    if f.as_json {
        let items: Vec<String> = ids.iter().map(|i| i.to_string()).collect();
        return print_json(env, format!("[{}]", items.join(",")));
    }
    for id in ids {
        wl!(env.out, "{id}");
    }
    0
}

/// How many nodes tend builds with the default model, Claude Code, before it
/// asks for --profile or --yes.
const PAID_TEND_LIMIT: usize = 25;

/// The default model runs on Claude Code and is paid per call.
fn paid_default(profile: &str) -> bool {
    profile.is_empty() || profile == "default"
}

/// The text that stops a tend that would make more than PAID_TEND_LIMIT paid
/// calls, or None when it may go on. max caps the calls (0 is no cap), so a
/// run bounded by --max is its own limit.
fn paid_tend_refusal(pending: usize, f: &Flags) -> Option<String> {
    let calls = if f.max > 0 && f.max < pending { f.max } else { pending };
    if f.yes || !paid_default(&f.profile) || calls <= PAID_TEND_LIMIT {
        return None;
    }
    Some(format!(
        "The default model is Claude Code (paid), and this run makes at most {calls} model calls. Nothing was built.\n\
         Run again with --profile NAME to use an agent profile, or --yes to accept the default.\n"
    ))
}

/// The nodes to build in a store and its children.
fn pending_all(dir: &Path) -> Result<usize, String> {
    let mut roots = vec![dir.to_path_buf()];
    for ss in sub_stores(dir) {
        roots.push(ss.dir);
    }
    let mut n = 0;
    for r in &roots {
        n += Chat::open_read_only(r)?.pending();
    }
    Ok(n)
}

fn cli_tend(env: &mut CliEnv, dir: &Path, f: &Flags) -> i32 {
    let total = match pending_all(dir) {
        Ok(n) => n,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    wl!(env.out, "nodes to build: {total}");
    if f.dry {
        let mut subs = vec![SubStore { rel: String::new(), dir: dir.to_path_buf() }];
        subs.extend(sub_stores(dir));
        for ss in &subs {
            let c = match Chat::open_read_only(&ss.dir) {
                Ok(c) => c,
                Err(e) => {
                    wl!(env.err, "{e}");
                    return 1;
                }
            };
            let label = ss.rel.clone();
            wl!(env.out, "{label}pending: {}", c.pending());
            wl!(env.out, "{label}not in the view yet: {}", c.not_in_view());
            for id in c.pending_ids() {
                wl!(env.out, "{label}{}", id.name());
            }
        }
        wl!(env.out, "nothing was changed");
        return 0;
    }
    if let Some(why) = paid_tend_refusal(total, f) {
        let _ = write!(env.err, "{why}");
        return 1;
    }
    let Some(mk) = env.new_summarizer else {
        wl!(env.err, "no model is set up for tend");
        return 1;
    };
    let s = match mk(&f.profile) {
        Ok(s) => s,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let mut c = match Chat::open(Config {
        dir: dir.to_path_buf(),
        now: env.now.clone(),
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if !c.notice().is_empty() {
        wl!(env.err, "note: {}", c.notice());
    }
    let ctx = Ctx::default();
    cap_calls(s.as_ref(), f.max);
    let mut run = TendRun {
        stop_on_clips: true,
        progress: if f.quiet { None } else { env.progress.clone() },
        ..Default::default()
    };
    let mut err = tend_calls(&ctx, &mut c, s.as_ref(), f.max, &mut run);
    if err.is_none() && (f.max == 0 || run.spent < f.max) {
        let rest = if f.max > 0 { f.max - run.spent } else { 0 };
        err = tend_children_calls(&ctx, dir, s.as_ref(), rest, env.now.clone(), &mut run);
    }
    let pending = c.pending();
    c.close();
    finish_tend(env, &run, pending, err, "")
}

/// The one line tend says when some nodes fell back to a clipped copy of the
/// source.
pub const CLIPPED_WHY: &str = "The model returned no text for the clipped nodes. If it is a thinking model, raise max_tokens in its profile or use a non-thinking model. Build them again with: daisugi pando tend --redo-clipped";

/// The end of a tend run: the counts, and why when a node fell back to
/// clipped text. It returns the exit code. `after` is a line to add when the
/// run stopped on an error.
fn finish_tend(env: &mut CliEnv, run: &TendRun, pending: usize, err: Option<String>, after: &str) -> i32 {
    let _ = write!(
        env.out,
        "built: {}, fell back to clipped: {}\npending: {pending}\n",
        run.built, run.clipped
    );
    let all = err.as_deref() == Some(ERR_ALL_CLIPPED);
    if run.clipped > 0 && !all {
        wl!(env.out, "{CLIPPED_WHY}");
    }
    if let Some(e) = err {
        wl!(env.err, "tend stopped: {e}");
        if all {
            wl!(env.err, "The nodes built so far hold clipped text. After you fix the model, run: daisugi pando tend --redo-clipped");
        }
        if !after.is_empty() {
            let _ = write!(env.err, "{after}");
        }
        return 1;
    }
    0
}

/// tend --redo-clipped and tend --redo-level N: build again the nodes that
/// hold a clipped fallback and the nodes above them, and the nodes at level N
/// and above, with the same budget and the same refusal of a paid run as
/// tend. The refusal counts the model calls the plan can make.
fn cli_redo(env: &mut CliEnv, dir: &Path, f: &Flags) -> i32 {
    let set = RedoSet {
        clipped: f.redo,
        from_level: f.redo_level,
    };
    let mut subs = vec![SubStore { rel: String::new(), dir: dir.to_path_buf() }];
    subs.extend(sub_stores(dir));
    let roots: Vec<PathBuf> = subs.iter().map(|ss| ss.dir.clone()).collect();
    let mut total = 0;
    let mut calls = 0;
    let mut lines: Vec<String> = vec![];
    for (k, r) in roots.iter().enumerate() {
        let mut c = match Chat::open_read_only(r) {
            Ok(c) => c,
            Err(e) => {
                wl!(env.err, "{e}");
                return 1;
            }
        };
        c.redo_model = f.profile.clone();
        let label = subs[k].rel.clone();
        let (ids, n) = c.redo_plan(set);
        calls += n;
        for id in ids {
            total += 1;
            lines.push(format!("{label}{}", id.name()));
        }
    }
    wl!(env.out, "nodes to build again: {total}");
    wl!(env.out, "model calls: at most {calls}");
    if f.dry {
        for l in &lines {
            wl!(env.out, "{l}");
        }
        wl!(env.out, "nothing was changed");
        return 0;
    }
    if total == 0 {
        for r in &roots {
            redo_end(r, set, &f.profile);
        }
        return 0;
    }
    if let Some(why) = paid_tend_refusal(calls, f) {
        let _ = write!(env.err, "{why}");
        return 1;
    }
    let Some(mk) = env.new_summarizer else {
        wl!(env.err, "no model is set up for tend");
        return 1;
    };
    let s = match mk(&f.profile) {
        Ok(s) => s,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let mut c = match Chat::open(Config {
        dir: dir.to_path_buf(),
        now: env.now.clone(),
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if !c.notice().is_empty() {
        wl!(env.err, "note: {}", c.notice());
    }
    let ctx = Ctx::default();
    cap_calls(s.as_ref(), f.max);
    let mut run = TendRun {
        stop_on_clips: true,
        model: f.profile.clone(),
        progress: if f.quiet { None } else { env.progress.clone() },
        ..Default::default()
    };
    let mut err = redo_calls(&ctx, &mut c, s.as_ref(), set, f.max, &mut run);
    if err.is_none() && (f.max == 0 || run.spent < f.max) {
        let rest = if f.max > 0 { f.max - run.spent } else { 0 };
        err = redo_children(&ctx, dir, s.as_ref(), set, rest, env.now.clone(), &mut run);
    }
    c.close();
    if err.is_none() && (f.max == 0 || run.spent < f.max) {
        // Every store was gone through: a next run starts again.
        for r in &roots {
            if !run.locked.contains(r) {
                redo_end(r, set, &f.profile);
            }
        }
    }
    let _ = write!(
        env.out,
        "built again: {}, fell back to clipped: {}\n",
        run.built, run.clipped
    );
    let all = err.as_deref() == Some(ERR_ALL_CLIPPED);
    if run.clipped > 0 && !all {
        wl!(env.out, "{CLIPPED_WHY}");
    }
    if let Some(e) = err {
        wl!(env.err, "tend stopped: {e}");
        return 1;
    }
    0
}

fn cli_migrate(env: &mut CliEnv, data: &Path, f: &Flags) -> i32 {
    let from = if f.mig_from.is_empty() {
        data.join("foreman")
    } else {
        PathBuf::from(&f.mig_from)
    };
    let rep = match migrate(&from, data, &f.store, env.now()) {
        Ok(r) => r,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if rep.already {
        wl!(env.out, "already moved: {}", rep.moved_to);
        return 0;
    }
    if f.as_json {
        let list = |v: &Vec<String>| {
            if v.is_empty() {
                "null".to_string()
            } else {
                format!("[{}]", v.iter().map(|s| js(s)).collect::<Vec<_>>().join(","))
            }
        };
        return print_json(
            env,
            format!(
                "{{\"Copied\":{},\"Kept\":{},\"MovedTo\":{},\"Messages\":{},\"Already\":false}}",
                list(&rep.copied),
                list(&rep.kept),
                js(&rep.moved_to),
                rep.messages
            ),
        );
    }
    let _ = write!(
        env.out,
        "copied: {}\nmessages: {}\nmoved: {}\n",
        rep.copied.join(", "),
        rep.messages,
        rep.moved_to
    );
    if !rep.kept.is_empty() {
        wl!(env.out, "kept in the moved dir for coppice: {}", rep.kept.join(", "));
    }
    0
}

fn cli_import(env: &mut CliEnv, data: &Path, dir: &Path, f: &Flags) -> i32 {
    let from = if f.from.is_empty() { "all" } else { f.from.as_str() };
    if !["claude", "codex", "web", "all"].contains(&from) {
        return usage_err(
            env,
            &format!(
                "--from needs claude, codex, web or all, not {}",
                crate::sys::go_quote(from)
            ),
        );
    }
    let mut maps: Vec<PathMap> = Vec::new();
    for w in &f.maps {
        match parse_map(w) {
            Ok(m) => maps.push(m),
            Err(e) => {
                wl!(env.err, "{e}");
                return 2;
            }
        }
    }
    let mut o = ImportOpts {
        from: from.to_string(),
        tools: f.tools,
        map: maps,
        web_zip: f.web.clone(),
        ..Default::default()
    };
    if !f.dir.is_empty() {
        if !f.from.is_empty() && f.from != "claude" {
            return usage_err(
                env,
                &format!("--dir holds Claude Code chats: use it with --from claude, not --from {}", f.from),
            );
        }
        if !f.propose.is_empty() || !f.accept.is_empty() {
            return usage_err(env, "--dir does not go with --propose or --accept");
        }
        if !Path::new(&f.dir).is_dir() {
            wl!(env.err, "no folder {}", f.dir);
            return 1;
        }
        o.from = "claude".into();
        // Absolute, so a queued session names the same file from any directory.
        o.dir = std::path::absolute(&f.dir).map(|p| p.to_string_lossy().into_owned()).unwrap_or_else(|_| f.dir.clone());
    }
    if !f.since.is_empty() {
        match super::import::parse_day(&f.since) {
            Some(t) => o.since = Some(t),
            None => {
                return usage_err(
                    env,
                    &format!(
                        "--since needs a date like 2026-03-01, not {}",
                        crate::sys::go_quote(&f.since)
                    ),
                )
            }
        }
    }
    if !f.propose.is_empty() && !f.accept.is_empty() {
        return usage_err(env, "give --propose or --accept, not both");
    }
    // The model is chosen before anything is written, so a store is never
    // left half imported for want of one.
    let mut summ: Option<Box<dyn Summarizer>> = None;
    if f.tend && !f.dry && f.propose.is_empty() {
        let Some(mk) = env.new_summarizer else {
            wl!(env.err, "no model is set up for tend");
            return 1;
        };
        match mk(&f.profile) {
            Ok(s) => summ = Some(s),
            Err(e) => {
                wl!(env.err, "{e}");
                return 1;
            }
        }
    }
    if from == "web" || !f.propose.is_empty() || !f.accept.is_empty() {
        return cli_import_web(env, data, f, o, summ.as_deref());
    }
    cli_import_local(env, dir, f, o, summ.as_deref())
}

fn has_act_of(dir: &Path) -> Option<Chat> {
    Chat::open_read_only(dir).ok()
}

fn cli_import_local(env: &mut CliEnv, dir: &Path, f: &Flags, mut o: ImportOpts, summ: Option<&dyn Summarizer>) -> i32 {
    o.skip = vec![dir.to_string_lossy().into_owned()];
    let st = match read_import_state(dir) {
        Ok(s) => s,
        Err(e) => {
            wl!(env.err, "cannot read the import state in {}: {e}", dir.display());
            return 1;
        }
    };
    let chat = has_act_of(dir);
    let has = chat.as_ref().map(|c| move |a: &str| c.has_act(a));
    let ss = scan(
        &Ctx::default(),
        &env.home.clone(),
        &o,
        &st,
        has.as_ref().map(|h| h as &dyn Fn(&str) -> bool),
    );
    let _ = write!(env.out, "{}", describe(&ss));
    if f.dry {
        wl!(env.out, "nothing was changed");
        return 0;
    }
    if ss.is_empty() {
        wl!(env.out, "nothing to import");
        return 0;
    }
    accept_and_feed(env, dir, &o, &ss, summ, f)
}

/// Queues the sessions and logs them. When another process holds the
/// store, they stay queued for it.
fn accept_and_feed(
    env: &mut CliEnv,
    dir: &Path,
    o: &ImportOpts,
    ss: &[ImportSession],
    summ: Option<&dyn Summarizer>,
    f: &Flags,
) -> i32 {
    if let Err(e) = accept(dir, o, ss) {
        wl!(env.err, "cannot write the import state in {}: {e}", dir.display());
        return 1;
    }
    let mut c = match Chat::open(Config {
        dir: dir.to_path_buf(),
        now: env.now.clone(),
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) if e == ERR_LOCKED => {
            wl!(env.out, "queued: the process that has the store open logs them, one message at a time");
            return 0;
        }
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let mut im = Importer::new(dir.to_path_buf());
    im.skip = o.skip.clone();
    let mut n = 0;
    loop {
        match im.step(&mut c) {
            Err(e) => {
                wl!(env.err, "import stopped: {e}");
                return 1;
            }
            Ok(false) => break,
            Ok(true) => n += 1,
        }
    }
    let Some(summ) = summ else {
        let pending = c.pending();
        c.close();
        let _ = write!(
            env.out,
            "logged: {n} messages\nnodes to build: {pending}. Run: daisugi pando tend\n"
        );
        return 0;
    };
    wl!(env.out, "logged: {n} messages");
    if let Some(why) = paid_tend_refusal(c.pending(), f) {
        let _ = write!(
            env.err,
            "nodes to build: {}
{why}The messages are logged. Run: daisugi pando tend
",
            c.pending()
        );
        return 1;
    }
    cap_calls(summ, f.max);
    let mut run = TendRun {
        stop_on_clips: true,
        progress: if f.quiet { None } else { env.progress.clone() },
        ..Default::default()
    };
    let err = tend_calls(&Ctx::default(), &mut c, summ, f.max, &mut run);
    let pending = c.pending();
    c.close();
    finish_tend(env, &run, pending, err, "the rest stays pending. Run: daisugi pando tend\n")
}

fn cli_import_web(
    env: &mut CliEnv,
    data: &Path,
    f: &Flags,
    o: ImportOpts,
    summ: Option<&dyn Summarizer>,
) -> i32 {
    if f.web.is_empty() {
        return usage_err(
            env,
            "--from web needs --web PATH, the export zip or its conversations.json",
        );
    }
    let web = PathBuf::from(&f.web);
    if f.accept.is_empty() {
        let chat = store_dir(data, &f.store).ok().and_then(|d| has_act_of(&d));
        let has = chat.as_ref().map(|c| move |a: &str| c.has_act(a));
        let ss = match scan_web(&web, &o, has.as_ref().map(|h| h as &dyn Fn(&str) -> bool)) {
            Ok(s) => s,
            Err(e) => {
                wl!(env.err, "{e}");
                return 1;
            }
        };
        if !f.propose.is_empty() {
            let res = std::fs::File::create(&f.propose)
                .and_then(|mut w| write_proposal(&mut w, &ss).and_then(|_| w.flush()));
            if let Err(e) = res {
                wl!(env.err, "{e}");
                return 1;
            }
            wl!(
                env.out,
                "wrote {}: {} chats. Edit the store of each line, then run import --accept. No store was changed.",
                f.propose,
                ss.len()
            );
            return 0;
        }
        let (all, _) = totals(&ss);
        wl!(
            env.out,
            "web chats: {} (about {})\nnothing was changed",
            ss.len(),
            plural(all.messages as i64, "message")
        );
        return 0;
    }
    let pf = match std::fs::File::open(&f.accept) {
        Ok(p) => p,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let choices = match read_proposal(&mut std::io::BufReader::new(pf)) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 2;
        }
    };
    let all = match scan_web(&web, &o, None) {
        Ok(s) => s,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let mut per_store: std::collections::HashMap<String, Vec<ImportSession>> = Default::default();
    let mut order: Vec<String> = Vec::new();
    for ch in &choices {
        let Some(s) = all.iter().find(|s| s.key == ch.key) else {
            wl!(env.err, "the export has no chat {}", ch.key);
            return 1;
        };
        if ch.store == "skip" {
            continue;
        }
        if !per_store.contains_key(&ch.store) {
            order.push(ch.store.clone());
        }
        per_store.entry(ch.store.clone()).or_default().push(s.clone());
    }
    for store in &order {
        let d = match store_dir(data, store) {
            Ok(d) => d,
            Err(e) => {
                wl!(env.err, "{e}");
                return 2;
            }
        };
        wl!(env.out, "store {store}: {} chats", per_store[store].len());
        let mut o2 = o.clone();
        o2.from = SRC_WEB.into();
        let code = accept_and_feed(env, &d, &o2, &per_store[store], summ, f);
        if code != 0 {
            return code;
        }
    }
    if order.is_empty() {
        wl!(env.out, "every line said skip; nothing was changed");
    }
    0
}

/// Makes --max cap every model call of the run, a re-ask for a shorter line
/// included, when the summarizer can be capped.
fn cap_calls(s: &dyn Summarizer, max: usize) {
    s.set_budget(max);
}

fn cli_profile(env: &mut CliEnv, data: &Path, args: &[String]) -> i32 {
    use super::live::{read_profile, write_profile, PROFILE_FILE};
    let path = data.join("pando").join(PROFILE_FILE);
    let Some(name) = args.first() else {
        let p = read_profile(data, None);
        if p.is_empty() {
            wl!(env.out, "none: nodes wait for a profile");
        } else {
            wl!(env.out, "{p}");
        }
        return 0;
    };
    if name == "none" {
        if let Err(e) = std::fs::remove_file(&path) {
            if e.kind() != std::io::ErrorKind::NotFound {
                wl!(env.err, "{e}");
                return 1;
            }
        }
        wl!(env.out, "no profile: nodes wait");
        return 0;
    }
    if name.contains([' ', '\t', '\n', '/', '\\']) {
        return usage_err(
            env,
            &format!("a profile name has no spaces or slashes, not {}", crate::sys::go_quote(name)),
        );
    }
    if let Err(e) = write_profile(data, name) {
        wl!(env.err, "{e}");
        return 1;
    }
    wl!(env.out, "profile: {name}");
    0
}

fn cli_import_hermes(env: &mut CliEnv, dir: &Path, f: &Flags) -> i32 {
    let Some(mk) = env.hermes_source else {
        wl!(env.err, "this build cannot read Hermes chats");
        return 1;
    };
    let mut src = match mk(&f.hermes_profile) {
        Ok(s) => s,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    if f.dry {
        wl!(env.out, "hermes: would read the chats of {}", src.name());
        wl!(env.out, "nothing was changed");
        return 0;
    }
    let now = env.now.clone();
    let d = dir.to_path_buf();
    let mut c = match super::live::open_wait(
        || Config { dir: d.clone(), now: now.clone(), ..Default::default() },
        std::time::Duration::from_secs(20),
    ) {
        Ok(c) => c,
        Err(e) => {
            wl!(env.err, "{e}");
            return 1;
        }
    };
    let r = src.poll(&mut c, None);
    c.close();
    match r {
        Ok(n) => {
            wl!(env.out, "hermes: {n} messages logged");
            0
        }
        Err(e) => {
            wl!(env.out, "hermes: 0 messages logged");
            wl!(env.err, "{e}");
            1
        }
    }
}

fn cli_follow(env: &mut CliEnv, data: &Path, dir: &Path, f: &Flags) -> i32 {
    use super::live::{follow, follow_once, read_profile, ClaudeSource, FollowOpts};
    let mut profile = f.profile.clone();
    if profile.is_empty() {
        profile = read_profile(data, env.getenv);
    }
    let root = if f.claude_dir.is_empty() {
        env.home.join(".claude").join("projects")
    } else {
        PathBuf::from(&f.claude_dir)
    };
    let mut errw: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.to_path_buf(), &mut errw);
    o.now = env.now.clone();
    if f.interval > 0 {
        o.interval = std::time::Duration::from_secs(f.interval as u64);
    }
    let mut cs = ClaudeSource::new(root);
    cs.skip = super::live::skip_dirs(dir, &env.home, env.getenv);
    o.sources.push(Box::new(cs));
    let mut pre = String::new();
    if let Some(mk) = env.hermes_source {
        match mk(&f.hermes_profile) {
            Ok(s) => o.sources.push(s),
            Err(e) => pre.push_str(&format!("follow: hermes: {e}\n")),
        }
    }
    if !profile.is_empty() && profile != "default" {
        if let Some(ns) = env.new_summarizer {
            match ns(&profile) {
                Ok(s) => o.summarizer = Some(s),
                Err(e) => {
                    wl!(env.err, "{e}");
                    return 1;
                }
            }
        }
    }
    let ctx = Ctx::background();
    if f.once {
        let r = follow_once(&ctx, &mut o);
        drop(o);
        let _ = write!(env.err, "{pre}{}", String::from_utf8_lossy(&errw));
        return match r {
            Ok((added, built, more)) => {
                wl!(env.out, "logged: {added}, built a node: {built}, more due: {more}");
                0
            }
            Err(e) => {
                wl!(env.err, "{e}");
                1
            }
        };
    }
    let _ = write!(env.err, "{pre}");
    if let Err(e) = std::fs::create_dir_all(dir) {
        wl!(env.err, "{e}");
        return 1;
    }
    // The long run writes its log lines straight to standard error.
    let mut stderr = std::io::stderr();
    let mut o2 = FollowOpts::new(dir.to_path_buf(), &mut stderr);
    o2.now = o.now.take();
    o2.interval = o.interval;
    o2.sources = std::mem::take(&mut o.sources);
    o2.summarizer = o.summarizer.take();
    let _ = writeln!(env.err, "follow: store {}, {} sources", dir.display(), o2.sources.len());
    match follow(&ctx, &mut o2) {
        Ok(()) => 0,
        Err(e) => {
            wl!(env.err, "{e}");
            1
        }
    }
}

fn cli_branches(env: &mut CliEnv, dir: &Path, f: &Flags) -> i32 {
    let rows = branch_rows(dir);
    if f.as_json {
        let items: Vec<String> = rows
            .iter()
            .map(|r| {
                format!(
                    "{{\"path\":{},\"state\":{},\"messages\":{},\"newest\":{}}}",
                    js(&r.path),
                    js(&r.state),
                    r.messages,
                    js(&r.newest)
                )
            })
            .collect();
        return print_json(env, format!("[{}]", items.join(",")));
    }
    if rows.is_empty() {
        wl!(env.out, "no branch");
        return 0;
    }
    for r in &rows {
        wl!(env.out, "{}\t{}\t{}\t{}", r.path, r.state, r.messages, r.newest);
    }
    0
}

/// What a process with a marker hears for a verb that is the owner's (F6).
fn marked_refusal(branch: &str, verb: &str) -> String {
    if branch.trim().is_empty() {
        return format!("this process is a worker of the memory; {verb} is for the owner's own shell");
    }
    format!("this process works in a branch of the memory ({branch}); {verb} is for the owner's own shell")
}

/// `routes` and `unroute`: the owner reads and fixes the session map (F2).
fn cli_routes(env: &mut CliEnv, dir: &Path, f: &Flags, verb: &str, args: &[String]) -> i32 {
    if verb == "unroute" {
        return match super::branch::remove_route(dir, &args[0]) {
            Err(e) => {
                wl!(env.err, "{e}");
                1
            }
            Ok(false) => {
                wl!(env.err, "no route for {}", args[0]);
                1
            }
            Ok(true) => {
                wl!(env.out, "unrouted: {}", args[0]);
                0
            }
        };
    }
    let rows = super::branch::route_rows(dir);
    if f.as_json {
        let items: Vec<String> =
            rows.iter().map(|(i, b)| format!("{{\"id\":{},\"branch\":{}}}", js(i), js(b))).collect();
        return print_json(env, format!("[{}]", items.join(",")));
    }
    if rows.is_empty() {
        wl!(env.out, "no route");
        return 0;
    }
    for (i, b) in &rows {
        wl!(env.out, "{i}\t{b}");
    }
    0
}
