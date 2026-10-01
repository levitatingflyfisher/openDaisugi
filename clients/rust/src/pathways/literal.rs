//! `ast.literal_eval(text)` on Python 3.12, for the text
//! `decode_dict_text` hands it: dicts, lists, tuples (read as lists, with
//! `tuple` set), a str in every quote and prefix form with Python's
//! escapes and implicit concatenation, ints in every base with
//! underscores (any size), floats, True, False and None. Every text
//! literal_eval raises on reads as `Status::None`. A set, bytes, a complex
//! number, Ellipsis, a `\N{...}` escape and a dict key that is not a str
//! read as `Status::Refused`, as does any text whose tokens make Python's
//! parser print a SyntaxWarning (an invalid escape, an octal escape above
//! 0o377, a number run into a keyword), since Python then writes to
//! stderr. The Go client's `pyjson.LiteralEval` is the reference.

use crate::gate::py::text::{as_surrogate, cp_char};
use crate::gate::pyjson::{Object, Value};

/// What `ast.literal_eval` does with a text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    /// It returns `Literal::value`.
    Value,
    /// It raises one of the errors `decode_dict_text` catches.
    None,
    /// Python's answer is one this port does not model. With a value, the
    /// value lost a part (`why` says which; a top-level key whose value it
    /// lost is in `opaque_keys`); with none, Python's parser prints a
    /// SyntaxWarning on stderr while it reads the text.
    Refused,
}

#[derive(Debug, Clone)]
pub struct Literal {
    pub status: Status,
    pub value: Option<Value>,
    pub why: String,
    /// The value held a tuple, read here as a list.
    pub tuple: bool,
    /// Keys of a top-level dict whose values this port does not model.
    pub opaque_keys: Vec<String>,
}

impl Literal {
    fn none() -> Self {
        Literal { status: Status::None, value: None, why: String::new(), tuple: false, opaque_keys: vec![] }
    }
}

pub fn literal_eval(text: &str) -> Literal {
    if text.contains('\0') {
        // "source code string cannot contain null bytes".
        return Literal::none();
    }
    if text.chars().any(|c| as_surrogate(c).is_some()) {
        // A lone surrogate does not encode to UTF-8 (UnicodeEncodeError).
        return Literal::none();
    }
    // literal_eval strips spaces and tabs; the tokenizer translates \r\n
    // and \r to \n.
    let src = text.trim_start_matches([' ', '\t']).replace("\r\n", "\n").replace('\r', "\n");
    let mut lx = Lexer { s: src.as_bytes(), t: &src, i: 0, toks: vec![], stack: vec![], err: false, warn: None };
    lx.run();
    if let Some(w) = lx.warn {
        return Literal { status: Status::Refused, value: None, why: w.into(), tuple: false, opaque_keys: vec![] };
    }
    if lx.err {
        return Literal::none();
    }
    let mut p = Parser { toks: &lx.toks, i: 0 };
    let Some(n) = p.top() else {
        return Literal::none();
    };
    let mut ev = Eval { why: None, tuple: false };
    // Which top-level keys hold a value this port does not model.
    let mut opaque_keys = vec![];
    let v = if matches!(n, Node::Dict(..)) { ev.dict(&n, Some(&mut opaque_keys)) } else { ev.eval(&n) };
    let Some(v) = v else {
        return Literal::none();
    };
    match ev.why {
        Some(why) => Literal { status: Status::Refused, value: Some(v), why: why.into(), tuple: ev.tuple, opaque_keys },
        None => Literal { status: Status::Value, value: Some(v), why: String::new(), tuple: ev.tuple, opaque_keys },
    }
}

// --- tokens ---

#[derive(Debug, Clone, PartialEq)]
enum Tok {
    Num(String),
    /// A string token: its body, its prefix (lower-cased) and whether its
    /// quote is tripled.
    Str(String, String),
    Name(String),
    Op(String),
    Newline,
    End,
}

const MAX_LEVEL: usize = 200;

struct Lexer<'a> {
    s: &'a [u8],
    t: &'a str,
    i: usize,
    toks: Vec<Tok>,
    stack: Vec<u8>,
    /// The tokenizer raised: no token after this.
    err: bool,
    /// A SyntaxWarning Python prints.
    warn: Option<&'static str>,
}

