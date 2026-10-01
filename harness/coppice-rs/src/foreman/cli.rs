//! `coppice foreman`: read the foreman's chat. It never takes the chat's
//! lock and never writes. The twin of Go's internal/foreman cli.go.

use std::io::Write;
use std::path::{Path, PathBuf};

use super::chat::{flat, Chat, Message};
use super::order::parse_name;
use super::turns::{read_turns, stats};

/// The help for coppice foreman.
pub const USAGE: &str = "coppice foreman: read the foreman's chat. It never writes.

  coppice foreman log [--day YYYY-MM-DD] [--json]   the messages, one per line
  coppice foreman view                              the view a turn would see
  coppice foreman zoom ID+N                         open a node; N=1 is the message
  coppice foreman date ID                           when message ID was logged
  coppice foreman stats                             the turns and the cache read share

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
    let mut words: Vec<String> = Vec::new();
    let mut k = 0;
    while k < argv.len() {
        let a = argv[k].as_str();
        match a {
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
            "--json" => as_json = true,
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
    let want = match verb {
        "log" | "view" | "stats" => 0,
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
    let c = match Chat::open_read_only(&dir) {
        Ok(c) => c,
        Err(e) => {
            let _ = writeln!(errw, "no foreman chat at {}: {e}", dir.display());
            return 1;
        }
    };
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
                    let _ = writeln!(errw, "{e}");
                    return 1;
                }
            };
            match c.zoom(node.first(), node.count()) {
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
