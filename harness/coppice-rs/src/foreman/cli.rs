//! `coppice foreman`: read the foreman's chat. It never takes the chat's
//! lock and writes nothing but import.json (import). The twin of Go's internal/foreman cli.go.

use std::io::Write;
use std::path::{Path, PathBuf};

use super::chat::{flat, Chat, Message};
use super::order::parse_name;
use super::turns::{read_turns, stats};

/// The help for coppice foreman.
pub const USAGE: &str = "coppice foreman: read the foreman's chat. Only import changes anything.

  coppice foreman log [--day YYYY-MM-DD] [--json]   the messages, one per line
  coppice foreman view                              the view a turn would see
  coppice foreman zoom ID+N [--offset B] [--size B] open a node; N=1 is the message;
                                                    a long node comes in pages
  coppice foreman date ID                           when message ID was logged
  coppice foreman stats                             the turns and the cache read share
  coppice foreman status [--json]                   the turn profiles and the state of each:
                                                    ready, cooling until T, or failing
  coppice foreman queue [--json]                    the jobs that wait for a machine, and the
                                                    pool entries: free, busy or cooling
  coppice foreman queue add --project P --brief TEXT --profile POOL [--model M]
                                                    add a background job; it runs on a pool
                                                    entry that has been idle (pools.toml)
  coppice foreman import [--from claude|codex|all] [--since DATE] [--tools]
                         [--dry-run | --pause | --resume | --status [--json]]
                                                    bring old Claude Code and Codex chats
                                                    into the foreman, only if you ask;
                                                    --dry-run counts them and changes nothing

  --data-dir DIR   the foreman dir itself. Without it, coppice --data-dir X
                   reads X/foreman (for coppice's default data dir, the
                   foreman dir beside it, as below); with neither, $OPENDAISUGI_HOME/foreman,
                   else $XDG_DATA_HOME/opendaisugi/foreman when ~/.opendaisugi
                   does not exist, else ~/.opendaisugi/foreman.";