fn is_ident_start(c: u8) -> bool {
    c.is_ascii_alphabetic() || c == b'_' || c >= 0x80
}

fn is_ident_char(c: u8) -> bool {
    is_ident_start(c) || c.is_ascii_digit()
}

impl Lexer<'_> {
    fn at(&self, k: usize) -> u8 {
        self.s.get(self.i + k).copied().unwrap_or(0)
    }

    /// Tokenizes the whole text, as Python does while it parses it and,
    /// after a syntax error, while it checks the rest of the source. It
    /// stops at the first error the tokenizer raises. A string's escapes
    /// are checked here too: Python warns on an invalid one when it reads
    /// the string.
    fn run(&mut self) {
        while !self.err && self.warn.is_none() {
            let c = self.at(0);
            if self.i >= self.s.len() {
                self.toks.push(Tok::End);
                return;
            }
            match c {
                b' ' | b'\t' | 0x0c => self.i += 1,
                b'#' => {
                    while self.i < self.s.len() && self.s[self.i] != b'\n' {
                        self.i += 1;
                    }
                }
                b'\n' => {
                    self.i += 1;
                    if self.stack.is_empty() {
                        self.toks.push(Tok::Newline);
                    }
                }
                b'\\' => {
                    if self.at(1) == b'\n' {
                        self.i += 2;
                    } else {
                        self.err = true;
                    }
                }
                b'0'..=b'9' => self.number(),
                b'.' if self.at(1).is_ascii_digit() => self.number(),
                b'.' => {
                    if self.at(1) == b'.' && self.at(2) == b'.' {
                        self.toks.push(Tok::Op("...".into()));
                        self.i += 3;
                    } else {
                        self.toks.push(Tok::Op(".".into()));
                        self.i += 1;
                    }
                }
                b'\'' | b'"' => self.string(String::new()),
                c if is_ident_start(c) => self.name(),
                b'(' | b'[' | b'{' => {
                    if self.stack.len() >= MAX_LEVEL {
                        self.err = true; // "too many nested parentheses"
                        return;
                    }
                    self.stack.push(c);
                    self.toks.push(Tok::Op((c as char).to_string()));
                    self.i += 1;
                }
                b')' | b']' | b'}' => {
                    let open = match c {
                        b')' => b'(',
                        b']' => b'[',
                        _ => b'{',
                    };
                    if self.stack.last() != Some(&open) {
                        self.err = true; // unmatched or not matching
                        return;
                    }
                    self.stack.pop();
                    self.toks.push(Tok::Op((c as char).to_string()));
                    self.i += 1;
                }
                b'*' if self.at(1) == b'*' => {
                    self.toks.push(Tok::Op("**".into()));
                    self.i += 2;
                }
                _ => {
                    // Another operator: the parser fails on it. A byte the
                    // tokenizer rejects also ends in a SyntaxError; reading
                    // on can only find a warning Python would not print,
                    // which refuses rather than answers wrongly.
                    self.toks.push(Tok::Op((c as char).to_string()));
                    self.i += 1;
                }
            }
        }
    }

    fn name(&mut self) {
        let start = self.i;
        let (mut b, mut r, mut u, mut f) = (false, false, false, false);
        loop {
            let lc = self.at(0) | 0x20;
            if lc == b'b' && !(b || u || f) {
                b = true;
            } else if lc == b'u' && !(b || u || r || f) {
                u = true;
            } else if lc == b'r' && !(r || u) {
                r = true;
            } else if lc == b'f' && !(f || b || u) {
                f = true;
            } else {
                break;
            }
            self.i += 1;
            let q = self.at(0);
            if q == b'\'' || q == b'"' {
                if f {
                    // An f-string: its own tokens, and never a literal.
                    // Python may still warn inside it; refuse.
                    self.warn = Some("an f-string");
                    return;
                }
                let prefix = self.t[start..self.i].to_ascii_lowercase();
                self.string(prefix);
                return;
            }
        }
        while self.i < self.s.len() && is_ident_char(self.s[self.i]) {
            self.i += 1;
        }
        self.toks.push(Tok::Name(self.t[start..self.i].to_string()));
    }

    fn string(&mut self, prefix: String) {
        let q = self.at(0);
        let triple = self.at(1) == q && self.at(2) == q;
        let n = if triple { 3 } else { 1 };
        self.i += n;
        let start = self.i;
        loop {
            if self.i >= self.s.len() {
                self.err = true; // unterminated
                return;
            }
            let c = self.s[self.i];
            if c == b'\\' {
                self.i += 2;
                if self.i > self.s.len() {
                    self.err = true;
                    return;
                }
                continue;
            }
            if c == b'\n' && !triple {
                self.err = true; // unterminated string literal
                return;
            }
            if c == q && (!triple || (self.at(1) == q && self.at(2) == q)) {
                let body = self.t[start..self.i].to_string();
                self.i += n;
                if let Some(w) = escape_warning(&body, &prefix) {
                    self.warn = Some(w);
                }
                let p = if triple { format!("{prefix}3") } else { prefix };
                self.toks.push(Tok::Str(body, p));
                return;
            }
            self.i += 1;
        }
    }

    /// A numeric token as the tokenizer reads it (tok_get), with
    /// verify_end_of_number after it.
    fn number(&mut self) {
        let start = self.i;
        let mut kind = "decimal";
        let c = self.at(0);
        let ok = if c == b'.' {
            self.i += 1;
            self.fraction(&mut kind)
        } else if c == b'0' {
            self.i += 1;
            match self.at(0) | 0x20 {
                base @ (b'x' | b'o' | b'b') => {
                    kind = match base {
                        b'x' => "hexadecimal",
                        b'o' => "octal",
                        _ => "binary",
                    };
                    self.i += 1;
                    let digit = |c: u8| match base {
                        b'x' => c.is_ascii_hexdigit(),
                        b'o' => (b'0'..=b'7').contains(&c),
                        _ => c == b'0' || c == b'1',
                    };
                    loop {
                        if self.at(0) == b'_' {
                            self.i += 1;
                        }
                        if !digit(self.at(0)) {
                            self.err = true;
                            return;
                        }
                        while digit(self.at(0)) {
                            self.i += 1;
                        }
                        if self.at(0) != b'_' {
                            break;
                        }
                    }
                    if base != b'x' && self.at(0).is_ascii_digit() {
                        self.err = true; // "invalid digit ... in octal/binary literal"
                        return;
                    }
                    true
                }
                _ => {
                    let mut nonzero = false;
                    loop {
                        if self.at(0) == b'_' {
                            self.i += 1;
                            if !self.at(0).is_ascii_digit() {
                                self.err = true;
                                return;
                            }
                        }
                        if self.at(0) != b'0' {
                            break;
                        }
                        self.i += 1;
                    }
                    if self.at(0).is_ascii_digit() {
                        nonzero = true;
                        if !self.decimal_tail() {
                            return;
                        }
                    }
                    match self.at(0) {
                        b'.' => {
                            self.i += 1;
                            self.fraction(&mut kind)
                        }
                        b'e' | b'E' => self.exponent(&mut kind),
                        b'j' | b'J' => self.imaginary(&mut kind),
                        _ => {
                            if nonzero {
                                self.err = true; // leading zeros
                                return;
                            }
                            true
                        }
                    }
                }
            }
        } else {
            if !self.decimal_tail() {
                return;
            }
            match self.at(0) {
                b'.' => {
                    self.i += 1;
                    self.fraction(&mut kind)
                }
                b'e' | b'E' => self.exponent(&mut kind),
                b'j' | b'J' => self.imaginary(&mut kind),
                _ => true,
            }
        };
        if !ok {
            return;
        }
        self.toks.push(Tok::Num(self.t[start..self.i].to_string()));
        self.end_of_number(kind);
    }

    fn decimal_tail(&mut self) -> bool {
        loop {
            while self.at(0).is_ascii_digit() {
                self.i += 1;
            }
            if self.at(0) != b'_' {
                return true;
            }
            self.i += 1;
            if !self.at(0).is_ascii_digit() {
                self.err = true;
                return false;
            }
        }
    }

    fn imaginary(&mut self, kind: &mut &'static str) -> bool {
        self.i += 1;
        *kind = "imaginary";
        true
    }

    fn exponent(&mut self, kind: &mut &'static str) -> bool {
        let e = self.i;
        self.i += 1;
        let c = self.at(0);
        if c == b'+' || c == b'-' {
            self.i += 1;
            if !self.at(0).is_ascii_digit() {
                self.err = true;
                return false;
            }
        } else if !c.is_ascii_digit() {
            // "1e" then no digit: the number ends before the e, and the e
            // is checked as what follows it.
            self.i = e;
            return true;
        }
        if !self.decimal_tail() {
            return false;
        }
        if matches!(self.at(0), b'j' | b'J') {
            return self.imaginary(kind);
        }
        true
    }

    fn fraction(&mut self, kind: &mut &'static str) -> bool {
        if self.at(0).is_ascii_digit() && !self.decimal_tail() {
            return false;
        }
        match self.at(0) {
            b'e' | b'E' => self.exponent(kind),
            b'j' | b'J' => self.imaginary(kind),
            _ => true,
        }
    }

    /// verify_end_of_number: a keyword that can follow a number in valid
    /// code makes a SyntaxWarning, another ASCII name character a
    /// SyntaxError.
    fn end_of_number(&mut self, kind: &str) {
        let rest = &self.s[self.i..];
        let warn = match self.at(0) {
            b'a' => rest.starts_with(b"and"),
            b'e' => rest.starts_with(b"else"),
            b'f' => rest.starts_with(b"for"),
            b'i' => matches!(self.at(1), b'f' | b'n' | b's'),
            b'o' => rest.starts_with(b"or"),
            b'n' => rest.starts_with(b"not"),
            _ => false,
        };
        if warn {
            self.warn = Some(match kind {
                "hexadecimal" => "an invalid hexadecimal literal",
                "octal" => "an invalid octal literal",
                "binary" => "an invalid binary literal",
                "imaginary" => "an invalid imaginary literal",
                _ => "an invalid decimal literal",
            });
            return;
        }
        let c = self.at(0);
        if c < 0x80 && is_ident_char(c) && self.i < self.s.len() {
            self.err = true;
        }
    }
}

