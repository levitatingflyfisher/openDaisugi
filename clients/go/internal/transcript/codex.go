package transcript

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

var (
	rolloutKinds = map[string]bool{"session_meta": true, "response_item": true, "event_msg": true,
		"turn_context": true, "compacted": true}
	responseItemTypes = map[string]bool{"message": true, "function_call": true, "function_call_output": true,
		"local_shell_call": true, "custom_tool_call": true, "web_search_call": true, "reasoning": true}
	shellToolNames = map[string]bool{"shell": true, "exec_command": true, "container.exec": true}
	shells         = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true}
	rePatchFile    = lazyre.New(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)
)

// strKey is a value compared against a set of str: its text, when it is
// a str.
func strKey(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// unwrap is codex._unwrap: the rollout line peeled to (kind, item).
func unwrap(line *pyjson.Object) (string, *pyjson.Object, error) {
	var d any = line
	for i := 0; i < 4; i++ {
		o, isObj := d.(*pyjson.Object)
		if !isObj {
			return "", nil, nil
		}
		kindV := get(o, "type")
		if err := unhashable(kindV); err != nil {
			return "", nil, err
		}
		kind, _ := strKey(kindV)
		if rolloutKinds[kind] {
			if p, isObj := get(o, "payload").(*pyjson.Object); isObj {
				return kind, p, nil
			}
		}
		if responseItemTypes[kind] {
			return "response_item", o, nil
		}
		if it, isObj := get(o, "item").(*pyjson.Object); isObj {
			d = it
			continue
		}
		return "", nil, nil
	}
	return "", nil, nil
}

// argvToCommand is _argv_to_command; ok is false for None.
func argvToCommand(argv any) (string, bool, error) {
	if s, isStr := argv.(string); isStr {
		s = pystr.Strip(s)
		return s, s != "", nil
	}
	list, isList := argv.([]any)
	if !isList || len(list) == 0 {
		return "", false, nil
	}
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = pyStr(a)
	}
	if len(parts) >= 3 && shells[parts[0]] {
		flags := true
		for _, p := range parts[1 : len(parts)-1] {
			if !strings.HasPrefix(p, "-") {
				flags = false
			}
		}
		if flags {
			last := pystr.Strip(parts[len(parts)-1])
			return last, last != "", nil
		}
	}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = verify.ShlexQuote(p)
	}
	return strings.Join(quoted, " "), true, nil
}

// textOf is codex._text_of.
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if o, isObj := b.(*pyjson.Object); isObj {
				if t, isStr := get(o, "text").(string); isStr {
					parts = append(parts, t)
				}
			}
		}
		return pystr.Strip(strings.Join(parts, "\n"))
	}
	return ""
}

func toolUse(name any, input *pyjson.Object) *pyjson.Object {
	return pyjson.NewObject().Set("type", "tool_use").Set("name", name).Set("input", input)
}

// patchWrites is _patch_write_blocks: re.findall raises on a patch that
// is not a str.
func patchWrites(p any) ([]any, error) {
	patch, isStr := p.(string)
	if !isStr {
		return nil, &ParseError{Msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyTypeName(p))}
	}
	var out []any
	for _, m := range rePatchFile().FindAllStringSubmatch(patch, -1) {
		out = append(out, toolUse("Write", pyjson.NewObject().Set("file_path", pystr.Strip(m[1])).Set("content", "")))
	}
	return out, nil
}

// CodexMessages is CodexParser._read_messages.
func CodexMessages(text string) ([]Message, error) {
	var msgs []Message
	lastUser := ""
	hasLast := false
	addUser := func(t string) {
		t = pystr.Strip(t)
		if t == "" || (hasLast && t == lastUser) {
			return
		}
		lastUser, hasLast = t, true
		msgs = append(msgs, Message{Role: "user", Content: t, HasContent: true})
	}
	addTools := func(blocks []any) {
		if len(blocks) > 0 {
			msgs = append(msgs, Message{Role: "assistant", Content: blocks, HasContent: true})
		}
	}
	for _, raw := range Lines(text) {
		raw = pystr.Strip(raw)
		if raw == "" {
			continue
		}
		v, ok, err := loadLine(raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		line, isObj := v.(*pyjson.Object)
		if !isObj {
			continue
		}
		kind, item, err := unwrap(line)
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		if kind == "event_msg" {
			if get(item, "type") == "user_message" {
				if m, isStr := get(item, "message").(string); isStr {
					addUser(m)
				} else {
					addUser(textOf(get(item, "message")))
				}
			}
			continue
		}
		if kind != "response_item" {
			continue
		}
		itype, _ := strKey(get(item, "type"))
		switch itype {
		case "message":
			t := textOf(get(item, "content"))
			if get(item, "role") == "user" {
				addUser(t)
			} else if t != "" {
				msgs = append(msgs, Message{Role: "assistant",
					Content: []any{pyjson.NewObject().Set("type", "text").Set("text", t)}, HasContent: true})
			}
		case "function_call":
			nameV := or2(get(item, "name"), "")
			name, _ := nameV.(string)
			argText := or2(get(item, "arguments"), "{}")
			at, isStr := argText.(string)
			if !isStr {
				return nil, &ParseError{Msg: fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s",
					pyTypeName(argText))}
			}
			av, ok, err := loadLine(at)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			args, isObj := av.(*pyjson.Object)
			if !isObj {
				continue
			}
			if err := unhashable(nameV); err != nil {
				return nil, err
			}
			_, nameStr := nameV.(string)
			switch {
			case nameStr && shellToolNames[name]:
				cmd, ok, err := argvToCommand(get(args, "command"))
				if err != nil {
					return nil, err
				}
				if ok {
					addTools([]any{toolUse("Bash", pyjson.NewObject().Set("command", cmd))})
				}
			case nameStr && name == "apply_patch":
				blocks, err := patchWrites(or2(or2(get(args, "input"), get(args, "patch")), ""))
				if err != nil {
					return nil, err
				}
				addTools(blocks)
			case truthy(get(item, "namespace")):
				addTools([]any{toolUse("mcp__"+pyStr(get(item, "namespace"))+"__"+pyStr(nameV), args)})
			}
		case "local_shell_call":
			action := or2(get(item, "action"), pyjson.NewObject())
			ao, isObj := action.(*pyjson.Object)
			if !isObj {
				return nil, noAttr(action, "get")
			}
			cmd, ok, err := argvToCommand(get(ao, "command"))
			if err != nil {
				return nil, err
			}
			if ok {
				addTools([]any{toolUse("Bash", pyjson.NewObject().Set("command", cmd))})
			}
		case "custom_tool_call":
			if get(item, "name") == "apply_patch" {
				blocks, err := patchWrites(or2(get(item, "input"), ""))
				if err != nil {
					return nil, err
				}
				addTools(blocks)
			}
		case "web_search_call":
			action := or2(get(item, "action"), pyjson.NewObject())
			ao, isObj := action.(*pyjson.Object)
			if !isObj {
				return nil, noAttr(action, "get")
			}
			if q := get(ao, "query"); truthy(q) {
				addTools([]any{toolUse("WebSearch", pyjson.NewObject().Set("query", q))})
			}
		}
	}
	return msgs, nil
}
