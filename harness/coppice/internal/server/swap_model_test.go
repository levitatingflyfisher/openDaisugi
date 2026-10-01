package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A model chosen for a sprig agent stays in its record, so the turns of a
// resumed pane run on it again.
func TestASprigModelChoiceSurvivesAResume(t *testing.T) {
	bin, err := filepath.Abs("../../testdata/adapters/fake-sprig.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", bin)
	s := newEndedServer(t)
	s.RegisterAgentCommands()
	work := t.TempDir()
	echo := filepath.Join(work, "argv.txt")
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","kind":"headless","harness":"sprig","cwd":"`+work+
		`","cmd_argv":["--session-dir","`+work+`/sessions","--session","s1"],"env":{"COPPICE_SPRIG_ECHO_ARGV":"`+echo+`"}}`)
	id, _ := result(t, got[0])["pane"].(string)
	got = roundTrip(t, s, `{"id":"2","cmd":"agent.set_model","pane":"`+id+`","model":"opus-x"}`)
	if !got[0].OK {
		t.Fatalf("agent.set_model: %+v", got[0].Error)
	}
	if rec, _ := s.tree.Pane(id); rec.Model != "opus-x" {
		t.Fatalf("the record has model %q, want opus-x", rec.Model)
	}
	// End the pane as a process that ends on its own does, keeping the record.
	lp, _ := s.Live(id)
	_ = lp.Adapter.Stop()
	<-lp.gone
	if err := s.closePane(id, nil); err != nil {
		t.Fatal(err)
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.resume","pane":"`+id+`"}`)
	newID, _ := result(t, got[0])["pane"].(string)
	if rec, _ := s.tree.Pane(newID); rec.Model != "opus-x" {
		t.Fatalf("the resumed record has model %q, want opus-x", rec.Model)
	}
	got = roundTrip(t, s, `{"id":"4","cmd":"agent.prompt","pane":"`+newID+`","text":"after resume","wait":true,"until":"idle","timeout_ms":10000}`)
	if !got[0].OK {
		t.Fatalf("agent.prompt: %+v", got[0].Error)
	}
	b, _ := os.ReadFile(echo)
	if !strings.Contains(string(b), "--model opus-x -- after resume") {
		t.Fatalf("the resumed pane's turn did not run on opus-x:\n%s", b)
	}
}
