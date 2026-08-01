package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	sprig "github.com/opendaisugi/sprig"
)

// decodeResult pulls the JSON-RPC result object out of a response, failing on a
// transport-level rpc error.
func decodeResult(t *testing.T, resp []byte) map[string]any {
	t.Helper()
	var r struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("bad response JSON: %v (%s)", err, resp)
	}
	if r.Error != nil {
		t.Fatalf("unexpected rpc error: %s", r.Error.Message)
	}
	return r.Result
}

func TestInitializeReturnsProtocolAndServerInfo(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	resp, ok := s.Handle([]byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`))
	if !ok {
		t.Fatal("initialize must produce a response")
	}
	res := decodeResult(t, resp)
	if res["protocolVersion"] == nil {
		t.Fatal("initialize must return a protocolVersion")
	}
	si, _ := res["serverInfo"].(map[string]any)
	if si == nil || si["name"] != "sprig" {
		t.Fatalf("serverInfo.name must be sprig, got %v", res["serverInfo"])
	}
}

func TestNotificationProducesNoResponse(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	_, ok := s.Handle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if ok {
		t.Fatal("a notification (no id) must not produce a response")
	}
}

func TestToolsListAdvertisesTheFourTools(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	resp, _ := s.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	res := decodeResult(t, resp)
	tools, _ := res["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("want 4 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tv := range tools {
		m := tv.(map[string]any)
		names[m["name"].(string)] = true
		if m["inputSchema"] == nil {
			t.Fatalf("tool %v must carry an inputSchema", m["name"])
		}
	}
	for _, want := range []string{"read", "write", "edit", "bash"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q; got %v", want, names)
		}
	}
}

func TestToolsCallRunsAnAllowedToolThroughTheExecutor(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	resp, _ := s.Handle([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bash","arguments":{"cmd":"echo hi"}}}`))
	res := decodeResult(t, resp)
	if res["isError"] == true {
		t.Fatalf("an allowed call must not be an error: %v", res)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "hi") {
		t.Fatalf("want the tool output to reach the caller, got %q", text)
	}
}

func TestToolsCallGateDenialIsAToolErrorWithTheReason(t *testing.T) {
	// The crux of the MCP bridge: a denied call comes back to the DRIVER as a tool
	// error it can see and adapt to — fail-closed, but legible, same as the loop.
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.DenyAll{Reason: "outside envelope"}))
	resp, _ := s.Handle([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"cmd":"rm -rf /"}}}`))
	res := decodeResult(t, resp)
	if res["isError"] != true {
		t.Fatalf("a gate denial must be reported as a tool error, got %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "outside envelope") {
		t.Fatalf("the refusal reason must be surfaced, got %q", text)
	}
}

func TestServeStreamsOneResponsePerRequestAndSkipsNotifications(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":0,"method":"initialize"}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out strings.Builder
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses (the notification is silent), got %d: %q", len(lines), out.String())
	}
	res := decodeResult(t, []byte(lines[1]))
	if tools, _ := res["tools"].([]any); len(tools) != 4 {
		t.Fatalf("second response must be the 4-tool listing, got %v", res)
	}
}

func TestUnknownMethodIsAnRPCError(t *testing.T) {
	s := NewServer(sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}))
	resp, ok := s.Handle([]byte(`{"jsonrpc":"2.0","id":9,"method":"nonesuch"}`))
	if !ok {
		t.Fatal("a request with an id must always get a response")
	}
	var r struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(resp, &r)
	if r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("unknown method must be JSON-RPC error -32601, got %s", resp)
	}
}
