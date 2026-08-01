use daisugi_verify::shell_decompose::ShellParser;
use daisugi_verify::wire;
use std::io::{self, BufRead, Write};

fn main() {
    let stdin = io::stdin();
    let stdout = io::stdout();
    let mut out = stdout.lock();
    let mut shell_parser = ShellParser::new();

    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("stdin read error: {e}");
                break;
            }
        };
        if line.trim().is_empty() {
            continue;
        }
        let verdict = wire::handle_line(&line, &mut shell_parser);
        let text = serde_json::to_string(&verdict).unwrap_or_else(|e| format!("{{\"error\":\"serialize failure: {e}\"}}"));
        if let Err(e) = writeln!(out, "{text}") {
            eprintln!("stdout write error: {e}");
            break;
        }
        if let Err(e) = out.flush() {
            eprintln!("stdout flush error: {e}");
            break;
        }
    }
}
