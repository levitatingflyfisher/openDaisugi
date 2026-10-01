//! sprig, headless: one process per prompt, as codex. Each turn runs
//! `sprig --session-dir DIR --session ID [extra] -- TASK`, or `--resume ID`
//! once the session file exists. The state comes from the session tree
//! sprig writes, read from where the previous turn left off; a verdict row
//! that denies is the one blocked this adapter reports. A turn that exits
//! non-zero names sprig's last useful output line and the exit code. This
//! adapter never sends an end event. Model text goes into the grid as is.

use std::io::{Read, Seek, SeekFrom};
use std::os::fd::OwnedFd;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use super::codex::{spawn_turn, stop_turns, TurnState};
use super::{Adapter, Ev, EventsSlot, Lines, Out, Proc, StartOpts, Turns};
use crate::godec::{self, StructTy, Ty};
use crate::pane::Flag;

pub const BINARY: &str = "sprig";
const BIN_VAR: &str = "COPPICE_SPRIG_BIN";

/// How often the session tree is read while a turn runs.
const TAIL_INTERVAL: Duration = Duration::from_millis(200);
/// The cap on one read of the tree.
const MAX_TAIL_READ: u64 = 8 << 20;
/// The cap on one captured line, in runes.
const DETAIL_LINE_CAP: usize = 400;

pub struct Sprig;

/// argv up to the first literal "--".
fn before_double_dash(argv: &[String]) -> &[String] {
    match argv.iter().position(|a| a == "--") {
        Some(i) => &argv[..i],
        None => argv,
    }
}

/// Reads token a as sprig's own flag parser (Go's flag package) does: one
/// or two dashes, then a name, then "=value" or no value. Some((name,
/// value)) only for session-dir, session and resume; the value is Some
/// when it came with "=".
fn session_flag(a: &str) -> Option<(&str, Option<&str>)> {
    let b = a.strip_prefix('-')?;
    let b = b.strip_prefix('-').unwrap_or(b);
    if b.is_empty() || b.starts_with('-') || b.starts_with('=') {
        return None;
    }
    let (name, val) = match b.find('=') {
        Some(eq) => (&b[..eq], Some(&b[eq + 1..])),
        None => (b, None),
    };
    matches!(name, "session-dir" | "session" | "resume").then_some((name, val))
}

/// The last value argv gives session flag name, in any spelling sprig's
/// flag parser takes: -name, --name, with "=value" or the next word as the
/// value. The last one wins, as in sprig. Never at or past a literal "--".
/// An empty value counts as none.
pub fn flag_value(argv: &[String], name: &str) -> Option<String> {
    let head = before_double_dash(argv);
    let mut val = String::new();
    let mut i = 0;
    while i < head.len() {
        if let Some((n, inline)) = session_flag(&head[i]) {
            let v = match inline {
                Some(v) => v.to_string(),
                None if i + 1 < head.len() => {
                    i += 1;
                    head[i].clone()
                }
                None => {
                    i += 1;
                    continue;
                }
            };
            if n == name {
                val = v;
            }
        }
        i += 1;
    }
    (!val.is_empty()).then_some(val)
}

/// The session tree file for a pane with argv and session id: only an
/// absolute --session-dir and a plain file name.
pub fn tree_path(argv: &[String], session_id: &str) -> Option<String> {
    let dir = flag_value(argv, "session-dir")?;
    if !dir.starts_with('/') {
        return None;
    }
    if session_id.is_empty()
        || session_id == "."
        || session_id == ".."
        || session_id.contains('/')
        || session_id.contains('\\')
    {
        return None;
    }
    Some(crate::sys::clean(&format!("{dir}/{session_id}.jsonl")))
}

/// Splices extra in right after --session-dir's own value, never past a
/// literal "--".
fn insert_after_session_dir(argv: &[String], extra: &[String]) -> Vec<String> {
    let head = before_double_dash(argv);
    let tail = &argv[head.len()..];
    let mut pos = head.len();
    for (i, a) in head.iter().enumerate() {
        if a == "--session-dir" && i + 1 < head.len() {
            pos = i + 2;
        } else if a.starts_with("--session-dir=") {
            pos = i + 1;
        }
    }
    let mut out = head[..pos].to_vec();
    out.extend_from_slice(extra);
    out.extend_from_slice(&head[pos..]);
    out.extend_from_slice(tail);
    out
}

