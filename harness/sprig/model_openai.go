package sprig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultOpenAIBase is the OpenAI wire's base URL when --base-url is not
// given. The request goes to <base>/chat/completions.
const DefaultOpenAIBase = "https://api.openai.com/v1"

// OpenAIModel drives a model over the OpenAI chat-completions wire, with
// native function calling. Many servers speak it: OpenAI, Ollama,
// llama.cpp, vLLM, and gateways in front of other providers. The gate is
// unchanged: every returned call still goes through the executor.
type OpenAIModel struct {
	APIKey    string // empty sends no Authorization header
	Model     string
	BaseURL   string
	Timeout   time.Duration
	MaxTokens int
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
func (m *OpenAIModel) ModelName() string { return m.Model }

// HeldKey is the key this backend sends, for redaction.
func (m *OpenAIModel) HeldKey() string { return m.APIKey }

// NewOpenAIModel makes the OpenAI-wire backend. With keySet, key is the
// key and no key is read from the environment; else OPENAI_API_KEY is
// read. base is --base-url ("" for the default). A key is required only
// for the default base; a local server gets no Authorization header.
func NewOpenAIModel(key string, keySet bool, base, model string) (*OpenAIModel, error) {
	if !keySet {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if base == "" {
		base = DefaultOpenAIBase
		if key == "" {
			return nil, fmt.Errorf("the openai wire needs a key for %s: pass --key-fd, or set --base-url to a server that needs none", DefaultOpenAIBase)
		}
	}
	if model == "" {
		model = os.Getenv("SPRIG_MODEL")
	}
	if model == "" {
		return nil, fmt.Errorf("the openai wire needs a model: pass --model, or set SPRIG_MODEL")
	}
	return &OpenAIModel{
		APIKey:    key,
		Model:     model,
		BaseURL:   strings.TrimRight(base, "/"),
		Timeout:   120 * time.Second,
		MaxTokens: 4096,
	}, nil
}

// openaiToolsFor is the wall's tools as OpenAI function definitions, in
// sprig's order.
func openaiToolsFor(wall []string) []map[string]any {
	var out []map[string]any
	for _, t := range apiToolsFor(wall) {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t["name"],
				"description": t["description"],
				"parameters":  t["input_schema"],
			},
		})
	}
	return out
}

// openaiMessages maps sprig's history to chat messages: the system
// prompt first, each call with a synthetic id call_N, and the Nth tool
// result paired with the Nth call (the loop appends results in call
// order). A call's arguments are a JSON string, as the wire wants.
func openaiMessages(history []Message, system string) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": system}}
	nUse, nRes := 0, 0
	for _, m := range history {
		switch m.Role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Text})
		case "assistant":
			msg := map[string]any{"role": "assistant", "content": m.Text}
			if len(m.Calls) > 0 {
				if m.Text == "" {
					msg["content"] = nil
				}
				var calls []map[string]any
				for _, c := range m.Calls {
					args, _ := json.Marshal(nonNilInput(c.Input))
					calls = append(calls, map[string]any{
						"id":   fmt.Sprintf("call_%d", nUse),
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": string(args),
						},
					})
					nUse++
				}
				msg["tool_calls"] = calls
			}
			msgs = append(msgs, msg)
		case "tool":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": fmt.Sprintf("call_%d", nRes),
				"content":      m.Text,
			})
			nRes++
		}
	}
	return msgs
}

func (m *OpenAIModel) requestBody(history []Message) []byte {
	wall := m.Tools
	if len(wall) == 0 {
		wall = AllToolNames
	}
	maxTokens := m.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	req := map[string]any{
		"model":      m.Model,
		"max_tokens": maxTokens,
		"messages":   openaiMessages(history, systemFor(wall, m.NoTools, m.System, m.SystemSet)),
	}
	var tools []map[string]any
	if !m.NoTools {
		tools = openaiToolsFor(wall)
	}
	for _, e := range m.Extra {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": e.Name, "description": e.Description, "parameters": e.Schema,
			},
		})
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	body, _ := json.Marshal(req)
	return body
}

