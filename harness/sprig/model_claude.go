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
// raw responder, not its own nested agent. `--output-format json` is the same
// flag src/opendaisugi/claude_code_llm.py's call_claude_p_metered uses. It
// makes the CLI print one JSON envelope (result text plus Claude Code's own
// usage accounting) instead of bare text, so this backend can report real
// token counts. The model speaks sprig's protocol inside that result text:
// one fenced ```sprig-tool JSON block to call a tool, or plain text to finish.
type ClaudeCodeModel struct {
	Binary  string
	Model   string
	Timeout time.Duration
	// Tools is the wall: the tools the prompt offers. Empty offers all four.
	Tools []string
	// NoTools starts claude with every built-in tool off and an empty
	// strict MCP config, and the prompt names no tool (--tools none).
	NoTools bool
	// System, when SystemSet, is the prompt's opening in place of sprig's.
	System    string
	SystemSet bool
	// MCPConfig is the JSON of --mcp-config, passed to claude, which
	// runs the servers; MCPServers are their names. Empty is none.
	MCPConfig  string
	MCPServers []string
	run        func(prompt string) (string, error) // overridable for tests (no live call)
}

// NewClaudeCodeModel returns a model wired to the real `claude -p` subprocess.
func NewClaudeCodeModel() *ClaudeCodeModel {
	m := &ClaudeCodeModel{Binary: "claude", Model: "haiku", Timeout: 120 * time.Second}
	m.run = m.callClaude
	return m
}

// ModelName is the model this backend asks for.
func (m *ClaudeCodeModel) ModelName() string { return m.Model }

func (m *ClaudeCodeModel) Next(history []Message) (Message, error) {
	runner := m.run
	if runner == nil {
		runner = m.callClaude
	}
	tools := m.Tools
	if len(tools) == 0 {
		tools = AllToolNames
	}
	prompt := formatPromptFor(history, tools)
	if m.NoTools || m.SystemSet {
		prompt = formatPromptWith(history, tools, m.NoTools, m.System, m.SystemSet)
	}
	out, err := runner(prompt)
	if err != nil {
		return Message{}, err
	}
	text, usage, err := parseClaudeCLIEnvelope(out)
	if err != nil {
		return Message{}, err
	}
	msg := parseResponse(text)
	// The model NAME is real: this backend's own configured value, not a
	// guess, same as before. Usage now comes from the CLI's own envelope
	// (see parseClaudeCLIEnvelope), not a hardcoded zero.
	msg.Model = m.Model
	msg.Usage = usage
	return msg, nil
}

// claudeArgs builds the `claude -p` argv, without the prompt (which rides on
// stdin, injection-safe and unbounded). Model can be operator- or
// plan-authored, so it is bound with the --model=<value> form: the value can
// never be reparsed as a separate flag.
func claudeArgs(binary, model string, noTools bool, mcpConfig string, mcpServers []string) []string {
	if binary == "" {
		binary = "claude"
	}
	allowed := ""
	for i, s := range mcpServers {
		if i > 0 {
			allowed += ","
		}
		allowed += "mcp__" + s
	}
	args := []string{binary, "-p", "--output-format", "json", "--allowedTools", allowed}
	switch {
	case noTools && mcpConfig == "":
		// Every built-in off, and no MCP server but none: the call is a
		// raw responder that cannot touch a file or a tool.
		args = append(args, "--tools", "", "--strict-mcp-config", "--mcp-config="+emptyMCPConfig)
	case noTools:
		args = append(args, "--tools", "", "--strict-mcp-config", "--mcp-config="+mcpConfig)
	case mcpConfig != "":
		args = append(args, "--strict-mcp-config", "--mcp-config="+mcpConfig)
	}
	if model != "" {
		args = append(args, "--model="+model)
	}
	return args
}

