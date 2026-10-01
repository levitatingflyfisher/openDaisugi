//! `yaml.safe_load` on a str, as PyYAML 6 does it: the reader, scanner,
//! parser, composer, resolver and SafeConstructor, translated from the
//! Python source one method at a time, so the same text gives the same
//! value, or raises the same error with the same words. The Go client's
//! `pyyaml/full.go` and `pyyaml/pyyaml.go` are the reference.
//!
//! A load answers in three ways: the value, the exception safe_load raises
//! (its class and `str(exc)`), or `Unsupported`, for a value the result
//! model does not hold (`!!binary`, `!!omap`, `!!pairs` and `!!set`
//! values, a node that holds itself, a float or timestamp key, nesting
//! deep enough to reach Python's recursion limit). The caller must not
//! guess at an unsupported text.

use crate::gate::py::text::{lower, repr, splitlines};
use num_bigint::BigInt;
use regex::Regex;
use std::cell::Cell;
use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::OnceLock;

const MAX_DEPTH: usize = 200;

/// A loaded value. A mapping keeps its keys in dict order; a key that is
/// not a str is marked with a leading NUL (see `dict_key`), so it never
/// equals a field name.
#[derive(Debug, Clone, PartialEq)]
pub enum Val {
    Null,
    Bool(bool),
    /// An int as its decimal text, so any size round-trips.
    Int(String),
    Float(f64),
    Str(String),
    /// A `datetime.date` or `datetime.datetime`, as its text.
    Timestamp(String),
    List(Vec<Val>),
    Map(Vec<(String, Val)>),
}

impl Val {
    /// The value of a str key, if the mapping holds it.
    pub fn get(&self, k: &str) -> Option<&Val> {
        match self {
            Val::Map(m) => m.iter().find(|(kk, _)| kk == k).map(|(_, v)| v),
            _ => None,
        }
    }
}

/// An exception safe_load raises: its class and `str(exc)`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Exc {
    pub kind: &'static str,
    pub msg: String,
}

/// Why a load gives no value.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Fail {
    Exc(Exc),
    Unsupported(String),
}

type R<T> = Result<T, Fail>;

fn exc(kind: &'static str, msg: impl Into<String>) -> Fail {
    Fail::Exc(Exc { kind, msg: msg.into() })
}

fn unsupported<T>(why: &str) -> R<T> {
    Err(Fail::Unsupported(why.into()))
}

/// `yaml.safe_load(text)`.
pub fn load(text: &str) -> R<Val> {
    if let Some((i, c)) = text.chars().enumerate().find(|&(_, c)| !printable(c)) {
        // yaml.reader.Reader.check_printable raises ReaderError.
        return Err(exc(
            "ReaderError",
            format!(
                "unacceptable character #x{:04x}: special characters are not allowed\n  in \"<unicode string>\", position {i}",
                c as u32
            ),
        ));
    }
    let mut l = Loader::new(text);
    let r = l.load_all();
    if l.overrun.get() {
        // Python's Reader.peek raises IndexError past the buffer.
        return unsupported("a read past the end of the text");
    }
    r
}

fn printable(c: char) -> bool {
    let r = c as u32;
    r == 0x9
        || r == 0xa
        || r == 0xd
        || (0x20..=0x7e).contains(&r)
        || r == 0x85
        || (0xa0..=0xd7ff).contains(&r)
        || (0xe000..=0xfffd).contains(&r)
        || (0x10000..=0x10ffff).contains(&r)
}

/// Whether `v` holds only what JSON holds: no timestamp and no key that is
/// not a str, at any depth.
pub fn plain(v: &Val) -> bool {
    match v {
        Val::Timestamp(_) => false,
        Val::List(xs) => xs.iter().all(plain),
        Val::Map(m) => m.iter().all(|(k, v)| !k.starts_with('\0') && plain(v)),
        _ => true,
    }
}

/// Python's truth value of `v`.
pub fn truthy(v: &Val) -> bool {
    match v {
        Val::Null => false,
        Val::Bool(b) => *b,
        Val::Int(t) => t != "0",
        Val::Float(f) => *f != 0.0,
        Val::Str(s) => !s.is_empty(),
        Val::Timestamp(_) => true,
        Val::List(xs) => !xs.is_empty(),
        Val::Map(m) => !m.is_empty(),
    }
}

/// The value as JSON holds it, or None when it is not `plain`.
pub fn to_json(v: &Val) -> Option<crate::gate::pyjson::Value> {
    use crate::gate::pyjson::{Object, Value};
    Some(match v {
        Val::Null => Value::Null,
        Val::Bool(b) => Value::Bool(*b),
        Val::Int(t) => Value::Int(t.clone()),
        Val::Float(f) => Value::Float(*f),
        Val::Str(s) => Value::Str(s.clone()),
        Val::Timestamp(_) => return None,
        Val::List(xs) => Value::List(xs.iter().map(to_json).collect::<Option<Vec<_>>>()?),
        Val::Map(m) => {
            let mut o = Object::new();
            for (k, x) in m {
                if k.starts_with('\0') {
                    return None;
                }
                o.set(k, to_json(x)?);
            }
            Value::Obj(o)
        }
    })
}

/// Whether `except (yaml.YAMLError, ValueError)` catches `e`: PyYAML's own
/// errors and a ValueError (a date out of range).
pub fn caught(e: &Exc) -> bool {
    matches!(
        e.kind,
        "ScannerError" | "ParserError" | "ComposerError" | "ConstructorError" | "ReaderError" | "ValueError"
    )
}

/// `str(exc).splitlines()[0]`.
pub fn first_line(e: &Exc) -> String {
    splitlines(&e.msg).first().map(|s| s.to_string()).unwrap_or_default()
}

/// The exception's class as a traceback's last line names it.
pub fn qualified(e: &Exc) -> String {
    match e.kind {
        "ScannerError" => "yaml.scanner.ScannerError".into(),
        "ParserError" => "yaml.parser.ParserError".into(),
        "ComposerError" => "yaml.composer.ComposerError".into(),
        "ConstructorError" => "yaml.constructor.ConstructorError".into(),
        "ReaderError" => "yaml.reader.ReaderError".into(),
        k => k.into(),
    }
}

// ---------------------------------------------------------------------------
// Resolver patterns and scalar constructors (yaml/resolver.py,
// yaml/constructor.py)
// ---------------------------------------------------------------------------

fn re(cell: &'static OnceLock<Regex>, pat: &str) -> &'static Regex {
    cell.get_or_init(|| Regex::new(pat).expect("pattern"))
}

fn bool_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(&C, r"^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$")
}

fn float_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(
        &C,
        r"^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$",
    )
}

fn int_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(
        &C,
        r"^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$",
    )
}

fn null_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(&C, r"^(?:~|null|Null|NULL|)$")
}

fn time_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(
        &C,
        r"^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$",
    )
}

fn timestamp_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(
        &C,
        r"^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$",
    )
}

/// What Python's float() reads from lower-case ASCII text with no white
/// space.
fn py_float_re() -> &'static Regex {
    static C: OnceLock<Regex> = OnceLock::new();
    re(&C, r"^[-+]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[-+]?[0-9]+)?|inf|infinity|nan)$")
}

/// SafeConstructor.construct_yaml_int on text the resolver's int pattern
/// takes.
fn yaml_int(v: &str) -> R<String> {
    let mut v = v.replace('_', "");
    let mut neg = false;
    if v.starts_with('-') || v.starts_with('+') {
        neg = v.starts_with('-');
        v.remove(0);
    }
    let n: Option<BigInt> = if v == "0" {
        Some(BigInt::from(0))
    } else if let Some(d) = v.strip_prefix("0b") {
        BigInt::parse_bytes(d.as_bytes(), 2)
    } else if let Some(d) = v.strip_prefix("0x") {
        BigInt::parse_bytes(d.as_bytes(), 16)
    } else if v.starts_with('0') {
        BigInt::parse_bytes(v.as_bytes(), 8)
    } else if v.contains(':') {
        let mut n = BigInt::from(0);
        for part in v.split(':') {
            let Ok(d) = part.parse::<i64>() else {
                return unsupported("a base 60 int the subset does not read");
            };
            n = n * 60 + d;
        }
        Some(n)
    } else {
        // int(value) reads at most 4300 decimal digits (Python 3.12's
        // sys.int_info.default_max_str_digits); past that it raises.
        if v.len() > 4300 {
            return Err(exc(
                "ValueError",
                format!(
                    "Exceeds the limit (4300 digits) for integer string conversion: value has {} digits; use sys.set_int_max_str_digits() to increase the limit",
                    v.len()
                ),
            ));
        }
        BigInt::parse_bytes(v.as_bytes(), 10)
    };
    let Some(mut n) = n else {
        return unsupported("an int with no digits");
    };
    if neg {
        n = -n;
    }
    Ok(n.to_string())
}

/// SafeConstructor.construct_yaml_float on text the resolver's float
/// pattern takes.
fn yaml_float(v: &str) -> R<f64> {
    let mut v = v.replace('_', "").to_lowercase();
    let mut sign = 1.0;
    if v.starts_with('-') || v.starts_with('+') {
        if v.starts_with('-') {
            sign = -1.0;
        }
        v.remove(0);
    }
    if v == ".inf" {
        return Ok(sign * f64::INFINITY);
    }
    if v == ".nan" {
        return Ok(f64::NAN);
    }
    if v.contains(':') {
        let (mut value, mut base) = (0.0, 1.0);
        for part in v.split(':').rev() {
            let Ok(d) = part.parse::<f64>() else {
                return unsupported("a base 60 float the subset does not read");
            };
            value += d * base;
            base *= 60.0;
        }
        return Ok(sign * value);
    }
    match v.parse::<f64>() {
        Ok(f) => Ok(sign * f),
        Err(_) => unsupported("a float the subset does not read"),
    }
}

/// construct_yaml_float on text the resolver's float pattern does not
/// take: the error it raises, or a refusal where float() may read the text.
fn float_error<T>(v: &str) -> R<T> {
    let mut s = lower(&v.replace('_', ""));
    if s.is_empty() {
        return Err(exc("IndexError", "string index out of range"));
    }
    if s.starts_with('-') || s.starts_with('+') {
        s.remove(0);
    }
    if s == ".inf" || s == ".nan" || s.contains(':') || py_float_re().is_match(&s) {
        return unsupported("a !!float value Python's float() may read another way");
    }
    if s.chars().any(|r| r <= ' ' || r as u32 >= 0x7f) {
        return unsupported("a !!float value Python's float() may read another way");
    }
    Err(exc("ValueError", format!("could not convert string to float: {}", repr(&s))))
}

/// repr(datetime.timedelta(minutes=±mins)).
fn offset_repr(mins: i64, negative: bool) -> String {
    let secs = if negative { -mins * 60 } else { mins * 60 };
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    let mut parts = vec![];
    if days != 0 {
        parts.push(format!("days={days}"));
    }
    if rem != 0 {
        parts.push(format!("seconds={rem}"));
    }
    if parts.is_empty() {
        return "datetime.timedelta(0)".into();
    }
    format!("datetime.timedelta({})", parts.join(", "))
}

/// SafeConstructor.construct_yaml_timestamp: it raises ValueError where
/// datetime does.
fn timestamp(v: &str) -> R<Val> {
    let Some(m) = timestamp_re().captures(v) else {
        return Err(exc("AttributeError", "'NoneType' object has no attribute 'groupdict'"));
    };
    let g = |i: usize| m.get(i).map(|x| x.as_str()).unwrap_or("");
    let atoi = |s: &str| s.parse::<i64>().unwrap_or(0);
    let (year, month, day) = (atoi(g(1)), atoi(g(2)), atoi(g(3)));
    let bad = |msg: String| -> R<Val> { Err(exc("ValueError", msg)) };
    // construct_yaml_timestamp makes the timezone before the datetime, so
    // a bad offset raises before a bad date.
    if !g(4).is_empty() && !g(9).is_empty() {
        let off = atoi(g(10)) * 60 + atoi(g(11));
        if off >= 24 * 60 {
            return bad(format!(
                "offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not {}.",
                offset_repr(off, g(9) == "-")
            ));
        }
    }
    if year < 1 {
        return bad("year 0 is out of range".into());
    }
    if !(1..=12).contains(&month) {
        return bad("month must be in 1..12".into());
    }
    let mut days = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][(month - 1) as usize];
    if month == 2 && year % 4 == 0 && (year % 100 != 0 || year % 400 == 0) {
        days = 29;
    }
    if day < 1 || day > days {
        return bad("day is out of range for month".into());
    }
    if g(4).is_empty() {
        return Ok(Val::Timestamp(v.into()));
    }
    if atoi(g(4)) > 23 {
        return bad("hour must be in 0..23".into());
    }
    if atoi(g(5)) > 59 {
        return bad("minute must be in 0..59".into());
    }
    if atoi(g(6)) > 59 {
        return bad("second must be in 0..59".into());
    }
    Ok(Val::Timestamp(v.into()))
}

