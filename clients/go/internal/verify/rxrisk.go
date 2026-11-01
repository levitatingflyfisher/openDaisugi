package verify

import (
	"strings"
)

// Helpers of the MCP argument rules (mcp_rules.py): the string cap and the
// host-name forms. The rule regex engine is RE2 (rulere.go).

// MaxRuleStringChars is mcp_rules.MAX_RULE_STRING_CHARS: a rule-checked
// string longer than this (in characters) denies.
const MaxRuleStringChars = 4096

// FlatName is mcp_rules.flat_name: ASCII letters lowercased, ASCII digits
// kept, every other code point "_".
func FlatName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// FlatPattern is mcp_rules.flat_pattern: a rule pattern in the same form;
// "* ? [ ]" stay, and inside "[...]" so do "- ! ^".
func FlatPattern(p string) string {
	var b strings.Builder
	inClass := false
	for _, r := range p {
		switch {
		case inClass && r == ']':
			inClass = false
			b.WriteRune(r)
		case inClass && strings.ContainsRune("-!^", r):
			b.WriteRune(r)
		case inClass:
			b.WriteString(FlatName(string(r)))
		case r == '[':
			inClass = true
			b.WriteRune(r)
		case r == '*' || r == '?':
			b.WriteRune(r)
		default:
			b.WriteString(FlatName(string(r)))
		}
	}
	return b.String()
}

// HostRuleIndexes is mcp_rules.host_named_rules, less the rules that match
// the key already (the oracle skips those as duplicates): the indexes into
// tools of the rules whose pattern names this host-flattened tool once both
// sides are in the flat form. Only the servers opencode and pi are flattened.
func HostRuleIndexes(tools []string, key string) []int {
	server, flat, found := strings.Cut(key, "/")
	if !found || flat == "" || (server != "opencode" && server != "pi") {
		return nil
	}
	flat = FlatName(flat)
	var out []int
	for i, t := range tools {
		if headAllowed(flat, []string{FlatPattern(t)}) && !headAllowed(key, []string{t}) {
			out = append(out, i)
		}
	}
	return out
}
