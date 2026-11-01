package wardenroot

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/statefile"
	"daisugi-verify/internal/verify"
)

// This file is opendaisugi/mcp_schemas.py and the lint half of
// opendaisugi/mcp_rules.py: the tools/list results on file, and the check
// `warden register` runs on an envelope's mcp_rules (MP-5, MP-8).

var serverRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// SchemaError is a refusal of schemas import, in the oracle's words.
type SchemaError struct{ Text string }

func (e *SchemaError) Error() string { return e.Text }

func validToolsList(v any) (*pyjson.Object, bool) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	tools, ok := o.Value("tools").([]any)
	if !ok {
		return nil, false
	}
	for _, t := range tools {
		to, ok := t.(*pyjson.Object)
		if !ok {
			return nil, false
		}
		if _, ok := to.Value("name").(string); !ok {
			return nil, false
		}
		if _, ok := to.Value("inputSchema").(*pyjson.Object); !ok {
			return nil, false
		}
	}
	return o, true
}

const notToolsList = `not a tools/list result: expected {"tools": [{"name": ..., "inputSchema": {...}}]}`

// ImportSchema is mcp_schemas.import_schema: it stores the tools/list result
// in src as the schema of server and returns the path and the tool count.
func ImportSchema(root, server, src string) (string, int, error) {
	if !serverRE.MatchString(server) || server == "." || server == ".." {
		return "", 0, &SchemaError{fmt.Sprintf("not a server name: %s", pyjson.Repr(server))}
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", 0, &SchemaError{"not a tools/list result: " + err.Error()}
	}
	v, jerr := pyjson.Loads(string(raw))
	if jerr != nil {
		return "", 0, &SchemaError{"not a tools/list result: " + jerr.Error()}
	}
	o, ok := validToolsList(v)
	if !ok {
		return "", 0, &SchemaError{notToolsList}
	}
	if _, err := statefile.WriteFile(root, []string{"schemas"}, server+".json", pyjson.DumpsIndent(o, 2, false)+"\n", false); err != nil {
		return "", 0, err
	}
	return Join(Join(root, "schemas"), server+".json"), len(o.Value("tools").([]any)), nil
}

// LoadedSchema is one schema file read back.
type LoadedSchema struct {
	Server string
	Obj    *pyjson.Object
}

// LoadSchemas is mcp_schemas.load_schemas, sorted by file name. A file that
// cannot be read is skipped with a warning on warn.
func LoadSchemas(root string, warn func(string)) []LoadedSchema {
	entries, err := os.ReadDir(Join(root, "schemas"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []LoadedSchema
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") || !serverRE.MatchString(strings.TrimSuffix(name, ".json")) {
			continue
		}
		raw, present, refusal, rerr := statefile.ProbeRead(root, []string{"schemas"}, name)
		var why string
		switch {
		case rerr != nil:
			why = rerr.Error()
		case !present:
			why = refusal
			if why == "" {
				why = "unreadable"
			}
		default:
			v, jerr := pyjson.Loads(string(raw))
			if jerr != nil {
				why = jerr.Error()
			} else if o, ok := validToolsList(v); !ok {
				why = notToolsList
			} else {
				out = append(out, LoadedSchema{strings.TrimSuffix(name, ".json"), o})
				continue
			}
		}
		warn(fmt.Sprintf("warden: skipped the schema file %s: %s", name, why))
	}
	return out
}

func pyStrOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case pyjson.Int:
		return x.Text
	}
	return pyjson.Canonical(v)
}

func sameKindEq(a, b any) bool {
	return reflect.TypeOf(a) == reflect.TypeOf(b) && pyjson.Canonical(a) == pyjson.Canonical(b)
}

// topLevelAlternation is mcp_rules._top_level_alternation: a | outside every
// group and character class.
func topLevelAlternation(re string) bool {
	r := []rune(re)
	depth, inClass := 0, false
	at := func(i int) rune {
		if i < len(r) {
			return r[i]
		}
		return 0
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\':
			i++
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
			if at(i+1) == '^' {
				i++
			}
			if at(i+1) == ']' {
				i++
			}
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == '|' && depth == 0:
			return true
		}
	}
	return false
}

