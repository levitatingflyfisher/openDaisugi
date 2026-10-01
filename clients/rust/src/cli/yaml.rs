//! YAML as `yaml.safe_load(path.read_text())` reads it, in the node shape
//! the config and envelope readers use. The loader is `crate::pyyaml`, a
//! translation of PyYAML's; a value its result model does not hold is
//! `YErr::Unsupported`, for the caller to refuse. The Go client's
//! `config/yaml.go` is the reference.

use std::collections::HashMap;

/// Why a text gives no node.
#[derive(Debug, Clone, PartialEq)]
pub enum YErr {
    /// A value the result model does not hold: refuse, never guess.
    Unsupported,
    /// The exception yaml.safe_load raises.
    Exc(crate::pyyaml::Exc),
}

impl YErr {
    /// The last line of the oracle's traceback: the qualified class and
    /// the first line of `str(exc)`.
    pub fn traceback_line(&self) -> String {
        match self {
            YErr::Unsupported => "YAML this binary does not read".into(),
            YErr::Exc(e) => format!("{}: {}", crate::pyyaml::qualified(e), crate::pyyaml::first_line(e)),
        }
    }
}

type R<T> = Result<T, YErr>;

/// The type PyYAML gives a node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Null,
    Bool,
    Int,
    Float,
    Str,
    Seq,
    Map,
    /// A date or a datetime: no field takes one.
    Other,
}

/// One node. A mapping's keys keep their first position; a repeated key
/// takes its last value. Only string keys are kept by name; `non_str_keys`
/// counts the others.
#[derive(Debug, Clone, PartialEq)]
pub struct Node {
    pub kind: Kind,
    pub b: bool,
    /// An Int's digits, a Float's text, a Str's value.
    pub text: String,
    pub items: Vec<Node>,
    pub keys: Vec<String>,
    pub map: HashMap<String, Node>,
    pub non_str_keys: usize,
}

impl Node {
    pub fn of(kind: Kind) -> Node {
        Node { kind, b: false, text: String::new(), items: vec![], keys: vec![], map: HashMap::new(), non_str_keys: 0 }
    }

    fn text(kind: Kind, t: &str) -> Node {
        let mut n = Node::of(kind);
        n.text = t.to_string();
        n
    }
}

/// `yaml.safe_load(path.read_text())` on one document.
pub fn parse(text: &str) -> R<Node> {
    // read_text reads with universal newlines.
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    match crate::pyyaml::load(&text) {
        Ok(v) => Ok(from_val(&v)),
        Err(crate::pyyaml::Fail::Unsupported(_)) => Err(YErr::Unsupported),
        Err(crate::pyyaml::Fail::Exc(e)) => Err(YErr::Exc(e)),
    }
}

fn from_val(v: &crate::pyyaml::Val) -> Node {
    use crate::pyyaml::Val;
    match v {
        Val::Null => Node::of(Kind::Null),
        Val::Bool(b) => {
            let mut n = Node::of(Kind::Bool);
            n.b = *b;
            n
        }
        Val::Int(t) => Node::text(Kind::Int, t),
        // A text Rust's float parser reads back to the same value.
        Val::Float(f) => Node::text(Kind::Float, &format!("{f:?}")),
        Val::Str(s) => Node::text(Kind::Str, s),
        Val::Timestamp(t) => Node::text(Kind::Other, t),
        Val::List(xs) => {
            let mut n = Node::of(Kind::Seq);
            n.items = xs.iter().map(from_val).collect();
            n
        }
        Val::Map(m) => {
            let mut n = Node::of(Kind::Map);
            for (k, x) in m {
                if k.starts_with('\0') {
                    n.non_str_keys += 1;
                    continue;
                }
                n.keys.push(k.clone());
                n.map.insert(k.clone(), from_val(x));
            }
            n
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// PyYAML reads .nan and .inf as floats; their text is one Rust's float
    /// parser reads, so register can refuse an envelope holding one.
    #[test]
    fn nan_and_inf_are_floats() {
        for (t, want) in [(".nan", f64::NAN), (".NaN", f64::NAN), (".inf", f64::INFINITY), ("+.inf", f64::INFINITY), ("-.INF", f64::NEG_INFINITY), (".Inf", f64::INFINITY)] {
            let n = parse(&format!("{t}\n")).unwrap_or_else(|_| panic!("{t} refused"));
            assert!(matches!(n.kind, Kind::Float), "{t}");
            let f: f64 = n.text.parse().unwrap();
            assert!(if want.is_nan() { f.is_nan() } else { f == want }, "{t}: {f}");
        }
        for t in ["nan", "inf", "NaN", "+.nan", ".nAn"] {
            assert!(matches!(parse(&format!("{t}\n")).map(|n| n.kind), Ok(Kind::Str)), "{t}");
        }
    }
}