// ---------------------------------------------------------------------------
// Marks, tokens, events, nodes
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy)]
struct Mark {
    line: usize,
    column: usize,
    ptr: usize,
}

fn is_break_z(c: char) -> bool {
    matches!(c, '\0' | '\r' | '\n' | '\u{85}' | '\u{2028}' | '\u{2029}')
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Tk {
    Directive,
    DocStart,
    DocEnd,
    StreamStart,
    StreamEnd,
    BlockSeqStart,
    BlockMapStart,
    BlockEnd,
    FlowSeqStart,
    FlowMapStart,
    FlowSeqEnd,
    FlowMapEnd,
    Key,
    Value,
    BlockEntry,
    FlowEntry,
    Alias,
    Anchor,
    Tag,
    Scalar,
}

fn tok_id(k: Tk) -> &'static str {
    match k {
        Tk::Directive => "<directive>",
        Tk::DocStart => "<document start>",
        Tk::DocEnd => "<document end>",
        Tk::StreamStart => "<stream start>",
        Tk::StreamEnd => "<stream end>",
        Tk::BlockSeqStart => "<block sequence start>",
        Tk::BlockMapStart => "<block mapping start>",
        Tk::BlockEnd => "<block end>",
        Tk::FlowSeqStart => "[",
        Tk::FlowMapStart => "{",
        Tk::FlowSeqEnd => "]",
        Tk::FlowMapEnd => "}",
        Tk::Key => "?",
        Tk::Value => ":",
        Tk::BlockEntry => "-",
        Tk::FlowEntry => ",",
        Tk::Alias => "<alias>",
        Tk::Anchor => "<anchor>",
        Tk::Tag => "<tag>",
        Tk::Scalar => "<scalar>",
    }
}

#[derive(Debug, Clone)]
struct Token {
    kind: Tk,
    start: Mark,
    end: Mark,
    /// A scalar's, alias's or anchor's value; a directive's name.
    value: String,
    plain: bool,
    /// A tag's handle (None is Python's None).
    handle: Option<String>,
    suffix: String,
    major: i64,
    tag_handle: String,
    tag_prefix: String,
}

