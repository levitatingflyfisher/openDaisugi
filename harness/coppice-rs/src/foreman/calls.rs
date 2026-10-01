//! The write-ahead log of the foreman's acting tool calls (FM-R-66). The
//! twin of Go's internal/foreman calls.go.
//!
//! Before a tool effect (the gated spawn) the server writes a start line,
//! fsynced; after the effect it writes an end line. A start with no end,
//! found when the chat opens, is a call whose outcome is unknown: it is
//! never run again.

use std::collections::HashSet;
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;
use std::sync::Mutex;

use super::fsutil::{self, clip, json_str};

/// The file in the log dir that holds the call lines.
pub const CALLS_FILE: &str = "calls.jsonl";

/// The phases of a call line, and the outcomes of an end line.
pub const CALL_START: &str = "start";
pub const CALL_END: &str = "end";
pub const CALL_OK: &str = "ok";
pub const CALL_FAILED: &str = "error";
pub const CALL_UNKNOWN: &str = "unknown";

/// Caps the arguments kept in a start line.
const MAX_CALL_ARGS: usize = 4096;

/// One line of calls.jsonl.
#[derive(Clone, Debug, Default, PartialEq, serde::Deserialize)]
#[serde(default)]
pub struct CallRecord {
    /// Names the call: c<turn>.<n>.
    pub call: String,
    pub phase: String,
    /// The owner message the turn answers, on a start line.
    pub turn: i64,
    /// The call, on a start line. args is JSON text.
    pub tool: String,
    pub args: String,
    pub at: String,
    /// ok, error or unknown, on an end line.
    pub outcome: String,
}

impl CallRecord {
    /// The line as Go writes it: turn, tool, args and outcome left out when
    /// empty or zero; the args cut at 4 KiB.
    pub fn encode(&self) -> String {
        let mut s = format!(
            "{{\"call\":{},\"phase\":{}",
            json_str(&self.call),
            json_str(&self.phase)
        );
        if self.turn != 0 {
            s.push_str(&format!(",\"turn\":{}", self.turn));
        }
        if !self.tool.is_empty() {
            s.push_str(&format!(",\"tool\":{}", json_str(&self.tool)));
        }
        let args = clip(&self.args, MAX_CALL_ARGS);
        if !args.is_empty() {
            s.push_str(&format!(",\"args\":{}", json_str(args)));
        }
        s.push_str(&format!(",\"at\":{}", json_str(&self.at)));
        if !self.outcome.is_empty() {
            s.push_str(&format!(",\"outcome\":{}", json_str(&self.outcome)));
        }
        s.push_str("}\n");
        s
    }
}

/// Orders the writers of one calls.jsonl: two spawns at once in one turn
/// must not cut each other's line on an error path.
static CALLS_MU: Mutex<()> = Mutex::new(());

/// Adds one line to calls.jsonl in dir, 0600, fsynced. A torn tail left by
/// a crash is cut back to the last newline first, so the new line starts
/// whole (FM-R-67).
pub fn append_call(dir: &Path, r: &CallRecord) -> Result<(), String> {
    let _g = CALLS_MU.lock().unwrap_or_else(|e| e.into_inner());
    let path = dir.join(CALLS_FILE);
    repair_tail(&path)?;
    fsutil::append_line(&path, r.encode().as_bytes())
        .map(|_| ())
        .map_err(|e| e.msg)
}

/// Cuts a torn tail of calls.jsonl in dir back to its last newline,
/// fsynced. A missing file is fine.
pub fn repair_calls(dir: &Path) -> Result<(), String> {
    let _g = CALLS_MU.lock().unwrap_or_else(|e| e.into_inner());
    repair_tail(&dir.join(CALLS_FILE))
}

