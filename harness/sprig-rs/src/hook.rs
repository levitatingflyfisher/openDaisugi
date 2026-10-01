//! sprig-hook: a Claude Code PreToolUse hook that rules on a driver's own
//! tools with the gate. It reads {tool_name, tool_input} and prints the
//! decision; the caller exits 2 on a deny, an unconditional block.

use crate::gate::{Gate, ToolCall};
use crate::json::{encode, jmap, Map, J};

/// The decision JSON and whether the call is allowed. An unreadable
/// payload denies.
pub fn decide(input: &[u8], gate: &dyn Gate) -> (Vec<u8>, bool) {
    let mut name = String::new();
    let mut tool_input: Option<Map> = None;
    let r = crate::json::unmarshal(input, |d, n| {
        d.object(
            n,
            "preToolUse",
            "hook.preToolUse",
            &["tool_name", "tool_input"],
            |d, i, v| match i {
                0 => d.string(v, &mut name),
                _ => d.map_any(v, &mut tool_input),
            },
        )
    });
    if r.is_err() || name.is_empty() {
        return (
            decision(
                "deny",
                b"sprig-hook: unreadable PreToolUse payload (fail-closed)",
            ),
            false,
        );
    }
    let v = gate.check(&ToolCall {
        id: String::new(),
        name,
        input: tool_input,
    });
    if v.allow {
        return (decision("allow", b""), true);
    }
    let reason: &[u8] = if v.reason.is_empty() {
        b"refused by the gate"
    } else {
        &v.reason
    };
    (decision("deny", reason), false)
}

fn decision(kind: &str, reason: &[u8]) -> Vec<u8> {
    let mut out = vec![
        ("hookEventName", J::s("PreToolUse")),
        ("permissionDecision", J::s(kind)),
    ];
    if !reason.is_empty() {
        out.push(("permissionDecisionReason", J::s(reason)));
    }
    encode(&jmap(vec![("hookSpecificOutput", jmap(out))]))
}

/// Go's strconv.Atoi.
pub fn atoi(s: &[u8]) -> Option<i64> {
    let digits = match s.first() {
        Some(b'+') | Some(b'-') => &s[1..],
        _ => s,
    };
    if digits.is_empty() || !digits.iter().all(|c| c.is_ascii_digit()) {
        return None;
    }
    std::str::from_utf8(s).ok()?.parse::<i64>().ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::{AllowAll, Verdict};

    struct Deny(&'static [u8]);
    impl Gate for Deny {
        fn check(&self, c: &ToolCall) -> Verdict {
            Verdict {
                allow: c.name == "Read",
                reason: self.0.to_vec(),
            }
        }
    }

    #[test]
    fn decisions() {
        let (o, ok) = decide(br#"{"tool_name":"Read"}"#, &AllowAll);
        assert!(ok);
        assert_eq!(o, br#"{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}"#);
        let (o, ok) = decide(
            br#"{"TOOL_NAME":"Read","tool_name":"Bash"}"#,
            &Deny(b"no <way>"),
        );
        assert!(!ok);
        assert_eq!(
            o,
            br#"{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no \u003cway\u003e"}}"#
        );
        let (o, _) = decide(br#"{"tool_name":"Bash"}"#, &Deny(b""));
        assert!(o.ends_with(br#""permissionDecisionReason":"refused by the gate"}}"#));
        let (o, ok) = decide(br#"{"tool_name":5}"#, &AllowAll);
        assert!(!ok);
        assert!(o.ends_with(br#""sprig-hook: unreadable PreToolUse payload (fail-closed)"}}"#));
        assert_eq!(atoi(b"+5"), Some(5));
        assert_eq!(atoi(b"007"), Some(7));
        assert_eq!(atoi(b"1_0"), None);
        assert_eq!(atoi(b""), None);
        assert_eq!(atoi(b"-4"), Some(-4));
        let _ = Map::new();
        let _ = J::Null;
        let _ = encode(&jmap(vec![]));
    }
}
