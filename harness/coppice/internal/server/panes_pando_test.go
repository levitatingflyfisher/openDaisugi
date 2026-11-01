package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/pando"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/steward"
)

// claudeStub is a program that writes its arguments to out and exits.
func claudeStub(t *testing.T) (prog, out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(dir, "args.txt")
	prog = filepath.Join(dir, "claude-stub.sh")
	body := "#!/bin/sh\necho \"$@\" >> " + out + "\nexit 0\n"
	if err := os.WriteFile(prog, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return prog, out
}

func argLines(t *testing.T, out string, n int) []string {
	t.Helper()
	var lines []string
	waitFor(t, 5*time.Second, func() bool {
		b, _ := os.ReadFile(out)
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
		return len(b) > 0 && len(lines) >= n
	})
	return lines
}

func createClaudePane(t *testing.T, s *Server, asPane bool, prog string) string {
	t.Helper()
	req := fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"claude","cmd_argv":[%q,"--model","opus"]}`,
		t.TempDir(), prog)
	if !asPane {
		return result(t, roundTrip(t, s, req)[0])["pane"].(string)
	}
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p9"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	send(req)
	r := next()
	if r["ok"] != true {
		t.Fatalf("pane.create: %v", r)
	}
	return r["result"].(map[string]any)["pane"].(string)
}

func listed(s *Server) pando.SkipSet {
	return pando.LoadSkip(pando.SkipListIn(steward.StoreRoot(s.cfg.DataDir)))
}

// A pane an agent starts is a worker: marked, and its claude gets a session
// id that is on the skip list.
func TestAPaneAnAgentCreatesIsMarkedAndItsSessionIsListed(t *testing.T) {
	s := newEndedServer(t)
	prog, out := claudeStub(t)
	id := createClaudePane(t, s, true, prog)
	rec, _ := s.tree.Pane(id)
	if rec.Env[pando.SkipVar] != "1" || rec.Env[pando.SkipListVar] != pando.SkipListIn(steward.StoreRoot(s.cfg.DataDir)) {
		t.Fatalf("env = %v, want both Pando variables", rec.Env)
	}
	line := argLines(t, out, 1)[0]
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != "--session-id" || f[2] != "--model" {
		t.Fatalf("args = %q, want --session-id <id> before the others", line)
	}
	if !listed(s)[f[1]] {
		t.Fatalf("session %s is not on the skip list", f[1])
	}
	waitFor(t, 2*time.Second, func() bool { r, _ := s.tree.Pane(id); return r.HarnessSessionID == f[1] })
	if rec, _ := s.tree.Pane(id); strings.Contains(strings.Join(rec.Argv, " "), "--session-id") {
		t.Fatalf("the saved argv holds the flag: %v", rec.Argv)
	}
}

// The owner's panes, and the steward's own (the server asks for it itself,
// with no client), are not marked.
func TestAnOperatorsPaneIsNotMarked(t *testing.T) {
	s := newEndedServer(t)
	prog, out := claudeStub(t)
	id := createClaudePane(t, s, false, prog)
	rec, _ := s.tree.Pane(id)
	if _, ok := rec.Env[pando.SkipVar]; ok || rec.Env[pando.SkipListVar] != "" || rec.HarnessSessionID != "" {
		t.Fatalf("operator pane marked: env %v session %q", rec.Env, rec.HarnessSessionID)
	}
	if line := argLines(t, out, 1)[0]; strings.Contains(line, "--session-id") {
		t.Fatalf("args = %q, want no session id", line)
	}
	if len(listed(s)) != 0 {
		t.Fatalf("skip list = %v, want empty", listed(s))
	}
}

func TestTheStewardsOwnPaneIsNotMarked(t *testing.T) {
	s := newEndedServer(t)
	prog, _ := claudeStub(t)
	var req = mustRequest(t, fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"claude","label":"steward","cmd_argv":[%q]}`, t.TempDir(), prog))
	resp := s.handlePaneCreate(nil, &req)
	if !resp.OK {
		t.Fatalf("create: %+v", resp.Error)
	}
	for _, p := range s.tree.Panes() {
		if _, ok := p.Env[pando.SkipVar]; ok || p.HarnessSessionID != "" {
			t.Fatalf("steward pane marked: %v", p.Env)
		}
	}
}

// A resume starts the pane from its record: the marker stays, and a
// restart without a harness resume gets a new listed session.
func TestAResumedAgentPaneKeepsTheMarker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newEndedServer(t)
	prog, out := claudeStub(t)
	id := createClaudePane(t, s, true, prog)
	waitDone(t, s, id)
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume: %+v", got[0].Error)
	}
	newID := result(t, got[0])["pane"].(string)
	rec, _ := s.tree.Pane(newID)
	if rec.Env[pando.SkipVar] != "1" || rec.Env[pando.SkipListVar] == "" {
		t.Fatalf("resumed env = %v, want the marker kept", rec.Env)
	}
	lines := argLines(t, out, 2)
	a, b := strings.Fields(lines[0])[1], strings.Fields(lines[1])[1]
	if a == b || !listed(s)[a] || !listed(s)[b] {
		t.Fatalf("sessions %s and %s: want two ids, both listed", a, b)
	}
}

func mustRequest(t *testing.T, line string) proto.Request {
	t.Helper()
	var r proto.Request
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
