package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is opendaisugi/mcp_rules.py and verify._check_mcp_step for the
// standalone verifier (plan w3-mcp-args-and-parked, MP-1 to MP-10). The
// warden has its own twin in internal/warden; the messages are the
// oracle's in both.

// McpRule is models.McpRule: the tool pattern, the predicate expressions
// (raw JSON, parsed when a call is checked), and the known keys when named.
type McpRule struct {
	Tool  string            `json:"tool"`
	Args  []json.RawMessage `json:"args"`
	Known *[]string         `json:"known"`
}

const (
	spawnToolKey         = "opendaisugi/spawn"
	maxMCPArgumentsChars = 256 * 1024
	maxMCPArgumentsDepth = 64
)

// kindEq is predicate_z3._kind_eq: a bool is never equal to a number.
func kindEq(a, b interface{}) bool {
	_, aBool := a.(bool)
	_, bBool := b.(bool)
	return aBool == bBool && pyEqual(a, b)
}

// pathContext is what a path_in reads while an mcp_rules entry runs: the
// envelope's globs and the call's cwd. It rides with the evaluation, as
// Python's ArgScope does, so two checks at once never see each other's.
// A nil context (an invariant, a quantifier's step) makes path_in false.
type pathContext struct {
	fileRead, fileWrite []string
	cwd                 string
}

// PlaceArgPath is mcp_rules.place_arg_path: ok is false when the path
// cannot be placed. A relative value is never placed (owner, 2026-11-03);
// cwd is not read.
func PlaceArgPath(value, cwd string) (string, bool) {
	if value == "" {
		return "", false
	}
	if uriScheme(value) || strings.ContainsAny(value, "\\\x00") ||
		strings.ContainsRune(pathSpace, rune(value[0])) || strings.ContainsRune(pathSpace, rune(value[len(value)-1])) {
		return "", false
	}
	if strings.HasPrefix(value, "/") {
		return posixNormpath(value), true
	}
	return "", false
}

// pathSpace is the white space that may not start or end a placed path.
const pathSpace = " \t\n\r\x0b\x0c"

// uriScheme is _URI_SCHEME.match: a letter, then letters, digits and + . -,
// then a colon. A Windows drive prefix is the one-letter case.
func uriScheme(s string) bool {
	if s == "" || !(s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			return true
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '.', c == '-':
		default:
			return false
		}
	}
	return false
}

// PathInAbsolute is mcp_rules.PATH_IN_ABSOLUTE (owner, 2026-11-03): an
// MCP server runs in its own working directory, so a relative path
// argument cannot be placed.
const PathInAbsolute = "path_in takes absolute paths only"

// evalPathIn is predicate_z3._eval_path_in. A value that is not an
// absolute path is refused, not false: under a "not" a false would admit it.
func evalPathIn(e PathIn, scope map[string]interface{}, pathCtx *pathContext) (bool, error) {
	if pathCtx == nil {
		return false, nil
	}
	val, present := resolvePath(scope, e.Path)
	if !present {
		return false, nil
	}
	items, isList := val.([]interface{})
	if !isList {
		items = []interface{}{val}
	}
	if len(items) == 0 {
		return false, nil
	}
	globs := pathCtx.fileRead
	if e.Scope == "file_write" {
		globs = pathCtx.fileWrite
	}
	for _, item := range items {
		s, isStr := item.(string)
		if !isStr {
			return false, nil
		}
		if !strings.HasPrefix(s, "/") {
			return false, ruleRefused{fmt.Sprintf("%s: argument '%s' is %s, which is not an absolute path; give the tool an absolute path",
				PathInAbsolute, e.Path, pyjson.Dumps(s, true))}
		}
		placed, ok := PlaceArgPath(s, pathCtx.cwd)
		if !ok || !PathMatchesAny(placed, globs) {
			return false, nil
		}
	}
	return true, nil
}

func exprPath(e Expression) (string, bool) {
	switch x := e.(type) {
	case Equals:
		return x.Path, true
	case NotEquals:
		return x.Path, true
	case InSet:
		return x.Path, true
	case NotInSet:
		return x.Path, true
	case Matches:
		return x.Path, true
	case NotMatches:
		return x.Path, true
	case NumericRange:
		return x.Path, true
	case LengthRange:
		return x.Path, true
	case Exists:
		return x.Path, true
	case IsEmpty:
		return x.Path, true
	case PathIn:
		return x.Path, true
	}
	return "", false
}

