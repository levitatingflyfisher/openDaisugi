package tui

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// s swaps the selected agent's model or router (F12a). A model goes at
// once; a router asks first, since it restarts the agent.

// fakeSwapServer answers agent.set_model with when queued and every other
// request ok, and keeps each swap request it gets.
func fakeSwapServer(t *testing.T) (sock string, got func() []map[string]any) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var sets []map[string]any
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req map[string]any
					_ = json.Unmarshal(sc.Bytes(), &req)
					reply := `{"id":"1","ok":true,"result":{"panes":[]}}`
					switch req["cmd"] {
					case "agent.set_model":
						reply = `{"id":"1","ok":true,"result":{"when":"queued"}}`
						fallthrough
					case "agent.set_router":
						mu.Lock()
						sets = append(sets, req)
						mu.Unlock()
					case "agent.move":
						reply = `{"id":"1","ok":true,"result":{"pane":"w1:p2","when":"queued"}}`
						if p, _ := req["preview"].(bool); p {
							reply = `{"id":"1","ok":true,"result":{"tokens":3200,"trimmed":true,"conversation":38000,"messages":5}}`
						}
						mu.Lock()
						sets = append(sets, req)
						mu.Unlock()
					}
					_, _ = c.Write([]byte(reply + "\n"))
				}
			}(c)
		}
	}()
	return sock, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), sets...)
	}
}

func typeLine(t *testing.T, f *floor, line string) {
	t.Helper()
	for _, c := range line {
		handleAll(t, f, key{kind: keyByte, b: byte(c)})
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
}

func TestSAsksForAModelAndSendsItAtOnce(t *testing.T) {
	sock, got := fakeSwapServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Harness: "claude", Kind: "pty"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 's'})
	if m.Swapping != "w1:p1" || m.PromptLine() != "swap auth: " {
		t.Fatalf("s did not open the line: %q %q", m.Swapping, m.PromptLine())
	}
	typeLine(t, f, "model opus")
	sets := got()
	if len(sets) != 1 || sets[0]["cmd"] != "agent.set_model" || sets[0]["pane"] != "w1:p1" || sets[0]["model"] != "opus" {
		t.Fatalf("requests %v", sets)
	}
	if want := "auth switches to opus at its next idle prompt."; m.Message != want {
		t.Fatalf("message %q, want %q", m.Message, want)
	}
	if m.Swapping != "" {
		t.Fatal("the line stayed open")
	}
}

func TestSRouterAsksFirstAndEnterRestartsTheAgent(t *testing.T) {
	sock, got := fakeSwapServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Harness: "claude", Kind: "pty"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 's'})
	typeLine(t, f, "router gateway")
	if len(got()) != 0 {
		t.Fatalf("the router was swapped before the question: %v", got())
	}
	if m.Confirm != "w1:p1" || m.ConfirmRouter != "gateway" ||
		m.PromptLine() != "Restarts auth and resumes its conversation. A turn in progress ends. Enter resumes, Esc keeps." {
		t.Fatalf("question: %q %q %q", m.Confirm, m.ConfirmRouter, m.PromptLine())
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	sets := got()
	if len(sets) != 1 || sets[0]["cmd"] != "agent.set_router" || sets[0]["router"] != "gateway" {
		t.Fatalf("requests %v", sets)
	}
	if want := "auth restarted and resumed through the gateway. It enforces again."; m.Message != want {
		t.Fatalf("message %q, want %q", m.Message, want)
	}
	if m.Confirm != "" || m.ConfirmRouter != "" {
		t.Fatal("the question stayed")
	}
}

func TestSRouterThenEscKeepsTheAgentAndOtherWordsGetTheHint(t *testing.T) {
	sock, got := fakeSwapServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Harness: "claude", Kind: "pty"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 's'})
	typeLine(t, f, "router direct")
	handleAll(t, f, key{kind: keyEsc})
	if len(got()) != 0 || m.Confirm != "" || m.ConfirmRouter != "" {
		t.Fatalf("Esc swapped: %v %q", got(), m.Confirm)
	}
	for _, line := range []string{"router switchyard", "model", "colour red", "model a b"} {
		handleAll(t, f, key{kind: keyByte, b: 's'})
		typeLine(t, f, line)
		if m.Message != SwapHint || len(got()) != 0 {
			t.Fatalf("line %q: message %q, requests %v", line, m.Message, got())
		}
	}
}

