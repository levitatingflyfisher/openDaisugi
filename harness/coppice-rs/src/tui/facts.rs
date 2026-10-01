//! The floor header row: the agents by daisugi mode, the gateway, and the
//! tokens used today, in the web page's words.

use crate::textwidth;

use super::model::{boolean, num, obj, s, Model, Obj};
use super::stackline::count_text;

/// What the header row shows from floor.facts.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Facts {
    pub enforcing: i64,
    pub watching: i64,
    pub off: i64,
    pub disarmed: bool,
    pub not_installed: bool,
    pub gateway: String,
    pub answers: String,
    pub tokens: f64,
}

/// The narrowest screen that shows the header row.
pub const FACTS_MIN_COLS: i64 = 100;

/// The line the floor shows while no harness has a daisugi gate hook.
pub const GATE_HINT: &str = "daisugi is off: no agent is guarded. Run: daisugi install --gate";

pub fn gate_hint_shown(fa: &Option<Facts>) -> bool {
    matches!(fa, Some(f) if f.not_installed && f.enforcing + f.watching == 0)
}

/// The narrowest screen that can hold the hint.
pub fn hint_min_cols() -> i64 {
    textwidth::width(GATE_HINT) as i64
}

/// Reads a floor.facts reply, or None when it has no daisugi object.
pub fn decode_facts(res: &Obj) -> Option<Facts> {
    let d = obj(res, "daisugi")?;
    let n = |m: &Obj, k: &str| -> i64 {
        let v = num(m, k).unwrap_or(0.0);
        if v < 0.0 {
            0
        } else {
            v as i64
        }
    };
    let mut f = Facts {
        enforcing: n(d, "enforcing"),
        watching: n(d, "watching"),
        off: n(d, "off"),
        ..Default::default()
    };
    if boolean(d, "armed") == Some(false) {
        f.disarmed = true;
    }
    if boolean(d, "installed") == Some(false) {
        f.not_installed = true;
    }
    if let Some(gw) = obj(res, "gateway") {
        f.gateway = s(gw, "url");
        if let Some(a) = boolean(gw, "answers") {
            f.answers = if a { "yes" } else { "no" }.into();
        }
    }
    if let Some(t) = obj(res, "tokens_today") {
        for k in ["fresh", "cache_read", "cache_write", "out"] {
            if let Some(v) = num(t, k) {
                if v > 0.0 {
                    f.tokens += v;
                }
            }
        }
    }
    Some(f)
}

/// The header row.
pub fn facts_line(m: &Model) -> String {
    let fa = m.facts.clone().unwrap_or_default();
    let mut mode = Vec::new();
    for (n, word) in [
        (fa.enforcing, "enforcing"),
        (fa.watching, "watching"),
        (fa.off, "off"),
    ] {
        if n > 0 {
            mode.push(format!("{n} {word}"));
        }
    }
    let mut daisugi = "daisugi · no agents".to_string();
    if !mode.is_empty() {
        daisugi = format!("daisugi · {}", mode.join(" · "));
    }
    if fa.disarmed {
        daisugi.push_str(" · disarmed");
    }
    let gateway = if fa.gateway.is_empty() {
        "no gateway"
    } else if fa.answers == "yes" {
        "gateway answers"
    } else if fa.answers == "no" {
        "gateway silent"
    } else {
        "gateway ?"
    };
    let working = m
        .rows
        .iter()
        .filter(|r| r.parent.is_empty() && (r.state == "working" || r.held.is_some()))
        .count();
    let need = if m.need_you == 1 {
        "1 needs you".to_string()
    } else {
        format!("{} need you", m.need_you)
    };
    let parts = [
        daisugi,
        gateway.to_string(),
        format!("{working} working"),
        need,
        format!("tokens today {}", count_text(fa.tokens)),
    ];
    textwidth::printable(&parts.join("   "), 0)
}
