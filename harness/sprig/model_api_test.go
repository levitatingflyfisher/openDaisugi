package sprig

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBuildMessagesPairsToolUseWithResult(t *testing.T) {
	history := []Message{
		{Role: "user", Text: "read x.go"},
		{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "x.go"}}}},
		{Role: "tool", Text: "file contents"},
	}
	msgs := buildMessages(history)
	if len(msgs) != 3 {
		t.Fatalf("want 3 api messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content[0].Type != "text" {
		t.Fatalf("msg0 should be a user text message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content[0].Type != "tool_use" {
		t.Fatalf("msg1 should be an assistant tool_use: %+v", msgs[1])
	}
	if msgs[2].Role != "user" || msgs[2].Content[0].Type != "tool_result" {
		t.Fatalf("msg2 should be a user tool_result: %+v", msgs[2])
	}
	// The result MUST reference the call's id, or the API rejects the turn.
	if msgs[1].Content[0].ID == "" || msgs[1].Content[0].ID != msgs[2].Content[0].ToolUseID {
		t.Fatalf("tool_use id %q must equal tool_result id %q", msgs[1].Content[0].ID, msgs[2].Content[0].ToolUseID)
	}
}

func TestParseAPIResponseExtractsToolUse(t *testing.T) {
	body := []byte(`{"content":[{"type":"tool_use","id":"toolu_0","name":"bash","input":{"cmd":"ls"}}]}`)
	msg, err := parseAPIResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "bash" || msg.Calls[0].Input["cmd"] != "ls" {
		t.Fatalf("bad parse: %+v", msg)
	}
	// The API hands back a REAL tool_use id — the loop must not need to mint
	// a second one over it (session_tree writes must be joinable to it).
	if msg.Calls[0].ID != "toolu_0" {
		t.Fatalf("want the API's own tool_use id preserved, got %q", msg.Calls[0].ID)
	}
}

func TestParseAPIResponseFillsModelAndUsage(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5","content":[{"type":"text","text":"done"}],` +
		`"usage":{"input_tokens":12,"cache_read_input_tokens":1000,"cache_creation_input_tokens":50,"output_tokens":30}}`)
	msg, err := parseAPIResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Model != "claude-haiku-4-5" {
		t.Fatalf("want the API's own model name, got %q", msg.Model)
	}
	want := Usage{Fresh: 12, CacheRead: 1000, CacheWrite: 50, Out: 30}
	if msg.Usage != want {
		t.Fatalf("want usage %+v, got %+v", want, msg.Usage)
	}
}

func TestParseAPIResponseWithNoUsageFieldStaysZeroNotCrashing(t *testing.T) {
	// A response with no usage object (e.g. an old fixture, a proxy that
	// strips it) must decode to zero usage, not error.
	body := []byte(`{"content":[{"type":"text","text":"done"}]}`)
	msg, err := parseAPIResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Usage != (Usage{}) {
		t.Fatalf("want zero usage with no usage field, got %+v", msg.Usage)
	}
}

