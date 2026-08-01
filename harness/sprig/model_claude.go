package sprig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCodeModel backs sprig with `claude -p` — the subscription path openDaisugi
// itself uses (no API key, so it respects the exposure freeze). The prompt rides
// on stdin (injection-safe, unbounded); the subprocess runs in a neutral CWD so
// the host's CLAUDE.md/.git never leaks in; `--allowedTools ""` makes claude a
// raw responder, not its own nested agent. The model speaks sprig's protocol:
// one fenced ```sprig-tool JSON block to call a tool, or plain text to finish.
type ClaudeCodeModel struct {
	Binary  string
	Model   string
	Timeout time.Duration
	run     func(prompt string) (string, error) // overridable for tests (no live call)
}

// NewClaudeCodeModel returns a model wired to the real `claude -p` subprocess.
func NewClaudeCodeModel() *ClaudeCodeModel {
	m := &ClaudeCodeModel{Binary: "claude", Model: "haiku", Timeout: 120 * time.Second}
	m.run = m.callClaude
	return m
}

func (m *ClaudeCodeModel) Next(history []Message) (Message, error) {
	runner := m.run
	if runner == nil {
		runner = m.callClaude
	}
	out, err := runner(formatPrompt(history))
	if err != nil {
		return Message{}, err
	}
	msg := parseResponse(out)
	// The model NAME is real — this backend's own configured value, not a
	// guess. Usage stays at Message's zero value: `claude -p`'s plain-text
	// output carries no token counts, so zero here is the honest answer, not
	// a silent stand-in for data this backend could have reported.
	msg.Model = m.Model
	return msg, nil
}

// callClaude is the proven `claude -p` invocation, mirrored from openDaisugi's
// claude_code_llm.py: prompt on stdin, neutral CWD, model bound with --model=.
func (m *ClaudeCodeModel) callClaude(prompt string) (string, error) {
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	binary := m.Binary
	if binary == "" {
		binary = "claude"
	}
	args := []string{"-p", "--allowedTools", ""}
	if m.Model != "" {
		args = append(args, "--model="+m.Model)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir() // neutral CWD — no project context leaks into the call
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude -p failed (quota/auth/binary): %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

const _toolFence = "```sprig-tool"

// knownTools is sprig's fixed protocol vocabulary. It guards the forgiving
// parser: an UNfenced JSON object counts as a call only if it names one of
// these, so a final answer that merely contains some JSON is never misread.
var knownTools = map[string]bool{"read": true, "write": true, "edit": true, "bash": true}

// formatPrompt renders the conversation plus the tool protocol into one prompt.
func formatPrompt(history []Message) string {
	var b strings.Builder
	b.WriteString("You are sprig, a minimal coding agent. You have four tools:\n")
	b.WriteString("  read  {\"path\"}          — return a file's contents\n")
	b.WriteString("  write {\"path\",\"content\"} — write a file\n")
	b.WriteString("  edit  {\"path\",\"old\",\"new\"} — replace a unique string\n")
	b.WriteString("  bash  {\"cmd\"}           — run a shell command\n\n")
	b.WriteString("To CALL a tool, reply with ONLY this and nothing else:\n")
	b.WriteString(_toolFence + "\n{\"tool\":\"<name>\",\"input\":{...}}\n```\n")
	b.WriteString("To FINISH, reply with your final answer as plain text (no block).\n\n")
	b.WriteString("Example — call a tool:\n")
	b.WriteString(_toolFence + "\n{\"tool\":\"read\",\"input\":{\"path\":\"main.go\"}}\n```\n")
	b.WriteString("Example — finish (plain text, no block):\n")
	b.WriteString("main.go defines three functions: New, Run, and Close.\n\n")
	b.WriteString("Conversation so far:\n")
	for _, msg := range history {
		role := msg.Role
		if role == "tool" {
			role = "tool-result"
		}
		text := msg.Text
		if text == "" && len(msg.Calls) > 0 {
			text = fmt.Sprintf("(called %s)", msg.Calls[0].Name)
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, text)
	}
	b.WriteString("\nYour reply:")
	return b.String()
}

// toolFences are the fenced forms a real model reaches for. The exact sprig tag
// and a generic ```tool are unambiguous intent, so any named tool inside is
// honored. A ```json block is ambiguous (an answer may quote JSON), so it only
// counts when it names a KNOWN tool — see requireKnown.
var toolFences = []struct {
	tag          string
	requireKnown bool
}{
	{"```sprig-tool", false},
	{"```tool", false},
	{"```json", true},
}

// parseResponse turns the model's text into a Message: a tool call if it holds a
// recognizable call (a tool fence, or a bare call object), otherwise a final
// plain-text answer. Models drift off the exact protocol, so parsing is
// forgiving; anything unrecognizable degrades to text rather than crashing.
func parseResponse(text string) Message {
	if call, ok := parseFencedCall(text); ok {
		return Message{Role: "assistant", Calls: []ToolCall{call}}
	}
	// No fence: accept the whole reply as a call only if it IS a call object
	// naming a known tool — the guard that keeps prose answers from misfiring.
	if call, ok := decodeCall(text, true); ok {
		return Message{Role: "assistant", Calls: []ToolCall{call}}
	}
	return Message{Role: "assistant", Text: strings.TrimSpace(text)}
}

// parseFencedCall scans for the first fenced block that decodes to a valid call.
func parseFencedCall(text string) (ToolCall, bool) {
	for _, fence := range toolFences {
		start := strings.Index(text, fence.tag)
		if start < 0 {
			continue
		}
		rest := text[start+len(fence.tag):]
		end := strings.Index(rest, "```")
		if end < 0 {
			continue
		}
		if call, ok := decodeCall(rest[:end], fence.requireKnown); ok {
			return call, true
		}
	}
	return ToolCall{}, false
}

// decodeCall parses a JSON call object. requireKnown rejects tools sprig does not
// have, so an ambiguous source (a bare reply, a ```json block) can't invent one.
func decodeCall(s string, requireKnown bool) (ToolCall, bool) {
	var call struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &call); err != nil || call.Tool == "" {
		return ToolCall{}, false
	}
	if requireKnown && !knownTools[call.Tool] {
		return ToolCall{}, false
	}
	if call.Input == nil {
		call.Input = map[string]any{}
	}
	return ToolCall{Name: call.Tool, Input: call.Input}, true
}
