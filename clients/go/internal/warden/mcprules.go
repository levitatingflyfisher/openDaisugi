package warden

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// This file is opendaisugi/mcp_rules.py and verify._check_mcp_step: argument
// rules for MCP calls (plan w3-mcp-args-and-parked, MP-1 to MP-10). Every
// message is the oracle's, byte for byte.

// spawnToolKey is verify._SPAWN_TOOL_KEY: the delegate's spawn tool, whose path
// the warden reads with a check of its own.
const spawnToolKey = "opendaisugi/spawn"

const (
	maxMCPArgumentsChars = 256 * 1024
	maxMCPArgumentsDepth = 64
)

// mcpRule is models.McpRule: the tool pattern, the parsed expressions, and
// the known keys when the rule names them.
type mcpRule struct {
	Tool     string
	Args     []any
	Known    []string
	KnownSet bool
}

func mcpRulesFrom(v any) []mcpRule {
	var out []mcpRule
	for _, x := range v.([]any) {
		o := x.(*pyjson.Object)
		rule := mcpRule{Tool: o.Value("tool").(string), Args: o.Value("args").([]any)}
		if k := o.Value("known"); k != nil {
			rule.KnownSet = true
			rule.Known = strs(k)
		}
		out = append(out, rule)
	}
	return out
}