/// The SyntaxWarning Python prints for a string body, if any.
fn escape_warning(body: &str, prefix: &str) -> Option<&'static str> {
    if prefix.contains('r') {
        return None;
    }
    let is_bytes = prefix.contains('b');
    let b = body.as_bytes();
    let mut i = 0;
    while i < b.len() {
        if b[i] != b'\\' {
            i += 1;
            continue;
        }
        i += 1;
        if i >= b.len() {
            return None;
        }
        let c = b[i];
        match c {
            b'\n' | b'\\' | b'\'' | b'"' | b'a' | b'b' | b'f' | b'n' | b'r' | b't' | b'v' | b'x' => {}
            b'0'..=b'7' => {
                let mut j = i;
                while j < b.len() && j < i + 3 && (b'0'..=b'7').contains(&b[j]) {
                    j += 1;
                }
                let v = u32::from_str_radix(&body[i..j], 8).unwrap_or(0);
                if v > 0o377 {
                    return Some("an invalid octal escape");
                }
                i = j - 1;
            }
            b'N' | b'u' | b'U' if !is_bytes => {}
            c if c >= 0x80 && !is_bytes => {
                // A backslash before a non-ASCII character is kept, with
                // no warning.
            }
            _ => return Some("an invalid escape sequence"),
        }
        i += 1;
    }
    None
}

