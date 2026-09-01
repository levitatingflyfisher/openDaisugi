//! `yaml.safe_dump(v, sort_keys=False, default_flow_style=False)`: PyYAML
//! 6's representer, serializer and emitter for the values JSON gives, with
//! its defaults: indent 2, width 80, no unicode (so non-ASCII text is
//! written double-quoted with escapes), no aliases. The Go client's
//! `pyyaml/dump.go` is the reference.

use std::sync::OnceLock;

use regex::Regex;

use crate::gate::py::text::{as_surrogate, cp_char};
use crate::gate::pyjson::{float_repr, Object, Value};

/// A value or text outside what this port writes or reads.
#[derive(Debug, Clone)]
pub struct Unsupported(pub String);

type R<T> = Result<T, Unsupported>;

fn unsupported<T>(why: &str) -> R<T> {
    Err(Unsupported(why.into()))
}

const BEST_INDENT: i64 = 2;
const BEST_WIDTH: i64 = 80;

/// A code point of a Python str: a lone surrogate is its own value.
pub fn cp(c: char) -> u32 {
    as_surrogate(c).unwrap_or(c as u32)
}

pub fn cps(s: &str) -> Vec<u32> {
    s.chars().map(cp).collect()
}

fn from_cps(c: &[u32]) -> String {
    c.iter().map(|&x| cp_char(x)).collect()
}

pub fn safe_dump(v: &Value) -> R<String> {
    let mut e = Emitter {
        b: String::new(),
        indents: vec![],
        indent: -1,
        flow_level: 0,
        root_ctx: false,
        seq_ctx: false,
        map_ctx: false,
        simple_key_ctx: false,
        column: 0,
        whitespace: true,
        indention: true,
        open_ended: false,
    };
    e.node(v, true, false, false, false)?;
    // DocumentEnd of an implicit document, then StreamEnd.
    e.write_indent();
    if e.open_ended {
        e.write_indicator("...", true, false, false);
        e.write_indent();
    }
    Ok(e.b)
}

struct Emitter {
    b: String,
    indents: Vec<i64>,
    indent: i64,
    flow_level: usize,
    root_ctx: bool,
    #[allow(dead_code)]
    seq_ctx: bool,
    map_ctx: bool,
    simple_key_ctx: bool,
    column: i64,
    whitespace: bool,
    indention: bool,
    open_ended: bool,
}

impl Emitter {
    fn increase_indent(&mut self, flow: bool, indentless: bool) {
        self.indents.push(self.indent);
        if self.indent < 0 {
            self.indent = if flow { BEST_INDENT } else { 0 };
        } else if !indentless {
            self.indent += BEST_INDENT;
        }
    }

    fn pop_indent(&mut self) {
        self.indent = self.indents.pop().unwrap_or(-1);
    }

    fn write(&mut self, s: &str) {
        self.column += s.chars().count() as i64;
        self.b.push_str(s);
    }

    fn write_indicator(&mut self, ind: &str, need_whitespace: bool, whitespace: bool, indention: bool) {
        let data = if !self.whitespace && need_whitespace { format!(" {ind}") } else { ind.to_string() };
        self.whitespace = whitespace;
        self.indention = self.indention && indention;
        self.open_ended = false;
        self.write(&data);
    }

    fn write_indent(&mut self) {
        let indent = self.indent.max(0);
        if !self.indention || self.column > indent || (self.column == indent && !self.whitespace) {
            self.write_line_break("\n");
        }
        if self.column < indent {
            self.whitespace = true;
            self.b.push_str(&" ".repeat((indent - self.column) as usize));
            self.column = indent;
        }
    }

    fn write_line_break(&mut self, data: &str) {
        self.whitespace = true;
        self.indention = true;
        self.column = 0;
        self.b.push_str(data);
    }

    fn node(&mut self, v: &Value, root: bool, seq: bool, mapping: bool, simple_key: bool) -> R<()> {
        self.root_ctx = root;
        self.seq_ctx = seq;
        self.map_ctx = mapping;
        self.simple_key_ctx = simple_key;
        match v {
            Value::List(xs) => {
                if self.flow_level > 0 || xs.is_empty() {
                    self.flow_sequence(xs)
                } else {
                    self.block_sequence(xs)
                }
            }
            Value::Obj(o) => {
                if self.flow_level > 0 || o.is_empty() {
                    self.flow_mapping(o)
                } else {
                    self.block_mapping(o)
                }
            }
            other => {
                let (tag, value) = represent(other);
                self.scalar(tag, &value)
            }
        }
    }

