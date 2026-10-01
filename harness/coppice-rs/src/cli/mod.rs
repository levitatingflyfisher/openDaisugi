//! The coppice command line. Every verb is one socket call, so the command
//! line, the floor and the phone all drive the same server through the same
//! protocol.

mod client;
mod goflag;
pub mod gofmt;
mod tree;
mod verbs;
mod web;

use std::io::{IsTerminal, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use crate::config;
use crate::server::{self, Server};
use crate::sys;
use client::{dial, dial_existing, dial_remote, log_path, Client, DoErr};

pub use client::Client as PubClient;

/// A connection to a server that already listens, for the tmux mirror.
pub fn dial_existing_pub(socket: &Path) -> Result<Client, String> {
    dial_existing(socket, None)
}

/// Prints a call's error and returns the exit code.
pub fn report_pub(cl: &mut Client, e: DoErr) -> i32 {
    report(cl, e)
}

/// Prints a call's error and picks the exit code: 3 for a connection that
/// failed, with a remote child's stderr after it; 1 otherwise.
fn report(cl: &mut Client, e: DoErr) -> i32 {
    match e {
        DoErr::Transport => {
            cl.close();
            errln("the server closed the connection. Run: coppice server status");
            let tail = cl.stderr_tail();
            if !tail.is_empty() {
                errln(&tail);
            }
            3
        }
        DoErr::Other(m) => {
            errln(&m);
            1
        }
    }
}

pub const USAGE: &str = "coppice: panes, agents and their state, on one machine.

  coppice                                  open the floor
  coppice claude [ARGS...]                 open that harness here and attach
  coppice open HARNESS [ARGS...]           the long form
  coppice server start|stop|status|token
  coppice workspace create [--cwd DIR] [--label TEXT]
  coppice workspace list
  coppice tab create [--label TEXT] [--workspace ID]
  coppice tab list [--workspace ID]
  coppice pane create [--cwd DIR] [--label TEXT] [--kind pty|headless]
                      [--harness NAME] [--cols N] [--rows N] [--task ID] -- COMMAND...
  coppice pane list [--ended]|send-text|send-keys|run|read|close|resize|wait-output|explain
  coppice pane fork PANE [--label TEXT]
  coppice pane forget PANE|--ended        remove an ended pane's record; a live pane refuses
  coppice pane resume PANE                start a new pane from an ended one; removes it
  coppice pane rename PANE LABEL...       change a pane's label in place, live or ended
  coppice pane trust PANE [--not-now]     answer Claude's folder trust screen; --not-now
                                           presses Esc, which ends Claude
  coppice new [PROJECT] [--no-attach]     the default harness, in a project by name or
                                           path, or here; attaches, unless --no-attach
  coppice project add|list|rm [DIR]       pin, list or unpin a working directory
  coppice agent list|get|prompt|wait|read
  coppice agent allow|deny PANE ASK [--reason TEXT]   answer a gate ask; a pane cannot,
                                           but a foreman may deny an ask it holds
  coppice agent allow PANE ASK --confirm NAME  allow a permanent ask by its pane name
  coppice agent allow PANE ASK --scope task    allow an undoable ask for the task
  coppice floor note TEXT                  print one line on the floor
  coppice floor talk TEXT                  say TEXT to the floor's foreman; starts one if none runs
  coppice skill foreman                    print the page that makes a pane the foreman
  coppice task create --label NAME [--parent ID] [--cwd DIR] [--worktree] [--model NAME]
  coppice task list [--tree]
  coppice task close ID [--keep-worktree]
  coppice task move ID --parent ID
  coppice task set-foreman ID --pane PANE  name the pane that hears the task's asks first
  coppice attach [PANE]
  coppice tmux-mirror [--session NAME] [--tmux-socket PATH]
                                           one tmux window per pane, each attached
  coppice web cert init|show|tailscale
  coppice web serve|token
  coppice --remote ssh://HOST <pane, agent, workspace or tab command, or server status/stop>
  coppice --stdio                          speak the protocol on stdin and stdout

Add --json to any server, workspace, tab, pane, agent or task command to get
one JSON object instead of a table or a line. attach has no --json flag.
Run coppice pane list to see your panes.
The first command brings up a server on its own. Set COPPICE_NO_AUTOSTART to
turn that off.";

/// The default socket: the runtime dir when the session has one, else a
/// private directory in the home directory.
pub fn socket_path() -> PathBuf {
    if let Some(rt) = std::env::var_os("XDG_RUNTIME_DIR") {
        if !rt.is_empty() {
            return PathBuf::from(rt).join("coppice").join("server.sock");
        }
    }
    match sys::home_dir() {
        Some(h) => h.join(".opendaisugi").join("coppice").join("server.sock"),
        None => PathBuf::from(".opendaisugi/coppice/server.sock"),
    }
}

/// The default data dir, which holds what must survive a restart.
pub fn data_dir() -> PathBuf {
    match sys::home_dir() {
        Some(h) => h.join(".opendaisugi").join("coppice"),
        None => PathBuf::from(".opendaisugi/coppice"),
    }
}

/// Go's os.Getwd: $PWD when it names the working directory, else the
/// kernel's answer.
pub fn getwd() -> Result<String, String> {
    use std::os::unix::fs::MetadataExt;
    let dot = std::fs::metadata(".").map_err(|e| sys::go_path_err("stat", Path::new("."), &e))?;
    if let Ok(pwd) = std::env::var("PWD") {
        if pwd.starts_with('/') {
            if let Ok(m) = std::fs::metadata(&pwd) {
                if m.dev() == dot.dev() && m.ino() == dot.ino() {
                    return Ok(pwd);
                }
            }
        }
    }
    std::env::current_dir()
        .map(|p| p.to_string_lossy().into_owned())
        .map_err(|e| sys::go_errno_text(&e))
}

/// Go's filepath.Abs.
fn abs(p: &str) -> Result<String, String> {
    if p.starts_with('/') {
        return Ok(sys::clean(p));
    }
    let wd = getwd()?;
    Ok(sys::clean(&format!("{wd}/{p}")))
}

/// Go's term.IsTerminal on a file descriptor.
fn is_terminal(fd: i32) -> bool {
    // SAFETY: isatty reads no memory of ours.
    unsafe { libc::isatty(fd) == 1 }
}

/// The size of the terminal on fd, when it is one.
pub fn term_size(fd: i32) -> Option<(i64, i64)> {
    // SAFETY: the ioctl writes one winsize into ws.
    unsafe {
        let mut ws: libc::winsize = std::mem::zeroed();
        if libc::ioctl(fd, libc::TIOCGWINSZ, &mut ws) != 0 {
            return None;
        }
        Some((ws.ws_col as i64, ws.ws_row as i64))
    }
}

/// The size the floor and attach use: the terminal's, else 120 by 40.
fn floor_size() -> (i64, i64) {
    term_size(1).unwrap_or((120, 40))
}

fn out(s: &str) {
    let mut o = std::io::stdout().lock();
    let _ = o.write_all(s.as_bytes());
    let _ = o.flush();
}

fn errln(s: &str) {
    let mut e = std::io::stderr().lock();
    let _ = e.write_all(s.as_bytes());
    let _ = e.write_all(b"\n");
}

pub struct Cli {
    socket: PathBuf,
    data_dir: PathBuf,
}

pub fn run(argv: Vec<String>) -> i32 {
    let mut c = Cli {
        socket: socket_path(),
        data_dir: data_dir(),
    };
    let mut remote = String::new();
    let mut args = argv.as_slice();
    while let Some(a) = args.first() {
        if !a.starts_with("--") {
            break;
        }
        match a.as_str() {
            "--version" => {
                out(&format!("coppice {}\n", env!("COPPICE_VERSION")));
                return 0;
            }
            "--help" | "-h" => {
                out(&format!("{USAGE}\n"));
                return 0;
            }
            "--stdio" => return c.run_stdio(),
            "--remote" => {
                let Some(v) = args.get(1) else {
                    errln("--remote needs a target like ssh://host");
                    return 1;
                };
                remote = v.clone();
                args = &args[2..];
            }
            "--socket" | "--data-dir" => {
                let Some(v) = args.get(1) else {
                    errln(&format!("{a} needs a path"));
                    return 1;
                };
                if a == "--socket" {
                    c.socket = PathBuf::from(v);
                } else {
                    c.data_dir = PathBuf::from(v);
                }
                args = &args[2..];
            }
            _ => {
                errln(&format!("unknown option {}\n{USAGE}", sys::go_quote(a)));
                return 1;
            }
        }
    }
    let Some(first) = args.first() else {
        return c.run_floor(&remote);
    };
    match first.as_str() {
        "server" => c.run_server(&remote, &args[1..]),
        "attach" => c.run_attach(&remote, &args[1..]),
        "tmux-mirror" => c.run_tmux_mirror(&remote, &args[1..]),
        "web" => c.run_web(&remote, &args[1..]),
        "task" => c.run_task(&remote, args),
        "workspace" | "tab" | "pane" | "agent" | "session" | "floor" => c.run_verb(&remote, args),
        "skill" => c.run_skill(&args[1..]),
        "open" => {
            if args.len() < 2 {
                errln("open needs a harness name. Run coppice to see the list.");
                return 1;
            }
            c.open_at(&remote, &args[1..], false, "", false)
        }
        "new" => c.run_new(&remote, &args[1..]),
        "project" => c.run_project(&remote, &args[1..]),
        _ => c.open_at(&remote, args, true, "", false),
    }
}

impl Cli {
    fn report(&self, cl: &mut Client, e: DoErr) -> i32 {
        report(cl, e)
    }

    fn dial(&self, remote: &str) -> Result<Client, i32> {
        let r = if remote.is_empty() {
            dial(&self.socket, &self.data_dir)
        } else {
            dial_remote(remote)
        };
        r.map_err(|e| {
            errln(&e);
            3
        })
    }

    fn run_skill(&self, argv: &[String]) -> i32 {
        let names = "foreman";
        if argv.len() != 1 {
            errln(&format!("skill needs one page name. Pages: {names}"));
            return 1;
        }
        if argv[0] != "foreman" {
            errln(&format!(
                "no skill page {}. Pages: {names}",
                sys::go_quote(&argv[0])
            ));
            return 1;
        }
        out(server::foreman::FOREMAN_PAGE);
        0
    }

    /// open's whole body: argv[0] is a harness name; a pane opens for it
    /// and attaches. bare says the name came as a bare word.
    fn open_at(
        &self,
        remote: &str,
        argv: &[String],
        bare: bool,
        cwd_override: &str,
        no_attach: bool,
    ) -> i32 {
        if !remote.is_empty() {
            errln("coppice open needs a local socket; it is not wired for --remote yet. SSH into the host and run coppice open there.");
            return 1;
        }
        let (name, rest) = (&argv[0], &argv[1..]);
        let cfg = match config::load() {
            Ok(c) => c,
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                return 1;
            }
        };
        let mut save = None;
        match cfg {
            Some(cfg) => {
                if !cfg.harness.contains_key(name) {
                    let names: Vec<&str> = cfg.harness.keys().map(String::as_str).collect();
                    errln(&format!(
                        "unknown command {}. Harnesses in {}: {}.",
                        sys::go_quote(name),
                        config::path().display(),
                        names.join(", ")
                    ));
                    if bare {
                        errln(USAGE);
                    }
                    return 1;
                }
            }
            None => {
                let disc = config::discover();
                if disc.is_empty() {
                    errln(config::APP_ONLY_NOTE);
                    if bare {
                        errln(USAGE);
                    }
                    return 1;
                }
                if !disc.iter().any(|f| &f.name == name) {
                    if bare {
                        errln(&format!("unknown command {}\n{USAGE}", sys::go_quote(name)));
                    } else {
                        let names: Vec<&str> = disc.iter().map(|f| f.name.as_str()).collect();
                        errln(&format!(
                            "{name} is not on PATH. Harnesses found: {}.",
                            names.join(", ")
                        ));
                    }
                    return 1;
                }
                save = Some(config::from_found(&disc, name));
            }
        }
        if let Some(s) = save {
            if let Err(e) = config::save(&s) {
                errln(&format!("cannot write {}: {e}", config::path().display()));
                return 1;
            }
        }
        let cwd = if cwd_override.is_empty() {
            match getwd() {
                Ok(w) => w,
                Err(e) => {
                    errln(&format!("cannot read the working directory: {e}"));
                    return 1;
                }
            }
        } else {
            cwd_override.to_string()
        };
        let mut cl = match self.dial(remote) {
            Ok(c) => c,
            Err(code) => return code,
        };
        let (cols, mut rows) = floor_size();
        if rows > 1 {
            rows -= 1;
        }
        let mut params = Map::new();
        params.insert("harness".into(), json!(name));
        params.insert("cwd".into(), json!(cwd));
        params.insert("kind".into(), json!("pty"));
        params.insert("cols".into(), json!(cols));
        params.insert("rows".into(), json!(rows));
        if !rest.is_empty() {
            params.insert("args".into(), json!(rest));
        }
        let res = match cl.call("pane.create", params) {
            Ok(r) => r,
            Err(e) => return self.report(&mut cl, e),
        };
        cl.close();
        let id = res
            .get("pane")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        if id.is_empty() {
            errln("the server created no pane. Run: coppice pane list");
            return 1;
        }
        if no_attach {
            out(&format!("opened {id}\n"));
            return 0;
        }
        if !std::io::stdin().is_terminal() {
            out(&format!("opened {id}. attach needs a terminal.\n"));
            return 0;
        }
        self.run_attach("", &[id])
    }

    fn run_new(&self, remote: &str, argv: &[String]) -> i32 {
        if !remote.is_empty() {
            errln("coppice new needs a local socket; it is not wired for --remote yet. SSH into the host and run coppice new there.");
            return 1;
        }
        let mut no_attach = false;
        let mut project = String::new();
        for a in argv {
            if a == "--no-attach" {
                no_attach = true;
                continue;
            }
            if !project.is_empty() {
                errln("coppice new takes at most one project name or path");
                return 1;
            }
            project = a.clone();
        }
        let cwd = match self.resolve_new_cwd(remote, &project) {
            Ok(c) => c,
            Err(code) => return code,
        };
        let name = match self.default_harness_name() {
            Ok(n) => n,
            Err(code) => return code,
        };
        self.open_at(remote, &[name], false, &cwd, no_attach)
    }

    fn resolve_new_cwd(&self, remote: &str, project: &str) -> Result<String, i32> {
        if project.is_empty() {
            return getwd().map_err(|e| {
                errln(&format!("cannot read the working directory: {e}"));
                1
            });
        }
        let mut cl = self.dial(remote)?;
        let res = match cl.call("project.list", Map::new()) {
            Ok(r) => r,
            Err(e) => return Err(self.report(&mut cl, e)),
        };
        cl.close();
        let rows: Vec<&Map<String, Value>> = res
            .get("projects")
            .and_then(Value::as_array)
            .map(|a| a.iter().filter_map(Value::as_object).collect())
            .unwrap_or_default();
        let get = |m: &Map<String, Value>, k: &str| {
            m.get(k).and_then(Value::as_str).unwrap_or("").to_string()
        };
        let by_name: Vec<String> = rows
            .iter()
            .filter(|m| get(m, "name") == project)
            .map(|m| get(m, "path"))
            .filter(|p| !p.is_empty())
            .collect();
        match by_name.len() {
            1 => return Ok(by_name[0].clone()),
            0 => {}
            n => {
                errln(&format!(
                    "{n} projects are named {}: {}. Pass the path instead.",
                    sys::go_quote(project),
                    by_name.join(", ")
                ));
                return Err(1);
            }
        }
        if let Ok(a) = abs(project) {
            for m in &rows {
                if get(m, "path") == a {
                    return Ok(a);
                }
            }
        }
        errln(&format!(
            "no project named or at {}. Run: coppice project list",
            sys::go_quote(project)
        ));
        Err(1)
    }

    fn default_harness_name(&self) -> Result<String, i32> {
        match config::load() {
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                Err(1)
            }
            Ok(Some(cfg)) => {
                if !cfg.default.is_empty() {
                    return Ok(cfg.default);
                }
                errln(&format!(
                    "no default harness in {}. Run coppice open HARNESS once to set one.",
                    config::path().display()
                ));
                Err(1)
            }
            Ok(None) => {
                let disc = config::discover();
                match disc.first() {
                    Some(f) => Ok(f.name.clone()),
                    None => {
                        errln(config::APP_ONLY_NOTE);
                        Err(1)
                    }
                }
            }
        }
    }

    fn run_project(&self, remote: &str, argv: &[String]) -> i32 {
        let Some(sub) = argv.first() else {
            errln("project needs a verb: add, list or rm");
            return 1;
        };
        let rest = &argv[1..];
        if (sub == "add" || sub == "rm") && !remote.is_empty() {
            errln(&format!("coppice project {sub} edits the local coppice.toml; it is not wired for --remote yet. SSH into the host and run coppice project {sub} there."));
            return 1;
        }
        match sub.as_str() {
            "add" => self.project_add(rest),
            "rm" => self.project_remove(rest),
            "list" => {
                let mut a = vec!["project".to_string(), "list".to_string()];
                a.extend(rest.iter().cloned());
                self.run_verb(remote, &a)
            }
            _ => {
                errln(&format!(
                    "unknown project verb {}. Known verbs: add, list, rm",
                    sys::go_quote(sub)
                ));
                1
            }
        }
    }

    fn project_add(&self, argv: &[String]) -> i32 {
        if argv.len() != 1 {
            errln("project add needs one directory");
            return 1;
        }
        let a = match abs(&argv[0]) {
            Ok(a) => a,
            Err(e) => {
                errln(&format!("cannot resolve {}: {e}", sys::go_quote(&argv[0])));
                return 1;
            }
        };
        match std::fs::metadata(&a) {
            Err(_) => {
                errln(&format!("{a} does not exist"));
                return 1;
            }
            Ok(m) if !m.is_dir() => {
                errln(&format!("{a} is not a directory"));
                return 1;
            }
            Ok(_) => {}
        }
        let resolved = resolve_symlinks(&a);
        let mut cfg = match config::load() {
            Ok(c) => c.unwrap_or_default(),
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                return 1;
            }
        };
        if cfg.projects.contains(&resolved) {
            out(&format!("{resolved} is already a project\n"));
            return 0;
        }
        cfg.projects.push(resolved.clone());
        if let Err(e) = config::save(&cfg) {
            errln(&format!("cannot write {}: {e}", config::path().display()));
            return 1;
        }
        out(&format!("added {resolved}\n"));
        0
    }

    fn project_remove(&self, argv: &[String]) -> i32 {
        if argv.len() != 1 {
            errln("project rm needs one directory");
            return 1;
        }
        let a = match abs(&argv[0]) {
            Ok(a) => a,
            Err(e) => {
                errln(&format!("cannot resolve {}: {e}", sys::go_quote(&argv[0])));
                return 1;
            }
        };
        let resolved = resolve_symlinks(&a);
        let mut cfg = match config::load() {
            Ok(c) => c.unwrap_or_default(),
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                return 1;
            }
        };
        let before = cfg.projects.len();
        cfg.projects.retain(|p| p != &resolved);
        if cfg.projects.len() == before {
            errln(&format!(
                "{resolved} is not a project. Run: coppice project list"
            ));
            return 1;
        }
        if let Err(e) = config::save(&cfg) {
            errln(&format!("cannot write {}: {e}", config::path().display()));
            return 1;
        }
        out(&format!("removed {a}\n"));
        0
    }

    fn run_verb(&self, remote: &str, argv: &[String]) -> i32 {
        if argv.len() < 2 {
            errln(&format!("{} needs a verb. {USAGE}", argv[0]));
            return 1;
        }
        let (group, verb) = (&argv[0], &argv[1]);
        let (params, as_json) = match verbs::parse(group, verb, &argv[2..]) {
            Ok(p) => p,
            Err(m) => {
                errln(&m);
                return 1;
            }
        };
        let mut cl = match self.dial(remote) {
            Ok(c) => c,
            Err(code) => return code,
        };
        let res = match cl.call(&verbs::socket_command(group, verb), params) {
            Ok(r) => r,
            Err(e) => return self.report(&mut cl, e),
        };
        cl.close();
        if as_json {
            out(&format!("{}\n", gofmt::marshal(&Value::Object(res))));
            return 0;
        }
        print_human(group, verb, &res);
        0
    }

    fn run_task(&self, remote: &str, argv: &[String]) -> i32 {
        let mut argv = argv.to_vec();
        if argv.len() >= 2 && argv[1] == "create" && remote.is_empty() {
            let mut i = 2;
            while i + 1 < argv.len() {
                if argv[i] == "--cwd" {
                    match abs(&argv[i + 1]) {
                        Ok(a) => argv[i + 1] = a,
                        Err(e) => {
                            errln(&format!(
                                "cannot resolve --cwd {}: {e}",
                                sys::go_quote(&argv[i + 1])
                            ));
                            return 1;
                        }
                    }
                }
                i += 1;
            }
        }
        if argv.len() >= 2 && argv[1] == "list" {
            let kept: Vec<&String> = argv[2..].iter().filter(|a| *a != "--tree").collect();
            let tree = argv[2..].iter().any(|a| a == "--tree");
            if tree {
                if let Some(k) = kept.first() {
                    errln(&format!(
                        "task list --tree takes no other flag, got {}",
                        sys::go_quote(k)
                    ));
                    return 1;
                }
                return self.run_task_tree(remote);
            }
        }
        self.run_verb(remote, &argv)
    }

    fn run_task_tree(&self, remote: &str) -> i32 {
        let mut cl = match self.dial(remote) {
            Ok(c) => c,
            Err(code) => return code,
        };
        let tasks = match cl.call("task.list", Map::new()) {
            Ok(r) => r,
            Err(e) => return self.report(&mut cl, e),
        };
        let panes = match cl.call("pane.list", Map::new()) {
            Ok(r) => r,
            Err(e) => return self.report(&mut cl, e),
        };
        cl.close();
        let list = |m: &Map<String, Value>, k: &str| {
            m.get(k)
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default()
        };
        let cols = term_size(1)
            .map(|(w, _)| w)
            .filter(|w| *w > 0)
            .unwrap_or(80);
        let lines = tree::render(&list(&tasks, "tasks"), &list(&panes, "panes"), cols);
        if lines.is_empty() {
            out("No tasks. Run: coppice task create --label NAME\n");
            return 0;
        }
        for l in lines {
            out(&format!("{l}\n"));
        }
        0
    }

    fn run_server(&self, remote: &str, argv: &[String]) -> i32 {
        let Some(sub) = argv.first() else {
            errln("coppice server needs start, stop, status or token");
            return 1;
        };
        let known: &[&str] = match sub.as_str() {
            "start" => &["foreground", "json"],
            "stop" | "status" | "token" => &["json"],
            _ => {
                errln(&format!(
                    "unknown server command {}. Try start, stop, status or token",
                    sys::go_quote(sub)
                ));
                return 1;
            }
        };
        let mut foreground = false;
        let mut as_json = false;
        for a in &argv[1..] {
            let Some(name) = a.strip_prefix("--") else {
                errln(&format!("server {sub} does not take {}", sys::go_quote(a)));
                return 1;
            };
            if !known.contains(&name) {
                let mut flags: Vec<String> = known.iter().map(|k| format!("--{k}")).collect();
                flags.sort();
                errln(&format!(
                    "unknown flag --{name} for server {sub}. Known flags: {}",
                    flags.join(", ")
                ));
                return 1;
            }
            match name {
                "foreground" => foreground = true,
                _ => as_json = true,
            }
        }
        match sub.as_str() {
            "start" => {
                if !remote.is_empty() {
                    errln("coppice server start does not take --remote. Start the server on the remote host itself.");
                    return 1;
                }
                self.server_start(foreground, as_json)
            }
            "stop" => self.server_stop(remote, as_json),
            "status" => self.server_status(remote, as_json),
            _ => self.server_token(as_json),
        }
    }

    fn started_line(&self, as_json: bool) -> String {
        let log = log_path(&self.data_dir);
        if as_json {
            return crate::gojson::marshal(&crate::gojson::map(vec![
                ("socket", json!(self.socket.to_string_lossy())),
                ("log", json!(log.to_string_lossy())),
            ]));
        }
        format!(
            "coppice is listening on {}\nLogs: {}",
            self.socket.display(),
            log.display()
        )
    }

    fn server_start(&self, foreground: bool, as_json: bool) -> i32 {
        if !foreground {
            if UnixStream::connect(&self.socket).is_ok() {
                if as_json {
                    out(&format!(
                        "{}\n",
                        crate::gojson::marshal(&crate::gojson::map(vec![
                            ("already_running", json!(true)),
                            ("socket", json!(self.socket.to_string_lossy())),
                        ]))
                    ));
                } else {
                    out(&format!(
                        "already running. Socket: {}\n",
                        self.socket.display()
                    ));
                }
                return 0;
            }
            if let Err(e) = client::start_background_server(&self.socket, &self.data_dir) {
                errln(&e);
                return 1;
            }
            let deadline = Instant::now() + Duration::from_secs(5);
            while Instant::now() < deadline {
                if UnixStream::connect(&self.socket).is_ok() {
                    out(&format!("{}\n", self.started_line(as_json)));
                    return 0;
                }
                thread::sleep(Duration::from_millis(50));
            }
            errln(&format!(
                "started a server but {} never accepted a connection. Check {}.",
                self.socket.display(),
                log_path(&self.data_dir).display()
            ));
            return 3;
        }

        // Signals are blocked before any thread starts, so only the waiter
        // below ever takes them.
        let signals = sys_signals::block();
        let s = Server::new(server::Config {
            socket_path: self.socket.clone(),
            data_dir: self.data_dir.clone(),
            start_dir: PathBuf::new(),
            voice: voice_config(),
        });
        if let Err(e) = s.acquire_start_lock() {
            errln(&e.to_string());
            return 1;
        }
        let stopper = s.clone();
        let _ = s.handle(
            "server.stop",
            Arc::new(move |_s, _c, r| {
                let st = stopper.clone();
                thread::spawn(move || st.close());
                crate::proto::ok_resp(&r.id, crate::gojson::map(vec![("stopping", json!(true))]))
            }),
        );
        s.start_detection();
        let ln = match s.listen() {
            Ok(l) => l,
            Err(e) => {
                errln(&e.to_string());
                s.close();
                return 1;
            }
        };
        // Restore runs after listen: a pane's hook that dials the socket
        // waits in the backlog instead of being refused.
        if let Err(e) = s.restore() {
            errln(&e.to_string());
            s.close();
            return 1;
        }
        // Voice starts on its own thread, so a model that loads slowly
        // never holds up the socket or the started line.
        s.voice.ensure();
        let (enabled, settings) = load_plugins_and_config();
        s.set_plugins(crate::plugins::policy_specs(&enabled));
        let runner = crate::plugins::Runner::new(
            self.socket.to_string_lossy().into_owned(),
            settings,
            Arc::new(server::plugins::PluginHost(s.clone())),
        );
        runner.start(&enabled);
        // The phone server is opt-in and reads its own settings from
        // web.json. A file that does not read is a warning, not a reason to
        // refuse the whole server.
        let web_running = match crate::web::serve::auto_start(
            &crate::web::config::config_path(&self.data_dir),
            crate::web::serve::Options {
                dial: crate::web::upstream::Dialer {
                    path: self.socket.clone(),
                },
                tokens: crate::web::token::TokenStore::new(crate::web::config::token_path(
                    &self.data_dir,
                )),
                gate_root: crate::web::config::gate_root(&self.data_dir),
                push: None,
                voice_url: String::new(),
                voice_token_file: String::new(),
                views: crate::web::serve::views_of(&enabled),
                events: None,
            },
        ) {
            Ok(r) => Some(r),
            Err(e) => {
                errln(&format!("web: the phone server did not start: {e}"));
                None
            }
        };
        let waiter = s.clone();
        thread::spawn(move || {
            let mut first = true;
            loop {
                sys_signals::wait(&signals);
                if first {
                    first = false;
                    let w = waiter.clone();
                    thread::spawn(move || w.close());
                    continue;
                }
                errln("stopping now");
                std::process::exit(1);
            }
        });
        out(&format!("{}\n", self.started_line(as_json)));
        s.serve(ln);
        if let Some(w) = web_running {
            w.stop.store(true, std::sync::atomic::Ordering::SeqCst);
        }
        // No policy outlives the server.
        runner.stop();
        s.close();
        0
    }

    fn server_stop(&self, remote: &str, as_json: bool) -> i32 {
        let cl = if remote.is_empty() {
            dial_existing(&self.socket, None)
        } else {
            dial_remote(remote)
        };
        let mut cl = match cl {
            Ok(c) => c,
            Err(e) => {
                if remote.is_empty() {
                    if as_json {
                        out("{\"stopped\":false,\"note\":\"no server is running\"}\n");
                    } else {
                        out("no server is running\n");
                    }
                    return 0;
                }
                errln(&e);
                return 3;
            }
        };
        if let Err(e) = cl.call("server.stop", Map::new()) {
            return self.report(&mut cl, e);
        }
        cl.close();
        if !remote.is_empty() {
            if as_json {
                out("{\"stopped\":true,\"note\":\"the remote server is stopping\"}\n");
            } else {
                out("the remote server is stopping\n");
            }
            return 0;
        }
        let deadline = Instant::now() + Duration::from_secs(8);
        while Instant::now() < deadline {
            if let Ok(false) = server::lock::held(&self.data_dir) {
                if as_json {
                    out("{\"stopped\":true}\n");
                } else {
                    out("stopped\n");
                }
                return 0;
            }
            thread::sleep(Duration::from_millis(50));
        }
        errln(&format!(
            "the server did not stop within 8 s. Check {}.",
            log_path(&self.data_dir).display()
        ));
        1
    }

    fn server_status(&self, remote: &str, as_json: bool) -> i32 {
        let mut cl = match self.dial(remote) {
            Ok(c) => c,
            Err(code) => return code,
        };
        let res = match cl.call("server.status", Map::new()) {
            Ok(r) => r,
            Err(e) => return self.report(&mut cl, e),
        };
        cl.close();
        if as_json {
            out(&format!("{}\n", gofmt::marshal(&Value::Object(res))));
            return 0;
        }
        out(&status_text(&res));
        0
    }

    fn server_token(&self, as_json: bool) -> i32 {
        let note = format!(
            "coppice has no token. The socket is the credential. It is {}, mode 0600, and the server refuses any peer whose uid is not yours. To reach it from another machine, run: coppice --remote ssh://HOST pane list.",
            self.socket.display()
        );
        if as_json {
            out(&format!(
                "{}\n",
                crate::gojson::marshal(&crate::gojson::map(vec![
                    ("token", Value::Null),
                    ("note", json!(note)),
                ]))
            ));
            return 0;
        }
        out(&format!(
            "coppice has no token. The socket is the credential.\nIt is {}, mode 0600, and the server refuses any peer whose uid is not yours.\nTo reach it from another machine, run: coppice --remote ssh://HOST pane list\n",
            self.socket.display()
        ));
        0
    }

    fn run_stdio(&self) -> i32 {
        if let Err(e) = client::proxy(&self.socket, &self.data_dir) {
            errln(&e);
            return 3;
        }
        0
    }

    fn run_floor(&self, remote: &str) -> i32 {
        if !remote.is_empty() {
            errln("the floor needs a local socket; it is not wired for --remote yet. SSH into the host and run coppice there.");
            return 1;
        }
        if !std::io::stdin().is_terminal() {
            errln(USAGE);
            return 1;
        }
        let cwd = match getwd() {
            Ok(d) => d,
            Err(e) => {
                errln(&format!("cannot read the working directory: {e}"));
                return 1;
            }
        };
        match dial(&self.socket, &self.data_dir) {
            Ok(mut cl) => cl.close(),
            Err(e) => {
                errln(&e);
                return 3;
            }
        }
        let cfg = match config::load() {
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                return 1;
            }
            Ok(Some(c)) => c,
            Ok(None) => {
                let found = config::discover();
                let mut stdout = std::io::stdout();
                match crate::tui::first_run(&found, &mut std::io::stdin(), &mut stdout) {
                    Ok(c) => c,
                    Err(_) => return 1,
                }
            }
        };
        let leave = match cfg.leave_key() {
            Ok(b) => b,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        let talk = match cfg.talk_key(leave) {
            Ok(b) => b,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        let tty_out = is_terminal(1);
        if tty_out {
            out("\x1b[?1049h\x1b[2J\x1b[H");
        }
        // A web config that does not read leaves the address empty, which
        // the floor reports as a web server that is off.
        let web_cfg = crate::web::config::load(&crate::web::config::config_path(&self.data_dir))
            .unwrap_or_default();
        let (ps, _) = crate::plugins::load_enabled(&cfg.enabled_plugins());
        let views: Vec<String> = crate::web::serve::views_of(&ps)
            .into_iter()
            .map(|v| v.id)
            .collect();
        let keys = crate::attach::keys()
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let res = crate::tui::run(
            crate::tui::Options {
                socket: self.socket.clone(),
                size: Box::new(|| term_size(1).unwrap_or((120, 40))),
                cwd,
                default: cfg.default.clone(),
                data_dir: self.data_dir.display().to_string(),
                leave,
                views,
                web_url: crate::web::config::local_url(&web_cfg),
                talk,
                out: Box::new(std::io::stdout()),
                tty: is_terminal(0),
            },
            &keys,
        );
        drop(keys);
        if tty_out {
            out("\x1b[?1049l");
        }
        match res {
            Ok(()) => 0,
            Err(e) => {
                errln(&e.text());
                if matches!(e, crate::tui::FloorErr::Gone(_)) {
                    3
                } else {
                    1
                }
            }
        }
    }

    fn run_attach(&self, remote: &str, argv: &[String]) -> i32 {
        if !remote.is_empty() {
            errln("coppice attach needs a local socket; it is not wired for --remote yet. SSH into the host and run coppice attach there.");
            return 1;
        }
        let mut pane_id = String::new();
        if let Some(a) = argv.first() {
            if a.starts_with("--") {
                errln(&format!(
                    "unknown flag {}. coppice attach's only argument is a pane id.",
                    sys::go_quote(a)
                ));
                return 1;
            }
            pane_id = a.clone();
        }
        if !std::io::stdin().is_terminal() {
            errln("attach needs a terminal. Run it from a shell, not a pipe.");
            return 1;
        }
        let leave = match config::load() {
            Err(e) => {
                errln(&format!("cannot read {}: {e}", config::path().display()));
                return 1;
            }
            Ok(c) => match c.unwrap_or_default().leave_key() {
                Ok(b) => b,
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            },
        };
        if pane_id.is_empty() {
            let Ok(mut cl) = dial_existing(&self.socket, None) else {
                errln(&format!(
                    "cannot reach the server at {}. Run: coppice server start",
                    self.socket.display()
                ));
                return 3;
            };
            let res = cl.call("pane.list", Map::new());
            cl.close();
            let res = match res {
                Ok(r) => r,
                Err(e) => return self.report(&mut cl, e),
            };
            for r in res
                .get("panes")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default()
            {
                if r.get("closed").and_then(Value::as_bool) != Some(true) {
                    pane_id = r
                        .get("id")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .to_string();
                    break;
                }
            }
            if pane_id.is_empty() {
                errln("there are no open panes. Run: coppice pane create --cwd . -- claude");
                return 1;
            }
        }
        if let Some((kind, label)) = self.pane_kind(&pane_id) {
            if kind == "headless" {
                let name = if label.is_empty() {
                    pane_id.clone()
                } else {
                    label
                };
                errln(&format!("{name} takes whole messages, not raw keystrokes. Open the floor and type in its window, or run: coppice agent prompt {pane_id} \"...\""));
                return 1;
            }
        }
        let (cols, rows) = term_size(1).unwrap_or((120, 40));
        let tty = is_terminal(1);
        if tty {
            out("\x1b[?1049h\x1b[2J\x1b[H");
        }
        let resize = crate::attach::ResizeWatch::start(self.socket.clone(), pane_id.clone());
        let r = crate::attach::run(crate::attach::Options {
            socket: self.socket.clone(),
            pane: pane_id,
            cols,
            rows,
            leave,
            keys: None,
            end_on_exit: false,
        });
        resize.stop();
        if tty {
            out("\x1b[?1049l");
        }
        match r {
            Ok(()) => 0,
            Err(e) => {
                errln(&e.to_string());
                if matches!(e, crate::attach::Error::ServerGone(_)) {
                    3
                } else {
                    1
                }
            }
        }
    }

    /// One pane's kind and label from pane.list, on a short connection.
    fn pane_kind(&self, id: &str) -> Option<(String, String)> {
        let mut cl = dial_existing(&self.socket, None).ok()?;
        let res = cl.call("pane.list", Map::new()).ok()?;
        cl.close();
        for r in res.get("panes").and_then(Value::as_array)? {
            if r.get("id").and_then(Value::as_str) == Some(id) {
                let s = |k: &str| r.get(k).and_then(Value::as_str).unwrap_or("").to_string();
                return Some((s("kind"), s("label")));
            }
        }
        None
    }

    fn run_tmux_mirror(&self, remote: &str, argv: &[String]) -> i32 {
        if !remote.is_empty() {
            errln("coppice tmux-mirror needs a local socket; it is not wired for --remote yet. SSH into the host and run it there.");
            return 1;
        }
        let mut session = String::new();
        let mut tmux_socket = String::new();
        let mut a = argv;
        while let Some(f) = a.first() {
            match f.as_str() {
                "--session" => {
                    match a.get(1) {
                        Some(v) if !v.is_empty() => session = v.clone(),
                        _ => {
                            errln("--session needs a name");
                            return 1;
                        }
                    }
                    a = &a[2..];
                }
                "--tmux-socket" => {
                    match a.get(1) {
                        Some(v) if !v.is_empty() => tmux_socket = v.clone(),
                        _ => {
                            errln("--tmux-socket needs a path");
                            return 1;
                        }
                    }
                    a = &a[2..];
                }
                _ => {
                    errln(&format!(
                        "unknown flag {}. coppice tmux-mirror takes --session and --tmux-socket.",
                        sys::go_quote(f)
                    ));
                    return 1;
                }
            }
        }
        crate::tmuxmirror::run(&self.socket, session, tmux_socket)
    }
}

