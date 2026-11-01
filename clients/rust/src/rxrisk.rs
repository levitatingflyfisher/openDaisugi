//! `mcp_rules.MAX_RULE_STRING_CHARS`, `flat_name`, `flat_pattern` and
//! `host_named_rules`: the string cap on an MCP argument rule and the loose
//! form of a host-flattened tool name. The rule regex engine is RE2
//! (`rulere.rs`).

/// `mcp_rules.MAX_RULE_STRING_CHARS`: a rule-checked string longer than this
/// (in characters) denies.
pub const MAX_RULE_STRING_CHARS: usize = 4096;

/// `mcp_rules.flat_name`: ASCII letters lowercased, ASCII digits kept, every
/// other code point `_`.
pub fn flat_name(s: &str) -> String {
    s.chars()
        .map(|c| if c.is_ascii_alphanumeric() { c.to_ascii_lowercase() } else { '_' })
        .collect()
}

/// `mcp_rules.flat_pattern`: a rule pattern in the same form. `* ? [ ]` stay,
/// and inside `[...]` so do `- ! ^`.
pub fn flat_pattern(p: &str) -> String {
    let mut out = String::new();
    let mut in_class = false;
    for c in p.chars() {
        if in_class {
            if c == ']' {
                in_class = false;
                out.push(c);
            } else if "-!^".contains(c) {
                out.push(c);
            } else {
                out.push_str(&flat_name(&c.to_string()));
            }
        } else if c == '[' {
            in_class = true;
            out.push(c);
        } else if c == '*' || c == '?' {
            out.push(c);
        } else {
            out.push_str(&flat_name(&c.to_string()));
        }
    }
    out
}

/// `mcp_rules.host_named_rules`, less the rules that match the key already
/// (the oracle skips those as duplicates): the indexes into `tools` of the
/// rules whose pattern names this host-flattened tool once both sides are in
/// the flat form. Only the servers `opencode` and `pi` are flattened.
/// `allowed(head, pattern)` is the glob match.
pub fn host_rule_indexes<E>(
    tools: &[&str],
    key: &str,
    allowed: &dyn Fn(&str, &str) -> Result<bool, E>,
) -> Result<Vec<usize>, E> {
    let Some((server, flat)) = key.split_once('/') else { return Ok(vec![]) };
    if flat.is_empty() || !matches!(server, "opencode" | "pi") {
        return Ok(vec![]);
    }
    let flat = flat_name(flat);
    let mut out = Vec::new();
    for (i, t) in tools.iter().enumerate() {
        if allowed(&flat, &flat_pattern(t))? && !allowed(key, t)? {
            out.push(i);
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flat_names_compare_loosely() {
        assert_eq!(flat_name("GitHub-get_issue"), "github_get_issue");
        assert_eq!(flat_name("\u{e9}"), "_");
        assert_eq!(flat_pattern("Cased/Tool"), "cased_tool");
        assert_eq!(flat_pattern("my-srv/[a-z]*"), "my_srv_[a-z]*");
    }
}
