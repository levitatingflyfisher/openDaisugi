package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// A pane connection that creates a pane makes an agent-made pane. The
// operator's own create is not.
func TestAPaneMadeByAPaneConnectionIsAgentMade(t *testing.T) {
	s := readyServer(t)
	dir := t.TempDir()
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p99"})
	defer stop()
	send(`{"id":"1","cmd":"pane.create","kind":"pty","cwd":"` + dir + `","cmd_argv":["bash","-c","sleep 5"],"label":"kid"}`)
	res, _ := next()["result"].(map[string]any)
	kid, _ := res["pane"].(string)
	if rec, ok := s.tree.Pane(kid); !ok || rec.MadeBy != layout.MadeByAgent {
		t.Fatalf("pane made by a pane connection: %+v", rec)
	}
	own := startFake(t, s, "", "sleep 5")
	if rec, _ := s.tree.Pane(own); rec.MadeBy != "" {
		t.Fatalf("pane made by the operator: MadeBy = %q", rec.MadeBy)
	}
}

// The floor never answers a prompt by itself: an agent-made pane at Claude's
// trust screen waits for the operator, and the row says it asks to trust.
func TestTheFloorNeverAnswersATrustScreenByItself(t *testing.T) {
	s := readyServer(t)
	dir := t.TempDir()
	f := filepath.Join(t.TempDir(), "screen.txt")
	if err := os.WriteFile(f, []byte(strings.Replace(trustScreen(t), "/home/me/new-project", dir, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "stty raw -echo; cat " + f + `; if read -t 3 -n 1 x; then echo KEYS; else echo NOKEYS; fi; sleep 10`
	got := call(t, s, map[string]any{
		"id": "c", "cmd": "pane.create", "cwd": dir, "kind": "pty", "harness": "claude",
		"cmd_argv": []string{"bash", "-c", script}, "label": "fake", "cols": 120, "rows": 30,
	})
	res, _ := got.Result.(map[string]any)
	id, _ := res["pane"].(string)
	if err := s.updatePane(id, func(r *layout.Pane) { r.MadeBy = layout.MadeByAgent }); err != nil {
		t.Fatal(err)
	}
	ev := waitManifest(t, s, id, proto.StateBlocked)
	if !IsTrustDetail(ev.Detail) {
		t.Fatalf("detail = %q", ev.Detail)
	}
	waitScreenFor(t, s, id, "NOKEYS", 8*time.Second)
}

// waitScreenFor is waitScreen with its own limit.
func waitScreenFor(t *testing.T, s *Server, id, want string, limit time.Duration) {
	t.Helper()
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if strings.Contains(screen(t, s, id), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never showed %q: %q", id, want, screen(t, s, id))
}