impl Token {
    fn new(kind: Tk, start: Mark, end: Mark) -> Token {
        Token {
            kind,
            start,
            end,
            value: String::new(),
            plain: false,
            handle: None,
            suffix: String::new(),
            major: 0,
            tag_handle: String::new(),
            tag_prefix: String::new(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Ev {
    StreamStart,
    StreamEnd,
    DocStart,
    DocEnd,
    Alias,
    Scalar,
    SeqStart,
    SeqEnd,
    MapStart,
    MapEnd,
}

#[derive(Debug, Clone)]
struct Event {
    kind: Ev,
    start: Mark,
    anchor: Option<String>,
    tag: Option<String>,
    implicit0: bool,
    value: String,
}

impl Event {
    fn new(kind: Ev, start: Mark) -> Event {
        Event { kind, start, anchor: None, tag: None, implicit0: false, value: String::new() }
    }
}

/// The parser's states (yaml/parser.py's `self.state` functions).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum St {
    StreamStart,
    ImplicitDocumentStart,
    DocumentStart,
    DocumentEnd,
    DocumentContent,
    BlockNode,
    BlockSequenceFirstEntry,
    BlockSequenceEntry,
    IndentlessSequenceEntry,
    BlockMappingFirstKey,
    BlockMappingKey,
    BlockMappingValue,
    FlowSequenceFirstEntry,
    FlowSequenceEntry,
    FlowSequenceEntryMappingKey,
    FlowSequenceEntryMappingValue,
    FlowSequenceEntryMappingEnd,
    FlowMappingFirstKey,
    FlowMappingKey,
    FlowMappingValue,
    FlowMappingEmptyValue,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Nk {
    Scalar,
    Seq,
    Map,
}

fn node_id(k: Nk) -> &'static str {
    match k {
        Nk::Scalar => "scalar",
        Nk::Seq => "sequence",
        Nk::Map => "mapping",
    }
}

#[derive(Debug, Clone)]
struct Node {
    kind: Nk,
    tag: String,
    value: String,
    items: Vec<usize>,
    pairs: Vec<(usize, usize)>,
    start: Mark,
}

#[derive(Debug, Clone)]
struct PossibleKey {
    token_number: usize,
    required: bool,
    index: usize,
    line: usize,
    column: usize,
    mark: Mark,
}

const BREAKZ: &str = "\0\r\n\u{85}\u{2028}\u{2029}";
const BLANKZ: &str = "\0 \t\r\n\u{85}\u{2028}\u{2029}";
const BREAKS: &str = "\r\n\u{85}\u{2028}\u{2029}";
const ALNUM: &str = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_";
const HEX: &str = "0123456789ABCDEFabcdef";
const URI_EXTRA: &str = ";/?:@&=+$,_.!~*'()[]%";

fn is_in(c: char, set: &str) -> bool {
    set.contains(c)
}

fn is_alnum(c: char) -> bool {
    c.is_ascii() && ALNUM.contains(c)
}

fn rr(c: char) -> String {
    repr(&c.to_string())
}

const TAG_NULL: &str = "tag:yaml.org,2002:null";
const TAG_BOOL: &str = "tag:yaml.org,2002:bool";
const TAG_INT: &str = "tag:yaml.org,2002:int";
const TAG_FLOAT: &str = "tag:yaml.org,2002:float";
const TAG_STR: &str = "tag:yaml.org,2002:str";
const TAG_SEQ: &str = "tag:yaml.org,2002:seq";
const TAG_MAP: &str = "tag:yaml.org,2002:map";
const TAG_TIMESTAMP: &str = "tag:yaml.org,2002:timestamp";
const TAG_MERGE: &str = "tag:yaml.org,2002:merge";
const TAG_VALUE: &str = "tag:yaml.org,2002:value";

/// A constructed value before it becomes a `Val`: a list or dict is a slot
/// filled in later, as Python's generators fill them.
#[derive(Debug, Clone)]
enum CVal {
    Null,
    Bool(bool),
    Int(String),
    Float(f64),
    Str(String),
    Ts(String),
    Seq(usize),
    Map(usize),
    /// A set, an ordered map or pairs: not held by the result model, and,
    /// like a list or a set, not hashable.
    Other,
}

#[derive(Debug, Clone, Copy)]
enum Pending {
    Seq(usize, usize),
    Map(usize, usize),
    Set(usize),
    OMap(usize),
}

// ---------------------------------------------------------------------------
// The loader: reader + scanner + parser + composer + constructor
// ---------------------------------------------------------------------------

struct Loader {
    // reader
    buf: Vec<char>,
    ptr: usize,
    index: usize,
    line: usize,
    col: usize,
    overrun: Cell<bool>,

    // scanner
    done: bool,
    flow_level: usize,
    tokens: VecDeque<Token>,
    tokens_taken: usize,
    indent: i64,
    indents: Vec<i64>,
    allow_simple: bool,
    /// possible_simple_keys, in dict order.
    possible: Vec<(usize, PossibleKey)>,

    /// The first value the result model does not hold. It is refused only
    /// when the whole load ends without an error, since a later error is
    /// what safe_load raises.
    unmodeled: Option<String>,

    // parser
    current: Option<Event>,
    tag_handles: HashMap<String, String>,
    states: Vec<St>,
    marks: Vec<Mark>,
    state: Option<St>,

    // composer
    nodes: Vec<Node>,
    anchors: HashMap<String, usize>,
    composing: HashSet<usize>,
    depth: usize,

    // constructor
    built: HashMap<usize, CVal>,
    in_progress: HashSet<usize>,
    pending: Vec<Pending>,
    seqs: Vec<Vec<CVal>>,
    maps: Vec<Vec<(CVal, CVal)>>,
}

fn default_tags() -> HashMap<String, String> {
    HashMap::from([("!".to_string(), "!".to_string()), ("!!".to_string(), "tag:yaml.org,2002:".to_string())])
}

impl Loader {
    fn new(text: &str) -> Loader {
        let mut buf: Vec<char> = text.chars().collect();
        buf.push('\0');
        let mut l = Loader {
            buf,
            ptr: 0,
            index: 0,
            line: 0,
            col: 0,
            overrun: Cell::new(false),
            done: false,
            flow_level: 0,
            tokens: VecDeque::new(),
            tokens_taken: 0,
            indent: -1,
            indents: vec![],
            allow_simple: true,
            possible: vec![],
            unmodeled: None,
            current: None,
            tag_handles: HashMap::new(),
            states: vec![],
            marks: vec![],
            state: Some(St::StreamStart),
            nodes: vec![],
            anchors: HashMap::new(),
            composing: HashSet::new(),
            depth: 0,
            built: HashMap::new(),
            in_progress: HashSet::new(),
            pending: vec![],
            seqs: vec![],
            maps: vec![],
        };
        let m = l.mark();
        l.tokens.push_back(Token::new(Tk::StreamStart, m, m));
        l
    }

    fn load_all(&mut self) -> R<Val> {
        let Some(n) = self.get_single_node()? else {
            return Ok(Val::Null);
        };
        let data = self.document(n)?;
        if let Some(why) = self.unmodeled.clone() {
            return unsupported(&why);
        }
        self.finish(&data, 0)
    }

    // --- errors -----------------------------------------------------------------

    fn snippet(&self, m: &Mark) -> String {
        let (indent, max_length) = (4usize, 75.0f64);
        let mut head = "";
        let mut start = m.ptr;
        while start > 0 && !is_break_z(self.buf[start - 1]) {
            start -= 1;
            if (m.ptr - start) as f64 > max_length / 2.0 - 1.0 {
                head = " ... ";
                start += 5;
                break;
            }
        }
        let mut tail = "";
        let mut end = m.ptr;
        while end < self.buf.len() && !is_break_z(self.buf[end]) {
            end += 1;
            if (end - m.ptr) as f64 > max_length / 2.0 - 1.0 {
                tail = " ... ";
                end -= 5;
                break;
            }
        }
        let body: String = self.buf[start..end].iter().collect();
        format!(
            "{}{head}{body}{tail}\n{}^",
            " ".repeat(indent),
            " ".repeat(indent + m.ptr - start + head.chars().count())
        )
    }

    fn mark_str(&self, m: &Mark) -> String {
        format!("  in \"<unicode string>\", line {}, column {}:\n{}", m.line + 1, m.column + 1, self.snippet(m))
    }

    /// MarkedYAMLError(context, context_mark, problem, problem_mark) raised
    /// as `kind`; an empty context is None.
    fn raise(&self, kind: &'static str, context: &str, cmark: Option<Mark>, problem: &str, pmark: Option<Mark>) -> Fail {
        let mut lines = vec![];
        if !context.is_empty() {
            lines.push(context.to_string());
        }
        if let Some(c) = cmark {
            let same = pmark.map(|p| p.line == c.line && p.column == c.column).unwrap_or(false);
            if problem.is_empty() || pmark.is_none() || !same {
                lines.push(self.mark_str(&c));
            }
        }
        if !problem.is_empty() {
            lines.push(problem.to_string());
        }
        if let Some(p) = pmark {
            lines.push(self.mark_str(&p));
        }
        exc(kind, lines.join("\n"))
    }

    // --- reader -------------------------------------------------------------------

    fn peek(&self, i: usize) -> char {
        match self.buf.get(self.ptr + i) {
            Some(&c) => c,
            None => {
                self.overrun.set(true);
                '\0'
            }
        }
    }

    fn prefix(&self, n: usize) -> String {
        let end = (self.ptr + n).min(self.buf.len());
        self.buf[self.ptr.min(end)..end].iter().collect()
    }

    fn forward(&mut self, n: usize) {
        for _ in 0..n {
            let Some(&ch) = self.buf.get(self.ptr) else {
                self.overrun.set(true);
                return;
            };
            self.ptr += 1;
            self.index += 1;
            let next = self.buf.get(self.ptr).copied().unwrap_or('\0');
            if matches!(ch, '\n' | '\u{85}' | '\u{2028}' | '\u{2029}') || (ch == '\r' && next != '\n') {
                self.line += 1;
                self.col = 0;
            } else if ch != '\u{feff}' {
                self.col += 1;
            }
        }
    }

    fn mark(&self) -> Mark {
        Mark { line: self.line, column: self.col, ptr: self.ptr }
    }

    // --- scanner public -------------------------------------------------------------

    fn check_token(&mut self, kinds: &[Tk]) -> R<bool> {
        while self.need_more()? {
            self.fetch_more()?;
        }
        if let Some(t) = self.tokens.front() {
            if kinds.is_empty() || kinds.contains(&t.kind) {
                return Ok(true);
            }
        }
        Ok(false)
    }

    fn peek_token(&mut self) -> R<Token> {
        while self.need_more()? {
            self.fetch_more()?;
        }
        match self.tokens.front() {
            Some(t) => Ok(t.clone()),
            None => unsupported("a token past the end of the stream"),
        }
    }

    fn get_token(&mut self) -> R<Token> {
        while self.need_more()? {
            self.fetch_more()?;
        }
        match self.tokens.pop_front() {
            Some(t) => {
                self.tokens_taken += 1;
                Ok(t)
            }
            None => unsupported("a token past the end of the stream"),
        }
    }

    fn need_more(&mut self) -> R<bool> {
        if self.done {
            return Ok(false);
        }
        if self.tokens.is_empty() {
            return Ok(true);
        }
        self.stale_keys()?;
        Ok(self.next_possible() == Some(self.tokens_taken))
    }

    fn fetch_more(&mut self) -> R<()> {
        self.scan_to_next_token();
        self.stale_keys()?;
        self.unwind_indent(self.col as i64);
        let ch = self.peek(0);
        match ch {
            '\0' => self.fetch_stream_end(),
            '%' if self.col == 0 => self.fetch_directive(),
            '-' if self.check_document("---") => self.fetch_doc_indicator(Tk::DocStart),
            '.' if self.check_document("...") => self.fetch_doc_indicator(Tk::DocEnd),
            '[' => self.fetch_flow_start(Tk::FlowSeqStart),
            '{' => self.fetch_flow_start(Tk::FlowMapStart),
            ']' => self.fetch_flow_end(Tk::FlowSeqEnd),
            '}' => self.fetch_flow_end(Tk::FlowMapEnd),
            ',' => self.fetch_flow_entry(),
            '-' if is_in(self.peek(1), BLANKZ) => self.fetch_block_entry(),
            '?' if self.flow_level > 0 || is_in(self.peek(1), BLANKZ) => self.fetch_key(),
            ':' if self.flow_level > 0 || is_in(self.peek(1), BLANKZ) => self.fetch_value(),
            '*' => self.fetch_anchor(Tk::Alias),
            '&' => self.fetch_anchor(Tk::Anchor),
            '!' => self.fetch_tag(),
            '|' if self.flow_level == 0 => self.fetch_block_scalar('|'),
            '>' if self.flow_level == 0 => self.fetch_block_scalar('>'),
            '\'' => self.fetch_flow_scalar('\''),
            '"' => self.fetch_flow_scalar('"'),
            _ if self.check_plain() => self.fetch_plain(),
            _ => Err(self.raise(
                "ScannerError",
                "while scanning for the next token",
                None,
                &format!("found character {} that cannot start any token", rr(ch)),
                Some(self.mark()),
            )),
        }
    }

    fn check_document(&self, ind: &str) -> bool {
        self.col == 0 && self.prefix(3) == ind && is_in(self.peek(3), BLANKZ)
    }

    // --- simple keys ------------------------------------------------------------------

    fn next_possible(&self) -> Option<usize> {
        self.possible.iter().map(|(_, k)| k.token_number).min()
    }

    fn del_possible(&mut self, level: usize) {
        self.possible.retain(|(lv, _)| *lv != level);
    }

    fn possible_at(&self, level: usize) -> Option<PossibleKey> {
        self.possible.iter().find(|(lv, _)| *lv == level).map(|(_, k)| k.clone())
    }

    fn stale_keys(&mut self) -> R<()> {
        for (lv, k) in self.possible.clone() {
            if k.line != self.line || self.index - k.index > 1024 {
                if k.required {
                    return Err(self.raise(
                        "ScannerError",
                        "while scanning a simple key",
                        Some(k.mark),
                        "could not find expected ':'",
                        Some(self.mark()),
                    ));
                }
                self.del_possible(lv);
            }
        }
        Ok(())
    }

    fn save_simple_key(&mut self) -> R<()> {
        let required = self.flow_level == 0 && self.indent == self.col as i64;
        if self.allow_simple {
            self.remove_simple_key()?;
            let k = PossibleKey {
                token_number: self.tokens_taken + self.tokens.len(),
                required,
                index: self.index,
                line: self.line,
                column: self.col,
                mark: self.mark(),
            };
            self.possible.push((self.flow_level, k));
        }
        Ok(())
    }

    fn remove_simple_key(&mut self) -> R<()> {
        if let Some(k) = self.possible_at(self.flow_level) {
            if k.required {
                return Err(self.raise(
                    "ScannerError",
                    "while scanning a simple key",
                    Some(k.mark),
                    "could not find expected ':'",
                    Some(self.mark()),
                ));
            }
            self.del_possible(self.flow_level);
        }
        Ok(())
    }

    // --- indentation -------------------------------------------------------------------

    fn unwind_indent(&mut self, column: i64) {
        if self.flow_level > 0 {
            return;
        }
        while self.indent > column {
            let m = self.mark();
            self.indent = self.indents.pop().unwrap_or(-1);
            self.tokens.push_back(Token::new(Tk::BlockEnd, m, m));
        }
    }

    fn add_indent(&mut self, column: usize) -> bool {
        if self.indent < column as i64 {
            self.indents.push(self.indent);
            self.indent = column as i64;
            return true;
        }
        false
    }

    // --- fetchers --------------------------------------------------------------------

    fn simple(&mut self, kind: Tk) -> Token {
        let s = self.mark();
        self.forward(1);
        Token::new(kind, s, self.mark())
    }

    fn fetch_stream_end(&mut self) -> R<()> {
        self.unwind_indent(-1);
        self.remove_simple_key()?;
        self.allow_simple = false;
        self.possible.clear();
        let m = self.mark();
        self.tokens.push_back(Token::new(Tk::StreamEnd, m, m));
        self.done = true;
        Ok(())
    }

    fn fetch_directive(&mut self) -> R<()> {
        self.unwind_indent(-1);
        self.remove_simple_key()?;
        self.allow_simple = false;
        let t = self.scan_directive()?;
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_doc_indicator(&mut self, kind: Tk) -> R<()> {
        self.unwind_indent(-1);
        self.remove_simple_key()?;
        self.allow_simple = false;
        let s = self.mark();
        self.forward(3);
        self.tokens.push_back(Token::new(kind, s, self.mark()));
        Ok(())
    }

    fn fetch_flow_start(&mut self, kind: Tk) -> R<()> {
        self.save_simple_key()?;
        self.flow_level += 1;
        self.allow_simple = true;
        let t = self.simple(kind);
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_flow_end(&mut self, kind: Tk) -> R<()> {
        self.remove_simple_key()?;
        // Python's flow_level goes below 0 on an unmatched close; the next
        // token raises or ends the stream before it is read again.
        self.flow_level = self.flow_level.saturating_sub(1);
        self.allow_simple = false;
        let t = self.simple(kind);
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_flow_entry(&mut self) -> R<()> {
        self.allow_simple = true;
        self.remove_simple_key()?;
        let t = self.simple(Tk::FlowEntry);
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_block_entry(&mut self) -> R<()> {
        if self.flow_level == 0 {
            if !self.allow_simple {
                return Err(self.raise(
                    "ScannerError",
                    "",
                    None,
                    "sequence entries are not allowed here",
                    Some(self.mark()),
                ));
            }
            if self.add_indent(self.col) {
                let m = self.mark();
                self.tokens.push_back(Token::new(Tk::BlockSeqStart, m, m));
            }
        }
        self.allow_simple = true;
        self.remove_simple_key()?;
        let t = self.simple(Tk::BlockEntry);
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_key(&mut self) -> R<()> {
        if self.flow_level == 0 {
            if !self.allow_simple {
                return Err(self.raise("ScannerError", "", None, "mapping keys are not allowed here", Some(self.mark())));
            }
            if self.add_indent(self.col) {
                let m = self.mark();
                self.tokens.push_back(Token::new(Tk::BlockMapStart, m, m));
            }
        }
        self.allow_simple = self.flow_level == 0;
        self.remove_simple_key()?;
        let t = self.simple(Tk::Key);
        self.tokens.push_back(t);
        Ok(())
    }

    fn insert_token(&mut self, at: usize, t: Token) {
        self.tokens.insert(at, t);
    }

    fn fetch_value(&mut self) -> R<()> {
        if let Some(k) = self.possible_at(self.flow_level) {
            self.del_possible(self.flow_level);
            self.insert_token(k.token_number - self.tokens_taken, Token::new(Tk::Key, k.mark, k.mark));
            if self.flow_level == 0 && self.add_indent(k.column) {
                self.insert_token(k.token_number - self.tokens_taken, Token::new(Tk::BlockMapStart, k.mark, k.mark));
            }
            self.allow_simple = false;
        } else {
            if self.flow_level == 0 {
                if !self.allow_simple {
                    return Err(self.raise(
                        "ScannerError",
                        "",
                        None,
                        "mapping values are not allowed here",
                        Some(self.mark()),
                    ));
                }
                if self.add_indent(self.col) {
                    let m = self.mark();
                    self.tokens.push_back(Token::new(Tk::BlockMapStart, m, m));
                }
            }
            self.allow_simple = self.flow_level == 0;
            self.remove_simple_key()?;
        }
        let t = self.simple(Tk::Value);
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_anchor(&mut self, kind: Tk) -> R<()> {
        self.save_simple_key()?;
        self.allow_simple = false;
        let t = self.scan_anchor(kind)?;
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_tag(&mut self) -> R<()> {
        self.save_simple_key()?;
        self.allow_simple = false;
        let t = self.scan_tag()?;
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_block_scalar(&mut self, style: char) -> R<()> {
        self.allow_simple = true;
        self.remove_simple_key()?;
        let t = self.scan_block_scalar(style)?;
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_flow_scalar(&mut self, style: char) -> R<()> {
        self.save_simple_key()?;
        self.allow_simple = false;
        let t = self.scan_flow_scalar(style)?;
        self.tokens.push_back(t);
        Ok(())
    }

    fn fetch_plain(&mut self) -> R<()> {
        self.save_simple_key()?;
        self.allow_simple = false;
        let t = self.scan_plain();
        self.tokens.push_back(t);
        Ok(())
    }

    fn check_plain(&self) -> bool {
        let ch = self.peek(0);
        !is_in(ch, "\0 \t\r\n\u{85}\u{2028}\u{2029}-?:,[]{}#&*!|>'\"%@`")
            || (!is_in(self.peek(1), BLANKZ) && (ch == '-' || (self.flow_level == 0 && is_in(ch, "?:"))))
    }

    // --- scanners -------------------------------------------------------------------

    fn scan_to_next_token(&mut self) {
        if self.index == 0 && self.peek(0) == '\u{feff}' {
            self.forward(1);
        }
        loop {
            while self.peek(0) == ' ' {
                self.forward(1);
            }
            if self.peek(0) == '#' {
                while !is_in(self.peek(0), BREAKZ) {
                    self.forward(1);
                }
            }
            if !self.scan_line_break().is_empty() {
                if self.flow_level == 0 {
                    self.allow_simple = true;
                }
            } else {
                return;
            }
        }
    }

    fn scan_directive(&mut self) -> R<Token> {
        let start = self.mark();
        self.forward(1);
        let name = self.scan_directive_name(start)?;
        let mut t = Token::new(Tk::Directive, start, start);
        t.value = name.clone();
        match name.as_str() {
            "YAML" => {
                t.major = self.scan_yaml_directive_value(start)?;
                t.end = self.mark();
            }
            "TAG" => {
                let (h, p) = self.scan_tag_directive_value(start)?;
                t.tag_handle = h;
                t.tag_prefix = p;
                t.end = self.mark();
            }
            _ => {
                t.end = self.mark();
                while !is_in(self.peek(0), BREAKZ) {
                    self.forward(1);
                }
            }
        }
        self.scan_directive_ignored_line(start)?;
        Ok(t)
    }

    fn directive_err(&self, start: Mark, problem: String) -> Fail {
        self.raise("ScannerError", "while scanning a directive", Some(start), &problem, Some(self.mark()))
    }

    fn scan_directive_name(&mut self, start: Mark) -> R<String> {
        let mut n = 0;
        while is_alnum(self.peek(n)) {
            n += 1;
        }
        if n == 0 {
            return Err(self.directive_err(
                start,
                format!("expected alphabetic or numeric character, but found {}", rr(self.peek(n))),
            ));
        }
        let v = self.prefix(n);
        self.forward(n);
        if !is_in(self.peek(0), "\0 \r\n\u{85}\u{2028}\u{2029}") {
            return Err(self.directive_err(
                start,
                format!("expected alphabetic or numeric character, but found {}", rr(self.peek(0))),
            ));
        }
        Ok(v)
    }

    fn scan_yaml_directive_value(&mut self, start: Mark) -> R<i64> {
        while self.peek(0) == ' ' {
            self.forward(1);
        }
        let major = self.scan_yaml_directive_number(start)?;
        if self.peek(0) != '.' {
            return Err(self.directive_err(start, format!("expected a digit or '.', but found {}", rr(self.peek(0)))));
        }
        self.forward(1);
        self.scan_yaml_directive_number(start)?;
        if !is_in(self.peek(0), "\0 \r\n\u{85}\u{2028}\u{2029}") {
            return Err(self.directive_err(start, format!("expected a digit or ' ', but found {}", rr(self.peek(0)))));
        }
        Ok(major)
    }

    fn scan_yaml_directive_number(&mut self, start: Mark) -> R<i64> {
        let ch = self.peek(0);
        if !ch.is_ascii_digit() {
            return Err(self.directive_err(start, format!("expected a digit, but found {}", rr(ch))));
        }
        let mut n = 0;
        while self.peek(n).is_ascii_digit() {
            n += 1;
        }
        let Ok(v) = self.prefix(n).parse::<i64>() else {
            return unsupported("a %YAML version number past an int");
        };
        self.forward(n);
        Ok(v)
    }

    fn scan_tag_directive_value(&mut self, start: Mark) -> R<(String, String)> {
        while self.peek(0) == ' ' {
            self.forward(1);
        }
        let handle = self.scan_tag_handle("directive", start)?;
        if self.peek(0) != ' ' {
            return Err(self.directive_err(start, format!("expected ' ', but found {}", rr(self.peek(0)))));
        }
        while self.peek(0) == ' ' {
            self.forward(1);
        }
        let prefix = self.scan_tag_uri("directive", start)?;
        if !is_in(self.peek(0), "\0 \r\n\u{85}\u{2028}\u{2029}") {
            return Err(self.directive_err(start, format!("expected ' ', but found {}", rr(self.peek(0)))));
        }
        Ok((handle, prefix))
    }

    fn scan_directive_ignored_line(&mut self, start: Mark) -> R<()> {
        while self.peek(0) == ' ' {
            self.forward(1);
        }
        if self.peek(0) == '#' {
            while !is_in(self.peek(0), BREAKZ) {
                self.forward(1);
            }
        }
        if !is_in(self.peek(0), BREAKZ) {
            return Err(self.directive_err(
                start,
                format!("expected a comment or a line break, but found {}", rr(self.peek(0))),
            ));
        }
        self.scan_line_break();
        Ok(())
    }

    fn scan_anchor(&mut self, kind: Tk) -> R<Token> {
        let start = self.mark();
        let name = if self.peek(0) == '*' { "alias" } else { "anchor" };
        self.forward(1);
        let mut n = 0;
        while is_alnum(self.peek(n)) {
            n += 1;
        }
        let context = format!("while scanning an {name}");
        if n == 0 {
            return Err(self.raise(
                "ScannerError",
                &context,
                Some(start),
                &format!("expected alphabetic or numeric character, but found {}", rr(self.peek(n))),
                Some(self.mark()),
            ));
        }
        let v = self.prefix(n);
        self.forward(n);
        if !is_in(self.peek(0), "\0 \t\r\n\u{85}\u{2028}\u{2029}?:,]}%@`") {
            return Err(self.raise(
                "ScannerError",
                &context,
                Some(start),
                &format!("expected alphabetic or numeric character, but found {}", rr(self.peek(0))),
                Some(self.mark()),
            ));
        }
        let mut t = Token::new(kind, start, self.mark());
        t.value = v;
        Ok(t)
    }

    fn scan_tag(&mut self) -> R<Token> {
        let start = self.mark();
        let mut ch = self.peek(1);
        let handle: Option<String>;
        let suffix: String;
        if ch == '<' {
            handle = None;
            self.forward(2);
            suffix = self.scan_tag_uri("tag", start)?;
            if self.peek(0) != '>' {
                return Err(self.raise(
                    "ScannerError",
                    "while parsing a tag",
                    Some(start),
                    &format!("expected '>', but found {}", rr(self.peek(0))),
                    Some(self.mark()),
                ));
            }
            self.forward(1);
        } else if is_in(ch, BLANKZ) {
            handle = None;
            suffix = "!".into();
            self.forward(1);
        } else {
            let mut n = 1;
            let mut use_handle = false;
            while !is_in(ch, "\0 \r\n\u{85}\u{2028}\u{2029}") {
                if ch == '!' {
                    use_handle = true;
                    break;
                }
                n += 1;
                ch = self.peek(n);
            }
            let h = if use_handle {
                self.scan_tag_handle("tag", start)?
            } else {
                self.forward(1);
                "!".to_string()
            };
            handle = Some(h);
            suffix = self.scan_tag_uri("tag", start)?;
        }
        if !is_in(self.peek(0), "\0 \r\n\u{85}\u{2028}\u{2029}") {
            return Err(self.raise(
                "ScannerError",
                "while scanning a tag",
                Some(start),
                &format!("expected ' ', but found {}", rr(self.peek(0))),
                Some(self.mark()),
            ));
        }
        let mut t = Token::new(Tk::Tag, start, self.mark());
        t.handle = handle;
        t.suffix = suffix;
        Ok(t)
    }

    fn scan_block_scalar(&mut self, style: char) -> R<Token> {
        let folded = style == '>';
        let mut chunks: Vec<String> = vec![];
        let start = self.mark();
        self.forward(1);
        let (chomping, increment) = self.scan_block_scalar_indicators(start)?;
        self.scan_block_scalar_ignored_line(start)?;
        let min_indent = (self.indent + 1).max(1) as usize;
        let mut brks: Vec<String>;
        let mut end: Mark;
        let indent: usize;
        if increment == 0 {
            let (b, max_indent, e) = self.scan_block_scalar_indentation();
            brks = b;
            end = e;
            indent = min_indent.max(max_indent);
        } else {
            indent = min_indent + increment - 1;
            let (b, e) = self.scan_block_scalar_breaks(indent);
            brks = b;
            end = e;
        }
        let mut line_break = String::new();
        while self.col == indent && self.peek(0) != '\0' {
            chunks.append(&mut brks.clone());
            let leading_non_space = !is_in(self.peek(0), " \t");
            let mut n = 0;
            while !is_in(self.peek(n), BREAKZ) {
                n += 1;
            }
            chunks.push(self.prefix(n));
            self.forward(n);
            line_break = self.scan_line_break();
            let (b, e) = self.scan_block_scalar_breaks(indent);
            brks = b;
            end = e;
            if self.col == indent && self.peek(0) != '\0' {
                if folded && line_break == "\n" && leading_non_space && !is_in(self.peek(0), " \t") {
                    if brks.is_empty() {
                        chunks.push(" ".into());
                    }
                } else {
                    chunks.push(line_break.clone());
                }
            } else {
                break;
            }
        }
        if chomping != 'F' {
            chunks.push(line_break);
        }
        if chomping == 'T' {
            chunks.append(&mut brks);
        }
        let mut t = Token::new(Tk::Scalar, start, end);
        t.value = chunks.concat();
        Ok(t)
    }

    /// Chomping is 'T' (True), 'F' (False) or '\0' (None); increment 0 is
    /// None.
    fn scan_block_scalar_indicators(&mut self, start: Mark) -> R<(char, usize)> {
        let mut chomping = '\0';
        let mut increment = 0usize;
        let zero = |l: &Loader| {
            l.raise(
                "ScannerError",
                "while scanning a block scalar",
                Some(start),
                "expected indentation indicator in the range 1-9, but found 0",
                Some(l.mark()),
            )
        };
        let mut ch = self.peek(0);
        if ch == '+' || ch == '-' {
            chomping = if ch == '+' { 'T' } else { 'F' };
            self.forward(1);
            ch = self.peek(0);
            if ch.is_ascii_digit() {
                increment = (ch as u8 - b'0') as usize;
                if increment == 0 {
                    return Err(zero(self));
                }
                self.forward(1);
            }
        } else if ch.is_ascii_digit() {
            increment = (ch as u8 - b'0') as usize;
            if increment == 0 {
                return Err(zero(self));
            }
            self.forward(1);
            ch = self.peek(0);
            if ch == '+' || ch == '-' {
                chomping = if ch == '+' { 'T' } else { 'F' };
                self.forward(1);
            }
        }
        if !is_in(self.peek(0), "\0 \r\n\u{85}\u{2028}\u{2029}") {
            return Err(self.raise(
                "ScannerError",
                "while scanning a block scalar",
                Some(start),
                &format!("expected chomping or indentation indicators, but found {}", rr(self.peek(0))),
                Some(self.mark()),
            ));
        }
        Ok((chomping, increment))
    }

    fn scan_block_scalar_ignored_line(&mut self, start: Mark) -> R<()> {
        while self.peek(0) == ' ' {
            self.forward(1);
        }
        if self.peek(0) == '#' {
            while !is_in(self.peek(0), BREAKZ) {
                self.forward(1);
            }
        }
        if !is_in(self.peek(0), BREAKZ) {
            return Err(self.raise(
                "ScannerError",
                "while scanning a block scalar",
                Some(start),
                &format!("expected a comment or a line break, but found {}", rr(self.peek(0))),
                Some(self.mark()),
            ));
        }
        self.scan_line_break();
        Ok(())
    }

    fn scan_block_scalar_indentation(&mut self) -> (Vec<String>, usize, Mark) {
        let mut chunks = vec![];
        let mut max_indent = 0;
        let mut end = self.mark();
        while is_in(self.peek(0), " \r\n\u{85}\u{2028}\u{2029}") && self.peek(0) != '\0' {
            if self.peek(0) != ' ' {
                chunks.push(self.scan_line_break());
                end = self.mark();
            } else {
                self.forward(1);
                if self.col > max_indent {
                    max_indent = self.col;
                }
            }
        }
        (chunks, max_indent, end)
    }

    fn scan_block_scalar_breaks(&mut self, indent: usize) -> (Vec<String>, Mark) {
        let mut chunks = vec![];
        let mut end = self.mark();
        while self.col < indent && self.peek(0) == ' ' {
            self.forward(1);
        }
        while is_in(self.peek(0), BREAKS) && self.peek(0) != '\0' {
            chunks.push(self.scan_line_break());
            end = self.mark();
            while self.col < indent && self.peek(0) == ' ' {
                self.forward(1);
            }
        }
        (chunks, end)
    }

    fn scan_flow_scalar(&mut self, style: char) -> R<Token> {
        let double = style == '"';
        let mut chunks: Vec<String> = vec![];
        let start = self.mark();
        let quote = self.peek(0);
        self.forward(1);
        chunks.append(&mut self.scan_flow_scalar_non_spaces(double, start)?);
        while self.peek(0) != quote {
            chunks.append(&mut self.scan_flow_scalar_spaces(double, start)?);
            chunks.append(&mut self.scan_flow_scalar_non_spaces(double, start)?);
        }
        self.forward(1);
        let mut t = Token::new(Tk::Scalar, start, self.mark());
        t.value = chunks.concat();
        Ok(t)
    }

    fn scan_flow_scalar_non_spaces(&mut self, double: bool, start: Mark) -> R<Vec<String>> {
        let mut chunks = vec![];
        loop {
            let mut n = 0;
            while !is_in(self.peek(n), "'\"\\\0 \t\r\n\u{85}\u{2028}\u{2029}") {
                n += 1;
            }
            if n > 0 {
                chunks.push(self.prefix(n));
                self.forward(n);
            }
            let ch = self.peek(0);
            if !double && ch == '\'' && self.peek(1) == '\'' {
                chunks.push("'".into());
                self.forward(2);
            } else if (double && ch == '\'') || (!double && (ch == '"' || ch == '\\')) {
                chunks.push(ch.to_string());
                self.forward(1);
            } else if double && ch == '\\' {
                self.forward(1);
                let ch = self.peek(0);
                let rep = match ch {
                    '0' => Some("\0"),
                    'a' => Some("\x07"),
                    'b' => Some("\x08"),
                    't' | '\t' => Some("\t"),
                    'n' => Some("\n"),
                    'v' => Some("\x0b"),
                    'f' => Some("\x0c"),
                    'r' => Some("\r"),
                    'e' => Some("\x1b"),
                    ' ' => Some(" "),
                    '"' => Some("\""),
                    '\\' => Some("\\"),
                    '/' => Some("/"),
                    'N' => Some("\u{85}"),
                    '_' => Some("\u{a0}"),
                    'L' => Some("\u{2028}"),
                    'P' => Some("\u{2029}"),
                    _ => None,
                };
                let width = match ch {
                    'x' => 2,
                    'u' => 4,
                    'U' => 8,
                    _ => 0,
                };
                if let Some(rep) = rep {
                    chunks.push(rep.into());
                    self.forward(1);
                } else if width > 0 {
                    self.forward(1);
                    for k in 0..width {
                        if !is_in(self.peek(k), HEX) || self.peek(k) == '\0' {
                            return Err(self.raise(
                                "ScannerError",
                                "while scanning a double-quoted scalar",
                                Some(start),
                                &format!(
                                    "expected escape sequence of {width} hexadecimal numbers, but found {}",
                                    rr(self.peek(k))
                                ),
                                Some(self.mark()),
                            ));
                        }
                    }
                    let code = u64::from_str_radix(&self.prefix(width), 16).unwrap_or(u64::MAX);
                    if code > 0x10ffff {
                        // chr() raises ValueError: not a YAML error.
                        return Err(exc("ValueError", "chr() arg not in range(0x110000)"));
                    }
                    let Some(c) = char::from_u32(code as u32) else {
                        return unsupported("an escape for a lone surrogate");
                    };
                    chunks.push(c.to_string());
                    self.forward(width);
                } else if is_in(ch, BREAKS) && ch != '\0' {
                    self.scan_line_break();
                    chunks.append(&mut self.scan_flow_scalar_breaks(start)?);
                } else {
                    return Err(self.raise(
                        "ScannerError",
                        "while scanning a double-quoted scalar",
                        Some(start),
                        &format!("found unknown escape character {}", rr(ch)),
                        Some(self.mark()),
                    ));
                }
            } else {
                return Ok(chunks);
            }
        }
    }

    fn scan_flow_scalar_spaces(&mut self, _double: bool, start: Mark) -> R<Vec<String>> {
        let mut chunks = vec![];
        let mut n = 0;
        while is_in(self.peek(n), " \t") && self.peek(n) != '\0' {
            n += 1;
        }
        let ws = self.prefix(n);
        self.forward(n);
        let ch = self.peek(0);
        if ch == '\0' {
            return Err(self.raise(
                "ScannerError",
                "while scanning a quoted scalar",
                Some(start),
                "found unexpected end of stream",
                Some(self.mark()),
            ));
        } else if is_in(ch, BREAKS) {
            let line_break = self.scan_line_break();
            let mut brks = self.scan_flow_scalar_breaks(start)?;
            if line_break != "\n" {
                chunks.push(line_break);
            } else if brks.is_empty() {
                chunks.push(" ".into());
            }
            chunks.append(&mut brks);
        } else {
            chunks.push(ws);
        }
        Ok(chunks)
    }

    fn scan_flow_scalar_breaks(&mut self, start: Mark) -> R<Vec<String>> {
        let mut chunks = vec![];
        loop {
            let p = self.prefix(3);
            if (p == "---" || p == "...") && is_in(self.peek(3), BLANKZ) {
                return Err(self.raise(
                    "ScannerError",
                    "while scanning a quoted scalar",
                    Some(start),
                    "found unexpected document separator",
                    Some(self.mark()),
                ));
            }
            while is_in(self.peek(0), " \t") && self.peek(0) != '\0' {
                self.forward(1);
            }
            if is_in(self.peek(0), BREAKS) && self.peek(0) != '\0' {
                chunks.push(self.scan_line_break());
            } else {
                return Ok(chunks);
            }
        }
    }

    fn scan_plain(&mut self) -> Token {
        let mut chunks: Vec<String> = vec![];
        let start = self.mark();
        let mut end = start;
        let indent = self.indent + 1;
        let mut spaces: Vec<String> = vec![];
        loop {
            let mut n = 0;
            if self.peek(0) == '#' {
                break;
            }
            loop {
                let ch = self.peek(n);
                // Python reads the character after a ':' only for a ':'.
                let colon_ends = ch == ':'
                    && (is_in(self.peek(n + 1), BLANKZ) || (self.flow_level > 0 && is_in(self.peek(n + 1), ",[]{}")));
                if is_in(ch, BLANKZ) || colon_ends || (self.flow_level > 0 && is_in(ch, ",?[]{}")) {
                    break;
                }
                n += 1;
            }
            if n == 0 {
                break;
            }
            self.allow_simple = false;
            chunks.append(&mut spaces);
            chunks.push(self.prefix(n));
            self.forward(n);
            end = self.mark();
            match self.scan_plain_spaces() {
                Some(s) => spaces = s,
                None => break,
            }
            if spaces.is_empty() || self.peek(0) == '#' || (self.flow_level == 0 && (self.col as i64) < indent) {
                break;
            }
        }
        let mut t = Token::new(Tk::Scalar, start, end);
        t.value = chunks.concat();
        t.plain = true;
        t
    }

    /// None where Python returns None.
    fn scan_plain_spaces(&mut self) -> Option<Vec<String>> {
        let mut chunks = vec![];
        let mut n = 0;
        while self.peek(n) == ' ' {
            n += 1;
        }
        let ws = self.prefix(n);
        self.forward(n);
        let ch = self.peek(0);
        if is_in(ch, BREAKS) && ch != '\0' {
            let line_break = self.scan_line_break();
            self.allow_simple = true;
            let sep = |l: &Loader| {
                let p = l.prefix(3);
                (p == "---" || p == "...") && is_in(l.peek(3), BLANKZ)
            };
            if sep(self) {
                return None;
            }
            let mut brks = vec![];
            while is_in(self.peek(0), " \r\n\u{85}\u{2028}\u{2029}") && self.peek(0) != '\0' {
                if self.peek(0) == ' ' {
                    self.forward(1);
                } else {
                    brks.push(self.scan_line_break());
                    if sep(self) {
                        return None;
                    }
                }
            }
            if line_break != "\n" {
                chunks.push(line_break);
            } else if brks.is_empty() {
                chunks.push(" ".into());
            }
            chunks.append(&mut brks);
        } else if !ws.is_empty() {
            chunks.push(ws);
        }
        Some(chunks)
    }

    fn scan_tag_handle(&mut self, name: &str, start: Mark) -> R<String> {
        let mut ch = self.peek(0);
        let context = format!("while scanning a {name}");
        if ch != '!' {
            return Err(self.raise(
                "ScannerError",
                &context,
                Some(start),
                &format!("expected '!', but found {}", rr(ch)),
                Some(self.mark()),
            ));
        }
        let mut n = 1;
        ch = self.peek(n);
        if ch != ' ' {
            while is_alnum(ch) {
                n += 1;
                ch = self.peek(n);
            }
            if ch != '!' {
                self.forward(n);
                return Err(self.raise(
                    "ScannerError",
                    &context,
                    Some(start),
                    &format!("expected '!', but found {}", rr(ch)),
                    Some(self.mark()),
                ));
            }
            n += 1;
        }
        let v = self.prefix(n);
        self.forward(n);
        Ok(v)
    }

    fn scan_tag_uri(&mut self, name: &str, start: Mark) -> R<String> {
        let mut chunks: Vec<String> = vec![];
        let mut n = 0;
        let mut ch = self.peek(n);
        while ch.is_ascii() && ch != '\0' && (ALNUM.contains(ch) || URI_EXTRA.contains(ch)) {
            if ch == '%' {
                chunks.push(self.prefix(n));
                self.forward(n);
                n = 0;
                chunks.push(self.scan_uri_escapes(name, start)?);
            } else {
                n += 1;
            }
            ch = self.peek(n);
        }
        if n > 0 {
            chunks.push(self.prefix(n));
            self.forward(n);
        }
        if chunks.is_empty() {
            return Err(self.raise(
                "ScannerError",
                &format!("while parsing a {name}"),
                Some(start),
                &format!("expected URI, but found {}", rr(ch)),
                Some(self.mark()),
            ));
        }
        Ok(chunks.concat())
    }

    fn scan_uri_escapes(&mut self, name: &str, start: Mark) -> R<String> {
        let mut codes: Vec<u8> = vec![];
        while self.peek(0) == '%' {
            self.forward(1);
            for k in 0..2 {
                if !is_in(self.peek(k), HEX) || self.peek(k) == '\0' {
                    return Err(self.raise(
                        "ScannerError",
                        &format!("while scanning a {name}"),
                        Some(start),
                        &format!(
                            "expected URI escape sequence of 2 hexadecimal numbers, but found {}",
                            rr(self.peek(k))
                        ),
                        Some(self.mark()),
                    ));
                }
            }
            codes.push(u8::from_str_radix(&self.prefix(2), 16).unwrap_or(0));
            self.forward(2);
        }
        match String::from_utf8(codes) {
            Ok(s) => Ok(s),
            // The UnicodeDecodeError's words follow; this port does not
            // write them.
            Err(_) => unsupported("a tag URI escape that is not UTF-8"),
        }
    }

    fn scan_line_break(&mut self) -> String {
        let ch = self.peek(0);
        if ch == '\r' || ch == '\n' || ch == '\u{85}' {
            if self.prefix(2) == "\r\n" {
                self.forward(2);
            } else {
                self.forward(1);
            }
            return "\n".into();
        }
        if ch == '\u{2028}' || ch == '\u{2029}' {
            self.forward(1);
            return ch.to_string();
        }
        String::new()
    }

    // ---------------------------------------------------------------------------
    // The parser (yaml/parser.py)
    // ---------------------------------------------------------------------------

    fn fill_event(&mut self) -> R<()> {
        if self.current.is_none() {
            if let Some(st) = self.state {
                let ev = self.run_state(st)?;
                self.current = Some(ev);
            }
        }
        Ok(())
    }

    fn check_event(&mut self, kinds: &[Ev]) -> R<bool> {
        self.fill_event()?;
        Ok(match &self.current {
            Some(e) => kinds.is_empty() || kinds.contains(&e.kind),
            None => false,
        })
    }

    fn peek_event(&mut self) -> R<Event> {
        self.fill_event()?;
        match &self.current {
            Some(e) => Ok(e.clone()),
            None => unsupported("an event past the end of the stream"),
        }
    }

    fn get_event(&mut self) -> R<Event> {
        self.fill_event()?;
        match self.current.take() {
            Some(e) => Ok(e),
            None => unsupported("an event past the end of the stream"),
        }
    }

    fn pop_state(&mut self) -> Option<St> {
        self.states.pop()
    }

    fn run_state(&mut self, st: St) -> R<Event> {
        match st {
            St::StreamStart => self.parse_stream_start(),
            St::ImplicitDocumentStart => self.parse_implicit_document_start(),
            St::DocumentStart => self.parse_document_start(),
            St::DocumentEnd => self.parse_document_end(),
            St::DocumentContent => self.parse_document_content(),
            St::BlockNode => self.parse_node(true, false),
            St::BlockSequenceFirstEntry => self.parse_block_sequence_first_entry(),
            St::BlockSequenceEntry => self.parse_block_sequence_entry(),
            St::IndentlessSequenceEntry => self.parse_indentless_sequence_entry(),
            St::BlockMappingFirstKey => self.parse_block_mapping_first_key(),
            St::BlockMappingKey => self.parse_block_mapping_key(),
            St::BlockMappingValue => self.parse_block_mapping_value(),
            St::FlowSequenceFirstEntry => self.parse_flow_sequence_first_entry(),
            St::FlowSequenceEntry => self.parse_flow_sequence_entry(false),
            St::FlowSequenceEntryMappingKey => self.parse_flow_sequence_entry_mapping_key(),
            St::FlowSequenceEntryMappingValue => self.parse_flow_sequence_entry_mapping_value(),
            St::FlowSequenceEntryMappingEnd => self.parse_flow_sequence_entry_mapping_end(),
            St::FlowMappingFirstKey => self.parse_flow_mapping_first_key(),
            St::FlowMappingKey => self.parse_flow_mapping_key(false),
            St::FlowMappingValue => self.parse_flow_mapping_value(),
            St::FlowMappingEmptyValue => self.parse_flow_mapping_empty_value(),
        }
    }

    fn parse_err(&self, context: &str, cmark: Option<Mark>, problem: String, pmark: Mark) -> Fail {
        self.raise("ParserError", context, cmark, &problem, Some(pmark))
    }

    fn parse_stream_start(&mut self) -> R<Event> {
        let t = self.get_token()?;
        self.state = Some(St::ImplicitDocumentStart);
        Ok(Event::new(Ev::StreamStart, t.start))
    }

    fn parse_implicit_document_start(&mut self) -> R<Event> {
        if !self.check_token(&[Tk::Directive, Tk::DocStart, Tk::StreamEnd])? {
            self.tag_handles = default_tags();
            let t = self.peek_token()?;
            self.states.push(St::DocumentEnd);
            self.state = Some(St::BlockNode);
            return Ok(Event::new(Ev::DocStart, t.start));
        }
        self.parse_document_start()
    }

    fn parse_document_start(&mut self) -> R<Event> {
        while self.check_token(&[Tk::DocEnd])? {
            self.get_token()?;
        }
        if !self.check_token(&[Tk::StreamEnd])? {
            let t = self.peek_token()?;
            let start = t.start;
            self.process_directives()?;
            if !self.check_token(&[Tk::DocStart])? {
                let t = self.peek_token()?;
                return Err(self.parse_err(
                    "",
                    None,
                    format!("expected '<document start>', but found {}", repr(tok_id(t.kind))),
                    t.start,
                ));
            }
            self.get_token()?;
            self.states.push(St::DocumentEnd);
            self.state = Some(St::DocumentContent);
            return Ok(Event::new(Ev::DocStart, start));
        }
        let t = self.get_token()?;
        self.state = None;
        Ok(Event::new(Ev::StreamEnd, t.start))
    }

    fn parse_document_end(&mut self) -> R<Event> {
        let t = self.peek_token()?;
        let start = t.start;
        if self.check_token(&[Tk::DocEnd])? {
            self.get_token()?;
        }
        self.state = Some(St::DocumentStart);
        Ok(Event::new(Ev::DocEnd, start))
    }

    fn parse_document_content(&mut self) -> R<Event> {
        if self.check_token(&[Tk::Directive, Tk::DocStart, Tk::DocEnd, Tk::StreamEnd])? {
            let t = self.peek_token()?;
            let ev = Self::empty_scalar(t.start);
            self.state = self.pop_state();
            return Ok(ev);
        }
        self.parse_node(true, false)
    }

    fn process_directives(&mut self) -> R<()> {
        let mut have_version = false;
        self.tag_handles = HashMap::new();
        while self.check_token(&[Tk::Directive])? {
            let t = self.get_token()?;
            match t.value.as_str() {
                "YAML" => {
                    if have_version {
                        return Err(self.parse_err("", None, "found duplicate YAML directive".into(), t.start));
                    }
                    if t.major != 1 {
                        return Err(self.parse_err(
                            "",
                            None,
                            "found incompatible YAML document (version 1.* is required)".into(),
                            t.start,
                        ));
                    }
                    have_version = true;
                }
                "TAG" => {
                    if self.tag_handles.contains_key(&t.tag_handle) {
                        return Err(self.parse_err(
                            "",
                            None,
                            format!("duplicate tag handle {}", repr(&t.tag_handle)),
                            t.start,
                        ));
                    }
                    self.tag_handles.insert(t.tag_handle.clone(), t.tag_prefix.clone());
                }
                _ => {}
            }
        }
        for (k, v) in default_tags() {
            self.tag_handles.entry(k).or_insert(v);
        }
        Ok(())
    }

    fn parse_node(&mut self, block: bool, indentless_sequence: bool) -> R<Event> {
        if self.check_token(&[Tk::Alias])? {
            let t = self.get_token()?;
            self.state = self.pop_state();
            let mut e = Event::new(Ev::Alias, t.start);
            e.anchor = Some(t.value);
            return Ok(e);
        }
        let mut anchor: Option<String> = None;
        let mut tag: Option<String> = None;
        let mut start: Option<Mark> = None;
        let mut tag_mark: Option<Mark> = None;
        let mut handle: Option<String> = None;
        let mut suffix = String::new();
        let mut has_tag = false;
        if self.check_token(&[Tk::Anchor])? {
            let t = self.get_token()?;
            start = Some(t.start);
            anchor = Some(t.value);
            if self.check_token(&[Tk::Tag])? {
                let t = self.get_token()?;
                tag_mark = Some(t.start);
                handle = t.handle;
                suffix = t.suffix;
                has_tag = true;
            }
        } else if self.check_token(&[Tk::Tag])? {
            let t = self.get_token()?;
            start = Some(t.start);
            tag_mark = Some(t.start);
            handle = t.handle;
            suffix = t.suffix;
            has_tag = true;
            if self.check_token(&[Tk::Anchor])? {
                let t = self.get_token()?;
                anchor = Some(t.value);
            }
        }
        if has_tag {
            let full = match &handle {
                Some(h) => match self.tag_handles.get(h) {
                    Some(pre) => format!("{pre}{suffix}"),
                    None => {
                        return Err(self.parse_err(
                            "while parsing a node",
                            start,
                            format!("found undefined tag handle {}", repr(h)),
                            tag_mark.unwrap_or(self.mark()),
                        ));
                    }
                },
                None => suffix.clone(),
            };
            tag = Some(full);
        }
        let start = match start {
            Some(s) => s,
            None => self.peek_token()?.start,
        };
        let implicit = tag.is_none() || tag.as_deref() == Some("!");
        let mk = |kind: Ev, anchor: &Option<String>, tag: &Option<String>, implicit0: bool| {
            let mut e = Event::new(kind, start);
            e.anchor = anchor.clone();
            e.tag = tag.clone();
            e.implicit0 = implicit0;
            e
        };
        if indentless_sequence && self.check_token(&[Tk::BlockEntry])? {
            self.state = Some(St::IndentlessSequenceEntry);
            return Ok(mk(Ev::SeqStart, &anchor, &tag, implicit));
        }
        if self.check_token(&[Tk::Scalar])? {
            let t = self.get_token()?;
            let i0 = (t.plain && tag.is_none()) || tag.as_deref() == Some("!");
            self.state = self.pop_state();
            let mut e = mk(Ev::Scalar, &anchor, &tag, i0);
            e.value = t.value;
            return Ok(e);
        }
        if self.check_token(&[Tk::FlowSeqStart])? {
            self.state = Some(St::FlowSequenceFirstEntry);
            return Ok(mk(Ev::SeqStart, &anchor, &tag, implicit));
        }
        if self.check_token(&[Tk::FlowMapStart])? {
            self.state = Some(St::FlowMappingFirstKey);
            return Ok(mk(Ev::MapStart, &anchor, &tag, implicit));
        }
        if block && self.check_token(&[Tk::BlockSeqStart])? {
            self.state = Some(St::BlockSequenceFirstEntry);
            return Ok(mk(Ev::SeqStart, &anchor, &tag, implicit));
        }
        if block && self.check_token(&[Tk::BlockMapStart])? {
            self.state = Some(St::BlockMappingFirstKey);
            return Ok(mk(Ev::MapStart, &anchor, &tag, implicit));
        }
        if anchor.is_some() || tag.is_some() {
            self.state = self.pop_state();
            return Ok(mk(Ev::Scalar, &anchor, &tag, implicit));
        }
        let node = if block { "block" } else { "flow" };
        let t = self.peek_token()?;
        Err(self.parse_err(
            &format!("while parsing a {node} node"),
            Some(start),
            format!("expected the node content, but found {}", repr(tok_id(t.kind))),
            t.start,
        ))
    }

    fn last_mark(&self) -> Option<Mark> {
        self.marks.last().copied()
    }

    fn parse_block_sequence_first_entry(&mut self) -> R<Event> {
        let t = self.get_token()?;
        self.marks.push(t.start);
        self.parse_block_sequence_entry()
    }

    fn parse_block_sequence_entry(&mut self) -> R<Event> {
        if self.check_token(&[Tk::BlockEntry])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::BlockEntry, Tk::BlockEnd])? {
                self.states.push(St::BlockSequenceEntry);
                return self.parse_node(true, false);
            }
            self.state = Some(St::BlockSequenceEntry);
            return Ok(Self::empty_scalar(t.end));
        }
        if !self.check_token(&[Tk::BlockEnd])? {
            let t = self.peek_token()?;
            return Err(self.parse_err(
                "while parsing a block collection",
                self.last_mark(),
                format!("expected <block end>, but found {}", repr(tok_id(t.kind))),
                t.start,
            ));
        }
        let t = self.get_token()?;
        self.state = self.pop_state();
        self.marks.pop();
        Ok(Event::new(Ev::SeqEnd, t.start))
    }

    fn parse_indentless_sequence_entry(&mut self) -> R<Event> {
        if self.check_token(&[Tk::BlockEntry])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::BlockEntry, Tk::Key, Tk::Value, Tk::BlockEnd])? {
                self.states.push(St::IndentlessSequenceEntry);
                return self.parse_node(true, false);
            }
            self.state = Some(St::IndentlessSequenceEntry);
            return Ok(Self::empty_scalar(t.end));
        }
        let t = self.peek_token()?;
        self.state = self.pop_state();
        Ok(Event::new(Ev::SeqEnd, t.start))
    }

    fn parse_block_mapping_first_key(&mut self) -> R<Event> {
        let t = self.get_token()?;
        self.marks.push(t.start);
        self.parse_block_mapping_key()
    }

    fn parse_block_mapping_key(&mut self) -> R<Event> {
        if self.check_token(&[Tk::Key])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::Key, Tk::Value, Tk::BlockEnd])? {
                self.states.push(St::BlockMappingValue);
                return self.parse_node(true, true);
            }
            self.state = Some(St::BlockMappingValue);
            return Ok(Self::empty_scalar(t.end));
        }
        if !self.check_token(&[Tk::BlockEnd])? {
            let t = self.peek_token()?;
            return Err(self.parse_err(
                "while parsing a block mapping",
                self.last_mark(),
                format!("expected <block end>, but found {}", repr(tok_id(t.kind))),
                t.start,
            ));
        }
        let t = self.get_token()?;
        self.state = self.pop_state();
        self.marks.pop();
        Ok(Event::new(Ev::MapEnd, t.start))
    }

    fn parse_block_mapping_value(&mut self) -> R<Event> {
        if self.check_token(&[Tk::Value])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::Key, Tk::Value, Tk::BlockEnd])? {
                self.states.push(St::BlockMappingKey);
                return self.parse_node(true, true);
            }
            self.state = Some(St::BlockMappingKey);
            return Ok(Self::empty_scalar(t.end));
        }
        self.state = Some(St::BlockMappingKey);
        let t = self.peek_token()?;
        Ok(Self::empty_scalar(t.start))
    }

