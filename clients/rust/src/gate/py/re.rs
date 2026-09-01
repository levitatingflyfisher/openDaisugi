//! Python's `re` for `str` patterns: the parser of `re._parser` (its
//! syntax, its tree and its error messages), the checks of
//! `re._compiler`, and a backtracking matcher with the matching rules of
//! `_sre`. The gate uses it for its own rule patterns, for `Matches`
//! predicates in envelopes, and for the Z3 regex translation, so all three
//! read a pattern exactly as the Python gate does.

use super::err::{PyErr, PyResult};
use super::tables;
use super::text::{as_surrogate, is_alnum, is_decimal, is_space};
use std::collections::HashMap;
use std::sync::OnceLock;

pub const MAXREPEAT: u64 = 4294967295;
pub const MAXGROUPS: usize = 1073741823;
const MAXWIDTH: u128 = 1 << 64;
const MAXCODE: u128 = (1 << 32) - 1;

pub mod flags {
    pub const TEMPLATE: u32 = 1;
    pub const IGNORECASE: u32 = 2;
    pub const LOCALE: u32 = 4;
    pub const MULTILINE: u32 = 8;
    pub const DOTALL: u32 = 16;
    pub const UNICODE: u32 = 32;
    pub const VERBOSE: u32 = 64;
    pub const DEBUG: u32 = 128;
    pub const ASCII: u32 = 256;
}
use flags::*;
const TYPE_FLAGS: u32 = ASCII | LOCALE | UNICODE;
const GLOBAL_FLAGS: u32 = DEBUG | TEMPLATE;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Cat {
    Digit,
    NotDigit,
    Space,
    NotSpace,
    Word,
    NotWord,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SetItem {
    Negate,
    Literal(u32),
    Range(u32, u32),
    Category(Cat),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum At {
    Beginning,
    BeginningString,
    Boundary,
    NonBoundary,
    End,
    EndString,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RepKind {
    Max,
    Min,
    Possessive,
}

/// One node of `re._parser`'s tree.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Node {
    Literal(u32),
    NotLiteral(u32),
    Any,
    In(Vec<SetItem>),
    Branch(Vec<Vec<Node>>),
    Repeat { kind: RepKind, min: u64, max: u64, body: Vec<Node> },
    Subpattern { group: Option<usize>, add: u32, del: u32, body: Vec<Node> },
    Atomic(Vec<Node>),
    Assert { behind: bool, neg: bool, body: Vec<Node> },
    GroupRef(usize),
    GroupRefExists { group: usize, yes: Vec<Node>, no: Option<Vec<Node>> },
    At(At),
}

/// A parsed pattern.
#[derive(Clone, Debug)]
pub struct Parsed {
    pub nodes: Vec<Node>,
    /// The pattern's flags after parsing, as `parsed.state.flags`.
    pub flags: u32,
    pub groups: usize,
    /// Whether parsing prints a FutureWarning.
    pub warned: bool,
}

/// The code point Python sees for one character of a string.
pub fn pycp(c: char) -> u32 {
    as_surrogate(c).unwrap_or(c as u32)
}

thread_local! {
    static WARNED: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

/// Whether parsing `pattern` prints a FutureWarning, as Python's parser
/// does before it raises or returns.
pub fn parse_warns(pattern: &str) -> bool {
    WARNED.with(|w| w.set(false));
    let _ = parse(pattern, 0);
    WARNED.with(|w| w.get())
}

/// `re.error(msg, pattern, pos)`, as `str()` prints it.
fn re_error(msg: &str, pattern: &[char], pos: Option<usize>) -> PyErr {
    let mut m = msg.to_string();
    if let Some(pos) = pos {
        m = format!("{m} at position {pos}");
        if pattern.contains(&'\n') {
            let before = &pattern[..pos.min(pattern.len())];
            let lineno = before.iter().filter(|&&c| c == '\n').count() + 1;
            let colno = match before.iter().rposition(|&c| c == '\n') {
                Some(i) => pos - i,
                None => pos + 1,
            };
            m = format!("{m} (line {lineno}, column {colno})");
        }
    }
    PyErr::new("error", m)
}

struct Tokenizer<'a> {
    s: &'a [char],
    index: usize,
    next: Option<String>,
}

impl<'a> Tokenizer<'a> {
    fn new(s: &'a [char]) -> PyResult<Self> {
        let mut t = Tokenizer { s, index: 0, next: None };
        t.advance()?;
        Ok(t)
    }

    fn advance(&mut self) -> PyResult<()> {
        let mut index = self.index;
        let c = match self.s.get(index) {
            None => {
                self.next = None;
                return Ok(());
            }
            Some(&c) => c,
        };
        let mut tok = c.to_string();
        if c == '\\' {
            index += 1;
            match self.s.get(index) {
                Some(&d) => tok.push(d),
                None => return Err(re_error("bad escape (end of pattern)", self.s, Some(self.s.len() - 1))),
            }
        }
        self.index = index + 1;
        self.next = Some(tok);
        Ok(())
    }

    fn next_is(&self, t: &str) -> bool {
        self.next.as_deref() == Some(t)
    }

    fn next_in(&self, set: &str) -> bool {
        match &self.next {
            Some(t) => t.chars().count() == 1 && set.contains(t.as_str()),
            None => false,
        }
    }

    fn matches(&mut self, t: &str) -> PyResult<bool> {
        if self.next_is(t) {
            self.advance()?;
            return Ok(true);
        }
        Ok(false)
    }

    fn get(&mut self) -> PyResult<Option<String>> {
        let this = self.next.clone();
        self.advance()?;
        Ok(this)
    }

    fn getwhile(&mut self, n: usize, set: &str) -> PyResult<String> {
        let mut out = String::new();
        for _ in 0..n {
            if !self.next_in(set) {
                break;
            }
            out.push_str(self.next.as_deref().unwrap_or(""));
            self.advance()?;
        }
        Ok(out)
    }

    fn getuntil(&mut self, terminator: char, name: &str) -> PyResult<String> {
        let mut result = String::new();
        loop {
            let c = self.next.clone();
            self.advance()?;
            match c {
                None => {
                    if result.is_empty() {
                        return Err(self.error(&format!("missing {name}"), 0));
                    }
                    return Err(self.error(
                        &format!("missing {terminator}, unterminated name"),
                        result.chars().count(),
                    ));
                }
                Some(c) if c == terminator.to_string() => {
                    if result.is_empty() {
                        return Err(self.error(&format!("missing {name}"), 1));
                    }
                    break;
                }
                Some(c) => result.push_str(&c),
            }
        }
        Ok(result)
    }

    fn tell(&self) -> usize {
        self.index - self.next.as_ref().map(|t| t.chars().count()).unwrap_or(0)
    }

    fn seek(&mut self, index: usize) -> PyResult<()> {
        self.index = index;
        self.advance()
    }

    fn error(&self, msg: &str, offset: usize) -> PyErr {
        re_error(msg, self.s, Some(self.tell().saturating_sub(offset)))
    }

    fn checkgroupname(&self, name: &str, offset: usize) -> PyResult<()> {
        if !is_identifier(name) {
            return Err(self.error(
                &format!("bad character in group name {}", super::text::repr(name)),
                name.chars().count() + offset,
            ));
        }
        Ok(())
    }
}

/// `str.isidentifier()` for the ASCII-plus-letters names a group can hold.
fn is_identifier(s: &str) -> bool {
    let mut it = s.chars();
    match it.next() {
        None => false,
        Some(c) if c == '_' || c.is_alphabetic() => it.all(|c| c == '_' || c.is_alphanumeric()),
        _ => false,
    }
}

struct State {
    flags: u32,
    groupdict: HashMap<String, usize>,
    groupwidths: Vec<Option<(u128, u128)>>,
    lookbehindgroups: Option<usize>,
    grouprefpos: Vec<(usize, usize)>,
    /// A FutureWarning the parser would print (a possible nested set or
    /// set operation).
    warned: bool,
}

impl State {
    fn groups(&self) -> usize {
        self.groupwidths.len()
    }
    fn checkgroup(&self, gid: usize) -> bool {
        gid < self.groups() && self.groupwidths[gid].is_some()
    }
    fn checklookbehindgroup(&self, gid: usize, source: &Tokenizer) -> PyResult<()> {
        if let Some(lb) = self.lookbehindgroups {
            if !self.checkgroup(gid) {
                return Err(source.error("cannot refer to an open group", 0));
            }
            if gid >= lb {
                return Err(source.error("cannot refer to group defined in the same lookbehind subpattern", 0));
            }
        }
        Ok(())
    }
}

/// The (min, max) width of a subpattern, as `SubPattern.getwidth`.
pub fn getwidth(nodes: &[Node], groupwidths: &[Option<(u128, u128)>]) -> (u128, u128) {
    let (mut lo, mut hi): (u128, u128) = (0, 0);
    for n in nodes {
        match n {
            Node::Branch(alts) => {
                let mut i = MAXWIDTH;
                let mut j = 0;
                for a in alts {
                    let (l, h) = getwidth(a, groupwidths);
                    i = i.min(l);
                    j = j.max(h);
                }
                lo += i;
                hi += j;
            }
            Node::Atomic(b) | Node::Subpattern { body: b, .. } => {
                let (i, j) = getwidth(b, groupwidths);
                lo += i;
                hi += j;
            }
            Node::Repeat { min, max, body, .. } => {
                let (i, j) = getwidth(body, groupwidths);
                lo += i * (*min as u128);
                if *max == MAXREPEAT && j > 0 {
                    hi = MAXWIDTH;
                } else {
                    hi += j * (*max as u128);
                }
            }
            Node::Any | Node::In(_) | Node::Literal(_) | Node::NotLiteral(_) => {
                lo += 1;
                hi += 1;
            }
            Node::GroupRef(g) => {
                if let Some(Some((i, j))) = groupwidths.get(*g) {
                    lo += i;
                    hi += j;
                }
            }
            Node::GroupRefExists { yes, no, .. } => {
                let (mut i, mut j) = getwidth(yes, groupwidths);
                match no {
                    Some(no) => {
                        let (l, h) = getwidth(no, groupwidths);
                        i = i.min(l);
                        j = j.max(h);
                    }
                    None => i = 0,
                }
                lo += i;
                hi += j;
            }
            Node::Assert { .. } | Node::At(_) => {}
        }
    }
    (lo.min(MAXWIDTH), hi.min(MAXWIDTH))
}

const ASCIILETTERS: &str = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
const DIGITS: &str = "0123456789";
const OCTDIGITS: &str = "01234567";
const HEXDIGITS: &str = "0123456789abcdefABCDEF";
const SPECIAL_CHARS: &str = ".\\[{()*+?^$|";
const REPEAT_CHARS: &str = "*+?{";
const WHITESPACE: &str = " \t\n\r\u{b}\u{c}";

fn escape_code(e: &str) -> Option<u32> {
    Some(match e {
        "\\a" => 7,
        "\\b" => 8,
        "\\f" => 12,
        "\\n" => 10,
        "\\r" => 13,
        "\\t" => 9,
        "\\v" => 11,
        "\\\\" => 92,
        _ => return None,
    })
}

enum Esc {
    Node(Node),
    Set(Vec<SetItem>),
}

fn category_code(e: &str) -> Option<Esc> {
    Some(match e {
        "\\A" => Esc::Node(Node::At(At::BeginningString)),
        "\\b" => Esc::Node(Node::At(At::Boundary)),
        "\\B" => Esc::Node(Node::At(At::NonBoundary)),
        "\\Z" => Esc::Node(Node::At(At::EndString)),
        "\\d" => Esc::Set(vec![SetItem::Category(Cat::Digit)]),
        "\\D" => Esc::Set(vec![SetItem::Category(Cat::NotDigit)]),
        "\\s" => Esc::Set(vec![SetItem::Category(Cat::Space)]),
        "\\S" => Esc::Set(vec![SetItem::Category(Cat::NotSpace)]),
        "\\w" => Esc::Set(vec![SetItem::Category(Cat::Word)]),
        "\\W" => Esc::Set(vec![SetItem::Category(Cat::NotWord)]),
        _ => return None,
    })
}

fn parse_hex(s: &str) -> u32 {
    u32::from_str_radix(s, 16).unwrap_or(0)
}

/// The item a class escape stands for: a literal, or a category set.
enum ClassCode {
    Literal(u32),
    Set(Vec<SetItem>),
}

fn class_escape(source: &mut Tokenizer, escape: &str) -> PyResult<ClassCode> {
    if let Some(c) = escape_code(escape) {
        return Ok(ClassCode::Literal(c));
    }
    if let Some(Esc::Set(s)) = category_code(escape) {
        return Ok(ClassCode::Set(s));
    }
    let mut escape = escape.to_string();
    let c: String = escape.chars().nth(1).map(String::from).unwrap_or_default();
    let len = |e: &String| e.chars().count();
    match c.as_str() {
        "x" => {
            escape += &source.getwhile(2, HEXDIGITS)?;
            if len(&escape) != 4 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            return Ok(ClassCode::Literal(parse_hex(&escape[2..])));
        }
        "u" => {
            escape += &source.getwhile(4, HEXDIGITS)?;
            if len(&escape) != 6 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            return Ok(ClassCode::Literal(parse_hex(&escape[2..])));
        }
        "U" => {
            escape += &source.getwhile(8, HEXDIGITS)?;
            if len(&escape) != 10 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            let v = parse_hex(&escape[2..]);
            if v > 0x10FFFF {
                return Err(source.error(&format!("bad escape {escape}"), len(&escape)));
            }
            return Ok(ClassCode::Literal(v));
        }
        "N" => return Err(PyErr::undecided("a \\N{...} escape in a regular expression")),
        c if c.len() == 1 && OCTDIGITS.contains(c) => {
            escape += &source.getwhile(2, OCTDIGITS)?;
            let v = u32::from_str_radix(&escape[1..], 8).unwrap_or(0);
            if v > 0o377 {
                return Err(source.error(
                    &format!("octal escape value {escape} outside of range 0-0o377"),
                    len(&escape),
                ));
            }
            return Ok(ClassCode::Literal(v));
        }
        c if c.len() == 1 && DIGITS.contains(c) => {}
        _ => {
            if len(&escape) == 2 {
                if ASCIILETTERS.contains(c.as_str()) {
                    return Err(source.error(&format!("bad escape {escape}"), len(&escape)));
                }
                return Ok(ClassCode::Literal(pycp(escape.chars().nth(1).unwrap())));
            }
        }
    }
    Err(source.error(&format!("bad escape {escape}"), len(&escape)))
}

fn parse_escape(source: &mut Tokenizer, escape: &str, state: &State) -> PyResult<Node> {
    if let Some(code) = category_code(escape) {
        return Ok(match code {
            Esc::Node(n) => n,
            Esc::Set(s) => Node::In(s),
        });
    }
    if let Some(c) = escape_code(escape) {
        return Ok(Node::Literal(c));
    }
    let mut escape = escape.to_string();
    let c: String = escape.chars().nth(1).map(String::from).unwrap_or_default();
    let len = |e: &String| e.chars().count();
    match c.as_str() {
        "x" => {
            escape += &source.getwhile(2, HEXDIGITS)?;
            if len(&escape) != 4 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            return Ok(Node::Literal(parse_hex(&escape[2..])));
        }
        "u" => {
            escape += &source.getwhile(4, HEXDIGITS)?;
            if len(&escape) != 6 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            return Ok(Node::Literal(parse_hex(&escape[2..])));
        }
        "U" => {
            escape += &source.getwhile(8, HEXDIGITS)?;
            if len(&escape) != 10 {
                return Err(source.error(&format!("incomplete escape {escape}"), len(&escape)));
            }
            let v = parse_hex(&escape[2..]);
            if v > 0x10FFFF {
                return Err(source.error(&format!("bad escape {escape}"), len(&escape)));
            }
            return Ok(Node::Literal(v));
        }
        "N" => return Err(PyErr::undecided("a \\N{...} escape in a regular expression")),
        "0" => {
            escape += &source.getwhile(2, OCTDIGITS)?;
            return Ok(Node::Literal(u32::from_str_radix(&escape[1..], 8).unwrap_or(0)));
        }
        c if c.len() == 1 && DIGITS.contains(c) => {
            if source.next_in(DIGITS) {
                escape += &source.get()?.unwrap_or_default();
                let b: Vec<char> = escape.chars().collect();
                if OCTDIGITS.contains(b[1]) && OCTDIGITS.contains(b[2]) && source.next_in(OCTDIGITS) {
                    escape += &source.get()?.unwrap_or_default();
                    let v = u32::from_str_radix(&escape[1..], 8).unwrap_or(0);
                    if v > 0o377 {
                        return Err(source.error(
                            &format!("octal escape value {escape} outside of range 0-0o377"),
                            len(&escape),
                        ));
                    }
                    return Ok(Node::Literal(v));
                }
            }
            let group: usize = escape[1..].parse().unwrap_or(usize::MAX);
            if group < state.groups() {
                if !state.checkgroup(group) {
                    return Err(source.error("cannot refer to an open group", len(&escape)));
                }
                state.checklookbehindgroup(group, source)?;
                return Ok(Node::GroupRef(group));
            }
            return Err(source.error(&format!("invalid group reference {group}"), len(&escape) - 1));
        }
        _ => {
            if len(&escape) == 2 {
                if ASCIILETTERS.contains(c.as_str()) {
                    return Err(source.error(&format!("bad escape {escape}"), len(&escape)));
                }
                return Ok(Node::Literal(pycp(escape.chars().nth(1).unwrap())));
            }
        }
    }
    Err(source.error(&format!("bad escape {escape}"), len(&escape)))
}

fn uniq(items: Vec<SetItem>) -> Vec<SetItem> {
    let mut out: Vec<SetItem> = Vec::new();
    for i in items {
        if !out.contains(&i) {
            out.push(i);
        }
    }
    out
}

fn parse_sub(source: &mut Tokenizer, state: &mut State, mut verbose: bool, nested: usize) -> PyResult<Vec<Node>> {
    let mut items: Vec<Vec<Node>> = Vec::new();
    loop {
        let first = nested == 0 && items.is_empty();
        items.push(parse_seq(source, state, verbose, nested + 1, first)?);
        if !source.matches("|")? {
            break;
        }
        if nested == 0 {
            verbose = state.flags & VERBOSE != 0;
        }
    }
    if items.len() == 1 {
        return Ok(items.pop().unwrap());
    }
    let mut sub: Vec<Node> = Vec::new();
    // Move a common prefix out of the branch.
    loop {
        let mut prefix: Option<Node> = None;
        let mut all = true;
        for item in &items {
            match item.first() {
                None => {
                    all = false;
                    break;
                }
                Some(n) => match &prefix {
                    None => prefix = Some(n.clone()),
                    Some(p) if p != n => {
                        all = false;
                        break;
                    }
                    _ => {}
                },
            }
        }
        if all {
            for item in items.iter_mut() {
                item.remove(0);
            }
            sub.push(prefix.unwrap());
            continue;
        }
        break;
    }
    // A branch of single literals or plain sets is one set.
    let mut set: Vec<SetItem> = Vec::new();
    let mut all_set = true;
    for item in &items {
        if item.len() != 1 {
            all_set = false;
            break;
        }
        match &item[0] {
            Node::Literal(c) => set.push(SetItem::Literal(*c)),
            Node::In(av) if av.first() != Some(&SetItem::Negate) => set.extend(av.iter().cloned()),
            _ => {
                all_set = false;
                break;
            }
        }
    }
    if all_set {
        sub.push(Node::In(uniq(set)));
        return Ok(sub);
    }
    sub.push(Node::Branch(items));
    Ok(sub)
}

fn parse_flags(source: &mut Tokenizer, state: &mut State, first_char: &str) -> PyResult<Option<(u32, u32)>> {
    let flag_of = |c: &str| -> Option<u32> {
        Some(match c {
            "i" => IGNORECASE,
            "L" => LOCALE,
            "m" => MULTILINE,
            "s" => DOTALL,
            "x" => VERBOSE,
            "a" => ASCII,
            "t" => TEMPLATE,
            "u" => UNICODE,
            _ => return None,
        })
    };
    let is_alpha = |c: &str| c.chars().all(|x| x.is_alphabetic());
    let mut add = 0u32;
    let mut del = 0u32;
    let mut ch = first_char.to_string();
    if ch != "-" {
        loop {
            let flag = flag_of(&ch).unwrap();
            if ch == "L" {
                return Err(source.error("bad inline flags: cannot use 'L' flag with a str pattern", 0));
            }
            add |= flag;
            if flag & TYPE_FLAGS != 0 && (add & TYPE_FLAGS) != flag {
                return Err(source.error("bad inline flags: flags 'a', 'u' and 'L' are incompatible", 0));
            }
            ch = match source.get()? {
                None => return Err(source.error("missing -, : or )", 0)),
                Some(c) => c,
            };
            if ch == ")" || ch == "-" || ch == ":" {
                break;
            }
            if flag_of(&ch).is_none() {
                let msg = if is_alpha(&ch) { "unknown flag" } else { "missing -, : or )" };
                return Err(source.error(msg, ch.chars().count()));
            }
        }
    }
    if ch == ")" {
        state.flags |= add;
        return Ok(None);
    }
    if add & GLOBAL_FLAGS != 0 {
        return Err(source.error("bad inline flags: cannot turn on global flag", 1));
    }
    if ch == "-" {
        ch = match source.get()? {
            None => return Err(source.error("missing flag", 0)),
            Some(c) => c,
        };
        if flag_of(&ch).is_none() {
            let msg = if is_alpha(&ch) { "unknown flag" } else { "missing flag" };
            return Err(source.error(msg, ch.chars().count()));
        }
        loop {
            let flag = flag_of(&ch).unwrap();
            if flag & TYPE_FLAGS != 0 {
                return Err(source.error("bad inline flags: cannot turn off flags 'a', 'u' and 'L'", 0));
            }
            del |= flag;
            ch = match source.get()? {
                None => return Err(source.error("missing :", 0)),
                Some(c) => c,
            };
            if ch == ":" {
                break;
            }
            if flag_of(&ch).is_none() {
                let msg = if is_alpha(&ch) { "unknown flag" } else { "missing :" };
                return Err(source.error(msg, ch.chars().count()));
            }
        }
    }
    if del & GLOBAL_FLAGS != 0 {
        return Err(source.error("bad inline flags: cannot turn off global flag", 1));
    }
    if add & del != 0 {
        return Err(source.error("bad inline flags: flag turned on and off", 1));
    }
    Ok(Some((add, del)))
}

fn parse_seq(
    source: &mut Tokenizer,
    state: &mut State,
    mut verbose: bool,
    nested: usize,
    first: bool,
) -> PyResult<Vec<Node>> {
    let mut sub: Vec<Node> = Vec::new();
    loop {
        let this = match &source.next {
            None => break,
            Some(t) => t.clone(),
        };
        if this == "|" || this == ")" {
            break;
        }
        source.get()?;
        if verbose {
            if this.chars().count() == 1 && WHITESPACE.contains(this.as_str()) {
                continue;
            }
            if this == "#" {
                loop {
                    match source.get()? {
                        None => break,
                        Some(t) if t == "\n" => break,
                        _ => {}
                    }
                }
                continue;
            }
        }
        let head = this.chars().next().unwrap();
        if head == '\\' {
            sub.push(parse_escape(source, &this, state)?);
        } else if !SPECIAL_CHARS.contains(head) {
            sub.push(Node::Literal(pycp(head)));
        } else if this == "[" {
            let here = source.tell() - 1;
            let mut set: Vec<SetItem> = Vec::new();
            if source.next.as_deref() == Some("[") {
                state.warned = true;
                WARNED.with(|w| w.set(true));
            }
            let negate = source.matches("^")?;
            loop {
                let this = match source.get()? {
                    None => return Err(source.error("unterminated character set", source.tell() - here)),
                    Some(t) => t,
                };
                if this == "]" && !set.is_empty() {
                    break;
                }
                let code1 = if this.starts_with('\\') {
                    class_escape(source, &this)?
                } else {
                    if !set.is_empty() && ["-", "&", "~", "|"].contains(&this.as_str()) && source.next.as_deref() == Some(this.as_str()) {
                        state.warned = true;
                WARNED.with(|w| w.set(true));
                    }
                    ClassCode::Literal(pycp(this.chars().next().unwrap()))
                };
                if source.matches("-")? {
                    let that = match source.get()? {
                        None => return Err(source.error("unterminated character set", source.tell() - here)),
                        Some(t) => t,
                    };
                    if that == "]" {
                        match code1 {
                            ClassCode::Set(s) => set.push(s[0].clone()),
                            ClassCode::Literal(c) => set.push(SetItem::Literal(c)),
                        }
                        set.push(SetItem::Literal('-' as u32));
                        break;
                    }
                    let code2 = if that.starts_with('\\') {
                        class_escape(source, &that)?
                    } else {
                        if that == "-" {
                            state.warned = true;
                WARNED.with(|w| w.set(true));
                        }
                        ClassCode::Literal(pycp(that.chars().next().unwrap()))
                    };
                    let msg = format!("bad character range {this}-{that}");
                    let off = this.chars().count() + 1 + that.chars().count();
                    let (lo, hi) = match (code1, code2) {
                        (ClassCode::Literal(a), ClassCode::Literal(b)) => (a, b),
                        _ => return Err(source.error(&msg, off)),
                    };
                    if hi < lo {
                        return Err(source.error(&msg, off));
                    }
                    set.push(SetItem::Range(lo, hi));
                } else {
                    match code1 {
                        ClassCode::Set(s) => set.push(s[0].clone()),
                        ClassCode::Literal(c) => set.push(SetItem::Literal(c)),
                    }
                }
            }
            let mut set = uniq(set);
            if set.len() == 1 {
                if let SetItem::Literal(c) = set[0] {
                    sub.push(if negate { Node::NotLiteral(c) } else { Node::Literal(c) });
                    continue;
                }
            }
            if negate {
                set.insert(0, SetItem::Negate);
            }
            sub.push(Node::In(set));
        } else if REPEAT_CHARS.contains(head) {
            let here = source.tell();
            let (min, max) = match this.as_str() {
                "?" => (0, 1),
                "*" => (0, MAXREPEAT),
                "+" => (1, MAXREPEAT),
                _ => {
                    if source.next_is("}") {
                        sub.push(Node::Literal('{' as u32));
                        continue;
                    }
                    let mut lo = String::new();
                    let mut hi = String::new();
                    while source.next_in(DIGITS) {
                        lo += &source.get()?.unwrap();
                    }
                    if source.matches(",")? {
                        while source.next_in(DIGITS) {
                            hi += &source.get()?.unwrap();
                        }
                    } else {
                        hi = lo.clone();
                    }
                    if !source.matches("}")? {
                        sub.push(Node::Literal('{' as u32));
                        source.seek(here)?;
                        continue;
                    }
                    let mut min = 0u64;
                    let mut max = MAXREPEAT;
                    if !lo.is_empty() {
                        min = lo.parse::<u128>().map(|v| v.min(u64::MAX as u128) as u64).unwrap_or(u64::MAX);
                        if min >= MAXREPEAT {
                            return Err(PyErr::new("OverflowError", "the repetition number is too large"));
                        }
                    }
                    if !hi.is_empty() {
                        max = hi.parse::<u128>().map(|v| v.min(u64::MAX as u128) as u64).unwrap_or(u64::MAX);
                        if max >= MAXREPEAT {
                            return Err(PyErr::new("OverflowError", "the repetition number is too large"));
                        }
                        if max < min {
                            return Err(source.error("min repeat greater than max repeat", source.tell() - here));
                        }
                    }
                    (min, max)
                }
            };
            let this_len = this.chars().count();
            let item = match sub.last() {
                None => return Err(source.error("nothing to repeat", source.tell() - here + this_len)),
                Some(n) => n.clone(),
            };
            if matches!(item, Node::At(_)) {
                return Err(source.error("nothing to repeat", source.tell() - here + this_len));
            }
            if matches!(item, Node::Repeat { .. }) {
                return Err(source.error("multiple repeat", source.tell() - here + this_len));
            }
            let body = match item {
                Node::Subpattern { group: None, add: 0, del: 0, body } => body,
                other => vec![other],
            };
            let kind = if source.matches("?")? {
                RepKind::Min
            } else if source.matches("+")? {
                RepKind::Possessive
            } else {
                RepKind::Max
            };
            *sub.last_mut().unwrap() = Node::Repeat { kind, min, max, body };
        } else if this == "." {
            sub.push(Node::Any);
        } else if this == "(" {
            let start = source.tell() - 1;
            let mut capture = true;
            let mut atomic = false;
            let mut name: Option<String> = None;
            let mut add = 0u32;
            let mut del = 0u32;
            if source.matches("?")? {
                let ch = match source.get()? {
                    None => return Err(source.error("unexpected end of pattern", 0)),
                    Some(c) => c,
                };
                if ch == "P" {
                    if source.matches("<")? {
                        let n = source.getuntil('>', "group name")?;
                        source.checkgroupname(&n, 1)?;
                        name = Some(n);
                    } else if source.matches("=")? {
                        let n = source.getuntil(')', "group name")?;
                        source.checkgroupname(&n, 1)?;
                        let gid = match state.groupdict.get(&n) {
                            None => {
                                return Err(source.error(
                                    &format!("unknown group name {}", super::text::repr(&n)),
                                    n.chars().count() + 1,
                                ))
                            }
                            Some(g) => *g,
                        };
                        if !state.checkgroup(gid) {
                            return Err(source.error("cannot refer to an open group", n.chars().count() + 1));
                        }
                        state.checklookbehindgroup(gid, source)?;
                        sub.push(Node::GroupRef(gid));
                        continue;
                    } else {
                        let c = match source.get()? {
                            None => return Err(source.error("unexpected end of pattern", 0)),
                            Some(c) => c,
                        };
                        return Err(source.error(&format!("unknown extension ?P{c}"), c.chars().count() + 2));
                    }
                } else if ch == ":" {
                    capture = false;
                } else if ch == "#" {
                    loop {
                        if source.next.is_none() {
                            return Err(source.error("missing ), unterminated comment", source.tell() - start));
                        }
                        if source.get()?.as_deref() == Some(")") {
                            break;
                        }
                    }
                    continue;
                } else if ch == "=" || ch == "!" || ch == "<" {
                    let mut ch = ch;
                    let mut behind = false;
                    let mut saved_lb = None;
                    let mut set_lb = false;
                    if ch == "<" {
                        ch = match source.get()? {
                            None => return Err(source.error("unexpected end of pattern", 0)),
                            Some(c) => c,
                        };
                        if ch != "=" && ch != "!" {
                            return Err(source.error(&format!("unknown extension ?<{ch}"), ch.chars().count() + 2));
                        }
                        behind = true;
                        saved_lb = state.lookbehindgroups;
                        if saved_lb.is_none() {
                            state.lookbehindgroups = Some(state.groups());
                            set_lb = true;
                        }
                    }
                    let p = parse_sub(source, state, verbose, nested + 1)?;
                    if behind && set_lb {
                        state.lookbehindgroups = None;
                    }
                    let _ = saved_lb;
                    if !source.matches(")")? {
                        return Err(source.error("missing ), unterminated subpattern", source.tell() - start));
                    }
                    sub.push(Node::Assert { behind, neg: ch == "!", body: p });
                    continue;
                } else if ch == "(" {
                    let condname = source.getuntil(')', "group name")?;
                    let condgroup;
                    if !(condname.chars().all(|c| c.is_ascii_digit()) && !condname.is_empty()) {
                        source.checkgroupname(&condname, 1)?;
                        condgroup = match state.groupdict.get(&condname) {
                            None => {
                                return Err(source.error(
                                    &format!("unknown group name {}", super::text::repr(&condname)),
                                    condname.chars().count() + 1,
                                ))
                            }
                            Some(g) => *g,
                        };
                    } else {
                        condgroup = condname.parse::<usize>().unwrap_or(usize::MAX);
                        if condgroup == 0 {
                            return Err(source.error("bad group number", condname.chars().count() + 1));
                        }
                        if condgroup >= MAXGROUPS {
                            return Err(source.error(
                                &format!("invalid group reference {condgroup}"),
                                condname.chars().count() + 1,
                            ));
                        }
                        if !state.grouprefpos.iter().any(|(g, _)| *g == condgroup) {
                            state.grouprefpos.push((condgroup, source.tell() - condname.chars().count() - 1));
                        }
                    }
                    state.checklookbehindgroup(condgroup, source)?;
                    let yes = parse_seq(source, state, verbose, nested + 1, false)?;
                    let no = if source.matches("|")? {
                        let no = parse_seq(source, state, verbose, nested + 1, false)?;
                        if source.next_is("|") {
                            return Err(source.error("conditional backref with more than two branches", 0));
                        }
                        Some(no)
                    } else {
                        None
                    };
                    if !source.matches(")")? {
                        return Err(source.error("missing ), unterminated subpattern", source.tell() - start));
                    }
                    sub.push(Node::GroupRefExists { group: condgroup, yes, no });
                    continue;
                } else if ch == ">" {
                    capture = false;
                    atomic = true;
                } else if "iLmsxatu-".contains(ch.as_str()) && ch.chars().count() == 1 {
                    match parse_flags(source, state, &ch)? {
                        None => {
                            if !first || !sub.is_empty() {
                                return Err(source.error(
                                    "global flags not at the start of the expression",
                                    source.tell() - start,
                                ));
                            }
                            verbose = state.flags & VERBOSE != 0;
                            continue;
                        }
                        Some((a, d)) => {
                            add = a;
                            del = d;
                            capture = false;
                        }
                    }
                } else {
                    return Err(source.error(&format!("unknown extension ?{ch}"), ch.chars().count() + 1));
                }
            }
            let group = if capture {
                let gid = state.groups();
                state.groupwidths.push(None);
                if state.groups() > MAXGROUPS {
                    return Err(source.error("too many groups", name.as_ref().map(|n| n.chars().count() + 1).unwrap_or(0)));
                }
                if let Some(n) = &name {
                    if let Some(ogid) = state.groupdict.get(n) {
                        return Err(source.error(
                            &format!(
                                "redefinition of group name {} as group {gid}; was group {ogid}",
                                super::text::repr(n)
                            ),
                            n.chars().count() + 1,
                        ));
                    }
                    state.groupdict.insert(n.clone(), gid);
                }
                Some(gid)
            } else {
                None
            };
            let sub_verbose = (verbose || add & VERBOSE != 0) && del & VERBOSE == 0;
            let p = parse_sub(source, state, sub_verbose, nested + 1)?;
            if !source.matches(")")? {
                return Err(source.error("missing ), unterminated subpattern", source.tell() - start));
            }
            if let Some(g) = group {
                let w = getwidth(&p, &state.groupwidths);
                state.groupwidths[g] = Some(w);
            }
            if atomic {
                sub.push(Node::Atomic(p));
            } else {
                sub.push(Node::Subpattern { group, add, del, body: p });
            }
        } else if this == "^" {
            sub.push(Node::At(At::Beginning));
        } else if this == "$" {
            sub.push(Node::At(At::End));
        }
    }
    // Unpack non-capturing groups with no flags.
    let mut i = sub.len();
    while i > 0 {
        i -= 1;
        if let Node::Subpattern { group: None, add: 0, del: 0, .. } = &sub[i] {
            if let Node::Subpattern { body, .. } = sub.remove(i) {
                for (k, n) in body.into_iter().enumerate() {
                    sub.insert(i + k, n);
                }
            }
        }
    }
    Ok(sub)
}

/// `re._parser.parse(pattern, flags)`.
pub fn parse(pattern: &str, flags: u32) -> PyResult<Parsed> {
    let chars: Vec<char> = pattern.chars().collect();
    let mut source = Tokenizer::new(&chars)?;
    let mut state = State {
        flags,
        groupdict: HashMap::new(),
        groupwidths: vec![None],
        lookbehindgroups: None,
        grouprefpos: Vec::new(),
        warned: false,
    };
    let nodes = parse_sub(&mut source, &mut state, flags & VERBOSE != 0, 0)?;
    // fix_flags for a str pattern.
    if state.flags & LOCALE != 0 {
        return Err(PyErr::value("cannot use LOCALE flag with a str pattern"));
    }
    if state.flags & ASCII == 0 {
        state.flags |= UNICODE;
    } else if state.flags & UNICODE != 0 {
        return Err(PyErr::value("ASCII and UNICODE flags are incompatible"));
    }
    if source.next.is_some() {
        return Err(source.error("unbalanced parenthesis", 0));
    }
    for (g, pos) in &state.grouprefpos {
        if *g >= state.groups() {
            return Err(re_error(&format!("invalid group reference {g}"), &chars, Some(*pos)));
        }
    }
    Ok(Parsed { nodes, flags: state.flags, groups: state.groups(), warned: state.warned })
}

/// The checks `re._compiler.compile` makes beyond the parser.
fn compile_checks(nodes: &[Node], groups: &[Option<(u128, u128)>]) -> PyResult<()> {
    for n in nodes {
        match n {
            Node::Assert { behind, body, .. } => {
                if *behind {
                    let (lo, hi) = getwidth(body, groups);
                    if lo > MAXCODE {
                        return Err(PyErr::new("error", "looks too much behind"));
                    }
                    if lo != hi {
                        return Err(PyErr::new("error", "look-behind requires fixed-width pattern"));
                    }
                }
                compile_checks(body, groups)?;
            }
            Node::Branch(alts) => {
                for a in alts {
                    compile_checks(a, groups)?;
                }
            }
            Node::Repeat { body, .. } | Node::Atomic(body) | Node::Subpattern { body, .. } => {
                compile_checks(body, groups)?
            }
            Node::GroupRefExists { yes, no, .. } => {
                compile_checks(yes, groups)?;
                if let Some(no) = no {
                    compile_checks(no, groups)?;
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// A compiled pattern: its tree and flags.
#[derive(Debug)]
pub struct Regex {
    parsed: Parsed,
    prog: OnceLock<PyResult<std::sync::Arc<Program>>>,
}

/// `re.compile(pattern)`, with its errors.
pub fn compile(pattern: &str) -> PyResult<Regex> {
    let parsed = parse(pattern, 0)?;
    // Group widths for the compile-time lookbehind check.
    let mut widths: Vec<Option<(u128, u128)>> = vec![None; parsed.groups];
    fill_widths(&parsed.nodes, &mut widths);
    compile_checks(&parsed.nodes, &widths)?;
    Ok(Regex { parsed, prog: OnceLock::new() })
}

fn fill_widths(nodes: &[Node], widths: &mut Vec<Option<(u128, u128)>>) {
    for n in nodes {
        match n {
            Node::Subpattern { group, body, .. } => {
                fill_widths(body, widths);
                if let Some(g) = group {
                    let w = getwidth(body, widths);
                    if *g < widths.len() {
                        widths[*g] = Some(w);
                    }
                }
            }
            Node::Branch(alts) => {
                for a in alts {
                    fill_widths(a, widths);
                }
            }
            Node::Repeat { body, .. } | Node::Atomic(body) | Node::Assert { body, .. } => fill_widths(body, widths),
            Node::GroupRefExists { yes, no, .. } => {
                fill_widths(yes, widths);
                if let Some(no) = no {
                    fill_widths(no, widths);
                }
            }
            _ => {}
        }
    }
}

/// `re.search(pattern, text) is not None`.
pub fn search(pattern: &str, text: &str) -> PyResult<bool> {
    cached(pattern)?.search(text)
}

/// `re.search(pattern, text) is not None` for a pattern from outside the
/// gate: not kept in the process cache. A search past the step budget is
/// not decided.
pub fn search_bounded(pattern: &str, text: &str) -> PyResult<bool> {
    compile(pattern)?.search(text)
}

fn cached(pattern: &str) -> PyResult<std::sync::Arc<Regex>> {
    static CACHE: OnceLock<std::sync::Mutex<HashMap<String, std::sync::Arc<Regex>>>> = OnceLock::new();
    let cache = CACHE.get_or_init(Default::default);
    if let Some(r) = cache.lock().ok().and_then(|c| c.get(pattern).cloned()) {
        return Ok(r);
    }
    let r = std::sync::Arc::new(compile(pattern)?);
    if let Ok(mut c) = cache.lock() {
        c.insert(pattern.to_string(), r.clone());
    }
    Ok(r)
}

// ---------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------

fn sre_lower(cp: u32) -> u32 {
    if cp < 128 {
        return (cp as u8).to_ascii_lowercase() as u32;
    }
    match tables::SRE_LOWER.binary_search_by_key(&cp, |&(a, _)| a) {
        Ok(i) => tables::SRE_LOWER[i].1,
        Err(_) => cp,
    }
}

fn sre_cased(cp: u32) -> bool {
    if cp < 128 {
        return (cp as u8).is_ascii_alphabetic();
    }
    tables::SRE_CASED
        .binary_search_by(|&(lo, hi)| {
            if hi < cp {
                std::cmp::Ordering::Less
            } else if lo > cp {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .is_ok()
}

fn extra_cases(lo: u32) -> &'static [u32] {
    match tables::SRE_EXTRA_CASES.binary_search_by_key(&lo, |&(k, _)| k) {
        Ok(i) => tables::SRE_EXTRA_CASES[i].1,
        Err(_) => &[],
    }
}

/// Every code point whose re-lowercase is `lo`.
fn lower_preimage(lo: u32) -> Vec<u32> {
    static INV: OnceLock<HashMap<u32, Vec<u32>>> = OnceLock::new();
    let inv = INV.get_or_init(|| {
        let mut m: HashMap<u32, Vec<u32>> = HashMap::new();
        for &(a, b) in tables::SRE_LOWER {
            m.entry(b).or_default().push(a);
        }
        for a in b'A'..=b'Z' {
            m.entry(a.to_ascii_lowercase() as u32).or_default().push(a as u32);
        }
        m
    });
    let mut out = vec![lo];
    if let Some(v) = inv.get(&lo) {
        out.extend(v.iter().copied());
    }
    out
}

fn is_word_cp(cp: u32, ascii: bool) -> bool {
    if ascii || cp < 128 {
        return cp < 128 && ((cp as u8).is_ascii_alphanumeric() || cp == '_' as u32);
    }
    match char::from_u32(cp) {
        Some(c) => c == '_' || is_alnum(c),
        None => false,
    }
}

fn category_match(cat: Cat, cp: u32, ascii: bool) -> bool {
    let c = char::from_u32(cp);
    match cat {
        Cat::Digit => {
            if ascii {
                cp < 128 && (cp as u8).is_ascii_digit()
            } else {
                c.map(is_decimal).unwrap_or(false)
            }
        }
        Cat::NotDigit => !category_match(Cat::Digit, cp, ascii),
        Cat::Space => {
            if ascii {
                matches!(cp, 9..=13 | 32)
            } else {
                c.map(is_space).unwrap_or(false)
            }
        }
        Cat::NotSpace => !category_match(Cat::Space, cp, ascii),
        Cat::Word => is_word_cp(cp, ascii),
        Cat::NotWord => !is_word_cp(cp, ascii),
    }
}

fn set_contains(items: &[SetItem], cp: u32, ascii: bool) -> bool {
    let mut neg = false;
    let mut hit = false;
    for it in items {
        match it {
            SetItem::Negate => neg = true,
            SetItem::Literal(c) => hit |= *c == cp,
            SetItem::Range(lo, hi) => hit |= *lo <= cp && cp <= *hi,
            SetItem::Category(cat) => hit |= category_match(*cat, cp, ascii),
        }
    }
    hit != neg
}

/// Whether a set has a cased member, as `_optimize_charset` decides it.
fn set_hascased(items: &[SetItem], ascii: bool) -> bool {
    let cased = |x: u32| if ascii { x < 128 && (x as u8).is_ascii_alphabetic() } else { sre_cased(x) };
    let lower = |x: u32| if ascii { if x < 128 { (x as u8).to_ascii_lowercase() as u32 } else { x } } else { sre_lower(x) };
    items.iter().any(|it| match it {
        SetItem::Literal(c) => cased(lower(*c)),
        SetItem::Range(lo, hi) => {
            if ascii {
                (*lo..=(*hi).min(127)).any(cased)
            } else {
                tables::SRE_CASED.iter().any(|&(a, b)| a <= *hi && *lo <= b)
            }
        }
        _ => false,
    })
}

/// Membership under IGNORECASE, as the compiled `IN_UNI_IGNORE` (or the
/// ASCII `IN_IGNORE`) decides it for a set with a cased member.
fn set_contains_ignore(items: &[SetItem], cp: u32, ascii: bool) -> bool {
    let lower = |x: u32| if ascii { if x < 128 { (x as u8).to_ascii_lowercase() as u32 } else { x } } else { sre_lower(x) };
    let l = lower(cp);
    let mut keys = vec![l];
    if !ascii {
        keys.extend(extra_cases(l).iter().copied());
    }
    let mut neg = false;
    let mut hit = false;
    for it in items {
        match it {
            SetItem::Negate => neg = true,
            SetItem::Literal(c) => hit |= keys.contains(&lower(*c)),
            SetItem::Range(lo, hi) => {
                hit |= keys.iter().any(|&k| {
                    if ascii {
                        (*lo..=*hi).contains(&k)
                            || (k < 128 && (*lo..=*hi).contains(&((k as u8).to_ascii_uppercase() as u32)))
                    } else {
                        lower_preimage(k).iter().any(|p| *lo <= *p && *p <= *hi)
                    }
                })
            }
            SetItem::Category(cat) => hit |= category_match(*cat, l, ascii),
        }
    }
    hit != neg
}

/// The flags in force at a node.
#[derive(Clone, Copy, Debug)]
struct Fl(u32);
impl Fl {
    fn ignore(self) -> bool {
        self.0 & IGNORECASE != 0
    }
    fn ascii(self) -> bool {
        self.0 & ASCII != 0
    }
    fn multiline(self) -> bool {
        self.0 & MULTILINE != 0
    }
    fn dotall(self) -> bool {
        self.0 & DOTALL != 0
    }
    fn combine(self, add: u32, del: u32) -> Fl {
        let mut f = self.0;
        if add & TYPE_FLAGS != 0 {
            f &= !TYPE_FLAGS;
        }
        Fl((f | add) & !del)
    }
}

/// One instruction of the backtracking program.
#[derive(Clone, Debug)]
enum Inst {
    Char { cp: u32, neg: bool, ignore: bool, ascii: bool },
    Set { set: usize, ignore: bool, ascii: bool },
    Any { dotall: bool },
    At { at: At, multiline: bool, ascii: bool },
    Split(usize, usize),
    Jmp(usize),
    Save(usize),
    /// Reset a repeat's counter and the position its last pass began at.
    RepInit(usize),
    /// The head of a repeat: loop into `body` or leave to `exit`.
    RepHead { reg: usize, min: u64, max: u64, greedy: bool, body: usize, exit: usize },
    /// The end of one pass of a repeat's body.
    RepTail { reg: usize, head: usize },
    GroupRef { group: usize, ignore: bool, ascii: bool },
    /// Run a sub-program once at this position, with no backtracking into
    /// it: an atomic group, a possessive repeat, or a lookaround.
    Sub { prog: usize, kind: SubKind },
    IfGroup { group: usize, no: usize },
    Match,
}

#[derive(Clone, Copy, Debug)]
enum SubKind {
    Atomic,
    Ahead { neg: bool },
    Behind { neg: bool, width: usize },
}

#[derive(Debug)]
struct Program {
    code: Vec<Vec<Inst>>,
    sets: Vec<Vec<SetItem>>,
    regs: usize,
    groups: usize,
}

struct Compiler {
    progs: Vec<Vec<Inst>>,
    sets: Vec<Vec<SetItem>>,
    regs: usize,
}

impl Compiler {
    fn emit_seq(&mut self, p: usize, nodes: &[Node], fl: Fl) -> PyResult<()> {
        for n in nodes {
            self.emit(p, n, fl)?;
        }
        Ok(())
    }

    fn push(&mut self, p: usize, i: Inst) -> usize {
        self.progs[p].push(i);
        self.progs[p].len() - 1
    }

    fn here(&self, p: usize) -> usize {
        self.progs[p].len()
    }

    fn emit(&mut self, p: usize, n: &Node, fl: Fl) -> PyResult<()> {
        if self.progs[p].len() > 2_000_000 {
            return Err(PyErr::undecided("a regular expression too large to expand"));
        }
        match n {
            Node::Literal(c) | Node::NotLiteral(c) => {
                let neg = matches!(n, Node::NotLiteral(_));
                let ascii = fl.ascii();
                let cased = if ascii { *c < 128 && (*c as u8).is_ascii_alphabetic() } else { sre_cased(*c) };
                if fl.ignore() && cased {
                    let lo = if ascii { (*c as u8).to_ascii_lowercase() as u32 } else { sre_lower(*c) };
                    if !ascii && !extra_cases(lo).is_empty() {
                        let mut items = vec![];
                        if neg {
                            items.push(SetItem::Negate);
                        }
                        items.push(SetItem::Literal(lo));
                        for k in extra_cases(lo) {
                            items.push(SetItem::Literal(*k));
                        }
                        self.sets.push(items);
                        let set = self.sets.len() - 1;
                        self.push(p, Inst::Set { set, ignore: true, ascii });
                    } else {
                        self.push(p, Inst::Char { cp: lo, neg, ignore: true, ascii });
                    }
                } else {
                    self.push(p, Inst::Char { cp: *c, neg, ignore: false, ascii });
                }
            }
            Node::Any => {
                self.push(p, Inst::Any { dotall: fl.dotall() });
            }
            Node::In(items) => {
                self.sets.push(items.clone());
                let set = self.sets.len() - 1;
                let ignore = fl.ignore() && set_hascased(items, fl.ascii());
                self.push(p, Inst::Set { set, ignore, ascii: fl.ascii() });
            }
            Node::At(at) => {
                self.push(p, Inst::At { at: *at, multiline: fl.multiline(), ascii: fl.ascii() });
            }
            Node::Branch(alts) => {
                let mut jumps = vec![];
                for (i, a) in alts.iter().enumerate() {
                    if i + 1 < alts.len() {
                        let split = self.push(p, Inst::Split(0, 0));
                        let start = self.here(p);
                        self.emit_seq(p, a, fl)?;
                        jumps.push(self.push(p, Inst::Jmp(0)));
                        let next = self.here(p);
                        self.progs[p][split] = Inst::Split(start, next);
                    } else {
                        self.emit_seq(p, a, fl)?;
                    }
                }
                let end = self.here(p);
                for j in jumps {
                    self.progs[p][j] = Inst::Jmp(end);
                }
            }
            Node::Subpattern { group, add, del, body } => {
                let f = fl.combine(*add, *del);
                if let Some(g) = group {
                    self.push(p, Inst::Save(g * 2));
                }
                self.emit_seq(p, body, f)?;
                if let Some(g) = group {
                    self.push(p, Inst::Save(g * 2 + 1));
                }
            }
            Node::Repeat { kind, min, max, body } => {
                if *kind == RepKind::Possessive {
                    let inner = Node::Repeat { kind: RepKind::Max, min: *min, max: *max, body: body.clone() };
                    let sub = self.sub_program(&[inner], fl)?;
                    self.push(p, Inst::Sub { prog: sub, kind: SubKind::Atomic });
                    return Ok(());
                }
                let reg = self.regs;
                self.regs += 1;
                self.push(p, Inst::RepInit(reg));
                let head = self.push(p, Inst::Jmp(0));
                let body_start = self.here(p);
                self.emit_seq(p, body, fl)?;
                self.push(p, Inst::RepTail { reg, head });
                let exit = self.here(p);
                self.progs[p][head] =
                    Inst::RepHead { reg, min: *min, max: *max, greedy: *kind == RepKind::Max, body: body_start, exit };
            }
            Node::Atomic(body) => {
                let sub = self.sub_program(body, fl)?;
                self.push(p, Inst::Sub { prog: sub, kind: SubKind::Atomic });
            }
            Node::Assert { behind, neg, body } => {
                let sub = self.sub_program(body, fl)?;
                let kind = if *behind {
                    let (lo, _) = getwidth(body, &[]);
                    SubKind::Behind { neg: *neg, width: lo as usize }
                } else {
                    SubKind::Ahead { neg: *neg }
                };
                self.push(p, Inst::Sub { prog: sub, kind });
            }
            Node::GroupRef(g) => {
                self.push(p, Inst::GroupRef { group: *g, ignore: fl.ignore(), ascii: fl.ascii() });
            }
            Node::GroupRefExists { group, yes, no } => {
                let test = self.push(p, Inst::IfGroup { group: *group, no: 0 });
                self.emit_seq(p, yes, fl)?;
                let jump = self.push(p, Inst::Jmp(0));
                let no_start = self.here(p);
                if let Some(no) = no {
                    self.emit_seq(p, no, fl)?;
                }
                let end = self.here(p);
                self.progs[p][test] = Inst::IfGroup { group: *group, no: no_start };
                self.progs[p][jump] = Inst::Jmp(end);
            }
        }
        Ok(())
    }

    fn sub_program(&mut self, nodes: &[Node], fl: Fl) -> PyResult<usize> {
        self.progs.push(Vec::new());
        let id = self.progs.len() - 1;
        self.emit_seq(id, nodes, fl)?;
        self.push(id, Inst::Match);
        Ok(id)
    }
}

impl Regex {
    fn program(&self) -> PyResult<std::sync::Arc<Program>> {
        self.prog
            .get_or_init(|| {
                let mut c = Compiler { progs: vec![Vec::new()], sets: Vec::new(), regs: 0 };
                c.emit_seq(0, &self.parsed.nodes, Fl(self.parsed.flags))?;
                c.push(0, Inst::Match);
                Ok(std::sync::Arc::new(Program { code: c.progs, sets: c.sets, regs: c.regs, groups: self.parsed.groups }))
            })
            .clone()
    }

    /// `re.search(self, text) is not None`, or the error a pattern too
    /// large to expand raises.
    pub fn search(&self, text: &str) -> PyResult<bool> {
        let prog = self.program()?;
        let s: Vec<u32> = text.chars().map(pycp).collect();
        let first = match prog.code[0].first() {
            Some(Inst::Char { cp, neg: false, ignore: false, .. }) => Some(*cp),
            _ => None,
        };
        let mut m = Matcher::new(&prog, &s);
        for start in 0..=s.len() {
            if let Some(c) = first {
                if start >= s.len() || s[start] != c {
                    continue;
                }
            }
            if m.run(0, start, None).is_some() && !m.blown {
                return Ok(true);
            }
            if m.blown {
                return Err(PyErr::undecided("a regular expression search that backtracks too long"));
            }
        }
        Ok(false)
    }

    /// `self.match(text) is not None`: a match that starts at 0.
    pub fn match_start(&self, text: &str) -> PyResult<bool> {
        let prog = self.program()?;
        let s: Vec<u32> = text.chars().map(pycp).collect();
        let mut m = Matcher::new(&prog, &s);
        let hit = m.run(0, 0, None).is_some();
        if m.blown {
            return Err(PyErr::undecided("a regular expression match that backtracks too long"));
        }
        Ok(hit)
    }

    /// `self.search(text, pos)` on the code points `s`: the first match
    /// that starts at `pos` or later. With `nonempty_at`, an empty match
    /// starting at that position does not count (re.sub's rule after an
    /// empty match).
    pub fn search_at(&self, s: &[u32], pos: usize, nonempty_at: Option<usize>) -> PyResult<Option<Match>> {
        let prog = self.program()?;
        let mut m = Matcher::new(&prog, s);
        for start in pos..=s.len() {
            m.caps.iter_mut().for_each(|c| *c = None);
            m.forbid_end = if nonempty_at == Some(start) { Some(start) } else { None };
            let found = m.run(0, start, None);
            if m.blown {
                return Err(PyErr::undecided("a regular expression search that backtracks too long"));
            }
            if let Some(end) = found {
                let groups = (1..prog.groups)
                    .map(|g| match (m.caps.get(g * 2).copied().flatten(), m.caps.get(g * 2 + 1).copied().flatten()) {
                        (Some(a), Some(b)) if a <= b => Some((a, b)),
                        _ => None,
                    })
                    .collect();
                return Ok(Some(Match { start, end, groups }));
            }
        }
        Ok(None)
    }

    /// `re.sub(self, f, text)`: each match replaced by what `f` returns.
    pub fn sub(&self, text: &str, mut f: impl FnMut(&Match, &[u32]) -> String) -> PyResult<String> {
        let s: Vec<u32> = text.chars().map(pycp).collect();
        let chars: Vec<char> = text.chars().collect();
        let mut out = String::new();
        let mut pos = 0;
        let mut last = 0;
        let mut after_empty: Option<usize> = None;
        while pos <= s.len() {
            let m = match self.search_at(&s, pos, after_empty)? {
                None => break,
                Some(m) => m,
            };
            out.extend(&chars[last..m.start]);
            out.push_str(&f(&m, &s));
            last = m.end;
            after_empty = if m.end == m.start { Some(m.end) } else { None };
            pos = m.end;
        }
        out.extend(&chars[last..]);
        Ok(out)
    }

    pub fn parsed(&self) -> &Parsed {
        &self.parsed
    }
}

/// One match: code point positions of the whole match and of each group
/// (group 1 first).
#[derive(Debug, Clone)]
pub struct Match {
    pub start: usize,
    pub end: usize,
    pub groups: Vec<Option<(usize, usize)>>,
}

impl Match {
    /// `m.group(g)` as text, or None when the group did not take part.
    pub fn group(&self, s: &[u32], g: usize) -> Option<String> {
        let (a, b) = if g == 0 { (self.start, self.end) } else { (*self.groups.get(g - 1)?)? };
        Some(s[a..b].iter().map(|&c| super::text::cp_char(c)).collect())
    }
}

/// `re.compile(pattern)` kept for the life of the process.
pub fn cached_compile(pattern: &str) -> PyResult<std::sync::Arc<Regex>> {
    cached(pattern)
}

struct Matcher<'a> {
    prog: &'a Program,
    s: &'a [u32],
    caps: Vec<Option<usize>>,
    regs: Vec<(u64, usize)>,
    /// A whole-pattern match may not end here (re.sub's rule after an
    /// empty match: the next match at that position must advance).
    forbid_end: Option<usize>,
    /// Steps taken, and whether the budget ran out.
    steps: u64,
    blown: bool,
}

/// Matcher steps past which a search is not decided: Python's own
/// matcher could run past the gate's time budget on such a pattern.
pub const STEP_BUDGET: u64 = 20_000_000;

enum Trail {
    Cap(usize, Option<usize>),
    Reg(usize, (u64, usize)),
}

enum Frame {
    Branch { pc: usize, pos: usize, trail: usize },
}

impl<'a> Matcher<'a> {
    fn new(prog: &'a Program, s: &'a [u32]) -> Self {
        Matcher {
            prog,
            s,
            caps: vec![None; prog.groups * 2],
            regs: vec![(0, usize::MAX); prog.regs],
            forbid_end: None,
            steps: 0,
            blown: false,
        }
    }

    fn undo(&mut self, trail: &mut Vec<Trail>, to: usize) {
        while trail.len() > to {
            match trail.pop().unwrap() {
                Trail::Cap(i, v) => self.caps[i] = v,
                Trail::Reg(i, v) => self.regs[i] = v,
            }
        }
    }

    fn word_at(&self, pos: usize, ascii: bool) -> bool {
        pos < self.s.len() && is_word_cp(self.s[pos], ascii)
    }

    /// Runs program `p` from `pos`. On success returns the end position,
    /// with captures set. `end_at` requires the match to end there.
    fn run(&mut self, p: usize, pos: usize, end_at: Option<usize>) -> Option<usize> {
        let code = &self.prog.code[p];
        let mut stack: Vec<Frame> = Vec::new();
        let mut trail: Vec<Trail> = Vec::new();
        let mut pc = 0usize;
        let mut pos = pos;
        let n = self.s.len();
        loop {
            self.steps += 1;
            if self.steps > STEP_BUDGET {
                self.blown = true;
                return None;
            }
            let ok = match &code[pc] {
                Inst::Match => {
                    let forbidden = p == 0 && self.forbid_end == Some(pos);
                    if end_at.map(|e| e == pos).unwrap_or(true) && !forbidden {
                        return Some(pos);
                    }
                    false
                }
                Inst::Char { cp, neg, ignore, ascii } => {
                    if pos < n {
                        let c = self.s[pos];
                        let eq = if *ignore {
                            let l = if *ascii { if c < 128 { (c as u8).to_ascii_lowercase() as u32 } else { c } } else { sre_lower(c) };
                            l == *cp
                        } else {
                            c == *cp
                        };
                        if eq != *neg {
                            pos += 1;
                            pc += 1;
                            true
                        } else {
                            false
                        }
                    } else {
                        false
                    }
                }
                Inst::Set { set, ignore, ascii } => {
                    if pos < n {
                        let items = &self.prog.sets[*set];
                        let c = self.s[pos];
                        let hit = if *ignore { set_contains_ignore(items, c, *ascii) } else { set_contains(items, c, *ascii) };
                        if hit {
                            pos += 1;
                            pc += 1;
                            true
                        } else {
                            false
                        }
                    } else {
                        false
                    }
                }
                Inst::Any { dotall } => {
                    if pos < n && (*dotall || self.s[pos] != 10) {
                        pos += 1;
                        pc += 1;
                        true
                    } else {
                        false
                    }
                }
                Inst::At { at, multiline, ascii } => {
                    let ok = match at {
                        At::Beginning => pos == 0 || (*multiline && self.s[pos - 1] == 10),
                        At::BeginningString => pos == 0,
                        At::End => {
                            if *multiline {
                                pos == n || self.s[pos] == 10
                            } else {
                                pos == n || (pos + 1 == n && self.s[pos] == 10)
                            }
                        }
                        At::EndString => pos == n,
                        At::Boundary | At::NonBoundary => {
                            let before = pos > 0 && self.word_at(pos - 1, *ascii);
                            let after = self.word_at(pos, *ascii);
                            let b = if n == 0 { false } else { before != after };
                            if *at == At::Boundary {
                                b
                            } else {
                                n != 0 && !b
                            }
                        }
                    };
                    if ok {
                        pc += 1;
                    }
                    ok
                }
                Inst::Split(a, b) => {
                    stack.push(Frame::Branch { pc: *b, pos, trail: trail.len() });
                    pc = *a;
                    true
                }
                Inst::Jmp(t) => {
                    pc = *t;
                    true
                }
                Inst::Save(i) => {
                    trail.push(Trail::Cap(*i, self.caps[*i]));
                    self.caps[*i] = Some(pos);
                    pc += 1;
                    true
                }
                Inst::RepInit(r) => {
                    trail.push(Trail::Reg(*r, self.regs[*r]));
                    self.regs[*r] = (0, usize::MAX);
                    pc += 1;
                    true
                }
                Inst::RepHead { reg, min, max, greedy, body, exit } => {
                    let (count, last) = self.regs[*reg];
                    // A pass that matched nothing ends the loop once the
                    // minimum is met, as _sre does.
                    if count >= *min && last == pos && count > 0 {
                        pc = *exit;
                        true
                    } else if count < *min {
                        trail.push(Trail::Reg(*reg, self.regs[*reg]));
                        self.regs[*reg] = (count, pos);
                        pc = *body;
                        true
                    } else if count >= *max {
                        pc = *exit;
                        true
                    } else if *greedy {
                        stack.push(Frame::Branch { pc: *exit, pos, trail: trail.len() });
                        trail.push(Trail::Reg(*reg, self.regs[*reg]));
                        self.regs[*reg] = (count, pos);
                        pc = *body;
                        true
                    } else {
                        // Lazy: leave first; on backtrack take one more pass.
                        stack.push(Frame::Branch { pc: usize::MAX - *reg, pos, trail: trail.len() });
                        pc = *exit;
                        true
                    }
                }
                Inst::RepTail { reg, head } => {
                    let (count, last) = self.regs[*reg];
                    trail.push(Trail::Reg(*reg, self.regs[*reg]));
                    self.regs[*reg] = (count + 1, last);
                    pc = *head;
                    true
                }
                Inst::GroupRef { group, ignore, ascii } => {
                    match (self.caps.get(group * 2).copied().flatten(), self.caps.get(group * 2 + 1).copied().flatten()) {
                        (Some(a), Some(b)) if a <= b => {
                            let len = b - a;
                            if pos + len <= n && (0..len).all(|k| {
                                let x = self.s[a + k];
                                let y = self.s[pos + k];
                                if *ignore {
                                    if *ascii {
                                        x == y || (x < 128 && y < 128 && (x as u8).eq_ignore_ascii_case(&(y as u8)))
                                    } else {
                                        sre_lower(x) == sre_lower(y)
                                    }
                                } else {
                                    x == y
                                }
                            }) {
                                pos += len;
                                pc += 1;
                                true
                            } else {
                                false
                            }
                        }
                        _ => false,
                    }
                }
                Inst::IfGroup { group, no } => {
                    let set = matches!(
                        (self.caps.get(group * 2).copied().flatten(), self.caps.get(group * 2 + 1).copied().flatten()),
                        (Some(_), Some(_))
                    );
                    pc = if set { pc + 1 } else { *no };
                    true
                }
                Inst::Sub { prog, kind } => {
                    let (prog, kind) = (*prog, *kind);
                    let saved = self.caps.clone();
                    match kind {
                        SubKind::Atomic => match self.run(prog, pos, None) {
                            Some(end) => {
                                for (i, v) in saved.iter().enumerate() {
                                    if self.caps[i] != *v {
                                        trail.push(Trail::Cap(i, *v));
                                    }
                                }
                                pos = end;
                                pc += 1;
                                true
                            }
                            None => {
                                self.caps = saved;
                                false
                            }
                        },
                        SubKind::Ahead { neg } => {
                            let hit = self.run(prog, pos, None).is_some();
                            if neg || !hit {
                                self.caps = saved;
                            } else {
                                for (i, v) in saved.iter().enumerate() {
                                    if self.caps[i] != *v {
                                        trail.push(Trail::Cap(i, *v));
                                    }
                                }
                            }
                            if hit != neg {
                                pc += 1;
                                true
                            } else {
                                false
                            }
                        }
                        SubKind::Behind { neg, width } => {
                            let hit = pos >= width && self.run(prog, pos - width, Some(pos)).is_some();
                            if neg || !hit {
                                self.caps = saved;
                            } else {
                                for (i, v) in saved.iter().enumerate() {
                                    if self.caps[i] != *v {
                                        trail.push(Trail::Cap(i, *v));
                                    }
                                }
                            }
                            if hit != neg {
                                pc += 1;
                                true
                            } else {
                                false
                            }
                        }
                    }
                }
            };
            if ok {
                continue;
            }
            // Backtrack.
            loop {
                match stack.pop() {
                    None => {
                        self.undo(&mut trail, 0);
                        return None;
                    }
                    Some(Frame::Branch { pc: bpc, pos: bpos, trail: t }) => {
                        self.undo(&mut trail, t);
                        pos = bpos;
                        if bpc > usize::MAX / 2 {
                            // A lazy repeat taking one more pass.
                            let reg = usize::MAX - bpc;
                            let head = code.iter().position(|i| matches!(i, Inst::RepHead { reg: r, .. } if *r == reg));
                            if let Some(Inst::RepHead { body, .. }) = head.map(|h| &code[h]) {
                                let (count, _) = self.regs[reg];
                                trail.push(Trail::Reg(reg, self.regs[reg]));
                                self.regs[reg] = (count, pos);
                                pc = *body;
                                break;
                            }
                            continue;
                        }
                        pc = bpc;
                        break;
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn search_follows_python() {
        let cases: &[(&str, &str, bool)] = &[
            (r"\bcoppice\b[^\n;&|]*?\bagent\b[\s'\x22]+(allow)\b", "coppice --x agent 'allow'", true),
            (r"\bcoppice\b", "coppiced", false),
            (r"a$", "a\n", true),
            (r"a\Z", "a\n", false),
            (r"(?i)straße", "STRASSE", false),
            (r"(?i)k", "\u{212a}", true),
            (r"^\d+$", "٣٤", true),
            (r"(?a)^\d+$", "٣٤", false),
            (r"(a)(?(1)b|c)", "ab", true),
            (r"(?<=ab)c", "abc", true),
            (r"(?<!ab)c", "abc", false),
            (r"(a+)+b", "aaaaaaaaaaaaaaaaaab", true),
            (r"(?:a|)*b", "b", true),
            (r"x{2,3}", "axxb", true),
            (r"x{2,3}?y", "xxxy", true),
            (r"(\w+)\s\1", "hello hello", true),
            (r"a++a", "aaa", false),
            (r"(?>a+)a", "aaa", false),
            (r"[^\w.-]\.opencode", "x .opencode", true),
            (r"é\b", "café", true),
        ];
        for (p, t, want) in cases {
            assert_eq!(search(p, t).unwrap(), *want, "search({p:?}, {t:?})");
        }
    }

    #[test]
    fn errors_are_pythons() {
        let cases: &[(&str, &str)] = &[
            ("(", "missing ), unterminated subpattern at position 0"),
            ("a**", "multiple repeat at position 2"),
            ("[a", "unterminated character set at position 0"),
            ("\\q", "bad escape \\q at position 0"),
            ("(?<=a+)b", "look-behind requires fixed-width pattern"),
            ("a)", "unbalanced parenthesis at position 1"),
            ("x{3,2}", "min repeat greater than max repeat at position 2"),
            ("(?P<1>a)", "bad character in group name '1' at position 4"),
            ("*", "nothing to repeat at position 0"),
            ("a\n(", "missing ), unterminated subpattern at position 2 (line 2, column 1)"),
        ];
        for (p, want) in cases {
            assert_eq!(compile(p).unwrap_err().msg, *want, "compile({p:?})");
        }
    }
}
