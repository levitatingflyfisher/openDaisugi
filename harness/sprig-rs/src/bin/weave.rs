//! weave: runs a workflow file, a small JSON graph of steps, each a sprig
//! task behind the warden. Results to stdout, logs to stderr, real exit
//! codes, --json for scripts.

use sprig::agent::Agent;
use sprig::warden::{AllowAll, DaisugiWarden, Executor, Warden, DEFAULT_WARDEN_CMD};
use sprig::gostr::fields;
use sprig::model_claude::ClaudeCodeModel;
use sprig::osx::out;
use std::os::unix::ffi::OsStrExt;
use std::rc::Rc;
use std::time::Duration;

fn usage() {
    let _ = out(
        2,
        b"usage: weave run <workflow.json> [--warden=false] [--warden-cmd \"<cmd>\"] [--json]\n\
run a JSON graph of checked agent steps in dependency order.\n\
the warden checks every tool call by default; --warden=false turns it off.\n",
    );
}

/// One warden shared by every step, as the Go binary shares its value.
struct Shared(Rc<dyn Warden>);

impl Warden for Shared {
    fn check(&self, call: &sprig::warden::ToolCall) -> sprig::warden::Verdict {
        self.0.check(call)
    }
}

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    let mut warden_on = true;
    let mut warden_cmd = DEFAULT_WARDEN_CMD.as_bytes().to_vec();
    let mut json_out = false;
    let mut file: Vec<u8> = Vec::new();
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_slice();
        if a == b"run" {
            // the optional verb
        } else if a == b"--warden" || a == b"--warden=true" {
            warden_on = true;
        } else if a == b"--warden=false" {
            warden_on = false;
        } else if a == b"--warden-cmd" && i + 1 < args.len() {
            warden_cmd = args[i + 1].clone();
            i += 1;
        } else if a == b"--json" {
            json_out = true;
        } else if a == b"--help" || a == b"-h" {
            usage();
            return;
        } else {
            file = a.to_vec();
        }
        i += 1;
    }
    if file.is_empty() {
        usage();
        std::process::exit(1);
    }
    let data = match sprig::osx::read_file(&file) {
        Ok(d) => d,
        Err(e) => {
            let _ = out(2, &[b"weave: ".as_slice(), &e, b"\n"].concat());
            std::process::exit(1);
        }
    };
    let wf = match sprig::weave::parse(&data) {
        Ok(wf) => wf,
        Err(e) => {
            let _ = out(2, &[e.as_slice(), b"\n"].concat());
            std::process::exit(1);
        }
    };
    let warden: Rc<dyn Warden> = if warden_on {
        Rc::new(DaisugiWarden {
            command: fields(&warden_cmd),
            timeout: Duration::from_secs(10),
            session_id: Vec::new(),
        })
    } else {
        let _ = out(2, sprig::warden::warden_off_notice("weave").as_bytes());
        Rc::new(AllowAll)
    };
    let line = format!(
        "weave: running {} ({} steps)\n",
        sprig::goerr::quote(wf.name.as_bytes()),
        wf.steps.len
    );
    let _ = out(2, line.as_bytes());
    let results = wf.run(&mut |_| {
        let exec = Executor::new(
            sprig::tools::default_tools(),
            Box::new(Shared(warden.clone())),
        );
        Agent::new(Box::new(ClaudeCodeModel::new()), exec, 20)
    });
    let results = match results {
        Ok(r) => r,
        Err(e) => {
            let _ = out(2, &[b"weave: ".as_slice(), &e, b"\n"].concat());
            std::process::exit(1);
        }
    };
    if json_out {
        let _ = out(1, &sprig::weave::results_json(&results));
    } else {
        for r in &results {
            let status = match &r.err {
                None => b"ok".to_vec(),
                Some(e) => [b"FAILED: ".as_slice(), e].concat(),
            };
            let text = [
                b"=== ".as_slice(),
                r.step_id.as_bytes(),
                b" [",
                &status,
                b"] ===\n",
                &r.output,
                b"\n\n",
            ]
            .concat();
            let _ = out(1, &text);
        }
    }
    if results.iter().any(|r| r.err.is_some()) {
        std::process::exit(1);
    }
}
