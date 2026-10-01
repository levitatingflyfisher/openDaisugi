//! sprig's command line: one task in, one answer out. The answer goes to
//! stdout and everything else to stderr; --json for scripts; real exit
//! codes. The flags parse as Go's flag package parses them.

use crate::agent::{Agent, Model};
use crate::gate::{AllowAll, DaisugiGate, Executor, Gate, DEFAULT_GATE_CMD};
use crate::gostr::{fields, trim_space};
use crate::json::{encode, jmap, J};
use crate::session::{head_path, history_from_entries, new_id, SessionObserver, SessionWriter};
use std::io::{Read, Write};
use std::rc::Rc;
use std::time::Duration;

/// What `sprig --help` prints, as Go's flag package prints it.
pub const USAGE: &str = "usage: sprig [flags] \"<task>\"     (or:  echo \"<task>\" | sprig -)\n\
a minimal coding-agent harness \u{2014} the answer goes to stdout, logs to stderr.\n\
\n\
examples:\n  sprig \"add a test for parse_url\"\n  sprig --json \"list the TODOs\" | jq -r .answer\n\
\n\
flags:\n  -gate\n    \tverify each tool call against the envelope, fail-closed\n  \
-gate-cmd string\n    \tthe out-of-process envelope gate to call when --gate is set \
(default \"daisugi gate check --mode enforce\")\n  -json\n    \temit the result as JSON (for scripts and jq)\n  \
-max-turns int\n    \tgive up after this many model turns (default 20)\n  -resume string\n    \t\
resume this session id from --session-dir: reopen its file, rebuild history from its head, and append\n  \
-session string\n    \tsession id to write under --session-dir (default: a fresh one)\n  \
-session-dir string\n    \twrite the session tree (JSONL) here \u{2014} the same shape daisugi's gate \
writes on path D. Empty = off.\n  -version\n    \tprint the version and exit\n";

/// The parsed flags.
#[derive(Debug, PartialEq)]
pub struct Flags {
    pub version: bool,
    pub json: bool,
    pub gate: bool,
    pub max_turns: i64,
    pub gate_cmd: Vec<u8>,
    pub session_dir: Vec<u8>,
    pub session: Vec<u8>,
    pub resume: Vec<u8>,
    /// The arguments after the flags.
    pub args: Vec<Vec<u8>>,
}

/// strconv's underscoreOK: underscores only between digits.
fn underscore_ok(s: &[u8]) -> bool {
    let lower = |c: u8| c | 0x20;
    let mut saw = b'^';
    let mut i = 0;
    let mut hex = false;
    if s.len() >= 2 && s[0] == b'0' && matches!(lower(s[1]), b'b' | b'o' | b'x') {
        i = 2;
        saw = b'0';
        hex = lower(s[1]) == b'x';
    }
    while i < s.len() {
        let c = s[i];
        i += 1;
        if c.is_ascii_digit() || hex && (b'a'..=b'f').contains(&lower(c)) {
            saw = b'0';
            continue;
        }
        if c == b'_' {
            if saw != b'0' {
                return false;
            }
            saw = b'_';
            continue;
        }
        if saw == b'_' {
            return false;
        }
        saw = b'!';
    }
    saw != b'_'
}

/// Go's strconv.ParseInt(s, 0, 64), its error as flag words it.
pub fn parse_int(s: &[u8]) -> Result<i64, &'static str> {
    const SYNTAX: &str = "parse error";
    const RANGE: &str = "value out of range";
    if s.is_empty() {
        return Err(SYNTAX);
    }
    let (neg, body) = match s[0] {
        b'+' => (false, &s[1..]),
        b'-' => (true, &s[1..]),
        _ => (false, s),
    };
    if body.is_empty() {
        return Err(SYNTAX);
    }
    let lower = |c: u8| c | 0x20;
    let (base, digits): (u64, &[u8]) = if body[0] == b'0' {
        if body.len() >= 3 && lower(body[1]) == b'b' {
            (2, &body[2..])
        } else if body.len() >= 3 && lower(body[1]) == b'o' {
            (8, &body[2..])
        } else if body.len() >= 3 && lower(body[1]) == b'x' {
            (16, &body[2..])
        } else {
            (8, &body[1..])
        }
    } else {
        (10, body)
    };
    let cutoff = u64::MAX / base + 1;
    let mut n: u64 = 0;
    let mut underscores = false;
    for &c in digits {
        let d = match c {
            b'_' => {
                underscores = true;
                continue;
            }
            b'0'..=b'9' => c - b'0',
            _ if lower(c).is_ascii_lowercase() => lower(c) - b'a' + 10,
            _ => return Err(SYNTAX),
        } as u64;
        if d >= base {
            return Err(SYNTAX);
        }
        if n >= cutoff {
            return Err(RANGE);
        }
        n *= base;
        let n1 = n.wrapping_add(d);
        if n1 < n {
            return Err(RANGE);
        }
        n = n1;
    }
    if underscores && !underscore_ok(body) {
        return Err(SYNTAX);
    }
    let cut = 1u64 << 63;
    if !neg && n >= cut {
        return Err(RANGE);
    }
    if neg && n > cut {
        return Err(RANGE);
    }
    Ok(if neg {
        (n as i64).wrapping_neg()
    } else {
        n as i64
    })
}

