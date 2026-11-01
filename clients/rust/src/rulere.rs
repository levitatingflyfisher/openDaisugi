//! The regex of an MCP argument rule: one small subset, whole string
//! (`opendaisugi/rule_regex.py`). [`supported`] is the oracle's validator
//! ported step for step; `clients/fixtures/rule_regex_subset.json` holds the
//! verdicts all three ports share. A pattern outside the subset is refused
//! with one reason, [`invalid_reason`]. Inside the subset Python's RE2
//! binding, Go `regexp` and this `regex` crate give the same answers, and RE2
//! takes time linear in the input.
//!
//! The only rewriting left is [`translate`]: the crate reads `\d \w \s` as
//! Unicode (the subset means ASCII), and reads `\<` and `\>` as word edges
//! (the subset means the literal character). Both are spelled as ASCII sets
//! and `\x{..}` escapes.

use regex::Regex;
use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};

/// Repeat counts multiply to at most this.
pub const MAX_REPEAT: usize = 1000;
/// Groups nest at most this deep.
pub const MAX_DEPTH: usize = 50;
/// The expanded size (repeat counts multiplied in) is at most this.
pub const MAX_SIZE: usize = 2000;

struct Outside;

type R<T> = Result<T, Outside>;

struct Parser {
    s: Vec<char>,
    pos: usize,
}

fn is_punct(c: char) -> bool {
    c.is_ascii_punctuation()
}

fn is_not_literal(c: char) -> bool {
    matches!(c, '\\' | '.' | '^' | '$' | '[' | ']' | '(' | ')' | '|' | '*' | '+' | '?' | '{' | '}')
}

fn is_quant_start(c: Option<char>) -> bool {
    matches!(c, Some('*' | '+' | '?' | '{'))
}

impl Parser {
    fn peek(&self, k: usize) -> Option<char> {
        self.s.get(self.pos + k).copied()
    }

    fn run(&mut self) -> R<()> {
        if self.s.len() >= 4 && self.s[..4] == ['(', '?', 'i', ')'] {
            self.pos = 4;
        }
        let (size, prod) = self.alt(0)?;
        if self.pos != self.s.len() {
            return Err(Outside); // a stray ")"
        }
        if size > MAX_SIZE || prod > MAX_REPEAT {
            return Err(Outside);
        }
        Ok(())
    }

    fn alt(&mut self, depth: usize) -> R<(usize, usize)> {
        if depth > MAX_DEPTH {
            return Err(Outside);
        }
        let (mut size, mut prod) = (0usize, 1usize);
        loop {
            let (s, p) = self.seq(depth)?;
            size += s;
            prod = prod.max(p);
            if self.peek(0) == Some('|') {
                self.pos += 1;
                size += 1;
                continue;
            }
            return Ok((size, prod));
        }
    }

    fn seq(&mut self, depth: usize) -> R<(usize, usize)> {
        let (mut size, mut prod) = (0usize, 1usize);
        while self.pos < self.s.len() && self.peek(0) != Some('|') && self.peek(0) != Some(')') {
            let (mut a_size, mut a_prod, quantifiable) = self.atom(depth)?;
            if quantifiable && is_quant_start(self.peek(0)) {
                let (f_size, f_prod) = self.quant()?;
                a_size = a_size.saturating_mul(f_size);
                a_prod = a_prod.saturating_mul(f_prod);
                if is_quant_start(self.peek(0)) {
                    return Err(Outside); // a second quantifier
                }
            }
            size = size.saturating_add(a_size);
            prod = prod.max(a_prod);
        }
        Ok((size, prod))
    }

    fn atom(&mut self, depth: usize) -> R<(usize, usize, bool)> {
        let Some(c) = self.peek(0) else { return Err(Outside) };
        match c {
            '\\' => match self.peek(1) {
                Some(e) if matches!(e, 'd' | 'w' | 's' | 'D' | 'W' | 'S') || is_punct(e) => {
                    self.pos += 2;
                    Ok((1, 1, true))
                }
                _ => Err(Outside),
            },
            '.' => {
                self.pos += 1;
                Ok((1, 1, true))
            }
            '^' | '$' => {
                self.pos += 1;
                Ok((1, 1, false))
            }
            '[' => {
                self.class()?;
                Ok((1, 1, true))
            }
            '(' => {
                if self.peek(1) == Some('?') && self.peek(2) == Some(':') {
                    self.pos += 3;
                } else if self.peek(1) == Some('?') {
                    return Err(Outside);
                } else {
                    self.pos += 1;
                }
                let (size, prod) = self.alt(depth + 1)?;
                if self.peek(0) != Some(')') {
                    return Err(Outside);
                }
                self.pos += 1;
                Ok((size, prod, true))
            }
            c if is_not_literal(c) => Err(Outside), // a quantifier without an atom, a bare ] } or {
            _ => {
                self.pos += 1;
                Ok((1, 1, true))
            }
        }
    }