func TestSHasNothingForAnAgentThatNamesNoHarness(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "sh", State: "idle"}}}
	f := testFloor(m, 120)
	handleAll(t, f, key{kind: keyByte, b: 's'})
	if m.Swapping != "" || m.Message != "Swapping the model, router or loop of sh is not built for a pane with no harness yet." {
		t.Fatalf("swapping %q, message %q", m.Swapping, m.Message)
	}
	// Sprig headless has a model swap and no router swap. Only an agent whose conversation the server
	// reads as claude or headless sprig can move to another loop.
	if mdl, rtr, lp := swaps(Row{Harness: "sprig", Kind: KindHeadless}); !mdl || rtr || !lp {
		t.Fatalf("sprig headless: model %v router %v loop %v", mdl, rtr, lp)
	}
	if mdl, rtr, lp := swaps(Row{Harness: "codex"}); mdl || rtr || lp {
		t.Fatalf("codex: model %v router %v loop %v", mdl, rtr, lp)
	}
	if _, _, lp := swaps(Row{Harness: "sprig", Kind: "pty"}); lp {
		t.Fatal("a terminal sprig has no conversation to move")
	}
	if mdl, rtr, lp := swaps(Row{Harness: "claude", Parent: "w1:p1"}); mdl || rtr || lp {
		t.Fatalf("a subagent row: model %v router %v loop %v", mdl, rtr, lp)
	}
}

func TestSLoopShowsTheReplayCostFirstAndEnterMoves(t *testing.T) {
	sock, got := fakeSwapServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Harness: "claude", Kind: "pty"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 's'})
	typeLine(t, f, "loop codex")
	sets := got()
	if len(sets) != 1 || sets[0]["cmd"] != "agent.move" || sets[0]["preview"] != true || sets[0]["harness"] != "codex" {
		t.Fatalf("the question was not a preview: %v", sets)
	}
	want := "Moves about 3.2k tokens of conversation to codex. Older turns stay behind (about 38k in all). This agent stays. Enter moves, Esc keeps."
	if m.Confirm != "w1:p1" || m.ConfirmLoop != "codex" || m.PromptLine() != want {
		t.Fatalf("question: %q %q %q", m.Confirm, m.ConfirmLoop, m.PromptLine())
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	sets = got()
	if len(sets) != 2 || sets[1]["cmd"] != "agent.move" || sets[1]["preview"] != nil || sets[1]["harness"] != "codex" {
		t.Fatalf("requests %v", sets)
	}
	if want := "Started codex with the conversation of auth. auth stays. The brief goes in when codex is ready, or a note says it did not."; m.Message != want {
		t.Fatalf("message %q, want %q", m.Message, want)
	}
	if m.Confirm != "" || m.ConfirmLoop != "" || m.ConfirmLine != "" {
		t.Fatal("the question stayed")
	}
}

func TestSLoopThenEscMovesNothing(t *testing.T) {
	sock, got := fakeSwapServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Harness: "claude", Kind: "pty"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 's'})
	typeLine(t, f, "loop pi")
	handleAll(t, f, key{kind: keyEsc})
	for _, r := range got() {
		if r["preview"] != true {
			t.Fatalf("Esc moved the conversation: %v", got())
		}
	}
	if m.Confirm != "" || m.ConfirmLoop != "" || m.ConfirmLine != "" {
		t.Fatal("the question stayed")
	}
}

func TestMoveLineWordsMatchTheWebPage(t *testing.T) {
	cases := []struct {
		res  map[string]any
		want string
	}{
		{map[string]any{"tokens": 38000.0}, "Moves about 38k tokens of conversation to pi. This agent stays."},
		{map[string]any{"tokens": 900.0, "trimmed": true}, "Moves about 900 tokens of conversation to pi. Older turns stay behind. This agent stays."},
		{map[string]any{}, "Moves about 0 tokens of conversation to pi. This agent stays."},
		{map[string]any{"tokens": 2500000.0}, "Moves about 2.5M tokens of conversation to pi. This agent stays."},
	}
	for _, c := range cases {
		if got := MoveLine(c.res, "pi"); got != c.want {
			t.Fatalf("MoveLine(%v) = %q, want %q", c.res, got, c.want)
		}
	}
}

func TestTheFooterNamesSOnlyWhereAnAgentHasASwap(t *testing.T) {
	claude := &Model{Rows: []Row{{ID: "a", State: "working", Harness: "claude", Kind: "pty"}}}
	if lines := footerLines(claude, 200); !strings.Contains(strings.Join(lines, " "), "s swap") {
		t.Fatalf("a claude row does not name s: %q", lines)
	}
	// A harness the server cannot read a conversation from has no loop move.
	codex := &Model{Rows: []Row{{ID: "a", State: "working", Harness: "codex"}}}
	if lines := footerLines(codex, 200); strings.Contains(strings.Join(lines, " "), "s swap") {
		t.Fatalf("a codex row names s: %q", lines)
	}
	bare := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	if lines := footerLines(bare, 200); strings.Contains(strings.Join(lines, " "), "s swap") {
		t.Fatalf("a row with no harness names s: %q", lines)
	}
}