/// Python 3.12's `sys.int_info.default_max_str_digits`.
const MAX_INT_DIGITS: usize = 4300;

// --- syntax tree ---

#[derive(Debug, Clone)]
enum Node {
    Int(String),
    Float(f64),
    Complex,
    /// A signed int or float plus or minus a complex constant.
    CplxSum,
    /// A hex, octal or binary int past 4300 decimal digits.
    HugeInt,
    /// A str; `true` when it holds a `\N{...}` escape this port does not
    /// resolve.
    Str(String, bool),
    Bytes,
    True,
    False,
    NoneV,
    Ellipsis,
    List(Vec<Node>),
    Tuple(Vec<Node>),
    Set(Vec<Node>),
    Dict(Vec<Node>, Vec<Node>),
    /// A sign on a number: negative, operand.
    Unary(bool, Box<Node>),
}

struct Parser<'a> {
    toks: &'a [Tok],
    i: usize,
}

impl Parser<'_> {
    fn is_op(&self, s: &str) -> bool {
        matches!(&self.toks[self.i], Tok::Op(o) if o == s)
    }

    fn top(&mut self) -> Option<Node> {
        let n = self.expr()?;
        while self.toks[self.i] == Tok::Newline {
            self.i += 1;
        }
        (self.toks[self.i] == Tok::End).then_some(n)
    }

    fn expr(&mut self) -> Option<Node> {
        let left = self.signed()?;
        if !self.is_op("+") && !self.is_op("-") {
            return Some(left);
        }
        self.i += 1;
        let right = self.signed()?;
        if self.is_op("+") || self.is_op("-") {
            return None;
        }
        let base = match &left {
            Node::Unary(_, x) => x.as_ref(),
            x => x,
        };
        if !matches!(base, Node::Int(_) | Node::Float(_) | Node::HugeInt) || !matches!(right, Node::Complex) {
            return None;
        }
        Some(Node::CplxSum)
    }

    fn signed(&mut self) -> Option<Node> {
        if self.is_op("+") || self.is_op("-") {
            let neg = self.is_op("-");
            self.i += 1;
            if self.is_op("+") || self.is_op("-") {
                return None;
            }
            let x = self.atom()?;
            if !matches!(x, Node::Int(_) | Node::Float(_) | Node::Complex | Node::HugeInt) {
                return None;
            }
            return Some(Node::Unary(neg, Box::new(x)));
        }
        self.atom()
    }

    fn atom(&mut self) -> Option<Node> {
        match self.toks[self.i].clone() {
            Tok::Num(t) => {
                self.i += 1;
                number_node(&t)
            }
            Tok::Str(..) => self.strings(),
            Tok::Name(n) => {
                self.i += 1;
                match n.as_str() {
                    "True" => Some(Node::True),
                    "False" => Some(Node::False),
                    "None" => Some(Node::NoneV),
                    "set" if self.is_op("(") && matches!(&self.toks[self.i + 1], Tok::Op(o) if o == ")") => {
                        self.i += 2;
                        Some(Node::Set(vec![]))
                    }
                    _ => None,
                }
            }
            Tok::Op(o) => match o.as_str() {
                "..." => {
                    self.i += 1;
                    Some(Node::Ellipsis)
                }
                "(" => {
                    self.i += 1;
                    if self.is_op(")") {
                        self.i += 1;
                        return Some(Node::Tuple(vec![]));
                    }
                    let first = self.expr()?;
                    if self.is_op(")") {
                        self.i += 1;
                        return Some(first);
                    }
                    Some(Node::Tuple(self.seq_after(first, ")")?))
                }
                "[" => {
                    self.i += 1;
                    if self.is_op("]") {
                        self.i += 1;
                        return Some(Node::List(vec![]));
                    }
                    let first = self.expr()?;
                    if self.is_op("]") {
                        self.i += 1;
                        return Some(Node::List(vec![first]));
                    }
                    Some(Node::List(self.seq_after(first, "]")?))
                }
                "{" => {
                    self.i += 1;
                    if self.is_op("}") {
                        self.i += 1;
                        return Some(Node::Dict(vec![], vec![]));
                    }
                    let first = self.expr()?;
                    if self.is_op(":") {
                        return self.dict_after(first);
                    }
                    if self.is_op("}") {
                        self.i += 1;
                        return Some(Node::Set(vec![first]));
                    }
                    Some(Node::Set(self.seq_after(first, "}")?))
                }
                _ => None,
            },
            _ => None,
        }
    }

    fn seq_after(&mut self, first: Node, end: &str) -> Option<Vec<Node>> {
        let mut xs = vec![first];
        loop {
            if !self.is_op(",") {
                return None;
            }
            self.i += 1;
            if self.is_op(end) {
                self.i += 1;
                return Some(xs);
            }
            xs.push(self.expr()?);
            if self.is_op(end) {
                self.i += 1;
                return Some(xs);
            }
        }
    }

    fn dict_after(&mut self, first: Node) -> Option<Node> {
        let (mut ks, mut vs) = (vec![], vec![]);
        let mut k = first;
        loop {
            if !self.is_op(":") {
                return None;
            }
            self.i += 1;
            let v = self.expr()?;
            ks.push(k);
            vs.push(v);
            if self.is_op("}") {
                self.i += 1;
                return Some(Node::Dict(ks, vs));
            }
            if !self.is_op(",") {
                return None;
            }
            self.i += 1;
            if self.is_op("}") {
                self.i += 1;
                return Some(Node::Dict(ks, vs));
            }
            k = self.expr()?;
        }
    }

    /// Adjacent string tokens as one constant.
    fn strings(&mut self) -> Option<Node> {
        let mut out = String::new();
        let mut named = false;
        let is_bytes = matches!(&self.toks[self.i], Tok::Str(_, p) if p.contains('b'));
        while let Tok::Str(body, prefix) = &self.toks[self.i] {
            self.i += 1;
            if prefix.contains('b') != is_bytes {
                return None; // "cannot mix bytes and nonbytes literals"
            }
            let raw = prefix.contains('r');
            if is_bytes {
                if !body.is_ascii() {
                    return None; // bytes can only contain ASCII
                }
                if !raw && !bytes_escapes_ok(body) {
                    return None;
                }
                continue;
            }
            if raw {
                out.push_str(body);
                continue;
            }
            let (s, nm) = decode_str_escapes(body)?;
            named |= nm;
            out.push_str(&s);
        }
        if is_bytes {
            return Some(Node::Bytes);
        }
        Some(Node::Str(out, named))
    }
}

