//! sprig-mcp: sprig's four tools as an MCP server over stdio, so a driver
//! calls them natively while the warden still rules every call.

use sprig::warden::{AllowAll, DaisugiWarden, Executor, Warden, DEFAULT_WARDEN_CMD};
use sprig::gostr::fields;
use sprig::hook::atoi;
use sprig::mcp::Server;
use sprig::osx::Fd;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

fn usage() {
    let _ = sprig::osx::out(
        2,
        b"usage: sprig-mcp [--warden=false] [--warden-cmd \"<cmd>\"]\n\
serve sprig's read/write/edit/bash tools to an MCP driver over stdio.\n\
the warden checks every call by default, fail-closed; --warden=false turns it off.\n",
    );
}

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    let mut warden_on = true;
    let mut warden_cmd = DEFAULT_WARDEN_CMD.as_bytes().to_vec();
    let mut timeout = Duration::from_secs(30);
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_slice();
        if a == b"--warden" || a == b"--warden=true" {
            warden_on = true;
        } else if a == b"--warden=false" {
            warden_on = false;
        } else if a == b"--warden-cmd" && i + 1 < args.len() {
            warden_cmd = args[i + 1].clone();
            i += 1;
        } else if a == b"--warden-timeout" && i + 1 < args.len() {
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
    let warden: Box<dyn Warden> = if warden_on {
        Box::new(DaisugiWarden {
            command: fields(&warden_cmd),
            timeout,
            session_id: Vec::new(),
        })
    } else {
        let _ = sprig::osx::out(2, sprig::warden::warden_off_notice("sprig-mcp").as_bytes());
        Box::new(AllowAll)
    };
    let srv = Server {
        exec: Executor::new(sprig::tools::default_tools(), warden),
    };
    if let Err(e) = srv.serve(&mut std::io::stdin().lock(), &mut Fd(1)) {
        let _ = sprig::osx::out(2, format!("sprig-mcp: {e}\n").as_bytes());
        std::process::exit(1);
    }
}
