//! Values the way the Go command line holds and prints them. Go decodes a
//! reply into map[string]any, so every number becomes a float64, and then
//! prints it with fmt's %v or writes it back with json.Marshal, which sorts
//! the keys of every map.

use serde_json::{Map, Value};

/// v as Go's json.Unmarshal into `any` leaves it: every number a float64.
/// None when a number does not fit a float64, which Go refuses.
pub fn decode(v: &Value) -> Option<Value> {
    Some(match v {
        Value::Number(n) => {
            let f: f64 = n.to_string().parse().ok()?;
            if !f.is_finite() {
                return None;
            }
            crate::gojson::float(f)
        }
        Value::Array(a) => Value::Array(a.iter().map(decode).collect::<Option<Vec<_>>>()?),
        Value::Object(m) => {
            let mut out = Map::new();
            for (k, x) in m {
                out.insert(k.clone(), decode(x)?);
            }
            Value::Object(out)
        }
        other => other.clone(),
    })
}

/// v with the keys of every object sorted, as Go writes a map.
pub fn sorted(v: &Value) -> Value {
    match v {
        Value::Array(a) => Value::Array(a.iter().map(sorted).collect()),
        Value::Object(m) => {
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            let mut out = Map::new();
            for k in keys {
                out.insert(k.clone(), sorted(&m[k]));
            }
            Value::Object(out)
        }
        other => other.clone(),
    }
}

/// json.Marshal of a decoded value.
pub fn marshal(v: &Value) -> String {
    crate::gojson::marshal(&sorted(v))
}

fn as_f64(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => n.to_string().parse().ok(),
        _ => None,
    }
}

/// fmt's %v of a float64, strconv.FormatFloat(f, 'g', -1, 64): the
/// shortest digits, in exponent form when the decimal exponent is below -4
/// or at least 6.
pub fn float_g(f: f64) -> String {
    if f.is_nan() {
        return "NaN".into();
    }
    if f.is_infinite() {
        return if f > 0.0 {
            "+Inf".into()
        } else {
            "-Inf".into()
        };
    }
    if f == 0.0 {
        return if f.is_sign_negative() {
            "-0".into()
        } else {
            "0".into()
        };
    }
    // The shortest digits and the decimal exponent, from Rust's own
    // shortest exponent form, d.ddde±x.
    let e = format!("{:e}", f.abs());
    let (mant, exp) = e.split_once('e').unwrap_or((&e, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let digits: String = mant.chars().filter(|c| *c != '.').collect();
    let sign = if f < 0.0 { "-" } else { "" };
    if !(-4..6).contains(&exp) {
        return exp_form(sign, &digits, exp);
    }
    // Plain form.
    let nd = digits.len() as i32;
    if exp < 0 {
        let zeros = "0".repeat((-exp - 1) as usize);
        return format!("{sign}0.{zeros}{digits}");
    }
    if nd <= exp + 1 {
        let zeros = "0".repeat((exp + 1 - nd) as usize);
        return format!("{sign}{digits}{zeros}");
    }
    let (a, b) = digits.split_at((exp + 1) as usize);
    format!("{sign}{a}.{b}")
}

fn exp_form(sign: &str, digits: &str, exp: i32) -> String {
    let (a, b) = digits.split_at(1);
    let m = if b.is_empty() {
        a.to_string()
    } else {
        format!("{a}.{b}")
    };
    let es = if exp < 0 { '-' } else { '+' };
    format!("{sign}{m}e{es}{:02}", exp.abs())
}

/// fmt's %v of a decoded value. width, when set, pads each scalar on the
/// right to that many runes, as %-Nv does, inside lists and maps too.
pub fn v(x: &Value, width: usize) -> String {
    let pad = |s: String| -> String {
        let n = s.chars().count();
        if n < width {
            format!("{s}{}", " ".repeat(width - n))
        } else {
            s
        }
    };
    match x {
        Value::Null => pad("<nil>".into()),
        Value::Bool(b) => pad(b.to_string()),
        Value::Number(_) => pad(float_g(as_f64(x).unwrap_or(0.0))),
        Value::String(s) => pad(s.clone()),
        Value::Array(a) => {
            let parts: Vec<String> = a.iter().map(|e| v(e, width)).collect();
            format!("[{}]", parts.join(" "))
        }
        Value::Object(m) => {
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            let parts: Vec<String> = keys
                .iter()
                .map(|k| format!("{}:{}", pad(k.to_string()), v(&m[*k], width)))
                .collect();
            format!("map[{}]", parts.join(" "))
        }
    }
}

/// The status text's number: a whole number as the integer it is, a
/// fraction kept, anything else as %v prints it.
pub fn number(x: Option<&Value>) -> String {
    match x.and_then(as_f64) {
        Some(f) if f == f.trunc() && f.abs() < 1e15 => format!("{}", f as i64),
        Some(f) => format!("{f}"),
        None => match x {
            None => "<nil>".into(),
            Some(o) => v(o, 0),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn floats_print_as_go_v() {
        assert_eq!(float_g(3.0), "3");
        assert_eq!(float_g(123456.0), "123456");
        assert_eq!(float_g(1234567.0), "1.234567e+06");
        assert_eq!(float_g(1e6), "1e+06");
        assert_eq!(float_g(1759400000.25), "1.75940000025e+09");
        assert_eq!(float_g(0.0001), "0.0001");
        assert_eq!(float_g(0.00001), "1e-05");
        assert_eq!(float_g(-2.5), "-2.5");
        assert_eq!(float_g(1e21), "1e+21");
        assert_eq!(float_g(1e100), "1e+100");
    }

    #[test]
    fn values_print_as_go_v() {
        let x = decode(&json!({"b": [1, "x", null], "a": {"k": true}})).unwrap();
        assert_eq!(v(&x, 0), "map[a:map[k:true] b:[1 x <nil>]]");
        assert_eq!(v(&json!("ab"), 4), "ab  ");
        assert_eq!(v(&json!(["a", "b"]), 2), "[a  b ]");
    }

    #[test]
    fn marshal_sorts_and_floats() {
        let x = decode(&serde_json::from_str(r#"{"z":1.50,"a":[3],"m":"<"}"#).unwrap()).unwrap();
        assert_eq!(marshal(&x), r#"{"a":[3],"m":"\u003c","z":1.5}"#);
    }

    #[test]
    fn status_numbers() {
        assert_eq!(number(Some(&json!(1234567))), "1234567");
        assert_eq!(number(Some(&json!(2.5))), "2.5");
        assert_eq!(number(None), "<nil>");
    }
}