/// Go's strconv.ParseBool.
pub fn parse_bool(s: &[u8]) -> Option<bool> {
    match s {
        b"1" | b"t" | b"T" | b"TRUE" | b"true" | b"True" => Some(true),
        b"0" | b"f" | b"F" | b"FALSE" | b"false" | b"False" => Some(false),
        _ => None,
    }
}

/// Go's FlagSet.Parse with sprig's flags. Err holds what Go prints before
/// the usage (empty for -h and -help).
pub fn parse_flags(args: &[Vec<u8>]) -> Result<Flags, Vec<u8>> {
    let mut f = Flags {
        version: false,
        json: false,
        gate: false,
        max_turns: 20,
        gate_cmd: DEFAULT_GATE_CMD.as_bytes().to_vec(),
        session_dir: Vec::new(),
        session: Vec::new(),
        resume: Vec::new(),
        args: Vec::new(),
    };
    let mut i = 0;
    while i < args.len() {
        let s = &args[i];
        if s.len() < 2 || s[0] != b'-' {
            break;
        }
        let mut minuses = 1;
        if s[1] == b'-' {
            minuses = 2;
            if s.len() == 2 {
                i += 1;
                break;
            }
        }
        let mut name: &[u8] = &s[minuses..];
        if name.is_empty() || name[0] == b'-' || name[0] == b'=' {
            return Err([b"bad flag syntax: ".as_slice(), s].concat());
        }
        i += 1;
        let mut value: Option<&[u8]> = None;
        if let Some(eq) = name[1..].iter().position(|&c| c == b'=') {
            value = Some(&name[eq + 2..]);
            name = &name[..eq + 1];
        }
        let dashed = [b"-".as_slice(), name].concat();
        let is_bool = matches!(name, b"version" | b"json" | b"gate");
        let is_known = is_bool
            || matches!(
                name,
                b"max-turns" | b"gate-cmd" | b"session-dir" | b"session" | b"resume"
            );
        if !is_known {
            if name == b"help" || name == b"h" {
                return Err(Vec::new());
            }
            return Err([b"flag provided but not defined: ".as_slice(), &dashed].concat());
        }
        if is_bool {
            let v = match value {
                Some(v) => match parse_bool(v) {
                    Some(b) => b,
                    None => {
                        return Err(format!(
                            "invalid boolean value {} for {}: parse error",
                            crate::goerr::quote(v),
                            String::from_utf8_lossy(&dashed)
                        )
                        .into_bytes())
                    }
                },
                None => true,
            };
            match name {
                b"version" => f.version = v,
                b"json" => f.json = v,
                _ => f.gate = v,
            }
            continue;
        }
        let v: Vec<u8> = match value {
            Some(v) => v.to_vec(),
            None if i < args.len() => {
                i += 1;
                args[i - 1].clone()
            }
            None => return Err([b"flag needs an argument: ".as_slice(), &dashed].concat()),
        };
        match name {
            b"max-turns" => match parse_int(&v) {
                Ok(n) => f.max_turns = n,
                Err(why) => {
                    return Err([
                        format!("invalid value {} for flag ", crate::goerr::quote(&v)).as_bytes(),
                        &dashed,
                        format!(": {why}").as_bytes(),
                    ]
                    .concat())
                }
            },
            b"gate-cmd" => f.gate_cmd = v,
            b"session-dir" => f.session_dir = v,
            b"session" => f.session = v,
            _ => f.resume = v,
        }
    }
    f.args = args[i..].to_vec();
    Ok(f)
}

