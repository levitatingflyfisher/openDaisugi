package warden

import (
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// applyPatchTool is OpenCode's apply_patch under the plugin's MCP-style
// name. It writes files, so it is never treated as an MCP call.
const applyPatchTool = "mcp__opencode__apply_patch"

// codexApplyPatchTool is Codex's name for the same tool: bare, with the
// patch text in tool_input.command. Under the claude-shaped format Codex's
// hook uses, it is a patch writer too.
const codexApplyPatchTool = "apply_patch"

// isPatchCall is hook.is_patch_call.
func isPatchCall(name, fmt string) bool {
	return name == applyPatchTool || (name == codexApplyPatchTool && fmt == "claude")
}

var toolTypeMap = map[string]string{
	"Bash": "shell", "shell": "shell", "command": "shell",
	"Edit": "file_write", "Write": "file_write", "MultiEdit": "file_write",
	"Read": "file_read", "Glob": "file_read", "Grep": "file_read", "search": "file_read",
	"WebFetch": "network", "WebSearch": "network",
}

var piToolTypeMap = map[string]string{
	"bash": "shell", "powershell": "shell",
	"read": "file_read", "write": "file_write", "edit": "file_write",
}

// hermesToolTypeMap and openclawToolTypeMap are hook._HERMES_TOOL_TYPE_MAP
// and hook._OPENCLAW_TOOL_TYPE_MAP: consulted only under their own format.
var hermesToolTypeMap = map[string]string{
	"terminal": "shell", "read_file": "file_read", "search_files": "file_read",
	"write_file": "file_write", "patch": "file_write",
	"web_search": "network", "web_extract": "network",
}

var openclawToolTypeMap = map[string]string{
	"exec": "shell", "read": "file_read", "write": "file_write", "edit": "file_write",
	"apply_patch": "file_write", "web_fetch": "network", "web_search": "network",
}

// joinKeys is hook.JOIN_KEYS, in order.
var joinKeys = []string{
	"tool_use_id", "agent_id", "agent_type", "cwd", "transcript_path",
	"hook_event_name", "permission_mode", "turn_id", "model",
}

// classifyTool is hook._classify_tool.
func classifyTool(name, fmt string) string {
	if strings.HasPrefix(name, "mcp__") {
		return "mcp"
	}
	if t, ok := toolTypeMap[name]; ok {
		return t
	}
	if fmt == "pi" {
		if t, ok := piToolTypeMap[name]; ok {
			return t
		}
		return "mcp"
	}
	if fmt == "hermes" {
		return hermesToolTypeMap[name]
	}
	if fmt == "openclaw" {
		return openclawToolTypeMap[name]
	}
	return ""
}

// parseMCPToolName is hook._parse_mcp_tool_name.
func parseMCPToolName(name string) (string, string, bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	server, tool, ok := strings.Cut(name[len("mcp__"):], "__")
	if !ok || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// record is one normalized capture record, hook._payload_to_record's dict.
type record struct {
	ToolName string
	StepType string
	// SearchPaths are the paths a file read's input names under
	// file_path, path and filePath, in that order (search_rule.search_paths).
	// BadPath is true when one of them is truthy but not a string.
	SearchPaths []string
	BadPath     bool
	// Command, Path and URL are str(value or "") of the record's fields,
	// the form the rules and the log lines read. The *Raw fields are the
	// values themselves, which a step model validates.
	Command    string
	Path       string
	URL        string
	CommandRaw any
	PathRaw    any
	URLRaw     any
	MCPServer  string
	MCPTool    string
	Arguments  *pyjson.Object
	// RawArguments is what the host sent as the call's arguments when that
	// was no object (a list, a string, a number, null); Arguments is then
	// empty. An argument rule reads it and denies (hook._mcp_call_arguments).
	RawArguments    any
	HasRawArguments bool
	// obj is the record as Python builds it, key for key, with
	// captured_at set when it is written.
	obj *pyjson.Object
}

// toolNameOf is the payload's tool name: tool_name, else tool, else name,
// by Python truthiness.
func toolNameOf(p *pyjson.Object) any {
	for _, k := range []string{"tool_name", "tool", "name"} {
		if v := p.Value(k); pyjson.Truthy(v) {
			return v
		}
	}
	return nil
}

// toolInputOf is `payload.get("tool_input") or ... or {}`.
func toolInputOf(p *pyjson.Object) any {
	for _, k := range []string{"tool_input", "args", "input"} {
		if v := p.Value(k); pyjson.Truthy(v) {
			return v
		}
	}
	return pyjson.NewObject()
}

// firstRaw is `inp.get(a) or inp.get(b) or ... or ""`.
func firstRaw(inp *pyjson.Object, keys ...string) any {
	for _, k := range keys {
		if v := inp.Value(k); pyjson.Truthy(v) {
			return v
		}
	}
	return ""
}

// joinOf is hook.join_keys: the JOIN_KEYS the payload holds with a truthy
// value, in order.
func joinOf(p *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range joinKeys {
		if v := p.Value(k); pyjson.Truthy(v) {
			out.Set(k, v)
		}
	}
	return out
}

// payloadToRecord is hook._payload_to_record. ok is false where Python
// returns None. It raises what Python raises on a field of an unexpected
// type (a tool name that is no str, a tool input that is no dict, content
// with no length).
func (r *runner) payloadToRecord(p *pyjson.Object, fmt string) (*record, bool) {
	tn := toolNameOf(p)
	if tn == nil {
		return nil, false
	}
	name, isStr := tn.(string)
	if !isStr {
		// _classify_tool: name.startswith("mcp__")
		noAttr(tn, "startswith")
	}
	if isStr && isPatchCall(name, fmt) {
		return nil, false
	}
	step := classifyTool(name, fmt)
	if step == "" {
		return nil, false
	}
	inpAny := toolInputOf(p)
	rec := &record{ToolName: name, StepType: step, obj: pyjson.NewObject()}
	rec.obj.Set("captured_at", nil)
	rec.obj.Set("session_id", r.safeSession(p.Value("session_id")))
	rec.obj.Set("tool_name", name)
	rec.obj.Set("step_type", step)
	if step != "mcp" {
		inp, isObj := inpAny.(*pyjson.Object)
		if !isObj {
			noAttr(inpAny, "get")
		}
		switch step {
		case "shell":
			rec.CommandRaw = firstRaw(inp, "command", "cmd")
			rec.Command = pyStrOr(rec.CommandRaw)
			rec.obj.Set("command", rec.CommandRaw)
		case "file_read", "file_write":
			for _, k := range []string{"file_path", "path", "filePath"} {
				v := inp.Value(k)
				if !pyjson.Truthy(v) {
					continue
				}
				if sv, ok := v.(string); ok {
					rec.SearchPaths = append(rec.SearchPaths, sv)
				} else {
					rec.BadPath = true
				}
			}
			rec.PathRaw = firstRaw(inp, "file_path", "path", "filePath", "pattern")
			if fmt == "openclaw" && name == "apply_patch" {
				// The patch text names its files; the first is the path.
				rec.PathRaw = ""
				if named, ok := parseApplyPatch(firstRaw(inp, "patch", "input")); ok && len(named) > 0 {
					rec.PathRaw = named[0]
				}
			}
			cwd, cwdIsStr := p.Value("cwd").(string)
			if ps, ok := rec.PathRaw.(string); ok && ps != "" && !isabs(ps) && !strings.HasPrefix(ps, "~") &&
				!strings.Contains(ps, "$") && cwdIsStr && isabs(cwd) {
				rec.PathRaw = normpath(join(cwd, ps))
			}
			rec.Path = pyStrOr(rec.PathRaw)
			rec.obj.Set("path", rec.PathRaw)
			if step == "file_write" {
				rec.obj.Set("content_len", pyLenOf(firstRaw(inp, "content", "new_string", "newString")))
			}
		case "network":
			rec.URLRaw = firstURL(inp)
			rec.URL = pyStrOr(rec.URLRaw)
			rec.obj.Set("url", rec.URLRaw)
		}
	} else {
		if name == applyPatchTool {
			return nil, false
		}
		server, tool, ok := parseMCPToolName(name)
		if !ok {
			if fmt != "pi" {
				return nil, false
			}
			server, tool = "pi", name
		}
		rec.MCPServer, rec.MCPTool = server, tool
		args, isObj := inpAny.(*pyjson.Object)
		var shown any = args
		if !isObj {
			args = pyjson.NewObject()
			rec.RawArguments, rec.HasRawArguments = inpAny, true
		} else if args.Len() == 0 {
			// The or-chain gave {}: an empty value (null, [], "", 0) that
			// the host did send is still no object.
			for _, k := range []string{"tool_input", "args", "input"} {
				if v, ok := p.Get(k); ok {
					if _, isO := v.(*pyjson.Object); !isO {
						rec.RawArguments, rec.HasRawArguments = v, true
						break
					}
				}
			}
		}
		if rec.HasRawArguments {
			shown = rec.RawArguments
		}
		rec.Arguments = args
		rec.obj.Set("mcp_server", server)
		rec.obj.Set("mcp_tool", tool)
		rec.obj.Set("arguments", shown)
	}
	j := joinOf(p)
	for _, k := range j.Keys() {
		rec.obj.Set(k, j.Value(k))
	}
	return rec, true
}

// firstURL is `inp.get("url") or first_url or inp.get("query") or ""`,
// where first_url is the first of a urls list.
func firstURL(inp *pyjson.Object) any {
	if v := inp.Value("url"); pyjson.Truthy(v) {
		return v
	}
	if urls, ok := inp.Value("urls").([]any); ok && len(urls) > 0 && pyjson.Truthy(urls[0]) {
		return urls[0]
	}
	return firstRaw(inp, "query")
}

// stepFieldCheck is _records_to_steps's step model on the record: the str
// field it validates, raising pydantic's error when the value is no str.
func (rec *record) stepFieldCheck() {
	var model, field string
	var v any
	switch rec.StepType {
	case "shell":
		model, field, v = "ShellStep", "command", rec.CommandRaw
	case "file_read":
		model, field, v = "FileReadStep", "path", rec.PathRaw
	case "file_write":
		model, field, v = "FileWriteStep", "path", rec.PathRaw
	case "network":
		model, field, v = "NetworkStep", "url", rec.URLRaw
	case "mcp":
		// ActionPlan's after-validator (models.non_finite_error): NaN or
		// an infinity in the call's arguments makes the plan invalid.
		if errs := pmodel.NonFinite(rec.Arguments, []any{"steps", 0, "arguments"}); len(errs) > 0 {
			e := &pmodel.ValidationError{Title: "ActionPlan", Errs: errs}
			panic(pystr.NewException("ValidationError", e.String()))
		}
		return
	default:
		return
	}
	if !pyjson.Truthy(v) {
		v = ""
	}
	if _, ok := v.(string); !ok {
		panic(stringTypeError(model, field, v))
	}
}

// pyStrings is the _strings helper of pane_rule and floor_config: every
// string inside a value, to depth 8.
func pyStrings(v any, depth int) []string {
	if depth > 8 {
		return nil
	}
	switch x := v.(type) {
	case string:
		return []string{x}
	case *pyjson.Object:
		var out []string
		for _, k := range x.Keys() {
			out = append(out, pyStrings(x.Value(k), depth+1)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, pyStrings(e, depth+1)...)
		}
		return out
	}
	return nil
}

// argumentsValue is record["arguments"]: what the host sent, an object or not.
func (rec *record) argumentsValue() any {
	if rec.HasRawArguments {
		return rec.RawArguments
	}
	return rec.Arguments
}
