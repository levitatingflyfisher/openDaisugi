package foreman

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeToolSocket serves one answer per connection and records the requests.
func fakeToolSocket(t *testing.T, answer func(ToolRequest) ToolResponse) (string, *[]ToolRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var seen []ToolRequest
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req ToolRequest
			_ = json.Unmarshal(line, &req)
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			b, _ := json.Marshal(answer(req))
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return path, &seen
}

// mcp sends the lines to ToolsMain and returns the responses.
func mcp(t *testing.T, sock string, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	var errb strings.Builder
	code := ToolsMain([]string{"--sock", sock}, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var got []map[string]any
	dec := json.NewDecoder(strings.NewReader(out.String()))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	return got
}

func TestTheRelayOffersExactlyFourTools(t *testing.T) {
	sock, _ := fakeToolSocket(t, func(ToolRequest) ToolResponse { return ToolResponse{OK: true} })
	got := mcp(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(got) != 2 {
		t.Fatalf("%d responses", len(got))
	}
	tools := got[1]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "zoom,date,spawn,status" {
		t.Fatalf("tools = %v", names)
	}
}

func TestTheRelayPassesACallToTheSocketAndBack(t *testing.T) {
	sock, seen := fakeToolSocket(t, func(r ToolRequest) ToolResponse {
		if r.Tool == "date" {
			return ToolResponse{Error: "no message 9"}
		}
		return ToolResponse{OK: true, Result: "the text"}
	})
	got := mcp(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":4}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"date","arguments":{"id":9}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"cmd":"ls"}}}`)
	text := func(i int) (string, bool) {
		r := got[i]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if s, bad := text(0); s != "the text" || bad {
		t.Fatalf("zoom = %q %v", s, bad)
	}
	if s, bad := text(1); s != "no message 9" || !bad {
		t.Fatalf("date = %q %v", s, bad)
	}
	if s, bad := text(2); !bad || !strings.Contains(s, "no tool named") {
		t.Fatalf("bash = %q %v", s, bad)
	}
	if len(*seen) != 2 || (*seen)[0].Tool != "zoom" || (*seen)[0].Args["n"].(float64) != 4 {
		t.Fatalf("the socket saw %+v", *seen)
	}
}

func TestACallAfterTheTurnIsOverSaysSo(t *testing.T) {
	got := mcp(t, filepath.Join(t.TempDir(), "gone.sock"),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{}}}`)
	r := got[0]["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !r["isError"].(bool) || !strings.Contains(text, "this turn is over") {
		t.Fatalf("result = %v", r)
	}
}