/// argv without session-dir, session and resume, in every spelling
/// session_flag reads, and their values, before any literal "--".
fn strip_session_flags(argv: &[String]) -> Vec<String> {
    let head = before_double_dash(argv);
    let tail = &argv[head.len()..];
    let mut out = Vec::new();
    let mut i = 0;
    while i < head.len() {
        if let Some((_, inline)) = session_flag(&head[i]) {
            i += if inline.is_some() { 1 } else { 2 };
            continue;
        }
        out.push(head[i].clone());
        i += 1;
    }
    out.extend_from_slice(tail);
    out
}

/// An id sprig takes as --session: "coppice-" and 8 random hex digits.
fn new_session_id() -> String {
    let mut b = [0u8; 4];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut b);
    }
    format!(
        "coppice-{}",
        b.iter().map(|x| format!("{x:02x}")).collect::<String>()
    )
}

/// sprig's own "sprig: session ID at PATH" line, which says nothing about
/// a failure. Its tree errors start "sprig: session tree:".
fn is_session_path_banner(line: &str) -> bool {
    line.starts_with("sprig: session ") && !line.starts_with("sprig: session tree:")
}

/// The last non-empty captured line that is not the banner, capped.
fn last_useful_line(captured: &str) -> String {
    let trimmed = captured.trim_end_matches('\n');
    for line in trimmed.split('\n').rev() {
        let line = line.trim_matches(|c: char| c.is_whitespace());
        if line.is_empty() || is_session_path_banner(line) {
            continue;
        }
        return line.chars().take(DETAIL_LINE_CAP).collect();
    }
    String::new()
}

impl Adapter for Sprig {
    fn name(&self) -> &'static str {
        "sprig"
    }

    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String> {
        let dir = match flag_value(&o.argv, "session-dir") {
            Some(d) if !d.is_empty() => d,
            _ => {
                return Err(
                    "sprig needs --session-dir in the pane's argv. That file is where its state comes from"
                        .into(),
                )
            }
        };
        let session_flag = flag_value(&o.argv, "session").unwrap_or_default();
        let resume_flag = flag_value(&o.argv, "resume").unwrap_or_default();
        let (sid, is_resume) = if !session_flag.is_empty() {
            (session_flag, false)
        } else if !resume_flag.is_empty() {
            (resume_flag, true)
        } else if !o.resume.is_empty() {
            (o.resume.clone(), true)
        } else {
            (new_session_id(), false)
        };
        let mut offset = 0;
        if let Ok(md) = std::fs::metadata(crate::sys::clean(&format!("{dir}/{sid}.jsonl"))) {
            offset = md.len();
        }
        let (out, rx) = Out::new();
        Ok(Arc::new(SprigProc(Arc::new(Inner {
            extra: strip_session_flags(&o.argv),
            opts: o,
            dir,
            out,
            events: Mutex::new(Some(rx)),
            st: Mutex::new(SessionState {
                id: sid,
                first_is_resume: is_resume,
                established: false,
                offset,
            }),
            turn: Mutex::new(TurnState::default()),
            turns: Arc::new(Turns::default()),
            relay_done: Flag::default(),
            stop_once: Mutex::new(false),
        }))))
    }
}

struct SessionState {
    id: String,
    first_is_resume: bool,
    established: bool,
    offset: u64,
}

struct SprigProc(Arc<Inner>);

struct Inner {
    opts: StartOpts,
    dir: String,
    extra: Vec<String>,
    out: Out,
    events: EventsSlot,
    st: Mutex<SessionState>,
    turn: Mutex<TurnState>,
    turns: Arc<Turns>,
    relay_done: Flag,
    stop_once: Mutex<bool>,
}

static TREE_ROW: StructTy = StructTy {
    name: "treeRow",
    text: "sprig.treeRow",
    fields: &[
        ("type", Ty::Str),
        ("id", Ty::Str),
        ("text", Ty::Str),
        ("name", Ty::Str),
        ("detail", Ty::Str),
        ("decision", Ty::Str),
        ("clause", Ty::Str),
        ("reason", Ty::Str),
        ("summary", Ty::Str),
        ("ok", Ty::PtrBool),
    ],
};

impl Inner {
    fn file(&self, sid: &str) -> String {
        crate::sys::clean(&format!("{}/{sid}.jsonl", self.dir))
    }

    fn begin_turn(&self) -> Result<(), String> {
        let mut t = self.turn.lock().unwrap_or_else(|e| e.into_inner());
        if self.out.stopped() {
            return Err("sprig: this pane has stopped".into());
        }
        if t.active {
            return Err("sprig: a turn is already running; wait for idle".into());
        }
        t.active = true;
        self.turns.add();
        Ok(())
    }

