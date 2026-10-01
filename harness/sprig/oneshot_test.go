package sprig

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A one-shot call: --tools none sends no tool schema, and --system-file
// replaces sprig's system prompt on every wire.

func TestAPIRequestWithNoToolsHasNoToolsKey(t *testing.T) {
	m := &APIModel{Model: "m", NoTools: true, System: "SYS FILE", SystemSet: true}
	var got map[string]any
	m.httpDo = capture(t, &got, `{"content":[{"type":"text","text":"ok"}]}`)
	msg, err := m.Next([]Message{{Role: "user", Text: "hi"}})
	if err != nil || msg.Text != "ok" {
		t.Fatalf("Next = %+v, %v", msg, err)
	}
	if _, has := got["tools"]; has {
		t.Fatalf("request holds tools: %v", got)
	}
	// A system file is one block, marked for the cache.
	blocks, _ := got["system"].([]any)
	blk, _ := blocks[0].(map[string]any)
	mark, _ := blk["cache_control"].(map[string]any)
	if len(blocks) != 1 || blk["text"] != "SYS FILE" || mark["type"] != "ephemeral" {
		t.Fatalf("system = %v", got["system"])
	}
}

func TestAPIRequestWithNoToolsAndNoFileUsesTheNoToolsLine(t *testing.T) {
	m := &APIModel{Model: "m", NoTools: true}
	var got map[string]any
	m.httpDo = capture(t, &got, `{"content":[{"type":"text","text":"ok"}]}`)
	if _, err := m.Next([]Message{{Role: "user", Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got["system"] != noToolsPrompt {
		t.Fatalf("system = %v", got["system"])
	}
}

func TestOpenAIRequestWithNoToolsHasNoToolsKey(t *testing.T) {
	m := &OpenAIModel{Model: "m", BaseURL: "http://x", NoTools: true, System: "SYS FILE", SystemSet: true}
	var body map[string]any
	if err := json.Unmarshal(m.requestBody([]Message{{Role: "user", Text: "hi"}}), &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body["tools"]; has {
		t.Fatalf("request holds tools: %v", body)
	}
	msgs := body["messages"].([]any)
	if first := msgs[0].(map[string]any); first["content"] != "SYS FILE" {
		t.Fatalf("system message = %v", first)
	}
}

func TestClaudeArgsWithNoToolsTurnEveryBuiltInOffAndKeepAnEmptyMCPConfig(t *testing.T) {
	args := claudeArgs("claude", "haiku", true, "", nil)
	joined := strings.Join(args, "\x00")
	for _, want := range []string{"--tools\x00\x00", "--strict-mcp-config", `--mcp-config={"mcpServers":{}}`, "--model=haiku"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv lacks %q: %q", want, args)
		}
	}
}

func TestClaudePromptWithNoToolsHasNoToolProtocol(t *testing.T) {
	var prompt string
	m := &ClaudeCodeModel{Model: "haiku", NoTools: true, System: "SYS FILE", SystemSet: true,
		run: func(p string) (string, error) {
			prompt = p
			return claudeEnvelope(t, "answer", false, nil), nil
		}}
	msg, err := m.Next([]Message{{Role: "user", Text: "the task"}})
	if err != nil || msg.Text != "answer" {
		t.Fatalf("Next = %+v, %v", msg, err)
	}
	if !strings.HasPrefix(prompt, "SYS FILE\n\n") || !strings.Contains(prompt, "[user] the task") {
		t.Fatalf("prompt = %q", prompt)
	}
	if strings.Contains(prompt, "sprig-tool") {
		t.Fatalf("prompt names the tool protocol: %q", prompt)
	}
}

func TestAToolCallFromTheModelFailsANoToolsRun(t *testing.T) {
	cli := &CLI{Version: "0", NewModel: func(o ModelOptions) (Model, error) {
		if !o.NoTools || len(o.Tools) != 0 {
			t.Errorf("options = %+v", o)
		}
		return &scriptModel{turns: []Message{{Role: "assistant",
			Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "ls"}}}}}}, nil
	}}
	var out, errb bytes.Buffer
	code := cli.Run([]string{"--tools", "none", "go"}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), `called the tool "bash"`) {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

func TestSystemFileMissingIsOneLineAndExit2(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("x").Run([]string{"--system-file", filepath.Join(t.TempDir(), "nope"), "go"}, strings.NewReader(""), &out, &errb)
	if code != 2 || strings.Count(errb.String(), "\n") != 1 || !strings.HasPrefix(errb.String(), "sprig: --system-file: cannot read ") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

// capture is an httpDo that decodes the request body into got and
// answers 200 with reply.
func capture(t *testing.T, got *map[string]any, reply string) func(*http.Request) (*http.Response, error) {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(b, got); err != nil {
			t.Fatalf("request body: %v", err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply))}, nil
	}
}

func TestSystemFileBytesReachTheModelOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sys.txt")
	if err := os.WriteFile(path, []byte("line one\nline two"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen ModelOptions
	cli := &CLI{Version: "0", NewModel: func(o ModelOptions) (Model, error) {
		seen = o
		return &scriptModel{turns: []Message{{Role: "assistant", Text: "ok"}}}, nil
	}}
	var out, errb bytes.Buffer
	if code := cli.Run([]string{"--system-file", path, "--tools", "none", "go"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !seen.SystemSet || seen.System != "line one\nline two" || !seen.NoTools {
		t.Fatalf("options = %+v", seen)
	}
}