/// False where `_PyBytes_DecodeEscape` raises: a `\x` without two hex
/// digits.
fn bytes_escapes_ok(body: &str) -> bool {
    let b = body.as_bytes();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'\\' {
            i += 1;
            if b.get(i) == Some(&b'x') {
                let ok = |k: usize| b.get(k).is_some_and(|c| c.is_ascii_hexdigit());
                if !ok(i + 1) || !ok(i + 2) {
                    return false;
                }
                i += 2;
            }
        }
        i += 1;
    }
    true
}

/// The unicode-escape decoding of a str body; the flag is set for a
/// `\N{...}` escape, whose character this port does not look up. None
/// where Python raises.
fn decode_str_escapes(body: &str) -> Option<(String, bool)> {
    let b = body.as_bytes();
    let mut out = String::new();
    let mut named = false;
    let mut i = 0;
    while i < b.len() {
        if b[i] != b'\\' {
            let ch = body[i..].chars().next()?;
            out.push(ch);
            i += ch.len_utf8();
            continue;
        }
        i += 1;
        let Some(&e) = b.get(i) else {
            out.push('\\');
            break;
        };
        i += 1;
        match e {
            b'\n' => {}
            b'\\' | b'\'' | b'"' => out.push(e as char),
            b'a' => out.push('\u{7}'),
            b'b' => out.push('\u{8}'),
            b'f' => out.push('\u{c}'),
            b'n' => out.push('\n'),
            b'r' => out.push('\r'),
            b't' => out.push('\t'),
            b'v' => out.push('\u{b}'),
            b'0'..=b'7' => {
                let j = i - 1;
                let mut k = j;
                while k < b.len() && k < j + 3 && (b'0'..=b'7').contains(&b[k]) {
                    k += 1;
                }
                let r = u32::from_str_radix(&body[j..k], 8).ok()?;
                i = k;
                out.push(cp_char(r));
            }
            b'x' | b'u' | b'U' => {
                let n = match e {
                    b'x' => 2,
                    b'u' => 4,
                    _ => 8,
                };
                let hex = body.get(i..i + n)?;
                if !hex.bytes().all(|h| h.is_ascii_hexdigit()) {
                    return None;
                }
                let r = u32::from_str_radix(hex, 16).ok()?;
                if r > 0x10FFFF {
                    return None;
                }
                i += n;
                out.push(cp_char(r));
            }
            b'N' => {
                // \N{name}: a malformed escape raises; a well-formed one
                // names a character this port does not look up.
                let end = body[i..].find('}');
                if b.get(i) != Some(&b'{') || end.is_none_or(|e| e < 2) {
                    return None;
                }
                named = true;
                out.push_str("\\N");
            }
            _ => {
                // Kept with its backslash (the warning was checked when the
                // token was read); a non-ASCII character keeps it with none.
                out.push('\\');
                i -= 1;
            }
        }
    }
    Some((out, named))
}