/// What coppice.toml's [voice] table asks of the server. A file that does
/// not read might have said enabled = false, so voice stays off.
fn voice_config() -> server::voice::VoiceConfig {
    match config::load() {
        Err(e) => server::voice::VoiceConfig {
            off: true,
            off_reason: format!(
                "Voice is off: {} does not read ({e}).",
                config::path().display()
            ),
            off_fix: "Fix the file, then start the coppice server again".into(),
            ..Default::default()
        },
        Ok(c) => {
            let c = c.unwrap_or_default();
            server::voice::VoiceConfig {
                off: !c.voice_on(),
                url: c.voice.url.clone(),
                token_file: c.voice.token_file.clone(),
                args: c.voice.args.clone(),
                ..Default::default()
            }
        }
    }
}

/// The plugins coppice.toml enables, each one that did not load printed,
/// and the operator's plugin tables. A config that does not parse runs no
/// plugin.
fn load_plugins_and_config() -> (
    Vec<crate::plugins::Plugin>,
    std::collections::BTreeMap<String, toml::Table>,
) {
    let cfg = match config::load() {
        Ok(c) => c.unwrap_or_default(),
        Err(e) => {
            errln(&format!(
                "plugins: cannot read {}: {e}. No plugin runs.",
                config::path().display()
            ));
            return (Vec::new(), Default::default());
        }
    };
    let (ps, probs) = crate::plugins::load_enabled(&cfg.enabled_plugins());
    for p in probs {
        errln(&format!("plugins: {p}"));
    }
    (ps, cfg.plugin)
}

