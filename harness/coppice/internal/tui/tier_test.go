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

// answerSocket is a fake server that records every request and answers
// ok, or with the refusal refuse returns for it.
type answerSocket struct {
	mu   sync.Mutex
	reqs []map[string]any
	path string
}

func newAnswerSocket(t *testing.T, refuse func(map[string]any) string) *answerSocket {
	t.Helper()
	s := &answerSocket{path: filepath.Join(t.TempDir(), "s.sock")}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				_ = json.Unmarshal(line, &req)
				s.mu.Lock()
				s.reqs = append(s.reqs, req)
				s.mu.Unlock()
				out := map[string]any{"id": req["id"], "ok": true, "result": map[string]any{}}
				if msg := refuse(req); msg != "" {
					out = map[string]any{"id": req["id"], "ok": false,
						"error": map[string]any{"code": "unauthorized", "message": msg}}
				}
				b, _ := json.Marshal(out)
				_, _ = c.Write(append(b, '\n'))
			}()
		}
	}()
	return s
}

func (s *answerSocket) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.reqs...)
}

func never(map[string]any) string { return "" }

// peekFloor is a floor with one blocked pane labelled label and a peek
// open on its ask at tier.
func peekFloor(t *testing.T, label, tier string, sock *answerSocket) *floor {
	t.Helper()
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: label, State: "blocked"}}}
	ask := map[string]any{"id": "toolu_1", "tool": "Bash", "summary": "git push --force"}
	if tier != "" {
		ask["tier"] = tier
	}
	m.OpenPeek("screen text", ask)
	f := testFloor(m, 100)
	f.o.Socket = sock.path
	return f
}

func press(t *testing.T, f *floor, keys string) {
	t.Helper()
	for i := 0; i < len(keys); i++ {
		k := key{kind: keyByte, b: keys[i]}
		if keys[i] == ' ' {
			k = key{kind: keyByte, b: ' '}
		}
		if _, err := f.handle(k); err != nil {
			t.Fatal(err)
		}
	}
}

func rendered(f *floor) string {
	return strings.Join(Render(f.m, 100, 20), "\n")
}

func TestAPermanentPeekAsksForTheName(t *testing.T) {
	f := peekFloor(t, "gate-refactor", "permanent", newAnswerSocket(t, never))
	out := rendered(f)
	for _, want := range []string{"Deny is the default.", "n deny", "type gate-refactor and enter"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the peek does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "y allow once") {
		t.Fatalf("a permanent peek offers y:\n%s", out)
	}
}

func TestAnAskWithNoTierPeeksAsPermanent(t *testing.T) {
	f := peekFloor(t, "gate-refactor", "", newAnswerSocket(t, never))
	if !strings.Contains(rendered(f), "type gate-refactor and enter") {
		t.Fatal("an ask with no tier did not peek as permanent")
	}
}

func TestYOnAPermanentPeekOnlySaysTypeTheName(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "gate-refactor", "permanent", sock)
	press(t, f, "y")
	if f.m.Message != "type the pane name to allow" {
		t.Fatalf("message %q", f.m.Message)
	}
	if n := len(sock.requests()); n != 0 {
		t.Fatalf("y sent %d requests", n)
	}
	if f.m.Prompt != "" || f.m.Talking {
		t.Fatalf("y went into the prompt: %q", f.m.Prompt)
	}
	press(t, f, "t")
	if f.m.Message != "type the pane name to allow" || len(sock.requests()) != 0 {
		t.Fatalf("t on a permanent peek: message %q, %d requests", f.m.Message, len(sock.requests()))
	}
}

func TestTypingTheNameThenEnterAllowsAPermanentAsk(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "gate-refactor", "permanent", sock)
	press(t, f, "\x14gate-refactor\r")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "agent.allow" || reqs[0]["confirm"] != "gate-refactor" ||
		reqs[0]["pane"] != "w1:p1" || reqs[0]["ask"] != "toolu_1" {
		t.Fatalf("requests %v", reqs)
	}
	if f.m.Peek != nil {
		t.Fatal("the peek stayed open after the answer")
	}
}