    fn parse_flow_sequence_first_entry(&mut self) -> R<Event> {
        let t = self.get_token()?;
        self.marks.push(t.start);
        self.parse_flow_sequence_entry(true)
    }

    fn parse_flow_sequence_entry(&mut self, first: bool) -> R<Event> {
        if !self.check_token(&[Tk::FlowSeqEnd])? {
            if !first {
                if self.check_token(&[Tk::FlowEntry])? {
                    self.get_token()?;
                } else {
                    let t = self.peek_token()?;
                    return Err(self.parse_err(
                        "while parsing a flow sequence",
                        self.last_mark(),
                        format!("expected ',' or ']', but got {}", repr(tok_id(t.kind))),
                        t.start,
                    ));
                }
            }
            if self.check_token(&[Tk::Key])? {
                let t = self.peek_token()?;
                self.state = Some(St::FlowSequenceEntryMappingKey);
                let mut e = Event::new(Ev::MapStart, t.start);
                e.implicit0 = true;
                return Ok(e);
            } else if !self.check_token(&[Tk::FlowSeqEnd])? {
                self.states.push(St::FlowSequenceEntry);
                return self.parse_node(false, false);
            }
        }
        let t = self.get_token()?;
        self.state = self.pop_state();
        self.marks.pop();
        Ok(Event::new(Ev::SeqEnd, t.start))
    }