// placeArgPath is mcp_rules.place_arg_path: ok is false when the path
// cannot be placed.
func placeArgPath(value, cwd string) (string, bool) {
	if value == "" {
		return "", false
	}
	if uriScheme(value) || strings.ContainsAny(value, "\\\x00") ||
		strings.ContainsRune(pathSpace, rune(value[0])) || strings.ContainsRune(pathSpace, rune(value[len(value)-1])) {
		return "", false
	}
	// A relative value is never placed: the server's working directory is
	// not the hook's (owner, 2026-11-03).
	if isabs(value) {
		return normpath(value), true
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

// ruledUnderHostName is mcp_rules.ruled_under_host_name: the tool pattern of
// a rule that names this host-flattened tool (opencode/<id>, pi/<name>) when
// only a wildcard allowlist entry admits the call, else "".
func ruledUnderHostName(env *envelope, key string) string {
	server, flat, found := strings.Cut(key, "/")
	if !found || flat == "" || (server != "opencode" && server != "pi") {
		return ""
	}
	for _, a := range env.McpAllowlist {
		if a == key {
			return ""
		}
	}
	for _, rule := range env.McpRules {
		if verify.HeadAllowed(verify.FlatName(flat), []string{verify.FlatPattern(rule.Tool)}) {
			return rule.Tool
		}
	}
	return ""
}

// ruleRegexes is mcp_rules.rule_regexes over the pyjson expression.
func ruleRegexes(e any, out *[]string) {
	o, ok := e.(*pyjson.Object)
	if !ok {
		return
	}
	switch opOf(e) {
	case "matches", "not_matches":
		if rx, ok := o.Value("regex").(string); ok {
			*out = append(*out, rx)
		}
	case "and", "or":
		if kids, ok := o.Value("children").([]any); ok {
			for _, c := range kids {
				ruleRegexes(c, out)
			}
		}
	case "not":
		ruleRegexes(o.Value("child"), out)
	case "implies":
		ruleRegexes(o.Value("a"), out)
		ruleRegexes(o.Value("b"), out)
	}
}

// firstInvalid is the first regex of e that RE2 cannot compile.
func firstInvalid(e any) (regex string, bad bool) {
	var all []string
	ruleRegexes(e, &all)
	for _, rx := range all {
		if !verify.RuleRegexValid(rx) {
			return rx, true
		}
	}
	return "", false
}

func exprPaths(e any, out *[]string) {
	o, ok := e.(*pyjson.Object)
	if !ok {
		return
	}
	switch opOf(e) {
	case "and", "or":
		for _, c := range o.Value("children").([]any) {
			exprPaths(c, out)
		}
	case "not":
		exprPaths(o.Value("child"), out)
	case "implies":
		exprPaths(o.Value("a"), out)
		exprPaths(o.Value("b"), out)
	default:
		if p, ok := o.Value("path").(string); ok {
			*out = append(*out, p)
		}
	}
}

// derivedKeys are the first segments of the paths the rule's expressions
// name, in order, once each.
func (rule mcpRule) derivedKeys() []string {
	var paths []string
	for _, e := range rule.Args {
		exprPaths(e, &paths)
	}
	var seen []string
	for _, p := range paths {
		key := strings.SplitN(p, ".", 2)[0]
		dup := false
		for _, s := range seen {
			dup = dup || s == key
		}
		if !dup {
			seen = append(seen, key)
		}
	}
	return seen
}

func (rule mcpRule) knownKeys() map[string]bool {
	out := map[string]bool{}
	keys := rule.derivedKeys()
	if rule.KnownSet {
		keys = rule.Known
	}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func describeExpr(e any) string {
	o, _ := e.(*pyjson.Object)
	op := opOf(e)
	if o != nil {
		if p, ok := o.Value("path").(string); ok {
			return fmt.Sprintf("%s on '%s'", op, p)
		}
	}
	return op
}

func matchingRules(rules []mcpRule, key string) []mcpRule {
	var out []mcpRule
	for _, rule := range rules {
		if verify.HeadAllowed(key, []string{rule.Tool}) {
			out = append(out, rule)
		}
	}
	return out
}

func tooDeep(v any, depth int) bool {
	switch x := v.(type) {
	case *pyjson.Object:
		depth++
		if depth > maxMCPArgumentsDepth {
			return true
		}
		for _, k := range x.Keys() {
			if tooDeep(x.Value(k), depth) {
				return true
			}
		}
	case []any:
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

// checkMCPCaps is mcp_rules.check_caps: "" when the arguments are within
// both caps.
func checkMCPCaps(args any) string {
	if utf8.RuneCountInString(pyjson.Canonical(args)) > maxMCPArgumentsChars {
		return fmt.Sprintf("mcp arguments are longer than %d characters; the warden does not read them", maxMCPArgumentsChars)
	}
	if tooDeep(args, 0) {
		return fmt.Sprintf("mcp arguments are nested deeper than %d levels", maxMCPArgumentsDepth)
	}
	return ""
}

// evaluateMCPRules is mcp_rules.evaluate_rules.
func (r *runner) evaluateMCPRules(rules []mcpRule, server, tool string, args any, env *envelope) []string {
	key := server + "/" + tool
	matching := matchingRules(rules, key)
	// A host that joins the server and tool names hides the rule a flat name
	// belongs to: those rules apply too (host_rules in the oracle).
	tools := make([]string, len(env.McpRules))
	for i, rl := range env.McpRules {
		tools[i] = rl.Tool
	}
	for _, i := range verify.HostRuleIndexes(tools, key) {
		matching = append(matching, env.McpRules[i])
	}
	prefix := "MCP tool '" + key + "': "
	if len(matching) == 0 {
		return nil
	}
	if c := checkMCPCaps(args); c != "" {
		return []string{prefix + c}
	}
	obj, isObj := args.(*pyjson.Object)
	if !isObj {
		return []string{prefix + "arguments are not an object"}
	}
	known := map[string]bool{}
	for _, rule := range matching {
		for k := range rule.knownKeys() {
			known[k] = true
		}
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range obj.Keys() {
		if !known[k] {
			out = append(out, prefix+fmt.Sprintf("argument '%s' is not named by its rule (known: %s)", k, strings.Join(names, ", ")))
		}
	}
	if len(out) > 0 {
		return out
	}
	prevEnv, prevCwd := r.argEnv, r.argCwd
	r.argEnv, r.argCwd = env, r.dialectBase
	defer func() { r.argEnv, r.argCwd = prevEnv, prevCwd }()
	for _, rule := range matching {
		for _, e := range rule.Args {
			if rx, bad := firstInvalid(e); bad {
				out = append(out, prefix+"argument rule refused: "+verify.InvalidRegexReason(rx))
				continue
			}
			ok := false
			if exc := catch(func() { ok = r.evalScalar(e, obj, 20) }); exc != nil {
				if exc.Type == "RuleRefused" {
					out = append(out, prefix+exc.Msg)
					continue
				}
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

// evalPathIn is predicate_z3._eval_path_in.
func (r *runner) evalPathIn(o *pyjson.Object, scope any) bool {
	if r.argEnv == nil {
		return false
	}
	val := resolvePath(scope, o.Value("path").(string))
	if val == pyMissing {
		return false
	}
	items, isList := val.([]any)
	if !isList {
		items = []any{val}
	}
	if len(items) == 0 {
		return false
	}
	globs := r.argEnv.FileRead
	if o.Value("scope").(string) == "file_write" {
		globs = r.argEnv.FileWrite
	}
	for _, item := range items {
		s, isStr := item.(string)
		if !isStr {
			return false
		}
		if !strings.HasPrefix(s, "/") {
			panic(pystr.NewException("RuleRefused", fmt.Sprintf("%s: argument '%s' is %s, which is not an absolute path; give the tool an absolute path",
				verify.PathInAbsolute, o.Value("path").(string), pyjson.Dumps(s, true))))
		}
		placed, ok := placeArgPath(s, r.argCwd)
		if !ok || !r.pathMatches(placed, globs) {
			return false
		}
	}
	return true
}

type pathArg struct{ name, path string }

// pathShapedStrings is mcp_rules.path_shaped_strings.
func pathShapedStrings(args any, home string, haveHome bool) []pathArg {
	var out []pathArg
	var walk func(v any, name string)
	walk = func(v any, name string) {
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, "/") {
				out = append(out, pathArg{name, x})
			} else if strings.HasPrefix(x, "~/") && haveHome && home != "" {
				out = append(out, pathArg{name, strings.TrimRight(home, "/") + "/" + x[2:]})
			}
		case *pyjson.Object:
			for _, k := range x.Keys() {
				n := k
				if name != "" {
					n = name + "." + k
				}
				walk(x.Value(k), n)
			}
		case []any:
			for i, e := range x {
				walk(e, fmt.Sprintf("%s[%d]", name, i))
			}
		}
	}
	walk(args, "")
	return out
}

// checkMCPStep is verify._check_mcp_step.
func (r *runner) checkMCPStep(rec *record, env *envelope, step string) []violation {
	key := rec.MCPServer + "/" + rec.MCPTool
	deny := func(message string, extra ...any) violation {
		return violation{Stage: "permissions", Message: "Step '" + step + "' " + message,
			Detail: kv(append([]any{"step", step, "mcp_tool", key}, extra...)...)}
	}
	var args any = pyjson.NewObject()
	if rec.HasRawArguments {
		args = rec.RawArguments // the host sent no object (MCPStep.call_arguments)
	} else if rec.Arguments != nil {
		args = rec.Arguments
	}
	if c := checkMCPCaps(args); c != "" {
		return []violation{deny("MCP tool '" + key + "': " + c)}
	}
	if matching := matchingRules(env.McpRules, key); len(matching) > 0 {
		if found := r.evaluateMCPRules(matching, rec.MCPServer, rec.MCPTool, args, env); len(found) > 0 {
			var vs []violation
			for _, m := range found {
				vs = append(vs, deny(m))
			}
			return vs
		}
	} else if !verify.HeadAllowed(key, env.McpAllowlist) {
		return []violation{deny(fmt.Sprintf("MCP tool '%s' not in mcp_allowlist %s", key, pyjson.ReprList(env.McpAllowlist)))}
	} else if ruled := ruledUnderHostName(env, key); ruled != "" {
		return []violation{deny(fmt.Sprintf("MCP tool '%s' has an argument rule under '%s'; the host names the tool '%s', so write the rule with that name", key, ruled, key))}
	}
	var out []violation
	home, haveHome := r.env["HOME"]
	for _, pa := range pathShapedStrings(args, home, haveHome) {
		if key == spawnToolKey && pa.name == "path" {
			continue // the warden's spawn check reads this file, with its own tier and words
		}
		if !lexists(pa.path) {
			continue
		}
		norm := normpath(pa.path)
		var ok bool
		if isDir(pa.path) {
			ok = r.pathMatches(norm, env.FileRead) && r.pathMatches(norm+"/_/_", env.FileRead)
		} else {
			ok = r.pathMatches(norm, env.FileRead)
		}
		if !ok {
			out = append(out, deny(fmt.Sprintf("MCP tool '%s': argument '%s' reads %s outside file_read %s; name it in mcp_rules with path_in to allow it",
				key, pa.name, pa.path, pyjson.ReprList(env.FileRead)), "argument", pa.name))
		}
	}
	return out
}
