//! The record of the foreman's turns, turns.jsonl, and the PreToolUse hook
//! of a turn. The twin of Go's internal/foreman turns.go.

use std::io::{Read, Write};
use std::path::Path;

use super::compactor::CallUsage;
use super::fsutil::{self, json_str};
use super::tools::TOOL_NAMES;

/// The file in the log dir that records the foreman's turns.
pub const TURNS_FILE: &str = "turns.jsonl";

/// Turn statuses. A turn writes a line when it starts, with status
/// "running", and one when it ends.
pub const TURN_RUNNING: &str = "running";
pub const TURN_OK: &str = "ok";
pub const TURN_FAILED: &str = "failed";
pub const TURN_TIMEOUT: &str = "timeout";

/// One line of turns.jsonl.
#[derive(Clone, Debug, Default, PartialEq, serde::Deserialize)]
#[serde(default)]
pub struct TurnRecord {
    /// The id of the owner's message the turn answers.
    pub msg: i64,
    pub started: String,
    pub ended: String,
    pub status: String,
    pub usage: Option<CallUsage>,
    /// The owner messages that reached the turn at a tool call. They are in
    /// the end record so that a restart knows they were heard.
    pub mid: Vec<i64>,
}

impl TurnRecord {
    /// The line as Go writes it: ended and usage left out when empty.
    pub fn encode(&self) -> String {
        let mut s = format!(
            "{{\"msg\":{},\"started\":{}",
            self.msg,
            json_str(&self.started)
        );
        if !self.ended.is_empty() {
            s.push_str(&format!(",\"ended\":{}", json_str(&self.ended)));
        }
        s.push_str(&format!(",\"status\":{}", json_str(&self.status)));
        if let Some(u) = &self.usage {
            s.push_str(&format!(",\"usage\":{{{}}}", u.fields()));
        }
        if !self.mid.is_empty() {
            let ids: Vec<String> = self.mid.iter().map(|i| i.to_string()).collect();
            s.push_str(&format!(",\"mid\":[{}]", ids.join(",")));
        }
        s.push_str("}\n");
        s
    }
}

/// Adds one record to turns.jsonl in dir, 0600, fsynced.
pub fn append_turn(dir: &Path, r: &TurnRecord) -> Result<(), String> {
    fsutil::append_line(&dir.join(TURNS_FILE), r.encode().as_bytes())
        .map(|_| ())
        .map_err(|e| e.msg)
}

/// Reads turns.jsonl in dir. A missing file is no turns, and a line that
/// does not read is skipped.
pub fn read_turns(dir: &Path) -> Result<Vec<TurnRecord>, String> {
    let p = dir.join(TURNS_FILE);
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
        if let Ok(r) = serde_json::from_slice::<TurnRecord>(line) {
            out.push(r);
        }
    }
    Ok(out)
}

/// Whether a turn for message msg has started.
pub fn turn_started(recs: &[TurnRecord], msg: i64) -> bool {
    recs.iter().any(|r| r.msg == msg)
}

/// The summary `coppice foreman stats` prints: the turns by status and the
/// share of input tokens that the cache read.
pub fn stats(recs: &[TurnRecord]) -> String {
    let (mut ok, mut failed, mut timeout) = (0, 0, 0);
    let (mut inp, mut read, mut write, mut out) = (0i64, 0i64, 0i64, 0i64);
    for r in recs {
        match r.status.as_str() {
            TURN_OK => ok += 1,
            TURN_FAILED => failed += 1,
            TURN_TIMEOUT => timeout += 1,
            _ => continue,
        }
        if let Some(u) = &r.usage {
            inp += u.input;
            read += u.cache_read;
            write += u.cache_write;
            out += u.output;
        }
    }
    let mut b = format!(
        "turns: {} (ok {ok}, failed {failed}, timeout {timeout})\n",
        ok + failed + timeout
    );
    let total = inp + read + write;
    if total == 0 {
        b.push_str("cache read share: none yet (no turn has reported tokens)\n");
    } else {
        b.push_str(&format!(
            "cache read share: {:.1}% of {total} input tokens (fresh {inp}, cache read {read}, cache write {write}); output {out}\n",
            100.0 * read as f64 / total as f64
        ));
    }
    b
}

