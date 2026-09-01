package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// firstHello dials the websocket with tok and returns the first line the
// coppice socket saw, read as JSON.
func firstHello(t *testing.T, tok string, f *fakeServer, url string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsAddr(url), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"id":"1","cmd":"pane.list"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	recv := f.Received()
	if len(recv) < 2 {
		t.Fatalf("the socket saw %q, want a hello and then the browser's line", recv)
	}
	var m map[string]any
	if err := json.Unmarshal(recv[0], &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != webHelloID || m["cmd"] != "hello" {
		t.Fatalf("first line %v, want the web hello", m)
	}
	return m
}

// A websocket opened with a named token names its connection upstream
// before any line from the browser.
func TestAWebsocketWithANamedTokenSaysItsNameFirst(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	ts, s, _ := newTestServer(t, Options{Dial: d})
	alice, err := s.opts.Tokens.MintFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	m := firstHello(t, alice, f, ts.URL)
	if m["name"] != "alice" || m["name_from"] != "token" {
		t.Fatalf("hello %v, want name alice from token", m)
	}
}

// The operator's own token has no name. Its hello still says the name
// comes from the token, so a browser cannot name itself later.
func TestAWebsocketWithTheMainTokenFixesNoName(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	m := firstHello(t, tok, f, ts.URL)
	if _, has := m["name"]; has || m["name_from"] != "token" {
		t.Fatalf("hello %v, want no name and name_from token", m)
	}
}

// The token check tells the page the name its token carries.
func TestTheTokenCheckSaysTheName(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, s, tok := newTestServer(t, Options{Dial: d})
	alice, err := s.opts.Tokens.MintFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tok, want string }{{alice, "alice"}, {tok, ""}} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/token/check", nil)
		req.Header.Set("Authorization", "Bearer "+c.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		got, _ := body["name"].(string)
		if resp.StatusCode != 200 || got != c.want {
			t.Fatalf("token check = %d %v, want name %q", resp.StatusCode, body, c.want)
		}
	}
}

// answerFile posts one answer with tok and returns the answer file the
// gate reads.
func answerFile(t *testing.T, tok, decision string) map[string]any {
	t.Helper()
	_, d := newFakeServer(t, agentsWithAsk("toolu_w", "undoable"))
	gate := t.TempDir()
	writeTieredAsk(t, gate, "toolu_w", "undoable")
	ts, s, main := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}})
	if tok == "" {
		tok = main
	} else {
		var err error
		if tok, err = s.opts.Tokens.MintFor(tok); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/ask/answer",
		strings.NewReader(`{"tool_use_id":"toolu_w","decision":"`+decision+`"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := os.ReadFile(filepath.Join(gate, "answers", "toolu_w.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// An answer from a named token names who gave it, from the token.
func TestAnAnswerFromANamedTokenNamesWhoGaveIt(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		body := answerFile(t, "alice", decision)
		if body["by"] != "alice" || body["whoFrom"] != "token" {
			t.Fatalf("%s answer = %v, want by alice from token", decision, body)
		}
	}
}

// The operator's own token has no name. Its answer is local, never empty.
func TestAnAnswerFromTheMainTokenIsLocal(t *testing.T) {
	body := answerFile(t, "", "allow")
	if body["by"] != "local" || body["whoFrom"] != "none" {
		t.Fatalf("answer = %v, want by local from none", body)
	}
}

func TestAnswerWithNoNameWritesLocal(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "t1", "undoable")
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "t1", Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "answers", "t1.json"))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["by"] != "local" || body["whoFrom"] != "none" {
		t.Fatalf("answer = %v, want by local from none", body)
	}
}

// A web hello the coppice server refuses closes the websocket before any
// browser line goes upstream, so a browser can never name the connection
// in its place.
func TestARefusedWebHelloClosesTheSocket(t *testing.T) {
	f, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		if req["cmd"] == "hello" {
			return []map[string]any{{"id": req["id"], "ok": false,
				"error": map[string]any{"code": "bad_request", "message": "no"}}}
		}
		return echoOK(req)
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":"1","cmd":"hello","name":"x","name_from":"token"}`))
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("read after a refused hello = %v, want an internal error close", err)
	}
	for _, line := range f.Received() {
		if !isWebHelloReply(line) {
			t.Fatalf("a browser line went upstream after a refused hello: %s", line)
		}
	}
}