/// `int(digits, base)` as decimal text, for any size.
fn to_decimal(digits: &str, base: u32) -> Option<String> {
    // Little-endian limbs of 10^9.
    let mut limbs: Vec<u64> = vec![0];
    for c in digits.chars() {
        let d = c.to_digit(base)? as u64;
        let mut carry = d;
        for l in limbs.iter_mut() {
            let v = *l * base as u64 + carry;
            *l = v % 1_000_000_000;
            carry = v / 1_000_000_000;
        }
        while carry > 0 {
            limbs.push(carry % 1_000_000_000);
            carry /= 1_000_000_000;
        }
    }
    let mut s = limbs.last()?.to_string();
    for l in limbs.iter().rev().skip(1) {
        s.push_str(&format!("{l:09}"));
    }
    Some(s)
}

fn number_node(text: &str) -> Option<Node> {
    let clean: String = text.chars().filter(|c| *c != '_').collect();
    let last = clean.as_bytes()[clean.len() - 1] | 0x20;
    if last == b'j' {
        return Some(Node::Complex);
    }
    let cb = clean.as_bytes();
    if cb.len() > 1 && cb[0] == b'0' {
        let base = match cb[1] | 0x20 {
            b'x' => 16,
            b'o' => 8,
            b'b' => 2,
            _ => 0,
        };
        if base != 0 {
            let d = to_decimal(&clean[2..], base)?;
            if d.len() > MAX_INT_DIGITS {
                // Read, but str() of it raises: a value this port does not
                // hold.
                return Some(Node::HugeInt);
            }
            return Some(Node::Int(d));
        }
    }
    if !clean.contains(['.', 'e', 'E']) {
        // Python 3.12 reads at most 4300 decimal digits into an int; past
        // that the literal is a SyntaxError (a run of zeros is 0).
        if clean.len() > MAX_INT_DIGITS && clean.trim_matches('0') != "" {
            return None;
        }
        return Some(Node::Int(to_decimal(&clean, 10)?));
    }
    clean.parse::<f64>().ok().map(Node::Float)
}

