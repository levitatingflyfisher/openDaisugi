//! rank_rule.py: no agent records a ranking vote. A shell line that runs
//! `daisugi rank record` is a hard deny, read word by word.

use super::pyjson::Value;
use super::record::Record;

/// `rank_rule.REFUSAL`.
pub const RANK_REFUSAL: &str = "only the owner records a ranking vote. Record it yourself.";

/// `rank_rule._WORD`: the ASCII characters a word is made of.
fn word_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || "_./:=+@%,-".contains(c)
}

/// `rank_rule._words`: backslashes and quotes dropped, then the runs of
/// word characters. Any other character, non-ASCII included, ends a word.
fn words(text: &str) -> Vec<String> {
    let text: String = text.chars().filter(|c| !matches!(c, '\\' | '\'' | '"')).collect();
    let mut out = Vec::new();
    let mut cur = String::new();
    for c in text.chars() {
        if word_char(c) {
            cur.push(c);
        } else if !cur.is_empty() {
            out.push(std::mem::take(&mut cur));
        }
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

fn is_head(w: &str) -> bool {
    let last = w.rsplit('/').next().unwrap_or(w);
    last == "daisugi" || last == "opendaisugi" || last.starts_with("opendaisugi.")
}

/// `rank_rule.names_rank_record`: a daisugi word, then `rank`, then
/// `record`, in that order.
pub fn names_rank_record(text: &str) -> bool {
    let mut state = 0;
    for w in words(text) {
        if state == 0 && is_head(&w) {
            state = 1;
        } else if state == 1 && w == "rank" {
            state = 2;
        } else if state == 2 && w == "record" {
            return true;
        }
    }
    false
}

/// `rank_rule.rank_record_hit`: a shell call whose command is a string
/// that names rank record.
pub fn rank_record_hit(rec: Option<&Record>) -> bool {
    match rec {
        Some(r) if r.step_type == "shell" => matches!(r.raw("command"), Value::Str(c) if names_rank_record(c)),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::names_rank_record;

    #[test]
    fn the_oracles_hits_and_near_misses() {
        for line in [
            "daisugi rank record --judge owner",
            "/usr/local/bin/daisugi rank record",
            "env FOO=1 daisugi rank record",
            "uv run --no-sync python -m opendaisugi rank record",
            "python -m opendaisugi.cli rank record",
            "sh -c 'daisugi rank record --judge owner'",
            "daisugi --data-dir /d rank --x record",
            "dai\\sugi rank rec\\ord",
            "'dai'sugi \"ra\"nk record",
            "true && daisugi\trank\nrecord",
            "daisugi\u{a0}rank record",
        ] {
            assert!(names_rank_record(line), "{line:?}");
        }
        for line in [
            "daisugi rank list",
            "daisugi record rank",
            "daisugi-helper rank record",
            "mydaisugi rank record",
            "daisugi rank recorder",
            "grep 'rank record' daisugi.log",
        ] {
            assert!(!names_rank_record(line), "{line:?}");
        }
    }
}
