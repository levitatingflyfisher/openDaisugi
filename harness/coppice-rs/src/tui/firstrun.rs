//! The one question the floor asks the first time it runs, before the
//! alternate screen.

use std::io::{Read, Write};

use crate::config::{self, Config, Found};

/// Writes the config file the first time coppice runs. With no harness it
/// prints the app-only note and returns it as the error. With one it makes
/// that one the default. With several it asks one numbered question and
/// reads one line; anything but a number from 1 to N takes the first.
pub fn first_run(
    found: &[Found],
    input: &mut dyn Read,
    out: &mut dyn Write,
) -> Result<Config, String> {
    if found.is_empty() {
        let _ = writeln!(out, "{}", config::APP_ONLY_NOTE);
        return Err(config::APP_ONLY_NOTE.into());
    }
    let mut pick = &found[0];
    if found.len() == 1 {
        let _ = writeln!(out, "{} is your default. Enter opens it here.", pick.name);
    } else {
        let _ = writeln!(out, "Which harness should Enter open?");
        for (i, f) in found.iter().enumerate() {
            let _ = writeln!(out, "  {}. {}    {}", i + 1, f.name, f.path);
        }
        let _ = write!(out, "Pick a number [1]: ");
        let _ = out.flush();
        let line = read_line(input);
        match super::keys::go_atoi(line.trim().as_bytes()) {
            Some(n) if n >= 1 && n <= found.len() as i64 => {
                pick = &found[(n - 1) as usize];
                let _ = writeln!(out, "{} is your default. Enter opens it here.", pick.name);
            }
            _ => {
                let _ = writeln!(
                    out,
                    "{} is your default. Edit {} to change it.",
                    pick.name,
                    config::path().display()
                );
            }
        }
    }
    let c = config::from_found(found, &pick.name);
    if let Err(e) = config::save(&c) {
        let _ = writeln!(out, "{e}");
        return Err(e);
    }
    let _ = write!(
        out,
        "\nEnter opens {} here. n opens another.\nSpace peeks. ctrl-c quits.\nctrl-t, then words, to talk.\n",
        pick.name
    );
    let _ = out.flush();
    Ok(c)
}

/// One line, a byte at a time, so nothing after it is taken from the
/// keys that follow.
fn read_line(input: &mut dyn Read) -> String {
    let mut out = Vec::new();
    let mut b = [0u8; 1];
    while let Ok(1) = input.read(&mut b) {
        out.push(b[0]);
        if b[0] == b'\n' {
            break;
        }
    }
    String::from_utf8_lossy(&out).into_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_harness_prints_the_note_and_writes_nothing() {
        let mut out: Vec<u8> = Vec::new();
        let err = first_run(&[], &mut &b""[..], &mut out).unwrap_err();
        assert_eq!(err, config::APP_ONLY_NOTE);
        assert_eq!(
            String::from_utf8(out).unwrap(),
            format!("{}\n", config::APP_ONLY_NOTE)
        );
    }

    #[test]
    fn the_answer_is_one_line_and_nothing_after_it_is_taken() {
        let mut input = &b"2\nrest"[..];
        assert_eq!(read_line(&mut input), "2\n");
        assert_eq!(input, b"rest");
    }
}