    fn end_turn(&self) {
        let mut t = self.turn.lock().unwrap_or_else(|e| e.into_inner());
        t.active = false;
        t.running = 0;
    }

    fn abort_turn(&self) {
        self.end_turn();
        self.turns.done();
    }

    /// This turn's argv. A first turn the caller asked to resume resumes
    /// even before the file exists, and is refused once it is clear the
    /// file is not there; after a turn has started, the file decides.
    fn turn_argv(&self, task: &str) -> Result<Vec<String>, String> {
        let (sid, established, first_is_resume) = {
            let s = self.st.lock().unwrap_or_else(|e| e.into_inner());
            (s.id.clone(), s.established, s.first_is_resume)
        };
        let exists = std::fs::metadata(self.file(&sid)).is_ok();
        let flag = if first_is_resume && !exists {
            return Err(format!("session {sid} not found. Create a new pane."));
        } else if (established && exists) || first_is_resume {
            "--resume"
        } else {
            "--session"
        };
        let mut base = vec!["--session-dir".to_string(), self.dir.clone()];
        base.extend(self.extra.iter().cloned());
        let mut argv = insert_after_session_dir(&base, &[flag.to_string(), sid]);
        argv.push("--".into());
        argv.push(task.into());
        Ok(argv)
    }

    fn run_turn(
        self: Arc<Self>,
        mut child: std::process::Child,
        r: OwnedFd,
        cancel: Arc<AtomicBool>,
    ) {
        let pid = child.id() as i32;
        let sid = self.st.lock().unwrap_or_else(|e| e.into_inner()).id.clone();
        let path = self.file(&sid);
        let tail_done = Arc::new(Flag::default());
        let tailer = {
            let me = self.clone();
            let done = tail_done.clone();
            thread::spawn(move || me.tail_turn(&path, &done))
        };
        let mut captured = String::new();
        let mut lines = Lines::new(r, cancel);
        for line in lines.by_ref() {
            if line.is_empty() {
                continue;
            }
            captured.push_str(&String::from_utf8_lossy(&line));
            captured.push('\n');
        }
        let scan_err = lines.err.clone();
        drop(lines);
        if let (Some(e), false) = (scan_err, self.out.stopped()) {
            self.out.send(Ev::error(format!("stream read failed: {e}")));
        }
        let status = child.wait();
        super::kill_group(pid);
        tail_done.set();
        let _ = tailer.join();
        if self.out.stopped() {
            self.end_turn();
            self.turns.done();
            return;
        }
        let code = status.as_ref().map(super::exit_code).unwrap_or(-1);
        self.end_turn();
        if code != 0 {
            let mut detail = last_useful_line(&captured);
            if detail.is_empty() {
                detail = match &status {
                    Ok(st) => crate::worktree::status_text(*st),
                    Err(e) => e.to_string(),
                };
            }
            self.out.send(Ev::error(format!("{detail} exit={code}")));
        } else {
            self.out.send(Ev::state(super::IDLE, "turn complete"));
        }
        self.turns.done();
    }

    fn tail_turn(&self, path: &str, done: &Flag) {
        loop {
            if done.wait(Some(TAIL_INTERVAL)) || self.out.stopped() {
                self.poll_tree(path);
                return;
            }
            self.poll_tree(path);
        }
    }

    fn poll_tree(&self, path: &str) {
        let offset = self.st.lock().unwrap_or_else(|e| e.into_inner()).offset;
        let n = self.read_new_rows(path, offset);
        self.st.lock().unwrap_or_else(|e| e.into_inner()).offset = n;
    }

    fn read_new_rows(&self, path: &str, offset: u64) -> u64 {
        let Ok(mut f) = std::fs::File::open(path) else {
            return offset;
        };
        let mut offset = offset;
        if let Ok(md) = f.metadata() {
            if md.len() < offset {
                offset = 0;
            }
        }
        if f.seek(SeekFrom::Start(offset)).is_err() {
            return offset;
        }
        let mut b = Vec::new();
        if f.take(MAX_TAIL_READ).read_to_end(&mut b).is_err() {
            return offset;
        }
        let mut start = 0;
        for i in 0..b.len() {
            if b[i] != b'\n' {
                continue;
            }
            let line = &b[start..i];
            start = i + 1;
            if line.is_empty() {
                continue;
            }
            for ev in self.parse_row(line) {
                self.out.send(ev);
            }
        }
        offset + start as u64
    }