    fn quant(&mut self) -> R<(usize, usize)> {
        let c = self.peek(0);
        let (mut size, mut prod) = (1usize, 1usize);
        self.pos += 1;
        if c == Some('{') {
            let m = self.number()?;
            let mut hi = Some(m);
            if self.peek(0) == Some(',') {
                self.pos += 1;
                hi = if self.peek(0).is_some_and(|d| d.is_ascii_digit()) { Some(self.number()?) } else { None };
            }
            if self.peek(0) != Some('}') {
                return Err(Outside);
            }
            self.pos += 1;
            if hi.is_some_and(|h| m > h) {
                return Err(Outside);
            }
            prod = hi.unwrap_or(m);
            size = hi.unwrap_or(m + 1);
            if prod == 0 {
                prod = 1;
            }
            if size == 0 {
                size = 1;
            }
        }
        if self.peek(0) == Some('?') {
            self.pos += 1;
        }
        Ok((size, prod))
    }

    fn number(&mut self) -> R<usize> {
        let start = self.pos;
        while self.peek(0).is_some_and(|d| d.is_ascii_digit()) {
            self.pos += 1;
        }
        let digits = &self.s[start..self.pos];
        if digits.is_empty() || digits.len() > 4 || (digits.len() > 1 && digits[0] == '0') {
            return Err(Outside);
        }
        let v: usize = digits.iter().collect::<String>().parse().map_err(|_| Outside)?;
        if v > MAX_REPEAT {
            return Err(Outside);
        }
        Ok(v)
    }

    /// One class item: (is a class escape, code point).
    fn single(&mut self) -> R<(bool, u32)> {
        let Some(c) = self.peek(0) else { return Err(Outside) };
        if c == '\\' {
            return match self.peek(1) {
                Some(e) if matches!(e, 'd' | 'w' | 's') => {
                    self.pos += 2;
                    Ok((true, 0))
                }
                Some(e) if is_punct(e) => {
                    self.pos += 2;
                    Ok((false, e as u32))
                }
                _ => Err(Outside),
            };
        }
        if c == '[' {
            return Err(Outside);
        }
        self.pos += 1;
        Ok((false, c as u32))
    }

    fn class(&mut self) -> R<()> {
        self.pos += 1; // [
        if self.peek(0) == Some('^') {
            self.pos += 1;
        }
        let mut items = 0usize;
        loop {
            let Some(c) = self.peek(0) else { return Err(Outside) };
            if c == ']' {
                if items == 0 {
                    return Err(Outside);
                }
                self.pos += 1;
                return Ok(());
            }
            if (c == '&' && self.peek(1) == Some('&')) || (c == '~' && self.peek(1) == Some('~')) {
                return Err(Outside);
            }
            if c == '-' {
                if self.peek(1) == Some('-') || self.s[self.pos - 1] == '-' {
                    return Err(Outside);
                }
                if items == 0 || self.peek(1) == Some(']') {
                    self.pos += 1;
                    items += 1;
                    continue;
                }
                return Err(Outside);
            }
            let (is_cls, lo) = self.single()?;
            items += 1;
            if self.peek(0) == Some('-') && self.peek(1) != Some(']') {
                if is_cls || self.peek(1) == Some('-') || self.s[self.pos - 1] == '-' {
                    return Err(Outside);
                }
                self.pos += 1;
                let (hi_cls, hi) = self.single()?;
                if hi_cls || hi < lo {
                    return Err(Outside);
                }
            }
        }
    }
}

/// `rule_regex.supported`: the pattern is inside the subset.
pub fn supported(pattern: &str) -> bool {
    if crate::warden::py::text::has_surrogate(pattern) {
        return false;
    }
    Parser { s: pattern.chars().collect(), pos: 0 }.run().is_ok()
}

/// The pattern as the `regex` crate must read it, for a pattern already in the
/// subset: `\d \w \s` and their negations are ASCII sets, and an escaped
/// punctuation character is its `\x{..}` escape.
fn translate(pattern: &str) -> String {
    let cs: Vec<char> = pattern.chars().collect();
    let mut out = String::with_capacity(pattern.len() + 8);
    let mut in_class = false;
    let mut i = 0usize;
    while i < cs.len() {
        let c = cs[i];
        if c == '\\' {
            let n = cs[i + 1];
            i += 2;
            match n {
                'd' => out.push_str(if in_class { "0-9" } else { "[0-9]" }),
                'D' => out.push_str("[^0-9]"),
                'w' => out.push_str(if in_class { "0-9A-Za-z_" } else { "[0-9A-Za-z_]" }),
                'W' => out.push_str("[^0-9A-Za-z_]"),
                's' => out.push_str(if in_class { "\\t\\n\\f\\r " } else { "[\\t\\n\\f\\r ]" }),
                'S' => out.push_str("[^\\t\\n\\f\\r ]"),
                _ => out.push_str(&format!("\\x{{{:x}}}", n as u32)),
            }
            continue;
        }
        if in_class {
            if c == ']' {
                in_class = false;
            }
        } else if c == '[' {
            in_class = true;
            out.push('[');
            i += 1;
            if cs.get(i) == Some(&'^') {
                out.push('^');
                i += 1;
            }
            continue;
        }
        out.push(c);
        i += 1;
    }
    out
}