    fn flow_sequence(&mut self, xs: &[Value]) -> R<()> {
        self.write_indicator("[", true, true, false);
        self.flow_level += 1;
        self.increase_indent(true, false);
        for (i, x) in xs.iter().enumerate() {
            if i > 0 {
                self.write_indicator(",", false, false, false);
            }
            if self.column > BEST_WIDTH {
                self.write_indent();
            }
            self.node(x, false, true, false, false)?;
        }
        self.pop_indent();
        self.flow_level -= 1;
        self.write_indicator("]", false, false, false);
        Ok(())
    }

    fn flow_mapping(&mut self, o: &Object) -> R<()> {
        self.write_indicator("{", true, true, false);
        self.flow_level += 1;
        self.increase_indent(true, false);
        for (i, k) in o.keys().iter().enumerate() {
            if i > 0 {
                self.write_indicator(",", false, false, false);
            }
            if self.column > BEST_WIDTH {
                self.write_indent();
            }
            if !simple_key(k) {
                return unsupported("a key too long or too odd for a simple key");
            }
            self.node(&Value::Str(k.clone()), false, false, true, true)?;
            self.write_indicator(":", false, false, false);
            self.node(o.value(k), false, false, true, false)?;
        }
        self.pop_indent();
        self.flow_level -= 1;
        self.write_indicator("}", false, false, false);
        Ok(())
    }

    fn block_sequence(&mut self, xs: &[Value]) -> R<()> {
        let indentless = self.map_ctx && !self.indention;
        self.increase_indent(false, indentless);
        for x in xs {
            self.write_indent();
            self.write_indicator("-", true, false, true);
            self.node(x, false, true, false, false)?;
        }
        self.pop_indent();
        Ok(())
    }

    fn block_mapping(&mut self, o: &Object) -> R<()> {
        self.increase_indent(false, false);
        for k in o.keys() {
            self.write_indent();
            let key = Value::Str(k.clone());
            if simple_key(k) {
                self.node(&key, false, false, true, true)?;
                self.write_indicator(":", false, false, false);
                self.node(o.value(k), false, false, true, false)?;
                continue;
            }
            self.write_indicator("?", true, false, true);
            self.node(&key, false, false, true, false)?;
            self.write_indent();
            self.write_indicator(":", true, false, true);
            self.node(o.value(k), false, false, true, false)?;
        }
        self.pop_indent();
        Ok(())
    }

    fn scalar(&mut self, tag: &str, value: &str) -> R<()> {
        let text = cps(value);
        let a = analyze(&text, value);
        let implicit_plain = resolve_tag(value) == tag;
        let implicit_quoted = tag == "str";
        let style = self.choose_style(&a, implicit_plain);
        if !((style == Style::Plain && implicit_plain) || (style != Style::Plain && implicit_quoted)) {
            return unsupported("a scalar that needs an explicit tag");
        }
        self.increase_indent(true, false);
        let split = !self.simple_key_ctx;
        match style {
            Style::Double => self.write_double_quoted(&text, split),
            Style::Single => self.write_single_quoted(&text, split),
            Style::Plain => self.write_plain(&text, split),
        }
        self.pop_indent();
        Ok(())
    }

    fn choose_style(&self, a: &Analysis, implicit_plain: bool) -> Style {
        if implicit_plain
            && !(self.simple_key_ctx && (a.empty || a.multiline))
            && ((self.flow_level > 0 && a.allow_flow_plain) || (self.flow_level == 0 && a.allow_block_plain))
        {
            return Style::Plain;
        }
        if a.allow_single_quoted && !(self.simple_key_ctx && a.multiline) {
            return Style::Single;
        }
        Style::Double
    }

