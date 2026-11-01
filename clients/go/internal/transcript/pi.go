package transcript

import (
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// piToClaude is parsers.pi._PI_TO_CLAUDE.
var piToClaude = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit",
	"grep": "Grep", "find": "Glob", "ls": "Glob",
}

// PiMessages is PiParser._read_messages. The entry ids (uuid, parentUuid,
// sessionId) the oracle carries do not reach an episode, so they are not
// kept here.
func PiMessages(text string) ([]Message, error) {
	var msgs []Message
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
		d, isObj := v.(*pyjson.Object)
		if !isObj {
			continue
		}
		if get(d, "type") != "message" {
			continue
		}
		msg, isObj := get(d, "message").(*pyjson.Object)
		if !isObj {
			continue
		}
		switch get(msg, "role") {
		case "user":
			content, has := msg.Get("content")
			if !has {
				content = ""
			}
			msgs = append(msgs, Message{Role: "user", Content: content, HasContent: true})
		case "assistant":
			blocks := piAssistantBlocks(get(msg, "content"))
			if len(blocks) > 0 {
				msgs = append(msgs, Message{Role: "assistant", Content: blocks, HasContent: true})
			}
		}
	}
	return msgs, nil
}

// piAssistantBlocks is PiParser._assistant_blocks.
func piAssistantBlocks(content any) []any {
	list, isList := content.([]any)
	if !isList {
		return nil
	}
	var blocks []any
	for _, b := range list {
		block, isObj := b.(*pyjson.Object)
		if !isObj {
			continue
		}
		switch get(block, "type") {
		case "text":
			if t, isStr := get(block, "text").(string); isStr {
				blocks = append(blocks, pyjson.NewObject().Set("type", "text").Set("text", t))
			}
		case "toolCall":
			name, isStr := get(block, "name").(string)
			if !isStr {
				continue
			}
			mapped, known := piToClaude[name]
			if !known {
				mapped = "mcp__pi__" + name
			}
			var input *pyjson.Object
			if o, isObj := get(block, "arguments").(*pyjson.Object); isObj {
				input = o
			} else {
				input = pyjson.NewObject()
			}
			blocks = append(blocks, toolUse(mapped, input))
		}
	}
	return blocks
}