fn repair_tail(path: &Path) -> Result<(), String> {
    let mut f = match std::fs::OpenOptions::new().read(true).write(true).open(path) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(fsutil::path_err("open", path, &e)),
    };
    let err = |what: &str, e: std::io::Error| fsutil::path_err(what, path, &e);
    let size = f.metadata().map_err(|e| err("stat", e))?.len();
    if size == 0 {
        return Ok(());
    }
    let mut last = [0u8; 1];
    f.seek(SeekFrom::Start(size - 1)).map_err(|e| err("seek", e))?;
    f.read_exact(&mut last).map_err(|e| err("read", e))?;
    if last[0] == b'\n' {
        return Ok(());
    }
    // Find the last newline, reading back in blocks.
    let mut cut = 0u64;
    let mut end = size;
    let mut buf = vec![0u8; 4096];
    while end > 0 {
        let start = end.saturating_sub(4096);
        let n = (end - start) as usize;
        f.seek(SeekFrom::Start(start)).map_err(|e| err("seek", e))?;
        f.read_exact(&mut buf[..n]).map_err(|e| err("read", e))?;
        if let Some(i) = buf[..n].iter().rposition(|&b| b == b'\n') {
            cut = start + i as u64 + 1;
            break;
        }
        end = start;
    }
    f.set_len(cut).map_err(|e| err("truncate", e))?;
    f.sync_all().map_err(|e| err("sync", e))
}

/// Reads calls.jsonl in dir. A missing file is no calls, and a line cut by a
/// crash or not JSON is skipped.
pub fn read_calls(dir: &Path) -> Result<Vec<CallRecord>, String> {
    let p = dir.join(CALLS_FILE);
    let data = match std::fs::read(&p) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(fsutil::path_err("open", &p, &e)),
    };
    let mut out = Vec::new();
    for line in data.split(|&b| b == b'\n') {
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        if let Ok(r) = serde_json::from_slice::<CallRecord>(line) {
            out.push(r);
        }
    }
    Ok(out)
}

/// The start lines that have no end line, oldest first.
pub fn dangling_calls(recs: &[CallRecord]) -> Vec<CallRecord> {
    let ended: HashSet<&str> = recs
        .iter()
        .filter(|r| r.phase == CALL_END)
        .map(|r| r.call.as_str())
        .collect();
    recs.iter()
        .filter(|r| r.phase == CALL_START && !ended.contains(r.call.as_str()))
        .cloned()
        .collect()
}

/// Copies the start lines with no end from the call log in `from` (a
/// history moved aside) into the call log in `to`, with turn -1 and a new id (`m<unix>-<hex>.<old id>`): the turn
/// numbers of the old history mean nothing in the new chat, and -1 matches
/// no turn. The next open reports each once. Act ids are not carried.
pub fn carry_dangling_calls(from: &Path, to: &Path) -> Result<(), String> {
    use std::hash::{BuildHasher, Hasher};
    let recs = read_calls(from)?;
    let rnd = std::collections::hash_map::RandomState::new()
        .build_hasher()
        .finish() as u32;
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    let prefix = format!("m{secs}-{rnd:x}.");
    for mut r in dangling_calls(&recs) {
        // A new id that no socket can make: the new chat starts its message
        // ids again from 0, so an old c5.1 would meet a new c5.1 and the old
        // end line would hide the new start.
        r.call = format!("{prefix}{}", r.call);
        r.turn = -1;
        append_call(to, &r)?;
    }
    Ok(())
}

