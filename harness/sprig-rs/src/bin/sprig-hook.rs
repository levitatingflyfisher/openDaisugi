//! sprig-hook: a Claude Code PreToolUse hook that checks the driver's own
//! tools. It reads {tool_name, tool_input} on stdin and prints the
//! decision; a deny exits 2, which bypassPermissions cannot skip.

use sprig::warden::{DaisugiWarden, DEFAULT_WARDEN_CMD};
use sprig::gostr::fields;
use sprig::hook::{atoi, decide};
use std::io::Read;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    let mut warden_cmd = DEFAULT_WARDEN_CMD.as_bytes().to_vec();
    // 30 seconds: a cold first warden call under load can take several.
    let mut timeout = Duration::from_secs(30);
    let mut i = 0;
    while i < args.len() {
        if args[i] == b"--warden-cmd" && i + 1 < args.len() {
            warden_cmd = args[i + 1].clone();
            i += 1;
        } else if args[i] == b"--warden-timeout" && i + 1 < args.len() {
            if let Some(s) = atoi(&args[i + 1]).filter(|&s| s > 0) {
                timeout = Duration::from_secs(s as u64);
            }
            i += 1;
        }
        i += 1;
    }
    let mut input = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut input);
    let warden = DaisugiWarden {
        command: fields(&warden_cmd),
        timeout,
        session_id: Vec::new(),
    };
    let (out, allow) = decide(&input, &warden);
    let _ = sprig::osx::out(1, &out);
    if !allow {
        std::process::exit(2);
    }
}
