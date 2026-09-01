//! `inheritance.verify_inheritance`: a child envelope must tighten its
//! parent. Envelopes are validated `model_dump()` values. The Go client's
//! `envgen/inherit.go` is the reference.

use std::collections::BTreeSet;

use num_bigint::BigInt;

use super::{float_of, list_of, obj, str_list};
use crate::gate::py::text::{repr, repr_list};
use crate::gate::pyjson::{py_repr, Object, Value};
use crate::pathways::pathway::dump_json;

/// `EnvelopeInheritanceError`: the violation messages.
#[derive(Debug, Clone)]
pub struct InheritanceError(pub Vec<String>);

impl std::fmt::Display for InheritanceError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "inheritance violations: {}", self.0.join("; "))
    }
}

fn sorted_set(xs: &[String]) -> Vec<String> {
    xs.iter().cloned().collect::<BTreeSet<_>>().into_iter().collect()
}

fn big(v: &Value) -> BigInt {
    match v {
        Value::Int(t) => t.parse().unwrap_or_default(),
        Value::Bool(b) => BigInt::from(*b as i64),
        _ => BigInt::default(),
    }
}

fn pyrepr(v: &Value) -> String {
    py_repr(v).unwrap_or_default()
}

/// `verify_inheritance(child, parent)`: the violation messages, none when
/// the child tightens the parent.
pub fn verify_inheritance(child: &Object, parent: &Object) -> Vec<String> {
    let mut out = vec![];
    if !parent.value("parent_envelope").is_null() {
        out.push("parent envelope has its own parent_envelope; v0.1.2 supports only depth-1 inheritance".to_string());
    }
    let cp = obj(child.value("permissions"));
    let pp = obj(parent.value("permissions"));
    let subset = |field: &str, out: &mut Vec<String>| {
        let parent_set: BTreeSet<String> = str_list(pp.value(field)).into_iter().collect();
        let extras: Vec<String> = str_list(cp.value(field)).into_iter().filter(|s| !parent_set.contains(s)).collect();
        for x in sorted_set(&extras) {
            out.push(format!(
                "{field}: child glob {} not in parent's allowed set {}",
                repr(&x),
                repr_list(&sorted_set(&str_list(pp.value(field))))
            ));
        }
    };
    let bool_le = |field: &str, out: &mut Vec<String>| {
        if cp.value(field).truthy() && !pp.value(field).truthy() {
            out.push(format!("{field}: child=True relaxes parent=False"));
        }
    };
    let int_le = |field: &str, out: &mut Vec<String>| {
        let (c, p) = (big(cp.value(field)), big(pp.value(field)));
        if c > p {
            out.push(format!("{field}: child={c} exceeds parent={p}"));
        }
    };
    subset("file_read", &mut out);
    subset("file_write", &mut out);
    bool_le("network", &mut out);
    let ph = str_list(pp.value("network_hosts"));
    let ch = str_list(cp.value("network_hosts"));
    if !ph.is_empty() {
        if ch.is_empty() {
            out.push(format!(
                "network_hosts: child is empty (means any host) but parent restricts to {}",
                repr_list(&sorted_set(&ph))
            ));
        } else {
            let ps: BTreeSet<&String> = ph.iter().collect();
            let extras: Vec<String> = ch.iter().filter(|h| !ps.contains(h)).cloned().collect();
            for x in sorted_set(&extras) {
                out.push(format!(
                    "network_hosts: child host {} not in parent's allowed set {}",
                    repr(&x),
                    repr_list(&sorted_set(&ph))
                ));
            }
        }
    }
    bool_le("shell", &mut out);
    subset("shell_allowlist", &mut out);
    bool_le("shell_allow_decomposition", &mut out);
    int_le("max_execution_time_s", &mut out);
    int_le("max_output_size_mb", &mut out);
    subset("mcp_allowlist", &mut out);
    if let Some(r) = robot_relaxed(&pp, &cp) {
        out.push(format!("robot capability relaxed: {r}"));
    }
    let rank = |s: &Value| match s.as_str() {
        Some("medium") => 1,
        Some("high") => 2,
        Some("physical") => 3,
        _ => 0,
    };
    let (cs, ps) = (child.value("stakes"), parent.value("stakes"));
    if rank(cs) < rank(ps) {
        out.push(format!(
            "stakes: child '{}' downgrades parent '{}'",
            cs.as_str().unwrap_or_default(),
            ps.as_str().unwrap_or_default()
        ));
    }
    for field in ["invariants", "postconditions"] {
        let child_keys: BTreeSet<String> = list_of(child.value(field)).iter().map(dump_json).collect();
        let missing: BTreeSet<String> =
            list_of(parent.value(field)).iter().map(dump_json).filter(|k| !child_keys.contains(k)).collect();
        if !missing.is_empty() {
            // Sorted, as the oracle lists them (K1-5).
            let parts: Vec<String> = missing.iter().map(|k| repr(k)).collect();
            out.push(format!("{field}: child is missing parent {field} frozenset({{{}}})", parts.join(", ")));
        }
    }
    out
}

