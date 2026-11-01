package pando

import (
	"encoding/json"
	"strings"
	"testing"
)

// The MCP zoom takes child, as the work message tells the model; the label
// goes through ChildDir, so it cannot leave the children dir.
func TestTheMCPZoomReadsAChildStore(t *testing.T) {
	data := t.TempDir()
	work := seedStore(t, data, "work", "the parent line")
	seedStore(t, data, "home", "a private home line")
	ch, err := OpenChild(ctx, work, "w1", Config{Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	ch.Append(ctx, KindUser, "the worker's own words")
	ch.Close()
	c, err := OpenReadOnly(work)
	if err != nil {
		t.Fatal(err)
	}
	res := rpc(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1,"child":"w1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1,"child":"../../home"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1,"child":"nobody"}}}`,
	)
	b, _ := json.Marshal(res[0])
	if !strings.Contains(string(b), `"child"`) {
		t.Fatalf("the zoom schema has no child: %s", b)
	}
	if b, _ := json.Marshal(res[1]); !strings.Contains(string(b), "the worker's own words") {
		t.Fatalf("child zoom: %s", b)
	}
	for _, r := range res[2:] {
		b, _ := json.Marshal(r)
		if r["result"].(map[string]any)["isError"] != true || strings.Contains(string(b), "private home") {
			t.Fatalf("a bad child must be a tool error: %s", b)
		}
	}
}

// A running server answers from the store as it is now: a message logged
// after the server started is found.
func TestTheMCPServerSeesMessagesLoggedAfterItStarted(t *testing.T) {
	data := t.TempDir()
	work := seedStore(t, data, "work", "the first line")
	c, err := OpenReadOnly(work)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(ctx, Config{Dir: work})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(ctx, KindUser, "a line logged later")
	w.Close()
	res := rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zoom","arguments":{"id":1,"n":1}}}`)
	if b, _ := json.Marshal(res[0]); !strings.Contains(string(b), "a line logged later") {
		t.Fatalf("the server did not see the new message: %s", b)
	}
}
