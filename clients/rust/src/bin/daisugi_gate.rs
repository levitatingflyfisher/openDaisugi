//! `daisugi-gate` answers one tool-call hook the way
//! `python -m opendaisugi.gate` does, with the same flags, stdin, stdout,
//! stderr, exit code and log lines, as one standalone binary. See
//! `gate::hook` for how the call is decided in a child process.

fn main() {
    let args: Vec<std::ffi::OsString> = std::env::args_os().skip(1).collect();
    daisugi_verify::gate::hook::main(args, &[])
}
