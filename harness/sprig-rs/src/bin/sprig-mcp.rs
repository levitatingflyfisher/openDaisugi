//! sprig-mcp: sprig's four tools as an MCP server over stdio, so a driver
//! calls them natively while the gate still rules every call.

use sprig::gate::{AllowAll, DaisugiGate, Executor, Gate, DEFAULT_GATE_CMD};
use sprig::gostr::fields;
use sprig::hook::atoi;
use sprig::mcp::Server;
use sprig::osx::Fd;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

fn usage() {
    let _ = sprig::osx::out(
        2,
        b"usage: sprig-mcp [--gate] [--gate-cmd \"<cmd>\"]\n\
serve sprig's read/write/edit/bash tools to an MCP driver over stdio.\n\
--gate verifies every call against the openDaisugi envelope, fail-closed.\n",
    );
}

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    let mut gate_on = false;
    let mut gate_cmd = DEFAULT_GATE_CMD.as_bytes().to_vec();
    let mut timeout = Duration::from_secs(30);
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_slice();
        if a == b"--gate" {
            gate_on = true;
        } else if a == b"--gate-cmd" && i + 1 < args.len() {
            gate_cmd = args[i + 1].clone();
            i += 1;
        } else if a == b"--gate-timeout" && i + 1 < args.len() {
            if let Some(s) = atoi(&args[i + 1]).filter(|&s| s > 0) {
                timeout = Duration::from_secs(s as u64);
            }
            i += 1;
        } else if a == b"--help" || a == b"-h" {
            usage();
            return;
        }
        i += 1;
    }
    let gate: Box<dyn Gate> = if gate_on {
        Box::new(DaisugiGate {
            command: fields(&gate_cmd),
            timeout,
            session_id: Vec::new(),
        })
    } else {
        Box::new(AllowAll)
    };
    let srv = Server {
        exec: Executor::new(sprig::tools::default_tools(), gate),
    };
    if let Err(e) = srv.serve(&mut std::io::stdin().lock(), &mut Fd(1)) {
        let _ = sprig::osx::out(2, format!("sprig-mcp: {e}\n").as_bytes());
        std::process::exit(1);
    }
}
