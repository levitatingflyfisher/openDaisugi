//go:build linux

package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"
	"github.com/opendaisugi/coppice/internal/web"
)

// TestWebDoorHelper is not a test on its own. A pane runs this test binary
// with COPPICE_WEB_HELPER set, and it tries to answer an ask through the
// web server, then prints what came back on the pane's screen.
func TestWebDoorHelper(t *testing.T) {
	mode := os.Getenv("COPPICE_WEB_HELPER")
	if mode == "" {
		t.Skip("run only inside a pane")
	}
	base, tok := os.Getenv("COPPICE_WEB_URL"), os.Getenv("COPPICE_WEB_TOKEN")
	switch mode {
	case "post":
		body := `{"tool_use_id":"ask-3","decision":"allow","reason":"x"}`
		req, _ := http.NewRequest("POST", base+"/api/ask/answer", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("RESULT failed", err)
			break
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("RESULT %d %s END\n", resp.StatusCode, strings.TrimSpace(string(b)))
	case "ws":
		h := http.Header{}
		h.Set("Authorization", "Bearer "+tok)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws",
			&websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			fmt.Println("RESULT failed", err)
			break
		}
		_ = c.Write(ctx, websocket.MessageText,
			[]byte(`{"id":"x","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`))
		for {
			_, line, err := c.Read(ctx)
			if err != nil {
				fmt.Println("RESULT failed", err)
				break
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil && m["id"] == "x" {
				fmt.Printf("RESULT %s END\n", line)
				break
			}
		}
		c.Close(websocket.StatusNormalClosure, "")
	}
	time.Sleep(10 * time.Second)
}

// doorFixture is a coppice server and a web server in this process, with
// one pending ask.
type doorFixture struct {
	sock, gate, url, tok string
}

func newDoorFixture(t *testing.T) doorFixture {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	if len(sock) > 100 {
		t.Skip("the temp dir is too long for a unix socket path")
	}
	gate := filepath.Join(dir, "gate")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir, GateRoot: gate})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })

	store := web.TokenStore{Path: web.TokenPath(dir)}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := web.New(web.Options{Dial: web.UnixDialer{Path: sock}, Tokens: store, Gate: web.AskChannel{Root: gate}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	if err := os.MkdirAll(filepath.Join(gate, "asks"), 0o700); err != nil {
		t.Fatal(err)
	}
	ask := `{"toolUseId":"ask-3","nonce":"n1","postedAt":1,"deadline":9999999999,"tier":"undoable"}`
	if err := os.WriteFile(filepath.Join(gate, "asks", "ask-3.json"), []byte(ask), 0o600); err != nil {
		t.Fatal(err)
	}
	return doorFixture{sock: sock, gate: gate, url: ts.URL, tok: tok}
}

// call sends one request on the coppice socket and returns the reply.
func call(t *testing.T, sock string, req map[string]any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req["id"] = "t"
	if err := proto.NewEncoder(conn).Send(req); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// runInPane runs the helper in a pty pane and returns what it printed
// between RESULT and END.
func runInPane(t *testing.T, f doorFixture, mode string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := call(t, f.sock, map[string]any{
		"cmd": "pane.create", "cwd": t.TempDir(), "kind": "pty", "cols": 200, "rows": 20,
		"cmd_argv": []string{self, "-test.run=^TestWebDoorHelper$"},
		"env": map[string]string{
			"COPPICE_WEB_HELPER": mode, "COPPICE_WEB_URL": f.url, "COPPICE_WEB_TOKEN": f.tok,
		},
	})
	res, _ := r["result"].(map[string]any)
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create: %v", r)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rr := call(t, f.sock, map[string]any{"cmd": "pane.read", "pane": id})
		rres, _ := rr["result"].(map[string]any)
		text, _ := rres["text"].(string)
		text = strings.ReplaceAll(text, "\n", "")
		if i := strings.Index(text, "RESULT"); i >= 0 {
			if j := strings.Index(text[i:], "END"); j >= 0 {
				return text[i : i+j]
			}
			if strings.Contains(text[i:], "failed") {
				t.Fatalf("the helper failed: %q", text[i:])
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the helper printed no result")
	return ""
}

func noAnswer(t *testing.T, f doorFixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.gate, "answers", "ask-3.json")); err == nil {
		t.Fatal("a pane's answer reached the gate")
	}
}

// A process inside a pane cannot answer an ask through the phone route,
// with the token in hand.
func TestAPaneCannotAllowThroughTheWebAnswerRoute(t *testing.T) {
	f := newDoorFixture(t)
	got := runInPane(t, f, "post")
	if !strings.Contains(got, "403") || !strings.Contains(got, web.PaneRefusal) {
		t.Fatalf("the pane's POST answered %q", got)
	}
	noAnswer(t, f)
}

// A process inside a pane cannot answer an ask with an allow line over the
// websocket either.
func TestAPaneCannotAllowOverTheWebSocket(t *testing.T) {
	f := newDoorFixture(t)
	got := runInPane(t, f, "ws")
	if !strings.Contains(got, "unauthorized") || !strings.Contains(got, "cannot allow") {
		t.Fatalf("the pane's websocket allow answered %q", got)
	}
	noAnswer(t, f)
}

// The operator's own process still answers through the route.
func TestTheOperatorStillAnswersThroughTheWebRoute(t *testing.T) {
	f := newDoorFixture(t)
	r := call(t, f.sock, map[string]any{
		"cmd": "pane.create", "cwd": t.TempDir(), "kind": "pty", "cols": 80, "rows": 20,
		"cmd_argv": []string{"sh", "-c", "sleep 30"},
	})
	res, _ := r["result"].(map[string]any)
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create: %v", r)
	}
	rep := call(t, f.sock, map[string]any{"cmd": "pane.report_state", "pane": id, "event": map[string]any{
		"v": 1, "ts": 1, "session_id": "s", "harness": "claude-code", "pane": id,
		"state": "blocked", "source": "gate",
		"ask": map[string]any{"id": "ask-3", "tool": "Bash", "summary": "ls", "deadline": 9999999999.0, "tier": "undoable"},
	}})
	if rep["ok"] != true {
		t.Fatalf("pane.report_state: %v", rep)
	}
	body := `{"tool_use_id":"ask-3","decision":"allow","reason":"x"}`
	req, _ := http.NewRequest("POST", f.url+"/api/ask/answer", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(f.gate, "answers", "ask-3.json")); err != nil {
		t.Fatalf("no answer written: %v", err)
	}
}