/// Go's filepath.EvalSymlinks, or path itself when it cannot resolve.
fn resolve_symlinks(p: &str) -> String {
    match std::fs::canonicalize(p) {
        Ok(r) => r.to_string_lossy().into_owned(),
        Err(_) => p.to_string(),
    }
}

fn or_dash(v: Option<&Value>) -> Value {
    match v {
        None | Some(Value::Null) => json!("-"),
        Some(Value::String(s)) if s.is_empty() => json!("-"),
        Some(x) => x.clone(),
    }
}

/// The table, or the lines of scalars, for a person.
fn print_human(group: &str, verb: &str, res: &Map<String, Value>) {
    let key = verbs::plural_key(group, verb);
    if !key.is_empty() {
        if let Some(rows) = res.get(key).and_then(Value::as_array) {
            let mut s = String::new();
            for r in rows {
                let empty = Map::new();
                let m = r.as_object().unwrap_or(&empty);
                let f = |k: &str, w: usize| gofmt::v(&or_dash(m.get(k)), w);
                if group == "task" {
                    s.push_str(&format!(
                        "{} {} {} {} {}\n",
                        f("id", 6),
                        f("state", 10),
                        f("parent", 6),
                        f("label", 20),
                        f("worktree", 0)
                    ));
                    continue;
                }
                if group == "project" {
                    let mark = if m.get("pinned").and_then(Value::as_bool) == Some(true) {
                        "*"
                    } else {
                        " "
                    };
                    s.push_str(&format!("{mark} {} {}\n", f("name", 16), f("path", 0)));
                    continue;
                }
                let id = m
                    .get("id")
                    .or_else(|| m.get("pane"))
                    .cloned()
                    .unwrap_or(json!("-"));
                let mut line = format!(
                    "{} {} {} {} {}",
                    gofmt::v(&id, 10),
                    f("kind", 10),
                    f("state", 10),
                    f("source", 10),
                    f("label", 0)
                );
                if m.get("closed").and_then(Value::as_bool) == Some(true) {
                    line.push_str(" closed");
                }
                s.push_str(&line);
                s.push('\n');
            }
            out(&s);
            return;
        }
    }
    let mut keys: Vec<&String> = res.keys().collect();
    keys.sort();
    if keys.is_empty() {
        out("ok\n");
        return;
    }
    let mut s = String::new();
    for k in keys {
        s.push_str(&format!("{k} {}\n", gofmt::v(&res[k], 0)));
    }
    out(&s);
}