func (m *OpenAIModel) Next(history []Message) (Message, error) {
	req, err := http.NewRequest(http.MethodPost, m.BaseURL+"/chat/completions", bytes.NewReader(m.requestBody(history)))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("content-type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+m.APIKey)
	}
	do := m.httpDo
	if do == nil {
		timeout := m.Timeout
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		do = wireClient(timeout).Do
	}
	resp, err := do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, err := readBody(resp.Body, "openai")
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("openai api %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseOpenAIResponse(data)
}

// jsonField is the last value a JSON object holds under key, or nil.
func jsonField(v any, key string) any {
	if o, ok := v.(map[string]any); ok {
		return o[key]
	}
	return nil
}

// jsonInt reads a JSON integer as Python reads one: a number written
// with no fraction or exponent that fits 64 bits. ok is false for
// anything else.
func jsonInt(v any) (int, bool) {
	n, isNum := v.(json.Number)
	if !isNum {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// openaiUsage maps the wire's usage onto sprig's four counts, as the
// gateway does: prompt_tokens holds the cached tokens, so fresh input is
// the difference; cache writes are not reported, so they are zero.
func openaiUsage(u any) Usage {
	var out Usage
	prompt, hasPrompt := jsonInt(jsonField(u, "prompt_tokens"))
	cached, _ := jsonInt(jsonField(jsonField(u, "prompt_tokens_details"), "cached_tokens"))
	if hasPrompt {
		out.CacheRead = cached
		if prompt < 0 {
			// Kept negative, so the loop refuses the turn.
			out.Fresh = prompt
		} else if cached >= 0 {
			out.Fresh = max(0, prompt-cached)
		}
	}
	if c, ok := jsonInt(jsonField(u, "completion_tokens")); ok {
		out.Out = c
	}
	return out
}

// parseOpenAIResponse turns a chat completion into a sprig Message: the
// first choice's tool_calls become Calls (their ids kept), else its
// content is the text. A reply with no choice is an empty answer, which
// the loop nudges once.
func parseOpenAIResponse(data []byte) (Message, error) {
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return Message{}, fmt.Errorf("bad openai response: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	model, _ := jsonField(v, "model").(string)
	usage := openaiUsage(jsonField(v, "usage"))
	var msg any
	if choices, ok := jsonField(v, "choices").([]any); ok && len(choices) > 0 {
		msg = jsonField(choices[0], "message")
	}
	var calls []ToolCall
	if tcs, ok := jsonField(msg, "tool_calls").([]any); ok {
		for _, tc := range tcs {
			id, _ := jsonField(tc, "id").(string)
			fn := jsonField(tc, "function")
			name, _ := jsonField(fn, "name").(string)
			input, ok := openaiArguments(jsonField(fn, "arguments"))
			if !ok {
				return Message{}, fmt.Errorf("openai api: the arguments of tool call %q are not a JSON object", name)
			}
			calls = append(calls, ToolCall{ID: id, Name: name, Input: input})
		}
	}
	if len(calls) > 0 {
		return Message{Role: "assistant", Calls: calls, Model: model, Usage: usage}, nil
	}
	text, _ := jsonField(msg, "content").(string)
	return Message{Role: "assistant", Text: strings.TrimSpace(text), Model: model, Usage: usage}, nil
}

// openaiArguments reads a call's arguments: a JSON string that holds an
// object (or null, or nothing), or an object as some servers send it.
func openaiArguments(a any) (map[string]any, bool) {
	switch x := a.(type) {
	case nil:
		return map[string]any{}, true
	case map[string]any:
		return plainNumbers(x).(map[string]any), true
	case string:
		if strings.TrimSpace(x) == "" {
			return map[string]any{}, true
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			return nil, false
		}
		return nonNilInput(m), true
	}
	return nil, false
}

// plainNumbers turns the json.Number values of a UseNumber decode into
// float64, as a plain decode gives them, so a tool input reads the same
// whichever way it came.
func plainNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainNumbers(e)
		}
		return out
	}
	return v
}
