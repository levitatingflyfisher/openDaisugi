package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/opendaisugi/coppice/internal/web"
)

// onlyClient waits for the one client the server holds and returns it.
func onlyClient(t *testing.T, s *Server) *Client {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		var got []*Client
		for c := range s.clients {
			got = append(got, c)
		}
		s.mu.Unlock()
		if len(got) == 1 {
			return got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the server does not hold exactly one client")
	return nil
}

func okReply(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	if r["ok"] != true {
		t.Fatalf("reply %v, want ok", r)
	}
	res, _ := r["result"].(map[string]any)
	return res
}

func badRequest(t *testing.T, r map[string]any, msg string) {
	t.Helper()
	e, _ := r["error"].(map[string]any)
	if r["ok"] != false || e["code"] != "bad_request" || !strings.Contains(e["message"].(string), msg) {
		t.Fatalf("reply %v, want bad_request with %q", r, msg)
	}
}

// A socket hello from this uid names the connection.
func TestASocketHelloNamesTheConnection(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"bob"}`)
	if res := okReply(t, next()); res["name"] != "bob" {
		t.Fatalf("hello result %v, want name bob", res)
	}
	if name, from := onlyClient(t, s).Who(); name != "bob" || from != "socket" {
		t.Fatalf("Who() = %q, %q, want bob, socket", name, from)
	}
}

func TestAnUnnamedConnectionHasNoName(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true})
	defer stop()
	send(`{"id":"1","cmd":"hello"}`)
	next()
	if name, from := onlyClient(t, s).Who(); name != "" || from != "none" {
		t.Fatalf("Who() = %q, %q, want no name", name, from)
	}
}

func TestANameIsOneShortWord(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true})
	defer stop()
	for i, bad := range []string{"has space", "a.b", "abcdefghijklmnopqrstuvwxyz0123456", "ålice"} {
		send(`{"id":"` + string(rune('a'+i)) + `","cmd":"hello","name":"` + bad + `"}`)
		badRequest(t, next(), web.NameRule)
	}
	if name, _ := onlyClient(t, s).Who(); name != "" {
		t.Fatalf("a refused name stuck: %q", name)
	}
}

// A name sticks. A later hello may repeat it but not change it.
func TestANameCannotChange(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice"}`)
	okReply(t, next())
	send(`{"id":"2","cmd":"hello","name":"bob"}`)
	badRequest(t, next(), "this connection is alice.")
	send(`{"id":"3","cmd":"hello","name":"alice"}`)
	okReply(t, next())
	send(`{"id":"4","cmd":"hello"}`)
	okReply(t, next())
	if name, _ := onlyClient(t, s).Who(); name != "alice" {
		t.Fatalf("Who() = %q, want alice", name)
	}
}

// A pane has its pane id for a name, whatever its hello says.
func TestAPaneIsNamedByItsPane(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p1"})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice"}`)
	if res := okReply(t, next()); res["name"] != "pane:w1:p1" {
		t.Fatalf("hello result %v, want name pane:w1:p1", res)
	}
	if name, from := onlyClient(t, s).Who(); name != "pane:w1:p1" || from != "pane" {
		t.Fatalf("Who() = %q, %q", name, from)
	}
}

// A connection that named itself and then became a pane is that pane.
func TestANamedConnectionThatBecomesAPaneIsThePane(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice"}`)
	okReply(t, next())
	send(`{"id":"2","cmd":"hello","role":"pane","pane":"w1:p2"}`)
	okReply(t, next())
	if name, _ := onlyClient(t, s).Who(); name != "pane:w1:p2" {
		t.Fatalf("Who() = %q, want pane:w1:p2", name)
	}
}

func TestAPluginIsNamedByItsPlugin(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, plugin: "merge"})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice"}`)
	next()
	if name, from := onlyClient(t, s).Who(); name != "plugin:merge" || from != "plugin" {
		t.Fatalf("Who() = %q, %q", name, from)
	}
}

// Only the server's own phone server can say a name came from a token.
// From any other process the name is a socket name.
func TestATokenNameCountsOnlyFromTheServerItself(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, self: true})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice","name_from":"token"}`)
	okReply(t, next())
	if name, from := onlyClient(t, s).Who(); name != "alice" || from != "token" {
		t.Fatalf("Who() = %q, %q, want alice, token", name, from)
	}
	stop()

	s2 := newTestServer(t)
	send2, next2, stop2 := streamFacts(t, s2, &peerFacts{checked: true})
	defer stop2()
	send2(`{"id":"1","cmd":"hello","name":"alice","name_from":"token"}`)
	okReply(t, next2())
	if name, from := onlyClient(t, s2).Who(); name != "alice" || from != "socket" {
		t.Fatalf("Who() = %q, %q, want alice, socket", name, from)
	}
}

// The phone server fixes the name first. The browser's own hello then
// cannot name the connection, even when the token had no name.
func TestABrowserCannotRenameItsConnection(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, self: true})
	defer stop()
	send(`{"id":"1","cmd":"hello","name_from":"token"}`)
	okReply(t, next())
	send(`{"id":"2","cmd":"hello","name":"mallory"}`)
	badRequest(t, next(), "this connection takes its name from its token.")
	send(`{"id":"3","cmd":"hello","name":"mallory","name_from":"token"}`)
	badRequest(t, next(), "this connection takes its name from its token.")
	if name, from := onlyClient(t, s).Who(); name != "" || from != "none" {
		t.Fatalf("Who() = %q, %q, want no name", name, from)
	}
}

