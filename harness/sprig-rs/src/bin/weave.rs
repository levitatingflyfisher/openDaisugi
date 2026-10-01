//! weave: runs a workflow file, a small JSON graph of steps, each a sprig
//! task behind the gate. Results to stdout, logs to stderr, real exit
//! codes, --json for scripts.

use sprig::agent::Agent;
use sprig::gate::{AllowAll, DaisugiGate, Executor, Gate, DEFAULT_GATE_CMD};
use sprig::gostr::fields;
use sprig::model_claude::ClaudeCodeModel;
use sprig::osx::out;
use std::os::unix::ffi::OsStrExt;
use std::rc::Rc;
use std::time::Duration;

fn usage() {
    let _ = out(
        2,
        b"usage: weave run <workflow.json> [--gate] [--json]\n\
run a JSON graph of gated agent steps in dependency order.\n",
    );
}

/// One gate shared by every step, as the Go binary shares its value.
struct Shared(Rc<dyn Gate>);

impl Gate for Shared {
    fn check(&self, call: &sprig::gate::ToolCall) -> sprig::gate::Verdict {
        self.0.check(call)
    }
}

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    let mut gate_on = false;
    let mut gate_cmd = DEFAULT_GATE_CMD.as_bytes().to_vec();
    let mut json_out = false;
    let mut file: Vec<u8> = Vec::new();
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_slice();
        if a == b"run" {
            // the optional verb
        } else if a == b"--gate" {
            gate_on = true;
        } else if a == b"--gate-cmd" && i + 1 < args.len() {
            gate_cmd = args[i + 1].clone();
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
    let gate: Rc<dyn Gate> = if gate_on {
        Rc::new(DaisugiGate {
            command: fields(&gate_cmd),
            timeout: Duration::from_secs(10),
            session_id: Vec::new(),
        })
    } else {
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
            Box::new(Shared(gate.clone())),
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