    fn parse_row(&self, line: &[u8]) -> Vec<Ev> {
        let (r, err) = godec::decode(line, &Ty::Struct(&TREE_ROW));
        if let Some(e) = err {
            return vec![Ev::error(format!("cannot read a sprig session row: {e}"))];
        }
        let ty = r.f("type").s();
        match ty.as_str() {
            "session" => {
                self.st.lock().unwrap_or_else(|e| e.into_inner()).id = r.f("id").s();
                Vec::new()
            }
            "prompt" => vec![Ev::state(super::WORKING, "prompt")],
            "assistant" => {
                let text = r.f("text").s();
                if text.is_empty() {
                    vec![Ev::state(super::WORKING, "")]
                } else {
                    vec![Ev::text(text)]
                }
            }
            "tool_call" => vec![Ev::tool(r.f("name").s(), r.f("detail").s())],
            "verdict" => {
                if r.f("decision").s() == "deny" {
                    let mut clause = r.f("clause").s();
                    if clause.is_empty() {
                        clause = r.f("reason").s();
                    }
                    vec![Ev::state(
                        super::BLOCKED,
                        format!("verdict=deny clause={clause}"),
                    )]
                } else {
                    vec![Ev::state(super::WORKING, "verdict=allow")]
                }
            }
            "tool_result" => {
                let summary = r.f("summary").s();
                let text = if r.f("ok").boolean() == Some(false) {
                    format!("[tool failed] {summary}")
                } else {
                    summary
                };
                vec![Ev::text(text)]
            }
            _ => {
                let detail = if ty.is_empty() {
                    "unrecognized session-tree row".to_string()
                } else {
                    ty
                };
                vec![Ev::state(super::WORKING, detail)]
            }
        }
    }

    fn stopped_err(&self) -> Result<(), String> {
        if self.out.stopped() || self.relay_done.is_set() {
            return Err("sprig: this pane has stopped".into());
        }
        Ok(())
    }
}

impl Proc for SprigProc {
    fn prompt(&self, text: &str) -> Result<(), String> {
        let me = &self.0;
        me.begin_turn()?;
        let argv = match me.turn_argv(text) {
            Ok(a) => a,
            Err(e) => {
                me.abort_turn();
                me.out.send(Ev::error(e.clone()));
                return Err(format!("sprig: {e}"));
            }
        };
        let bin = me.opts.binary(BIN_VAR, BINARY);
        let (child, r) = match spawn_turn(&bin, &argv, &me.opts, true) {
            Ok(x) => x,
            Err(e) => {
                me.abort_turn();
                return Err(e);
            }
        };
        let cancel = Arc::new(AtomicBool::new(false));
        {
            let mut t = me.turn.lock().unwrap_or_else(|e| e.into_inner());
            t.running = child.id() as i32;
            t.pids.push(child.id() as i32);
            if t.pids.len() > super::PID_KEEP {
                let n = t.pids.len() - super::PID_KEEP;
                t.pids.drain(..n);
            }
            t.cancel = Some(cancel.clone());
            me.st.lock().unwrap_or_else(|e| e.into_inner()).established = true;
        }
        let me = me.clone();
        thread::spawn(move || me.run_turn(child, r, cancel));
        Ok(())
    }

    fn write_stdin(&self, _b: &[u8]) -> Result<(), String> {
        self.0.stopped_err()?;
        Err("sprig: this harness does not support that. Use agent prompt.".into())
    }

    fn take_events(&self) -> Option<std::sync::mpsc::Receiver<Ev>> {
        self.0
            .events
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
    }

    fn session_id(&self) -> Option<String> {
        let s = self
            .0
            .st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .id
            .clone();
        (!s.is_empty()).then_some(s)
    }

    fn stop(&self) {
        let me = &self.0;
        stop_turns(&me.stop_once, &me.out, &me.turn, &me.turns, &me.relay_done);
    }

    fn pids(&self) -> Vec<i32> {
        self.0
            .turn
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .pids
            .clone()
    }
}