/// Runs coppice foreman with the words after "foreman". coppice_data_dir
/// is coppice's own --data-dir, or None. It returns the exit code: 0 done,
/// 1 a user error.
pub fn main(
    argv: &[String],
    coppice_data_dir: Option<&Path>,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    let (mut dir, mut day, mut as_json) = (String::new(), String::new(), false);
    let mut imp = super::import::ImportFlags::new();
    let mut words: Vec<String> = Vec::new();
    let mut qreq = super::pool::QueueRequest::default();
    let (mut offset, mut size) = (0i64, 0i64);
    let mut k = 0;
    while k < argv.len() {
        let a = argv[k].as_str();
        match a {
            "--offset" | "--size" => {
                if k + 1 >= argv.len() {
                    let _ = writeln!(errw, "{a} needs a value");
                    return 1;
                }
                match super::order::go_parse_int(&argv[k + 1]).filter(|v| *v >= 0) {
                    Some(v) => {
                        if a == "--offset" {
                            offset = v;
                        } else {
                            size = v;
                        }
                    }
                    None => {
                        let _ = writeln!(
                            errw,
                            "{a} needs a number of bytes, not {}",
                            crate::sys::go_quote(&argv[k + 1])
                        );
                        return 1;
                    }
                }
                k += 1;
            }
            "--data-dir" | "--day" => {
                if k + 1 >= argv.len() {
                    let _ = writeln!(errw, "{a} needs a value");
                    return 1;
                }
                if a == "--data-dir" {
                    dir = argv[k + 1].clone();
                } else {
                    day = argv[k + 1].clone();
                }
                k += 1;
            }
            "--project" | "--brief" | "--profile" | "--model" => {
                if k + 1 >= argv.len() {
                    let _ = writeln!(errw, "{a} needs a value");
                    return 1;
                }
                let v = argv[k + 1].clone();
                match a {
                    "--project" => qreq.project = v,
                    "--brief" => qreq.brief = v,
                    "--profile" => qreq.profile = v,
                    _ => qreq.model = v,
                }
                k += 1;
            }
            "--json" => as_json = true,
            "--from" | "--since" => {
                if k + 1 >= argv.len() {
                    let _ = writeln!(errw, "{a} needs a value");
                    return 1;
                }
                if a == "--from" {
                    imp.from = argv[k + 1].clone();
                } else {
                    imp.since = argv[k + 1].clone();
                }
                k += 1;
            }
            "--dry-run" | "--pause" | "--resume" | "--status" | "--tools" => imp.set(a),
            "--help" | "-h" => {
                let _ = writeln!(out, "{USAGE}");
                return 0;
            }
            _ => {
                if a.starts_with("--") {
                    let _ = writeln!(errw, "unknown option {}\n{USAGE}", crate::sys::go_quote(a));
                    return 1;
                }
                words.push(a.to_string());
            }
        }
        k += 1;
    }
    if words.is_empty() {
        let _ = writeln!(errw, "{USAGE}");
        return 1;
    }
    let mut dir = PathBuf::from(dir);
    if dir.as_os_str().is_empty() {
        if let Some(d) = coppice_data_dir.filter(|d| !d.as_os_str().is_empty()) {
            dir = super::log_dir(d);
        }
    }
    if dir.as_os_str().is_empty() {
        dir = super::default_dir();
    }
    let (verb, args) = (words[0].as_str(), &words[1..]);
    if verb == "queue" {
        return super::pool::run_queue(&dir, args, &qreq, as_json, out, errw);
    }
    let want = match verb {
        "log" | "view" | "stats" | "status" | "import" => 0,
        "zoom" | "date" => 1,
        _ => {
            let _ = writeln!(
                errw,
                "unknown foreman command {}\n{USAGE}",
                crate::sys::go_quote(verb)
            );
            return 1;
        }
    };
    if args.len() != want {
        let _ = writeln!(errw, "foreman {verb} takes {want} argument(s)\n{USAGE}");
        return 1;
    }
    if verb == "import" {
        // The sessions of coppice's own are never imported (FM-R-78).
        imp.skip = vec![super::default_dir().to_string_lossy().into_owned()];
        if let Some(d) = coppice_data_dir.filter(|d| !d.as_os_str().is_empty()) {
            imp.skip.push(d.to_string_lossy().into_owned());
        }
        return super::import::run_import(&dir, &imp, as_json, out, errw);
    }
    if verb == "stats" {
        return match read_turns(&dir) {
            Ok(recs) => {
                let _ = write!(out, "{}", stats(&recs));
                0
            }
            Err(e) => {
                let _ = writeln!(errw, "cannot read the turns in {}: {e}", dir.display());
                1
            }
        };
    }
    if verb == "status" {
        return status(&dir, as_json, out, errw);
    }
    let c = match Chat::open_read_only(&dir) {
        Ok(c) => c,
        Err(e) => {
            let _ = writeln!(errw, "no foreman chat at {}: {e}", dir.display());
            return 1;
        }
    };
    if !c.notice().is_empty() {
        let _ = writeln!(errw, "note: {}", c.notice());
    }
    match verb {
        "log" => {
            let msgs: Vec<Message> = if !day.is_empty() {
                match c.day(&day) {
                    Ok(m) => m,
                    Err(e) => {
                        let _ = writeln!(errw, "{e}");
                        return 1;
                    }
                }
            } else {
                let mut all = Vec::new();
                for i in 0..c.count() {
                    match c.message(i) {
                        Ok(m) => all.push(m),
                        Err(e) => {
                            let _ = writeln!(errw, "{e}");
                            return 1;
                        }
                    }
                }
                all
            };
            for m in msgs {
                if as_json {
                    let _ = out.write_all(m.encode().as_bytes());
                    continue;
                }
                let _ = writeln!(out, "{} {} {}|{}", m.i, m.date, m.kind, flat(&m.text));
            }
        }
        "view" => {
            let _ = write!(out, "{}", c.render());
        }
        "zoom" => {
            let mut s = args[0].clone();
            if !s.contains('+') {
                s.push_str("+1");
            }
            let node = match parse_name(&s) {
                Ok(n) => n,
                Err(e) => {
                    let _ = writeln!(errw, "{e}.{}", c.root_hint());
                    return 1;
                }
            };
            match c.zoom_page(node.first(), node.count(), offset, size) {
                Ok(text) => {
                    let _ = write!(out, "{text}");
                    if !text.ends_with('\n') {
                        let _ = writeln!(out);
                    }
                }
                Err(e) => {
                    let _ = writeln!(errw, "{e}");
                    return 1;
                }
            }
        }
        "date" => {
            let Some(i) = super::order::go_parse_int(&args[0]) else {
                let _ = writeln!(
                    errw,
                    "date needs a message id, not {}",
                    crate::sys::go_quote(&args[0])
                );
                return 1;
            };
            match c.date(i) {
                Ok(d) => {
                    let _ = writeln!(out, "{d}");
                }
                Err(e) => {
                    let _ = writeln!(errw, "{e}");
                    return 1;
                }
            }
        }
        _ => {}
    }
    0
}

/// The clock of status, in Unix seconds; a test sets it.
pub static NOW: std::sync::Mutex<Option<i64>> = std::sync::Mutex::new(None);

fn now_secs() -> i64 {
    if let Some(t) = *NOW.lock().unwrap_or_else(|e| e.into_inner()) {
        return t;
    }
    super::chat::unix_secs(std::time::SystemTime::now())
}

/// Prints the chain of turn profiles from the state file the server keeps.
/// The server writes the file when the chat opens, so a missing file means no
/// server has run in chat mode here.
fn status(dir: &Path, as_json: bool, out: &mut dyn Write, errw: &mut dyn Write) -> i32 {
    let Some(c) = super::failover::Chain::read(dir, now_secs()) else {
        let _ = writeln!(
            errw,
            "no failover state in {}. Start the coppice server in chat mode first",
            dir.display()
        );
        return 1;
    };
    let now = now_secs();
    if as_json {
        let _ = write!(out, "{}", c.status_json(now));
        return 0;
    }
    let rows = c.status(now);
    let w = rows.iter().map(|r| r.name.len()).max().unwrap_or(0);
    for (i, r) in rows.iter().enumerate() {
        let mut line = match r.state.as_str() {
            "cooling" => {
                let mut l = format!("{:<w$}  cooling until {}", r.name, r.until);
                if !r.reason.is_empty() {
                    l.push_str(&format!(" ({})", r.reason));
                }
                l
            }
            "failing" => format!("{:<w$}  failing ({})", r.name, r.reason),
            _ => format!("{:<w$}  {}", r.name, r.state),
        };
        if c.last_ran() == Some(i) {
            line.push_str(" (the last turn ran here)");
        }
        let _ = writeln!(out, "{line}");
    }
    0
}