/// Whether the log holds a start line for the turn that answers message
/// turn. Such a turn is not run again on another account, whatever the
/// memory of the process says (FM-R-50).
pub fn turn_has_tool_start(recs: &[CallRecord], turn: i64) -> bool {
    recs.iter().any(|r| r.phase == CALL_START && r.turn == turn)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn dir(name: &str) -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!("coppice-rs-calls-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn rec(call: &str, phase: &str, turn: i64) -> CallRecord {
        CallRecord {
            call: call.into(),
            phase: phase.into(),
            turn,
            tool: if phase == CALL_START { "spawn".into() } else { String::new() },
            at: "2026-10-22T10:00:00Z".into(),
            ..Default::default()
        }
    }

    #[test]
    fn a_dangling_call_is_a_start_with_no_end() {
        let d = dir("dangling");
        append_call(&d, &rec("c4.1", CALL_START, 4)).unwrap();
        let mut end = rec("c4.1", CALL_END, 0);
        end.outcome = CALL_OK.into();
        append_call(&d, &end).unwrap();
        append_call(&d, &rec("c5.1", CALL_START, 5)).unwrap();
        let recs = read_calls(&d).unwrap();
        assert_eq!(recs.len(), 3);
        let dang = dangling_calls(&recs);
        assert_eq!(dang.len(), 1);
        assert_eq!((dang[0].call.as_str(), dang[0].turn), ("c5.1", 5));
        assert!(turn_has_tool_start(&recs, 4) && turn_has_tool_start(&recs, 5));
        assert!(!turn_has_tool_start(&recs, 6));
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(d.join(CALLS_FILE)).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn an_unknown_end_settles_a_call_once() {
        let d = dir("unknown");
        append_call(&d, &rec("c1.1", CALL_START, 1)).unwrap();
        assert_eq!(dangling_calls(&read_calls(&d).unwrap()).len(), 1);
        let mut end = rec("c1.1", CALL_END, 0);
        end.outcome = CALL_UNKNOWN.into();
        append_call(&d, &end).unwrap();
        assert!(dangling_calls(&read_calls(&d).unwrap()).is_empty());
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_line_cut_by_a_crash_is_skipped() {
        let d = dir("cut");
        append_call(&d, &rec("c1.1", CALL_START, 1)).unwrap();
        use std::io::Write;
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .open(d.join(CALLS_FILE))
            .unwrap();
        f.write_all(b"{\"call\":\"c2.1\",\"phase\":\"sta").unwrap();
        assert_eq!(read_calls(&d).unwrap().len(), 1);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn the_line_is_the_bytes_go_writes() {
        let mut r = rec("c0.1", CALL_START, 0);
        r.args = "{\"project\":\"a\"}".into();
        assert_eq!(
            r.encode(),
            "{\"call\":\"c0.1\",\"phase\":\"start\",\"tool\":\"spawn\",\"args\":\"{\\\"project\\\":\\\"a\\\"}\",\"at\":\"2026-10-22T10:00:00Z\"}\n"
        );
        let mut e = rec("c0.1", CALL_END, 0);
        e.outcome = CALL_FAILED.into();
        assert_eq!(
            e.encode(),
            "{\"call\":\"c0.1\",\"phase\":\"end\",\"at\":\"2026-10-22T10:00:00Z\",\"outcome\":\"error\"}\n"
        );
    }

    fn tear(d: &Path, text: &str) {
        use std::io::Write;
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .open(d.join(CALLS_FILE))
            .unwrap();
        f.write_all(text.as_bytes()).unwrap();
    }

    #[test]
    fn a_start_after_a_torn_tail_is_whole() {
        let d = dir("torn");
        append_call(&d, &rec("c1.1", CALL_START, 1)).unwrap();
        tear(&d, "{\"call\":\"c2.1\",\"phase\":\"sta");
        append_call(&d, &rec("c3.1", CALL_START, 3)).unwrap();
        let recs = read_calls(&d).unwrap();
        assert_eq!(recs.len(), 2, "{recs:?}");
        assert_eq!(recs[1].call, "c3.1");
        assert_eq!(dangling_calls(&recs).len(), 2);
    }

    #[test]
    fn a_file_that_is_all_torn_is_cut_to_nothing() {
        let d = dir("alltorn");
        tear(&d, &"x".repeat(9000));
        repair_calls(&d).unwrap();
        assert_eq!(std::fs::metadata(d.join(CALLS_FILE)).unwrap().len(), 0);
        repair_calls(&dir("missing")).unwrap();
    }

    #[test]
    fn a_carried_call_never_shares_an_id_with_the_new_chat() {
        let (old, new) = (dir("carry-old"), dir("carry-new"));
        append_call(&old, &rec("c5.1", CALL_START, 5)).unwrap();
        carry_dangling_calls(&old, &new).unwrap();
        let carried = dangling_calls(&read_calls(&new).unwrap());
        assert!(carried.len() == 1 && carried[0].call != "c5.1", "{carried:?}");
        let mut end = rec(&carried[0].call, CALL_END, 0);
        end.outcome = CALL_UNKNOWN.into();
        append_call(&new, &end).unwrap();
        // The new chat's turn for message 5 makes the id the old one had.
        append_call(&new, &rec("c5.1", CALL_START, 5)).unwrap();
        let d = dangling_calls(&read_calls(&new).unwrap());
        assert!(d.len() == 1 && d[0].call == "c5.1", "{d:?}");
    }
}