// splitFlag is mcp_rules._split_flag: a leading (?i) and the rest.
func splitFlag(re string) (string, string) {
	if strings.HasPrefix(re, "(?i)") {
		return "(?i)", re[4:]
	}
	return "", re
}

func anchored(re string) bool {
	_, body := splitFlag(re)
	return strings.HasPrefix(body, "^") && strings.HasSuffix(body, "$") && !topLevelAlternation(body)
}

var ruleOps = map[string]bool{
	"equals": true, "not_equals": true, "in_set": true, "not_in_set": true, "matches": true,
	"not_matches": true, "numeric_range": true, "length_range": true, "exists": true,
	"is_empty": true, "path_in": true, "and": true, "or": true, "not": true, "implies": true,
}

func lintExprs(tool string, e any, refusals *[]string, perms *pyjson.Object) {
	o, ok := e.(*pyjson.Object)
	if !ok {
		return
	}
	op, _ := o.Value("op").(string)
	if !ruleOps[op] {
		*refusals = append(*refusals, fmt.Sprintf("mcp_rules: %s: op '%s' cannot be used in a rule", tool, op))
		return
	}
	if op == "path_in" && perms != nil {
		// Owner, 2026-11-03: path_in takes absolute paths only, so its
		// scope may hold no relative glob.
		scope, _ := o.Value("scope").(string)
		path, _ := o.Value("path").(string)
		globs, _ := perms.Value(scope).([]any)
		for _, g := range globs {
			if gs, ok := g.(string); ok && !strings.HasPrefix(gs, "/") {
				*refusals = append(*refusals, fmt.Sprintf(
					"mcp_rules: %s: path_in on '%s' reads %s, which holds the relative glob '%s'. %s: write the glob from /",
					tool, path, scope, gs, verify.PathInAbsolute))
			}
		}
	}
	if op == "matches" || op == "not_matches" {
		re, _ := o.Value("regex").(string)
		if !verify.RuleRegexValid(re) {
			*refusals = append(*refusals, fmt.Sprintf("mcp_rules: %s: %s", tool, verify.InvalidRegexReason(re)))
		} else {
			if !anchored(re) {
				flag, body := splitFlag(re)
				fix := flag + "^(?:" + body + ")$"
				if !topLevelAlternation(body) {
					fix = body
					if !strings.HasPrefix(body, "^") {
						fix = "^" + fix
					}
					if !strings.HasSuffix(body, "$") {
						fix += "$"
					}
					fix = flag + fix
				}
				*refusals = append(*refusals, fmt.Sprintf("mcp_rules: %s: regex %s is not anchored; write %s", tool, "'"+re+"'", fix))
			}
		}
	}
	switch op {
	case "and", "or":
		for _, c := range o.Value("children").([]any) {
			lintExprs(tool, c, refusals, perms)
		}
	case "not":
		lintExprs(tool, o.Value("child"), refusals, perms)
	case "implies":
		lintExprs(tool, o.Value("a"), refusals, perms)
		lintExprs(tool, o.Value("b"), refusals, perms)
	}
}

func lintPaths(e any, out *[]string) {
	o, ok := e.(*pyjson.Object)
	if !ok {
		return
	}
	switch o.Value("op") {
	case "and", "or":
		for _, c := range o.Value("children").([]any) {
			lintPaths(c, out)
		}
	case "not":
		lintPaths(o.Value("child"), out)
	case "implies":
		lintPaths(o.Value("a"), out)
		lintPaths(o.Value("b"), out)
	default:
		if p, ok := o.Value("path").(string); ok {
			*out = append(*out, p)
		}
	}
}

type enumUse struct {
	path  string
	value any
}