func TestNameFromMustBeTokenOrSocket(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true})
	defer stop()
	send(`{"id":"1","cmd":"hello","name":"alice","name_from":"passport"}`)
	badRequest(t, next(), "name_from must be token or socket")
}

// The kernel places the server's own pid as the server itself.
func TestClassifyPeerMarksTheServerItself(t *testing.T) {
	f := classifyPeer(42, 42, false, nil, nil, func(int) (int, int, error) { return 1, 1, nil })
	if !f.self || f.pane || f.unknown {
		t.Fatalf("facts %+v, want self", f)
	}
}

// answerOf reads the answer file the gate reads for ask id.
func answerOf(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.cfg.GateRoot, "answers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// An allow from a connection named alice names alice in the answer.
func TestAnAllowFromAliceNamesAlice(t *testing.T) {
	s := newAgentServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	got := roundTrip(t, s,
		`{"id":"0","cmd":"hello","name":"alice"}`,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-3", 1),
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","confirm":"auth fix"}`)
	if !got[3].OK {
		t.Fatalf("agent.allow: %+v", got[3].Error)
	}
	if a := answerOf(t, s, "ask-3"); a["by"] != "alice" || a["whoFrom"] != "socket" {
		t.Fatalf("answer = %v, want by alice from socket", a)
	}
}

// An answer from an unnamed operator is local, never empty.
func TestADenyFromNoNameIsLocal(t *testing.T) {
	s := newAgentServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-3", 1),
		`{"id":"2","cmd":"agent.deny","pane":"w1:p1","ask":"ask-3"}`)
	if !got[2].OK {
		t.Fatalf("agent.deny: %+v", got[2].Error)
	}
	if a := answerOf(t, s, "ask-3"); a["by"] != "local" || a["whoFrom"] != "none" {
		t.Fatalf("answer = %v, want by local from none", a)
	}
}

func operatorReport(pane, who string) string {
	extra := ""
	if who != "" {
		extra = `,"who":"` + who + `"`
	}
	return `{"id":"r","cmd":"pane.report_state","pane":"` + pane + `","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"` + pane +
		`","state":"idle","source":"operator","detail":""` + extra + `}}`
}

// The server stamps an operator report with the name of the connection
// that made it. A name the report itself carries counts for nothing.
func TestAnOperatorReportCarriesTheConnectionsName(t *testing.T) {
	s := newAgentServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","name":"alice"}`)
	next()
	send(strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	next()
	send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1"}`)
	for r := next(); r["id"] != "a"; r = next() {
	}
	send(operatorReport("w1:p1", "mallory"))
	for r := next(); r["id"] != "r"; r = next() {
	}
	eff, _ := s.effectiveState("w1:p1")
	if eff.Source != "operator" || eff.Who == nil || *eff.Who != "alice" {
		t.Fatalf("state = %+v, want an operator event from alice", eff)
	}
}

// A report that is not the operator's carries no name, whatever it says.
func TestAGateReportCarriesNoName(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		`{"id":"0","cmd":"hello","name":"alice"}`,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		operatorReport("w1:p1", "mallory"))
	if !got[2].OK {
		t.Fatalf("report: %+v", got[2].Error)
	}
	eff, _ := s.effectiveState("w1:p1")
	if eff.Who != nil {
		t.Fatalf("a downgraded report kept who %q", *eff.Who)
	}
}

// The whole path: a token minted for alice, a websocket opened with it
// through the phone server, and a connection on the real socket whose
// name is alice from token. The test process is the server process, so
// the kernel places the phone server's connection as the server itself.
func TestAWebsocketWithAliceTokenIsAlice(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	store := web.TokenStore{Path: filepath.Join(t.TempDir(), "tok")}
	tok, err := store.MintFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := web.New(web.Options{Dial: web.UnixDialer{Path: s.cfg.SocketPath}, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: []string{web.WSSubprotocol, web.WSBearerPrefix + tok}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		var clients []*Client
		for cl := range s.clients {
			clients = append(clients, cl)
		}
		s.mu.Unlock()
		for _, cl := range clients {
			if name, from := cl.Who(); name == "alice" {
				if from != "token" {
					t.Fatalf("Who() = alice, %q, want token", from)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no connection on the socket is alice")
}

// The phone server's hello for a local pane behind a named token carries
// both the token's name and the pane's pid. The pane wins: the connection
// is that pane, it cannot allow, and it does not count as looking.
func TestANamedWebHelloFromAPaneIsThePane(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	lp, ok := s.Live("w1:p1")
	if !ok || lp.PTY.Pid() <= 0 {
		t.Fatal("no live pty pane")
	}
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, self: true})
	defer stop()
	send(fmt.Sprintf(`{"id":"0","cmd":"hello","name":"alice","name_from":"token","peer_pids":[%d]}`, lp.PTY.Pid()))
	res := okReply(t, next())
	if res["role"] != "pane" || res["allow"] != false || res["name"] != "pane:w1:p1" {
		t.Fatalf("hello answered %v, want the pane w1:p1 with no allow", res)
	}
	var c *Client
	s.mu.Lock()
	for cl := range s.clients {
		if name, _ := cl.Who(); name == "pane:w1:p1" {
			c = cl
		}
	}
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no connection is the pane")
	}
	send(`{"id":"1","cmd":"agent.allow","pane":"w1:p1","ask":"none"}`)
	refusedWith(t, next())
	send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1","view_only":true}`)
	okReply(t, replyTo(next, "a"))
	if got := lookingOf(t, s, "w1:p1"); got != nil {
		t.Fatalf("looking = %v, want nobody", got)
	}
}