#[cfg(test)]
mod tests {
    use super::super::testutil::{scratch, testdata, until};
    use super::*;

    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    /// sprig parses its flags with Go's flag package: one or two dashes, a
    /// value after "=" or in the next word, and the last value wins.
    #[test]
    fn session_flags_read_every_spelling_sprig_takes() {
        for (argv, dir, res) in [
            (
                s(&["-session-dir", "/d", "-resume", "r"]),
                Some("/d"),
                Some("r"),
            ),
            (s(&["-session-dir=/d", "--resume=r"]), Some("/d"), Some("r")),
            (
                s(&[
                    "--session-dir",
                    "/mine",
                    "-session-dir",
                    "/victim",
                    "-resume",
                    "x",
                    "--resume",
                    "r",
                ]),
                Some("/victim"),
                Some("r"),
            ),
            (
                s(&["--session-dir", "/d", "--", "-resume", "r"]),
                Some("/d"),
                None,
            ),
        ] {
            assert_eq!(flag_value(&argv, "session-dir").as_deref(), dir, "{argv:?}");
            assert_eq!(flag_value(&argv, "resume").as_deref(), res, "{argv:?}");
        }
        assert_eq!(
            strip_session_flags(&s(&[
                "-session-dir",
                "/v",
                "--gate",
                "-resume=r",
                "--session",
                "s",
                "-session-dir=/w",
                "--",
                "-resume",
                "x"
            ])),
            s(&["--gate", "--", "-resume", "x"])
        );
    }

    #[test]
    fn argv_helpers_stop_at_a_double_dash() {
        assert_eq!(
            flag_value(
                &s(&["--session-dir", "/d", "--", "--session", "x"]),
                "session"
            ),
            None
        );
        assert_eq!(
            flag_value(&s(&["--session-dir=/d"]), "session-dir").as_deref(),
            Some("/d")
        );
        assert_eq!(flag_value(&s(&["--session-dir="]), "session-dir"), None);
        assert_eq!(
            strip_session_flags(&s(&[
                "--gate",
                "g",
                "--session",
                "a",
                "--resume=b",
                "--",
                "--session",
                "c"
            ])),
            s(&["--gate", "g", "--", "--session", "c"])
        );
        assert_eq!(
            insert_after_session_dir(
                &s(&["--session-dir", "/d", "--gate", "g"]),
                &s(&["--session", "x"])
            ),
            s(&["--session-dir", "/d", "--session", "x", "--gate", "g"])
        );
        assert_eq!(
            tree_path(&s(&["--session-dir", "/d"]), "abc").as_deref(),
            Some("/d/abc.jsonl")
        );
        assert_eq!(tree_path(&s(&["--session-dir", "rel"]), "abc"), None);
        assert_eq!(tree_path(&s(&["--session-dir", "/d"]), "a/b"), None);
        assert_eq!(last_useful_line("x\nsprig: session s at /p\n\n"), "x");
        assert_eq!(
            last_useful_line("sprig: session tree: bad\n"),
            "sprig: session tree: bad"
        );
    }

    #[test]
    fn two_turns_resume_the_session_the_first_made() {
        let root = scratch("sprig");
        let dir = root.join("sessions");
        let argv_log = root.join("argv.txt");
        let mut o = StartOpts {
            bin: testdata("fake-sprig.sh"),
            argv: s(&["--session-dir", dir.to_str().unwrap(), "--session", "s1"]),
            cwd: root.to_string_lossy().into_owned(),
            ..Default::default()
        };
        o.env.insert(
            "COPPICE_SPRIG_ECHO_ARGV".into(),
            argv_log.to_string_lossy().into_owned(),
        );
        let p = Sprig.start(o).unwrap();
        let rx = p.take_events().unwrap();
        p.prompt("first").unwrap();
        let evs = until(&rx, Duration::from_secs(10), |e| {
            e.detail == "turn complete"
        });
        assert_eq!(evs.first().unwrap().detail, "prompt");
        p.prompt("second").unwrap();
        until(&rx, Duration::from_secs(10), |e| {
            e.detail == "turn complete"
        });
        let log = std::fs::read_to_string(&argv_log).unwrap();
        let lines: Vec<&str> = log.lines().collect();
        assert!(lines[0].contains("--session s1 -- first"), "{log}");
        assert!(lines[1].contains("--resume s1 -- second"), "{log}");
        assert_eq!(p.pids().len(), 2);
        p.stop();
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_failed_turn_names_the_line_and_the_code() {
        let root = scratch("sprigfail");
        let dir = root.join("sessions");
        std::fs::create_dir_all(&dir).unwrap();
        let o = StartOpts {
            bin: testdata("fake-sprig.sh"),
            argv: s(&["--session-dir", dir.to_str().unwrap(), "--resume", "gone"]),
            ..Default::default()
        };
        let p = Sprig.start(o).unwrap();
        let rx = p.take_events().unwrap();
        assert_eq!(
            p.prompt("x").unwrap_err(),
            "sprig: session gone not found. Create a new pane."
        );
        let evs = until(&rx, Duration::from_secs(2), |_| true);
        assert_eq!(evs[0].detail, "session gone not found. Create a new pane.");
        p.stop();
        let _ = std::fs::remove_dir_all(&root);
    }
}