/// One (x, y, z) of a box as floats, and as Python prints the tuple.
fn triple(v: &Value) -> ([f64; 3], String) {
    let mut f = [0.0; 3];
    let xs = list_of(v);
    let mut parts = vec![];
    for (i, x) in xs.iter().enumerate() {
        if i < 3 {
            f[i] = float_of(x).unwrap_or(0.0);
        }
        parts.push(pyrepr(x));
    }
    (f, format!("({})", parts.join(", ")))
}

fn box_repr(v: &Value) -> ([f64; 3], [f64; 3], String) {
    let xs = list_of(v);
    if xs.len() != 2 {
        return ([0.0; 3], [0.0; 3], pyrepr(v));
    }
    let (lo, a) = triple(&xs[0]);
    let (hi, b) = triple(&xs[1]);
    (lo, hi, format!("({a}, {b})"))
}

/// `subsumption._robot_capability_violation(outer, inner)`, worded as the
/// oracle words it.
fn robot_relaxed(outer: &Object, inner: &Object) -> Option<String> {
    let ob = outer.value("workspace_bounds");
    if !ob.is_null() {
        let ib = inner.value("workspace_bounds");
        if ib.is_null() {
            return Some(
                "inner declares no workspace_bounds but outer constrains the workspace (undeclared = unbounded → denied)".into(),
            );
        }
        let (omin, omax, ot) = box_repr(ob);
        let (imin, imax, it) = box_repr(ib);
        for k in 0..3 {
            if imin[k] < omin[k] || imax[k] > omax[k] {
                return Some(format!("inner workspace_bounds {it} exceed outer {ot}"));
            }
        }
    }
    for axis in ["velocity_limit", "torque_limit"] {
        let ov = outer.value(axis);
        if ov.is_null() {
            continue;
        }
        let iv = inner.value(axis);
        if iv.is_null() {
            return Some(format!("inner declares no {axis} but outer caps it (undeclared → denied)"));
        }
        if float_of(iv).unwrap_or(0.0) > float_of(ov).unwrap_or(0.0) {
            return Some(format!("inner {axis} {} exceeds outer {}", pyrepr(iv), pyrepr(ov)));
        }
    }
    let oj = obj(outer.value("joint_limits"));
    let ij = obj(inner.value("joint_limits"));
    for joint in oj.keys() {
        let Some(ir) = ij.get(joint) else {
            return Some(format!("inner does not bound joint {} that outer limits (undeclared → denied)", repr(joint)));
        };
        let or = list_of(oj.value(joint));
        let irl = list_of(ir);
        if or.len() != 2 || irl.len() != 2 {
            continue;
        }
        let f = |v: &Value| float_of(v).unwrap_or(0.0);
        if f(&irl[0]) < f(&or[0]) || f(&irl[1]) > f(&or[1]) {
            return Some(format!(
                "inner joint {} range ({},{}) exceeds outer ({},{})",
                repr(joint),
                pyrepr(&irl[0]),
                pyrepr(&irl[1]),
                pyrepr(&or[0]),
                pyrepr(&or[1])
            ));
        }
    }
    // A region as the set of its six floats, -0.0 read as 0.0, as tuple
    // equality reads it.
    let freeze = |v: &Value| -> BTreeSet<[u64; 6]> {
        list_of(v)
            .iter()
            .map(|b| {
                let (lo, hi, _) = box_repr(b);
                let mut k = [0u64; 6];
                for i in 0..3 {
                    k[i] = (lo[i] + 0.0).to_bits();
                    k[i + 3] = (hi[i] + 0.0).to_bits();
                }
                k
            })
            .collect()
    };
    let o = freeze(outer.value("obstacles"));
    let i = freeze(inner.value("obstacles"));
    let missing = o.iter().filter(|k| !i.contains(*k)).count();
    if missing > 0 {
        return Some(format!(
            "inner omits {missing} obstacle region(s) the outer forbids (undeclared forbidden region → denied)"
        ));
    }
    None
}
