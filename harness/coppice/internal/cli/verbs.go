package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// flagSpec is one flag a verb accepts: the wire parameter it sets, its kind,
// and for a bool flag, the value presence itself sets.
type flagSpec struct {
	wire  string
	kind  string // "string" (default), "int", "bool"
	value any    // used only when kind == "bool"
}

// verbSpec is everything the parser needs to know about one group-and-verb
// pair: whether its first positional is a pane id, what flags it accepts,
// what its remaining positionals mean, and whether it takes a command after
// --.
type verbSpec struct {
	pane        bool
	flags       map[string]flagSpec
	trailing    string // "", "join" (positionals joined with a space), or "list"
	trailingKey string
	argv        bool
}

// verbSpecs is the one table every verb's flags and shape come from. An
// unknown flag is refused by name rather than silently sent to the wire,
// where the server would ignore it without a word.
var verbSpecs = map[string]map[string]verbSpec{
	"workspace": {
		"create": {flags: map[string]flagSpec{
			"cwd":   {wire: "cwd", kind: "string"},
			"label": {wire: "label", kind: "string"},
		}},
		"list": {},
	},
	"tab": {
		"create": {flags: map[string]flagSpec{
			"label":     {wire: "label", kind: "string"},
			"workspace": {wire: "workspace", kind: "string"},
		}},
		"list": {flags: map[string]flagSpec{
			"workspace": {wire: "workspace", kind: "string"},
		}},
	},
	"pane": {
		"create": {argv: true, flags: map[string]flagSpec{
			"cwd":       {wire: "cwd", kind: "string"},
			"label":     {wire: "label", kind: "string"},
			"kind":      {wire: "kind", kind: "string"},
			"harness":   {wire: "harness", kind: "string"},
			"cols":      {wire: "cols", kind: "int"},
			"rows":      {wire: "rows", kind: "int"},
			"workspace": {wire: "workspace", kind: "string"},
			"tab":       {wire: "tab", kind: "string"},
		}},
		"list": {},
		"send-text": {pane: true, trailing: "join", trailingKey: "text", flags: map[string]flagSpec{
			"no-enter": {wire: "enter", kind: "bool", value: false},
		}},
		"send-keys": {pane: true, trailing: "list", trailingKey: "keys"},
		"run":       {pane: true, trailing: "join", trailingKey: "line"},
		"read":      {pane: true, flags: map[string]flagSpec{"source": {wire: "source", kind: "string"}}},
		"close":     {pane: true},
		"wait-output": {pane: true, flags: map[string]flagSpec{
			"contains": {wire: "contains", kind: "string"},
			"state":    {wire: "state", kind: "string"},
			"timeout":  {wire: "timeout_ms", kind: "int"},
		}},
		"resize": {pane: true, flags: map[string]flagSpec{
			"cols": {wire: "cols", kind: "int"},
			"rows": {wire: "rows", kind: "int"},
		}},
		"explain": {pane: true},
	},
	"agent": {
		"list": {},
		"get":  {pane: true},
		"prompt": {pane: true, trailing: "join", trailingKey: "text", flags: map[string]flagSpec{
			"wait":    {wire: "wait", kind: "bool", value: true},
			"until":   {wire: "until", kind: "string"},
			"timeout": {wire: "timeout_ms", kind: "int"},
		}},
		"wait": {pane: true, flags: map[string]flagSpec{
			"until":   {wire: "until", kind: "string"},
			"timeout": {wire: "timeout_ms", kind: "int"},
		}},
		"read": {pane: true, flags: map[string]flagSpec{"region": {wire: "region", kind: "string"}}},
	},
	// session is an internal alias for pane.list/pane.close's session.*
	// twins. It stays out of the usage text; the CLI accepts it for a caller
	// that already knows the wire names.
	"session": {
		"list": {},
		"stop": {pane: true},
	},
}

// serverFlags is the known-flag set for each server subcommand, parsed the
// same way verbSpecs is for every other group: an unknown flag is refused by
// name instead of silently starting the wrong thing. start takes only
// --foreground; the other three take only --json.
var serverFlags = map[string]map[string]bool{
	"start":  {"foreground": true, "json": true},
	"stop":   {"json": true},
	"status": {"json": true},
	"token":  {"json": true},
}