func enumUses(e any, out *[]enumUse) {
	o, ok := e.(*pyjson.Object)
	if !ok {
		return
	}
	switch o.Value("op") {
	case "and":
		for _, c := range o.Value("children").([]any) {
			enumUses(c, out)
		}
	case "equals":
		if p := o.Value("path").(string); !strings.Contains(p, ".") {
			*out = append(*out, enumUse{p, o.Value("value")})
		}
	case "in_set":
		if p := o.Value("path").(string); !strings.Contains(p, ".") {
			for _, v := range o.Value("values").([]any) {
				*out = append(*out, enumUse{p, v})
			}
		}
	}
}

// LintRules is mcp_rules.rule_lint over the validated mcp_rules list.
// perms, when not nil, is the envelope's permissions: a path_in whose scope
// holds a relative glob is refused too.
func LintRules(rules []any, schemas []LoadedSchema, perms *pyjson.Object) (refusals, notes []string) {
	byServer := map[string]*pyjson.Object{}
	for _, s := range schemas {
		byServer[s.Server] = s.Obj
	}
	for _, r := range rules {
		rule := r.(*pyjson.Object)
		tool := rule.Value("tool").(string)
		args := rule.Value("args").([]any)
		for _, e := range args {
			lintExprs(tool, e, &refusals, perms)
		}
		if strings.ContainsAny(tool, "*?[") || !strings.Contains(tool, "/") {
			continue
		}
		parts := strings.SplitN(tool, "/", 2)
		server, name := parts[0], parts[1]
		schema, ok := byServer[server]
		if !ok {
			note := fmt.Sprintf("mcp_rules: no schema on file for %s; argument names are not checked (daisugi warden schemas import)", server)
			dup := false
			for _, n := range notes {
				dup = dup || n == note
			}
			if !dup {
				notes = append(notes, note)
			}
			continue
		}
		var found *pyjson.Object
		var have []string
		for _, t := range schema.Value("tools").([]any) {
			to := t.(*pyjson.Object)
			have = append(have, to.Value("name").(string))
			if to.Value("name").(string) == name {
				found = to
			}
		}
		if found == nil {
			sort.Strings(have)
			refusals = append(refusals, fmt.Sprintf("mcp_rules: %s: the schema of %s has no tool '%s'; it has: %s",
				tool, server, name, strings.Join(have, ", ")))
			continue
		}
		props := pyjson.NewObject()
		if po, ok := found.Value("inputSchema").(*pyjson.Object).Value("properties").(*pyjson.Object); ok {
			props = po
		}
		var names []string
		if kn, ok := rule.Value("known").([]any); ok {
			for _, k := range kn {
				names = append(names, k.(string))
			}
		}
		var paths []string
		for _, e := range args {
			lintPaths(e, &paths)
		}
		for _, p := range paths {
			key := strings.SplitN(p, ".", 2)[0]
			dup := false
			for _, n := range names {
				dup = dup || n == key
			}
			if !dup {
				names = append(names, key)
			}
		}
		propNames := append([]string{}, props.Keys()...)
		sort.Strings(propNames)
		for _, n := range names {
			if _, ok := props.Get(n); !ok {
				refusals = append(refusals, fmt.Sprintf("mcp_rules: %s names argument '%s'; the schema has: %s",
					tool, n, strings.Join(propNames, ", ")))
			}
		}
		for _, e := range args {
			var uses []enumUse
			enumUses(e, &uses)
			for _, u := range uses {
				po, ok := props.Value(u.path).(*pyjson.Object)
				if !ok {
					continue
				}
				enum, ok := po.Value("enum").([]any)
				if !ok {
					continue
				}
				hit := false
				for _, ev := range enum {
					hit = hit || sameKindEq(u.value, ev)
				}
				if !hit {
					shown := make([]string, len(enum))
					for i, ev := range enum {
						shown[i] = pyStrOf(ev)
					}
					refusals = append(refusals, fmt.Sprintf("mcp_rules: %s: value '%s' for argument '%s' is not in the schema enum (%s); the rule can never pass",
						tool, pyStrOf(u.value), u.path, strings.Join(shown, ", ")))
				}
			}
		}
	}
	return refusals, notes
}