    fn write_single_quoted(&mut self, text: &[u32], split: bool) {
        self.write_indicator("'", true, false, false);
        let (mut spaces, mut breaks) = (false, false);
        let (mut start, mut end) = (0usize, 0usize);
        let n = text.len();
        while end <= n {
            let ch: Option<u32> = text.get(end).copied();
            if spaces {
                if ch != Some(b' ' as u32) {
                    if start + 1 == end && self.column > BEST_WIDTH && split && start != 0 && end != n {
                        self.write_indent();
                    } else {
                        self.write(&from_cps(&text[start..end]));
                    }
                    start = end;
                }
            } else if breaks {
                if ch.is_none_or(|c| !is_break(c)) {
                    if text[start] == '\n' as u32 {
                        self.write_line_break("\n");
                    }
                    for &br in &text[start..end] {
                        self.write_line_break(&from_cps(&[br]));
                    }
                    self.write_indent();
                    start = end;
                }
            } else if ch.is_none_or(|c| c == ' ' as u32 || is_break(c) || c == '\'' as u32) && start < end {
                self.write(&from_cps(&text[start..end]));
                start = end;
            }
            if ch == Some('\'' as u32) {
                self.write("''");
                start = end + 1;
            }
            if let Some(c) = ch {
                spaces = c == ' ' as u32;
                breaks = is_break(c);
            }
            end += 1;
        }
        self.write_indicator("'", false, false, false);
    }

    fn write_double_quoted(&mut self, text: &[u32], split: bool) {
        self.write_indicator("\"", true, false, false);
        let (mut start, mut end) = (0usize, 0usize);
        let n = text.len();
        while end <= n {
            let ch: Option<u32> = text.get(end).copied();
            let special = match ch {
                None => true,
                Some(c) => {
                    c == '"' as u32
                        || c == '\\' as u32
                        || c == 0x85
                        || c == 0x2028
                        || c == 0x2029
                        || c == 0xfeff
                        || !(0x20..=0x7e).contains(&c)
                }
            };
            if special {
                if start < end {
                    self.write(&from_cps(&text[start..end]));
                    start = end;
                }
                if let Some(c) = ch {
                    let data = match escape_replacement(c) {
                        Some(r) => format!("\\{r}"),
                        None if c <= 0xff => format!("\\x{c:02X}"),
                        None if c <= 0xffff => format!("\\u{c:04X}"),
                        None => format!("\\U{c:08X}"),
                    };
                    self.write(&data);
                    start = end + 1;
                }
            }
            if 0 < end
                && end + 1 < n
                && (ch == Some(' ' as u32) || start >= end)
                && self.column + (end as i64 - start as i64) > BEST_WIDTH
                && split
            {
                let mut data = String::from("\\");
                if start < end {
                    data = from_cps(&text[start..end]) + "\\";
                    start = end;
                }
                self.write(&data);
                self.write_indent();
                self.whitespace = false;
                self.indention = false;
                if text[start] == ' ' as u32 {
                    self.write("\\");
                }
            }
            end += 1;
        }
        self.write_indicator("\"", false, false, false);
    }