func TestAWrongNameAllowsNothingAndTalksToNobody(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "gate-refactor", "permanent", sock)
	press(t, f, "\x14gate-refactr\r")
	if n := len(sock.requests()); n != 0 {
		t.Fatalf("a wrong name sent %v", sock.requests())
	}
	if !strings.Contains(f.m.Message, "not the pane name") {
		t.Fatalf("message %q", f.m.Message)
	}
	if f.m.Peek == nil {
		t.Fatal("a wrong name closed the peek")
	}
	// The prompt is empty again, so n denies at once.
	press(t, f, "n")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "agent.deny" {
		t.Fatalf("requests %v", reqs)
	}
}

func TestNDeniesInOneKeyOnEitherTier(t *testing.T) {
	for _, tier := range []string{"permanent", "undoable"} {
		sock := newAnswerSocket(t, never)
		f := peekFloor(t, "gate-refactor", tier, sock)
		press(t, f, "n")
		reqs := sock.requests()
		if len(reqs) != 1 || reqs[0]["cmd"] != "agent.deny" || reqs[0]["ask"] != "toolu_1" {
			t.Fatalf("%s: requests %v", tier, reqs)
		}
		if _, ok := reqs[0]["confirm"]; ok {
			t.Fatalf("%s: a deny carried confirm", tier)
		}
	}
}

func TestAnUndoablePeekAcceptsY(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "gate-refactor", "undoable", sock)
	out := rendered(f)
	if !strings.Contains(out, "Deny is the default.") || !strings.Contains(out, "y allow once  t allow for this task  n deny") {
		t.Fatalf("the undoable peek:\n%s", out)
	}
	press(t, f, "y")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "agent.allow" {
		t.Fatalf("requests %v", reqs)
	}
	if _, ok := reqs[0]["confirm"]; ok {
		t.Fatal("an undoable allow carried confirm")
	}
	if _, ok := reqs[0]["scope"]; ok {
		t.Fatal("y carried a scope")
	}
}

func TestTAllowsForTheTask(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "gate-refactor", "undoable", sock)
	press(t, f, "t")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "agent.allow" || reqs[0]["scope"] != "task" {
		t.Fatalf("requests %v", reqs)
	}
}

// A name that starts with a bound key cannot be typed from an empty
// prompt, so the hint says to open the prompt first.
func TestANameThatStartsWithAKeyNamesCtrlT(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := peekFloor(t, "notes", "permanent", sock)
	if !strings.Contains(rendered(f), "press ctrl-t, type notes and enter") {
		t.Fatalf("the peek:\n%s", rendered(f))
	}
	if _, err := f.handle(key{kind: keyByte, b: 0x14}); err != nil {
		t.Fatal(err)
	}
	press(t, f, "notes\r")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "agent.allow" || reqs[0]["confirm"] != "notes" {
		t.Fatalf("requests %v", reqs)
	}
}

// A pane with no label is named by its id, the same rule the server uses.
func TestAPaneWithNoLabelIsNamedByItsID(t *testing.T) {
	f := peekFloor(t, "", "permanent", newAnswerSocket(t, never))
	if !strings.Contains(rendered(f), "type w1:p1 and enter") {
		t.Fatalf("the peek:\n%s", rendered(f))
	}
}

// A refusal from the server shows on the message line and leaves the peek.
func TestARefusalShowsAndKeepsThePeek(t *testing.T) {
	sock := newAnswerSocket(t, func(map[string]any) string {
		return "this cannot be undone. Type the pane name to allow: gate-refactor"
	})
	f := peekFloor(t, "gate-refactor", "undoable", sock)
	press(t, f, "y")
	if !strings.Contains(f.m.Message, "this cannot be undone") {
		t.Fatalf("message %q", f.m.Message)
	}
	if f.m.Peek == nil {
		t.Fatal("a refused allow closed the peek")
	}
}

// With no ask in the peek, y t n after ctrl-t are text for the prompt.
func TestKeysWithNoAskAreText(t *testing.T) {
	sock := newAnswerSocket(t, never)
	m := &Model{Rows: []Row{{ID: "w1:p1", State: "working"}}}
	m.OpenPeek("text", nil)
	f := testFloor(m, 100)
	f.o.Socket = sock.path
	press(t, f, "\x14ytn")
	if f.m.Prompt != "ytn" || len(sock.requests()) != 0 {
		t.Fatalf("prompt %q requests %v", f.m.Prompt, sock.requests())
	}
}
