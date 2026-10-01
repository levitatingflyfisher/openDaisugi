package sprig

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMCPHelper is not a test: it is the fake server the others start,
// as this test binary run with SPRIG_MCP_HELPER set. SPRIG_MCP_HELPER
// names its fault: "silent" never answers tools/call, "big" answers it
// with a message over 32 MiB.
func TestMCPHelper(t *testing.T) {
	mode := os.Getenv("SPRIG_MCP_HELPER")
	if mode == "" {
		t.Skip("helper process")
	}
	in := bufio.NewReader(os.Stdin)
	out := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(os.Stdout, "%s\n", b) }
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var m struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &m)
		switch m.Method {
		case "initialize":
			out(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
		case "tools/list":
			out(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"tools": []any{map[string]any{"name": "t"}}}})
		case "tools/call":
			switch mode {
			case "silent":
				select {}
			case "big":
				io.WriteString(os.Stdout, `{"jsonrpc":"2.0","id":`+fmt.Sprint(m.ID)+`,"result":{"content":[{"type":"text","text":"`)
				io.WriteString(os.Stdout, strings.Repeat("x", mcpMaxMessage))
				io.WriteString(os.Stdout, "\"}]}}\n")
			}
		}
	}
}

func helperSpec(t *testing.T, mode string) MCPServerSpec {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return MCPServerSpec{Name: "h", Command: exe, Args: []string{"-test.run=^TestMCPHelper$"},
		Env: []string{"SPRIG_MCP_HELPER=" + mode}}
}

func TestMCPCallTimeoutIsAToolResult(t *testing.T) {
	old := mcpCallTimeout
	mcpCallTimeout = 1500 * time.Millisecond
	defer func() { mcpCallTimeout = old }()
	set, err := StartMCP([]MCPServerSpec{helperSpec(t, "silent")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	_, err = set.Tools[0].Run(map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("err=%v", err)
	}
	// The server is dead from then on, and says so at once.
	if _, err = set.Tools[0].Run(map[string]any{}); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("second call: %v", err)
	}
}

func TestMCPMessageOver32MiBKillsTheServer(t *testing.T) {
	set, err := StartMCP([]MCPServerSpec{helperSpec(t, "big")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	_, err = set.Tools[0].Run(map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseMCPConfig(t *testing.T) {
	specs, _, err := ParseMCPConfig(`{"mcpServers":{"b":{"command":"x"},"a":{"command":"y","args":["1"],"env":{"K":"v","ANTHROPIC_API_KEY":"no"}}}}`)
	if err != nil || len(specs) != 2 || specs[0].Name != "a" || specs[1].Name != "b" {
		t.Fatalf("specs=%v err=%v", specs, err)
	}
	if len(specs[0].Env) != 1 || specs[0].Env[0] != "K=v" {
		t.Fatalf("a key variable reached a server: %v", specs[0].Env)
	}
	for _, bad := range []string{`{}`, `{"mcpServers":{"a":{"type":"sse","url":"u"}}}`, `{"mcpServers":{"a":{}}}`, `{"mcpServers":{"a":{"command":""}}}`} {
		if _, _, err := ParseMCPConfig(bad); err == nil {
			t.Fatalf("%s was taken", bad)
		}
	}
}