    fn parse_flow_sequence_entry_mapping_key(&mut self) -> R<Event> {
        let t = self.get_token()?;
        if !self.check_token(&[Tk::Value, Tk::FlowEntry, Tk::FlowSeqEnd])? {
            self.states.push(St::FlowSequenceEntryMappingValue);
            return self.parse_node(false, false);
        }
        self.state = Some(St::FlowSequenceEntryMappingValue);
        Ok(Self::empty_scalar(t.end))
    }

    fn parse_flow_sequence_entry_mapping_value(&mut self) -> R<Event> {
        if self.check_token(&[Tk::Value])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::FlowEntry, Tk::FlowSeqEnd])? {
                self.states.push(St::FlowSequenceEntryMappingEnd);
                return self.parse_node(false, false);
            }
            self.state = Some(St::FlowSequenceEntryMappingEnd);
            return Ok(Self::empty_scalar(t.end));
        }
        self.state = Some(St::FlowSequenceEntryMappingEnd);
        let t = self.peek_token()?;
        Ok(Self::empty_scalar(t.start))
    }

    fn parse_flow_sequence_entry_mapping_end(&mut self) -> R<Event> {
        self.state = Some(St::FlowSequenceEntry);
        let t = self.peek_token()?;
        Ok(Event::new(Ev::MapEnd, t.start))
    }

    fn parse_flow_mapping_first_key(&mut self) -> R<Event> {
        let t = self.get_token()?;
        self.marks.push(t.start);
        self.parse_flow_mapping_key(true)
    }

    fn parse_flow_mapping_key(&mut self, first: bool) -> R<Event> {
        if !self.check_token(&[Tk::FlowMapEnd])? {
            if !first {
                if self.check_token(&[Tk::FlowEntry])? {
                    self.get_token()?;
                } else {
                    let t = self.peek_token()?;
                    return Err(self.parse_err(
                        "while parsing a flow mapping",
                        self.last_mark(),
                        format!("expected ',' or '}}', but got {}", repr(tok_id(t.kind))),
                        t.start,
                    ));
                }
            }
            if self.check_token(&[Tk::Key])? {
                let t = self.get_token()?;
                if !self.check_token(&[Tk::Value, Tk::FlowEntry, Tk::FlowMapEnd])? {
                    self.states.push(St::FlowMappingValue);
                    return self.parse_node(false, false);
                }
                self.state = Some(St::FlowMappingValue);
                return Ok(Self::empty_scalar(t.end));
            } else if !self.check_token(&[Tk::FlowMapEnd])? {
                self.states.push(St::FlowMappingEmptyValue);
                return self.parse_node(false, false);
            }
        }
        let t = self.get_token()?;
        self.state = self.pop_state();
        self.marks.pop();
        Ok(Event::new(Ev::MapEnd, t.start))
    }

    fn parse_flow_mapping_value(&mut self) -> R<Event> {
        if self.check_token(&[Tk::Value])? {
            let t = self.get_token()?;
            if !self.check_token(&[Tk::FlowEntry, Tk::FlowMapEnd])? {
                self.states.push(St::FlowMappingKey);
                return self.parse_node(false, false);
            }
            self.state = Some(St::FlowMappingKey);
            return Ok(Self::empty_scalar(t.end));
        }
        self.state = Some(St::FlowMappingKey);
        let t = self.peek_token()?;
        Ok(Self::empty_scalar(t.start))
    }

    fn parse_flow_mapping_empty_value(&mut self) -> R<Event> {
        self.state = Some(St::FlowMappingKey);
        let t = self.peek_token()?;
        Ok(Self::empty_scalar(t.start))
    }

    fn empty_scalar(m: Mark) -> Event {
        let mut e = Event::new(Ev::Scalar, m);
        e.implicit0 = true;
        e
    }

    // ---------------------------------------------------------------------------
    // The composer (yaml/composer.py) and resolver (yaml/resolver.py)
    // ---------------------------------------------------------------------------

    fn get_single_node(&mut self) -> R<Option<usize>> {
        self.get_event()?;
        let mut doc = None;
        if !self.check_event(&[Ev::StreamEnd])? {
            doc = Some(self.compose_document()?);
        }
        if !self.check_event(&[Ev::StreamEnd])? {
            let ev = self.get_event()?;
            let ds = doc.map(|d| self.nodes[d].start);
            return Err(self.raise(
                "ComposerError",
                "expected a single document in the stream",
                ds,
                "but found another document",
                Some(ev.start),
            ));
        }
        self.get_event()?;
        Ok(doc)
    }

    fn compose_document(&mut self) -> R<usize> {
        self.get_event()?;
        let n = self.compose_node()?;
        self.get_event()?;
        self.anchors.clear();
        Ok(n)
    }

    fn compose_node(&mut self) -> R<usize> {
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            return unsupported("nesting deep enough to reach Python's recursion limit");
        }
        let r = self.compose_node_inner();
        self.depth -= 1;
        r
    }

    fn new_node(&mut self, kind: Nk, tag: String, value: String, start: Mark) -> usize {
        self.nodes.push(Node { kind, tag, value, items: vec![], pairs: vec![], start });
        self.nodes.len() - 1
    }

    fn compose_node_inner(&mut self) -> R<usize> {
        if self.check_event(&[Ev::Alias])? {
            let ev = self.get_event()?;
            let a = ev.anchor.unwrap_or_default();
            let Some(&n) = self.anchors.get(&a) else {
                return Err(self.raise(
                    "ComposerError",
                    "",
                    None,
                    &format!("found undefined alias {}", repr(&a)),
                    Some(ev.start),
                ));
            };
            if self.composing.contains(&n) {
                self.refuse_later("a node that holds itself");
            }
            return Ok(n);
        }
        let ev = self.peek_event()?;
        if let Some(a) = &ev.anchor {
            if let Some(&first) = self.anchors.get(a) {
                return Err(self.raise(
                    "ComposerError",
                    &format!("found duplicate anchor {}; first occurrence", repr(a)),
                    Some(self.nodes[first].start),
                    "second occurrence",
                    Some(ev.start),
                ));
            }
        }
        if self.check_event(&[Ev::Scalar])? {
            let ev = self.get_event()?;
            let tag = match &ev.tag {
                Some(t) if t != "!" => t.clone(),
                _ => resolve_tag(&ev.value, ev.implicit0)?,
            };
            let n = self.new_node(Nk::Scalar, tag, ev.value, ev.start);
            if let Some(a) = ev.anchor {
                self.anchors.insert(a, n);
            }
            return Ok(n);
        }
        let seq = self.check_event(&[Ev::SeqStart])?;
        let ev = self.get_event()?;
        let (kind, default, end) =
            if seq { (Nk::Seq, TAG_SEQ, Ev::SeqEnd) } else { (Nk::Map, TAG_MAP, Ev::MapEnd) };
        let tag = match &ev.tag {
            Some(t) if t != "!" => t.clone(),
            _ => default.to_string(),
        };
        let n = self.new_node(kind, tag, String::new(), ev.start);
        if let Some(a) = ev.anchor {
            self.anchors.insert(a, n);
        }
        self.composing.insert(n);
        while !self.check_event(&[end])? {
            if seq {
                let c = self.compose_node()?;
                self.nodes[n].items.push(c);
            } else {
                let k = self.compose_node()?;
                let v = self.compose_node()?;
                self.nodes[n].pairs.push((k, v));
            }
        }
        self.composing.remove(&n);
        self.get_event()?;
        Ok(n)
    }

    /// Notes a value the result model does not hold; see `unmodeled`.
    fn refuse_later(&mut self, why: &str) {
        if self.unmodeled.is_none() {
            self.unmodeled = Some(why.into());
        }
    }

    // ---------------------------------------------------------------------------
    // The constructor (yaml/constructor.py, SafeConstructor)
    // ---------------------------------------------------------------------------

    fn construct_err(&self, context: &str, cmark: Option<Mark>, problem: String, pmark: Mark) -> Fail {
        self.raise("ConstructorError", context, cmark, &problem, Some(pmark))
    }

    fn document(&mut self, n: usize) -> R<CVal> {
        let data = self.object(n)?;
        while !self.pending.is_empty() {
            let run = std::mem::take(&mut self.pending);
            for p in run {
                self.run_pending(p)?;
            }
        }
        Ok(data)
    }

    fn run_pending(&mut self, p: Pending) -> R<()> {
        match p {
            Pending::Seq(n, slot) => {
                if self.nodes[n].kind != Nk::Seq {
                    let nd = &self.nodes[n];
                    return Err(self.construct_err(
                        "",
                        None,
                        format!("expected a sequence node, but found {}", node_id(nd.kind)),
                        nd.start,
                    ));
                }
                for child in self.nodes[n].items.clone() {
                    let v = self.object(child)?;
                    self.seqs[slot].push(v);
                }
                Ok(())
            }
            Pending::Map(n, slot) => {
                let pairs = self.mapping(n)?;
                self.maps[slot].extend(pairs);
                Ok(())
            }
            Pending::Set(n) => {
                self.mapping(n)?;
                let tag = self.nodes[n].tag.clone();
                self.refuse_later(&format!("a {tag} value"));
                Ok(())
            }
            Pending::OMap(n) => {
                let nd = self.nodes[n].clone();
                let context = if nd.tag == "tag:yaml.org,2002:pairs" {
                    "while constructing pairs"
                } else {
                    "while constructing an ordered map"
                };
                if nd.kind != Nk::Seq {
                    return Err(self.construct_err(
                        context,
                        Some(nd.start),
                        format!("expected a sequence, but found {}", node_id(nd.kind)),
                        nd.start,
                    ));
                }
                for sub in nd.items {
                    let s = self.nodes[sub].clone();
                    if s.kind != Nk::Map {
                        return Err(self.construct_err(
                            context,
                            Some(nd.start),
                            format!("expected a mapping of length 1, but found {}", node_id(s.kind)),
                            s.start,
                        ));
                    }
                    if s.pairs.len() != 1 {
                        return Err(self.construct_err(
                            context,
                            Some(nd.start),
                            format!("expected a single mapping item, but found {} items", s.pairs.len()),
                            s.start,
                        ));
                    }
                    self.object(s.pairs[0].0)?;
                    self.object(s.pairs[0].1)?;
                }
                self.refuse_later(&format!("a {} value", nd.tag));
                Ok(())
            }
        }
    }

    fn object(&mut self, n: usize) -> R<CVal> {
        if let Some(v) = self.built.get(&n) {
            return Ok(v.clone());
        }
        if self.in_progress.contains(&n) {
            return Err(self.construct_err(
                "",
                None,
                "found unconstructable recursive node".into(),
                self.nodes[n].start,
            ));
        }
        self.in_progress.insert(n);
        let tag = self.nodes[n].tag.clone();
        let data = match tag.as_str() {
            TAG_NULL => {
                self.scalar(n)?;
                CVal::Null
            }
            TAG_BOOL => {
                let v = self.scalar(n)?;
                match lower(&v).as_str() {
                    "yes" | "true" | "on" => CVal::Bool(true),
                    "no" | "false" | "off" => CVal::Bool(false),
                    // self.bool_values[value.lower()] raises KeyError.
                    _ => return Err(exc("KeyError", repr(&lower(&v)))),
                }
            }
            TAG_INT => {
                let v = self.scalar(n)?;
                if !int_re().is_match(&v) || v.ends_with('\n') {
                    if v.replace('_', "").is_empty() {
                        return Err(exc("IndexError", "string index out of range"));
                    }
                    return unsupported("an !!int value Python's int() may read another way");
                }
                CVal::Int(yaml_int(&v)?)
            }
            TAG_FLOAT => {
                let v = self.scalar(n)?;
                if !float_re().is_match(&v) || v.ends_with('\n') {
                    return float_error(&v);
                }
                CVal::Float(yaml_float(&v)?)
            }
            TAG_TIMESTAMP => {
                self.scalar(n)?;
                if self.nodes[n].kind != Nk::Scalar {
                    return unsupported("a !!timestamp on a collection");
                }
                match timestamp(&self.nodes[n].value)? {
                    Val::Timestamp(t) => CVal::Ts(t),
                    _ => CVal::Null,
                }
            }
            TAG_STR => CVal::Str(self.scalar(n)?),
            TAG_SEQ => {
                self.seqs.push(vec![]);
                let slot = self.seqs.len() - 1;
                self.pending.push(Pending::Seq(n, slot));
                CVal::Seq(slot)
            }
            TAG_MAP => {
                self.maps.push(vec![]);
                let slot = self.maps.len() - 1;
                self.pending.push(Pending::Map(n, slot));
                CVal::Map(slot)
            }
            "tag:yaml.org,2002:binary" => {
                // base64.decodebytes: its errors are not modeled.
                return unsupported(&format!("a {tag} value"));
            }
            "tag:yaml.org,2002:set" => {
                self.pending.push(Pending::Set(n));
                CVal::Other
            }
            "tag:yaml.org,2002:omap" | "tag:yaml.org,2002:pairs" => {
                self.pending.push(Pending::OMap(n));
                CVal::Other
            }
            _ => {
                return Err(self.construct_err(
                    "",
                    None,
                    format!("could not determine a constructor for the tag {}", repr(&tag)),
                    self.nodes[n].start,
                ));
            }
        };
        self.built.insert(n, data.clone());
        self.in_progress.remove(&n);
        Ok(data)
    }

    /// SafeConstructor.construct_scalar.
    fn scalar(&self, n: usize) -> R<String> {
        let nd = &self.nodes[n];
        if nd.kind == Nk::Map {
            for &(k, v) in &nd.pairs {
                if self.nodes[k].tag == TAG_VALUE {
                    return self.scalar(v);
                }
            }
        }
        if nd.kind != Nk::Scalar {
            return Err(self.construct_err(
                "",
                None,
                format!("expected a scalar node, but found {}", node_id(nd.kind)),
                nd.start,
            ));
        }
        Ok(nd.value.clone())
    }

    /// SafeConstructor.flatten_mapping.
    fn flatten(&mut self, n: usize) -> R<()> {
        let mut merge: Vec<(usize, usize)> = vec![];
        let mut i = 0;
        while i < self.nodes[n].pairs.len() {
            let (k, v) = self.nodes[n].pairs[i];
            let ktag = self.nodes[k].tag.clone();
            if ktag == TAG_MERGE {
                self.nodes[n].pairs.remove(i);
                let vk = self.nodes[v].kind;
                match vk {
                    Nk::Map => {
                        self.flatten(v)?;
                        merge.extend(self.nodes[v].pairs.clone());
                    }
                    Nk::Seq => {
                        let mut sub: Vec<Vec<(usize, usize)>> = vec![];
                        for s in self.nodes[v].items.clone() {
                            if self.nodes[s].kind != Nk::Map {
                                return Err(self.construct_err(
                                    "while constructing a mapping",
                                    Some(self.nodes[n].start),
                                    format!(
                                        "expected a mapping for merging, but found {}",
                                        node_id(self.nodes[s].kind)
                                    ),
                                    self.nodes[s].start,
                                ));
                            }
                            self.flatten(s)?;
                            sub.push(self.nodes[s].pairs.clone());
                        }
                        for p in sub.into_iter().rev() {
                            merge.extend(p);
                        }
                    }
                    Nk::Scalar => {
                        return Err(self.construct_err(
                            "while constructing a mapping",
                            Some(self.nodes[n].start),
                            format!(
                                "expected a mapping or list of mappings for merging, but found {}",
                                node_id(vk)
                            ),
                            self.nodes[v].start,
                        ));
                    }
                }
            } else if ktag == TAG_VALUE {
                self.nodes[k].tag = TAG_STR.into();
                i += 1;
            } else {
                i += 1;
            }
        }
        if !merge.is_empty() {
            let rest = std::mem::take(&mut self.nodes[n].pairs);
            merge.extend(rest);
            self.nodes[n].pairs = merge;
        }
        Ok(())
    }

    /// construct_mapping: the pairs in order, a repeated key included.
    fn mapping(&mut self, n: usize) -> R<Vec<(CVal, CVal)>> {
        if self.nodes[n].kind == Nk::Map {
            self.flatten(n)?;
        }
        if self.nodes[n].kind != Nk::Map {
            let nd = &self.nodes[n];
            return Err(self.construct_err(
                "",
                None,
                format!("expected a mapping node, but found {}", node_id(nd.kind)),
                nd.start,
            ));
        }
        let mut out = vec![];
        for (k, v) in self.nodes[n].pairs.clone() {
            let key = self.object(k)?;
            if matches!(key, CVal::Seq(_) | CVal::Map(_) | CVal::Other) {
                return Err(self.construct_err(
                    "while constructing a mapping",
                    Some(self.nodes[n].start),
                    "found unhashable key".into(),
                    self.nodes[k].start,
                ));
            }
            let val = self.object(v)?;
            out.push((key, val));
        }
        Ok(out)
    }

    // ---------------------------------------------------------------------------
    // From the constructed values to the result model
    // ---------------------------------------------------------------------------

    fn finish(&self, v: &CVal, depth: usize) -> R<Val> {
        if depth > MAX_DEPTH {
            return unsupported("nesting deep enough to reach Python's recursion limit");
        }
        Ok(match v {
            CVal::Null => Val::Null,
            CVal::Bool(b) => Val::Bool(*b),
            CVal::Int(s) => Val::Int(s.clone()),
            CVal::Float(f) => Val::Float(*f),
            CVal::Str(s) => Val::Str(s.clone()),
            CVal::Ts(s) => Val::Timestamp(s.clone()),
            CVal::Seq(slot) => {
                let mut out = vec![];
                for e in &self.seqs[*slot] {
                    out.push(self.finish(e, depth + 1)?);
                }
                Val::List(out)
            }
            CVal::Map(slot) => {
                let mut out: Vec<(String, Val)> = vec![];
                for (k, e) in &self.maps[*slot] {
                    let key = dict_key(k)?;
                    let val = self.finish(e, depth + 1)?;
                    // A repeated key keeps its first place and takes the last
                    // value.
                    match out.iter_mut().find(|(kk, _)| *kk == key) {
                        Some(slot) => slot.1 = val,
                        None => out.push((key, val)),
                    }
                }
                Val::Map(out)
            }
            CVal::Other => return unsupported("a value the result model does not hold"),
        })
    }
}

