//! coppice, the Rust port (harness/coppice-rs): the floor that owns the
//! panes and the agents in them. It speaks the same wire as the Go coppice,
//! so the two can be checked against each other.

// A handler's refusal is a whole reply, built once per request, so a large
// Err is the right shape.
#![allow(clippy::result_large_err)]

mod adapters;
mod attach;
mod cli;
mod config;
mod datahome;
mod detect;
mod godec;
mod gojson;
mod layout;
mod pane;
mod plugins;
mod proto;
mod server;
mod sha1;
mod sha256;
mod state;
mod sys;
#[cfg(test)]
mod testhome;
mod textwidth;
mod tiles;
mod tmuxmirror;
mod tui;
mod vt;
mod web;
mod worktree;

fn main() {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    std::process::exit(cli::run(argv));
}
