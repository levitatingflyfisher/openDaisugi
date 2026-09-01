//! pydantic's ValidationError, as `str(exc)` prints it (pydantic 2.13,
//! pydantic-core 2.46), for the models the gate validates.

use super::py::PyErr;
use super::pyjson::{py_repr, py_type_name, Value};
use super::R;

/// The documentation version pydantic prints in its error links.
pub const DOCS_VERSION: &str = "2.13";

/// One line error: where, what, and the input it was about.
#[derive(Debug, Clone)]
pub struct LineErr {
    /// The location, each part as pydantic prints it.
    pub loc: Vec<String>,
    pub msg: String,
    /// The error type, such as `string_type` or `missing`.
    pub typ: String,
    /// `input_value` (its repr) and `input_type`; None when pydantic hides
    /// the input.
    pub input: Option<(String, String)>,
}

impl LineErr {
    /// An error about `input`, a value `json.loads` gave.
    pub fn about(loc: &[&str], msg: impl Into<String>, typ: &str, input: &Value) -> R<LineErr> {
        Ok(LineErr {
            loc: loc.iter().map(|s| s.to_string()).collect(),
            msg: msg.into(),
            typ: typ.to_string(),
            input: Some((py_repr(input)?, py_type_name(input).to_string())),
        })
    }
}

/// pydantic-core's `truncate_safe_repr`: a repr longer than 50 bytes keeps
/// its first 25 bytes and its last 24, each cut back to whole characters,
/// with `...` between (measured on pydantic-core 2.46).
pub fn truncate_repr(s: &str) -> String {
    const MAX: usize = 50;
    if s.len() <= MAX {
        return s.to_string();
    }
    let mid = MAX.div_ceil(2);
    let mut head_end = mid;
    while !s.is_char_boundary(head_end) {
        head_end -= 1;
    }
    let mut tail_start = s.len() - (MAX - mid - 1);
    while !s.is_char_boundary(tail_start) {
        tail_start += 1;
    }
    format!("{}...{}", &s[..head_end], &s[tail_start..])
}

/// `str(ValidationError)` for `title` with these line errors.
pub fn validation_error(title: &str, errs: &[LineErr]) -> PyErr {
    let n = errs.len();
    let mut out = format!("{n} validation error{} for {title}", if n == 1 { "" } else { "s" });
    for e in errs {
        out.push('\n');
        // An error at the root prints no location line.
        if !e.loc.is_empty() {
            out.push_str(&e.loc.join("."));
            out.push('\n');
        }
        out.push_str("  ");
        out.push_str(&e.msg);
        out.push_str(&format!(" [type={}", e.typ));
        if let Some((v, t)) = &e.input {
            out.push_str(&format!(", input_value={}, input_type={t}", truncate_repr(v)));
        }
        out.push(']');
        out.push_str(&format!("\n    For further information visit https://errors.pydantic.dev/{DOCS_VERSION}/v/{}", e.typ));
    }
    PyErr::new("ValidationError", out)
}

/// `models.non_finite_error`'s walk over a validated value: one
/// `finite_number` error for each NaN, Infinity or -Infinity, at `loc`
/// plus its place, in document order.
pub fn non_finite(x: &Value, loc: &[String], errs: &mut Vec<LineErr>) -> R<()> {
    let at = |part: String| {
        let mut l = loc.to_vec();
        l.push(part);
        l
    };
    match x {
        Value::Float(f) if !f.is_finite() => errs.push(LineErr {
            loc: loc.to_vec(),
            msg: "Input should be a finite number".into(),
            typ: "finite_number".into(),
            input: Some((py_repr(x)?, "float".into())),
        }),
        Value::List(l) => {
            for (i, e) in l.iter().enumerate() {
                non_finite(e, &at(i.to_string()), errs)?;
            }
        }
        Value::Obj(o) => {
            for k in o.keys() {
                non_finite(o.value(k), &at(k.clone()), errs)?;
            }
        }
        _ => {}
    }
    Ok(())
}

/// A `str` field given `v`: the error pydantic gives when `v` is no str.
pub fn check_str(loc: &str, v: &Value, errs: &mut Vec<LineErr>) -> R<()> {
    if !matches!(v, Value::Str(_)) {
        errs.push(LineErr::about(&[loc], "Input should be a valid string", "string_type", v)?);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn truncation_is_pydantics() {
        let list: Vec<String> = (0..40).map(|i| i.to_string()).collect();
        let r = format!("[{}]", list.join(", "));
        assert_eq!(truncate_repr(&r), "[0, 1, 2, 3, 4, 5, 6, 7, ... 34, 35, 36, 37, 38, 39]");
        let e = format!("['{}']", "é".repeat(40));
        assert_eq!(truncate_repr(&e), format!("['{}...{}']", "é".repeat(11), "é".repeat(11)));
    }

    #[test]
    fn a_step_error_reads_as_pydantics() {
        let mut errs = Vec::new();
        check_str("path", &Value::Int("7".into()), &mut errs).unwrap();
        assert_eq!(
            validation_error("FileReadStep", &errs).msg,
            "1 validation error for FileReadStep\npath\n  Input should be a valid string [type=string_type, \
             input_value=7, input_type=int]\n    For further information visit \
             https://errors.pydantic.dev/2.13/v/string_type"
        );
    }
}