/// Resolver.resolve for a scalar.
fn resolve_tag(value: &str, implicit: bool) -> R<String> {
    if implicit {
        if value.ends_with('\n') {
            // Python's $ also matches before a last line break.
            return unsupported("a resolved scalar that ends in a line break");
        }
        let first = value.chars().next();
        let resolvers: [(&str, &str, bool, fn(&str) -> bool); 8] = [
            (TAG_BOOL, "yYnNtTfFoO", false, |s| bool_re().is_match(s)),
            (TAG_FLOAT, "-+0123456789.", false, |s| float_re().is_match(s)),
            (TAG_INT, "-+0123456789", false, |s| int_re().is_match(s)),
            (TAG_MERGE, "<", false, |s| s == "<<"),
            (TAG_NULL, "~nN", true, |s| null_re().is_match(s)),
            (TAG_TIMESTAMP, "0123456789", false, |s| time_re().is_match(s)),
            (TAG_VALUE, "=", false, |s| s == "="),
            ("tag:yaml.org,2002:yaml", "!&*", false, |s| s == "!" || s == "&" || s == "*"),
        ];
        for (tag, firsts, empty, matches) in resolvers {
            match first {
                None => {
                    if !empty {
                        continue;
                    }
                }
                Some(c) => {
                    if !firsts.contains(c) {
                        continue;
                    }
                }
            }
            if matches(value) {
                return Ok(tag.into());
            }
        }
    }
    Ok(TAG_STR.into())
}