/// server.status for a person.
fn status_text(res: &Map<String, Value>) -> String {
    let n = |k: &str| gofmt::number(res.get(k));
    let v = |x: Option<&Value>| gofmt::v(x.unwrap_or(&Value::Null), 0);
    let mut b = String::new();
    b.push_str(&format!("pid {}\n", n("pid")));
    b.push_str(&format!("socket {}\n", v(res.get("socket"))));
    if let Some(dd) = res.get("data_dir") {
        b.push_str(&format!("data dir {}\n", v(Some(dd))));
    }
    b.push_str(&format!("panes {}, {} live\n", n("panes"), n("panes_live")));
    if res.contains_key("resumable") {
        b.push_str(&format!("{} resumable\n", n("resumable")));
    }
    b.push_str(&format!("up {}s\n", n("uptime_s")));
    b.push_str(&format!("{}\n", v(res.get("restart_note"))));
    if let Some(Value::Object(r)) = res.get("restore") {
        let rn = |k: &str| gofmt::number(r.get(k));
        b.push_str(&format!(
            "restore: {} panes, {} already closed, {} resumed, {} marked done, {} marked unknown\n",
            rn("panes"),
            rn("already_closed"),
            rn("resumed"),
            rn("marked_done"),
            rn("marked_unknown")
        ));
        if let Some(Value::Array(notes)) = r.get("notes") {
            for note in notes {
                b.push_str(&format!("  {}\n", gofmt::v(note, 0)));
            }
        }
    }
    if let Some(Value::Array(ws)) = res.get("detection_warnings") {
        if !ws.is_empty() {
            b.push_str("\nagent detection warnings:\n");
            for w in ws {
                b.push_str(&format!("  {}\n", gofmt::v(w, 0)));
            }
        }
    }
    b
}

/// SIGINT and SIGTERM, taken by one waiting thread.
mod sys_signals {
    pub struct Set(libc::sigset_t);

    /// Blocks SIGINT and SIGTERM in this thread and every thread it starts
    /// after this.
    pub fn block() -> Set {
        // SAFETY: the set is initialized by sigemptyset before use.
        unsafe {
            let mut set: libc::sigset_t = std::mem::zeroed();
            libc::sigemptyset(&mut set);
            libc::sigaddset(&mut set, libc::SIGINT);
            libc::sigaddset(&mut set, libc::SIGTERM);
            libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut());
            Set(set)
        }
    }

    /// Waits for one of the blocked signals.
    pub fn wait(s: &Set) {
        let mut sig: libc::c_int = 0;
        // SAFETY: the set is initialized and sig is a live int.
        unsafe {
            libc::sigwait(&s.0, &mut sig);
        }
    }
}
