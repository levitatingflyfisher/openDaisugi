package sprig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// APIModel is Design E: sprig drives the model over the Anthropic Messages API
// wire protocol DIRECTLY, with native tool-use (a tools:[] array the model calls
// and we execute). This is the oh-my-pi path — and the ONLY one with a genuinely
// minimal prompt: sprig's own tiny system prompt + four tool schemas, none of
// Claude Code's ~57-86k rented context. It needs an API key, so it is OFF by
// default and breaks the exposure freeze by design — you opt in explicitly.
//
// The gate is unchanged: sprig's loop still runs every returned call through the
// in-process Executor, so "own the boundary" holds exactly as in A/B.
type APIModel struct {
	APIKey    string
	Model     string
	BaseURL   string
	Timeout   time.Duration
	MaxTokens int
	// Session is one id for the whole run, sent on every request.
	// OpenCode Go refuses a call without it; other endpoints ignore it.
	Session string
	// Tools is the wall: the tools the request offers. Empty offers all four.
	Tools []string
	// NoTools sends no tool schema (--tools none).
	NoTools bool
	// System, when SystemSet, is the system prompt in place of sprig's.
	System    string
	SystemSet bool
	// Extra are more tools (from MCP servers) the request offers after the
	// wall's. With NoTools they are the only tools.
	Extra  []MCPToolDef
	httpDo func(*http.Request) (*http.Response, error) // overridable for tests
}

// ModelName is the model this backend asks for.
func (m *APIModel) ModelName() string { return m.Model }

// HeldKey is the key this backend sends, for redaction.
func (m *APIModel) HeldKey() string { return m.APIKey }

// apiSystemPrompt is deliberately tiny — the reason E exists.
const apiSystemPrompt = "You are sprig, a minimal coding agent. Use the tools " +
	"(read, write, edit, bash) to complete the task. When finished, reply with your " +
	"final answer as plain text."

// NewAPIModel reads ANTHROPIC_API_KEY and refuses to start without it — E is opt-in.
// ANTHROPIC_BASE_URL (the standard SDK env var) overrides the endpoint, so the same
// binary can drive a local mock, an Anthropic-compatible proxy, or the openDaisugi
// gateway — not only api.anthropic.com.
func NewAPIModel() (*APIModel, error) { return NewAPIModelFor("", false, "") }

// DefaultAnthropicBase is the anthropic wire's base URL when neither
// --base-url nor ANTHROPIC_BASE_URL is set.
const DefaultAnthropicBase = "https://api.anthropic.com"

