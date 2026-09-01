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
	httpDo  func(*http.Request) (*http.Response, error) // overridable for tests
}

// apiSystemPrompt is deliberately tiny — the reason E exists.
const apiSystemPrompt = "You are sprig, a minimal coding agent. Use the tools " +
	"(read, write, edit, bash) to complete the task. When finished, reply with your " +
	"final answer as plain text."

// NewAPIModel reads ANTHROPIC_API_KEY and refuses to start without it — E is opt-in.
// ANTHROPIC_BASE_URL (the standard SDK env var) overrides the endpoint, so the same
// binary can drive a local mock, an Anthropic-compatible proxy, or the openDaisugi
// gateway — not only api.anthropic.com.
func NewAPIModel() (*APIModel, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("the api backend needs ANTHROPIC_API_KEY — E talks the raw API and breaks the exposure freeze, so set a key to opt in")
	}
	base := "https://api.anthropic.com"
	if o := strings.TrimRight(os.Getenv("ANTHROPIC_BASE_URL"), "/"); o != "" {
		base = o // trailing slash trimmed, or the path becomes //v1/messages
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
}

type apiMessage struct {
	Role    string     `json:"role"`
	Content []apiBlock `json:"content"`
}

func (m *APIModel) Next(history []Message) (Message, error) {
	reqBody := map[string]any{
		"model":      m.Model,
		"max_tokens": m.maxTokens(),
		"system":     apiSystemPrompt,
		"tools":      apiTools(),
		"messages":   buildMessages(history),
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, m.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", m.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	if m.Session != "" {
		req.Header.Set("x-opencode-session", m.Session)
	}

	do := m.httpDo
	if do == nil {
		do = (&http.Client{Timeout: m.timeout()}).Do
	}
	resp, err := do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest && !hasErrorText(data) {
			return Message{}, fmt.Errorf("%s refused this request and gave no reason. Some providers refuse topics by policy. Set SPRIG_MODEL to another model and try again", m.Model)
		}
		return Message{}, fmt.Errorf("anthropic api %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseAPIResponse(data)
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

// buildMessages maps sprig's flat history to Messages-API content blocks. It
// assigns synthetic tool-use ids in order and pairs the Nth tool result with the
// Nth call — sound because the loop appends results in call order. Consecutive
// tool results coalesce into one user message, as the API requires.
func buildMessages(history []Message) []apiMessage {
	var msgs []apiMessage
	nUse, nRes := 0, 0
	for i := 0; i < len(history); {
		m := history[i]
		switch m.Role {
		case "user":
			// A prompt that follows another prompt joins its message, so
			// the roles still alternate after a turn the provider refused.
			block := apiBlock{Type: "text", Text: m.Text}
			if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
				msgs[n-1].Content = append(msgs[n-1].Content, block)
			} else {
				msgs = append(msgs, apiMessage{Role: "user", Content: []apiBlock{block}})
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