/// The key a value makes in a dict. A key that is not a str is marked, so
/// it never equals a field name; True, 1 and 1.0 are one key in Python, and
/// the marks keep that for bool and int.
fn dict_key(k: &CVal) -> R<String> {
    match k {
        CVal::Str(s) => Ok(s.clone()),
        CVal::Null => Ok("\0None".into()),
        CVal::Bool(true) => Ok("\0True".into()),
        CVal::Bool(false) => Ok("\0False".into()),
        CVal::Int(t) => Ok(match t.as_str() {
            "1" => "\0True".into(),
            "0" => "\0False".into(),
            _ => format!("\0int:{t}"),
        }),
        CVal::Float(_) => unsupported("a float key"),
        CVal::Ts(_) => unsupported("a timestamp key"),
        _ => unsupported("a key of another type"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::py_float_repr;
    use serde_json::{json, Value as J};

    /// clients/yaml_cases.py's typed dump.
    fn dump(v: &Val) -> J {
        match v {
            Val::Null => J::Null,
            Val::Bool(b) => J::Bool(*b),
            Val::Str(s) => J::String(s.clone()),
            Val::Int(t) => json!({"int": t}),
            Val::Float(f) => json!({"float": if f.is_nan() { "nan".to_string() } else { py_float_repr(*f) }}),
            Val::Timestamp(t) => json!({"other": if t.contains(':') { "datetime" } else { "date" }}),
            Val::List(xs) => json!({"list": xs.iter().map(dump).collect::<Vec<_>>()}),
            Val::Map(m) => {
                let pairs: Vec<J> = m
                    .iter()
                    .map(|(k, v)| {
                        let key = match k.strip_prefix('\0') {
                            Some(rest) => json!({"key": rest}),
                            None => J::String(k.clone()),
                        };
                        json!([key, dump(v)])
                    })
                    .collect();
                json!({"dict": pairs})
            }
        }
    }

    #[test]
    fn full_against_pyyaml() {
        let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/yaml/cases.jsonl");
        let text = std::fs::read_to_string(path).expect("clients/fixtures/yaml (clients/yaml_cases.py)");
        let (mut n, mut refused, mut bad) = (0, 0, 0);
        let mut reasons: HashMap<String, usize> = HashMap::new();
        for line in text.lines().filter(|l| !l.trim().is_empty()) {
            let row: J = serde_json::from_str(line).unwrap();
            let src = row["text"].as_str().unwrap();
            let want = &row["expect"];
            n += 1;
            let got = match load(src) {
                Ok(v) => dump(&v),
                Err(Fail::Exc(e)) => json!({"exc": e.kind, "msg": e.msg}),
                Err(Fail::Unsupported(why)) => {
                    if want.get("unmodeled").is_none() {
                        refused += 1;
                        *reasons.entry(why).or_default() += 1;
                    }
                    continue;
                }
            };
            if &got != want {
                bad += 1;
                if bad <= 25 {
                    eprintln!("{src:?}:\n got {got}\nwant {want}");
                }
            }
        }
        eprintln!("{n} texts: {refused} refused ({reasons:?}), {bad} differ");
        assert!(n >= 1000, "only {n} texts");
        assert_eq!(bad, 0);
        assert_eq!(refused, 0, "{reasons:?}");
    }

    #[test]
    fn small_texts() {
        assert_eq!(load("a: yes\n").unwrap(), Val::Map(vec![("a".into(), Val::Bool(true))]));
        assert_eq!(load("a: 0x10\n").unwrap(), Val::Map(vec![("a".into(), Val::Int("16".into()))]));
        assert_eq!(load("a: 1:30\n").unwrap(), Val::Map(vec![("a".into(), Val::Int("90".into()))]));
        assert_eq!(load("a: 1e5\n").unwrap(), Val::Map(vec![("a".into(), Val::Str("1e5".into()))]));
        assert_eq!(load("").unwrap(), Val::Null);
        assert!(matches!(load("a: \x7f\n"), Err(Fail::Exc(_))));
        assert!(matches!(load("a: &r [*r]\n"), Err(Fail::Unsupported(_))));
    }
}