func TestParseAPIResponsePlainTextFinishes(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"all done"}]}`)
	msg, err := parseAPIResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 0 || msg.Text != "all done" {
		t.Fatalf("bad parse: %+v", msg)
	}
}

func TestAPIModelNextSendsToolsAndKeyAndParses(t *testing.T) {
	var gotBody []byte
	var gotKey string
	m := &APIModel{APIKey: "sk-test", Model: "claude-haiku-4-5", BaseURL: "https://api.anthropic.com", MaxTokens: 1024,
		httpDo: func(req *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(req.Body)
			gotKey = req.Header.Get("x-api-key")
			resp := `{"content":[{"type":"tool_use","id":"t","name":"write","input":{"path":"a","content":"b"}}]}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp))}, nil
		}}
	msg, err := m.Next([]Message{{Role: "user", Text: "make a file"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "write" {
		t.Fatalf("Next did not parse the native tool call: %+v", msg)
	}
	if gotKey != "sk-test" {
		t.Fatalf("x-api-key header not set, got %q", gotKey)
	}
	// The point of E: a native tools:[] array with the 4 schemas rides every request.
	if !bytes.Contains(gotBody, []byte(`"tools"`)) || !bytes.Contains(gotBody, []byte(`"input_schema"`)) {
		t.Fatalf("request missing the native tools array: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"name":"read"`)) {
		t.Fatalf("request missing the tool schemas: %s", gotBody)
	}
}

func TestNewAPIModelRequiresKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewAPIModel(); err == nil {
		t.Fatal("E must refuse to start without a key — it breaks the exposure freeze, so opt in explicitly")
	}
}

func TestAPISystemPromptIsMinimal(t *testing.T) {
	// The whole reason E exists: a tiny prompt, not Claude Code's rented context.
	if len(apiSystemPrompt) > 400 {
		t.Fatalf("E's system prompt must stay minimal, got %d chars", len(apiSystemPrompt))
	}
}

func TestNewAPIModelHonorsBaseURLOverride(t *testing.T) {
	// So the real `sprig` binary can be pointed at a local mock, an Anthropic-
	// compatible proxy, or the openDaisugi gateway — not only api.anthropic.com.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9999/")
	m, err := NewAPIModel()
	if err != nil {
		t.Fatal(err)
	}
	// The trailing slash must be trimmed, or the request path becomes //v1/messages.
	if m.BaseURL != "http://127.0.0.1:9999" {
		t.Fatalf("NewAPIModel must honor ANTHROPIC_BASE_URL (trailing slash trimmed), got %q", m.BaseURL)
	}
}

func TestNewAPIModelDefaultsToRealAPI(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	m, err := NewAPIModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("with no override, base URL should be the real API, got %q", m.BaseURL)
	}
}

// OpenCode Go refuses a Messages call with no x-opencode-session header, so
// every request carries one id for the whole run, the same on each turn.
func TestAPIModelSendsOneSessionIDEveryTurn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	m, err := NewAPIModel()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	m.httpDo = func(req *http.Request) (*http.Response, error) {
		got = append(got, req.Header.Get("x-opencode-session"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"ok"}]}`))}, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := m.Next([]Message{{Role: "user", Text: "hi"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got[0] == "" || got[0] != got[1] {
		t.Fatalf("want one non-empty session id on every turn, got %q", got)
	}
}

// SPRIG_MODEL picks the model, so the api backend can drive any model an
// Anthropic-compatible endpoint serves.
func TestNewAPIModelHonorsModelOverride(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("SPRIG_MODEL", "kimi-k3")
	m, err := NewAPIModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.Model != "kimi-k3" {
		t.Fatalf("SPRIG_MODEL not honored, got %q", m.Model)
	}
}

// A turn the provider refused leaves its prompt in the history, so the
// next prompt follows it. Two user turns in a row go out as one message,
// since the Messages API wants the roles to alternate.
func TestBuildMessagesJoinsPromptsInARow(t *testing.T) {
	msgs := buildMessages([]Message{{Role: "user", Text: "one"}, {Role: "user", Text: "two"}})
	if len(msgs) != 1 || len(msgs[0].Content) != 2 {
		t.Fatalf("want one user message with two text blocks, got %+v", msgs)
	}
}

// A 400 with no error text is how some providers refuse a request by
// policy. The error says so, names the model, and names the way out.
func TestAPIModelNamesARefusalWithNoReason(t *testing.T) {
	m := &APIModel{APIKey: "k", Model: "kimi-k3", BaseURL: "http://x",
		httpDo: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"model":"kimi-k3"}`))}, nil
		}}
	_, err := m.Next([]Message{{Role: "user", Text: "q"}})
	if err == nil || !strings.Contains(err.Error(), "kimi-k3 refused this request and gave no reason") || !strings.Contains(err.Error(), "SPRIG_MODEL") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
}