// parseServerFlags checks argv against serverFlags[sub]. errMsg is empty on
// success; foreground and asJSON report which known flags were present.
func parseServerFlags(sub string, argv []string) (foreground, asJSON bool, errMsg string) {
	known := serverFlags[sub]
	for _, a := range argv {
		if !strings.HasPrefix(a, "--") {
			return false, false, fmt.Sprintf("server %s does not take %q", sub, a)
		}
		name := strings.TrimPrefix(a, "--")
		if !known[name] {
			out := make([]string, 0, len(known))
			for k := range known {
				out = append(out, "--"+k)
			}
			sort.Strings(out)
			return false, false, fmt.Sprintf("unknown flag --%s for server %s. Known flags: %s",
				name, sub, strings.Join(out, ", "))
		}
		switch name {
		case "foreground":
			foreground = true
		case "json":
			asJSON = true
		}
	}
	return foreground, asJSON, ""
}

func knownVerbs(group string) string {
	specs := verbSpecs[group]
	out := make([]string, 0, len(specs))
	for v := range specs {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func knownFlags(spec verbSpec) string {
	out := make([]string, 0, len(spec.flags)+1)
	for f := range spec.flags {
		out = append(out, "--"+f)
	}
	sort.Strings(out)
	out = append(out, "--json")
	return strings.Join(out, ", ")
}

// parseVerbArgs turns a verb's own argument list into wire parameters. It is
// a pure function of group, verb and rest, so it can be tested without a
// socket. errMsg is empty on success; every other return value is
// meaningless when it is not.
func parseVerbArgs(group, verb string, rest []string) (params map[string]any, asJSON bool, errMsg string) {
	groupSpecs, ok := verbSpecs[group]
	if !ok {
		return nil, false, fmt.Sprintf("unknown command group %q", group)
	}
	spec, ok := groupSpecs[verb]
	if !ok {
		return nil, false, fmt.Sprintf("unknown %s verb %q. Known verbs: %s", group, verb, knownVerbs(group))
	}

	params = map[string]any{}
	var positional []string
	haveArgv := false
	var cmdArgv []string

	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			if !spec.argv {
				return nil, false, fmt.Sprintf("%s %s does not take a command after --", group, verb)
			}
			cmdArgv = append([]string{}, rest[i+1:]...)
			haveArgv = true
			i = len(rest)
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--"):
			name := strings.TrimPrefix(a, "--")
			fspec, known := spec.flags[name]
			if !known {
				return nil, false, fmt.Sprintf("unknown flag --%s for %s %s. Known flags: %s",
					name, group, verb, knownFlags(spec))
			}
			if fspec.kind == "bool" {
				params[fspec.wire] = fspec.value
				// A bool flag immediately followed by a bare number is
				// almost certainly a mistyped value-taking flag, not free
				// text: agent prompt w1:p1 --wait 5000 used to set wait
				// true and silently join "5000" into the prompt text.
				if i+1 < len(rest) {
					if n, err := strconv.Atoi(rest[i+1]); err == nil {
						if _, hasTimeout := spec.flags["timeout"]; hasTimeout {
							return nil, false, fmt.Sprintf(
								"--%s takes no value. Did you mean --timeout %d?", name, n)
						}
						return nil, false, fmt.Sprintf(
							"--%s takes no value, but %d looks like one for %s %s",
							name, n, group, verb)
					}
				}
				continue
			}
			if i+1 >= len(rest) {
				return nil, false, fmt.Sprintf("--%s needs a value", name)
			}
			val := rest[i+1]
			i++
			switch fspec.kind {
			case "int":
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, false, fmt.Sprintf("--%s needs a number, got %q", name, val)
				}
				params[fspec.wire] = n
			default:
				params[fspec.wire] = val
			}
		default:
			positional = append(positional, a)
		}
	}
	if haveArgv {
		params["cmd_argv"] = cmdArgv
	}
	if spec.pane {
		if len(positional) == 0 {
			return nil, false, fmt.Sprintf("%s %s needs a pane id. Run: coppice pane list", group, verb)
		}
		params["pane"] = positional[0]
		positional = positional[1:]
	}
	switch spec.trailing {
	case "join":
		params[spec.trailingKey] = strings.Join(positional, " ")
	case "list":
		if len(positional) > 0 {
			params[spec.trailingKey] = positional
		}
	}
	return params, asJSON, ""
}

// socketCommand turns "pane send-text" into the wire command "pane.send_text".
func socketCommand(group, verb string) string {
	return group + "." + strings.ReplaceAll(verb, "-", "_")
}

// pluralKey names the list a verb's reply carries, so printHuman can render a
// table instead of falling back to one line per scalar key.
func pluralKey(group, verb string) string {
	if verb != "list" {
		return ""
	}
	switch group {
	case "pane", "session":
		return "panes"
	case "agent":
		return "agents"
	case "workspace":
		return "workspaces"
	case "tab":
		return "tabs"
	}
	return ""
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return "-"
}

func orDash(v any) any {
	if v == nil || v == "" {
		return "-"
	}
	return v
}