type Cached = Result<Arc<Regex>, ()>;

fn cache() -> &'static Mutex<HashMap<String, Cached>> {
    static CACHE: OnceLock<Mutex<HashMap<String, Cached>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

/// The cached whole-string compile.
fn compiled(pattern: &str) -> Cached {
    if let Some(c) = cache().lock().unwrap_or_else(|e| e.into_inner()).get(pattern) {
        return c.clone();
    }
    let built = if supported(pattern) {
        Regex::new(&format!(r"\A(?:{})\z", translate(pattern))).map(Arc::new).map_err(|_| ())
    } else {
        Err(())
    };
    cache().lock().unwrap_or_else(|e| e.into_inner()).insert(pattern.to_string(), built.clone());
    built
}

/// `rule_regex.compiles`: in the subset, and the engine compiles it.
pub fn valid(pattern: &str) -> bool {
    compiled(pattern).is_ok()
}

/// `rule_regex.fullmatch`: `Err` when the pattern does not compile.
pub fn full_match(pattern: &str, text: &str) -> Result<bool, ()> {
    Ok(compiled(pattern)?.is_match(text))
}

/// `rule_regex.invalid_reason`.
pub fn invalid_reason(pattern: &str) -> String {
    format!("regex '{pattern}' is outside the supported subset")
}

/// What `predicate_z3._regex_hit` checks after the length cap: a pattern
/// outside the subset, then a value that cannot be written as UTF-8 (a lone
/// surrogate). `None` when the rule may run.
pub fn text_refusal(pattern: &str, text: &str, path: &str) -> Option<String> {
    if !valid(pattern) {
        return Some(format!("argument rule refused: {}", invalid_reason(pattern)));
    }
    if crate::warden::py::text::has_surrogate(text) {
        return Some(format!("argument rule refused: argument '{path}' is not valid UTF-8 text"));
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    fn fixture(name: &str) -> Vec<Value> {
        let raw = std::fs::read_to_string(format!("{}/../fixtures/{name}", env!("CARGO_MANIFEST_DIR"))).unwrap();
        serde_json::from_str::<Value>(&raw).unwrap().as_array().unwrap().clone()
    }

    #[test]
    fn the_validator_gives_the_fixture_verdict() {
        let rows = fixture("rule_regex_subset.json");
        assert!(rows.len() > 300);
        for r in rows {
            let p = r["pattern"].as_str().unwrap();
            let ok = r["ok"].as_bool().unwrap();
            assert_eq!(supported(p), ok, "supported {p:?}");
            assert_eq!(valid(p), ok, "valid {p:?}");
        }
    }

    #[test]
    fn the_engine_answers_the_recorded_matches() {
        for r in fixture("rule_regex_match.json") {
            let p = r["pattern"].as_str().unwrap();
            let got: String = r["probes"]
                .as_array()
                .unwrap()
                .iter()
                .map(|t| if full_match(p, t.as_str().unwrap()).unwrap() { '1' } else { '0' })
                .collect();
            assert_eq!(got, r["match"].as_str().unwrap(), "{p:?}");
        }
    }

    #[test]
    fn a_surrogate_in_a_pattern_or_a_value_is_refused() {
        use crate::warden::py::text::cp_char_string;
        let sur = cp_char_string(0xD800);
        assert!(!supported(&format!("a{sur}")));
        assert_eq!(
            text_refusal("^evil$", &format!("x{sur}"), "a").as_deref(),
            Some("argument rule refused: argument 'a' is not valid UTF-8 text")
        );
        assert_eq!(text_refusal("^evil$", "good\u{fffd}", "a"), None);
        assert_eq!(
            text_refusal("(?x)a", &sur, "a").as_deref(),
            Some("argument rule refused: regex '(?x)a' is outside the supported subset")
        );
    }

    #[test]
    fn the_old_hanging_shapes_are_inside_the_subset_and_answer_at_once() {
        let start = std::time::Instant::now();
        for rx in ["^(a|aa)+$", "^(a+)+$", "^(.*)*$", "^a*a*a*a*b$"] {
            assert!(supported(rx), "{rx}");
            for text in ["a".repeat(4096), format!("{}!", "a".repeat(4095))] {
                assert!(full_match(rx, &text).is_ok());
            }
        }
        assert!(start.elapsed().as_secs() < 1);
    }
}