func walkPaths(e Expression, out *[]string) {
	switch x := e.(type) {
	case And:
		for _, c := range x.Children {
			walkPaths(c, out)
		}
	case Or:
		for _, c := range x.Children {
			walkPaths(c, out)
		}
	case Not:
		walkPaths(x.Child, out)
	case Implies:
		walkPaths(x.A, out)
		walkPaths(x.B, out)
	default:
		if p, ok := exprPath(e); ok {
			*out = append(*out, p)
		}
	}
}

func (r McpRule) exprs() []Expression {
	var out []Expression
	for _, raw := range r.Args {
		if e, err := ParseExpression(raw); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// KnownKeys is mcp_rules.known_keys.
func (r McpRule) KnownKeys() map[string]bool {
	out := map[string]bool{}
	if r.Known != nil {
		for _, k := range *r.Known {
			out[k] = true
		}
		return out
	}
	var paths []string
	for _, e := range r.exprs() {
		walkPaths(e, &paths)
	}
	for _, p := range paths {
		out[strings.SplitN(p, ".", 2)[0]] = true
	}
	return out
}

func describeExpr(e Expression) string {
	if p, ok := exprPath(e); ok {
		return fmt.Sprintf("%s on '%s'", e.Op(), p)
	}
	return e.Op()
}

// MatchingRules is mcp_rules.matching_rules.
func MatchingRules(rules []McpRule, key string) []McpRule {
	var out []McpRule
	for _, r := range rules {
		if headAllowed(key, []string{r.Tool}) {
			out = append(out, r)
		}
	}
	return out
}

func tooDeep(v interface{}, depth int) bool {
	switch x := v.(type) {
	case map[string]interface{}:
		depth++
		if depth > maxMCPArgumentsDepth {
			return true
		}
		for _, e := range x {
			if tooDeep(e, depth) {
				return true
			}
		}
	case []interface{}:
		depth++
		if depth > maxMCPArgumentsDepth {
			return true
		}
		for _, e := range x {
			if tooDeep(e, depth) {
				return true
			}
		}
	}
	return false
}

// CheckMCPCaps is mcp_rules.check_caps: "" when the arguments are within
// both caps.
func CheckMCPCaps(args interface{}) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err == nil && utf8.RuneCount(buf.Bytes())-1 > maxMCPArgumentsChars {
		return fmt.Sprintf("mcp arguments are longer than %d characters; the warden does not read them", maxMCPArgumentsChars)
	}
	if tooDeep(args, 0) {
		return fmt.Sprintf("mcp arguments are nested deeper than %d levels", maxMCPArgumentsDepth)
	}
	return ""
}

// evaluateMCPRules is mcp_rules.evaluate_rules.
func evaluateMCPRules(matching []McpRule, key string, args interface{}, perms Permission, cwd string) []string {
	// A host that joins the server and tool names hides the rule a flat name
	// belongs to: those rules apply too (host_rules in the oracle).
	tools := make([]string, len(perms.McpRules))
	for i, r := range perms.McpRules {
		tools[i] = r.Tool
	}
	for _, i := range HostRuleIndexes(tools, key) {
		matching = append(matching, perms.McpRules[i])
	}
	prefix := "MCP tool '" + key + "': "
	if len(matching) == 0 {
		return nil
	}
	if c := CheckMCPCaps(args); c != "" {
		return []string{prefix + c}
	}
	obj, isObj := args.(map[string]interface{})
	if !isObj {
		return []string{prefix + "arguments are not an object"}
	}
	known := map[string]bool{}
	for _, r := range matching {
		for k := range r.KnownKeys() {
			known[k] = true
		}
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	var unknown []string
	for k := range obj {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		var out []string
		for _, k := range unknown {
			out = append(out, prefix+fmt.Sprintf("argument '%s' is not named by its rule (known: %s)", k, strings.Join(names, ", ")))
		}
		return out
	}
	pc := &pathContext{perms.FileRead, perms.FileWrite, cwd}
	var out []string
	for _, r := range matching {
		for _, raw := range r.Args {
			e, err := ParseExpression(raw)
			if err != nil {
				out = append(out, prefix+"argument rule could not be evaluated: "+opOfRaw(raw))
				continue
			}
			if rx, bad := firstInvalid(e); bad {
				out = append(out, prefix+"argument rule refused: "+InvalidRegexReason(rx))
				continue
			}
			ok, err := evalScalar(e, obj, pc)
			var refused ruleRefused
			if errors.As(err, &refused) {
				out = append(out, prefix+refused.msg)
				continue
			}
			if err != nil {
				out = append(out, prefix+"argument rule could not be evaluated: "+describeExpr(e))
				continue
			}
			if !ok {
				out = append(out, prefix+"argument rule does not hold: "+describeExpr(e))
			}
		}
	}
	return out
}

func opOfRaw(raw json.RawMessage) string {
	var o struct {
		Op string `json:"op"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Op
}

type pathArg struct{ name, path string }

// pathShapedStrings is mcp_rules.path_shaped_strings. Objects walk in key
// order as the decoder gave them (sorted: the verifier's steps are maps).
func pathShapedStrings(args interface{}, home string) []pathArg {
	var out []pathArg
	var walk func(v interface{}, name string)
	walk = func(v interface{}, name string) {
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, "/") {
				out = append(out, pathArg{name, x})
			} else if strings.HasPrefix(x, "~/") && home != "" {
				out = append(out, pathArg{name, strings.TrimRight(home, "/") + "/" + x[2:]})
			}
		case map[string]interface{}:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				n := k
				if name != "" {
					n = name + "." + k
				}
				walk(x[k], n)
			}
		case []interface{}:
			for i, e := range x {
				walk(e, fmt.Sprintf("%s[%d]", name, i))
			}
		}
	}
	walk(args, "")
	return out
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// checkMCPStep is verify._check_mcp_step.
func checkMCPStep(step Step, server, tool string, perms Permission, callCwd string) []Violation {
	key := server + "/" + tool
	deny := func(message string, extra ...any) Violation {
		return VStep("permissions", step.ID, "Step '"+step.ID+"' "+message).
			With(kv(append([]any{"step", step.ID, "mcp_tool", key}, extra...)...), nil)
	}
	var args interface{} = map[string]interface{}{}
	if a, ok := step.Raw["arguments"]; ok {
		args = a
	}
	if c := CheckMCPCaps(args); c != "" {
		return []Violation{deny("MCP tool '" + key + "': " + c)}
	}
	if matching := MatchingRules(perms.McpRules, key); len(matching) > 0 {
		if found := evaluateMCPRules(matching, key, args, perms, callCwd); len(found) > 0 {
			var vs []Violation
			for _, m := range found {
				vs = append(vs, deny(m))
			}
			return vs
		}
	} else if !headAllowed(key, perms.McpAllowlist) {
		return []Violation{deny(fmt.Sprintf("MCP tool '%s' not in mcp_allowlist %s", key, pystr.ReprList(perms.McpAllowlist)))}
	} else if ruled := RuledUnderHostName(perms, key); ruled != "" {
		return []Violation{deny(fmt.Sprintf("MCP tool '%s' has an argument rule under '%s'; the host names the tool '%s', so write the rule with that name", key, ruled, key))}
	}
	var out []Violation
	for _, pa := range pathShapedStrings(args, os.Getenv("HOME")) {
		if key == spawnToolKey && pa.name == "path" {
			continue
		}
		if _, err := os.Lstat(pa.path); err != nil {
			continue
		}
		norm := posixNormpath(pa.path)
		var ok bool
		if isDir(pa.path) {
			ok = PathMatchesAny(norm, perms.FileRead) && PathMatchesAny(norm+"/_/_", perms.FileRead)
		} else {
			ok = PathMatchesAny(norm, perms.FileRead)
		}
		if !ok {
			out = append(out, deny(fmt.Sprintf("MCP tool '%s': argument '%s' reads %s outside file_read %s; name it in mcp_rules with path_in to allow it",
				key, pa.name, pa.path, pystr.ReprList(perms.FileRead)), "argument", pa.name))
		}
	}
	return out
}

// RuledUnderHostName is mcp_rules.ruled_under_host_name: the tool pattern of
// a rule that names this host-flattened tool (opencode/<id>, pi/<name>) when
// only a wildcard allowlist entry admits the call, else "".
func RuledUnderHostName(perms Permission, key string) string {
	server, flat, found := strings.Cut(key, "/")
	if !found || flat == "" || (server != "opencode" && server != "pi") {
		return ""
	}
	for _, a := range perms.McpAllowlist {
		if a == key {
			return ""
		}
	}
	for _, rule := range perms.McpRules {
		if headAllowed(FlatName(flat), []string{FlatPattern(rule.Tool)}) {
			return rule.Tool
		}
	}
	return ""
}

func toolsDisjoint(a, b string) bool {
	lit := func(s string) bool { return !strings.ContainsAny(s, "*?[") }
	return lit(a) && lit(b) && a != b
}

// mcpRulesSubsume is subsumption._mcp_rules_subsume, narrower than the
// oracle's Z3 proof: an outer rule covers an inner one only when every
// expression of the outer rule is also an expression of the inner rule
// (equal as JSON). A proof the oracle finds by reasoning is a refusal here
// (fail closed; ruling MP-R-1). "" means every call inner admits, outer
// admits too.
func mcpRulesSubsume(z3c *Z3Client, inner, outer Permission, timeoutMs int) (string, error) {
	covers := func(pattern string, outerP []string) (bool, error) {
		reason, err := patternsSubsume(z3c, []string{pattern}, outerP, timeoutMs)
		return reason == "", err
	}
	for _, rule := range inner.McpRules {
		var overlapping []McpRule
		for _, o := range outer.McpRules {
			if !toolsDisjoint(rule.Tool, o.Tool) {
				overlapping = append(overlapping, o)
			}
		}
		if len(overlapping) == 0 {
			ok, err := covers(rule.Tool, outer.McpAllowlist)
			if err != nil {
				return "", err
			}
			if !ok {
				return "mcp_rules: " + rule.Tool + ": outer does not admit it by name or by rule", nil
			}
			continue
		}
		for _, o := range overlapping {
			ok, err := covers(rule.Tool, []string{o.Tool})
			if err != nil {
				return "", err
			}
			if !ok {
				return fmt.Sprintf("mcp_rules: %s: outer rule for %s covers only part of the tools this rule admits", rule.Tool, o.Tool), nil
			}
			outerKnown := o.KnownKeys()
			var extra []string
			for k := range rule.KnownKeys() {
				if !outerKnown[k] {
					extra = append(extra, k)
				}
			}
			if len(extra) > 0 {
				sort.Strings(extra)
				return fmt.Sprintf("mcp_rules: %s: inner known arguments %s are not in the known arguments of the outer rule for %s",
					rule.Tool, pystr.ReprList(extra), o.Tool), nil
			}
			for _, oa := range o.Args {
				found := false
				for _, ia := range rule.Args {
					found = found || bytes.Equal(canonJSON(ia), canonJSON(oa))
				}
				if !found {
					return fmt.Sprintf("mcp_rules: %s: its argument rules are not proved narrower than the outer rule for %s", rule.Tool, o.Tool), nil
				}
			}
		}
	}
	for _, pattern := range inner.McpAllowlist {
		if !strings.Contains(pattern, "*") {
			decided := false
			for _, r := range inner.McpRules {
				decided = decided || headAllowed(pattern, []string{r.Tool})
			}
			if decided {
				continue
			}
		}
		for _, o := range outer.McpRules {
			if !toolsDisjoint(pattern, o.Tool) {
				return fmt.Sprintf("mcp_rules: %s: inner admits it by name; the outer constrains %s by rule", pattern, o.Tool), nil
			}
		}
	}
	return "", nil
}

// canonJSON is raw JSON with its keys sorted and its spacing removed.
func canonJSON(raw json.RawMessage) []byte {
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// ruleRegexes is mcp_rules.rule_regexes.
func ruleRegexes(e Expression, out *[]string) {
	switch x := e.(type) {
	case Matches:
		*out = append(*out, x.Regex)
	case NotMatches:
		*out = append(*out, x.Regex)
	case And:
		for _, c := range x.Children {
			ruleRegexes(c, out)
		}
	case Or:
		for _, c := range x.Children {
			ruleRegexes(c, out)
		}
	case Not:
		ruleRegexes(x.Child, out)
	case Implies:
		ruleRegexes(x.A, out)
		ruleRegexes(x.B, out)
	}
}

// firstInvalid is the first regex of e that RE2 cannot compile.
func firstInvalid(e Expression) (regex string, bad bool) {
	var all []string
	ruleRegexes(e, &all)
	for _, rx := range all {
		if !RuleRegexValid(rx) {
			return rx, true
		}
	}
	return "", false
}