// NewAPIModelFor is NewAPIModel with the key and the base given. With
// keySet, key is the key and no key is read from the environment. base
// is --base-url; "" falls back to ANTHROPIC_BASE_URL, then the default.
// A key is required only for the default base: a local server gets no
// x-api-key header.
func NewAPIModelFor(key string, keySet bool, base string) (*APIModel, error) {
	if !keySet {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if base == "" {
		base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	base = strings.TrimRight(base, "/") // or the path becomes //v1/messages
	if base == "" {
		base = DefaultAnthropicBase
	}
	if key == "" && base == DefaultAnthropicBase {
		return nil, fmt.Errorf("the api backend needs ANTHROPIC_API_KEY — E talks the raw API and breaks the exposure freeze, so set a key to opt in")
	}
	model := "claude-haiku-4-5"
	if o := os.Getenv("SPRIG_MODEL"); o != "" {
		model = o
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("cannot make a session id: %w", err)
	}
	return &APIModel{
		APIKey:    key,
		Model:     model,
		Session:   "sprig-" + hex.EncodeToString(id[:]),
		BaseURL:   base,
		Timeout:   120 * time.Second,
		MaxTokens: 4096,
	}, nil
}

// apiBlock is one content block in a Messages-API turn (text, tool_use, or
// tool_result — the omitempty fields select the shape).
type apiBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	// CacheControl marks the end of a cached prefix.
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

var ephemeral = map[string]string{"type": "ephemeral"}

// toolDefs are the tools of the request: the wall's, then the extra
// ones. With no tools only the extra ones go.
func (m *APIModel) toolDefs(wall []string) []map[string]any {
	var tools []map[string]any
	if !m.NoTools {
		tools = apiToolsFor(wall)
	}
	for _, e := range m.Extra {
		tools = append(tools, map[string]any{"name": e.Name, "description": e.Description, "input_schema": e.Schema})
	}
	return tools
}

type apiMessage struct {
	Role    string     `json:"role"`
	Content []apiBlock `json:"content"`
}

func (m *APIModel) Next(history []Message) (Message, error) {
	wall := m.Tools
	if len(wall) == 0 {
		wall = AllToolNames
	}
	var system any = systemFor(wall, m.NoTools, m.System, m.SystemSet)
	if text := system.(string); m.SystemSet && text != "" {
		system = []apiBlock{{Type: "text", Text: text, CacheControl: ephemeral}}
	}
	reqBody := map[string]any{
		"model":      m.Model,
		"max_tokens": m.maxTokens(),
		"system":     system,
		"messages":   buildMessages(history, m.SystemSet && (!m.NoTools || len(m.Extra) > 0)),
	}
	if tools := m.toolDefs(wall); len(tools) > 0 {
		reqBody["tools"] = tools
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, m.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("content-type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("x-api-key", m.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	if m.Session != "" {
		req.Header.Set("x-opencode-session", m.Session)
	}

	do := m.httpDo
	if do == nil {
		do = wireClient(m.timeout()).Do
	}
	resp, err := do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, err := readBody(resp.Body, "anthropic")
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest && !hasErrorText(data) {
			return Message{}, fmt.Errorf("%s refused this request and gave no reason. Some providers refuse topics by policy. Set SPRIG_MODEL to another model and try again", m.Model)
		}
		return Message{}, fmt.Errorf("anthropic api %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseAPIResponse(data)
}

// plainTransport is Go's default transport without the automatic
// Accept-Encoding: gzip, so the request is the one sprig-rs sends too.
func plainTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}

// wireClient is the HTTP client of both model wires. It follows no
// redirect: a 3xx comes back as the response, so the key and the request
// never go to a host the operator did not name.
func wireClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: plainTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// maxResponse is the most a model's response body may hold.
const maxResponse = 32 << 20

// readBody reads a model response, at most maxResponse bytes.
func readBody(r io.Reader, wire string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponse+1))
	if len(data) > maxResponse {
		return nil, fmt.Errorf("%s api: the response is larger than 32 MiB", wire)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (m *APIModel) maxTokens() int {
	if m.MaxTokens == 0 {
		return 4096
	}
	return m.MaxTokens
}

func (m *APIModel) timeout() time.Duration {
	if m.Timeout == 0 {
		return 120 * time.Second
	}
	return m.Timeout
}

// The request carries two cache marks when it has a system prompt file and
// tools to call: the system block and the end of the request. The next call
// of a tool loop then reads everything up to the end of this one.
//
// buildMessages maps sprig's flat history to Messages-API content blocks. It
// assigns synthetic tool-use ids in order and pairs the Nth tool result with the
// Nth call — sound because the loop appends results in call order. Consecutive
// tool results coalesce into one user message, as the API requires.
func buildMessages(history []Message, markEnd bool) []apiMessage {
	var msgs []apiMessage
	nUse, nRes := 0, 0
	for i := 0; i < len(history); {
		m := history[i]
		switch m.Role {
		case "user":
			// A prompt that follows another prompt joins its message, so
			// the roles still alternate after a turn the provider refused.
			block := apiBlock{Type: "text", Text: m.Text}
			blocks := []apiBlock{block}
			if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
				msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			} else {
				msgs = append(msgs, apiMessage{Role: "user", Content: blocks})
			}
			i++
		case "assistant":
			var blocks []apiBlock
			if m.Text != "" {
				blocks = append(blocks, apiBlock{Type: "text", Text: m.Text})
			}
			for _, c := range m.Calls {
				blocks = append(blocks, apiBlock{Type: "tool_use", ID: fmt.Sprintf("toolu_%d", nUse), Name: c.Name, Input: nonNilInput(c.Input)})
				nUse++
			}
			msgs = append(msgs, apiMessage{Role: "assistant", Content: blocks})
			i++
		case "tool":
			var blocks []apiBlock
			for i < len(history) && history[i].Role == "tool" {
				blocks = append(blocks, apiBlock{Type: "tool_result", ToolUseID: fmt.Sprintf("toolu_%d", nRes), Content: history[i].Text})
				nRes++
				i++
			}
			msgs = append(msgs, apiMessage{Role: "user", Content: blocks})
		default:
			i++
		}
	}
	// The end of the request carries a mark, so the next call of a tool
	// loop reads everything up to here.
	if markEnd && len(msgs) > 0 {
		// An empty text block cannot carry a mark: the last block with
		// something in it does.
		last := &msgs[len(msgs)-1]
		for i := len(last.Content) - 1; i >= 0; i-- {
			if b := &last.Content[i]; b.Type != "text" || b.Text != "" {
				b.CacheControl = ephemeral
				break
			}
		}
	}
	return msgs
}

// parseAPIResponse turns a Messages-API reply into a sprig Message: any tool_use
// blocks become Calls (keeping the API's own id — a real tool_use id, not one
// the loop has to mint), the top-level model name is kept, and usage is mapped
// onto Usage's four buckets so OnAssistant's usage is never a silent zero on
// this backend. The claude-code backend (model_claude.go) gets its usage the
// same honest way, from its own provider's own accounting. See
// parseClaudeCLIEnvelope. A response with no usage object decodes to zero
// usage, not an error: an old fixture or a stripping proxy must not break
// parsing.
func parseAPIResponse(data []byte) (Message, error) {
	var r struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Message{}, fmt.Errorf("bad api response: %w", err)
	}
	usage := Usage{
		Fresh:      r.Usage.InputTokens,
		CacheRead:  r.Usage.CacheReadInputTokens,
		CacheWrite: r.Usage.CacheCreationInputTokens,
		Out:        r.Usage.OutputTokens,
	}
	var calls []ToolCall
	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "tool_use":
			calls = append(calls, ToolCall{ID: b.ID, Name: b.Name, Input: nonNilInput(b.Input)})
		case "text":
			text.WriteString(b.Text)
		}
	}
	if len(calls) > 0 {
		return Message{Role: "assistant", Calls: calls, Model: r.Model, Usage: usage}, nil
	}
	return Message{Role: "assistant", Text: strings.TrimSpace(text.String()), Model: r.Model, Usage: usage}, nil
}