    fn write_plain(&mut self, text: &[u32], split: bool) {
        if self.root_ctx {
            self.open_ended = true;
        }
        if text.is_empty() {
            return;
        }
        if !self.whitespace {
            self.write(" ");
        }
        self.whitespace = false;
        self.indention = false;
        let (mut spaces, mut breaks) = (false, false);
        let (mut start, mut end) = (0usize, 0usize);
        let n = text.len();
        while end <= n {
            let ch: Option<u32> = text.get(end).copied();
            if spaces {
                if ch != Some(' ' as u32) {
                    if start + 1 == end && self.column > BEST_WIDTH && split {
                        self.write_indent();
                        self.whitespace = false;
                        self.indention = false;
                    } else {
                        self.write(&from_cps(&text[start..end]));
                    }
                    start = end;
                }
            } else if breaks {
                if ch.is_none_or(|c| !is_break(c)) {
                    if text[start] == '\n' as u32 {
                        self.write_line_break("\n");
                    }
                    for &br in &text[start..end] {
                        self.write_line_break(&from_cps(&[br]));
                    }
                    self.write_indent();
                    self.whitespace = false;
                    self.indention = false;
                    start = end;
                }
            } else if ch.is_none_or(|c| c == ' ' as u32 || is_break(c)) {
                self.write(&from_cps(&text[start..end]));
                start = end;
            }
            if let Some(c) = ch {
                spaces = c == ' ' as u32;
                breaks = is_break(c);
            }
            end += 1;
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Style {
    Plain,
    Single,
    Double,
}

fn escape_replacement(c: u32) -> Option<&'static str> {
    Some(match c {
        0 => "0",
        0x07 => "a",
        0x08 => "b",
        0x09 => "t",
        0x0a => "n",
        0x0b => "v",
        0x0c => "f",
        0x0d => "r",
        0x1b => "e",
        0x22 => "\"",
        0x5c => "\\",
        0x85 => "N",
        0xa0 => "_",
        0x2028 => "L",
        0x2029 => "P",
        _ => return None,
    })
}

/// SafeRepresenter for a scalar: its tag and text.
fn represent(v: &Value) -> (&'static str, String) {
    match v {
        Value::Null => ("null", "null".into()),
        Value::Bool(true) => ("bool", "true".into()),
        Value::Bool(false) => ("bool", "false".into()),
        Value::Int(t) => ("int", t.clone()),
        Value::Float(f) => ("float", float_text(*f)),
        Value::Str(s) => ("str", s.clone()),
        // Lists and dicts never reach here.
        _ => ("str", String::new()),
    }
}

/// `represent_float`.
fn float_text(f: f64) -> String {
    if f.is_nan() {
        return ".nan".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { ".inf".into() } else { "-.inf".into() };
    }
    let mut v = float_repr(f).to_lowercase();
    if !v.contains('.') && v.contains('e') {
        v = v.replacen('e', ".0e", 1);
    }
    v
}

/// `check_simple_key` for a str key.
fn simple_key(k: &str) -> bool {
    let text = cps(k);
    let a = analyze(&text, k);
    2 + 3 + text.len() < 128 && !a.empty && !a.multiline
}

struct Analysis {
    empty: bool,
    multiline: bool,
    allow_flow_plain: bool,
    allow_block_plain: bool,
    allow_single_quoted: bool,
}

pub fn is_break(c: u32) -> bool {
    c == '\n' as u32 || c == 0x85 || c == 0x2028 || c == 0x2029
}

fn is_blank_or_end(c: u32) -> bool {
    c == 0 || c == ' ' as u32 || c == '\t' as u32 || c == '\r' as u32 || is_break(c)
}

/// `Emitter.analyze_scalar` with allow_unicode off.
fn analyze(rs: &[u32], s: &str) -> Analysis {
    if rs.is_empty() {
        return Analysis {
            empty: true,
            multiline: false,
            allow_flow_plain: false,
            allow_block_plain: true,
            allow_single_quoted: true,
        };
    }
    let (mut block_ind, mut flow_ind, mut line_breaks, mut special) = (false, false, false, false);
    let (mut leading_space, mut leading_break, mut trailing_space, mut trailing_break) = (false, false, false, false);
    let (mut break_space, mut space_break) = (false, false);
    if s.starts_with("---") || s.starts_with("...") {
        block_ind = true;
        flow_ind = true;
    }
    let mut preceded_by_ws = true;
    let mut followed_by_ws = rs.len() == 1 || is_blank_or_end(rs[1]);
    let (mut prev_space, mut prev_break) = (false, false);
    let n = rs.len();
    for (i, &ch) in rs.iter().enumerate() {
        let c = char::from_u32(ch).unwrap_or('\u{FFFD}');
        if i == 0 {
            if "#,[]{}&*!|>'\"%@`".contains(c) {
                flow_ind = true;
                block_ind = true;
            }
            if c == '?' || c == ':' {
                flow_ind = true;
                if followed_by_ws {
                    block_ind = true;
                }
            }
            if c == '-' && followed_by_ws {
                flow_ind = true;
                block_ind = true;
            }
        } else {
            if ",?[]{}".contains(c) {
                flow_ind = true;
            }
            if c == ':' {
                flow_ind = true;
                if followed_by_ws {
                    block_ind = true;
                }
            }
            if c == '#' && preceded_by_ws {
                flow_ind = true;
                block_ind = true;
            }
        }
        if is_break(ch) {
            line_breaks = true;
        }
        if !(ch == '\n' as u32 || (0x20..=0x7e).contains(&ch)) {
            // Any other character is special: allow_unicode is off.
            special = true;
        }
        if ch == ' ' as u32 {
            if i == 0 {
                leading_space = true;
            }
            if i == n - 1 {
                trailing_space = true;
            }
            if prev_break {
                break_space = true;
            }
            prev_space = true;
            prev_break = false;
        } else if is_break(ch) {
            if i == 0 {
                leading_break = true;
            }
            if i == n - 1 {
                trailing_break = true;
            }
            if prev_space {
                space_break = true;
            }
            prev_space = false;
            prev_break = true;
        } else {
            prev_space = false;
            prev_break = false;
        }
        preceded_by_ws = is_blank_or_end(ch);
        followed_by_ws = i + 2 >= n || is_blank_or_end(rs[i + 2]);
    }
    let mut a = Analysis {
        empty: false,
        multiline: line_breaks,
        allow_flow_plain: true,
        allow_block_plain: true,
        allow_single_quoted: true,
    };
    if leading_space || leading_break || trailing_space || trailing_break {
        a.allow_flow_plain = false;
        a.allow_block_plain = false;
    }
    if break_space {
        a.allow_flow_plain = false;
        a.allow_block_plain = false;
        a.allow_single_quoted = false;
    }
    if space_break || special {
        a.allow_flow_plain = false;
        a.allow_block_plain = false;
        a.allow_single_quoted = false;
    }
    if line_breaks {
        a.allow_flow_plain = false;
        a.allow_block_plain = false;
    }
    if flow_ind {
        a.allow_flow_plain = false;
    }
    if block_ind {
        a.allow_block_plain = false;
    }
    a
}

pub struct Resolvers {
    pub bool_re: Regex,
    pub float_re: Regex,
    pub int_re: Regex,
    pub null_re: Regex,
    pub time_re: Regex,
}

pub fn resolvers() -> &'static Resolvers {
    static R: OnceLock<Resolvers> = OnceLock::new();
    R.get_or_init(|| Resolvers {
        bool_re: Regex::new(r"^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$").expect("regex"),
        float_re: Regex::new(
            r"^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$",
        )
        .expect("regex"),
        int_re: Regex::new(
            r"^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$",
        )
        .expect("regex"),
        null_re: Regex::new(r"^(?:~|null|Null|NULL|)$").expect("regex"),
        time_re: Regex::new(
            r"^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$",
        )
        .expect("regex"),
    })
}

/// `Resolver.resolve(ScalarNode, value, (True, False))`: the short name of
/// the tag a plain scalar with this text would get.
pub fn resolve_tag(v: &str) -> &'static str {
    let r = resolvers();
    // Python's $ also matches before a final newline.
    let m = |re: &Regex| re.is_match(v) || (v.ends_with('\n') && re.is_match(&v[..v.len() - 1]));
    let first = match v.chars().next() {
        None => return "null",
        Some(c) => c,
    };
    let trimmed = v.strip_suffix('\n').unwrap_or(v);
    if "yYnNtTfFoO".contains(first) && m(&r.bool_re) {
        return "bool";
    }
    if "-+0123456789.".contains(first) && m(&r.float_re) {
        return "float";
    }
    if "-+0123456789".contains(first) && m(&r.int_re) {
        return "int";
    }
    if first == '<' && (v == "<<" || v == "<<\n") {
        return "merge";
    }
    if "~nN".contains(first) && m(&r.null_re) {
        return "null";
    }
    if "0123456789".contains(first) && m(&r.time_re) {
        return "timestamp";
    }
    if first == '=' && (v == "=" || v == "=\n") {
        return "value";
    }
    if "!&*".contains(first) && trimmed.trim_matches(['!', '&', '*']).is_empty() && trimmed.len() == 1 {
        return "yaml";
    }
    "str"
}

#[cfg(test)]
mod tests {
    use super::*;

    fn obj(pairs: &[(&str, Value)]) -> Value {
        let mut o = Object::new();
        for (k, v) in pairs {
            o.set(k, v.clone());
        }
        Value::Obj(o)
    }

    #[test]
    fn a_small_mapping_dumps_as_pyyaml_dumps_it() {
        let v = obj(&[
            ("name", Value::Str("build-it".into())),
            ("n", Value::Int("3".into())),
            ("f", Value::Float(1e20)),
            ("yes", Value::Str("yes".into())),
            ("xs", Value::List(vec![Value::Str("a".into()), Value::List(vec![])])),
            ("e", Value::Obj(Object::new())),
            ("u", Value::Str("café".into())),
        ]);
        assert_eq!(
            safe_dump(&v).unwrap(),
            "name: build-it\nn: 3\nf: 1.0e+20\n'yes': 'yes'\nxs:\n- a\n- []\ne: {}\nu: \"caf\\xE9\"\n"
        );
    }
}