/// The PreToolUse hook of a turn: `coppice foreman hook`. It reads the
/// hook's JSON from stdin and exits 2 for any tool that is not one of the
/// foreman's four, so a built-in that a Claude Code update adds is still
/// refused.
pub fn hook_main(input: &mut dyn Read, errw: &mut dyn Write) -> i32 {
    let mut data = Vec::new();
    let _ = input.take(1 << 20).read_to_end(&mut data);
    let name = serde_json::from_slice::<serde_json::Value>(&data)
        .ok()
        .and_then(|v| {
            v.get("tool_name")
                .and_then(|t| t.as_str())
                .map(str::to_string)
        });
    if let Some(n) = &name {
        if TOOL_NAMES
            .iter()
            .any(|t| format!("mcp__foreman__{t}") == *n)
        {
            return 0;
        }
    }
    let _ = writeln!(
        errw,
        "the foreman may use only its four tools ({}). {} is not one of them.",
        TOOL_NAMES.join(", "),
        crate::sys::go_quote(name.as_deref().unwrap_or(""))
    );
    2
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn turn_records_round_trip_and_stats_print_the_cache_share() {
        let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-turns-{}", std::process::id())));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let rec = |msg, started: &str, ended: &str, status: &str, usage| TurnRecord {
            msg,
            started: started.into(),
            ended: ended.into(),
            status: status.into(),
            usage,
            mid: Vec::new(),
        };
        for r in [
            rec(0, "2026-10-13T10:00:00Z", "", TURN_RUNNING, None),
            rec(
                0,
                "2026-10-13T10:00:00Z",
                "2026-10-13T10:00:05Z",
                TURN_OK,
                Some(CallUsage {
                    input: 100,
                    output: 50,
                    cache_read: 900,
                    cache_write: 0,
                    stop: "",
                }),
            ),
            rec(1, "2026-10-13T10:01:00Z", "", TURN_RUNNING, None),
            rec(
                1,
                "2026-10-13T10:01:00Z",
                "2026-10-13T10:01:01Z",
                TURN_FAILED,
                None,
            ),
            rec(
                2,
                "2026-10-13T10:02:00Z",
                "2026-10-13T10:07:00Z",
                TURN_TIMEOUT,
                None,
            ),
        ] {
            append_turn(&dir, &r).unwrap();
        }
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(dir.join(TURNS_FILE))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
        let text = std::fs::read_to_string(dir.join(TURNS_FILE)).unwrap();
        assert!(text.starts_with("{\"msg\":0,\"started\":\"2026-10-13T10:00:00Z\",\"status\":\"running\"}\n{\"msg\":0,\"started\":\"2026-10-13T10:00:00Z\",\"ended\":\"2026-10-13T10:00:05Z\",\"status\":\"ok\",\"usage\":{\"input\":100,\"output\":50,\"cache_read\":900,\"cache_write\":0}}\n"), "{text}");
        let recs = read_turns(&dir).unwrap();
        assert_eq!(recs.len(), 5);
        assert!(turn_started(&recs, 1) && !turn_started(&recs, 7));
        assert_eq!(
            stats(&recs),
            "turns: 3 (ok 1, failed 1, timeout 1)\ncache read share: 90.0% of 1000 input tokens (fresh 100, cache read 900, cache write 0); output 50\n"
        );
        assert!(stats(&[]).contains("none yet"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_hook_refuses_every_tool_but_the_foremans_four() {
        for (tool, want) in [
            ("mcp__foreman__zoom", 0),
            ("mcp__foreman__date", 0),
            ("mcp__foreman__spawn", 0),
            ("mcp__foreman__status", 0),
            ("Bash", 2),
            ("Read", 2),
            ("WebFetch", 2),
            ("SomeNewBuiltIn", 2),
            ("mcp__foreman__bash", 2),
            ("mcp__other__zoom", 2),
            ("", 2),
        ] {
            let input = format!("{{\"tool_name\":\"{tool}\",\"tool_input\":{{}}}}");
            let mut errb = Vec::new();
            assert_eq!(hook_main(&mut input.as_bytes(), &mut errb), want, "{tool}");
        }
        let mut errb = Vec::new();
        assert_eq!(hook_main(&mut &b"not json"[..], &mut errb), 2);
        assert_eq!(
            String::from_utf8(errb).unwrap(),
            "the foreman may use only its four tools (zoom, date, spawn, status). \"\" is not one of them.\n"
        );
    }
}