// --- evaluation ---

struct Eval {
    why: Option<&'static str>,
    tuple: bool,
}

/// False for a list, a dict or a set, and for a tuple that holds one: as
/// a dict key or a set item it raises TypeError.
fn hashable(n: &Node) -> bool {
    match n {
        Node::List(_) | Node::Dict(..) | Node::Set(_) => false,
        Node::Tuple(xs) => xs.iter().all(hashable),
        _ => true,
    }
}

/// Whether evaluating `n` marked it as a value this port does not model.
fn opaque(n: &Node) -> bool {
    matches!(n, Node::Complex | Node::CplxSum | Node::HugeInt | Node::Bytes | Node::Ellipsis | Node::Set(_) | Node::Str(_, true))
        || matches!(n, Node::Unary(_, x) if matches!(x.as_ref(), Node::Complex))
}

impl Eval {
    fn mark(&mut self, why: &'static str) -> Value {
        self.why.get_or_insert(why);
        Value::Null
    }

    fn eval(&mut self, n: &Node) -> Option<Value> {
        Some(match n {
            Node::Int(t) => Value::Int(t.clone()),
            Node::Float(f) => Value::Float(*f),
            Node::Unary(neg, x) => match x.as_ref() {
                Node::Int(t) if !*neg || t == "0" => Value::Int(t.clone()),
                Node::Int(t) => Value::Int(format!("-{t}")),
                Node::Float(f) => Value::Float(if *neg { -f } else { *f }),
                Node::HugeInt => self.mark("an int past 4300 decimal digits"),
                _ => self.mark("a complex number"),
            },
            Node::Complex | Node::CplxSum => self.mark("a complex number"),
            Node::HugeInt => self.mark("an int past 4300 decimal digits"),
            Node::Str(_, true) => self.mark("a \\N{...} escape"),
            Node::Str(s, false) => Value::Str(s.clone()),
            Node::Bytes => self.mark("bytes"),
            Node::True => Value::Bool(true),
            Node::False => Value::Bool(false),
            Node::NoneV => Value::Null,
            Node::Ellipsis => self.mark("Ellipsis"),
            Node::List(xs) | Node::Tuple(xs) => {
                if matches!(n, Node::Tuple(_)) {
                    self.tuple = true;
                }
                let mut out = Vec::with_capacity(xs.len());
                for x in xs {
                    out.push(self.eval(x)?);
                }
                Value::List(out)
            }
            Node::Set(xs) => {
                for x in xs {
                    if !hashable(x) {
                        return None;
                    }
                    self.eval(x)?;
                }
                self.mark("a set")
            }
            Node::Dict(..) => self.dict(n, None)?,
        })
    }