// callClaude is the proven `claude -p` invocation, mirrored from openDaisugi's
// claude_code_llm.py: prompt on stdin, neutral CWD, --output-format json so
// stdout is one parseable envelope (see parseClaudeCLIEnvelope) rather than
// bare text.
func (m *ClaudeCodeModel) callClaude(prompt string) (string, error) {
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := claudeArgs(m.Binary, m.Model, m.NoTools, m.MCPConfig, m.MCPServers)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir() // neutral CWD — no project context leaks into the call
	cmd.Env = ChildEnv(cmd.Dir)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude -p failed (quota/auth/binary): %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// claudeCLIEnvelope is the JSON object `claude -p --output-format json` prints
// on success: type "result", the turn's final text in Result, whether the
// turn itself ended in an error, and Claude Code's own token accounting. This
// is the same shape src/opendaisugi/claude_code_llm.py's call_claude_p_metered
// reads, and the same fields the "result" line of --output-format stream-json
// carries. See harness/coppice/internal/adapters/claude's wireLine.
type claudeCLIEnvelope struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens              int `json:"input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// parseClaudeCLIEnvelope reads the CLI's --output-format json stdout: the
// final text (to hand to parseResponse) and its usage, mapped onto sprig's
// four buckets. It requires type "result" so that stdout which is NOT this
// envelope (an old CLI still on plain text, a proxy that strips fields, a
// dropped flag, or any other JSON such as "{}") is caught here as an error,
// instead of silently decoding to empty text and zero usage. is_error is a
// real CLI-reported failure (max turns, a refused turn) that exits 0, so it
// surfaces as an error here rather than being handed to parseResponse as if
// it were a genuine final answer.
func parseClaudeCLIEnvelope(raw string) (string, Usage, error) {
	var env claudeCLIEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Type != "result" {
		return "", Usage{}, fmt.Errorf("claude -p --output-format json gave an unreadable envelope: %s", truncateRunes(raw, 200))
	}
	if env.IsError {
		return "", Usage{}, fmt.Errorf("claude -p reported is_error: %s", truncateRunes(env.Result, 200))
	}
	usage := Usage{
		Fresh:      env.Usage.InputTokens,
		CacheRead:  env.Usage.CacheReadInputTokens,
		CacheWrite: env.Usage.CacheCreationInputTokens,
		Out:        env.Usage.OutputTokens,
	}
	return strings.TrimSpace(env.Result), usage, nil
}

// emptyMCPConfig is the --mcp-config of a call with no tools.
const emptyMCPConfig = `{"mcpServers":{}}`

const _toolFence = "```sprig-tool"

// knownTools is sprig's fixed protocol vocabulary. It guards the forgiving
// parser: an UNfenced JSON object counts as a call only if it names one of
// these, so a final answer that merely contains some JSON is never misread.
var knownTools = map[string]bool{"read": true, "write": true, "edit": true, "bash": true, "spawn": true}

// promptToolLines is each tool's line in the prompt's tool list, and
// promptExamples a call of it for the prompt's example.
var promptToolLines = map[string]string{
	"read":  "  read  {\"path\"}          — return a file's contents\n",
	"write": "  write {\"path\",\"content\"} — write a file\n",
	"edit":  "  edit  {\"path\",\"old\",\"new\"} — replace a unique string\n",
	"bash":  "  bash  {\"cmd\"}           — run a shell command\n",
	"spawn": "  spawn {\"task\",\"envelope\",\"profile\",\"model\",\"tools\",\"tokens\",\"turns\"}: start a child agent and wait for its answer\n",
}

var promptExamples = map[string]string{
	"read":  `{"tool":"read","input":{"path":"main.go"}}`,
	"write": `{"tool":"write","input":{"path":"notes.txt","content":"hi"}}`,
	"edit":  `{"tool":"edit","input":{"path":"main.go","old":"Run","new":"Start"}}`,
	"bash":  `{"tool":"bash","input":{"cmd":"ls"}}`,
	"spawn": `{"tool":"spawn","input":{"task":"list the TODOs","envelope":{"permissions":{"file_read":["/w/**"]}}}}`,
}

var countWords = []string{"no tools", "one tool", "two tools", "three tools", "four tools", "five tools"}

// formatPromptWith renders the prompt for a run with no tools or with a
// system file. With no tools there is no tool protocol: the opening is the
// system text (sprig's no-tools line when no file was given), then the
// conversation. With a system file and tools, the file replaces the first
// line of the tool protocol and the rest stays.
func formatPromptWith(history []Message, wall []string, noTools bool, system string, systemSet bool) string {
	if !noTools {
		p := formatPromptFor(history, wall)
		first := strings.SplitN(p, "\n", 2)
		return system + "\n" + first[1]
	}
	open := noToolsPrompt
	if systemSet {
		open = system
	}
	var b strings.Builder
	b.WriteString(open)
	b.WriteString("\n\nConversation so far:\n")
	for _, msg := range history {
		role := msg.Role
		if role == "tool" {
			role = "tool-result"
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, msg.Text)
	}
	b.WriteString("\nYour reply:")
	return b.String()
}

// formatPrompt renders the conversation plus the tool protocol into one
// prompt, offering all four tools.
func formatPrompt(history []Message) string { return formatPromptFor(history, AllToolNames) }

// formatPromptFor renders the prompt offering only the tools of the wall,
// in sprig's order. The example call uses the wall's first tool.
func formatPromptFor(history []Message, wall []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are sprig, a minimal coding agent. You have %s:\n", countWords[len(wall)])
	for _, name := range wall {
		b.WriteString(promptToolLines[name])
	}
	b.WriteString("\n")
	b.WriteString("To CALL a tool, reply with ONLY this and nothing else:\n")
	b.WriteString(_toolFence + "\n{\"tool\":\"<name>\",\"input\":{...}}\n```\n")
	b.WriteString("To FINISH, reply with your final answer as plain text (no block).\n\n")
	b.WriteString("Example — call a tool:\n")
	b.WriteString(_toolFence + "\n" + promptExamples[wall[0]] + "\n```\n")
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