// apiToolsFor is the tools of the wall, in sprig's order, in Anthropic
// tool-definition form.
func apiToolsFor(wall []string) []map[string]any {
	var tools []map[string]any
	for _, t := range apiTools() {
		for _, name := range wall {
			if t["name"] == name {
				tools = append(tools, t)
			}
		}
	}
	return tools
}

// apiSystemPromptFor is the system prompt naming the tools of the wall.
// The full wall gives apiSystemPrompt itself.
func apiSystemPromptFor(wall []string) string {
	return "You are sprig, a minimal coding agent. Use the tools (" + strings.Join(wall, ", ") +
		") to complete the task. When finished, reply with your final answer as plain text."
}

// noToolsPrompt is the system prompt of a run with no tools.
const noToolsPrompt = "You are sprig, a minimal agent. Reply with your final answer as plain text."

// systemFor is the system prompt of an API request: the --system-file
// bytes when one was given, else sprig's own for the wall (or for no
// tools).
func systemFor(wall []string, noTools bool, system string, systemSet bool) string {
	switch {
	case systemSet:
		return system
	case noTools:
		return noToolsPrompt
	}
	return apiSystemPromptFor(wall)
}

// apiTools is sprig's four tools in Anthropic tool-definition form (input_schema).
func apiTools() []map[string]any {
	obj := func(required []string, fields ...string) map[string]any {
		props := map[string]any{}
		for _, f := range fields {
			props[f] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []map[string]any{
		{"name": "read", "description": "Return a file's contents.", "input_schema": obj([]string{"path"}, "path")},
		{"name": "write", "description": "Write content to a file.", "input_schema": obj([]string{"path", "content"}, "path", "content")},
		{"name": "edit", "description": "Replace a unique occurrence of old with new.", "input_schema": obj([]string{"path", "old", "new"}, "path", "old", "new")},
		{"name": "bash", "description": "Run a shell command.", "input_schema": obj([]string{"cmd"}, "cmd")},
		{"name": "spawn", "description": "Start a child agent on a task inside a narrower envelope, wait, and return its answer.",
			"input_schema": spawnSchema()},
	}
}

// spawnSchema is the spawn tool's input: the task and the child's
// envelope, and optionally the agent profile, the model, the child's
// tools, its token and turn reservation and its turn limit.
func spawnSchema() map[string]any {
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "integer"}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": str, "envelope": map[string]any{"type": "object"}, "profile": str, "model": str,
			"tools":  map[string]any{"type": "array", "items": str},
			"tokens": num, "turns": num, "max_turns": num,
		},
		"required": []string{"task", "envelope"},
	}
}

func nonNilInput(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// hasErrorText reports whether an error body carries a message a person
// can read, in the Messages API shape {"error":{"message":...}}.
func hasErrorText(data []byte) bool {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(data, &e) == nil && strings.TrimSpace(e.Error.Message) != ""
}
