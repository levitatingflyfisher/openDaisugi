package sprig

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIMessagesPairCallsAndResults(t *testing.T) {
	h := []Message{
		{Role: "user", Text: "go"},
		{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "a<b"}}, {Name: "bash"}}},
		{Role: "tool", Text: "A"},
		{Role: "tool", Text: "B"},
		{Role: "assistant", Text: "done"},
	}
	got, _ := json.Marshal(openaiMessages(h, "sys"))
	want := `[{"content":"sys","role":"system"},{"content":"go","role":"user"},` +
		`{"content":null,"role":"assistant","tool_calls":[` +
		`{"function":{"arguments":"{\"path\":\"a\\u003cb\"}","name":"read"},"id":"call_0","type":"function"},` +
		`{"function":{"arguments":"{}","name":"bash"},"id":"call_1","type":"function"}]},` +
		`{"content":"A","role":"tool","tool_call_id":"call_0"},` +
		`{"content":"B","role":"tool","tool_call_id":"call_1"},` +
		`{"content":"done","role":"assistant"}]`
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestParseOpenAIResponseToolCallsAndUsage(t *testing.T) {
	msg, err := parseOpenAIResponse([]byte(`{"model":"llama3.3:70b","choices":[{"message":{"content":"",` +
		`"tool_calls":[{"id":"c9","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\",\"n\":2}"}}]}}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].ID != "c9" || msg.Calls[0].Input["path"] != "x" || msg.Calls[0].Input["n"] != 2.0 {
		t.Fatalf("calls %+v", msg.Calls)
	}
	if msg.Model != "llama3.3:70b" || msg.Usage != (Usage{Fresh: 60, CacheRead: 40, Out: 7}) {
		t.Fatalf("model %q usage %+v", msg.Model, msg.Usage)
	}
}

func TestParseOpenAIResponseBadArguments(t *testing.T) {
	_, err := parseOpenAIResponse([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"read","arguments":"[1]"}}]}}]}`))
	if err == nil || err.Error() != `openai api: the arguments of tool call "read" are not a JSON object` {
		t.Fatalf("err %v", err)
	}
}

func TestParseOpenAIResponseText(t *testing.T) {
	msg, err := parseOpenAIResponse([]byte(`{"choices":[{"message":{"content":"  hi  "}}],"usage":{"prompt_tokens":2.5}}`))
	if err != nil || msg.Text != "hi" || msg.Usage != (Usage{}) {
		t.Fatalf("msg %+v err %v", msg, err)
	}
}

func TestOpenAIModelSendsBearerOnlyWithAKey(t *testing.T) {
	for _, key := range []string{"", "sk-x"} {
		var seen *http.Request
		var body []byte
		m := &OpenAIModel{APIKey: key, Model: "qwen3-coder", BaseURL: "http://local/v1", Tools: []string{"read"},
			httpDo: func(r *http.Request) (*http.Response, error) {
				seen = r
				body, _ = io.ReadAll(r.Body)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
			}}
		if _, err := m.Next([]Message{{Role: "user", Text: "x"}}); err != nil {
			t.Fatal(err)
		}
		if seen.URL.String() != "http://local/v1/chat/completions" {
			t.Fatal(seen.URL)
		}
		if got := seen.Header.Get("authorization"); (key == "") != (got == "") || (key != "" && got != "Bearer "+key) {
			t.Fatalf("key %q auth %q", key, got)
		}
		if !bytes.Contains(body, []byte(`"tools":[{"function":{"description":"Return a file's contents.","name":"read"`)) {
			t.Fatalf("body %s", body)
		}
	}
}

func TestNewOpenAIModelNeedsKeyOnlyForDefaultBase(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := NewOpenAIModel("", false, "", "m"); err == nil {
		t.Fatal("default base without a key must refuse")
	}
	if m, err := NewOpenAIModel("", false, "http://127.0.0.1:1/v1/", "m"); err != nil || m.APIKey != "" || m.BaseURL != "http://127.0.0.1:1/v1" {
		t.Fatalf("m %+v err %v", m, err)
	}
	t.Setenv("SPRIG_MODEL", "")
	if _, err := NewOpenAIModel("", false, "http://x", ""); err == nil {
		t.Fatal("no model must refuse")
	}
}

func TestCLIWireFlag(t *testing.T) {
	var got ModelOptions
	cli := &CLI{Version: "0", NewModel: func(o ModelOptions) (Model, error) {
		got = o
		return &scriptModel{turns: []Message{{Role: "assistant", Text: "ok"}}}, nil
	}}
	var out, errb bytes.Buffer
	if code := cli.Run([]string{"--wire", "openai", "--base-url", "http://h/v1", "x"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if got.Wire != "openai" || got.BaseURL != "http://h/v1" {
		t.Fatalf("%+v", got)
	}
	t.Setenv("SPRIG_BACKEND", "api")
	cli.Run([]string{"x"}, strings.NewReader(""), &out, &errb)
	if got.Wire != "anthropic" {
		t.Fatalf("SPRIG_BACKEND=api gave wire %q", got.Wire)
	}
	errb.Reset()
	if code := cli.Run([]string{"--wire", "grpc", "x"}, strings.NewReader(""), &out, &errb); code != 2 ||
		errb.String() != "sprig: --wire: unknown wire \"grpc\"; the wires are claude-code, anthropic, openai\n" {
		t.Fatalf("exit %d %q", code, errb.String())
	}
	errb.Reset()
	if code := cli.Run([]string{"--wire", "claude-code", "--base-url", "http://h", "x"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Fatalf("exit %d %q", code, errb.String())
	}
}