/// Runs one task and returns the exit code.
pub fn run(
    args: &[Vec<u8>],
    stdin: &mut dyn Read,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
    version: &str,
    new_model: &dyn Fn() -> Result<Box<dyn Model>, Vec<u8>>,
) -> i32 {
    let f = match parse_flags(args) {
        Ok(f) => f,
        Err(msg) => {
            if !msg.is_empty() {
                let _ = stderr.write_all(&[msg.as_slice(), b"\n"].concat());
            }
            let _ = stderr.write_all(USAGE.as_bytes());
            return 2;
        }
    };
    let say = |w: &mut dyn Write, parts: &[&[u8]]| {
        let _ = w.write_all(&parts.concat());
    };
    if f.version {
        say(stdout, &[version.as_bytes(), b"\n"]);
        return 0;
    }
    if !f.resume.is_empty() && f.session_dir.is_empty() {
        say(
            stderr,
            &[b"sprig: --resume needs --session-dir too (which session store to resume from)\n"],
        );
        return 1;
    }
    let joined = f.args.join(b" ".as_slice());
    let mut task = trim_space(&joined).to_vec();
    if task.is_empty() || task == b"-" {
        let mut b = Vec::new();
        let _ = stdin.read_to_end(&mut b);
        task = trim_space(&b).to_vec();
    }
    if task.is_empty() {
        say(
            stderr,
            &["sprig: no task given \u{2014} pass it as an argument or on stdin (sprig -).\ntry: sprig --help\n".as_bytes()],
        );
        return 1;
    }
    let model = match new_model() {
        Ok(m) => m,
        Err(e) => {
            say(stderr, &[b"sprig: ", &e, b"\n"]);
            return 1;
        }
    };
    let mut writer: Option<Rc<SessionWriter>> = None;
    let mut seed = Vec::new();
    let mut sid = Vec::new();
    if !f.session_dir.is_empty() {
        sid = if !f.resume.is_empty() {
            f.resume.clone()
        } else if !f.session.is_empty() {
            f.session.clone()
        } else {
            (new_id() + &new_id()).into_bytes()
        };
        let opened = if !f.resume.is_empty() {
            SessionWriter::open(&f.session_dir, &sid)
        } else {
            let cwd = crate::osx::getwd().unwrap_or_default();
            SessionWriter::create(&f.session_dir, &sid, &cwd)
        };
        let w = match opened {
            Ok(w) => w,
            Err(e) => {
                say(stderr, &[b"sprig: session tree: ", &e, b"\n"]);
                return 1;
            }
        };
        if !f.resume.is_empty() {
            match head_path(w.path()) {
                Ok(entries) => seed = history_from_entries(&entries),
                Err(e) => {
                    say(
                        stderr,
                        &[b"sprig: session tree: rebuilding history: ", &e, b"\n"],
                    );
                    return 1;
                }
            }
        }
        say(
            stderr,
            &[b"sprig: session ", &sid, b" at ", w.path(), b"\n"],
        );
        writer = Some(Rc::new(w));
    }
    let gate: Box<dyn Gate> = if f.gate {
        Box::new(DaisugiGate {
            command: fields(&f.gate_cmd),
            timeout: Duration::from_secs(10),
            session_id: sid,
        })
    } else {
        Box::new(AllowAll)
    };
    let obs: Option<Rc<dyn SessionObserver>> = writer.map(|w| w as Rc<dyn SessionObserver>);
    let mut exec = Executor::new(crate::tools::default_tools(), gate);
    exec.session = obs.clone();
    let mut agent = Agent::new(model, exec, f.max_turns);
    agent.session = obs;
    agent.seed = seed;
    let answer = match agent.run(&task) {
        Ok(a) => a,
        Err(e) => {
            say(stderr, &[b"sprig: ", &e, b"\n"]);
            return 1;
        }
    };
    if f.json {
        let j = jmap(vec![
            ("answer", J::Str(answer)),
            ("turns", J::Int(agent.history.len() as i64)),
        ]);
        say(stdout, &[&encode(&j), b"\n"]);
    } else {
        say(stdout, &[&answer, b"\n"]);
    }
    0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(xs: &[&str]) -> Vec<Vec<u8>> {
        xs.iter().map(|s| s.as_bytes().to_vec()).collect()
    }

    #[test]
    fn ints_and_bools() {
        assert_eq!(parse_int(b"0x10"), Ok(16));
        assert_eq!(parse_int(b"1_0"), Ok(10));
        assert_eq!(parse_int(b"010"), Ok(8));
        assert_eq!(parse_int(b"-0b11"), Ok(-3));
        assert_eq!(parse_int(b"+7"), Ok(7));
        assert_eq!(parse_int(b"0"), Ok(0));
        assert_eq!(parse_int(b"_1"), Err("parse error"));
        assert_eq!(parse_int(b"1__0"), Err("parse error"));
        assert_eq!(parse_int(b"08"), Err("parse error"));
        assert_eq!(parse_int(b"x"), Err("parse error"));
        assert_eq!(parse_int(b""), Err("parse error"));
        assert_eq!(parse_int(b"9223372036854775807"), Ok(i64::MAX));
        assert_eq!(parse_int(b"9223372036854775808"), Err("value out of range"));
        assert_eq!(parse_int(b"-9223372036854775808"), Ok(i64::MIN));
        assert_eq!(
            parse_int(b"99999999999999999999x"),
            Err("value out of range")
        );
        assert_eq!(parse_bool(b"T"), Some(true));
        assert_eq!(parse_bool(b"False"), Some(false));
        assert_eq!(parse_bool(b"yes"), None);
    }

    #[test]
    fn flags_follow_go() {
        let f = parse_flags(&a(&[
            "-json",
            "--max-turns=3",
            "--gate-cmd",
            "x y",
            "task",
            "--gate",
        ]))
        .unwrap();
        assert!(f.json && !f.gate);
        assert_eq!((f.max_turns, f.gate_cmd.as_slice()), (3, b"x y".as_slice()));
        assert_eq!(f.args, a(&["task", "--gate"]));
        let f = parse_flags(&a(&["--", "--version"])).unwrap();
        assert_eq!(f.args, a(&["--version"]));
        let f = parse_flags(&a(&["-", "x"])).unwrap();
        assert_eq!(f.args, a(&["-", "x"]));
        assert_eq!(
            parse_flags(&a(&["--nope"])).unwrap_err(),
            b"flag provided but not defined: -nope"
        );
        assert_eq!(
            parse_flags(&a(&["---x"])).unwrap_err(),
            b"bad flag syntax: ---x"
        );
        assert_eq!(
            parse_flags(&a(&["-=x"])).unwrap_err(),
            b"bad flag syntax: -=x"
        );
        assert_eq!(
            parse_flags(&a(&["--max-turns"])).unwrap_err(),
            b"flag needs an argument: -max-turns"
        );
        assert_eq!(
            parse_flags(&a(&["--json=maybe"])).unwrap_err(),
            b"invalid boolean value \"maybe\" for -json: parse error"
        );
        assert_eq!(
            parse_flags(&a(&["--max-turns", "x"])).unwrap_err(),
            b"invalid value \"x\" for flag -max-turns: parse error"
        );
        assert_eq!(parse_flags(&a(&["-help"])).unwrap_err(), b"");
        assert_eq!(parse_flags(&a(&["-max-turns=-1"])).unwrap().max_turns, -1);
    }

    struct Fixed;
    impl Model for Fixed {
        fn next(&mut self, _: &[crate::agent::Message]) -> Result<crate::agent::Message, Vec<u8>> {
            Ok(crate::agent::Message {
                role: "assistant",
                text: b"hi <x>".to_vec(),
                ..Default::default()
            })
        }
    }

    fn go(args: &[&str], stdin: &str) -> (i32, String, String) {
        let (mut o, mut e) = (Vec::new(), Vec::new());
        let nm = || -> Result<Box<dyn Model>, Vec<u8>> { Ok(Box::new(Fixed)) };
        let code = run(&a(args), &mut stdin.as_bytes(), &mut o, &mut e, "v1", &nm);
        (
            code,
            String::from_utf8(o).unwrap(),
            String::from_utf8(e).unwrap(),
        )
    }

    #[test]
    fn runs_follow_go() {
        assert_eq!(go(&["--version"], ""), (0, "v1\n".into(), "".into()));
        let (c, o, e) = go(&["-h"], "");
        assert_eq!((c, o.as_str(), e.as_str()), (2, "", USAGE));
        let (c, _, e) = go(&["--nope"], "");
        assert_eq!(
            (c, e),
            (2, format!("flag provided but not defined: -nope\n{USAGE}"))
        );
        assert_eq!(
            go(&[], " \n"),
            (1, "".into(), "sprig: no task given \u{2014} pass it as an argument or on stdin (sprig -).\ntry: sprig --help\n".into())
        );
        assert_eq!(
            go(&["--resume", "r", "t"], "").2,
            "sprig: --resume needs --session-dir too (which session store to resume from)\n"
        );
        assert_eq!(go(&["say", "hi"], ""), (0, "hi <x>\n".into(), "".into()));
        assert_eq!(
            go(&["--json", "-"], "task"),
            (
                0,
                "{\"answer\":\"hi \\u003cx\\u003e\",\"turns\":2}\n".into(),
                "".into()
            )
        );
    }
}