    /// A dict; `opaque_keys`, when given, collects the keys whose values
    /// this port does not model.
    fn dict(&mut self, n: &Node, mut opaque_keys: Option<&mut Vec<String>>) -> Option<Value> {
        let Node::Dict(ks, vs) = n else { return None };
        let mut o = Object::new();
        for (k, v) in ks.iter().zip(vs) {
            if !hashable(k) {
                return None;
            }
            let kv = self.eval(k)?;
            let vv = self.eval(v)?;
            match (kv, k) {
                (Value::Str(s), Node::Str(_, false)) => {
                    if let Some(keys) = opaque_keys.as_deref_mut() {
                        if opaque(v) {
                            keys.push(s.clone());
                        } else {
                            keys.retain(|x| x != &s);
                        }
                    }
                    o.set(&s, vv);
                }
                _ => {
                    self.mark("a dict key that is not a str");
                }
            }
        }
        Some(Value::Obj(o))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::dumps;

    fn lit(s: &str) -> (Status, Option<String>, bool) {
        let l = literal_eval(s);
        (l.status, l.value.filter(|_| l.status == Status::Value).map(|v| dumps(&v, false)), l.tuple)
    }

    #[test]
    fn reads_the_literals_a_model_writes() {
        assert_eq!(
            lit("{'a': 1, \"b\": [True, None, (2,)], 'c': -1.5e3}"),
            (Status::Value, Some(r#"{"a": 1, "b": [true, null, [2]], "c": -1500.0}"#.into()), true)
        );
        assert_eq!(lit("(1)").1.unwrap(), "1");
        assert_eq!(lit("'\\x41\\n\\101'").1.unwrap(), r#""A\nA""#);
        assert_eq!(lit("-0").1.unwrap(), "0");
        assert_eq!(lit("1_000").1.unwrap(), "1000");
        assert_eq!(lit("0x10").1.unwrap(), "16");
        assert_eq!(lit("0xFFFFFFFFFFFFFFFFFFFF").1.unwrap(), "1208925819614629174706175");
        assert_eq!(lit("'''x'''").1.unwrap(), r#""x""#);
        assert_eq!(lit("'a' r'\\d' \"b\"").1.unwrap(), r#""a\\db""#);
        assert_eq!(lit("{'a': }").0, Status::None);
        assert_eq!(lit("{1: 2}").0, Status::Refused);
        assert_eq!(lit("01").0, Status::None);
        assert_eq!(lit("{'a': 1} x").0, Status::None);
        assert_eq!(lit("{'a': '\\d'}").0, Status::Refused);
        assert_eq!(lit("{'a': 1if 1 else 2}").0, Status::Refused);
        assert_eq!(lit("{[1]: 2}").0, Status::None);
        assert_eq!(lit("{'a': -(1+2j)}").0, Status::None);
        assert_eq!(lit("{'a': 1+2j}").0, Status::Refused);
    }
}
